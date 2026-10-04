// orchestrator.go 「被托管 provider」的编排：把一份配置变成一个可用的本机端点。
//
// 职责：产出**一个 RootURL + 一个就绪状态**，并保证退出时回收干净。
//
// 托管型上游（workbuddy / zcode / trae）**只有一种运行方式：进程内原生**——
// 由 Mergence 自己装配上游的 http.Handler，在本进程内起一个只绑回环的服务，
// 再用 HTTP 打它（见 internal/native 与 native.go）。独立子进程模式（exec +
// 动态端口 + 端口表）已整块移除：账号落在上游自己的目录、随包分发第三方二进制、
// 进程树回收与端口持久化这些麻烦，都随「内嵌上游」一起消失了。
//
// 「托管型」指那些不是可 import 的库、只能当服务来用的上游。它们对外都只是
// 一个 OpenAI 兼容的 HTTP 端点 + 一套管理 API，Mergence 只需要知道「它在哪」。
package orchestrator

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"mergence/internal/config"
	"mergence/internal/logging"
)

// State 实例状态。
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateFailed   State = "failed"
)

// Instance 一个被托管 provider 的运行时状态。
type Instance struct {
	cfg       config.ManagedProvider
	state     State
	port      int
	lastErr   string
	startedAt time.Time
	// native 非空表示这个实例正在运行（进程内原生服务句柄）。
	native *nativeHandle

	// exited 在实例被回收后关闭。Stop/Restart 要等它：上游收尾要落盘/关库，
	// 不等就重启会读到半截状态文件（见 native.go 的 Close 注释）。
	exited chan struct{}
}

