// Package claim 限时套餐的定时领取。
//
// 场景：某些上游（zcode2api 对接的 Z.AI）每天固定时刻放出一批限时免费套餐，
// 名额有限、抢完即止。让人守着点领取不现实，由本包代劳。
//
// 三条设计约束，每条都对应一类真实翻车：
//
//  1. **窗口而非精确时刻**。上游的回执里 next_at（名额恢复时间）会浮动，
//     精确到分钟的定时器会错过；机器在 12:00 关机、12:03 开机更是整天错过。
//     所以是「12:01–12:05 内跑一次」，窗口内启动会补领当天这一次。
//  2. **同日幂等**。窗口有 4 分钟、轮询每 30 秒，不做同日去重会重复领取——
//     而领取是**上游写流量**，重复调用可能撞验证码配额甚至触发风控。
//  3. **默认关闭**。自动领取会消耗账号的写流量配额，这种事不能替用户决定。
package claim

import (
	"context"
	"fmt"
	"sync"
	"time"

	"mergence/internal/config"
)

// TickInterval 判定周期。30 秒足够细：窗口最短也有一分钟级容错。
const TickInterval = 30 * time.Second

// Result 一次领取的结果。
type Result struct {
	// At 执行时刻。
	At time.Time `json:"at"`
	// Trigger 触发方式：auto（窗口内自动）/ manual（手动）。
	Trigger string `json:"trigger"`
	// Channel 目标渠道名。
	Channel string `json:"channel"`
	// OK 成功领取的账号数。
	OK int `json:"ok"`
	// Fail 失败的账号数。
	Fail int `json:"fail"`
	// Outcomes 逐账号明细（截断到前若干条，避免面板被刷屏）。
	Outcomes []Outcome `json:"outcomes"`
	// Error 整体失败原因（拿不到 HTTP 回执时）。有回执时为空。
	Error string `json:"error,omitempty"`
	// Skipped 当次未真正调用上游的原因：不在窗口 / 今天已领过 / 开关关闭 /
	// 渠道不可用。留空表示真的执行了。
	Skipped string `json:"skipped,omitempty"`
}

// Outcome 单个账号的领取结果。
type Outcome struct {
	Account string `json:"account"`
	OK      bool   `json:"ok"`
	Plan    string `json:"plan,omitempty"`
	Message string `json:"message,omitempty"`
	// NextAt 上游说的名额恢复时间（1005 名额用完时给出）。
	NextAt string `json:"next_at,omitempty"`
}

// SettingsProvider 提供当前领取设置。每次判定时调用，因此支持热改开关。
type SettingsProvider func() config.ClaimConfig

// Runner 执行一次领取。由 web 层注入（要访问子进程地址与 HTTP 客户端）。
type Runner func(ctx context.Context, cfg config.ClaimConfig, trigger string) Result

// Scheduler 定时领取调度器。
type Scheduler struct {
	settings SettingsProvider
	run      Runner
	logf     func(msg string, args ...any)

	// now 时间源。抽成字段是为了让测试能固定「现在」——
	// 同日幂等比较的是日期字符串，测试若用固定日期而 Manual 用真实时钟，
	// 一到跨零点（23:59 之后跑测试）两者就差一天，幂等判定会假性失败。
	now func() time.Time

	mu       sync.Mutex
	lastDay  string   // 上次**实际执行**的日期（yyyy-mm-dd），用于同日幂等
	last     Result   // 最近一次结果（成功或跳过）
	history  []Result // 最近若干次，供面板查看
	stop     chan struct{}
	stopOnce sync.Once
}

// New 创建调度器。logf 可为 nil。
func New(settings SettingsProvider, run Runner, logf func(string, ...any)) *Scheduler {
	return &Scheduler{
		settings: settings, run: run, logf: logf,
		now:  time.Now,
		stop: make(chan struct{}),
	}
}

// Start 启动后台协程（阻塞直到 ctx 取消或 Stop）。
func (s *Scheduler) Start(ctx context.Context) {
	t := time.NewTicker(TickInterval)
	defer t.Stop()
	// 启动时立刻判一次：进程若是在窗口内启动的，这一次就能补领。
	s.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case now := <-t.C:
			s.tickAt(ctx, now)
		}
	}
}

// Stop 停止调度器。
func (s *Scheduler) Stop() { s.stopOnce.Do(func() { close(s.stop) }) }

// Tick 暴露一次判定，供测试与「立即检查」使用。
func (s *Scheduler) Tick(ctx context.Context) { s.tickAt(ctx, s.now()) }

