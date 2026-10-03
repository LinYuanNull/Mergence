// metrics_api.go 概览指标接口。
//
// 指标有两个来源，必须分开处理而不是混成一个数字：
//
//	API 型平台（内嵌型渠道）
//	    数据源是 ModelMux 自己的计量：每次转发都记了 token 与缓存，
//	    费用 = token × 价格表。这是**估算**——ModelMux 只是网关，
//	    不知道上游实际扣了多少（有折扣、赠送额度、中转站倍率都会让两者不等）。
//	    所以费用字段一律带 known 标记，绝不用 0 冒充「没花钱」。
//
//	积分型平台（托管型渠道，如 WorkBuddy）
//	    数据源是它自己的 usage 接口：那里有真实的**积分消耗**与缓存命中率，
//	    是账号在这个平台上的真实账面数据，不需要我们估算。
//	    这类渠道的「费用」一栏显示积分，而不是美元。
//
// 两类分开展示是刻意的：把「估算的美元」和「真实的积分」加在一起
// 会得到一个毫无意义的数字。
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"modelmux/internal/metrics"
	"modelmux/internal/provider"
)

// 拉取托管型上游用量时的失败原因。
//
// 刻意把「为什么读不到」写清楚：面板要把原因原样显示给用户，
// 一句「加载失败」会让人以为是 ModelMux 本身坏了。
var (
	errNotManaged    = errors.New("内嵌型渠道没有独立管理 API")
	errNoRoot        = errors.New("渠道缺少子进程地址")
	errBadUsageShape = errors.New("上游 usage 返回的结构无法识别（既无 totals 也无 buckets）")
)

func errUpstreamStatus(code int) error {
	return fmt.Errorf("上游返回 HTTP %d", code)
}

// maxMetricsRange 允许查询的最大天数。
//
// 上游的 usage 接口会按天做全表扫描，无上限地传 days=99999
// 会让它把整个历史翻一遍，面板只要一个「全部」而已。
const maxMetricsRange = 3650

// upstreamUsageTimeout 拉取托管型上游用量的超时。
//
// 概览是「打开就想看到」，不能等一个刚启动的子进程慢慢算账；
// 超时就如实标成「读取失败」，用户可以刷新重试。
const upstreamUsageTimeout = 8 * time.Second

// MetricsResponse 概览接口的完整响应。
type MetricsResponse struct {
	// Local 内嵌型（API 型平台）的本地计量 + 费用估算。
	Local metrics.Report `json:"local"`
	// Upstreams 托管型渠道的真实用量（积分口径）。
	Upstreams []UpstreamUsage `json:"upstreams"`
	// AccountSpend 积分型平台的花费汇总（人民币）。
	AccountSpend AccountSpend `json:"account_spend"`
	// Channels 当前渠道清单（含未就绪的），供面板标注状态。
	Channels []ChannelBrief `json:"channels"`
	// PriceDate 内置价格表采集时间。
	PriceDate string `json:"price_date"`
}