// Status 对外暴露的只读状态。
type Status struct {
	Name        string    `json:"name"`
	DisplayName string    `json:"display_name"`
	State       State     `json:"state"`
	Port        int       `json:"port"`
	BaseURL     string    `json:"base_url"`
	LastErr     string    `json:"last_err,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
}

// Orchestrator 编排器。
type Orchestrator struct {
	log  *logging.Logger
	home string

	mu  sync.Mutex
	ins map[string]*Instance
}

// New 创建编排器。home 为数据根目录。
func New(home string, lg *logging.Logger) *Orchestrator {
	return &Orchestrator{log: lg, home: home, ins: map[string]*Instance{}}
}

// StartAll 按配置顺序启动所有启用的托管 provider。
// 单个失败不阻断其他（多渠道的意义就是互为备份）。
func (o *Orchestrator) StartAll(ctx context.Context, cfgs []config.ManagedProvider) {
	for _, c := range cfgs {
		if !c.Enabled {
			o.log.Info("跳过已禁用的托管 provider", "provider", c.Name)
			continue
		}
		if err := o.Start(ctx, c); err != nil {
			o.log.Error("托管 provider 启动失败", "provider", c.Name, "err", err.Error())
		}
	}
}

// Start 启动单个托管 provider，并等待其就绪。
func (o *Orchestrator) Start(ctx context.Context, c config.ManagedProvider) error {
	lg := o.log.WithProvider(c.Name)

	o.mu.Lock()
	if old, ok := o.ins[c.Name]; ok && old.state == StateRunning {
		o.mu.Unlock()
		return fmt.Errorf("provider %s 已在运行", c.Name)
	}
	inst := &Instance{cfg: c, state: StateStarting}
	o.ins[c.Name] = inst
	o.mu.Unlock()

	fail := func(msg string, err error) error {
		o.mu.Lock()
		inst.state = StateFailed
		inst.lastErr = msg
		o.mu.Unlock()
		lg.Error("启动失败", "stage", msg, "err", errString(err))
		return fmt.Errorf("%s：%s", msg, errString(err))
	}

	// 数据目录（每实例独立，避免多实例互踩数据库/状态文件）。
	// 留空即 运行根/data/instances/<渠道名>/data —— 账号、状态、用量都落在这里。
	dataDir := c.DataDir
	if dataDir == "" {
		dataDir = config.DataPath(o.home, "instances", c.Name, "data")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fail("创建数据目录失败", err)
	}

	// 进程内装配 + 回环监听，等它就绪（见 native.go）
	return o.startNative(ctx, c, inst, dataDir, fail)
}

// Restart 重启单个 provider：先停干净（等上游真的收完），再重新装配。
func (o *Orchestrator) Restart(ctx context.Context, c config.ManagedProvider) error {
	if err := o.Stop(c.Name); err != nil {
		o.log.WithProvider(c.Name).Warn("重启前停止失败，继续尝试启动", "err", err.Error())
	}
	return o.Start(ctx, c)
}

// waitReady 轮询健康检查直到就绪、超时，或实例提前退出。
//
// 原生实例同样真的发一次 HTTP 探活：监听在内核里是同步完成的，但
// 「handler 构造成功」不等于「首个请求能跑通」，而后者才是接缝依赖的东西。
func (o *Orchestrator) waitReady(ctx context.Context, port int, path string,
	timeout time.Duration, inst *Instance, lg *logging.Logger) error {

	if path == "" {
		path = "/healthz"
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	start := time.Now()

	for time.Now().Before(deadline) {
		select {
		case <-inst.exited:
			// 实例提前退出——比干等超时有用得多的信息。
			// 退出原因在句柄里（close(done) 之前写入，读它无竞态）。
			msg := "无错误信息"
			if inst.native != nil && inst.native.exitErr != nil {
				msg = inst.native.exitErr.Error()
			}
			return fmt.Errorf("实例在就绪前自行退出（端口 %d）：%s", port, msg)
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		resp, err := client.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			// 502 / 504 是**反向代理自己**在「够不到上游」时回的（见
			// native/external.go 的 ErrorHandler），不代表上游起来了：接管一个
			// 打不通的外部地址时必须继续等、最后报超时，而不是当场判成就绪。
			// 其它 HTTP 响应才算就绪——上游可能用 503 表达「起来了但还没数据」。
			if resp.StatusCode != http.StatusBadGateway &&
				resp.StatusCode != http.StatusGatewayTimeout {
				lg.Info("托管 provider 已就绪",
					"port", port, "http_status", resp.StatusCode,
					"dur_ms", time.Since(start).Milliseconds())
				return nil
			}
		}
		time.Sleep(400 * time.Millisecond)
	}
	return fmt.Errorf("等待就绪超时（%s，探测 %s）——请查看该 provider 的日志", timeout, url)
}

// Stop 停止单个 provider。
//
// 停止即优雅收尾：停监听 → drain 在途请求 → 上游落盘/关库。它本来就在本进程里，
// 没有进程可杀，所以不做任何 kill。
func (o *Orchestrator) Stop(name string) error {
	o.mu.Lock()
	inst, ok := o.ins[name]
	o.mu.Unlock()
	if !ok || inst.native == nil {
		return nil
	}
	lg := o.log.WithProvider(name)

	err := inst.native.Close()

	o.mu.Lock()
	inst.state = StateStopped
	exited := inst.exited
	o.mu.Unlock()
	if err != nil {
		lg.Warn("停止 provider 失败", "err", err.Error())
		return err
	}

	// 等它真的收完再返回：上游收尾期间要落盘/关库，不等就重启会读到半截状态文件。
	if exited != nil {
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			lg.Warn("实例在 5s 内未回收，继续")
		}
	}
	lg.Info("托管 provider 已停止")
	return nil
}

// Shutdown 停止全部 provider，并等待它们真的收完。
//
// grace 用于给上游一点收尾时间，超时也不阻塞整体退出——宁可硬断也不能卡住
// 用户的「退出」按钮。
func (o *Orchestrator) Shutdown(ctx context.Context, grace time.Duration) {
	o.mu.Lock()
	names := make([]string, 0, len(o.ins))
	for n := range o.ins {
		names = append(names, n)
	}
	o.mu.Unlock()
	sort.Strings(names)

	var wg sync.WaitGroup
	for _, n := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			_ = o.Stop(name)
		}(n)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
		o.log.Warn("部分托管 provider 未在宽限期内退出，继续退出流程",
			"grace", grace.String())
	case <-ctx.Done():
	}
}

// List 返回全部实例状态（按名称排序，便于面板稳定展示）。
func (o *Orchestrator) List() []Status {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Status, 0, len(o.ins))
	for _, inst := range o.ins {
		out = append(out, o.statusOf(inst))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (o *Orchestrator) statusOf(inst *Instance) Status {
	s := Status{
		Name:        inst.cfg.Name,
		DisplayName: inst.cfg.DisplayName,
		State:       inst.state,
		Port:        inst.port,
		LastErr:     inst.lastErr,
		StartedAt:   inst.startedAt,
	}
	if inst.port > 0 {
		s.BaseURL = fmt.Sprintf("http://127.0.0.1:%d", inst.port)
	}
	return s
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
