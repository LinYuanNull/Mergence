package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"mergence/internal/config"
	"mergence/internal/provider"
)

// trae 内置原生渠道的接缝测试。
//
// 与 zcode 的对照关系（这是本文件存在的理由）：
//
//	前缀   两者都是 /admin/api —— 都不是通用默认的 /panel/api；
//	凭据   zcode 认 config.Claim.AdminKey（后台密码），
//	       trae 认渠道 route 的 key（一个渠道一把钥匙）。
//
// 凭据来源这一条尤其容易写错：写错了会拿到上游 401，而 401 看起来像
// 「接口不存在」，排查时会往错的方向找。所以两条路都要各锁一遍。

// TestPanelAPIPrefixForContractTable 契约表：按 kind 决定管理 API 前缀。
//
// 不读配置里的 panel_api_prefix 是刻意的 —— 老 zcode 渠道存的是错的
// /panel/api（当年照抄 workbuddy 预设留下），而这是网关自己的固定契约。
func TestPanelAPIPrefixForContractTable(t *testing.T) {
	cases := []struct {
		name       string
		kind       string
		configured string
		want       string
	}{
		{"trae 走契约表", "trae", "/panel/api", "/admin/api"},
		{"zcode 走契约表", "zcode", "/panel/api", "/admin/api"},
		{"trae 即便配置为空也走契约表", "trae", "", "/admin/api"},
		{"workbuddy 用配置值", "workbuddy", "/panel/api", "/panel/api"},
		{"自定义网关用配置值", "", "/custom/api", "/custom/api"},
		{"认不出且未配置就返回空", "", "", ""},
	}
	for _, c := range cases {
		if got := panelAPIPrefixFor(c.kind, c.configured); got != c.want {
			t.Errorf("%s: panelAPIPrefixFor(%q, %q) = %q，期望 %q",
				c.name, c.kind, c.configured, got, c.want)
		}
	}
}

// TestClaimPathForTrae 签到路径与 zcode 的领取路径是两条不同的路由。
//
// trae 的签到上游本来没有 HTTP 入口（scheduler 只在进程内按整点跑），
// 由 native/trae 装配层在 handler 外面补出 POST /admin/api/checkin。
// 常量必须与 native/trae.CheckinPath 一致，否则会打到 404 上。
func TestClaimPathForTrae(t *testing.T) {
	cases := []struct {
		kind string
		want string
	}{
		{"trae", "/admin/api/checkin"},
		{"zcode", "/admin/api/claim"},
		{"workbuddy", "/admin/api/claim"}, // 非 trae 一律走 zcode 那条（现有行为）
		{"", "/admin/api/claim"},
	}
	for _, c := range cases {
		if got := claimPathFor(c.kind); got != c.want {
			t.Errorf("claimPathFor(%q) = %q，期望 %q", c.kind, got, c.want)
		}
	}
}

