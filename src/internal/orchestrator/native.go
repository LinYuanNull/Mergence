// native.go 原生型 provider 的编排。
//
// ── 与子进程路径的差别只有三处 ───────────────────────────────
//
//  1. **不分配端口**：原生服务绑 127.0.0.1:0，端口由内核给，且**不进 ports.json**
//     （端口表服务于「用户要把地址填到别处」的子进程实例，原生实例没有这个需求）。
//  2. **不 exec**：改成本进程内装配（native.Boot）+ 回环监听（native.Serve）。
//  3. **停止是优雅 Close**（停监听 → drain 在途请求 → 上游落盘/关库），
//     不是杀进程树——它本来就在本进程里，没有进程可杀。
//
// 除此之外一切相同：状态机（starting / running / failed / stopped）、
// 就绪判定（真的发一次 HTTP 探活，而不是「handler 构造完就算好」）、
// Status 里的 Port / BaseURL 语义、Start / Restart / Shutdown 的行为。
//
// ── 为什么这层是四条接缝能一行不改的原因 ─────────────────────
//
// 接缝（前端控制台代理 / 服务端管理代理 / 领取执行器 / 数据面转发）全都只依赖
// 「一个 RootURL + 一个就绪状态」，而这两个值**只由编排器产出**。把原生后端
// 塞进编排器，接缝拿到的东西与对着子进程时**逐字段一致**，于是它们看不出区别，
// 也不需要看出区别。
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"modelmux/internal/config"
	"modelmux/internal/native"
)

// nativeStopGrace 优雅停机的上限。
//
// 需要它是因为上游有在途的流式请求（SSE）：硬关连接会截断用户正在收的回答。
// 但不能无限等——「重启渠道 / 退出」按钮不能因为某个卡住的请求而失效。
// 超时后 native.Runtime 会硬断（它自己的 Shutdown 语义），我们继续往下走。
const nativeStopGrace = 5 * time.Second

// nativeReadyTimeout 原生实例的就绪探测上限。
//
// 监听在内核里是同步完成的（net.Listen 返回即可用），所以正常情况第一次探测
// 就通，这个超时几乎不会用满。留余量是为了让「首个请求就失败的上游」有事可报，
// 而不是被当成「还在启动」一直等下去。
const nativeReadyTimeout = 5 * time.Second

// nativeHandle 一个运行中的原生实例。
type nativeHandle struct {
	rt *native.Runtime
	// stop 上游自己的收尾（停排程 → 落盘 → 关库），由 native.Boot 给出。
	stop func() error

	// done 在 Serve 循环真正退出后关闭；exitErr 是它的错误。
	done    chan struct{}
	exitErr error

	closeOnce sync.Once
	finishOne sync.Once
}

// newNativeHandle 建立句柄并起**唯一**的退出观察者。
//
// 为什么要有观察者而不是让 Close 直接关 done：Serve 有两条退出路径——我们主动
// Close，或监听自己出错。两条都该让 Stop/Shutdown 的等待结束。让它们收敛到
// 同一个 finish()，调用方就只需等 done，不必区分来由（也就不会漏掉一条路径
// 而卡满 5 秒超时）。
func newNativeHandle(rt *native.Runtime, stop func() error) *nativeHandle {
	h := &nativeHandle{rt: rt, stop: stop, done: make(chan struct{})}
	go func() {
		// Wait 用 background：退出只由 Close 触发，不需要额外取消源。
		h.finish(rt.Wait(context.Background()))
	}()
	return h
}

func (h *nativeHandle) finish(err error) {
	h.finishOne.Do(func() {
		h.exitErr = err
		close(h.done)
	})
}

// Close 优雅停机，幂等。
//
// 顺序是「先停监听，再收上游」：反过来的话，上游收尾期间（Flush 账号池、
// 关 SQLite、写完最后一笔用量）仍可能有请求打进来，落盘与在途写会互相踩。
// Shutdown 会等完在途请求，所以不会截断正在收的流式回答。
func (h *nativeHandle) Close() error {
	var err error
	h.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), nativeStopGrace)
		defer cancel()
		_ = h.rt.Close(ctx)
		err = h.stop()
	})
	return err
}

// Wait 等 Serve 循环退出（Close 之后用）。
func (h *nativeHandle) Wait(ctx context.Context) error {
	select {
	case <-h.done:
		return h.exitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// startNative 启动一个原生型 provider，并等它就绪。
//
// 返回的错误与子进程路径同形（"<阶段>：<原因>"）：调用方（面板的 start_error、
// 状态里的 last_err）不区分形态，多一套错误格式只会让前端多一个分支。
func (o *Orchestrator) startNative(ctx context.Context, c config.ManagedProvider,
	inst *Instance, dataDir string, fail func(string, error) error) error {

	lg := o.log.WithProvider(c.Name)

	boot, ok := native.Lookup(c.Kind)
	if !ok {
		// 报错必须列出可用项：`kind: "workbudy"`（拼错）时，只有列出可选项
		// 用户才看得出问题，否则他只看到「未注册」，会去怀疑是没编译进去。
		return fail("未注册的原生实现", fmt.Errorf(
			"kind=%q 没有对应的内置实现（可用：%s）",
			c.Kind, strings.Join(native.Kinds(), "、")))
	}

	svc, err := boot(c, dataDir, lg)
	if err != nil {
		return fail("原生服务装配失败", err)
	}
	if svc == nil || svc.Handler == nil {
		// 实现契约要求两者非空。这里挡住而不是让它在 Serve 里空指针，
		// 因为空 handler 的 panic 发生在 http 包的 goroutine 里，栈里看不到是谁装配的。
		return fail("原生服务装配失败", errors.New("实现返回了空的 handler"))
	}

	rt, err := native.Serve(c.Name, svc.Handler, lg)
	if err != nil {
		// 装配已经产生了副作用（起了排程协程、拿了文件句柄、连了 Redis），
		// 监听失败也必须收掉，否则「装配成功但没起服务」会留下半条命实例。
		_ = svc.Close()
		return fail("原生服务监听失败", err)
	}

	nh := newNativeHandle(rt, svc.Close)
	inst.port, inst.startedAt, inst.native = rt.Port(), time.Now(), nh
	// 复用同一个「已回收」信号：Stop / Shutdown 不必按形态分支。
	inst.exited = nh.done

	lg.Info("原生 provider 已拉起",
		"port", inst.port, "kind", c.Kind, "data_dir", dataDir, "proto", "in-process")

	// 就绪判定与子进程完全同口径：真的发一次 HTTP 探活。
	// 不因为是进程内就跳过——「handler 构造成功」不等于「首个请求能跑通」，
	// 而后者才是接缝真正依赖的东西。
	if err := o.waitReady(ctx, inst.port, c.HealthPath, nativeReadyTimeout, inst, lg); err != nil {
		_ = nh.Close()
		o.mu.Lock()
		inst.state, inst.lastErr = StateFailed, err.Error()
		o.mu.Unlock()
		return err
	}

	o.mu.Lock()
	inst.state = StateRunning
	o.mu.Unlock()
	return nil
}
