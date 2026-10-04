// trae_test.go 原生 trae 的**真实运行**验证。
//
// 与 workbuddy 的同名测试同样的理由：装配不报错 ≠ 首个请求能跑通。上游 handler
// 的路由表、鉴权中间件、面板子树都可能在装配之后才暴露问题（embed 资源没落位、
// panel 没挂上），只有真起监听、真发 HTTP 才看得到。编排器判就绪用的也正是
// 「发一次 HTTP 探活」，这里复刻同一条路径。
//
// 本文件额外钉住 trae 独有的两处：
//
//  1. **签到入口是本包补出来的**（上游没有 HTTP 入口），它必须与上游其它写接口
//     同口径地校验 Bearer；否则配了 route key 的渠道会多出一个不校验凭据的写入口。
//  2. **回调监听是第二监听**，默认端口 18080 是 OAuth 预注册值。测试里必须显式
//     关掉它（`callback_port: "0"`），否则并行/重复跑测试会互相抢端口。
package trae

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

// testLogger 用一个真实 logger（写到临时目录、级别 error）——原生装配会往它写
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
		Name: "trae", DisplayName: "Trae", Enabled: true,
		Mode: config.ModeNative, Kind: Kind,
		HealthPath: HealthPath, PanelPath: PanelPath, PanelAPIPrefix: PanelAPIPrefix,
		Route: &config.RouteSpec{
			ModelPrefix: "trae", Protocol: "chat", APIKey: apiKey,
		},
	}
}

