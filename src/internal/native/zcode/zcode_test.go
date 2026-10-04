// zcode_test.go 原生 zcode 的**真实运行**验证。
//
// 与 workbuddy / trae 的同名测试同样的理由：装配不报错 ≠ 首个请求能跑通。
// 这里真起监听、真发 HTTP，把「装配 → 探活 → 管理面鉴权 → 账号 CRUD → 网关授权」
// 整条路径走一遍。
//
// 本文件额外钉住 zcode 合并后的三处关键语义：
//
//  1. **密码只有一处**（Track 3 的核心收益）。装配只吃渠道 route key。
//     测试验证：空 key ⇒ 管理面不鉴权（只绑回环）；给了 key ⇒ 错 key 401、对 key 200。
//  2. **`/admin/api/claim/captcha-config` 返回样本同值**（不是空配置）——
//     A6 更正过的那条：空 scene_id 会让面板的人机验证控件整块不可用。
//  3. **账号库落在实例数据目录下**，且能重复打开（用户老 data/ 指过来就能接管）。
package zcode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"modelmux/internal/config"
	"modelmux/internal/logging"
	"modelmux/internal/native"
)

// testLogger 用一个真实 logger（写到临时目录、级别 error）。原生装配会往它写
// 结构化日志，用 nil 会在第一条日志就 panic。结束时必须关掉：它持有打开的日志
// 文件，Windows 上会占住文件导致 t.TempDir 清理失败。
func testLogger(t *testing.T) *logging.Logger {
	t.Helper()
	lg, err := logging.New("error", t.TempDir(), 200, 8)
	if err != nil {
		t.Fatalf("构造 logger 失败：%v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	return lg
}

// testHost 一份「最小可用」的原生渠道配置。
func testHost(apiKey string) config.ManagedProvider {
	return config.ManagedProvider{
		Name: "zcode", DisplayName: "ZCode", Enabled: true,
		Mode: config.ModeNative, Kind: Kind,
		HealthPath: HealthPath, PanelPath: PanelPath, PanelAPIPrefix: PanelAPIPrefix,
		Route: &config.RouteSpec{
			ModelPrefix: "zcode-", Protocol: "anthropic", APIKey: apiKey,
		},
	}
}

// startNative 装配 + 起监听，并在测试结束时按与编排器相同的顺序收尾：
// 先 Close 服务（停止接受连接、等在途请求），再让 t.TempDir 清理数据目录。
func startNative(t *testing.T, host config.ManagedProvider, dataDir string) (*native.Runtime, string) {
	t.Helper()
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("建数据目录失败：%v", err)
	}
	svc, err := Boot(host, dataDir, testLogger(t))
	if err != nil {
		t.Fatalf("装配原生 zcode 失败：%v", err)
	}
	rt, err := native.Serve(host.Name, svc.Handler, testLogger(t))
	if err != nil {
		_ = svc.Close()
		t.Fatalf("起监听失败：%v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = rt.Close(ctx)
		_ = rt.Wait(ctx)
		_ = svc.Close()
	})
	return rt, rt.RootURL()
}

// httpDo 发一个请求并读回状态码 + 响应体。
func httpDo(t *testing.T, method, url, key string, body io.Reader) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求 %s %s 失败：%v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// TestBootRegistersKind 注册表里必须能按 kind 找到本实现。
func TestBootRegistersKind(t *testing.T) {
	if _, ok := native.Lookup(Kind); !ok {
		t.Fatalf("native 注册表里没有 kind=%q（Kinds=%v）", Kind, native.Kinds())
	}
}

// TestMetaEndpointIsProbeable 探活端点必须与预设里的 health_path 一致，
// 且响应体只有一个 version 键（实测靶机如此）。
func TestMetaEndpointIsProbeable(t *testing.T) {
	dir := t.TempDir()
	rt, base := startNative(t, testHost(""), dir)
	_ = rt

	st, raw := httpDo(t, http.MethodGet, base+HealthPath, "", nil)
	if st != 200 {
		t.Fatalf("GET %s 状态码 = %d，期望 200（体=%s）", HealthPath, st, raw)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("探活响应不是合法 JSON：%v（体=%s）", err, raw)
	}
	if len(m) != 1 {
		t.Fatalf("探活响应应只有 version 一个键，实际 %d 个：%s", len(m), raw)
	}
	if _, ok := m["version"]; !ok {
		t.Fatalf("探活响应缺 version 键：%s", raw)
	}
}

// TestAdminAPIEmptyKeyFallsBackToContractDefault 空 route key ⇒ 后台密码回落到
// 契约默认值 `zcode`（实测：不带 ZCODE_ADMIN_KEY 起靶机、用 `Bearer zcode` 得 200）。
//
// 这不是「不设密码」，而是**样本事实**。钉住它是因为合并密码后唯一的来源就是
// route key —— 若把空 key 当成「不鉴权」，落库的 admin_key（zcode）与鉴权基准
// （无）就不一致，面板代理发空值仍会 401。EffectiveAdminKey 是两处共用的真源。
func TestAdminAPIEmptyKeyFallsBackToContractDefault(t *testing.T) {
	dir := t.TempDir()
	host := testHost("")
	_, base := startNative(t, host, dir)

	if got := EffectiveAdminKey(host); got != "zcode" {
		t.Fatalf("EffectiveAdminKey(空 route key) = %q，期望 %q", got, "zcode")
	}
	// 空 key 时用契约默认值能过、无凭据 401。
	if st, raw := httpDo(t, http.MethodGet, base+"/admin/api/accounts", "", nil); st != 401 {
		t.Fatalf("无凭据状态码 = %d，期望 401（体=%s）", st, raw)
	}
	if st, raw := httpDo(t, http.MethodGet, base+"/admin/api/accounts", "zcode", nil); st != 200 {
		t.Fatalf("用契约默认密码状态码 = %d，期望 200（体=%s）", st, raw)
	}
}

// TestAdminAPIRejectsWrongKey 给了 key ⇒ 错 key 401、对 key 200。
func TestAdminAPIRejectsWrongKey(t *testing.T) {
	dir := t.TempDir()
	host := testHost("s3cret")
	_, base := startNative(t, host, dir)

	if got := EffectiveAdminKey(host); got != "s3cret" {
		t.Fatalf("EffectiveAdminKey = %q，期望 %q", got, "s3cret")
	}
	if st, raw := httpDo(t, http.MethodGet, base+"/admin/api/accounts", "wrong", nil); st != 401 {
		t.Fatalf("错 key 状态码 = %d，期望 401（体=%s）", st, raw)
	}
	if st, raw := httpDo(t, http.MethodGet, base+"/admin/api/accounts", "s3cret", nil); st != 200 {
		t.Fatalf("对 key 状态码 = %d，期望 200（体=%s）", st, raw)
	}
}

// TestAccountRoundTrip 账号能经管理 API 落库并被读回 —— 证明 SQLite 存储可用。
func TestAccountRoundTrip(t *testing.T) {
	dir := t.TempDir()
	_, base := startNative(t, testHost(""), dir)
	key := "zcode" // 空 route key 回落到契约默认值

	body := `{"provider":"zai","tokens":["tok-a"],"name":"acct-a"}`
	st, raw := httpDo(t, http.MethodPost, base+"/admin/api/accounts", key, stringReader(body))
	if st != 200 {
		t.Fatalf("新增账号状态码 = %d，期望 200（体=%s）", st, raw)
	}
	st, raw = httpDo(t, http.MethodGet, base+"/admin/api/accounts", key, nil)
	if st != 200 {
		t.Fatalf("读账号列表状态码 = %d，期望 200（体=%s）", st, raw)
	}
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("账号列表不是合法 JSON：%v", err)
	}
	if len(doc.Accounts) != 1 {
		t.Fatalf("账号数 = %d，期望 1（体=%s）", len(doc.Accounts), raw)
	}
}

