// metrics_api_test.go 概览指标接口的解析与状态处理。
//
// 重点：各种「拿不到数据」的情况必须各自可区分。
// 把它们混成一种（都显示 0 或都显示「失败」）会让用户
// 误判问题出在 Mergence 上，而实际上可能是子进程没起、
// 上游没这个接口、或者真的就是还没有用量。
package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"mergence/internal/config"
	"mergence/internal/logging"
	"mergence/internal/metrics"
	"mergence/internal/orchestrator"
	"mergence/internal/provider"
)

func TestParseUpstreamUsageStructured(t *testing.T) {
	raw := []byte(`{"totals":{"requests":4,"errors":1,"prompt_tokens":8000,
		"completion_tokens":500,"total_tokens":8500,"cache_hit_tokens":6000,
		"cache_miss_tokens":2000,"cache_hit_rate":75,"credits":0.42,
		"credits_per_1m_tokens":49.4,"avg_latency_ms":1200,"avg_tokens_per_second":31.5},
		"by_account":[{"key":"acc-1","extra":"账号A","requests":3,
		"total_tokens":6000,"credits":0.3,"cache_hit_tokens":5000,"cache_miss_tokens":1000},
		{"key":"acc-2","extra":"账号B","requests":1,"total_tokens":2500,"credits":0.12}],
		"by_model":[{"key":"auto","requests":2,"total_tokens":1000,"credits":0.05}]}`)

	u, err := parseUpstreamUsage(raw)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if !u.Available {
		t.Error("有数据时应标记 Available")
	}
	if u.Requests != 4 || u.Errors != 1 {
		t.Errorf("请求/错误 = %d/%d，期望 4/1", u.Requests, u.Errors)
	}
	if u.TotalTokens != 8500 {
		t.Errorf("总 token = %d，期望 8500", u.TotalTokens)
	}
	if u.CachedTokens != 6000 {
		t.Errorf("缓存 token = %d，期望 6000", u.CachedTokens)
	}
	if u.Credits != 0.42 {
		t.Errorf("积分 = %v，期望 0.42", u.Credits)
	}
	if !u.RateKnown || u.CacheHitRate != 75 {
		t.Errorf("缓存命中率 = %v（known=%v），期望 75", u.CacheHitRate, u.RateKnown)
	}
	if u.Unit != "credits" {
		t.Errorf("单位 = %q，期望 credits", u.Unit)
	}
	if len(u.Accounts) != 2 {
		t.Fatalf("账号维度条目 = %d，期望 2", len(u.Accounts))
	}
	// 第二个账号没给缓存字段 → 不应有命中率
	if u.Accounts[1].RateKnown {
		t.Error("没有缓存数据的账号不应给出命中率")
	}
	if u.Accounts[0].Label != "账号A" {
		t.Errorf("账号标签 = %q，期望 账号A", u.Accounts[0].Label)
	}
}

func TestParseUpstreamUsageRateComputed(t *testing.T) {
	// 上游没直接给 cache_hit_rate，但给了 hit/miss → 应现算
	raw := []byte(`{"totals":{"requests":1,"cache_hit_tokens":300,"cache_miss_tokens":700}}`)
	u, err := parseUpstreamUsage(raw)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if !u.RateKnown {
		t.Fatal("有 hit/miss 时应能算出命中率")
	}
	if u.CacheHitRate < 0.299 || u.CacheHitRate > 0.301 {
		t.Errorf("命中率 = %v，期望 0.3", u.CacheHitRate)
	}
}

func TestParseUpstreamUsageNoRateData(t *testing.T) {
	// 完全没有缓存字段 → 不给命中率（而不是给 0）
	raw := []byte(`{"totals":{"requests":1,"total_tokens":100}}`)
	u, err := parseUpstreamUsage(raw)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if u.RateKnown {
		t.Error("上游没给缓存数据时不应给出命中率")
	}
}

func TestParseUpstreamUsageZeroIsKnown(t *testing.T) {
	// 显式给了 cache_hit_rate: 0 → 这是「确实一次没命中」，要给 0
	raw := []byte(`{"totals":{"requests":1,"cache_hit_rate":0,"cache_hit_tokens":0}}`)
	u, _ := parseUpstreamUsage(raw)
	if !u.RateKnown {
		t.Error("上游显式给了命中率 0，known 应为 true（区别于「没给」）")
	}
}

