// registry.go 渠道注册表与模型路由。
//
// 路由规则（按优先级）：
//  1. 前缀匹配：模型名以某渠道的 ModelPrefix 开头 → 命中该渠道。
//     前缀在所有渠道间保证唯一（config.normalize 会禁用重复者），所以不会歧义。
//  2. 裸名匹配：模型名不带前缀，但与某渠道「声明列表里的对外名或上游 ID」完全相同。
//     多个渠道都声明了同一个模型时，按 Priority 降序、Weight 降序、配置顺序择一。
//
// 「前缀匹配但未在声明列表里」会**原样透传**给上游——上游随时可能上新模型，
// 不该逼用户先改配置才能用；名字错了上游会返回一个明确的 404。
//
// 未就绪的渠道（托管型子进程还没起来）不参与路由，但会返回一个**能说清原因**的错误，
// 而不是让人误以为模型名写错了。
package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"mergence/internal/logging"
)

// 路由相关错误。
var (
	ErrModelRequired = errors.New("请求体缺少 model 字段")
	ErrNoChannel     = errors.New("尚未配置任何可用渠道，请先在「渠道」页添加")
	ErrModelNotFound = errors.New("没有渠道能提供该模型，请检查模型名前缀是否与渠道匹配")
	ErrAutoRouting   = errors.New("auto 路由需要额度与健康度数据，将在额度模块完成后提供；" +
		"当前请使用「渠道前缀/模型名」的形式，例如 or/openai/gpt-4o")
)

// NotReadyError 命中了渠道、但该渠道当前不可用（多为托管型子进程未就绪）。
type NotReadyError struct {
	Channel string
	Reason  string
}

func (e *NotReadyError) Error() string {
	msg := fmt.Sprintf("渠道 %q 当前不可用", e.Channel)
	if e.Reason != "" {
		msg += "：" + e.Reason
	}
	return msg
}

// Registry 全部可路由渠道。
type Registry struct {
	lg *logging.Logger

	mu     sync.RWMutex
	chans  []*Channel
	byName map[string]*Channel
	// sig 上一次同步时的上游指纹，用于避免无谓重建。
	sig string
}

// NewRegistry 创建空注册表。
func NewRegistry(lg *logging.Logger) *Registry {
	return &Registry{lg: lg, byName: map[string]*Channel{}}
}

// Reload 用新配置整体替换渠道集合（无条件重建）。
//
// 同名渠道会沿用其 Key 池与模型缓存（见 newChannel），避免每次保存配置都把
// 冷却状态清空。构建失败的渠道被跳过并记错误，不影响其余渠道。
func (r *Registry) Reload(ups []Upstream) {
	r.mu.Lock()
	old := r.byName
	next := make([]*Channel, 0, len(ups))
	byName := make(map[string]*Channel, len(ups))

	for _, up := range ups {
		ch, err := newChannel(up, r.lg, old[up.Name])
		if err != nil {
			r.lg.Error("渠道构建失败，已跳过", "provider", up.Name, "err", err.Error())
			continue
		}
		next = append(next, ch)
		byName[up.Name] = ch
	}
	r.chans, r.byName = next, byName
	r.sig = Signatures(ups)
	r.mu.Unlock()

	r.lg.Info("渠道已加载", "total", len(next), "enabled", len(r.Enabled()))
}

// SyncIfChanged 仅在上游集合「影响路由的部分」发生变化时才重建。
//
// 存在的理由：面板每 3 秒拉一次状态，而托管型渠道的就绪状态会随子进程变化。
// 若无条件重建，Key 冷却状态每 3 秒被清一次 —— 表现为「刚被限流的 Key 立刻又被撞」。
// 所以这里比对指纹，只有真的变了才动。
func (r *Registry) SyncIfChanged(ups []Upstream) bool {
	sig := Signatures(ups)
	r.mu.RLock()
	same := sig == r.sig
	r.mu.RUnlock()
	if same {
		return false
	}
	r.Reload(ups)
	return true
}