// TestClaimKeyForUsesRouteKeyAfterMerge 领取/签到凭据来源。
//
// 独立子进程模式已移除，所有托管渠道都是**原生型**：zcode 的后台密码 = 渠道
// route key（空回落契约默认值 `zcode`）；trae 等其它网关同样取 route key。
// 合并前「后台密码要在两处同步、只改一边就整块 401」的故障已彻底消除。
func TestClaimKeyForUsesRouteKeyAfterMerge(t *testing.T) {
	s, _ := newTestServer(t, nil)

	// trae：route key（它本来就没有独立后台密码）。
	traeUp := provider.FromManaged(config.ManagedProvider{
		Name: "trae", DisplayName: "Trae", Enabled: true, Preset: "trae", Kind: "trae",
		Mode:  config.ModeNative,
		Route: &config.RouteSpec{APIKey: "route-key", ModelPrefix: "trae/"},
	}, "http://127.0.0.1:1", true, "")
	if got := s.claimKeyFor("trae", traeUp); got != "route-key" {
		t.Errorf("trae 签到凭据 = %q，期望渠道路由密钥（route-key）", got)
	}

	// 原生型 zcode：route key。
	nativeUp := provider.FromManaged(config.ManagedProvider{
		Name: "zcode", DisplayName: "ZCode", Enabled: true, Kind: "zcode",
		Mode:  config.ModeNative,
		Route: &config.RouteSpec{APIKey: "route-key", ModelPrefix: "zcode-"},
	}, "http://127.0.0.1:1", true, "")
	if nativeUp.Source != provider.SourceNative {
		t.Fatalf("测试前提不成立：Mode=native 应产出 SourceNative，实际 %s", nativeUp.Source)
	}
	if got := s.claimKeyFor("zcode", nativeUp); got != "route-key" {
		t.Errorf("原生型 zcode 领取凭据 = %q，期望 route key（唯一来源）", got)
	}

	// 原生型 zcode 空 route key：回落契约默认值。
	emptyNative := provider.FromManaged(config.ManagedProvider{
		Name: "zcode", DisplayName: "ZCode", Enabled: true, Kind: "zcode",
		Mode:  config.ModeNative,
		Route: &config.RouteSpec{ModelPrefix: "zcode-"},
	}, "http://127.0.0.1:1", true, "")
	if got := s.claimKeyFor("zcode", emptyNative); got != "zcode" {
		t.Errorf("原生型 zcode 空 route key 时凭据 = %q，期望契约默认值 zcode", got)
	}
}

// traeManaged 一条 trae 内置原生渠道配置。
//
// PanelAPIPrefix 故意写成老配置里那个错的 /panel/api，用来证明代理走的是
// 契约表而不是这个字段。
func traeManaged() config.ManagedProvider {
	return config.ManagedProvider{
		Name: "trae", DisplayName: "Trae", Enabled: true, Preset: "trae",
		Mode: config.ModeNative, Kind: "trae",
		PanelAPIPrefix: "/panel/api",
		Route:          &config.RouteSpec{APIKey: "trae-route-key", ModelPrefix: "trae/"},
	}
}

// TestTraePanelProxyUsesAdminAPIAndRouteKey trae 面板代理的两条契约。
//
// 前缀走契约表 /admin/api；凭据用渠道 route key 而不是设置页的后台密码 ——
// 后者是 zcode 的约定，用错会静默 401。
func TestTraePanelProxyUsesAdminAPIAndRouteKey(t *testing.T) {
	fake := newFakeUpstream(t, http.StatusOK, `{"accounts":[]}`)

	s, base := newTestServer(t, func(c *config.Config) {
		// 设一个不同的后台密码：若 trae 走错了凭据来源就会露馅。
		c.Claim.AdminKey = "admin-secret"
		c.Managed = append(c.Managed, traeManaged())
	})
	registerManagedAs(t, s, s.currentConfig().Managed[0], fake.srv.URL)

	resp, err := http.Get(base + "/api/channels/trae/upstream/accounts")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("trae 代理应 200，得到 %d（body=%s）", resp.StatusCode, body)
	}
	if got := fake.gotPath(); got != "/admin/api/accounts" {
		t.Errorf("应打到契约表前缀 /admin/api/accounts，得到 %q（配置里写的是 /panel/api）", got)
	}
	if got := fake.gotAuth(); got != "Bearer trae-route-key" {
		t.Errorf("trae 应用渠道 route key，得到 %q（若拿到 admin-secret 说明凭据来源错了）", got)
	}
}

