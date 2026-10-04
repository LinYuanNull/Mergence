// orchestrator.go 「被托管 provider」的编排：把一份配置变成一个可用的本机端点。
//
// 职责：产出**一个 RootURL + 一个就绪状态**，并保证退出时回收干净。
// 两种形态在这里统一（见 Instance）：独立子进程（exec + 动态端口）与
// 进程内原生（装配上游 handler + 回环监听，见 native.go）。
//
// 「托管型」指那些不是可 import 的库、只能当服务来用的上游（new-api、
// zcode2api、workbuddy2api、trae2api-web 等）。它们对外都只是一个
// OpenAI 兼容的 HTTP 端点 + 一套管理 API，Mergence 只需要知道「它在哪」。
package orchestrator

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
//
// 「被托管」含两种形态，互斥地占用 cmd（子进程）或 native（进程内）之一：
// 其余字段（state / port / startedAt / exited / lastErr）对两者是同一套语义，
// 状态机与就绪判定也共用——差异只在 native.go 那三处（见该文件注释）。
type Instance struct {
	cfg       config.ManagedProvider
	state     State
	port      int
	pid       int
	lastErr   string
	startedAt time.Time
	cmd       *exec.Cmd
	// native 非空表示这是进程内原生实例（此时 cmd 恒为 nil）。
	native *nativeHandle
	cancel context.CancelFunc

	// exited 在进程被回收后关闭；exitErr 是它的退出错误。
	// Stop/Restart 要等这个信号：Windows 上 TerminateProcess 返回后端口不会立刻释放，
	// 紧接着重启到同一端口会 bind 失败，表现为「重启后起不来」。
	exited  chan struct{}
	exitErr error
}

// Status 对外暴露的只读状态。
type Status struct {
	Name        string    `json:"name"`
	DisplayName string    `json:"display_name"`
	State       State     `json:"state"`
	Port        int       `json:"port"`
	BaseURL     string    `json:"base_url"`
	PID         int       `json:"pid"`
	LastErr     string    `json:"last_err,omitempty"`
	StartedAt   time.Time `json:"started_at,omitempty"`
	FixedPort   bool      `json:"fixed_port"`
}

// Orchestrator 编排器。
type Orchestrator struct {
	log   *logging.Logger
	alloc *PortAllocator
	home  string

	mu  sync.Mutex
	ins map[string]*Instance
}

