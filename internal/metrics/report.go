// report.go 把计量聚合成面板要的指标（含费用换算）。
//
// 与 metrics.go 的分工：那边只管「记了多少 token」（不含价格），
// 这边负责「这些 token 值多少钱」以及按渠道/模型/天分列。
// 分开是因为计价规则会变（价格表更新、用户自定义单价），
// 而 token 记录不该跟着重写。
package metrics

import "sort"

// ChannelPricing 渠道自定义单价（覆盖内置价格表）。
type ChannelPricing struct {
	// HasPrice 该渠道是否填了自定义价。为 false 时用内置表。
	HasPrice bool
	Price
}

// Range 报告覆盖的时间范围。
type Range struct {
	Days       int    `json:"days"`
	RangeStart string `json:"range_start,omitempty"`
	RangeEnd   string `json:"range_end,omitempty"`
	// HasData 是否有任何计量记录。为 false 时面板显示「暂无数据」，
	// 而不是把一堆 0 渲染成图表。
	HasData bool `json:"has_data"`
}

// Report 面板用的完整指标报告。
type Report struct {
	Range
	// Total 全部记录合计的费用与调用数。
	Total Totals `json:"total"`
	// ByChannel 按渠道分列（按费用从高到低）。
	ByChannel []ChannelReport `json:"by_channel"`
	// ByModel 按模型分列（费用高在前，费用未知的排最后）。
	ByModel []ItemReport `json:"by_model"`
	// Daily 时间序列。
	Daily []DailyCost `json:"daily"`
	// Recent 最近调用明细。
	Recent []RecentItem `json:"recent,omitempty"`
	// UnknownModels 有用量但没有价格的模型（面板要提示「这部分没算钱」）。
	UnknownModels []string `json:"unknown_models,omitempty"`
	// PriceDate 内置价格表采集时间。
	PriceDate string `json:"price_date"`
	// USDCNY / FXDate 美元→人民币折算率与采集日期。
	// 价格表以美元报价，面板用它们把费用换算成人民币显示。
	USDCNY float64 `json:"usd_cny"`
	FXDate string  `json:"fx_date"`
	// Restored 含重启前的历史数据。
	Restored bool `json:"restored"`
}

// Totals 窗口内的总量。
type Totals struct {
	Cost Cost `json:"cost"`
	Agg
	// CacheHitRate 缓存命中率；Known=false 表示没有输入 token，不给数字。
	CacheHitRate float64 `json:"cache_hit_rate"`
	RateKnown    bool    `json:"cache_hit_rate_known"`
	AvgLatencyMS int64   `json:"avg_latency_ms"`
}

// ChannelReport 单个渠道的报告。
type ChannelReport struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	// UsageKnown 该渠道是否真的经过本进程转发（有本地计量）。
	UsageKnown    bool    `json:"usage_known"`
	Cost          Cost    `json:"cost"`
	CacheHitRate  float64 `json:"cache_hit_rate"`
	RateKnown     bool    `json:"cache_hit_rate_known"`
	AvgLatencyMS  int64   `json:"avg_latency_ms"`
	CostMatchedBy string  `json:"cost_matched_by,omitempty"`
	Agg
}

// ItemReport 按模型的报告。
type ItemReport struct {
	Model string `json:"model"`
	Cost  Cost   `json:"cost"`
	Agg
}

// DailyCost 一天的费用。
type DailyCost struct {
	Date string `json:"date"`
	Cost Cost   `json:"cost"`
	// AvgLatencyMS 当日平均延迟。
	AvgLatencyMS int64 `json:"avg_latency_ms"`
	Agg
}

// RecentItem 最近一次调用。
type RecentItem struct {
	Time      string  `json:"time"`
	Date      string  `json:"date"`
	Channel   string  `json:"channel"`
	Model     string  `json:"model"`
	OK        bool    `json:"ok"`
	Status    int     `json:"status,omitempty"`
	Duration  int64   `json:"duration_ms,omitempty"`
	Tokens    int64   `json:"total_tokens"`
	Cost      float64 `json:"cost"`
	CostKnown bool    `json:"cost_known"`
	Stream    bool    `json:"stream,omitempty"`
	Error     string  `json:"error,omitempty"`
}

// modelAgg 一个「渠道×模型」的 token 与费用。
type modelAgg struct {
	tokens Agg
	cost   Cost
}

