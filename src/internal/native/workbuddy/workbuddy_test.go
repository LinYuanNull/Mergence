// workbuddy_test.go 原生 workbuddy 的**真实运行**验证。
//
// 为什么这些用例必须真起监听、真发 HTTP，而不是只断言对象图构造成功：
//
//   - 「装配不报错」与「首个请求能跑通」是两件事。上游 handler 的路由表、
//     鉴权中间件、面板子树都可能在装配之后才暴露问题（比如 embed 资源没落位、
//     panel 没挂上），只有真的打一次才看得到。
//   - 编排器判就绪用的也正是「发一次 HTTP 探活」。这里复刻同一条路径，
//     才能保证「编排器认为就绪」时接缝真的能用。
//   - 收尾（停排程 → 落盘 → 关库）与监听释放同样只有真跑一遍才能验。
package workbuddy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mergence/internal/config"
	"mergence/internal/logging"
	"mergence/internal/native"

	"mergence/internal/provider/workbuddy/server"
)

// testLogger 用一个真实 logger（写到临时目录、级别 error），
// 因为原生装配会往它写结构化日志；用 nil 会在第一条日志就 panic。
//
// 必须在测试结束前关掉它：logger 持有打开的日志文件，Windows 上会占住文件，
// 导致 t.TempDir 的清理失败（报 “being used by another process”）。
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
		Name: "wb", DisplayName: "WorkBuddy", Enabled: true,
		Mode: config.ModeNative, Kind: Kind,
		HealthPath: HealthPath, PanelPath: PanelPath, PanelAPIPrefix: PanelAPIPrefix,
		Route: &config.RouteSpec{
			ModelPrefix: "wb", Protocol: "chat", APIKey: apiKey,
		},
	}
}

// startNative 装配 + 起监听，并在测试结束时按与编排器相同的顺序收尾。
func startNative(t *testing.T, host config.ManagedProvider, dataDir string) (*native.Runtime, *native.Service, string) {
	t.Helper()
	lg := testLogger(t)

	svc, err := Boot(host, dataDir, lg)
	if err != nil {
		t.Fatalf("Boot 失败：%v", err)
	}
	rt, err := native.Serve(host.Name, svc.Handler, lg)
	if err != nil {
		_ = svc.Close()
		t.Fatalf("Serve 失败：%v", err)
	}
	t.Cleanup(func() {
		// 与 orchestrator.nativeHandle.Close 同序：先停监听（drain 在途），
		// 再收上游。顺序反了会与落盘抢同一批文件。
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = rt.Close(ctx)
		_ = svc.Close()
	})
	return rt, svc, rt.RootURL()
}

// get 发一个 GET 并返回状态码、响应头与响应体。
func get(t *testing.T, url, bearer string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求 %s 失败：%v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		t.Fatalf("读响应体失败：%v", err)
	}
	return resp.StatusCode, resp.Header, body
}

