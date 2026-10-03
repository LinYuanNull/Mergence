package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modelmux/internal/config"
	"modelmux/internal/provider"
)

// zcode 面板代理的适配测试。
//
// 覆盖四件容易回归的事：
//  1. 目标前缀走契约表（/admin/api），而不是老配置里那个错值 /panel/api；
//  2. 管理凭据来自 config.Claim.AdminKey，而不是渠道 route 的 api_key；
//  3. 写操作（POST/PUT/DELETE）与方法、请求体一起透传，且 401 仍翻译成人话；
//  4. 上游等待上限按方法/路径分档（领取最宽）。
//
// workbuddy 渠道作为对照组，确认这些适配没有误伤非 zcode 的网关。

// fakeUpstream 记录最近一次收到的路径、方法、请求体与 Authorization，并按给定状态回包。
type fakeUpstream struct {
	srv     *httptest.Server
	hits    atomic.Int32
	path    atomic.Value
	auth    atomic.Value
	method  atomic.Value
	reqBody atomic.Value
}

func newFakeUpstream(t *testing.T, status int, body string) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.path.Store(r.URL.Path)
		f.auth.Store(r.Header.Get("Authorization"))
		f.method.Store(r.Method)
		b, _ := io.ReadAll(r.Body)
		f.reqBody.Store(string(b))
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

func (f *fakeUpstream) gotMethod() string {
	v, _ := f.method.Load().(string)
	return v
}

func (f *fakeUpstream) gotBody() string {
	v, _ := f.reqBody.Load().(string)
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

// 5：写操作透传——方法、路径、请求体一个都不改，凭据照旧注入。
func TestZcodePanelProxyForwardsWrites(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"新增账号", http.MethodPost, "accounts", `{"provider":"zai","tokens":["t1","t2"]}`},
		{"启停账号", http.MethodPost, "accounts/acc-1/enabled", `{"enabled":false}`},
		{"编辑账号", http.MethodPut, "accounts/acc-1", `{"name":"改名后的账号"}`},
		{"删除账号", http.MethodDelete, "accounts", `["acc-1","acc-2"]`},
		{"领取套餐", http.MethodPost, "claim", `{"account_ids":["acc-1"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := newFakeUpstream(t, http.StatusOK, `{"ok":true}`)

			s, base := newTestServer(t, func(c *config.Config) {
				c.Claim.AdminKey = "admin-secret"
				c.Managed = append(c.Managed, zcodeManaged())
			})
			registerManagedAs(t, s, s.currentConfig().Managed[0], fake.srv.URL)

			req, err := http.NewRequest(tc.method,
				base+"/api/channels/zcode/upstream/"+tc.path,
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
			if m := fake.gotMethod(); m != tc.method {
				t.Errorf("方法应原样转发为 %s，得到 %s", tc.method, m)
			}
			if p := fake.gotPath(); p != "/admin/api/"+tc.path {
				t.Errorf("路径应打到 /admin/api/%s，得到 %q", tc.path, p)
			}
			if b := fake.gotBody(); b != tc.body {
				t.Errorf("请求体应逐字节透传\n  want %s\n  got  %s", tc.body, b)
			}
			if a := fake.gotAuth(); a != "Bearer admin-secret" {
				t.Errorf("写操作同样要注入 Claim.AdminKey，得到 %q", a)
			}
		})
	}
}

// 6：写操作的 401 同样要翻译成人话——放开方法不等于丢掉这条。
func TestZcodePanelProxyTranslates401OnWrite(t *testing.T) {
	fake := newFakeUpstream(t, http.StatusUnauthorized, `{"detail":"unauthorized"}`)

	s, base := newTestServer(t, func(c *config.Config) {
		c.Claim.AdminKey = "wrong-key"
		c.Managed = append(c.Managed, zcodeManaged())
	})
	registerManagedAs(t, s, s.currentConfig().Managed[0], fake.srv.URL)

	resp, err := http.Post(base+"/api/channels/zcode/upstream/accounts",
		"application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("写操作遇到上游 401 应翻译成 502，得到 %d", resp.StatusCode)
	}
	if !strings.Contains(body, "后台密码被网关拒绝") {
		t.Errorf("错误文案应含「后台密码被网关拒绝」，得到 %s", body)
	}
}

// 7：上游等待上限分档。领取要解验证码，必须拿最宽的那一档——
// 用只读的 30s 会把它误杀成超时。
func TestProxyTimeoutForTiers(t *testing.T) {
	cases := []struct {
		method, rest string
		want         time.Duration
	}{
		{http.MethodGet, "accounts", upstreamProxyTimeout},
		{http.MethodGet, "monitoring", upstreamProxyTimeout},
		{http.MethodGet, "login/poll/abc", upstreamProxyTimeout},
		{http.MethodPost, "accounts", upstreamProxyWriteTimeout},
		{http.MethodPut, "accounts/acc-1", upstreamProxyWriteTimeout},
		{http.MethodDelete, "accounts", upstreamProxyWriteTimeout},
		{http.MethodPost, "import", upstreamProxyWriteTimeout},
		{http.MethodPost, "login/start", upstreamProxyWriteTimeout},
		// 领取最宽，且前缀不区分方法、也不怕前导斜杠
		{http.MethodPost, "claim", upstreamProxyClaimTimeout},
		{http.MethodGet, "claim/preview", upstreamProxyClaimTimeout},
		{http.MethodPost, "/claim/manual", upstreamProxyClaimTimeout},
	}
	for _, tc := range cases {
		if got := proxyTimeoutFor(tc.method, tc.rest); got != tc.want {
			t.Errorf("proxyTimeoutFor(%s, %q) = %v，want %v", tc.method, tc.rest, got, tc.want)
		}
	}
}

// 8：workbuddy 渠道行为不变——前缀仍取配置值、凭据仍是渠道 route key。
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