// Channels 返回全部渠道（含停用），按配置顺序。
func (r *Registry) Channels() []*Channel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Channel, len(r.chans))
	copy(out, r.chans)
	return out
}

// Enabled 返回启用中的渠道（不一定就绪）。
func (r *Registry) Enabled() []*Channel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Channel, 0, len(r.chans))
	for _, c := range r.chans {
		if c.up.Enabled {
			out = append(out, c)
		}
	}
	return out
}

// ByName 按名称取渠道。
func (r *Registry) ByName(name string) (*Channel, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.byName[name]
	return c, ok
}

// stripPrefix 从模型名里剥掉渠道前缀，返回剩余部分。
//
// 前缀不带斜杠（`or-` + `gpt-4o` → `or-gpt-4o`），但要容忍旧写法：
// 早期版本会自动补 `/`，于是存量配置与用户习惯里都是 `or/gpt-4o`。
// 两种都认，否则升级后用户手里的模型名会突然失配，而报错是
// 404 model_not_found —— 他会以为是自己写错了名字，去翻配置找问题。
//
// 做法：先按原样切；切不动就把前缀尾部的 `-` 去掉，再切一次并剥掉
// 模型名紧随的分隔符（`-` 或 `/` 都认，两者等价）。
func stripPrefix(model, prefix string) (string, bool) {
	if prefix == "" {
		return "", false
	}
	if rest, ok := strings.CutPrefix(model, prefix); ok && rest != "" {
		return rest, true
	}
	if bare := strings.TrimRight(prefix, "-/"); bare != "" && bare != prefix {
		if rest, ok := strings.CutPrefix(model, bare); ok {
			if r := strings.TrimLeft(rest, "-/"); r != "" {
				return r, true
			}
		}
	}
	return "", false
}

// Route 依据对外模型名找到渠道与上游真实模型名。
func (r *Registry) Route(model string) (*Channel, string, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, "", ErrModelRequired
	}
	if model == "auto" {
		return nil, "", ErrAutoRouting
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.chans) == 0 {
		return nil, "", ErrNoChannel
	}

	// ── 1) 前缀匹配：取最长前缀。同时记住「命中了但没就绪」的候选，
	//        以便在没有可用候选时报出真正的原因。
	var hit *Channel
	var up string
	best := -1
	var notReady *NotReadyError

	for _, c := range r.chans {
		p := c.up.ModelPrefix
		if p == "" {
			continue
		}
		// 前缀匹配。前缀不带斜杠（`or-`），但要容忍两种历史写法：
		//   `orgpt-4o`（裸连）        —— 现在的形态
		//   `or/gpt-4o`（带斜杠）     —— 旧配置/旧文档里的写法
		// 直接 HasPrefix 会在存量用法上突然失配，用户会以为模型名写错了。
		rest, ok := stripPrefix(model, p)
		if !ok {
			continue
		}
		real := rest
		if v, ok := c.upstreamFor(rest); ok {
			real = v
		}
		// 前缀匹配上就说明「这个模型归它管」，那么停用/未就绪都要说出真实原因。
		// 直接跳过会退化成 404 model_not_found，让用户以为模型名写错了去翻配置。
		if !c.up.Enabled || !c.Ready() {
			if notReady == nil {
				notReady = &NotReadyError{Channel: c.up.DisplayName, Reason: c.unreadyReason()}
			}
			continue
		}
		if len(p) > best {
			hit, up, best = c, real, len(p)
		}
	}
	if hit != nil {
		return hit, up, nil
	}

	// ── 2) 裸名匹配：只在声明列表里找，按 Priority / Weight / 顺序择一
	type cand struct {
		ch   *Channel
		upID string
	}
	var cands []cand
	var notReadyBare *NotReadyError

	for _, c := range r.chans {
		id, ok := c.upstreamByDeclared(model)
		if !ok {
			continue
		}
		if !c.up.Enabled || !c.Ready() {
			if notReadyBare == nil {
				notReadyBare = &NotReadyError{Channel: c.up.DisplayName, Reason: c.unreadyReason()}
			}
			continue
		}
		cands = append(cands, cand{c, id})
	}
	if len(cands) > 0 {
		sort.SliceStable(cands, func(i, j int) bool {
			if cands[i].ch.up.Priority != cands[j].ch.up.Priority {
				return cands[i].ch.up.Priority > cands[j].ch.up.Priority
			}
			return cands[i].ch.up.Weight > cands[j].ch.up.Weight
		})
		if len(cands) > 1 {
			r.lg.Debug("裸模型名被多个渠道声明，按优先级择一",
				"model", model, "picked", cands[0].ch.up.Name, "candidates", len(cands))
		}
		return cands[0].ch, cands[0].upID, nil
	}

	// 什么都没命中，但确实有渠道是「因为没就绪」才被跳过的 —— 说出真正原因
	if notReady != nil {
		return nil, "", notReady
	}
	if notReadyBare != nil {
		return nil, "", notReadyBare
	}
	return nil, "", ErrModelNotFound
}