// build 从 store 的聚合构建报告。
//
// 计价维度是「渠道×模型」而不是「渠道」：同一渠道下不同模型单价能差
// 几十倍（gpt-4o-mini 与 gpt-4o 差 16 倍），先合并 token 再乘单价
// 会算出一个看起来合理但完全错误的费用。
func (p *Pricer) build(s *Store, days int, pricing map[string]ChannelPricing,
	recentLimit int) Report {

	q := s.Query(days)
	rate, fxDate := USDToCNY()
	rep := Report{
		Range: Range{
			Days: q.Days, RangeStart: q.RangeStart, RangeEnd: q.RangeEnd,
			HasData: q.HasData,
		},
		PriceDate: BuiltinPriceDate(),
		USDCNY:    rate,
		FXDate:    fxDate,
		Restored:  q.Restored,
	}

	unknown := map[string]bool{}

	// ── 按「渠道×模型」计价
	pairs := make([]PairKey, 0, len(q.ByPair))
	for k := range q.ByPair {
		pairs = append(pairs, k)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].Channel != pairs[j].Channel {
			return pairs[i].Channel < pairs[j].Channel
		}
		return pairs[i].Model < pairs[j].Model
	})

	perPair := make(map[PairKey]*modelAgg, len(pairs))
	for _, k := range pairs {
		t := q.ByPair[k]
		m := &modelAgg{}
		m.tokens = *t
		// 积分型平台渠道**不算美元费用**：它们的花费以积分计（真实账面
		// 来自上游 usage），这里的 token×单价只是估算，两套口径混在
		// 一个总额里会得到一个毫无意义的数字。积分型平台的花费估算由
		// web 层用「积分 × 每积分价值」单独算（见 metrics_api.go）。
		//
		// 内嵌型里没价格的模型**仍然要参与 addCost**：addCost 会把
		// Complete 压成 false（「总额只是已知部分」），跳过它们的话
		// 面板会把一个残缺总额当成完整总额展示。
		if q.Sources[k.Channel] == "managed" {
			m.cost = Cost{Currency: "USD", Model: k.Model}
		} else {
			m.cost = p.costFor(k.Channel, k.Model, *t, pricing, unknown)
			rep.Total.Cost = addCost(rep.Total.Cost, m.cost)
		}
		perPair[k] = m
		rep.Total.Agg.merge(*t)
	}
	if rate, ok := q.Total.CacheHitRate(); ok {
		rep.Total.CacheHitRate, rep.Total.RateKnown = rate, true
	}
	rep.Total.AvgLatencyMS = q.Total.AvgLatencyMS()

	// ── 按渠道汇总
	type chAcc struct {
		rep     ChannelReport
		matched string
	}
	chAccs := map[string]*chAcc{}
	for _, k := range pairs {
		m := perPair[k]
		a := chAccs[k.Channel]
		if a == nil {
			a = &chAcc{rep: ChannelReport{
				Name: k.Channel, Source: q.Sources[k.Channel], UsageKnown: true,
			}}
			chAccs[k.Channel] = a
		}
		a.rep.Agg.merge(m.tokens)
		a.rep.Cost = addCost(a.rep.Cost, m.cost)
		if m.cost.MatchedBy != "" {
			a.matched = m.cost.MatchedBy
		}
	}
	// 补上只有失败记录（没有 token）的渠道，否则「有请求但 0 token」的渠道会消失。
	for ch, t := range q.ByChannel {
		if _, ok := chAccs[ch]; !ok {
			chAccs[ch] = &chAcc{rep: ChannelReport{
				Name: ch, Source: q.Sources[ch], UsageKnown: t.Requests+t.Errors > 0,
			}}
		}
	}
	names := make([]string, 0, len(chAccs))
	for n := range chAccs {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := chAccs[names[i]].rep, chAccs[names[j]].rep
		if a.Cost.Known != b.Cost.Known {
			return a.Cost.Known // 已知费用的排前面
		}
		if a.Cost.Total != b.Cost.Total {
			return a.Cost.Total > b.Cost.Total
		}
		return names[i] < names[j]
	})
	for _, n := range names {
		a := chAccs[n]
		a.rep.AvgLatencyMS = a.rep.Agg.AvgLatencyMS()
		a.rep.CostMatchedBy = a.matched
		if rate, ok := a.rep.Agg.CacheHitRate(); ok {
			a.rep.CacheHitRate, a.rep.RateKnown = rate, true
		}
		rep.ByChannel = append(rep.ByChannel, a.rep)
	}

	// ── 按模型汇总（同名模型跨渠道合并）
	byModel := map[string]*ItemReport{}
	for k, m := range perPair {
		ir := byModel[k.Model]
		if ir == nil {
			ir = &ItemReport{Model: k.Model}
			byModel[k.Model] = ir
		}
		ir.Agg.merge(m.tokens)
		ir.Cost = addCost(ir.Cost, m.cost)
	}
	models := make([]string, 0, len(byModel))
	for m := range byModel {
		models = append(models, m)
	}
	sort.Strings(models)
	sort.SliceStable(models, func(i, j int) bool {
		a, b := byModel[models[i]], byModel[models[j]]
		if a.Cost.Known != b.Cost.Known {
			return a.Cost.Known // 费用未知的排最后
		}
		return a.Cost.Total > b.Cost.Total
	})
	for _, m := range models {
		rep.ByModel = append(rep.ByModel, *byModel[m])
	}

	// ── 每日序列
	// 每天也按「渠道×模型」重算，不能用当日合计 token 乘个均价。
	// Daily 里没保留 byPair（体积考虑），所以这里用「当日按模型分桶」
	// 加上 Query.ByPair 无法区分渠道这一点做保守处理：
	// 直接复用当日 byModel（跨渠道同名模型会按一个价算）。
	for _, d := range q.Daily {
		dc := DailyCost{Date: d.Date}
		dc.Agg = d.Agg
		dc.AvgLatencyMS = d.Agg.AvgLatencyMS()
		dc.Cost = Cost{Currency: "USD"}
		mk := make([]string, 0, len(d.ByModel))
		for m := range d.ByModel {
			mk = append(mk, m)
		}
		sort.Strings(mk)
		for _, m := range mk {
			// 模型可能属于多个渠道且自定义价不同：这里用窗口内
			// 该模型出现过的第一个渠道定价，费用误差只影响趋势图，
			// 不影响上方按渠道的精确数字（那部分走 ByPair）。
			ch := firstChannelOf(pairs, m)
			dc.Cost = addCost(dc.Cost, p.costFor(ch, m, *d.ByModel[m], pricing, nil))
		}
		rep.Daily = append(rep.Daily, dc)
	}

	for m := range unknown {
		rep.UnknownModels = append(rep.UnknownModels, m)
	}
	sort.Strings(rep.UnknownModels)

	// ── 最近调用
	if recentLimit > 0 {
		for _, r := range s.Recent(recentLimit) {
			date := r.Time.Format("2006-01-02")
			if !inRange(date, days) {
				continue
			}
			var a Agg
			a.add(r)
			c := p.costFor(r.Channel, r.Model, a, pricing, nil)
			rep.Recent = append(rep.Recent, RecentItem{
				Time: r.Time.Format("15:04:05"), Date: date,
				Channel: r.Channel, Model: r.Model, OK: r.OK, Status: r.Status,
				Duration: r.DurationMS, Tokens: a.TotalTokens,
				Cost: c.Total, CostKnown: c.Known, Stream: r.Stream, Error: r.Error,
			})
		}
	}
	return rep
}