// TestCaptchaConfigMatchesSampleValues captcha-config 必须返回样本同值，
// 不是空配置（A6 更正：空 scene_id 会让面板人机验证控件整块不可用）。
func TestCaptchaConfigMatchesSampleValues(t *testing.T) {
	dir := t.TempDir()
	_, base := startNative(t, testHost(""), dir)

	st, raw := httpDo(t, http.MethodGet, base+"/admin/api/claim/captcha-config", "zcode", nil)
	if st != 200 {
		t.Fatalf("captcha-config 状态码 = %d，期望 200（体=%s）", st, raw)
	}
	got := string(raw)
	for _, want := range []string{
		`"enabled":true`, `"scene_id":"11xygtvd"`, `"region":"cn"`, `"prefix":"no8xfe"`,
	} {
		if !contains(got, want) {
			t.Fatalf("captcha-config 响应缺 %s：%s", want, got)
		}
	}
}

// TestDatabasePersistsAcrossBoot 账号库落在实例数据目录下，且**能重复打开**
// （用户把老部署的 data/ 指过来即可接管账号）。
func TestDatabasePersistsAcrossBoot(t *testing.T) {
	dir := t.TempDir()
	_, base := startNative(t, testHost(""), dir)
	if st, raw := httpDo(t, http.MethodPost, base+"/admin/api/accounts", "zcode",
		stringReader(`{"provider":"zai","tokens":["tok-persist"],"name":"persist"}`)); st != 200 {
		t.Fatalf("新增账号失败：%d %s", st, raw)
	}
	if _, err := os.Stat(filepath.Join(dir, "accounts.db")); err != nil {
		t.Fatalf("账号库未落在实例数据目录：%v", err)
	}

	// 第二次装配同一个目录：必须能打开且读到刚才那条账号。
	_, base2 := startNative(t, testHost(""), dir)
	st, raw := httpDo(t, http.MethodGet, base2+"/admin/api/accounts", "zcode", nil)
	if st != 200 {
		t.Fatalf("重开后再读状态码 = %d（体=%s）", st, raw)
	}
	var doc struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("账号列表不是合法 JSON：%v", err)
	}
	if len(doc.Accounts) != 1 {
		t.Fatalf("重开后账号数 = %d，期望 1（体=%s）", len(doc.Accounts), raw)
	}
}

