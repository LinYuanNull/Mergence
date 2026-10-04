package web

import (
	"testing"

	"mergence/internal/config"
)

// TestGatewayKindTaken 平台唯一性判重。
//
// ponytail：判重是本轮需求 2 的核心逻辑（一个分支 + 一个豁免），
// 留一条能跑的检查防止以后改坏。新api 豁免与「编辑自己不撞车」是两个
// 容易被顺手改坏的边界，所以都断言到。
func TestGatewayKindTaken(t *testing.T) {
	cfg := &config.Config{Managed: []config.ManagedProvider{
		{Name: "zc", DisplayName: "ZCode", Kind: "zcode"},
		{Name: "wb", DisplayName: "WorkBuddy", Kind: "workbuddy"},
	}}

	cases := []struct {
		name string
		kind string
		skip string
		want bool
	}{
		{"zcode 已有，重复添加", "zcode", "", true},
		{"workbuddy 已有，重复添加", "workbuddy", "", true},
		{"未知种类不拦（认不出就不拦）", "", "", false},
		{"认不出的种类不拦（老配置兼容）", "trae", "", false},
		{"new-api 可建多个", "newapi", "", false},
		{"编辑自己不算撞车", "zcode", "zc", false},
		// 回归：不传 original_name、直接用同名提交（e2e 领取就是这么调的）也
		// 必须被当成「编辑自己」，否则唯一性校验会把自己拦下来。
		{"同名提交（skipName 回退到 name）", "zcode", "zc", false},
		// 回归：不传 original_name、直接用同名提交（e2e 领取就是这么调的）也
		// 必须被当成「编辑自己」，否则唯一性校验会把自己拦下来。
		{"同名提交（skipName 回退到 name）", "zcode", "zc", false},
		{"编辑自己时仍能看到别人", "zcode", "wb", true},
	}
	for _, c := range cases {
		if got := gatewayKindTaken(cfg, c.kind, c.skip); got != c.want {
			t.Errorf("%s: gatewayKindTaken(%q, %q) = %v, 期望 %v",
				c.name, c.kind, c.skip, got, c.want)
		}
	}
}

// TestGatewayKindOf 回退识别：老配置没存 kind 时要能按名称/预设认出来。
//
// 这条保证「升级前建的渠道不会因为没存 kind 而绕过唯一性」。
func TestGatewayKindOf(t *testing.T) {
	cases := []struct {
		name string
		m    config.ManagedProvider
		want string
	}{
		{"有 kind 时直接用", config.ManagedProvider{Name: "x", Kind: "zcode"}, "zcode"},
		{"按预设认 zcode", config.ManagedProvider{Name: "a", Preset: "zcode"}, "zcode"},
		{"按预设认 workbuddy", config.ManagedProvider{Name: "a", Preset: "workbuddy"}, "workbuddy"},
		{"按启动命令认 new-api(名字)", config.ManagedProvider{Name: "new-api", Command: "new-api.exe"}, gatewayKindNewAPI},
		{"认不出返回空", config.ManagedProvider{Name: "whatever"}, ""},
		// 只靠名字认不出来：显示名/渠道名是用户起的，不构成「跑的是什么」的证据
		{"光有名字认不出 zcode", config.ManagedProvider{Name: "zcode-副本"}, ""},
		// 关键：存了 kind 就不再看名字，改名后依然认得出来
		{"有 kind 时忽略改名", config.ManagedProvider{Name: "我的网关", Kind: "zcode"}, "zcode"},
		// command/args 是客观事实，比名字可靠：认它
		{"按启动命令认 zcode", config.ManagedProvider{Name: "gw1", Command: "python", Args: []string{"cli.py", "serve"}}, "zcode"},
		{"按启动命令认 new-api", config.ManagedProvider{Name: "gw2", Command: "new-api.exe"}, gatewayKindNewAPI},
		// 关键回归：显示名里带 zcode 但启动的是别的东西 —— 不能凭名字判重，
		// 否则用户给渠道起名「ZCode 网关」就会被误判成重复（e2e 领取测试踩过这个）。
		{"显示名带 zcode 但命令不符，不认", config.ManagedProvider{Name: "x", DisplayName: "ZCode 网关", Command: "other.exe"}, ""},
	}
	for _, c := range cases {
		if got := gatewayKindOf(c.m); got != c.want {
			t.Errorf("%s: gatewayKindOf(%+v) = %q, 期望 %q", c.name, c.m, got, c.want)
		}
	}
}