func (s *Scheduler) tickAt(ctx context.Context, now time.Time) {
	cfg := s.settings()
	if !cfg.Enabled {
		// 开关关闭时**什么都不记**：面板上「未启用」比「每天跳过一次」清楚。
		return
	}
	start := cfg.WindowStart(now)
	win := time.Duration(cfg.Window) * time.Minute
	if now.Before(start) || now.After(start.Add(win)) {
		return
	}
	day := now.Format("2006-01-02")

	s.mu.Lock()
	if s.lastDay == day {
		s.mu.Unlock()
		return // 今天已领过：窗口内会 tick 多次，必须去重
	}
	s.lastDay = day // 先占位再执行：领取慢时也不会被并发的 tick 重复触发
	s.mu.Unlock()

	res := s.safeRun(ctx, cfg, "auto")
	s.record(res)
}

// Manual 手动触发一次领取（面板按钮）。
//
// 手动**不受开关限制**：用户点按钮就是要「现在领」，自动开关只管自动行为。
// 但结果里要写明「自动领取未启用」——否则他以为开关已经生效。
// 同样不受「同日已领」限制：领取本身在上游是幂等的（已领会回「已领取」），
// 沙雕情况是用户想补领一次（比如上次验证码失败）。
func (s *Scheduler) Manual(ctx context.Context) Result {
	cfg := s.settings()
	res := s.safeRun(ctx, cfg, "manual")
	if !cfg.Enabled {
		res.Skipped = "自动领取未启用（本次为手动执行）"
	}
	// 手动成功也记为「今天已领」，避免窗口内的自动轮重复去领。
	s.mu.Lock()
	s.lastDay = s.now().Format("2006-01-02")
	s.mu.Unlock()
	s.record(res)
	return res
}

// safeRun 执行领取，panic 也要兜住。
//
// 领取失败不该让整个 Mergence 挂掉：这是附加功能，
// 不能因为上游返回一个奇怪状态就带走主进程。
func (s *Scheduler) safeRun(ctx context.Context, cfg config.ClaimConfig, trigger string) Result {
	defer func() {
		if p := recover(); p != nil && s.logf != nil {
			s.logf("领取过程异常", "panic", fmt.Sprint(p))
		}
	}()
	return s.run(ctx, cfg, trigger)
}

func (s *Scheduler) record(r Result) {
	s.mu.Lock()
	s.last = r
	s.history = append([]Result{r}, s.history...)
	if len(s.history) > 20 {
		s.history = s.history[:20]
	}
	s.mu.Unlock()
	if s.logf == nil {
		return
	}
	switch {
	case r.Skipped != "":
		s.logf("限时套餐未领取", "原因", r.Skipped)
	case r.Error != "":
		s.logf("限时套餐领取失败", "err", r.Error)
	default:
		s.logf("限时套餐领取完成", "成功", r.OK, "失败", r.Fail, "渠道", r.Channel)
	}
}

// Status 返回面板需要的全部状态。
func (s *Scheduler) Status(cfg config.ClaimConfig) StatusView {
	return s.statusAt(cfg, s.now())
}

// statusAt 让「现在」可注入：InWindow/TodayDone 都依赖当前时刻，
// 用真实时间写测试就只能挑特定钟点跑，测试会变成 flaky。
func (s *Scheduler) statusAt(cfg config.ClaimConfig, now time.Time) StatusView {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := cfg.WindowStart(now)
	win := time.Duration(cfg.Window) * time.Minute
	return StatusView{
		Enabled:    cfg.Enabled,
		At:         cfg.At,
		Window:     cfg.Window,
		Channel:    cfg.Channel,
		Configured: cfg.AdminKey != "",
		// 窗口内且今天还没领 → 「可领取」，面板据此高亮按钮。
		InWindow:   !now.Before(start) && !now.After(start.Add(win)),
		TodayDone:  s.lastDay == now.Format("2006-01-02"),
		WindowFrom: start.Format("15:04"),
		WindowTo:   start.Add(win).Format("15:04"),
		Last:       s.last,
		History:    s.history,
	}
}

// StatusView 面板展示用的状态。
type StatusView struct {
	Enabled    bool     `json:"enabled"`
	At         string   `json:"at"`
	Window     int      `json:"window"`
	Channel    string   `json:"channel"`
	Configured bool     `json:"configured"` // 是否填了后台密码
	InWindow   bool     `json:"in_window"`
	TodayDone  bool     `json:"today_done"`
	WindowFrom string   `json:"window_from"`
	WindowTo   string   `json:"window_to"`
	Last       Result   `json:"last"`
	History    []Result `json:"history"`
}
