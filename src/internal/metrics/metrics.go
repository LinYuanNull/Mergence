// metrics.go Mergence 自身的调用计量。
//
// 存在的原因：面板要展示「API 费用 / 调用次数 / 缓存命中率」，而这些数字
// 上游不会主动交给我们——它只在响应体里回一句 usage。仅靠日志里的
// tokens 字段做不了聚合（环形缓冲会滚掉），所以这里独立记录每次调用，
// 落 JSONL、可回溯、可按天与按渠道聚合。
//
// 三条设计约束：
//
//  1. **费用只在有价格时才算**。价格表可能过期、模型可能没收录，
//     这时返回 Unknown 而不是 0——显示「$0.00」会被误读成"没花钱"，
//     而真相是"不知道花了多少"。Unknown 在面板上明确标成「价格未知」。
//  2. **计量失败绝不影响转发**。写盘是旁路，失败只记日志。
//  3. **不猜口径**。缓存命中率的分母是输入 token（OpenAI 口径），
//     分母为 0 时不给命中率，而不是给 0%。
package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Usage 一次调用的 token 用量。
//
// 字段全部允许为 0：不同上游给的字段差异很大（有的给 prompt_tokens，
// 有的只给 total_tokens），缺失就留 0，由归一化层决定哪些指标「有数据」。
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens int64 `json:"completion_tokens,omitempty"`
	TotalTokens      int64 `json:"total_tokens,omitempty"`
	// CachedTokens 命中缓存的输入 token。
	// OpenAI: usage.prompt_tokens_details.cached_tokens
	// Anthropic: usage.cache_read_input_tokens
	CachedTokens int64 `json:"cached_tokens,omitempty"`
	// CacheWriteTokens 写入缓存的 token（Anthropic: cache_creation_input_tokens）。
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	// HaveHitMiss 上游是否分别给了「命中/未命中」两个计数。
	// DeepSeek 用 prompt_cache_hit_tokens + prompt_cache_miss_tokens 这种口径；
	// OpenAI 只给 cached_tokens，需要拿 prompt_tokens 当分母。
	HaveHitMiss bool `json:"have_hit_miss,omitempty"`
	// PromptExcludesCache 为 true 表示 PromptTokens **不含**缓存命中部分
	// （Anthropic 口径）。OpenAI 的 prompt_tokens 是含缓存的。
	//
	// 必须显式区分：两种口径下「未缓存输入」算法相反，
	// 混用会让一个 8k 缓存 + 500 新输入的请求显示成 500 token。
	PromptExcludesCache bool `json:"prompt_excludes_cache,omitempty"`
}

// Normalized 归一化后的 token 计数，供计价与展示使用。
type Normalized struct {
	// Input 未命中缓存的输入 token。
	Input int64
	// CachedInput 命中缓存的输入 token。
	CachedInput int64
	// CacheWrite 写入缓存的 token。
	CacheWrite int64
	// Output 输出 token。
	Output int64
	// Total 总量。
	Total int64
}

// Normalize 把各上游口径归一到同一组数字。
func (u Usage) Normalize() Normalized {
	n := Normalized{
		CachedInput: u.CachedTokens,
		CacheWrite:  u.CacheWriteTokens,
		Output:      u.CompletionTokens,
		Total:       u.TotalTokens,
	}
	if n.Total == 0 {
		n.Total = u.PromptTokens + u.CompletionTokens
	}
	// 拆分「未缓存输入」：两家的 prompt_tokens 口径相反，必须分别处理。
	//   OpenAI 口径（含缓存）：未缓存输入 = prompt - cached
	//   Anthropic 口径（不含缓存）：未缓存输入 = prompt
	// 统一写成「总量减缓存」的形式：Anthropic 下 prompt 已经是不含缓存的量，
	// 所以 total 要自己把 cached 加回来，否则会重复扣减。
	if u.PromptExcludesCache {
		n.Input = u.PromptTokens
		if n.Total == 0 {
			n.Total = n.Input + n.CachedInput + n.Output
		}
	} else {
		switch {
		case u.HaveHitMiss:
			n.Input = u.PromptTokens - u.CachedTokens
		case u.PromptTokens > 0:
			n.Input = u.PromptTokens - u.CachedTokens
		default:
			// 上游没给 prompt_tokens 就无法拆分，保守地把剩余量都算作输入。
			n.Input = n.Total - n.Output
		}
	}
	if n.Input < 0 {
		n.Input = 0
	}
	return n
}