// ModelEntry 聚合模型列表里的一项。
type ModelEntry struct {
	// ID 对外全名（前缀 + 别名或上游名）。
	ID string `json:"id"`
	// Upstream 上游真实模型名，用于按厂商归类（P6）。
	Upstream string `json:"upstream"`
	Channel  string `json:"channel"`
	Source   Source `json:"source"`
	Vendor   string `json:"vendor"`
	Context  int    `json:"context,omitempty"`
	MaxOut   int    `json:"max_output,omitempty"`
}

// Models 聚合所有「启用且就绪」渠道的模型。
//
// 未就绪的渠道不列出：模型列表是客户端选模型用的，列出一个调用必然失败的模型
// 只会让人困惑。
func (r *Registry) Models() []ModelEntry {
	var out []ModelEntry
	seen := map[string]bool{}
	for _, c := range r.Enabled() {
		if !c.Ready() {
			continue
		}
		vendor := c.Vendor()
		for _, m := range c.effectiveModels() {
			id := c.up.ModelPrefix + m.Outward()
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, ModelEntry{
				ID: id, Upstream: m.ID, Channel: c.up.Name, Source: c.up.Source,
				Vendor: vendor, Context: m.Context, MaxOut: m.MaxOut,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Statuses 全部渠道的状态（面板用）。
func (r *Registry) Statuses() []ChannelStatus {
	chans := r.Channels()
	out := make([]ChannelStatus, 0, len(chans))
	for _, c := range chans {
		out = append(out, c.Status())
	}
	return out
}

// Vendor 渠道所属厂商（来自预设模板，手填渠道按显示名推断）。
func (c *Channel) Vendor() string {
	if c.up.Preset != "" {
		if p, ok := PresetByID(c.up.Preset); ok && p.Vendor != "" {
			return p.Vendor
		}
		if p, ok := ManagedPresetByID(c.up.Preset); ok && p.Vendor != "" {
			return p.Vendor
		}
	}
	return c.up.DisplayName
}

// ProbeConsoles 在后台探测各托管渠道的控制台形态（内部有 TTL，很便宜）。
// 与 PrefetchModels 同模式：不阻塞、可反复调用。
func (r *Registry) ProbeConsoles() {
	for _, c := range r.Channels() {
		c.ProbeConsole()
	}
}

// PrefetchModels 在后台为「自动」模式且尚未声明模型的渠道拉一次模型列表。
//
// 刻意不阻塞启动：上游不可达时也要能进面板（否则用户连改配置的入口都没有）。
// 可被反复调用：内部有时间窗，不会变成对上游的定时轰炸。
func (r *Registry) PrefetchModels() {
	now := time.Now()
	for _, c := range r.Enabled() {
		if !c.shouldPrefetch(now) {
			continue
		}
		go func(ch *Channel) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			ids, err := ch.FetchModels(ctx)
			if err != nil {
				ch.lg.Warn("预取模型列表失败（可稍后在面板手动拉取）", "err", err.Error())
				return
			}
			ch.lg.Info("预取模型列表成功", "count", len(ids))
		}(c)
	}
}