// UpstreamUsage 一个托管型渠道的用量。
type UpstreamUsage struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	// Ready 子进程是否就绪（false 时 Error 一定有值）。
	Ready bool `json:"ready"`
	// Kind 账号类型标识，供面板决定用积分还是别的单位。
	Kind string `json:"kind"`
	// DisplayName 渠道显示名。
	DisplayName string `json:"display_name"`
	// Unit 用量单位：credits（积分）/ tokens。
	Unit string `json:"unit"`

	// ── 真实数据（来自上游自己的 usage 接口）
	Requests     int64   `json:"requests"`
	Errors       int64   `json:"errors"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalTokens  int64   `json:"total_tokens"`
	CachedTokens int64   `json:"cached_tokens"`
	Credits      float64 `json:"credits"`
	// CreditsPer1M 每百万 token 的积分花费；上游没给则为 0。
	CreditsPer1M float64 `json:"credits_per_1m"`
	CacheHitRate float64 `json:"cache_hit_rate"`
	RateKnown    bool    `json:"cache_hit_rate_known"`
	AvgLatencyMS int64   `json:"avg_latency_ms"`
	TokensPerSec float64 `json:"avg_tokens_per_second"`

	// Accounts 账号维度明细（上游给了才有）。
	Accounts []UpstreamAccountUsage `json:"accounts,omitempty"`
	// Models 模型维度明细。
	Models []UpstreamBucket `json:"models,omitempty"`

	// Available 上游是否真的返回了用量数据。
	// false 且 Error 为空表示「上游正常，但没有用量」——两者要分开。
	Available bool `json:"available"`
	// Error 读取失败原因。面板据此显示失败状态而不是空表格。
	Error string `json:"error,omitempty"`

	// ── 实际花费估算（积分 × 每积分价值）
	//
	// 上游只知道积分，不知道钱；「1 积分值多少钱」优先取渠道设置里的
	// credit_value；没填时对 WorkBuddy 渠道回落到官方加量包价
	// （provider.WorkBuddyCreditValue = 0.05 元），并标 SpendDefault=true
	// 让面板注明价格来源。
	CreditValue  float64 `json:"credit_value"`
	Spend        float64 `json:"spend"`
	SpendKnown   bool    `json:"spend_known"`
	SpendDefault bool    `json:"spend_default"`
}

// UpstreamAccountUsage 托管渠道的账号维度用量。
type UpstreamAccountUsage struct {
	Key          string  `json:"key"`
	Label        string  `json:"label,omitempty"`
	Requests     int64   `json:"requests"`
	TotalTokens  int64   `json:"total_tokens"`
	Credits      float64 `json:"credits"`
	CacheHitRate float64 `json:"cache_hit_rate"`
	RateKnown    bool    `json:"cache_hit_rate_known"`
}

// UpstreamBucket 模型/域维度用量。
type UpstreamBucket struct {
	// Key 规范化后的展示名（面板显示这个）。
	Key string `json:"key"`
	// RawKey 上游返回的原始 ID。与 Key 不同时才出现，
	// 供「展示名对不上」时排查上游命名。
	RawKey       string  `json:"raw_key,omitempty"`
	Requests     int64   `json:"requests"`
	TotalTokens  int64   `json:"total_tokens"`
	Credits      float64 `json:"credits"`
	CacheHitRate float64 `json:"cache_hit_rate"`
	RateKnown    bool    `json:"cache_hit_rate_known"`
}

// ChannelBrief 渠道简要信息。
type ChannelBrief struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Source      string `json:"source"`
	Enabled     bool   `json:"enabled"`
	Ready       bool   `json:"ready"`
	ReadyReason string `json:"ready_reason,omitempty"`
}

// handleMetrics 概览指标。
//
// 参数：days（1/7/30/0=全部）。0 表示全部，但受 maxMetricsRange 约束。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	days := 7
	if v := strings.TrimSpace(r.URL.Query().Get("days")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSONStatus(w, http.StatusBadRequest,
				map[string]any{"error": "days 参数应为 0（全部）或正整数"})
			return
		}
		if n == 0 || n > maxMetricsRange {
			days = maxMetricsRange
		} else {
			days = n
		}
	}

	resp := MetricsResponse{
		Local:     s.store.Build(s.pricer, days, s.channelPricing(), 50),
		PriceDate: metrics.BuiltinPriceDate(),
	}
	for _, c := range s.reg.Channels() {
		st := c.Status()
		resp.Channels = append(resp.Channels, ChannelBrief{
			Name: st.Name, DisplayName: st.DisplayName, Source: string(st.Source),
			Enabled: st.Enabled, Ready: st.Ready, ReadyReason: st.ReadyReason,
		})
	}
	resp.Upstreams = s.collectUpstreamUsage(r.Context(), days)
	resp.AccountSpend = aggregateAccountSpend(resp.Upstreams)

	writeJSON(w, resp)
}

// channelPricing 收集各渠道的自定义单价。
//
// 来源是渠道配置里的 price 字段（用户可按中转站实际倍率覆盖内置表）。
// 没配的渠道不出现这张表里，计价会回落到内置价格表。
func (s *Server) channelPricing() map[string]metrics.ChannelPricing {
	out := map[string]metrics.ChannelPricing{}
	s.cfgMu.Lock()
	cfg := s.cfg
	s.cfgMu.Unlock()
	if cfg == nil {
		return out
	}
	for _, p := range cfg.Embedded {
		if cp, ok := p.Price.Override(); ok {
			out[p.Name] = metrics.ChannelPricing{
				HasPrice: true,
				Price: metrics.Price{
					InputPerM: cp.InputPerM, CachedPerM: cp.CachedPerM,
					OutputPerM: cp.OutputPerM, CachedRatio: cp.CachedRatio,
				},
			}
		}
	}
	return out
}

// collectUpstreamUsage 并发拉取各托管型渠道的用量。
//
// 并发而不是串行：概览要同时展示多个渠道，串行会让总耗时叠加。
// 每个渠道的错误都单独记录，一个渠道挂掉不影响其它渠道出数。
func (s *Server) collectUpstreamUsage(ctx context.Context, days int) []UpstreamUsage {
	chans := s.reg.Channels()
	results := make([]UpstreamUsage, len(chans))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for i, c := range chans {
		if !c.Source().Hosted() {
			continue
		}
		up := c.Upstream()
		results[i] = UpstreamUsage{
			Name: up.Name, DisplayName: up.DisplayName,
			// Family()：对外只暴露 内嵌/托管 两类（原生型对面板就是「托管的平台」），
			// 理由见 provider.Source.Family。
			Source: string(c.Source().Family()), Unit: "credits",
			Kind: kindOf(up),
		}
		if !up.Enabled {
			results[i].Error = "渠道已停用"
			continue
		}
		if !c.Ready() {
			results[i].Error = "子进程未就绪：" + c.Status().ReadyReason
			continue
		}
		wg.Add(1)
		go func(idx int, up provider.Upstream) {
			defer wg.Done()
			u, err := fetchUpstreamUsage(ctx, up, days)
			mu.Lock()
			defer mu.Unlock()
			// 失败也要保留渠道身份：面板要显示「哪个渠道读不到」，
			// 只给一句 error 的话用户无从下手。
			base := results[idx]
			base.Ready = true
			base.Available = false
			if err != nil {
				base.Error = err.Error()
			} else {
				u.Name, u.DisplayName = base.Name, base.DisplayName
				u.Source, u.Unit, u.Kind = base.Source, base.Unit, base.Kind
				results[idx] = u
				return
			}
			results[idx] = base
		}(i, up)
	}
	wg.Wait()

	out := make([]UpstreamUsage, 0, len(results))
	for _, r := range results {
		if r.Name == "" {
			continue
		}
		// 花费估算：积分 × 每积分价值。放在结果汇总阶段做，
		// 这样连读取失败的渠道也能带上 credit_value（面板表单回显用）。
		cv, isDefault := s.effectiveCreditValue(r.Name, r.Kind)
		if cv > 0 {
			r.CreditValue = cv
			r.SpendDefault = isDefault
			if r.Available {
				r.Spend = r.Credits * cv
				r.SpendKnown = true
			}
		}
		out = append(out, r)
	}
	return out
}

// AccountSpend 积分型平台的花费汇总（面板「积分型费用」卡的数据源）。
//
// 与本机转发的 API 型费用（token × 美元价 × 汇率）并列展示，
// 两者口径不同：前者是「积分 × 官方价」的真实账面折算，
// 后者是「若按厂商公开 API 价直连」的估算。
type AccountSpend struct {
	// Credits 参与汇总的渠道积分合计。
	Credits float64 `json:"credits"`
	// Spend 折算人民币。
	Spend float64 `json:"spend"`
	// Known 至少有一个渠道出数。false 时面板显示「暂无积分型用量」。
	Known bool `json:"known"`
	// Complete 所有可用渠道都拿到了积分单价。false 时总额是部分估算。
	Complete bool `json:"complete"`
	// Channels 参与汇总的渠道数 / 可用渠道总数。
	Channels      int `json:"channels"`
	ChannelsTotal int `json:"channels_total"`
}

// aggregateAccountSpend 把各托管渠道的花费汇总成一张卡。
func aggregateAccountSpend(us []UpstreamUsage) AccountSpend {
	var a AccountSpend
	avail := 0
	counted := 0
	for _, u := range us {
		if !u.Available {
			continue
		}
		avail++
		if u.SpendKnown {
			counted++
			a.Credits += u.Credits
			a.Spend += u.Spend
		}
	}
	a.Channels, a.ChannelsTotal = counted, avail
	a.Known = counted > 0
	a.Complete = avail > 0 && counted == avail
	return a
}

// effectiveCreditValue 取某渠道的每积分价值（元）。
//
// 优先级：渠道配置里的 credit_value > 平台内置默认价。
// 内置默认只有 WorkBuddy 有（0.05 元，官方加量包口径），返回 isDefault=true
// 让面板注明「按官方加量包价估算」；其它平台没填就是没填，不猜。
func (s *Server) effectiveCreditValue(name, kind string) (cv float64, isDefault bool) {
	if v := s.creditValueOf(name); v > 0 {
		return v, false
	}
	if kind == "workbuddy" {
		return provider.WorkBuddyCreditValue, true
	}
	return 0, false
}

// creditValueOf 取某托管渠道配置的每积分价值（元）。
func (s *Server) creditValueOf(name string) float64 {
	s.cfgMu.Lock()
	cfg := s.cfg
	s.cfgMu.Unlock()
	if cfg == nil {
		return 0
	}
	for _, m := range cfg.Managed {
		if m.Name == name {
			return m.CreditValue
		}
	}
	return 0
}

// fetchUpstreamUsage 拉一个托管渠道的真实用量。
//
// 只接受托管型：内嵌型渠道跑在本进程内，没有自己的管理 API。
// 拉的是它面板的 usage 端点（字段名各家有差异，全部做防御式读取）。
func fetchUpstreamUsage(ctx context.Context, up provider.Upstream, days int) (UpstreamUsage, error) {
	if up.Source != provider.SourceManaged {
		return UpstreamUsage{}, errNotManaged
	}
	root := strings.TrimRight(up.RootURL, "/")
	prefix := strings.TrimRight(up.PanelAPIPrefix, "/")
	if root == "" {
		return UpstreamUsage{}, errNoRoot
	}
	path := prefix + "/usage"
	if days > 0 && days < maxMetricsRange {
		path += "?days=" + strconv.Itoa(days)
	}

	rctx, cancel := context.WithTimeout(ctx, upstreamUsageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, root+path, nil)
	if err != nil {
		return UpstreamUsage{}, err
	}
	req.Header.Set("Accept", "application/json")
	if key := firstKey(up); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := upstreamClient.Do(req)
	if err != nil {
		return UpstreamUsage{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return UpstreamUsage{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return UpstreamUsage{}, errUpstreamStatus(resp.StatusCode)
	}
	return parseUpstreamUsage(raw)
}

// upstreamTotals 上游 usage 的 totals 结构。
//
// 字段全部按指针接收：缺失与 0 必须能区分——
// 「没有缓存数据」和「缓存命中 0 次」在面板上是两回事。
type upstreamTotals struct {
	Requests         *float64 `json:"requests"`
	Errors           *float64 `json:"errors"`
	PromptTokens     *float64 `json:"prompt_tokens"`
	CompletionTokens *float64 `json:"completion_tokens"`
	TotalTokens      *float64 `json:"total_tokens"`
	CacheHitTokens   *float64 `json:"cache_hit_tokens"`
	CacheMissTokens  *float64 `json:"cache_miss_tokens"`
	CacheHitRate     *float64 `json:"cache_hit_rate"`
	Credits          *float64 `json:"credits"`
	CreditsPer1M     *float64 `json:"credits_per_1m_tokens"`
	AvgLatencyMS     *float64 `json:"avg_latency_ms"`
	AvgTokensPerSec  *float64 `json:"avg_tokens_per_second"`
}

type upstreamBucketJSON struct {
	Key             string   `json:"key"`
	Realm           string   `json:"realm"`
	Extra           string   `json:"extra"`
	Requests        *float64 `json:"requests"`
	Errors          *float64 `json:"errors"`
	PromptTokens    *float64 `json:"prompt_tokens"`
	TotalTokens     *float64 `json:"total_tokens"`
	Credits         *float64 `json:"credits"`
	CacheHitTokens  *float64 `json:"cache_hit_tokens"`
	CacheMissTokens *float64 `json:"cache_miss_tokens"`
	CacheHitRate    *float64 `json:"cache_hit_rate"`
	AvgLatencyMS    *float64 `json:"avg_latency_ms"`
}

type upstreamUsageJSON struct {
	Totals    *upstreamTotals      `json:"totals"`
	ByAccount []upstreamBucketJSON `json:"by_account"`
	ByModel   []upstreamBucketJSON `json:"by_model"`
	ByRealm   []upstreamBucketJSON `json:"by_realm"`
}

// parseUpstreamUsage 解析上游 usage 响应。
//
// 兼容两种形状：
//   - 新版：{"totals":{...},"by_account":[...]}（WorkBuddy 2 / wb2api）
//   - 旧版：{"total_requests":N,"buckets":[...]}（更早的 2api）
func parseUpstreamUsage(raw []byte) (UpstreamUsage, error) {
	var v upstreamUsageJSON
	if err := json.Unmarshal(raw, &v); err == nil && v.Totals != nil {
		return fromStructured(v), nil
	}
	return UpstreamUsage{}, errBadUsageShape
}

func fromStructured(v upstreamUsageJSON) UpstreamUsage {
	u := UpstreamUsage{Ready: true, Unit: "credits", Kind: "account"}
	t := v.Totals
	u.Requests = numOf(t.Requests)
	u.Errors = numOf(t.Errors)
	u.InputTokens = numOf(t.PromptTokens)
	u.OutputTokens = numOf(t.CompletionTokens)
	u.TotalTokens = numOf(t.TotalTokens)
	u.CachedTokens = numOf(t.CacheHitTokens)
	u.Credits = fltOf(t.Credits)
	u.CreditsPer1M = fltOf(t.CreditsPer1M)
	u.TokensPerSec = fltOf(t.AvgTokensPerSec)
	u.AvgLatencyMS = numOf(t.AvgLatencyMS)

	// 缓存命中率优先用上游直接给的；没给则用 hit/(hit+miss) 算。
	// 两者都没有时不给数字（RateKnown=false），不填 0。
	if t.CacheHitRate != nil {
		u.CacheHitRate, u.RateKnown = *t.CacheHitRate, true
	} else if t.CacheHitTokens != nil || t.CacheMissTokens != nil {
		hit, miss := numOf(t.CacheHitTokens), numOf(t.CacheMissTokens)
		if den := hit + miss; den > 0 {
			u.CacheHitRate, u.RateKnown = float64(hit)/float64(den), true
		}
	}

	for _, b := range v.ByAccount {
		u.Accounts = append(u.Accounts, bucketToAccount(b))
	}
	for _, b := range v.ByModel {
		u.Models = append(u.Models, bucketToItem(b))
	}
	// 有任一维度数据就算「上游确实返回了用量」。
	u.Available = u.Requests > 0 || u.TotalTokens > 0 || u.Credits > 0 ||
		len(u.Accounts) > 0 || len(u.Models) > 0
	return u
}

func bucketToAccount(b upstreamBucketJSON) UpstreamAccountUsage {
	a := UpstreamAccountUsage{
		Key: b.Key, Label: firstNonEmptyStr(b.Extra, b.Realm),
		Requests: numOf(b.Requests), TotalTokens: numOf(b.TotalTokens),
		Credits: fltOf(b.Credits),
	}
	a.CacheHitRate, a.RateKnown = rateOf(b.CacheHitRate, b.CacheHitTokens, b.CacheMissTokens)
	return a
}

func bucketToItem(b upstreamBucketJSON) UpstreamBucket {
	// 模型名走一遍规范化，与 /v1/models 的对外名保持一致——
	// 面板上「概览的模型名」和「能调用的模型名」必须是同一个，
	// 否则用户按表格里的名字去填客户端会找不到。
	//
	// 只影响展示：这个 Key 从不参与路由（路由走 registry 的 modelCache
	// 与配置里的 models），原始 ID 一并留在 RawKey 里便于排查。
	raw := b.Key
	it := UpstreamBucket{
		Key: provider.NormalizeModelName(raw), Requests: numOf(b.Requests),
		TotalTokens: numOf(b.TotalTokens), Credits: fltOf(b.Credits),
	}
	if raw != "" && raw != it.Key {
		it.RawKey = raw
	}
	it.CacheHitRate, it.RateKnown = rateOf(b.CacheHitRate, b.CacheHitTokens, b.CacheMissTokens)
	return it
}

// rateOf 取缓存命中率：优先直接给的值，否则 hit/(hit+miss) 现算。
func rateOf(given *float64, hit, miss *float64) (float64, bool) {
	if given != nil {
		return *given, true
	}
	if hit == nil && miss == nil {
		return 0, false
	}
	h, m := numOf(hit), numOf(miss)
	if den := h + m; den > 0 {
		return float64(h) / float64(den), true
	}
	return 0, false
}

func numOf(f *float64) int64 {
	if f == nil {
		return 0
	}
	return int64(*f)
}

func fltOf(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

// kindOf 推断托管渠道的账号类型。
//
// 用途：概览要按账号类型选指标口径（WorkBuddy 显示积分消耗，
// 其它托管进程可能给美元或 token）。判定依据是渠道名/预设——
// 没有权威字段可查，写错的后果是「用错单位显示了一个真实数字」，
// 所以拿不准时返回空，由面板显示中性单位。
func kindOf(up provider.Upstream) string {
	hay := strings.ToLower(up.Name + " " + up.DisplayName + " " + up.Preset)
	switch {
	case strings.Contains(hay, "workbuddy"), strings.Contains(hay, "wb2api"),
		strings.Contains(hay, "wb2a"):
		return "workbuddy"
	default:
		return ""
	}
}