// TestGatewayRequiresKeyOnPrivateRoute 网关数据面按网关 Key 鉴权。
//
// 空 key 时行为由 gateway 包决定（本项目扩展位）；这里只钉住「配了 key 时
// 无凭据必须 401」——否则合并后网关会成为一个不校验凭据的转发入口。
func TestGatewayRequiresKeyOnPrivateRoute(t *testing.T) {
	dir := t.TempDir()
	_, base := startNative(t, testHost("gw-key"), dir)

	// 配了 route key ⇒ gateway_key 也是它（合并成一处）。无凭据必须 401。
	if st, _ := httpDo(t, http.MethodPost, base+"/v1/messages", "", stringReader(`{}`)); st != 401 {
		t.Fatalf("无凭据 POST /v1/messages 状态码 = %d，期望 401", st)
	}
}

// TestRouteKeyOverridesStoredPassword 内嵌形态下 route key 是后台密码的真源：
// 非空时**覆盖**库里已有的值（不是仅首启 seed）。
//
// 为什么必须覆盖：上游「首启写库、之后以库为准」对独立部署是对的，但内嵌形态下
// 若仍以库为准，用户在渠道设置里改 route key 就不会生效 —— 「只改一边就整块
// 401」换了方向重现。这里先把库里的密码改成别的值，再用新 route key 装配，
// 断言新值生效、旧值失效。
func TestRouteKeyOverridesStoredPassword(t *testing.T) {
	dir := t.TempDir()

	// 第一次装配：route key = "first"。
	_, base := startNative(t, testHost("first"), dir)
	if st, _ := httpDo(t, http.MethodGet, base+"/admin/api/accounts", "first", nil); st != 200 {
		t.Fatalf("首次装配后旧密码应可用，状态码 = %d", st)
	}
	// 假装用户在网关设置页把密码改成了 "stored"（走 PUT /settings）。
	if st, raw := httpDo(t, http.MethodPut, base+"/admin/api/settings", "first",
		stringReader(`{"admin_key":"stored"}`)); st != 200 {
		t.Fatalf("改密码失败：%d %s", st, raw)
	}
	if st, _ := httpDo(t, http.MethodGet, base+"/admin/api/accounts", "stored", nil); st != 200 {
		t.Fatalf("改密码后新值应可用，状态码 = %d", st)
	}

	// 第二次装配：route key 改成 "second" —— 必须覆盖库里的 "stored"。
	_, base2 := startNative(t, testHost("second"), dir)
	if st, raw := httpDo(t, http.MethodGet, base2+"/admin/api/accounts", "second", nil); st != 200 {
		t.Fatalf("route key 应覆盖库里的密码，状态码 = %d（体=%s）", st, raw)
	}
	if st, _ := httpDo(t, http.MethodGet, base2+"/admin/api/accounts", "stored", nil); st != 401 {
		t.Fatalf("被覆盖的旧密码应失效（401），实际 %d", st)
	}
}

// ── 小工具（不引 strings/bytes，保持 import 面最小）──────────

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

type sr struct {
	s string
	i int
}

func stringReader(s string) io.Reader { return &sr{s: s} }

func (r *sr) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}
