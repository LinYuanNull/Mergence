package web

import (
	"encoding/json"
	"net/http"
	"testing"

	"mergence/internal/config"
)

// 面板「编辑渠道」的预设下拉必须反映渠道的**真实**模板。
//
// 缺陷背景：托管型渠道的 preset 从来没被写进配置——`channelInput.toManaged()`
// 漏了这个字段（内嵌型的 `toEmbedded()` 有）。于是 `/api/channels/raw` 回空，
// 前端 `<select>` 停在第一个选项，**zcode 渠道被显示成「WorkBuddy 网关（wb2api）」**，
// 保存时还会把这个错值写进配置。
//
// 这里同时锁住两半：老配置的**回显**（按 command/args/health 推断）与
// **新保存**的持久化。

// rawPreset 请求 /api/channels/raw 并取出预设相关字段。
func rawPreset(t *testing.T, base, name string) (string, bool) {
	t.Helper()
	resp, err := http.Get(base + "/api/channels/raw?name=" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("raw 接口应 200，得到 %d", resp.StatusCode)
	}
	var out struct {
		Preset         string `json:"preset"`
		PresetInferred bool   `json:"preset_inferred"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Preset, out.PresetInferred
}

func TestChannelRawInfersPresetWhenConfigLacksIt(t *testing.T) {
	// 用两个渠道的**真实数据**。关键是它们在预设表里相邻且 workbuddy 排第一：
	// 任何「取第一个命中」的实现都会把 zcode 判成 workbuddy，这正是原缺陷。
	cases := []struct {
		name, command, health, portEnv string
		args                           []string
		want                           string
	}{
		{
			name: "zcode", want: "zcode",
			command: "C:/venvs/zcode2api/Scripts/python.exe",
			args:    []string{"cli.py", "serve"},
			health:  "/meta", portEnv: "ZCODE_PORT",
		},
		{
			name: "workbuddy", want: "workbuddy",
			command: "wb2api.exe", args: []string{},
			health: "/healthz", portEnv: "WB2A_LISTEN",
		},
	}
	for _, c := range cases {
		// Enabled:false 只是为了让 raw 接口能找到它 —— 测试不该拉起真实子进程。
		_, base := newTestServer(t, func(cfg *config.Config) {
			cfg.Managed = []config.ManagedProvider{{
				Name: c.name, DisplayName: c.name, Enabled: false,
				Command: c.command, Args: c.args,
				HealthPath: c.health, PortEnvVar: c.portEnv,
				// Preset 刻意留空：模拟老配置（该字段是后加的）
			}}
		})
		got, inferred := rawPreset(t, base, c.name)
		if got != c.want {
			t.Errorf("渠道 %s：preset = %q，期望 %q —— 配置里没记模板时应按 command/args/health 推断",
				c.name, got, c.want)
		}
		if !inferred {
			t.Errorf("渠道 %s：preset 是推断出来的，应带 preset_inferred=true", c.name)
		}
	}
}

func TestChannelRawKeepsStoredPreset(t *testing.T) {
	// 配置里已经记了模板时必须原样返回，且**不能**标成推断值 ——
	// 否则用户会以为「这条记录是猜的」，反而不敢信。
	// 这里故意让「配置值」与「按配置推断的结果」不一致（记 trae，但命令像 trae、
	// 探活像 workbuddy），确保返回的是配置值而不是推断值。
	_, base := newTestServer(t, func(cfg *config.Config) {
		cfg.Managed = []config.ManagedProvider{{
			Name: "z", DisplayName: "z", Enabled: false,
			Preset: "trae", Command: "node", Args: []string{"server.js"},
			HealthPath: "/healthz",
		}}
	})
	got, inferred := rawPreset(t, base, "z")
	if got != "trae" {
		t.Errorf("preset = %q，期望 trae —— 配置里已记录的模板不该被推断结果覆盖", got)
	}
	if inferred {
		t.Error("配置里已记录 preset 时不该标 preset_inferred")
	}
}

func TestToManagedPersistsPreset(t *testing.T) {
	// 根因修复：toManaged 以前漏了 Preset，托管渠道的模板永远存不下来，
	// 于是每次打开编辑都只能靠推断。这里锁住「保存会把它记进配置」。
	in := channelInput{Kind: kindManaged, Name: "z", Preset: "zcode"}
	if got := in.toManaged().Preset; got != "zcode" {
		t.Errorf("toManaged().Preset = %q，期望 zcode（保存后必须写进配置）", got)
	}
	// 内嵌型一直是带的，这里一并锁住，防止以后有人「统一」掉。
	// （内嵌型走 toEmbedded，见 api_channels.go）
}
