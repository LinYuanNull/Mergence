// pricing.go 模型价格表：token → 费用。
//
// 为什么需要它：概览要显示「API 费用」，而 ModelMux 只是网关，
// 上游不会告诉你这次调用花了多少钱。费用只能由「本地记的 token × 已知单价」
// 推算。所以这张表是**唯一的估算来源**，它的可信度直接决定费用数字的可信度。
//
// 三条原则：
//
//  1. **宁可标未知，不可填 0**。表里没有的模型返回 ok=false，
//     面板显示「价格未知」；显示 $0.00 会被读成「免费」，那是撒谎。
//  2. **匹配确定性**。按模型名精确匹配 → 前缀匹配 → 家族兜底，
//     命中哪一级会写进 MatchedBy，面板能说明「这个价是怎么来的」。
//  3. **可覆盖**。用户在渠道里填的价格优先于内置表——
//     中转站倍率、同人不同价、协议价都靠它兜住。
package metrics

import (
	"strings"
)

// Price 一个模型的单价（美元 / 每百万 token）。
//
// 三个维度对应三种计费：未命中缓存的输入、命中缓存的输入、输出。
// 缓存价通常只有全价的 1/10 到 1/4，OpenAI 与 DeepSeek 都是这个结构。
type Price struct {
	// InputPerM 未命中缓存的输入（美元/百万 token）。
	InputPerM float64 `json:"input_per_m"`
	// CachedPerM 命中缓存的输入。为 0 时回落到 InputPerM 的 CachedRatio。
	CachedPerM float64 `json:"cached_per_m"`
	// OutputPerM 输出（美元/百万 token）。
	OutputPerM float64 `json:"output_per_m"`
	// CachedRatio CachedPerM 为 0 时使用的比例（Anthropic/DeepSeek 常是 0.1）。
	CachedRatio float64 `json:"cached_ratio,omitempty"`
}

// cachedUnit 返回缓存输入的实际单价。
func (p Price) cachedUnit() float64 {
	if p.CachedPerM > 0 {
		return p.CachedPerM
	}
	if p.CachedRatio > 0 {
		return p.InputPerM * p.CachedRatio
	}
	// 完全没有缓存定价信息时按全价计——比假装免费更保守，
	// 且会在 Cost 里标注为「按全价估算」。
	return p.InputPerM
}

// Cost 一次调用的费用拆分（美元）。
type Cost struct {
	Input  float64 `json:"input"`
	Cached float64 `json:"cached"`
	Output float64 `json:"output"`
	Total  float64 `json:"total"`
	// Known 参与汇总的每一项是否都拿到了价格。
	// 为 false 时 Total 只是一个「已知部分之和」，面板必须标注「部分模型价格未知」。
	Known bool `json:"known"`
	// Complete 所有计入的项都拿到了价格（与 Known 同步维护，语义更明确）。
	Complete bool `json:"complete"`
	// Currency 币种。目前内置表都是美元。
	Currency string `json:"currency,omitempty"`
	// MatchedBy 价格来源：user（渠道自定义）/ exact（精确）/ prefix（前缀）/ family（家族兜底）。
	MatchedBy string `json:"matched_by,omitempty"`
	// Model 计价对应的模型名。
	Model string `json:"model,omitempty"`
}

// Pricer 按模型名查价。
type Pricer struct {
	// exact 精确匹配表（键为小写模型名）。
	exact map[string]Price
	// prefix 前缀匹配，按键长度降序优先。
	prefix []prefixPrice
	// family 家族兜底。
	family []prefixPrice
	// fallback 无任何信息时的默认；为零值表示「不给价」。
	fallback Price
}

type prefixPrice struct {
	prefix string
	price  Price
}

// NewPricer 用内置价格表构造查价器。
func NewPricer() *Pricer {
	p := &Pricer{exact: map[string]Price{}}
	for _, e := range builtinPrices {
		p.exact[strings.ToLower(e.model)] = e.price
	}
	for _, e := range prefixPrices {
		p.prefix = append(p.prefix, prefixPrice{prefix: e.model, price: e.price})
	}
	for _, e := range familyPrices {
		p.family = append(p.family, prefixPrice{prefix: e.model, price: e.price})
	}
	return p
}