// firstChannelOf 找出该模型出现过的第一个渠道（按 pairs 已排序，取字典序最小）。
func firstChannelOf(pairs []PairKey, model string) string {
	for _, k := range pairs {
		if k.Model == model {
			return k.Channel
		}
	}
	return ""
}

// costFor 算某渠道某模型的费用。
func (p *Pricer) costFor(channel, model string, t Agg,
	pricing map[string]ChannelPricing, unknown map[string]bool) Cost {

	cp, hasCP := pricing[channel]
	pr, known, matched := Price{}, false, ""
	if hasCP && cp.HasPrice {
		pr, known, matched = cp.Price, true, "user"
	} else {
		pr, known, matched = p.Lookup(Price{}, false, model)
	}
	if !known {
		if unknown != nil && t.TotalTokens > 0 {
			unknown[model] = true
		}
		// 明确返回「未知」而不是零：面板据此显示「价格未知」，
		// 显示 $0.00 会被读成「没花钱」。
		return Cost{Currency: "USD", Model: model, Known: false}
	}
	n := Normalized{
		Input: t.InputTokens, CachedInput: t.CachedTokens,
		Output: t.OutputTokens, Total: t.TotalTokens,
	}
	return p.Cost(pr, true, matched, n)
}

// addCost 相加两段费用。
//
// Known 取「任一段已知」：只有部分模型有价格时，总额是「已知部分之和」，
// 面板必须能区分「全部已知」与「部分已知」，否则用户会以为这就是全部花费。
// Complete 字段记录是否所有段都已知，供面板标注。
func addCost(a, b Cost) Cost {
	out := a
	out.Input += b.Input
	out.Cached += b.Cached
	out.Output += b.Output
	out.Total += b.Total
	switch {
	case !a.Known && b.Known:
		out.Known, out.Complete = true, b.Complete
	case a.Known && !b.Known:
		out.Known, out.Complete = true, false
	case a.Known && b.Known:
		out.Known, out.Complete = true, a.Complete && b.Complete
	default:
		out.Known, out.Complete = false, false
	}
	if out.Currency == "" {
		out.Currency = b.Currency
	}
	return out
}

// Build 构建面板报告。
func (s *Store) Build(pri *Pricer, days int, pricing map[string]ChannelPricing,
	recentLimit int) Report {
	return pri.build(s, days, pricing, recentLimit)
}