// Record 一条调用计量。
type Record struct {
	Time time.Time `json:"time"`
	// Channel 渠道名（Mergence 内部标识）。
	Channel string `json:"channel"`
	// Source 渠道来源：embedded（API 型平台）/ managed（积分型平台）。
	Source string `json:"source"`
	// Model 对外模型名（含前缀）。
	Model string `json:"model"`
	// UpstreamModel 上游真实模型名（价格表按它匹配）。
	UpstreamModel string `json:"upstream_model,omitempty"`
	Usage         Usage  `json:"usage"`
	// OK 本次调用是否成功。失败的调用照样计费（上游可能已扣费），
	// 但不计入「调用次数」的成功口径，分开统计。
	OK         bool  `json:"ok"`
	Status     int   `json:"status,omitempty"`
	DurationMS int64 `json:"duration_ms,omitempty"`
	Stream     bool  `json:"stream,omitempty"`
	// Error 失败原因摘要（成功时为空）。
	Error string `json:"error,omitempty"`
}

// warnFunc 日志回调。用函数而非直接依赖 logging 包，避免包间耦合成环。
type warnFunc func(msg string, args ...any)

// Store 计量存储：内存每日聚合 + JSONL 追加落盘。
//
// 明细不长期驻留内存：概览要的是聚合数字，把几个月前的每条记录全塞进
// 环形缓冲没有意义。落盘的 JSONL 供「最近调用」列表与外部工具使用。
type Store struct {
	dir string

	mu   sync.RWMutex
	days map[string]*DayAgg
	// recent 最近的明细（面板「最近调用」用），固定长度环形。
	recent    []Record
	head      int
	full      bool
	dirtyDays int // 本次启动后有写入的天数，用于区分「历史」与「本次」

	fileMu    sync.Mutex
	file      *os.File
	fileDay   string
	warnFn    warnFunc
	closeOnce sync.Once
}

const recentCap = 500

// NewStore 创建存储。dir 为空时只做内存计量（测试与 headless 用）。
func NewStore(dir string) *Store {
	s := &Store{dir: dir, days: map[string]*DayAgg{}, recent: make([]Record, recentCap)}
	if dir != "" {
		s.load()
	}
	return s
}

// SetWarnFunc 注入日志回调。
func (s *Store) SetWarnFunc(fn warnFunc) { s.warnFn = fn }

func (s *Store) warn(msg string, args ...any) {
	if s.warnFn != nil {
		s.warnFn(msg, args...)
	}
}

// logPath 返回某天的 JSONL 路径。
//
// 按天分文件：一个月后要清理/归档时按文件名操作即可，
// 读取时也不用判断一条记录属于哪一天。
func (s *Store) logPath(t time.Time) string {
	return filepath.Join(s.dir, "usage-"+t.Format("2006-01-02")+".jsonl")
}

// ensureFile 打开当天的追加文件（调用方须持 fileMu）。
func (s *Store) ensureFile(t time.Time) *os.File {
	day := t.Format("2006-01-02")
	if s.file != nil && s.fileDay == day {
		return s.file
	}
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		s.warn("计量目录创建失败，本次只统计内存", "dir", s.dir, "err", err.Error())
		s.dir = ""
		return nil
	}
	f, err := os.OpenFile(s.logPath(t), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		s.warn("计量文件打开失败，本次只统计内存", "err", err.Error())
		s.dir = ""
		return nil
	}
	s.file, s.fileDay = f, day
	return f
}

// Record 记录一次调用。
func (s *Store) Record(r Record) {
	if r.Time.IsZero() {
		r.Time = time.Now()
	}
	s.mu.Lock()
	day := s.days[r.Time.Format("2006-01-02")]
	if day == nil {
		day = &DayAgg{}
		s.days[r.Time.Format("2006-01-02")] = day
	}
	day.add(r)
	s.recent[s.head] = r
	s.head = (s.head + 1) % len(s.recent)
	if s.head == 0 {
		s.full = true
	}
	s.dirtyDays++
	s.mu.Unlock()

	if s.dir == "" {
		return
	}
	s.fileMu.Lock()
	defer s.fileMu.Unlock()
	f := s.ensureFile(r.Time)
	if f == nil {
		return
	}
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		s.warn("计量记录写入失败", "err", err.Error())
	}
}

// Close 关闭落盘文件。
func (s *Store) Close() error {
	var err error
	s.closeOnce.Do(func() {
		s.fileMu.Lock()
		defer s.fileMu.Unlock()
		if s.file != nil {
			err = s.file.Close()
			s.file = nil
		}
	})
	return err
}