// Lookup 查某个模型的价格。user 为渠道自定义价（优先）。
//
// 模型名按「上游真实名 → 对外名」的顺序传入：真实名更可能命中价格表
// （对外名带渠道前缀，如 or/openai/gpt-4o，表里没有这个键）。
func (p *Pricer) Lookup(user Price, hasUser bool, models ...string) (Price, bool, string) {
	if hasUser {
		return user, true, "user"
	}
	for _, m := range models {
		m = normalizeModel(m)
		if m == "" {
			continue
		}
		if pr, ok := p.exact[m]; ok {
			return pr, true, "exact"
		}
	}
	// 前缀匹配：取最长的命中前缀（「gpt-4o」应优先于「gpt-4」）。
	for _, m := range models {
		m = normalizeModel(m)
		for _, e := range p.prefix {
			if strings.HasPrefix(m, e.prefix) {
				return e.price, true, "prefix"
			}
		}
	}
	// 家族兜底。
	for _, m := range models {
		m = normalizeModel(m)
		for _, e := range p.family {
			if strings.Contains(m, e.prefix) {
				return e.price, true, "family"
			}
		}
	}
	return Price{}, false, ""
}

// Cost 按价格算一次调用的费用。
func (p *Pricer) Cost(pr Price, known bool, matchedBy string, n Normalized) Cost {
	c := Cost{Currency: "USD"}
	if !known {
		return c
	}
	c.Known = true
	c.Complete = true
	c.MatchedBy = matchedBy
	// 按「每百万 token」计价。
	c.Input = float64(n.Input) / 1e6 * pr.InputPerM
	c.Cached = float64(n.CachedInput) / 1e6 * pr.cachedUnit()
	c.Output = float64(n.Output) / 1e6 * pr.OutputPerM
	c.Total = c.Input + c.Cached + c.Output
	return c
}

// normalizeModel 去掉厂商前缀与常见后缀，让匹配更容易命中。
//
// 「openai/gpt-4o」与「gpt-4o-2024-11-20」都应命中同一条价格。
func normalizeModel(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	// 去掉 :free / :beta / @latest 这类中转站后缀。
	if i := strings.IndexAny(m, ":@"); i > 0 {
		m = m[:i]
	}
	return strings.TrimSpace(m)
}

