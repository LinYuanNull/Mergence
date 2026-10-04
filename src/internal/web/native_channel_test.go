package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"mergence/internal/config"
)

// rawManaged 取 /api/channels/raw 的原始返回（托管型渠道的完整可编辑配置）。
func rawManaged(t *testing.T, base, name string) map[string]any {
	t.Helper()
	resp, err := http.Get(base + "/api/channels/raw?name=" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("raw 接口应 200，得到 %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestNativeChannelRawEchoesKindAndPreset 回显是原生型渠道能「编辑一次而不坏」的前提。
//
// gateway_kind 不回显 ⇒ 唯一性校验失效、面板代理找不到契约前缀；
// preset 不回显 ⇒ 下拉框停在第一个选项，把渠道显示成别的模板。
// mode 不必回显：托管渠道现在只有进程内原生一种形态，由服务端固定写死。
func TestNativeChannelRawEchoesKindAndPreset(t *testing.T) {
	_, base := newTestServer(t, func(cfg *config.Config) {
		cfg.Managed = []config.ManagedProvider{{
			Name: "wb", DisplayName: "WorkBuddy", Enabled: false,
			Mode: config.ModeNative, Kind: "workbuddy",
			HealthPath: "/healthz",
		}}
	})
	out := rawManaged(t, base, "wb")
	if got := out["gateway_kind"]; got != "workbuddy" {
		t.Errorf("raw.gateway_kind = %v，期望 workbuddy", got)
	}
	// 原生型没有 command/args 当锚点，preset 只能靠 kind 查表补出来；
	// 不补的话下拉框会停在第一个选项，把渠道显示成别的模板。
	if got := out["preset"]; got != "workbuddy" {
		t.Errorf("raw.preset = %v，期望 workbuddy（原生型按 kind 查模板）", got)
	}
	if got := out["preset_inferred"]; got != true {
		t.Errorf("raw.preset_inferred = %v，期望 true", got)
	}
}

// TestNativeChannelRoundTripKeepsKind 面板新建一条原生渠道后，配置里必须留下
// gateway_kind / data_dir。这是「前端提交 → 落盘 → 回显」的完整往返。
func TestNativeChannelRoundTripKeepsKind(t *testing.T) {
	_, base := newTestServer(t, nil)

	body, _ := json.Marshal(map[string]any{
		"kind": "managed", "name": "wb", "display_name": "WorkBuddy",
		"mode": config.ModeNative, "gateway_kind": "workbuddy",
		"preset": "workbuddy", "health_path": "/healthz",
		// 原生型不带启动命令；enabled/expose 关掉，测试不该真的去路由。
		"data_dir": "D:/old-wb2api",
		"enabled":  false, "expose": false,
	})
	resp, err := http.Post(base+"/api/channels", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("保存原生渠道应 200，得到 %d", resp.StatusCode)
	}

	out := rawManaged(t, base, "wb")
	if got := out["gateway_kind"]; got != "workbuddy" {
		t.Errorf("落盘后 gateway_kind = %v，期望 workbuddy", got)
	}
	// data_dir 是「指到原 wb2api 目录以沿用已登录账号」的唯一入口，必须完整往返。
	if got := out["data_dir"]; got != "D:/old-wb2api" {
		t.Errorf("data_dir = %v，期望 D:/old-wb2api", got)
	}
}

// TestNativeChannelConflictWithSubprocessWorkBuddy 内置原生渠道与老 workbuddy
// 渠道是**同一个平台**，不能同时存在 —— 两者各持一份账号目录，并存只会互相覆盖。
// 这里锁住「新建原生渠道时会被老渠道拦下」。
//
// 老渠道按 kind=workbuddy 认出来（独立子进程模式已移除，空 mode 的渠道在
// 归一化时被禁用，但它的 kind 仍参与唯一性判重，不会被漏掉）。
func TestNativeChannelConflictWithSubprocessWorkBuddy(t *testing.T) {
	_, base := newTestServer(t, func(cfg *config.Config) {
		cfg.Managed = []config.ManagedProvider{{
			Name: "wb-old", DisplayName: "WorkBuddy", Enabled: false,
			// 老配置：没有 mode（子进程形态），但 kind 落盘了 workbuddy。
			Kind: "workbuddy", HealthPath: "/healthz",
		}}
	})
	body, _ := json.Marshal(map[string]any{
		"kind": "managed", "name": "wb-new", "display_name": "WorkBuddy 内置",
		"mode": config.ModeNative, "gateway_kind": "workbuddy",
		"preset": "workbuddy", "enabled": false, "expose": false,
	})
	resp, err := http.Post(base+"/api/channels", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("同一平台重复添加应 409，得到 %d", resp.StatusCode)
	}
}