// load 启动时把已有 JSONL 读回聚合。
//
// 单行损坏只跳过该行，不让整段历史作废——手工编辑/意外截断都可能发生。
func (s *Store) load() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if !os.IsNotExist(err) {
			s.warn("计量目录读取失败，本次从零开始统计", "dir", s.dir, "err", err.Error())
		}
		return
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, "usage-") || !strings.HasSuffix(n, ".jsonl") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, n))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var r Record
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				continue
			}
			day := s.days[r.Time.Format("2006-01-02")]
			if day == nil {
				day = &DayAgg{}
				s.days[r.Time.Format("2006-01-02")] = day
			}
			day.add(r)
		}
	}
}

// Recent 返回最近 n 条明细（时间正序）。
func (s *Store) Recent(n int) []Record {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cnt := s.head
	if s.full {
		cnt = len(s.recent)
	}
	if n <= 0 || n > cnt {
		n = cnt
	}
	out := make([]Record, 0, n)
	start := cnt - n
	for i := 0; i < n; i++ {
		out = append(out, s.recent[(start+i)%len(s.recent)])
	}
	return out
}

// Agg 一段时间内的聚合结果。
type Agg struct {
	Requests     int64 `json:"requests"`
	Errors       int64 `json:"errors"`
	InputTokens  int64 `json:"input_tokens"`
	CachedTokens int64 `json:"cached_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	CacheWrites  int64 `json:"cache_write_tokens"`
	DurationMS   int64 `json:"duration_ms"`
	// PromptTotal 输入 + 缓存命中，缓存命中率的分母。
	PromptTotal int64 `json:"prompt_total"`
}

func (a *Agg) add(r Record) {
	if r.OK {
		a.Requests++
	} else {
		a.Errors++
	}
	n := r.Usage.Normalize()
	a.InputTokens += n.Input
	a.CachedTokens += n.CachedInput
	a.OutputTokens += n.Output
	a.TotalTokens += n.Total
	a.CacheWrites += n.CacheWrite
	a.DurationMS += r.DurationMS
	a.PromptTotal += n.Input + n.CachedInput
}

func (a *Agg) merge(b Agg) {
	a.Requests += b.Requests
	a.Errors += b.Errors
	a.InputTokens += b.InputTokens
	a.CachedTokens += b.CachedTokens
	a.OutputTokens += b.OutputTokens
	a.TotalTokens += b.TotalTokens
	a.CacheWrites += b.CacheWrites
	a.DurationMS += b.DurationMS
	a.PromptTotal += b.PromptTotal
}

// CacheHitRate 返回缓存命中率（0–1）。分母为 0 时 ok=false。
//
// 不返回 0 是因为「没有输入 token」和「命中率真的是 0」在面板上
// 必须是两回事：前者是没数据，后者是上游一次都没命中。
func (a Agg) CacheHitRate() (rate float64, ok bool) {
	if a.PromptTotal <= 0 {
		return 0, false
	}
	return float64(a.CachedTokens) / float64(a.PromptTotal), true
}

// AvgLatencyMS 平均延迟（毫秒）。
func (a Agg) AvgLatencyMS() int64 {
	n := a.Requests + a.Errors
	if n <= 0 {
		return 0
	}
	return a.DurationMS / n
}

// PairKey 「渠道 × 模型」复合键。
//
// 为什么保留这个维度：费用必须按模型算（同一渠道下不同模型单价差几十倍），
// 而费用又必须归属到渠道。少存这一个维度，就得在聚合后靠 token 总量去
// 反推「这个模型属于哪个渠道」——那是猜，猜错就把钱算到别的渠道头上了。
type PairKey struct {
	Channel string
	Model   string
}

// DayAgg 单日聚合：按渠道、模型、以及渠道×模型三个维度分桶。
type DayAgg struct {
	Agg
	byChannel map[string]*Agg
	byModel   map[string]*Agg
	byPair    map[PairKey]*Agg
	sources   SourceByChannel
}

func (d *DayAgg) add(r Record) {
	d.Agg.add(r)
	if d.byChannel == nil {
		d.byChannel = map[string]*Agg{}
		d.byModel = map[string]*Agg{}
		d.byPair = map[PairKey]*Agg{}
		d.sources = SourceByChannel{}
	}
	// 渠道来源取当天最后一次见到的值：渠道类型不会中途变，
	// 真变了也只影响展示标签，不影响已记的 token。
	if src := strings.TrimSpace(r.Source); src != "" {
		d.sources[r.Channel] = src
	}
	c, ok := d.byChannel[r.Channel]
	if !ok {
		c = &Agg{}
		d.byChannel[r.Channel] = c
	}
	c.add(r)
	m, ok := d.byModel[r.Model]
	if !ok {
		m = &Agg{}
		d.byModel[r.Model] = m
	}
	m.add(r)
	pk := PairKey{Channel: r.Channel, Model: r.Model}
	p, ok := d.byPair[pk]
	if !ok {
		p = &Agg{}
		d.byPair[pk] = p
	}
	p.add(r)
}

// SourceByChannel 渠道来源（embedded / managed），来自记录本身。
type SourceByChannel map[string]string

// Daily 一天的聚合视图。
type Daily struct {
	Date string `json:"date"`
	Agg
	ByChannel map[string]*Agg  `json:"by_channel,omitempty"`
	ByModel   map[string]*Agg  `json:"by_model,omitempty"`
	ByPair    map[PairKey]*Agg `json:"by_pair,omitempty"`
	Sources   SourceByChannel  `json:"sources,omitempty"`
}

// Query 聚合查询结果。
type Query struct {
	Days int `json:"days"`
	// Total 全部渠道合计。
	Total Agg `json:"total"`
	// ByChannel 按渠道名分列。
	ByChannel map[string]*Agg `json:"by_channel"`
	// ByModel 按对外模型名分列。
	ByModel map[string]*Agg `json:"by_model"`
	// ByPair 按「渠道×模型」分列。计价走这个维度。
	ByPair map[PairKey]*Agg `json:"-"`
	// Sources 渠道来源。
	Sources SourceByChannel `json:"sources"`
	// Daily 按天的时间序列（时间正序）。
	Daily []Daily `json:"daily"`
	// RangeStart / RangeEnd 实际覆盖的日期范围。
	RangeStart string `json:"range_start,omitempty"`
	RangeEnd   string `json:"range_end,omitempty"`
	// HasData 是否存在任何计量记录。false 时面板显示「暂无数据」，
	// 而不是把一堆 0 渲染成图表。
	HasData bool `json:"has_data"`
	// Restored 聚合里含重启前从 JSONL 读回的历史。
	Restored bool `json:"restored"`
}

// Query 聚合最近 days 天的计量。days<=0 表示全部。
func (s *Store) Query(days int) Query {
	s.mu.RLock()
	defer s.mu.RUnlock()

	q := Query{
		Days: days, Sources: SourceByChannel{},
		ByChannel: map[string]*Agg{}, ByModel: map[string]*Agg{},
		ByPair: map[PairKey]*Agg{},
	}

	keys := make([]string, 0, len(s.days))
	for k := range s.days {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		d := s.days[k]
		if !inRange(k, days) {
			continue
		}
		if !q.HasData {
			q.HasData = true
			// 本次启动没写过任何记录 → 命中的都是磁盘上的历史。
			q.Restored = s.dirtyDays == 0
		}
		if q.RangeStart == "" {
			q.RangeStart = k
		}
		q.RangeEnd = k
		q.Total.merge(d.Agg)
		mergeInto(q.ByChannel, d.byChannel)
		mergeInto(q.ByModel, d.byModel)
		mergePair(q.ByPair, d.byPair)
		for ch, src := range d.sources {
			if _, ok := q.Sources[ch]; !ok {
				q.Sources[ch] = src
			}
		}
		q.Daily = append(q.Daily, Daily{Date: k, Agg: d.Agg,
			ByChannel: copyAgg(d.byChannel), ByModel: copyAgg(d.byModel)})
	}
	return q
}

func mergePair(dst, src map[PairKey]*Agg) {
	for k, v := range src {
		if dst[k] == nil {
			dst[k] = &Agg{}
		}
		dst[k].merge(*v)
	}
}

func mergeInto(dst, src map[string]*Agg) {
	for k, v := range src {
		if dst[k] == nil {
			dst[k] = &Agg{}
		}
		dst[k].merge(*v)
	}
}

func copyAgg(src map[string]*Agg) map[string]*Agg {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]*Agg, len(src))
	for k, v := range src {
		c := *v
		out[k] = &c
	}
	return out
}

// inRange 判断日期（YYYY-MM-DD）是否落在最近 days 天内。
func inRange(date string, days int) bool {
	if days <= 0 {
		return true
	}
	t, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		return false
	}
	cut := time.Now().AddDate(0, 0, -(days - 1))
	cut = time.Date(cut.Year(), cut.Month(), cut.Day(), 0, 0, 0, 0, time.Local)
	return !t.Before(cut)
}
