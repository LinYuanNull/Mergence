package provider

import (
	"errors"
	"strings"
	"testing"

	"mergence/internal/config"
)

func newTestRegistry(t *testing.T, ups ...Upstream) *Registry {
	t.Helper()
	reg := NewRegistry(testLogger(t))
	reg.Reload(ups)
	return reg
}

func ch(name, prefix string, models config.ModelList) Upstream {
	return Upstream{
		Name: name, DisplayName: name, Source: SourceEmbedded, Enabled: true,
		BaseURL: "http://127.0.0.1:1", Protocol: "chat",
		ModelPrefix: prefix, Models: models, Weight: 1,
	}
}

func TestRouteByPrefix(t *testing.T) {
	reg := newTestRegistry(t,
		ch("or", "or/", config.ChannelModelOf("openai/gpt-4o")),
		ch("zhipu", "zhipu/", config.ModelList{{ID: "glm-4.6", Alias: "glm"}}),
	)

	cases := []struct {
		model   string
		wantCh  string
		wantUp  string
		wantErr error
	}{
		{"or/openai/gpt-4o", "or", "openai/gpt-4o", nil},
		// 别名映射：对外叫 glm，上游叫 glm-4.6
		{"zhipu/glm", "zhipu", "glm-4.6", nil},
		// 上游真名也能直接用
		{"zhipu/glm-4.6", "zhipu", "glm-4.6", nil},
		// 未声明但前缀匹配 → 透传（上游上新模型不必改配置）
		{"zhipu/glm-9-turbo", "zhipu", "glm-9-turbo", nil},
		// 裸名匹配声明列表
		{"openai/gpt-4o", "or", "openai/gpt-4o", nil},
		{"", "", "", ErrModelRequired},
		{"auto", "", "", ErrAutoRouting},
		{"nope/whatever", "", "", ErrModelNotFound},
	}

	for _, c := range cases {
		gotCh, gotUp, err := reg.Route(c.model)
		if c.wantErr != nil {
			if !errors.Is(err, c.wantErr) {
				t.Errorf("Route(%q) 错误 = %v，期望 %v", c.model, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("Route(%q) 意外错误：%v", c.model, err)
			continue
		}
		if gotCh.Name() != c.wantCh || gotUp != c.wantUp {
			t.Errorf("Route(%q) = (%s, %s)，期望 (%s, %s)",
				c.model, gotCh.Name(), gotUp, c.wantCh, c.wantUp)
		}
	}
}

func TestRoutePrefersLongestPrefix(t *testing.T) {
	reg := newTestRegistry(t,
		ch("broad", "a/", nil),
		ch("narrow", "a/b/", nil),
	)
	got, up, err := reg.Route("a/b/c")
	if err != nil {
		t.Fatalf("Route 失败：%v", err)
	}
	if got.Name() != "narrow" {
		t.Errorf("应命中更长的前缀 a/b/，实际命中 %s", got.Name())
	}
	if up != "c" {
		t.Errorf("上游模型名 = %q，期望 c", up)
	}
}

func TestRouteSkipsDisabled(t *testing.T) {
	off := ch("off", "off/", config.ChannelModelOf("m"))
	off.Enabled = false
	reg := newTestRegistry(t, off)

	// 停用的渠道不参与转发，但错误必须说清「是停用了」而不是「模型不存在」——
	// 后者会让用户去改模型名，方向完全错了。
	_, _, err := reg.Route("off/m")
	var nre *NotReadyError
	if !errors.As(err, &nre) {
		t.Fatalf("应返回 NotReadyError，实际：%v", err)
	}
	if !strings.Contains(nre.Error(), "停用") {
		t.Errorf("原因应点明「停用」：%v", nre)
	}
	if len(reg.Models()) != 0 {
		t.Errorf("禁用渠道的模型不应出现在模型列表：%v", reg.Models())
	}
}

func TestRouteNotReadyManaged(t *testing.T) {
	// 托管型子进程没起来：同样不该退化成 404
	up := ch("mg", "mg/", config.ChannelModelOf("m"))
	up.Source = SourceManaged
	up.Ready = func() bool { return false }
	up.UnreadyReason = func() string { return "子进程未运行" }

	reg := newTestRegistry(t, up)
	_, _, err := reg.Route("mg/m")
	var nre *NotReadyError
	if !errors.As(err, &nre) {
		t.Fatalf("应返回 NotReadyError，实际：%v", err)
	}
	if !strings.Contains(nre.Error(), "子进程未运行") {
		t.Errorf("应带上未就绪原因：%v", nre)
	}
	// 未就绪的渠道也不该出现在模型列表里
	if len(reg.Models()) != 0 {
		t.Errorf("未就绪渠道的模型不应出现在列表：%v", reg.Models())
	}

	// 就绪后立即可路由（不需要重建注册表）
	up.Ready = func() bool { return true }
	reg.Reload([]Upstream{up})
	got, _, err := reg.Route("mg/m")
	if err != nil || got.Name() != "mg" {
		t.Fatalf("就绪后应可路由：%v %v", got, err)
	}
}

func TestSyncIfChanged(t *testing.T) {
	reg := newTestRegistry(t, ch("a", "a/", config.ChannelModelOf("m")))

	// 同样的上游不该触发重建（否则面板每次轮询都会清空 Key 冷却状态）
	if reg.SyncIfChanged([]Upstream{ch("a", "a/", config.ChannelModelOf("m"))}) {
		t.Error("指纹未变时不应重建")
	}
	// 影响路由的字段变了才重建
	if !reg.SyncIfChanged([]Upstream{ch("a", "b/", config.ChannelModelOf("m"))}) {
		t.Error("前缀变化应触发重建")
	}
	// 就绪状态变化也算（托管型子进程起来/退出）
	up := ch("a", "b/", config.ChannelModelOf("m"))
	up.Ready = func() bool { return false }
	if !reg.SyncIfChanged([]Upstream{up}) {
		t.Error("就绪状态变化应触发重建")
	}
}

func TestRouteBareNameUsesPriority(t *testing.T) {
	a := ch("low", "low/", config.ChannelModelOf("shared"))
	a.Priority = 1
	b := ch("high", "high/", config.ChannelModelOf("shared"))
	b.Priority = 9

	// 顺序刻意让低优先级在前，验证排序真的生效
	reg := newTestRegistry(t, a, b)
	got, up, err := reg.Route("shared")
	if err != nil {
		t.Fatalf("Route 失败：%v", err)
	}
	if got.Name() != "high" {
		t.Errorf("应选优先级更高的 high，实际 %s", got.Name())
	}
	if up != "shared" {
		t.Errorf("上游模型名 = %q", up)
	}
}

func TestRouteEmptyRegistry(t *testing.T) {
	reg := newTestRegistry(t)
	if _, _, err := reg.Route("anything"); !errors.Is(err, ErrNoChannel) {
		t.Errorf("空注册表应返回 ErrNoChannel，实际 %v", err)
	}
}

func TestModelsAggregation(t *testing.T) {
	reg := newTestRegistry(t,
		ch("or", "or/", config.ChannelModelOf("openai/gpt-4o", "anthropic/claude-3")),
		ch("zhipu", "zhipu/", config.ModelList{{ID: "glm-4.6", Alias: "glm"}}),
	)
	got := reg.Models()
	if len(got) != 3 {
		t.Fatalf("模型数 = %d，期望 3：%+v", len(got), got)
	}
	// 按 id 排序，前缀已拼上
	want := []string{"or/anthropic/claude-3", "or/openai/gpt-4o", "zhipu/glm"}
	for i, w := range want {
		if got[i].ID != w {
			t.Errorf("第 %d 项 = %q，期望 %q", i, got[i].ID, w)
		}
	}
	if got[2].Upstream != "glm-4.6" {
		t.Errorf("别名模型的 upstream 应为真名：%+v", got[2])
	}
}

func TestKeyPoolRotationAndCooldown(t *testing.T) {
	p := NewKeyPool([]string{"k1", "k2"})

	first, _, _ := p.Pick()
	second, _, _ := p.Pick()
	if first == second {
		t.Fatalf("轮询应取到不同 Key，两次都是 %q", first)
	}

	// 让 first 被限流，冷却期内不应再被选中
	p.ReportFailure(first, 429)
	for i := 0; i < 3; i++ {
		got, allCool, _ := p.Pick()
		if got == first && !allCool {
			t.Fatalf("冷却中的 Key 不应被优先取用：%v", p.Stats())
		}
	}

	// 400 不该惩罚 Key（那是请求本身的问题）
	p2 := NewKeyPool([]string{"only"})
	p2.ReportFailure("only", 400)
	if st := p2.Stats(); st[0].Cooling || st[0].Failures != 0 {
		t.Errorf("400 不应让 Key 进入冷却：%+v", st[0])
	}

	// 401 应冷却
	p2.ReportFailure("only", 401)
	if st := p2.Stats(); !st[0].Cooling {
		t.Errorf("401 应让 Key 冷却：%+v", st[0])
	}

	// 全部冷却时仍要返回一个 Key（用可能已解冻的比直接失败更有希望成功）
	got, allCool, ok := p2.Pick()
	if !ok || got != "only" || !allCool {
		t.Errorf("全冷却时仍应返回 Key：got=%q allCool=%v ok=%v", got, allCool, ok)
	}
}

func TestKeyPoolEmpty(t *testing.T) {
	p := NewKeyPool(nil)
	key, allCool, ok := p.Pick()
	if !ok || key != "" || allCool {
		t.Errorf("无 Key 时应返回空串且 ok=true：%q %v %v", key, allCool, ok)
	}
}

func TestMaskKey(t *testing.T) {
	if got := MaskKey("sk-1234567890abcdef"); got != "sk-123●●●●cdef" {
		t.Errorf("MaskKey = %q", got)
	}
	if got := MaskKey("short"); got != "●●●●" {
		t.Errorf("短 Key 应完全遮蔽，得到 %q", got)
	}
	if got := MaskKey(""); got != "" {
		t.Errorf("空 Key 应返回空串，得到 %q", got)
	}
}

func TestStripPrefixBothForms(t *testing.T) {
	// 前缀已规范化成不带斜杠，但存量配置与用户习惯里仍有带斜杠的写法。
	// 两种都必须能路由，否则升级即 404 model_not_found。
	cases := []struct {
		model, prefix, want string
		ok                  bool
	}{
		{"or-gpt-4o", "or-", "gpt-4o", true}, // 现在的形态
		{"or/gpt-4o", "or-", "gpt-4o", true}, // 旧写法：斜杠要被吃掉
		{"WB-GLM-5.3-Flash", "WB-", "GLM-5.3-Flash", true},
		{"or-", "or-", "", false},          // 光前缀没有模型名
		{"other-gpt-4o", "or-", "", false}, // 前缀不匹配
		{"", "or-", "", false},
	}
	for _, c := range cases {
		got, ok := stripPrefix(c.model, c.prefix)
		if ok != c.ok || got != c.want {
			t.Errorf("stripPrefix(%q, %q) = %q(%v)，期望 %q(%v)",
				c.model, c.prefix, got, ok, c.want, c.ok)
		}
	}
}

func TestModelsNoSlashAfterPrefix(t *testing.T) {
	// 用户明确要求：前缀后面不再加斜杠。
	r := newTestRegistry(t, ch("or", "or-", config.ModelList{{ID: "gpt-4o"}}))
	ids := make([]string, 0)
	for _, m := range r.Models() {
		ids = append(ids, m.ID)
	}
	if len(ids) != 1 || ids[0] != "or-gpt-4o" {
		t.Errorf("模型 ID 应为 or-gpt-4o，实际 %v", ids)
	}
	// 两种写法都要能路由到同一个渠道
	for _, m := range []string{"or-gpt-4o", "or/gpt-4o"} {
		c, up, err := r.Route(m)
		if err != nil || c == nil || up != "gpt-4o" {
			t.Errorf("Route(%q) 失败：%v / %q", m, err, up)
		}
	}
}