// TestTraePanelProxyForwardsWrites 写操作（新增/启停/编辑/删除账号）透传。
//
// 与 zcode 一样 GET/POST/PUT/DELETE 全开放；这条防「只放了 GET」的回归。
func TestTraePanelProxyForwardsWrites(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"新增账号", http.MethodPost, "accounts", `{"tokens":["t1"]}`},
		{"启停账号", http.MethodPost, "accounts/acc-1/enabled", `{"enabled":false}`},
		{"编辑账号", http.MethodPut, "accounts/acc-1", `{"name":"改名"}`},
		{"删除账号", http.MethodDelete, "accounts", `["acc-1"]`},
		{"触发签到", http.MethodPost, "checkin", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeUpstream(t, http.StatusOK, `{"ok":true}`)

			s, base := newTestServer(t, func(c *config.Config) {
				c.Claim.AdminKey = "admin-secret"
				c.Managed = append(c.Managed, traeManaged())
			})
			registerManagedAs(t, s, s.currentConfig().Managed[0], fake.srv.URL)

			req, err := http.NewRequest(tc.method,
				base+"/api/channels/trae/upstream/"+tc.path,
				strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			got := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s 应 200，得到 %d（body=%s）", tc.method, resp.StatusCode, got)
			}
			if p := fake.gotPath(); p != "/admin/api/"+tc.path {
				t.Errorf("路径应打到 /admin/api/%s，得到 %q", tc.path, p)
			}
			if b := fake.gotBody(); b != tc.body {
				t.Errorf("请求体应逐字节透传\n  want %s\n  got  %s", tc.body, b)
			}
			if a := fake.gotAuth(); a != "Bearer trae-route-key" {
				t.Errorf("写操作同样要注入渠道 route key，得到 %q", a)
			}
		})
	}
}

// TestNativeTraeChannelRoundTrip 面板新建一条 trae 原生渠道的完整往返：
// 前端提交 → 落盘 → /api/channels/raw 回显。
//
// kind 不回显 ⇒ 平台唯一性校验失效、面板代理找不到契约前缀；
// preset 不回显 ⇒ 下拉框停在第一个选项，把 trae 显示成别的模板。
// mode 不必回显：托管渠道现在只有进程内原生一种形态，由服务端固定写死。
func TestNativeTraeChannelRoundTrip(t *testing.T) {
	_, base := newTestServer(t, nil)

	post := func(t *testing.T, body map[string]any) *http.Response {
		t.Helper()
		b, _ := json.Marshal(body)
		resp, err := http.Post(base+"/api/channels", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := post(t, map[string]any{
		"kind": "managed", "name": "trae", "display_name": "Trae",
		"mode": config.ModeNative, "gateway_kind": "trae",
		"preset": "trae", "health_path": "/healthz", "panel_path": "/admin",
		"enabled": false, "expose": false,
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("保存 trae 原生渠道应 200，得到 %d", resp.StatusCode)
	}

	out := rawManaged(t, base, "trae")
	if got := out["gateway_kind"]; got != "trae" {
		t.Errorf("落盘后 gateway_kind = %v，期望 trae", got)
	}
	if got := out["preset"]; got != "trae" {
		t.Errorf("raw.preset = %v，期望 trae", got)
	}
}

// TestNativeTraeChannelConflictsWithStoredKind 同平台唯一性。
//
// 老配置里 trae 渠道可能没存 preset，但 kind 是创建时落盘的识别结果，
// 只要落盘了 kind=trae，就拦得住。
func TestNativeTraeChannelConflictsWithStoredKind(t *testing.T) {
	_, base := newTestServer(t, func(cfg *config.Config) {
		cfg.Managed = []config.ManagedProvider{{
			Name: "trae-old", DisplayName: "Trae", Enabled: false,
			Kind: "trae", Preset: "trae", HealthPath: "/healthz",
		}}
	})
	body, _ := json.Marshal(map[string]any{
		"kind": "managed", "name": "trae-new", "display_name": "Trae 内置",
		"mode": config.ModeNative, "gateway_kind": "trae",
		"preset": "trae", "enabled": false, "expose": false,
	})
	resp, err := http.Post(base+"/api/channels", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("同平台重复添加应 409，得到 %d", resp.StatusCode)
	}
}
