// scheduler_test.go 定时领取的判定逻辑。
//
// 重点测三类真实翻车：窗口边界、同日重复领取、开关关闭。
// 这些都不是「跑起来看日志」能发现的，必须用可控时钟验证。
package claim

import (
	"context"
	"testing"
	"time"

	"modelmux/internal/config"
)

func at(h, m int) time.Time {
	return time.Date(2026, 10, 2, h, m, 0, 0, time.Local)
}

func testCfg() config.ClaimConfig {
	return config.ClaimConfig{Enabled: true, At: "12:01", Window: 4, AdminKey: "k"}
}

func TestTickInsideWindow(t *testing.T) {
	// 每次判定用独立的调度器与计数器，避免互相干扰。
	newS := func(runs *int) *Scheduler {
		return New(func() config.ClaimConfig { return testCfg() },
			func(_ context.Context, _ config.ClaimConfig, trigger string) Result {
				*runs++
				return Result{OK: 1, Trigger: trigger}
			}, nil)
	}

	// 12:01–12:05 之间：应触发
	for _, tc := range []struct{ h, m int }{{12, 1}, {12, 2}, {12, 3}, {12, 5}} {
		runs := 0
		newS(&runs).tickAt(context.Background(), at(tc.h, tc.m))
		if runs != 1 {
			t.Errorf("%02d:%02d 应触发领取，实际触发 %d 次", tc.h, tc.m, runs)
		}
	}
	// 窗口外：不应触发
	for _, tc := range []struct{ h, m int }{{11, 59}, {12, 0}, {12, 6}, {18, 0}} {
		runs := 0
		newS(&runs).tickAt(context.Background(), at(tc.h, tc.m))
		if runs != 0 {
			t.Errorf("%02d:%02d 不在窗口内却触发了领取", tc.h, tc.m)
		}
	}
}

func TestTickIdempotentSameDay(t *testing.T) {
	// 窗口内有 4 分钟、每 30 秒 tick 一次 ≈ 8 次判定。
	// 不做同日去重就会重复领取 8 次——那是上游写流量，可能撞验证码配额。
	var runs int
	s := New(func() config.ClaimConfig { return testCfg() },
		func(_ context.Context, _ config.ClaimConfig, trigger string) Result {
			runs++
			return Result{OK: 1, Trigger: trigger}
		}, nil)

	for _, m := range []int{1, 1, 2, 2, 3, 4, 5} {
		s.tickAt(context.Background(), at(12, m))
	}
	if runs != 1 {
		t.Errorf("同一天应只领取一次，实际 %d 次", runs)
	}
	// 次日窗口内应再次触发
	s.tickAt(context.Background(), at(0, 0).AddDate(0, 0, 1).Add(12*time.Hour+time.Minute))
	if runs != 2 {
		t.Errorf("次日应再次领取，累计应 2 次，实际 %d 次", runs)
	}
}

func TestDisabledDoesNothing(t *testing.T) {
	var runs int
	s := New(func() config.ClaimConfig {
		c := testCfg()
		c.Enabled = false
		return c
	}, func(_ context.Context, _ config.ClaimConfig, trigger string) Result {
		runs++
		return Result{}
	}, nil)

	s.tickAt(context.Background(), at(12, 2))
	if runs != 0 {
		t.Errorf("开关关闭时不应领取，实际 %d 次", runs)
	}
	// 关闭时也不该记录任何结果：面板要显示「未启用」而不是「跳过一次」
	st := s.Status(config.ClaimConfig{Enabled: false, At: "12:01", Window: 4})
	if !st.Last.At.IsZero() {
		t.Errorf("开关关闭时不应留下结果记录：%+v", st.Last)
	}
}

func TestWindowStartInsideDayCatchesLateBoot(t *testing.T) {
	// 机器 12:00 关机、12:03 才开机：这次判定必须补领当天这一次，
	// 否则整天错过（用户第二天才发现没领到）。
	var runs int
	s := New(func() config.ClaimConfig { return testCfg() },
		func(_ context.Context, _ config.ClaimConfig, trigger string) Result {
			runs++
			return Result{OK: 1, Trigger: trigger}
		}, nil)
	s.tickAt(context.Background(), at(12, 3))
	if runs != 1 {
		t.Errorf("窗口内启动应补领当天的领取，实际 %d 次", runs)
	}
}