// TestNativeServiceEndToEnd 端到端：装配 → 起监听 → 打上游的探活 / 鉴权 / 管理 API。
func TestNativeServiceEndToEnd(t *testing.T) {
	dataDir := t.TempDir()
	rt, _, root := startNative(t, testHost("local-key"), dataDir)

	if rt.Port() <= 0 {
		t.Fatalf("内核没给出端口：%d", rt.Port())
	}

	// ── 1) /healthz：无鉴权，且必须带网关身份标识
	//
	// 账号池是空的，所以状态码是 503（「起来了但没数据」）。这正是编排器
	// 把「任意 HTTP 响应都算就绪」的原因，也是这里不能断言 200 的原因。
	st, hdr, body := get(t, root+HealthPath, "")
	if st != http.StatusOK && st != http.StatusServiceUnavailable {
		t.Fatalf("/healthz 状态码异常：%d body=%s", st, body)
	}
	if got := hdr.Get("X-Service"); got != server.ServiceName {
		t.Fatalf("/healthz 的 X-Service = %q，want %q", got, server.ServiceName)
	}
	var hz struct {
		Service string `json:"service"`
		Total   int    `json:"total"`
		Healthy int    `json:"healthy"`
	}
	if err := json.Unmarshal(body, &hz); err != nil {
		t.Fatalf("/healthz 响应不是 JSON：%v body=%s", err, body)
	}
	if hz.Service != server.ServiceName {
		t.Fatalf("/healthz 的 service = %q，want %q", hz.Service, server.ServiceName)
	}
	if hz.Total != 0 || hz.Healthy != 0 {
		t.Fatalf("空账号池下 total/healthy 应为 0/0，实际 %d/%d", hz.Total, hz.Healthy)
	}

	// ── 2) 管理 API 真的挂上了（接缝①②打的就是 /panel/api/*）
	//
	// 没有 Bearer 必须 401 —— 这条同时证明了鉴权中间件生效、
	// 以及 Mergence 侧「密钥由服务端注入」的做法是有意义的。
	if st, _, b := get(t, root+PanelAPIPrefix+"/overview", ""); st != http.StatusUnauthorized {
		t.Fatalf("无凭据访问管理 API 应 401，实际 %d body=%s", st, b)
	}
	st, _, body = get(t, root+PanelAPIPrefix+"/overview", "local-key")
	if st != http.StatusOK {
		t.Fatalf("带凭据访问管理 API 应 200，实际 %d body=%s", st, body)
	}
	var ov struct {
		Version      string `json:"version"`
		AuthRequired bool   `json:"auth_required"`
		RedisMode    string `json:"redis_mode"`
		Accounts     []any  `json:"accounts"`
		Total        int    `json:"total"`
	}
	if err := json.Unmarshal(body, &ov); err != nil {
		t.Fatalf("overview 响应不是 JSON：%v body=%s", err, body)
	}
	if ov.Version != upstreamVersion {
		t.Fatalf("overview.version = %q，want %q", ov.Version, upstreamVersion)
	}
	if !ov.AuthRequired {
		t.Fatal("配了 api_key 时 auth_required 应为 true")
	}
	// 未配置 Upstash → 必须**降级**而不是失败：这是「内嵌不要求用户装 Redis」的依据。
	if ov.RedisMode != "noop" {
		t.Fatalf("未配置 Upstash 时 redis_mode 应为 noop，实际 %q", ov.RedisMode)
	}
	if len(ov.Accounts) != 0 || ov.Total != 0 {
		t.Fatalf("空数据目录下不该有账号：accounts=%d total=%d", len(ov.Accounts), ov.Total)
	}

	// ── 3) 模型列表接口（ConsoleKind 探测打的就是它）
	//
	// 空账号池下它返回 503「没有可用账号」——这恰好是想要的证据：
	// 路由**存在**且真的执行到了业务判断，而不是 404（没挂上）或 500（装配坏了）。
	// 探测 ConsoleKind 时上游非 200 就等于「不是 gateway 形态」，所以这里
	// 只钉住「不是 404/500 且错误体可解析」。
	st, _, body = get(t, root+PanelAPIPrefix+"/models", "local-key")
	if st != http.StatusOK && st != http.StatusServiceUnavailable {
		t.Fatalf("管理 API /models 应为 200 或 503，实际 %d（其它值说明路由没挂上或装配坏了）：body=%s", st, body)
	}
	var mErr struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &mErr); err != nil {
		t.Fatalf("管理 API /models 响应不是 JSON：%v body=%s", err, body)
	}
	if st == http.StatusServiceUnavailable && mErr.Error == "" {
		t.Fatalf("503 必须带可读原因：body=%s", body)
	}

	// ── 4) 数据面带鉴权（接缝④转发时注入的就是这个 Bearer）
	if st, _, _ := get(t, root+"/v1/models", ""); st != http.StatusUnauthorized {
		t.Fatalf("无凭据访问 /v1/models 应 401，实际 %d", st)
	}
	st, _, body = get(t, root+"/v1/models", "local-key")
	if st != http.StatusOK {
		t.Fatalf("带凭据访问 /v1/models 应 200，实际 %d body=%s", st, body)
	}
	var ml struct {
		Object string `json:"object"`
	}
	if err := json.Unmarshal(body, &ml); err != nil || ml.Object != "list" {
		t.Fatalf("/v1/models 响应异常：err=%v body=%s", err, body)
	}

	// ── 5) 实例数据目录真的被用起来了
	//
	// 只断言「装配期就应该存在」的两项：config.json（首启落一份默认配置）
	// 与 auths/（账号目录）。state.json / usage.json 是**懒写**的——
	// 账号池没有变更、没有请求流过就不落盘，这是上游的既有语义，
	// 在这里断言它们存在会把「没变更所以不写」误判成缺陷。
	if _, err := os.Stat(filepath.Join(dataDir, "config.json")); err != nil {
		t.Errorf("实例数据目录缺少 config.json：%v", err)
	}
	if fi, err := os.Stat(filepath.Join(dataDir, "auths")); err != nil || !fi.IsDir() {
		t.Errorf("实例数据目录缺少 auths/：err=%v", err)
	}
}