// New 创建编排器。home 为数据根目录。
func New(home string, lg *logging.Logger, alloc *PortAllocator) *Orchestrator {
	return &Orchestrator{log: lg, alloc: alloc, home: home, ins: map[string]*Instance{}}
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

	// 1) 数据目录（每实例独立，避免多实例互踩数据库/状态文件）
	//
	// 刻意排在端口之前：两条路径都要它，而原生路径**根本不需要端口**
	// （内核分配、不进端口表），把端口分配提前会让原生实例白占一个端口号。
	dataDir := c.DataDir
	if dataDir == "" {
		dataDir = config.DataPath(o.home, "instances", c.Name, "data")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fail("创建数据目录失败", err)
	}

	// 2) 原生型走进程内装配，与子进程路径在此分道（见 native.go）
	if c.Native() {
		return o.startNative(ctx, c, inst, dataDir, fail)
	}

	// 3) 端口
	port, err := o.alloc.Allocate(c.Name, c.FixedPort)
	if err != nil {
		return fail("端口分配失败", err)
	}
	inst.port = port

	// 4) 命令行
	exe := c.Command
	if exe == "" {
		return fail("缺少 command", fmt.Errorf("managed_providers[%s].command 为空", c.Name))
	}
	if !filepath.IsAbs(exe) && c.Dir != "" {
		exe = filepath.Join(c.Dir, exe)
	}
	args := make([]string, len(c.Args))
	copy(args, c.Args)
	// 把 {port} / {data} 占位符展开，方便配置里直接引用动态值。
	for i, a := range args {
		a = strings.ReplaceAll(a, "{port}", strconv.Itoa(port))
		a = strings.ReplaceAll(a, "{data}", dataDir)
		args[i] = a
	}

	cmd := exec.Command(exe, args...)
	if c.Dir != "" {
		cmd.Dir = c.Dir
	}
	cmd.Env = buildEnv(c, port, dataDir)
	// 隐藏窗口：托管 provider 不该闪出黑框
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	// 子进程日志归一：stdout/stderr 全部过 ProviderWriter，每行强制带 provider 标签
	w := o.log.ProviderWriter(c.Name)
	cmd.Stdout = w
	cmd.Stderr = w

	// 5) 拉起
	if err := cmd.Start(); err != nil {
		return fail("启动进程失败", err)
	}
	inst.pid = cmd.Process.Pid
	inst.startedAt = time.Now()

	// 唯一的回收协程：负责 Wait、记录退出错误、关闭 exited、更新状态。
	// 刻意只留一个 —— 两个 goroutine 读同一个 channel，谁先读到是竞态。
	inst.exited = make(chan struct{})
	go func() {
		werr := cmd.Wait()
		o.mu.Lock()
		inst.exitErr = werr
		running := inst.state == StateRunning
		if running {
			inst.state = StateStopped
		}
		o.mu.Unlock()
		close(inst.exited)

		if werr != nil {
			// 不自动重启：由面板提示，避免上游一直崩时变成无限重启循环
			lg.Warn("托管 provider 已退出", "err", werr.Error(), "pid", inst.pid)
		} else if running {
			lg.Info("托管 provider 正常退出", "pid", inst.pid)
		}
	}()

	lg.Info("托管 provider 已拉起",
		"port", port, "pid", inst.pid, "exe", exe,
		"data_dir", dataDir, "mode", portMode(c))

	// 6) 等就绪
	readyTimeout := durationOr(c.ReadyTimeout, 40*time.Second)
	if err := o.waitReady(ctx, port, c.HealthPath, readyTimeout, inst, lg); err != nil {
		_ = killTree(cmd)
		o.mu.Lock()
		inst.state, inst.lastErr = StateFailed, err.Error()
		o.mu.Unlock()
		return err
	}

	o.mu.Lock()
	inst.state, inst.cmd = StateRunning, cmd
	o.mu.Unlock()

	return nil
}

// Restart 重启单个 provider：先停干净（等进程真的消失），再重新拉起。
func (o *Orchestrator) Restart(ctx context.Context, c config.ManagedProvider) error {
	if err := o.Stop(c.Name); err != nil {
		o.log.WithProvider(c.Name).Warn("重启前停止失败，继续尝试启动", "err", err.Error())
	}
	return o.Start(ctx, c)
}

// buildEnv 组装子进程环境变量：继承 + 用户自定义 + 端口 + 数据目录。
//
// 端口走双通道下发：MERGENCE_PORT 是我们的约定，PortEnvVar 是该 provider 原生的
// 变量名（如 new-api 用 PORT）——两者都注入，兼容不同 provider 的配置习惯。
func buildEnv(c config.ManagedProvider, port int, dataDir string) []string {
	env := os.Environ()
	env = append(env,
		"MERGENCE_PORT="+strconv.Itoa(port),
		"MERGENCE_DATA_DIR="+dataDir,
	)
	if c.PortEnvVar != "" {
		env = append(env, c.PortEnvVar+"="+strconv.Itoa(port))
	}
	for k, v := range c.Env {
		v = strings.ReplaceAll(v, "{port}", strconv.Itoa(port))
		v = strings.ReplaceAll(v, "{data}", dataDir)
		env = append(env, k+"="+v)
	}
	return env
}