func TestParseUpstreamUsageNoData(t *testing.T) {
	// 结构对但全是零：Available 应为 false，让面板显示「暂无用量」
	raw := []byte(`{"totals":{"requests":0,"total_tokens":0,"credits":0}}`)
	u, err := parseUpstreamUsage(raw)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if u.Available {
		t.Error("全零用量不应标记 Available")
	}
}

func TestParseUpstreamUsageBadShape(t *testing.T) {
	// 完全不认识的形状 → 明确报错，不能静默当成「零用量」
	for _, raw := range []string{`{"foo":1}`, `[]`, `not json`, `{}`} {
		if _, err := parseUpstreamUsage([]byte(raw)); err == nil {
			t.Errorf("无法识别的形状 %s 应返回错误", raw)
		}
	}
}

func TestHandleMetricsBadDays(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	s.handleMetrics(w, httptest.NewRequest(http.MethodGet, "/api/metrics?days=abc", nil))
	if w.Code != http.StatusBadRequest {
		t.Errorf("非法 days 应返回 400，实际 %d", w.Code)
	}
}

func TestHandleMetricsEmptyStore(t *testing.T) {
	// 没有计量记录时必须返回 has_data=false 而不是一堆 0
	s := newTestMetricsServer(t)
	w := httptest.NewRecorder()
	s.handleMetrics(w, httptest.NewRequest(http.MethodGet, "/api/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", w.Code)
	}
	var resp MetricsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if resp.Local.HasData {
		t.Error("无记录时 has_data 应为 false")
	}
	if resp.PriceDate == "" {
		t.Error("应返回价格表采集时间，供面板提示")
	}
}

func TestKindOf(t *testing.T) {
	// WorkBuddy 渠道应识别为 workbuddy（决定用积分口径）
	if k := kindOf(upstreamForTest("workbuddy", "WorkBuddy 网关", "workbuddy")); k != "workbuddy" {
		t.Errorf("kind = %q，期望 workbuddy", k)
	}
	if k := kindOf(upstreamForTest("wb2a", "wb2api", "")); k != "workbuddy" {
		t.Errorf("kind = %q，期望 workbuddy", k)
	}
	// 拿不准时返回空，由面板显示中性单位（而不是猜一个错的）
	if k := kindOf(upstreamForTest("newapi", "new-api", "new-api")); k != "" {
		t.Errorf("未知渠道的 kind 应为空，实际 %q", k)
	}
}

func TestUpstreamUsageErrorStatesDistinct(t *testing.T) {
	// 三种失败必须给出三种可区分的原因
	if errNotManaged.Error() == errNoRoot.Error() {
		t.Error("不同的失败原因不应共用同一句话")
	}
	if errBadUsageShape.Error() == errUpstreamStatus(500).Error() {
		t.Error("结构错误与状态码错误不应混为一谈")
	}
}

func TestMetricsResponseJSONShape(t *testing.T) {
	// 确认前端要用的字段都在，且未知状态用 null/空而不是假数字
	s := newTestMetricsServer(t)
	s.store.Record(metricsRecordForTest("oai", "embedded", "oai/gpt-4o", 1000, 200, 400))
	w := httptest.NewRecorder()
	s.handleMetrics(w, httptest.NewRequest(http.MethodGet, "/api/metrics?days=7", nil))
	var resp MetricsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if !resp.Local.HasData {
		t.Fatal("有记录时 has_data 应为 true")
	}
	if resp.Local.Total.Requests != 1 {
		t.Errorf("调用次数 = %d，期望 1", resp.Local.Total.Requests)
	}
	if !resp.Local.Total.Cost.Known {
		t.Error("gpt-4o 应算出费用")
	}
	// 缓存命中率：600 未缓存 + 400 缓存 = 40%
	if !resp.Local.Total.RateKnown {
		t.Fatal("有输入 token 时应有缓存命中率")
	}
	if resp.Local.Total.CacheHitRate < 0.39 || resp.Local.Total.CacheHitRate > 0.41 {
		t.Errorf("缓存命中率 = %v，期望约 0.4", resp.Local.Total.CacheHitRate)
	}
}

