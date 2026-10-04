package provider

import "testing"

// TestManagedPresetByKind 原生型渠道的模板回显靠 kind 直接查表。
//
// 原生型没有 command/args 可作推断锚点（进程内运行没有可执行文件），
// 它的 kind 本就是内置实现名、与模板 id 一一对应，照着查即可，不该去猜。
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

// TestTraePresetIsNative trae 是「内置原生」（上游 connectedGraph/trae2api-web，
// MIT 照搬）：必须给出 kind，管理面契约与面板路径正确。
func TestTraePresetIsNative(t *testing.T) {
	p, ok := ManagedPresetByID("trae")
	if !ok {
		t.Fatal("找不到 trae 预设")
	}
	if p.Kind != "trae" {
		t.Errorf("trae 预设的 kind = %q，期望 trae", p.Kind)
	}
	if p.PanelAPIPrefix != "/admin/api" {
		t.Errorf("trae 的管理 API 前缀 = %q，期望 /admin/api", p.PanelAPIPrefix)
	}
	if p.PanelPath != "/admin" {
		t.Errorf("trae 的面板路径 = %q，期望 /admin", p.PanelPath)
	}
}

// TestManagedPresetsAllHaveKind 所有托管型模板都必须给出 kind —— 它是「装配哪个
// 内置实现」的唯一依据，缺了渠道会被 config 的 normalize 直接禁用。
//
// 顺带锁住 health_path：原生服务同样走真实 HTTP 探活，没有它就无法判就绪。
func TestManagedPresetsAllHaveKind(t *testing.T) {
	if len(ManagedPresets) == 0 {
		t.Fatal("没有任何托管型预设，测试失去意义")
	}
	for _, p := range ManagedPresets {
		if p.Kind == "" {
			t.Errorf("托管型预设 %s 没填 kind（缺 kind 的渠道会被禁用）", p.ID)
		}
		if p.HealthPath == "" {
			t.Errorf("托管型预设 %s 没填 health_path（原生服务仍走真实 HTTP 探活）", p.ID)
		}
	}
}

// TestWorkBuddyPresetIsNative workbuddy 模板是「内置原生」：给出 kind=workbuddy。
func TestWorkBuddyPresetIsNative(t *testing.T) {
	p, ok := ManagedPresetByID("workbuddy")
	if !ok {
		t.Fatal("找不到 workbuddy 预设")
	}
	if p.Kind != "workbuddy" {
		t.Errorf("workbuddy 预设的 kind = %q，期望 workbuddy", p.Kind)
	}
}

// TestManagedPresetsExposeKind 前端靠 kind 决定「装配哪个内置实现」，
// 所以它必须随 /api/presets 一起下发（json tag 不能是 "-"）。
func TestManagedPresetsExposeKind(t *testing.T) {
	for _, p := range ManagedPresets {
		if p.Kind == "" {
			t.Errorf("托管型预设 %s 必须给出 kind（否则前端无法提交 kind）", p.ID)
		}
	}
}