// waitReady 轮询健康检查直到就绪、超时，或进程提前退出。
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
			// 进程提前退出——比干等超时有用得多的信息
			o.mu.Lock()
			werr := inst.exitErr
			o.mu.Unlock()
			if werr == nil {
				return fmt.Errorf("实例在就绪前自行退出（端口 %d，退出码 0）", port)
			}
			return fmt.Errorf("实例在就绪前退出：%s", werr.Error())
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		resp, err := client.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			// 任意 HTTP 响应都算就绪：上游可能用 503 表达「起来了但还没数据」
			lg.Info("托管 provider 已就绪",
				"port", port, "http_status", resp.StatusCode,
				"dur_ms", time.Since(start).Milliseconds())
			return nil
		}
		time.Sleep(400 * time.Millisecond)
	}
	return fmt.Errorf("等待就绪超时（%s，探测 %s）——请查看该 provider 的日志", timeout, url)
}

// Stop 停止单个 provider（子进程或原生实例）。
func (o *Orchestrator) Stop(name string) error {
	o.mu.Lock()
	inst, ok := o.ins[name]
	o.mu.Unlock()
	if !ok {
		return nil
	}
	// 两种形态都还没有可停的东西：子进程要有 cmd，原生要有 native。
	// （原生实例的 cmd 恒为 nil，所以判据不能只看 cmd——否则原生永远停不掉。）
	if inst.native == nil && (inst.cmd == nil || inst.cmd.Process == nil) {
		return nil
	}
	lg := o.log.WithProvider(name)

	var err error
	if inst.native != nil {
		// 优雅收尾：停监听 → drain 在途请求 → 上游落盘/关库。
		// 它本来就在本进程里，没有进程可杀，所以这里不做 killTree。
		err = inst.native.Close()
	} else {
		err = killTree(inst.cmd)
	}

	o.mu.Lock()
	inst.state = StateStopped
	exited := inst.exited
	o.mu.Unlock()
	if err != nil {
		lg.Warn("停止 provider 失败", "err", err.Error())
		return err
	}

	// 等它真的消失再返回。TerminateProcess 是异步的，不等就重启会出现
	// 「端口仍被占用 / 数据文件仍被锁」这类莫名其妙的启动失败；
	// 原生实例则要等上游落盘写完，否则紧随的重启会读到半截状态文件。
	if exited != nil {
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			lg.Warn("实例在 5s 内未回收，继续", "pid", inst.pid)
		}
	}
	lg.Info("托管 provider 已停止", "pid", inst.pid)
	return nil
}

// Shutdown 停止全部 provider，并等待进程真的消失。
//
// Windows 上没有可靠的「优雅终止信号」可用（非控制台进程收不到 SIGTERM/Ctrl+C），
// 所以这里是 Kill（即 TerminateProcess）后等待退出。grace 用于给进程一点收尾时间，
// 超时也不阻塞整体退出——宁可硬杀也不能卡住用户的「退出」按钮。
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
		PID:         inst.pid,
		LastErr:     inst.lastErr,
		StartedAt:   inst.startedAt,
		FixedPort:   inst.cfg.FixedPort > 0,
	}
	if inst.port > 0 {
		s.BaseURL = fmt.Sprintf("http://127.0.0.1:%d", inst.port)
	}
	return s
}

// killTree 结束进程**及其整棵进程树**。
//
// 只用 Process.Kill（TerminateProcess）会漏掉子进程：Node / Python 写的 provider
// 常会再拉起自己的子进程，只杀根进程会留下孤儿继续占着端口，下次重启就 bind 失败。
// 这里优先用 taskkill /T 杀树，它不可用时退回单进程 Kill —— 退路比不杀好。
func killTree(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	tk := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid))
	tk.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if err := tk.Run(); err == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func portMode(c config.ManagedProvider) string {
	if c.FixedPort > 0 {
		return "fixed(降级)"
	}
	return "dynamic"
}

func durationOr(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(s)); err == nil && d > 0 {
		return d
	}
	return def
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}