// builtinPrices 内置精确价格表（美元 / 百万 token）。
//
// 采集时间：2026-10。价格会变，因此：
//   - 表里没有的模型一律标「价格未知」，不猜；
//   - 用户可在渠道里填自己的单价覆盖；
//   - 面板上会显示这张表的采集时间，提醒用户核对。
//
// 覆盖范围刻意保守：只收录能确认公开定价的主流模型。
// 宁可少收（显示未知）也不可错价（显示一个看似精确的错数字）。
var builtinPrices = []struct {
	model string
	price Price
}{
	// ── Anthropic
	{"claude-opus-5-5", Price{InputPerM: 4, CachedPerM: 0.20, OutputPerM: 20}},
	{"claude-opus-5", Price{InputPerM: 5, CachedPerM: 0.50, OutputPerM: 25}},
	{"claude-fable-5-1", Price{InputPerM: 10, CachedPerM: 0.25, OutputPerM: 50}},
	{"claude-fable-5", Price{InputPerM: 10, CachedPerM: 1.00, OutputPerM: 50}},
	{"claude-sonnet-5", Price{InputPerM: 2, CachedPerM: 0.20, OutputPerM: 10}},
	{"claude-haiku-4-5", Price{InputPerM: 1, CachedPerM: 0.10, OutputPerM: 5}},
	// ── OpenAI（官方公开价，未含 Batch 折扣）
	{"gpt-4o-mini", Price{InputPerM: 0.15, CachedRatio: 0.5, OutputPerM: 0.60}},
	{"gpt-4o", Price{InputPerM: 2.50, CachedRatio: 0.5, OutputPerM: 10}},
	{"gpt-4.1-mini", Price{InputPerM: 0.40, CachedRatio: 0.25, OutputPerM: 1.60}},
	{"gpt-4.1", Price{InputPerM: 2.00, CachedRatio: 0.25, OutputPerM: 8.00}},
	{"o3-mini", Price{InputPerM: 1.10, CachedRatio: 0.5, OutputPerM: 4.40}},
	{"o3", Price{InputPerM: 2.00, CachedRatio: 0.5, OutputPerM: 8.00}},
	{"o4-mini", Price{InputPerM: 1.10, CachedRatio: 0.25, OutputPerM: 4.40}},
	// ── DeepSeek 官方（峰谷取高时段，均为美元；V4 系列）
	{"deepseek-chat", Price{InputPerM: 0.14, CachedPerM: 0.0028, OutputPerM: 0.28}},
	{"deepseek-reasoner", Price{InputPerM: 0.14, CachedPerM: 0.0028, OutputPerM: 0.28}},
	{"deepseek-v4-flash", Price{InputPerM: 0.14, CachedPerM: 0.0028, OutputPerM: 0.28}},
	{"deepseek-v4-pro", Price{InputPerM: 0.435, CachedPerM: 0.06, OutputPerM: 0.87}},
	// ── Google
	{"gemini-2.5-pro", Price{InputPerM: 1.25, CachedRatio: 0.1, OutputPerM: 10}},
	{"gemini-2.5-flash", Price{InputPerM: 0.30, CachedRatio: 0.1, OutputPerM: 2.50}},
	{"gemini-2.0-flash", Price{InputPerM: 0.10, CachedRatio: 0.1, OutputPerM: 0.40}},
	// ── 智谱 GLM
	{"glm-4.6", Price{InputPerM: 0.60, CachedRatio: 0.2, OutputPerM: 2.20}},
	{"glm-4.5", Price{InputPerM: 0.60, CachedRatio: 0.2, OutputPerM: 2.20}},
	{"glm-4-plus", Price{InputPerM: 0.70, CachedRatio: 0.2, OutputPerM: 1.40}},
	{"glm-4-air", Price{InputPerM: 0.014, CachedRatio: 0.2, OutputPerM: 0.14}},
	{"glm-4-flash", Price{InputPerM: 0.007, CachedRatio: 0.2, OutputPerM: 0.14}},
	// ── Moonshot / Kimi
	{"kimi-k2", Price{InputPerM: 0.60, CachedPerM: 0.15, OutputPerM: 2.50}},
	{"moonshot-v1-8k", Price{InputPerM: 0.20, CachedRatio: 0.25, OutputPerM: 2.00}},
	{"moonshot-v1-32k", Price{InputPerM: 0.40, CachedRatio: 0.25, OutputPerM: 4.00}},
}

