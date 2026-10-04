package provider

import (
	"testing"

	"mergence/internal/config"
)

// TestManagedPresetByKind 原生型渠道的模板回显靠 kind 直接查表。
//
// 原生型没有 command/args 可作推断锚点（进程内运行没有可执行文件），
// InferManagedPreset 只会落到「自定义托管进程（空白）」上。它的 kind 本就是
// 内置实现名、与模板 id 一一对应，照着查即可，不该去猜。
func TestManagedPresetByKind(t *testing.T) {
	p, ok := ManagedPresetByKind("workbuddy")
	if !ok || p.ID != "workbuddy" {
		t.Errorf("ManagedPresetByKind(\"workbuddy\") = %+v, ok=%v", p, ok)
	}
	if _, ok := ManagedPresetByKind(""); ok {
		t.Error("空 kind 不该查得到模板")
	}
	if _, ok := ManagedPresetByKind("不存在的实现"); ok {
		t.Error("未知 kind 不该查得到模板")
	}
	// id 与 kind 同名的那些（zcode / trae）按名字查得到，同样合理。
	if p, ok := ManagedPresetByKind("zcode"); !ok || p.ID != "zcode" {
		t.Errorf("ManagedPresetByKind(\"zcode\") = %+v, ok=%v", p, ok)
	}
	if p, ok := ManagedPresetByKind("trae"); !ok || p.ID != "trae" {
		t.Errorf("ManagedPresetByKind(\"trae\") = %+v, ok=%v", p, ok)
	}
}

// TestTraePresetIsNative trae 已从「托管子进程（node server.js）」改成
// 「内置原生」（上游 connectedGraph/trae2api-web，MIT 照搬）。
func TestTraePresetIsNative(t *testing.T) {
	p, ok := ManagedPresetByID("trae")
	if !ok {
		t.Fatal("找不到 trae 预设")
	}
	if p.Mode != config.ModeNative {
		t.Errorf("trae 预设的 mode = %q，期望 %q", p.Mode, config.ModeNative)
	}
	if p.Kind != "trae" {
		t.Errorf("trae 预设的 kind = %q，期望 trae", p.Kind)
	}
	if p.Command != "" {
		t.Errorf("原生型预设不该带 command，得到 %q —— 带了会让渠道被 normalize 禁用", p.Command)
	}
	if p.PortEnvVar != "" {
		t.Errorf("原生型预设不该带 port_env_var，得到 %q", p.PortEnvVar)
	}
	if p.PanelAPIPrefix != "/admin/api" {
		t.Errorf("trae 的管理 API 前缀 = %q，期望 /admin/api", p.PanelAPIPrefix)
	}
	if p.PanelPath != "/admin" {
		t.Errorf("trae 的面板路径 = %q，期望 /admin", p.PanelPath)
	}
	// 刻意**不**设 LegacyCommands：老 trae 模板的 command 是解释器名 `node`，
	// 拿它当锚点会把任何用 node 启动的渠道（含用户自建网关）都误判成 trae。
	// 这条断言是防回归的——将来有人「顺手补上 node 让老配置能回显」时会被拦下。
	if len(p.LegacyCommands) != 0 {
		t.Errorf("trae 预设不该有 LegacyCommands（node 是解释器名、不具区分度），得到 %v",
			p.LegacyCommands)
	}
}

// TestNativePresetsHaveNoCommandOrPort 所有原生型预设的通用约束。
//
// 进程内运行没有可执行文件、也没有「端口环境变量」这一说（端口由内核分配）。
// 预设里若留着这两者，config 的 normalize 会判成「两种运行方式只能选一种」
// 而把新建的渠道直接禁用——用户看到的是一建好就红着的渠道。
func TestNativePresetsHaveNoCommandOrPort(t *testing.T) {
	n := 0
	for _, p := range ManagedPresets {
		if p.Mode != config.ModeNative {
			continue
		}
		n++
		if p.Command != "" {
			t.Errorf("原生型预设 %s 带了 command=%q", p.ID, p.Command)
		}
		if len(p.Args) != 0 {
			t.Errorf("原生型预设 %s 带了 args=%v", p.ID, p.Args)
		}
		if p.PortEnvVar != "" {
			t.Errorf("原生型预设 %s 带了 port_env_var=%q", p.ID, p.PortEnvVar)
		}
		// 注意：fixed_port 不是 ManagedPreset 的字段（它属于 config.EmbeddedProvider），
		// 预设里根本没有这一项，所以这里不检查它。原生型渠道若被填了 fixed_port，
		// 由 config 的 normalize 清零并告警，见 config/managed_mode_test.go。
	}
	if n == 0 {
		t.Fatal("没有任何原生型预设，测试失去意义（workbuddy / trae 应至少各有一个）")
	}
}

// TestWorkBuddyPresetIsNative workbuddy 模板已从「托管子进程」改成「内置原生」：
// 它不该再带 command —— 带了会被 config 的 normalize 判成「两种运行方式只能选一种」
// 而把整个渠道禁用。
func TestWorkBuddyPresetIsNative(t *testing.T) {
	p, ok := ManagedPresetByID("workbuddy")
	if !ok {
		t.Fatal("找不到 workbuddy 预设")
	}
	if p.Mode != config.ModeNative {
		t.Errorf("workbuddy 预设的 mode = %q，期望 %q", p.Mode, config.ModeNative)
	}
	if p.Kind != "workbuddy" {
		t.Errorf("workbuddy 预设的 kind = %q，期望 workbuddy", p.Kind)
	}
	if p.Command != "" {
		t.Errorf("原生型预设不该带 command，得到 %q —— 带了会让渠道被 normalize 禁用", p.Command)
	}
	if p.PortEnvVar != "" {
		t.Errorf("原生型预设不该带 port_env_var，得到 %q", p.PortEnvVar)
	}
	// 老配置里的子进程形态仍要能被认出来（否则面板回显会退化成「自定义托管进程（空白）」，
	// 用户会以为自己填的东西丢了）。
	if len(p.LegacyCommands) == 0 {
		t.Error("workbuddy 预设需要 LegacyCommands 才能认老配置里的 wb2api.exe")
	}
}

// TestManagedPresetsExposeModeAndKind 前端靠这两个字段决定「提交 mode=native 还是拉子进程」，
// 所以它们必须随 /api/presets 一起下发（json tag 不能是 "-"）。
func TestManagedPresetsExposeModeAndKind(t *testing.T) {
	for _, p := range ManagedPresets {
		if p.Mode == config.ModeNative && p.Kind == "" {
			t.Errorf("原生型预设 %s 必须同时给出 kind（否则前端无法提交 kind）", p.ID)
		}
	}
}
