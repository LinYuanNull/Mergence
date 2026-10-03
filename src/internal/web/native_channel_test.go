package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"modelmux/internal/config"
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

// TestNativeChannelRawEchoesModeAndKind 回显是原生型渠道能「编辑一次而不坏」的前提。
//
// mode 不回显 ⇒ 编辑保存后 mode 变空值 ⇒ 等于子进程 ⇒ 渠道去拉起一个空的
// 可执行文件，再也起不来；gateway_kind 不回显 ⇒ 唯一性校验失效。
func TestNativeChannelRawEchoesModeAndKind(t *testing.T) {
	_, base := newTestServer(t, func(cfg *config.Config) {
		cfg.Managed = []config.ManagedProvider{{
			Name: "wb", DisplayName: "WorkBuddy", Enabled: false,
			Mode: config.ModeNative, Kind: "workbuddy",
			HealthPath: "/healthz",
		}}
	})
	out := rawManaged(t, base, "wb")
	if got := out["mode"]; got != config.ModeNative {
		t.Errorf("raw.mode = %v，期望 %q", got, config.ModeNative)
	}
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

// TestNativeChannelRoundTripKeepsMode 面板新建一条原生渠道后，配置里必须留下
// mode=native / kind / data_dir。这是「前端提交 → 落盘 → 回显」的完整往返。
func TestNativeChannelRoundTripKeepsMode(t *testing.T) {
	_, base := newTestServer(t, nil)

	body, _ := json.Marshal(map[string]any{
		"kind": "managed", "name": "wb", "display_name": "WorkBuddy",
		"mode": config.ModeNative, "gateway_kind": "workbuddy",
		"preset": "workbuddy", "health_path": "/healthz",
		// 留空 command 正是原生型的形态；enabled/expose 关掉，测试不该真的去路由。
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
	if got := out["mode"]; got != config.ModeNative {
		t.Errorf("落盘后 mode = %v，期望 %q", got, config.ModeNative)
	}
	if got := out["gateway_kind"]; got != "workbuddy" {
		t.Errorf("落盘后 gateway_kind = %v，期望 workbuddy", got)
	}
	if got := out["command"]; got != "" && got != nil {
		t.Errorf("原生型不该落盘 command，得到 %v", got)
	}
	// data_dir 是「指到原 wb2api 目录以沿用已登录账号」的唯一入口，必须完整往返。
	if got := out["data_dir"]; got != "D:/old-wb2api" {
		t.Errorf("data_dir = %v，期望 D:/old-wb2api", got)
	}
}

// TestNativeChannelConflictWithSubprocessWorkBuddy 内置原生渠道与子进程形态的
// 老 workbuddy 渠道是**同一个平台**，不能同时存在 —— 两者各持一份账号目录，
// 并存只会互相覆盖。这里锁住「新建原生渠道时会被老渠道拦下」。
func TestNativeChannelConflictWithSubprocessWorkBuddy(t *testing.T) {
	_, base := newTestServer(t, func(cfg *config.Config) {
		cfg.Managed = []config.ManagedProvider{{
			Name: "wb-old", DisplayName: "WorkBuddy", Enabled: false,
			// 老配置：没有 mode ⇒ 子进程形态。
			Kind: "workbuddy", Command: "wb2api.exe", HealthPath: "/healthz",
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