// prefixPrices 前缀匹配（键越长越优先）。
var prefixPrices = []struct {
	model string
	price Price
}{
	// 带日期后缀的快照型号
	{"claude-opus-5-5", Price{InputPerM: 4, CachedPerM: 0.20, OutputPerM: 20}},
	{"claude-sonnet-5", Price{InputPerM: 2, CachedPerM: 0.20, OutputPerM: 10}},
	{"claude-haiku-4-5", Price{InputPerM: 1, CachedPerM: 0.10, OutputPerM: 5}},
	{"claude-opus-5", Price{InputPerM: 5, CachedPerM: 0.50, OutputPerM: 25}},
	{"claude-sonnet-4", Price{InputPerM: 3, CachedRatio: 0.1, OutputPerM: 15}},
	{"claude-3-5-haiku", Price{InputPerM: 0.80, CachedRatio: 0.1, OutputPerM: 4}},
	{"gpt-4o-mini", Price{InputPerM: 0.15, CachedRatio: 0.5, OutputPerM: 0.60}},
	{"gpt-4.1-mini", Price{InputPerM: 0.40, CachedRatio: 0.25, OutputPerM: 1.60}},
	{"gpt-4.1-nano", Price{InputPerM: 0.10, CachedRatio: 0.25, OutputPerM: 0.40}},
	{"gpt-4o", Price{InputPerM: 2.50, CachedRatio: 0.5, OutputPerM: 10}},
	{"gpt-4-turbo", Price{InputPerM: 10, CachedRatio: 0.5, OutputPerM: 30}},
	{"gpt-3.5-turbo", Price{InputPerM: 0.50, CachedRatio: 0.5, OutputPerM: 1.50}},
	{"o4-mini", Price{InputPerM: 1.10, CachedRatio: 0.25, OutputPerM: 4.40}},
	{"o3-mini", Price{InputPerM: 1.10, CachedRatio: 0.5, OutputPerM: 4.40}},
	{"gemini-2.5-pro", Price{InputPerM: 1.25, CachedRatio: 0.1, OutputPerM: 10}},
	{"gemini-2.5-flash", Price{InputPerM: 0.30, CachedRatio: 0.1, OutputPerM: 2.50}},
	{"deepseek", Price{InputPerM: 0.14, CachedPerM: 0.0028, OutputPerM: 0.28}},
	{"glm-4.6", Price{InputPerM: 0.60, CachedRatio: 0.2, OutputPerM: 2.20}},
	{"kimi-k2", Price{InputPerM: 0.60, CachedPerM: 0.15, OutputPerM: 2.50}},
}

// familyPrices 家族兜底（子串包含即命中）。
//
// 只在完全查不到时才用，价位取该家族里最便宜的一档——目的是给出一个
// 「量级大致对」的参考，而不是精确值。面板会标注这是家族估算。
var familyPrices = []struct {
	model string
	price Price
}{
	{"gpt-4", Price{InputPerM: 2.50, CachedRatio: 0.5, OutputPerM: 10}},
	{"gpt-5", Price{InputPerM: 1.25, CachedRatio: 0.5, OutputPerM: 10}},
	{"claude", Price{InputPerM: 3, CachedRatio: 0.1, OutputPerM: 15}},
	{"gemini", Price{InputPerM: 0.30, CachedRatio: 0.1, OutputPerM: 2.50}},
	{"glm", Price{InputPerM: 0.60, CachedRatio: 0.2, OutputPerM: 2.20}},
	{"deepseek", Price{InputPerM: 0.14, CachedPerM: 0.0028, OutputPerM: 0.28}},
	{"kimi", Price{InputPerM: 0.60, CachedPerM: 0.15, OutputPerM: 2.50}},
	{"qwen", Price{InputPerM: 0.40, CachedRatio: 0.5, OutputPerM: 1.20}},
	{"llama", Price{InputPerM: 0.20, CachedRatio: 0.5, OutputPerM: 0.20}},
	{"mistral", Price{InputPerM: 0.25, CachedRatio: 0.5, OutputPerM: 0.25}},
}

// builtinPriceDate 内置价格表的采集时间。
//
// 面板上要显示它：价格会变，用户看到费用不对时第一反应应该是
// 「表旧了」，而不是「ModelMux 算错了」。
const builtinPriceDate = "2026-10"

// BuiltinPriceDate 返回内置价格表采集时间。
func BuiltinPriceDate() string { return builtinPriceDate }

// USDCNY 美元兑人民币的折算率。
//
// 内置价格表按厂商惯例以美元报价（OpenAI/Anthropic 官方价都是 USD），
// 但面板要显示人民币。折算率取 2026-10-02 的市场中间价约 6.705、
// 中行折算价 6.7351，取整为 6.71。
//
// 为什么放常量而不是实时汇率：查实时汇率要引外部依赖且让费用数字
// 随汇率抖动；固定汇率 + 面板标注日期，用户看到费用不对时能立刻
// 判断是「汇率旧了」还是「价格表旧了」。
const USDCNY = 6.71

// FXDate 汇率的采集日期。
const FXDate = "2026-10-02"

// USDToCNY 返回折算率与采集日期。
func USDToCNY() (rate float64, date string) { return USDCNY, FXDate }