// TestCloseReleasesListener 收尾之后端口必须真的不可连。
//
// 编排器的 Restart 是「Stop 完再 Start」。若 Close 只关 handler 不关监听，
// 旧实例会继续响应，表现为「重启成功但行为没变」——极难定位，必须断言。
func TestCloseReleasesListener(t *testing.T) {
	dataDir := t.TempDir()
	rt, svc, root := startNative(t, testHost("k"), dataDir)
	port := rt.Port()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := rt.Close(ctx); err != nil {
		t.Fatalf("关闭监听失败：%v", err)
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("收尾失败：%v", err)
	}
	if err := rt.Wait(ctx); err != nil {
		t.Fatalf("Serve 循环未在关闭后退出：%v", err)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	if _, err := client.Get(root); err == nil {
		t.Fatalf("关闭后 %d 端口仍可连接：监听没释放", port)
	}
	// 幂等：编排器的 Stop 与 Shutdown 可能在同一次生命周期里都调到它。
	if err := rt.Close(ctx); err != nil {
		t.Fatalf("重复关闭应无错，实际：%v", err)
	}
	if err := svc.Close(); err != nil {
		t.Fatalf("重复收尾应无错，实际：%v", err)
	}
}

// TestRouteKeyOverridesFileKey 路由 Key 与实例 config.json 里 api_key 的优先级。
//
// 这条规则很容易被「顺手改成先读文件」而破坏，后果是：用户在前端渠道设置里
// 改的 Key 不生效（面板代理与数据面都拿旧 Key → 全 401），而配置文件里明明写着对的值。
// 所以两个方向都要钉住：
//
//	渠道填了 route.api_key  → 它说了算（Mergence 是唯一配置源）
//	渠道没填                → 用实例 config.json 里的（兼容直接复用原 wb2api 目录）
func TestRouteKeyOverridesFileKey(t *testing.T) {
	t.Run("渠道填了 Key，覆盖实例配置里的值", func(t *testing.T) {
		dataDir := t.TempDir()
		// 预置一份实例配置，模拟「直接沿用原来 wb2api 的目录」。
		// 落盘格式与上游 config.json 一致，因此这里可以手写。
		seed := filepath.Join(dataDir, "config.json")
		if err := os.WriteFile(seed, []byte(`{"api_key":"file-key"}`), 0o644); err != nil {
			t.Fatalf("预置 config.json 失败：%v", err)
		}
		_, _, root := startNative(t, testHost("route-key"), dataDir)

		if st, _, _ := get(t, root+PanelAPIPrefix+"/overview", "file-key"); st != http.StatusUnauthorized {
			t.Fatalf("渠道填了 Key 时，实例文件里的旧 Key 应失效，实际 %d", st)
		}
		if st, _, _ := get(t, root+PanelAPIPrefix+"/overview", "route-key"); st != http.StatusOK {
			t.Fatalf("渠道填的 Key 应生效，实际 %d", st)
		}
	})

	t.Run("渠道没填 Key，沿用实例配置里的值", func(t *testing.T) {
		dataDir := t.TempDir()
		seed := filepath.Join(dataDir, "config.json")
		if err := os.WriteFile(seed, []byte(`{"api_key":"file-key"}`), 0o644); err != nil {
			t.Fatalf("预置 config.json 失败：%v", err)
		}
		_, _, root := startNative(t, testHost(""), dataDir)
		if st, _, _ := get(t, root+PanelAPIPrefix+"/overview", "file-key"); st != http.StatusOK {
			t.Fatalf("渠道没填 Key 时应沿用实例配置里的，实际 %d", st)
		}
	})
}

// TestBootKeepsExistingConfigFile 已存在的 config.json 必须**逐字节不动**。
//
// 为什么这条重要：用户把实例数据目录指到原来 wb2api 的目录就是为了沿用账号与
// 设置。若装配时把「缺失字段补默认值」的结果写回去，用户那些没被本实现认识的
// 键（上游新加的、或他手工加的）会被静默抹掉，而且下次启动行为就变了。
func TestBootKeepsExistingConfigFile(t *testing.T) {
	dataDir := t.TempDir()
	seed := filepath.Join(dataDir, "config.json")
	original := []byte("{\n  \"api_key\": \"file-key\",\n  \"soft_rate\": \"900s\",\n  \"future_key_we_do_not_know\": 42\n}\n")
	if err := os.WriteFile(seed, original, 0o644); err != nil {
		t.Fatalf("预置 config.json 失败：%v", err)
	}

	startNative(t, testHost(""), dataDir)

	got, err := os.ReadFile(seed)
	if err != nil {
		t.Fatalf("读取 config.json 失败：%v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("装配改写了已存在的 config.json：\nbefore=%q\nafter =%q", original, got)
	}
}