func TestCustomWindowAndAt(t *testing.T) {
	var runs int
	cfg := config.ClaimConfig{Enabled: true, At: "09:30", Window: 10, AdminKey: "k"}
	s := New(func() config.ClaimConfig { return cfg },
		func(_ context.Context, _ config.ClaimConfig, trigger string) Result {
			runs++
			return Result{}
		}, nil)

	s.tickAt(context.Background(), at(9, 25)) // 窗口前
	if runs != 0 {
		t.Error("09:25 不该触发（窗口 09:30 起）")
	}
	s.tickAt(context.Background(), at(9, 39)) // 窗口内
	if runs != 1 {
		t.Error("09:39 应触发")
	}
}

func TestManualRunsEvenWhenDisabled(t *testing.T) {
	// 手动是用户当下的明确意图：即使自动开关关着也要能领，
	// 但结果里要说清「自动未启用」。
	var got string
	s := New(func() config.ClaimConfig {
		return config.ClaimConfig{Enabled: false, At: "12:01", Window: 4}
	}, func(_ context.Context, _ config.ClaimConfig, trigger string) Result {
		got = trigger
		return Result{OK: 2, Trigger: trigger}
	}, nil)

	res := s.Manual(context.Background())
	if got != "manual" {
		t.Errorf("手动触发应标记 manual，实际 %q", got)
	}
	if res.OK != 2 {
		t.Errorf("手动结果应保留执行内容：%+v", res)
	}
	// 自动判定随后在窗口内不应重复领（手动已记为今天已领）
	var runs int
	s2 := New(func() config.ClaimConfig { return testCfg() },
		func(_ context.Context, _ config.ClaimConfig, trigger string) Result {
			runs++
			return Result{}
		}, nil)
	// 固定时钟：Manual 记的「今天」必须与下面 tickAt 的「今天」是同一天，
	// 否则这个测试只在北京时间中午前后才通过。
	s2.now = func() time.Time { return at(12, 2) }
	s2.Manual(context.Background())
	s2.tickAt(context.Background(), at(12, 2))
	if runs != 1 {
		t.Errorf("手动领过后窗口内不应再自动领，实际执行 %d 次", runs)
	}
}

func TestPanicContained(t *testing.T) {
	// 上游返回奇怪状态导致 panic 时，不能带走主进程。
	s := New(func() config.ClaimConfig { return testCfg() },
		func(_ context.Context, _ config.ClaimConfig, trigger string) Result {
			panic("boom")
		}, nil)
	s.tickAt(context.Background(), at(12, 2)) // 不应崩溃
}

func TestStatusView(t *testing.T) {
	s := New(func() config.ClaimConfig { return testCfg() },
		func(_ context.Context, _ config.ClaimConfig, trigger string) Result {
			return Result{OK: 3, Fail: 1, Trigger: trigger}
		}, nil)
	s.tickAt(context.Background(), at(12, 2))
	st := s.statusAt(testCfg(), at(12, 2)) // 注入「现在」= 窗口内
	if !st.Enabled || !st.InWindow || !st.TodayDone {
		t.Errorf("状态不对：%+v", st)
	}
	if st.WindowFrom != "12:01" || st.WindowTo != "12:05" {
		t.Errorf("窗口展示不对：%s–%s", st.WindowFrom, st.WindowTo)
	}
	if st.Last.OK != 3 || st.Last.Fail != 1 {
		t.Errorf("最近结果不对：%+v", st.Last)
	}
	if !st.Configured {
		t.Error("填了 admin_key 时 Configured 应为 true")
	}
}

func TestHistoryCapped(t *testing.T) {
	// 历史条数要有上限，否则面板长期运行会把内存吃满。
	s := New(func() config.ClaimConfig { return testCfg() },
		func(_ context.Context, _ config.ClaimConfig, trigger string) Result {
			return Result{OK: 1}
		}, nil)
	for i := 0; i < 30; i++ {
		s.record(Result{OK: i})
	}
	s.mu.Lock()
	n := len(s.history)
	s.mu.Unlock()
	if n > 20 {
		t.Errorf("历史条数应封顶 20，实际 %d", n)
	}
}