func TestMetricsRecentLimit(t *testing.T) {
	s := newTestMetricsServer(t)
	for i := 0; i < 20; i++ {
		s.store.Record(metricsRecordForTest("oai", "embedded", "oai/gpt-4o", 100, 10, 0))
	}
	w := httptest.NewRecorder()
	s.handleMetrics(w, httptest.NewRequest(http.MethodGet, "/api/metrics", nil))
	var resp MetricsResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Local.Recent) != 20 {
		t.Errorf("最近调用数 = %d，期望 20", len(resp.Local.Recent))
	}
}

// newTestMetricsServer 起一个不监听端口的服务（只测 handler，不需要真实 HTTP）。
func newTestMetricsServer(t *testing.T) *Server {
	t.Helper()
	lg, err := logging.New("error", t.TempDir(), 200, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	home := t.TempDir()
	s := New(lg, orchestrator.New(t.TempDir(), lg, nil), provider.NewRegistry(lg), home)
	s.SetConfig(config.Default(), filepath.Join(home, "mergence.json"))
	t.Cleanup(func() { _ = s.store.Close() })
	return s
}

func metricsRecordForTest(channel, source, model string, prompt, completion, cached int64) metrics.Record {
	return metrics.Record{
		Time: time.Now(), Channel: channel, Source: source, Model: model,
		UpstreamModel: model, OK: true, Status: 200,
		Usage: metrics.Usage{
			PromptTokens: prompt, CompletionTokens: completion,
			TotalTokens: prompt + completion, CachedTokens: cached,
		},
	}
}

func upstreamForTest(name, display, preset string) provider.Upstream {
	return provider.Upstream{Name: name, DisplayName: display, Preset: preset}
}

func TestUpstreamSpendEstimate(t *testing.T) {
	// 花费估算 = 积分 × 每积分价值；未配置 credit_value 时必须 SpendKnown=false，
	// 不能按 0 元展示（0 元会被读成「没花钱」）。
	u, err := parseUpstreamUsage([]byte(`{"totals":{"requests":2,"credits":0.02}}`))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	// 未配置单价
	if u.CreditValue != 0 || u.SpendKnown {
		t.Errorf("未配置单价时不应有花费估算：%+v", u)
	}
	// 配了单价后由 web 层计算：0.02 积分 × 0.000099 元 = 0.00000198 元
	spend := u.Credits * 0.000099
	if spend <= 0 {
		t.Errorf("估算应大于 0：%v", spend)
	}
}

func TestUpstreamModelNamesNormalized(t *testing.T) {
	// 上游 usage 返回的模型名是原始 ID（hy4-preview-f、cn:glm-5.3-flash），
	// 面板上必须显示规范化后的名字——概览里的名字要能和 /v1/models 对上，
	// 否则用户照着表格填客户端会找不到模型。
	raw := []byte(`{"totals":{"requests":4,"total_tokens":100},
		"by_model":[
			{"key":"hy4-preview-f","requests":2,"total_tokens":40},
			{"key":"cn:glm-5.3-flash","requests":1,"total_tokens":30},
			{"key":"hy3-x","requests":1,"total_tokens":30},
			{"key":"auto","requests":1,"total_tokens":10}],
		"by_account":[{"key":"8a0bdbb6-0408-4312-a03c-9d6ec7f8926c","label":"账号A","requests":4}]}`)
	u, err := parseUpstreamUsage(raw)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	got := map[string]string{}
	for _, m := range u.Models {
		got[m.Key] = m.RawKey
	}
	want := map[string]string{
		"Hy4-Preview-Free": "hy4-preview-f",
		"GLM-5.3-Flash":    "cn:glm-5.3-flash",
		"Hy3-Free":         "hy3-x", // -x 是免费档位
		"Auto":             "auto",
	}
	for k, v := range want {
		rawKey, ok := got[k]
		if !ok {
			t.Errorf("缺少规范化后的模型名 %q（实际有 %v）", k, got)
			continue
		}
		if rawKey != v {
			t.Errorf("模型 %q 的原始 ID = %q，期望 %q", k, rawKey, v)
		}
	}
	// 账号 key 是账号标识（uuid），绝不能被模型名规则改动
	if len(u.Accounts) != 1 ||
		u.Accounts[0].Key != "8a0bdbb6-0408-4312-a03c-9d6ec7f8926c" {
		t.Errorf("账号 key 被误改：%+v", u.Accounts)
	}
}