// seedConfig 预置一份实例 config.json。
//
// callback_port 一律写 "0"：测试绝不能去抢 18080（OAuth 预注册端口），
// 否则同机并行跑、或本机真有个 trae2api 在跑时，测试会互相干扰。
func seedConfig(t *testing.T, dataDir, extra string) {
	t.Helper()
	body := `{"callback_port":"0"` + extra + `}`
	if err := os.WriteFile(filepath.Join(dataDir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("预置 config.json 失败：%v", err)
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

// do 发一个请求并返回状态码、响应头与响应体。
func do(t *testing.T, method, url, bearer string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求 %s %s 失败：%v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		t.Fatalf("读响应体失败：%v", err)
	}
	return resp.StatusCode, resp.Header, body
}

// TestNativeServiceEndToEnd 端到端：装配 → 起监听 → 打探活 / 管理 API / 签到 / 数据面。
func TestNativeServiceEndToEnd(t *testing.T) {
	dataDir := t.TempDir()
	seedConfig(t, dataDir, `,"api_key":"route-key"`)
	rt, _, root := startNative(t, testHost("route-key"), dataDir)

	if rt.Port() <= 0 {
		t.Fatalf("内核没给出端口：%d", rt.Port())
	}

	// ── 1) /healthz：无鉴权，编排器等就绪打的就是它
	st, _, body := do(t, http.MethodGet, root+HealthPath, "")
	if st != http.StatusOK {
		t.Fatalf("/healthz 应 200，实际 %d body=%s", st, body)
	}
	if string(body) != "ok" {
		t.Fatalf("/healthz body = %q，期望 ok", body)
	}

	// ── 2) 管理 API 真的挂上了（接缝①②打的就是 /admin/api/*）
	//
	// 注意与 workbuddy 的差别：trae 上游的**读**接口刻意无鉴权
	// （`GET /admin/api/accounts` 直接挂在 mux 上，只有写接口过 withAdminAuth），
	// 所以这里不能照抄 workbuddy 的「无凭据必须 401」。
	st, _, body = do(t, http.MethodGet, root+PanelAPIPrefix+"/accounts", "")
	if st != http.StatusOK {
		t.Fatalf("GET %s/accounts 应 200（上游读接口不鉴权），实际 %d body=%s",
			PanelAPIPrefix, st, body)
	}
	var acc struct {
		Accounts []any `json:"accounts"`
	}
	if err := json.Unmarshal(body, &acc); err != nil {
		t.Fatalf("accounts 响应不是 JSON：%v body=%s", err, body)
	}
	if len(acc.Accounts) != 0 {
		t.Fatalf("空数据目录下不该有账号：%d 个", len(acc.Accounts))
	}

	// ── 3) 签到入口：本包补出来的那条，必须校验 Bearer
	//
	// 上游没有这个路由，是本包在 handler 外面包一层 ServeMux 补的。
	// 它必须与上游其它写接口同口径——否则配了 route key 的渠道会多出一个
	// 不校验凭据的写入口，用户拿错 Key 也照样「成功」。
	checkinURL := root + CheckinPath
	if st, _, _ := do(t, http.MethodPost, checkinURL, ""); st != http.StatusUnauthorized {
		t.Fatalf("无凭据触发签到应 401，实际 %d", st)
	}
	if st, _, _ := do(t, http.MethodPost, checkinURL, "wrong-key"); st != http.StatusUnauthorized {
		t.Fatalf("错误凭据触发签到应 401，实际 %d", st)
	}
	st, _, body = do(t, http.MethodPost, checkinURL, "route-key")
	if st != http.StatusOK {
		t.Fatalf("带凭据触发签到应 200，实际 %d body=%s", st, body)
	}
	// 回执形状必须与 zcode 的领取回执一致：claim 侧的 Translated() 按这个形状解析，
	// 形状一致才能复用同一套面板渲染，不必为 trae 分叉一条平行翻译路径。
	var receipt struct {
		Outcomes []struct {
			AccountID   string `json:"account_id"`
			AccountName string `json:"account_name"`
			OK          bool   `json:"ok"`
			Message     string `json:"message"`
		} `json:"outcomes"`
		Summary struct {
			OK   int `json:"ok"`
			Fail int `json:"fail"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(body, &receipt); err != nil {
		t.Fatalf("签到回执不是 JSON：%v body=%s", err, body)
	}
	if receipt.Outcomes == nil {
		t.Fatalf("outcomes 必须是空数组而不是 null（claim 侧按数组解析）：body=%s", body)
	}
	if len(receipt.Outcomes) != 0 || receipt.Summary.OK != 0 || receipt.Summary.Fail != 0 {
		t.Fatalf("空账号池下回执应为空：body=%s", body)
	}

	// ── 4) 外层只截 POST，其余方法照旧落到上游（证明是**纯增量**）
	//
	// 上游没有 GET /admin/api/checkin 这条路由，所以必须 404 —— 若返回 200
	// 说明外层把非 POST 也吞了，那就不再是「只加一条路由」。
	if st, _, _ := do(t, http.MethodGet, checkinURL, "route-key"); st != http.StatusNotFound {
		t.Fatalf("GET 签到路径应落到上游并 404，实际 %d（外层吞了非 POST 请求）", st)
	}

	// ── 5) 数据面鉴权（接缝④转发时注入的就是这个 Bearer）
	if st, _, _ := do(t, http.MethodGet, root+"/v1/models", ""); st != http.StatusUnauthorized {
		t.Fatalf("无凭据访问 /v1/models 应 401，实际 %d", st)
	}
	st, _, body = do(t, http.MethodGet, root+"/v1/models", "route-key")
	if st != http.StatusOK {
		t.Fatalf("带凭据访问 /v1/models 应 200，实际 %d body=%s", st, body)
	}
	var ml struct {
		Object string `json:"object"`
	}
	if err := json.Unmarshal(body, &ml); err != nil || ml.Object != "list" {
		t.Fatalf("/v1/models 响应异常：err=%v body=%s", err, body)
	}

	// ── 6) 实例数据目录真的被用起来了
	//
	// 只断言「装配期就应该存在」的两项：config.json（已预置）与 auths/（账号目录）。
	// state.json 是**懒写**的——账号池没有变更就不落盘，这是上游既有语义，
	// 在这里断言它存在会把「没变更所以不写」误判成缺陷。
	if _, err := os.Stat(filepath.Join(dataDir, "config.json")); err != nil {
		t.Errorf("实例数据目录缺少 config.json：%v", err)
	}
	if fi, err := os.Stat(filepath.Join(dataDir, "auths")); err != nil || !fi.IsDir() {
		t.Errorf("实例数据目录缺少 auths/：err=%v", err)
	}
}

// TestCheckinOpenWhenNoAPIKey 未配 Key 时签到入口不鉴权。
//
// 这是**刻意**的降级，与上游其它写接口同口径（服务只绑回环）。
// 钉住它是为了说明「401 只在配了 Key 时出现」，避免有人把 401 当成恒定行为。
func TestCheckinOpenWhenNoAPIKey(t *testing.T) {
	dataDir := t.TempDir()
	seedConfig(t, dataDir, "") // 不写 api_key
	_, _, root := startNative(t, testHost(""), dataDir)

	st, _, body := do(t, http.MethodPost, root+CheckinPath, "")
	if st != http.StatusOK {
		t.Fatalf("未配 Key 时签到入口应开放（200），实际 %d body=%s", st, body)
	}
}

// TestCallbackServerDisabledByZeroPort callback_port 为 "0" 时不应当起第二监听。
//
// 为什么必须能关掉：TRAE 的 OAuth redirect_uri 是**预注册**的固定端口
// （默认 18080），测试若真去绑它，就会与本机可能在跑的 trae2api 抢端口，
// 也会让并行测试互相干扰。这里直接钉住「0 = 不起」。
func TestCallbackServerDisabledByZeroPort(t *testing.T) {
	lg := testLogger(t)
	if cs := startCallbackServer("0", http.NewServeMux(), lg, "t"); cs != nil {
		t.Fatal(`callback_port="0" 不该起监听`)
	}
	if cs := startCallbackServer("", http.NewServeMux(), lg, "t"); cs != nil {
		t.Fatal(`callback_port="" 不该起监听`)
	}
}

// TestRouteKeyOverridesFileKey 路由 Key 与实例 config.json 里 api_key 的优先级。
//
// 规则很容易被「顺手改成先读文件」而破坏，后果是：用户在前端渠道设置里改的 Key
// 不生效（面板代理与数据面都拿旧 Key → 全 401），而配置文件里明明写着对的值。
//
//	渠道填了 route.api_key  → 它说了算（ModelMux 是唯一配置源）
//	渠道没填                → 用实例 config.json 里的（兼容直接复用原目录）
func TestRouteKeyOverridesFileKey(t *testing.T) {
	t.Run("渠道填了 Key，覆盖实例配置里的值", func(t *testing.T) {
		dataDir := t.TempDir()
		seedConfig(t, dataDir, `,"api_key":"file-key"`)
		_, _, root := startNative(t, testHost("route-key"), dataDir)

		if st, _, _ := do(t, http.MethodGet, root+"/v1/models", "file-key"); st != http.StatusUnauthorized {
			t.Fatalf("渠道填了 Key 时，实例文件里的旧 Key 应失效，实际 %d", st)
		}
		if st, _, _ := do(t, http.MethodGet, root+"/v1/models", "route-key"); st != http.StatusOK {
			t.Fatalf("渠道填的 Key 应生效，实际 %d", st)
		}
	})

	t.Run("渠道没填 Key，沿用实例配置里的值", func(t *testing.T) {
		dataDir := t.TempDir()
		seedConfig(t, dataDir, `,"api_key":"file-key"`)
		_, _, root := startNative(t, testHost(""), dataDir)

		if st, _, _ := do(t, http.MethodGet, root+"/v1/models", "file-key"); st != http.StatusOK {
			t.Fatalf("渠道没填 Key 时应沿用实例配置里的，实际 %d", st)
		}
	})
}

// TestBootKeepsExistingConfigFile 已存在的 config.json 必须**逐字节不动**。
//
// 用户把数据目录指到原来 trae2api-web 的目录就是为了沿用账号与设置。若装配时把
// 「缺失字段补默认值」的结果写回去，用户那些没被本实现认识的键（上游新加的、
// 或他手工加的）会被静默抹掉，而且下次启动行为就变了。
func TestBootKeepsExistingConfigFile(t *testing.T) {
	dataDir := t.TempDir()
	seed := filepath.Join(dataDir, "config.json")
	original := []byte("{\n  \"api_key\": \"file-key\",\n  \"callback_port\": \"0\",\n" +
		"  \"future_key_we_do_not_know\": 42\n}\n")
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

// TestCloseReleasesListener 收尾之后端口必须真的不可连。
//
// 编排器的 Restart 是「Stop 完再 Start」。若 Close 只关 handler 不关监听，
// 旧实例会继续响应，表现为「重启成功但行为没变」——极难定位，必须断言。
func TestCloseReleasesListener(t *testing.T) {
	dataDir := t.TempDir()
	seedConfig(t, dataDir, "")
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
