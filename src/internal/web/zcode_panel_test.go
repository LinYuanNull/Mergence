package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"modelmux/internal/config"
	"modelmux/internal/provider"
)

// zcode 面板代理的适配测试。
//
// 覆盖三件容易回归的事：
//  1. 目标前缀走契约表（/admin/api），而不是老配置里那个错值 /panel/api；
//  2. 管理凭据来自 config.Claim.AdminKey，而不是渠道 route 的 api_key；
//  3. zcode 的失败形态只开放只读、且把 401 翻译成人话。
//
// workbuddy 渠道作为对照组，确认这些适配没有误伤非 zcode 的网关。

// fakeUpstream 记录最近一次收到的路径与 Authorization，并按给定状态回包。
type fakeUpstream struct {
	srv  *httptest.Server
	hits atomic.Int32
	path atomic.Value
	auth atomic.Value
}

func newFakeUpstream(t *testing.T, status int, body string) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.path.Store(r.URL.Path)
		f.auth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) gotPath() string {
	v, _ := f.path.Load().(string)
	return v
}

func (f *fakeUpstream) gotAuth() string {
	v, _ := f.auth.Load().(string)
	return v
}

// registerManagedAs 把一个「就绪」的托管型上游直接放进注册表。
// 不真起子进程——代理只关心地址与凭据，不关心进程怎么拉起来。
func registerManagedAs(t *testing.T, s *Server, m config.ManagedProvider, root string) {
	t.Helper()
	up := provider.FromManaged(m, root, true, "")
	up.BaseURL = root + "/v1"
	up.RootURL = root
	s.reg.Reload([]provider.Upstream{up})
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// zcodeManaged 一条 zcode 渠道配置。
//
// 故意把 PanelAPIPrefix 写成 /panel/api（老配置里遗留的错值），
// 用来证明代理走的是契约表而不是这个字段。
func zcodeManaged() config.ManagedProvider {
	return config.ManagedProvider{
		Name: "zcode", DisplayName: "ZCode", Enabled: true, Preset: "zcode",
		PanelAPIPrefix: "/panel/api",
		Route:          &config.RouteSpec{APIKey: "route-key", ModelPrefix: "zc/"},
	}
}

// 1 + 2：前缀走契约表；凭据用 Claim.AdminKey。
func TestZcodePanelProxyUsesAdminAPIAndClaimKey(t *testing.T) {
	fake := newFakeUpstream(t, http.StatusOK, `{"accounts":[]}`)

	s, base := newTestServer(t, func(c *config.Config) {
		c.Claim.AdminKey = "admin-secret"
		c.Managed = append(c.Managed, zcodeManaged())
	})
	registerManagedAs(t, s, s.currentConfig().Managed[0], fake.srv.URL)

	resp, err := http.Get(base + "/api/channels/zcode/upstream/accounts")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("代理应 200，得到 %d（body=%s）", resp.StatusCode, body)
	}
	if got := fake.gotPath(); got != "/admin/api/accounts" {
		t.Errorf("应打到契约表前缀 /admin/api/accounts，得到 %q（配置里写的是 /panel/api）", got)
	}
	if got := fake.gotAuth(); got != "Bearer admin-secret" {
		t.Errorf("应注入 Claim.AdminKey，得到 %q", got)
	}
}

// 3：未配置后台密码时不发请求，直接 502 并给人话。
func TestZcodePanelProxyMissingAdminKey(t *testing.T) {
	fake := newFakeUpstream(t, http.StatusOK, `{}`)

	s, base := newTestServer(t, func(c *config.Config) {
		c.Managed = append(c.Managed, zcodeManaged()) // 不设 AdminKey
	})
	registerManagedAs(t, s, s.currentConfig().Managed[0], fake.srv.URL)

	resp, err := http.Get(base + "/api/channels/zcode/upstream/accounts")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("未配密码应 502，得到 %d", resp.StatusCode)
	}
	if !strings.Contains(body, "未配置后台密码") {
		t.Errorf("错误文案应含「未配置后台密码」，得到 %s", body)
	}
	if n := fake.hits.Load(); n != 0 {
		t.Errorf("未配密码时不应向上游发请求，实际发了 %d 次", n)
	}
}

// 4：上游 401 → 网关侧 502，且文案可行动。
func TestZcodePanelProxyTranslates401(t *testing.T) {
	fake := newFakeUpstream(t, http.StatusUnauthorized, `{"detail":"unauthorized"}`)

	s, base := newTestServer(t, func(c *config.Config) {
		c.Claim.AdminKey = "wrong-key"
		c.Managed = append(c.Managed, zcodeManaged())
	})
	registerManagedAs(t, s, s.currentConfig().Managed[0], fake.srv.URL)

	resp, err := http.Get(base + "/api/channels/zcode/upstream/accounts")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("上游 401 应翻译成 502，得到 %d", resp.StatusCode)
	}
	if !strings.Contains(body, "后台密码被网关拒绝") {
		t.Errorf("错误文案应含「后台密码被网关拒绝」，得到 %s", body)
	}
}

// 5：zcode 只代理 GET，写请求 405 且不转发。
func TestZcodePanelProxyRejectsWrites(t *testing.T) {
	fake := newFakeUpstream(t, http.StatusOK, `{}`)

	s, base := newTestServer(t, func(c *config.Config) {
		c.Claim.AdminKey = "admin-secret"
		c.Managed = append(c.Managed, zcodeManaged())
	})
	registerManagedAs(t, s, s.currentConfig().Managed[0], fake.srv.URL)

	resp, err := http.Post(base+"/api/channels/zcode/upstream/accounts",
		"application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("zcode 写请求应 405，得到 %d", resp.StatusCode)
	}
	if !strings.Contains(body, "只开放只读请求") {
		t.Errorf("错误文案应说明只读，得到 %s", body)
	}
	if n := fake.hits.Load(); n != 0 {
		t.Errorf("写请求不应转发到上游，实际发了 %d 次", n)
	}
}

// 6：workbuddy 渠道行为不变——前缀仍取配置值、凭据仍是渠道 route key。
func TestWorkBuddyPanelProxyUnchanged(t *testing.T) {
	fake := newFakeUpstream(t, http.StatusOK, `{"total":0}`)

	s, base := newTestServer(t, func(c *config.Config) {
		// 特意设置一个不同的后台密码：若 workbuddy 走错了凭据来源就会露馅。
		c.Claim.AdminKey = "admin-secret"
		c.Managed = append(c.Managed, config.ManagedProvider{
			Name: "wb", DisplayName: "WorkBuddy", Enabled: true, Preset: "workbuddy",
			PanelAPIPrefix: "/panel/api",
			Route:          &config.RouteSpec{APIKey: "route-key", ModelPrefix: "wb/"},
		})
	})
	registerManagedAs(t, s, s.currentConfig().Managed[0], fake.srv.URL)

	resp, err := http.Get(base + "/api/channels/wb/upstream/accounts")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("workbuddy 代理应 200，得到 %d（body=%s）", resp.StatusCode, body)
	}
	if got := fake.gotPath(); got != "/panel/api/accounts" {
		t.Errorf("workbuddy 仍应走配置前缀 /panel/api，得到 %q", got)
	}
	if got := fake.gotAuth(); got != "Bearer route-key" {
		t.Errorf("workbuddy 仍应用渠道 route key，得到 %q", got)
	}
}
