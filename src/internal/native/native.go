// Package native 让上游的 HTTP 服务跑在 ModelMux 自己的进程里。
//
// 与「托管型」的区别只有一个：**谁提供服务**。托管型是「ModelMux 拉起一个独立
// 子进程，再用 HTTP 打它」；原生型是「ModelMux 自己装配上游的 http.Handler，
// 在本进程内起一个只绑回环地址的服务，再用 HTTP 打它」。
//
// ── 为什么仍然走 HTTP，而不是把上游拆成函数直接调用 ──────────────
//
// 上游是一整套 `http.ServeMux` 应用：路由表、鉴权中间件、SSE 流式写出、管理面板。
// 把它拆成函数调用等于**重写上游**；而选择「fork + 照搬上游源码」的全部收益
// ——能 `git merge` 上游的 bug 修复、能对照上游排查问题——都建立在
// **上游代码逐字不动** 之上。回环 HTTP 把这两件事同时保住：
//
//	上游 handler 一行不改，四条接缝（前端代理 / 控制台代理 / 领取执行器 / 数据面）
//	一行不改，只是「地址指向哪里」变了。
//
// 这也是本类型的核心价值：**接缝架构的可插拔性由此得到验证**。
//
// ── 为什么绑 127.0.0.1:0 ────────────────────────────────────
//
//   - `:0` 由内核分配空闲端口，**不进 ports.json 的端口表**。端口表是给「用户可能
//     手工填进别处的外链地址」用的，原生实例没有这个需求，也就不该占用它。
//   - 只绑回环，不对外暴露；与用户渠道端口不可能冲突（内核保证瞬时唯一）。
//   - 用真实 net 栈 ⇒ 超时、连接池、SSE flush、Header 语义与「对着子进程」完全一致，
//     回归风险最低。换内存管道虽然省一个 socket，但那是另一套语义，试验田阶段
//     不值得为此引入不确定性。
//
// ── 与托管型的共同点（决定了接缝为什么可以不改）────────────────
//
// 两者对外都表现为「一个 RootURL + 一个就绪状态」：代理、领取、数据面拿到的
// 都是 `http://127.0.0.1:<port>`，差异在编排层（进程 vs 本进程）被吸收干净。
package native

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"modelmux/internal/config"
	"modelmux/internal/logging"
)

// Service 一个进程内原生服务的装配结果。
//
// Handler 是上游应用的主路由（含它自己的 /v1/* 与 /panel/* 子树）；
// Close 负责按序收尾（停排程 → 落盘 → 关连接），必须幂等。
type Service struct {
	Handler http.Handler
	Close   func() error
}

// Boot 装配一个原生服务。
//
// cfg 是该渠道在 ModelMux 配置里的条目（原生实现可以从它的 Env / Route 里取
// 用户覆盖项）；dataDir 是该实例的独立数据目录，由编排器保证存在且可写——
// 账号、状态文件、用量记录都落在它下面，与其它实例互不干扰。
type Boot func(cfg config.ManagedProvider, dataDir string, lg *logging.Logger) (*Service, error)

// ErrNoBuilder 该原生标识没有对应实现（配置写错，或被降级的旧配置）。
var ErrNoBuilder = errors.New("未注册的原生实现")

// Runtime 一个只绑回环地址的进程内 HTTP 服务。
//
// 编排器持有它，并把它暴露成一个与子进程无异的「端口 + 就绪状态」。
type Runtime struct {
	name string
	srv  *http.Server
	ln   net.Listener
	root string
	lg   *logging.Logger

	once sync.Once
	done chan struct{}
}

// Serve 在回环上启动一个 HTTP 服务并立即返回（不等首个请求）。
//
// 返回后 RootURL() 即刻可用——`net.Listen` 成功时内核已把端口绑好，
// 不需要像子进程那样轮询等端口打开。
func Serve(name string, h http.Handler, lg *logging.Logger) (*Runtime, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		_ = ln.Close()
		return nil, errors.New("回环监听地址不是 TCP")
	}
	srv := &http.Server{
		Handler: h,
		// 与上游自带服务同口径的保守上限：原生服务只服务本进程与面板代理，
		// 没有慢速公网客户端的场景，给个防呆值即可。
		ReadHeaderTimeout: 30 * time.Second,
		// 刻意不设 ReadTimeout / WriteTimeout：上游的聊天接口是长流式（SSE），
		// 全局 WriteTimeout 会把在途流式响应掐断。上游自己的 main 也是这么做的。
		IdleTimeout: 120 * time.Second,
	}
	rt := &Runtime{
		name: name,
		srv:  srv,
		ln:   ln,
		root: "http://" + net.JoinHostPort("127.0.0.1", itoa(addr.Port)),
		lg:   lg,
		done: make(chan struct{}),
	}
	go func() {
		defer close(rt.done)
		// ErrServerClosed 是 Close 的正常结果，不算错误。
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			lg.Error("进程内原生服务异常退出", "provider", name, "err", err.Error())
		}
	}()
	lg.Info("进程内原生服务已就绪", "provider", name, "root", rt.root, "proto", "in-process")
	return rt, nil
}

// RootURL 该服务的根地址（不含 API 前缀），如 http://127.0.0.1:54321。
func (r *Runtime) RootURL() string { return r.root }

// Port 内核分配到的端口。
func (r *Runtime) Port() int {
	if ta, ok := r.ln.Addr().(*net.TCPAddr); ok {
		return ta.Port
	}
	return 0
}

// Close 优雅停机：先停止接受新连接并等在途请求结束，再关监听。
//
// 超时后不等：宁可硬断也不能卡住用户的「重启渠道 / 退出」。
// 幂等——编排器的 Stop 与 Shutdown 都可能在同一次生命周期里调到它。
func (r *Runtime) Close(ctx context.Context) error {
	var err error
	r.once.Do(func() {
		_ = r.srv.Shutdown(ctx)
		err = r.ln.Close()
	})
	return err
}

// Wait 等服务的 Serve 循环真正退出（Close 之后用）。
//
// 编排器需要它：重启渠道时必须确认旧监听已完全释放，否则「关掉再开」在
// 日志上看着成功、实际仍有半开的旧实例响应，排查时极难定位。
func (r *Runtime) Wait(ctx context.Context) error {
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// itoa 只服务于端口拼接，避免为一个 strconv.Itoa 引入 import。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
