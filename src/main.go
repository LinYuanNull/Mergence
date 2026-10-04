// main.go ModelMux 入口。
//
// P0（骨架：配置 / 结构化日志 / 动态端口 / 进程编排）
// P1（托盘与窗口生命周期：关窗驻留托盘，仅托盘「退出」结束进程）
// P3（内嵌型渠道：自定义 OpenAI 兼容端点 + 协议适配 + 对外出口）
//
// 两种运行形态：
//
//	默认        WebView2 桌面壳 + 托盘驻留（面向人）
//	无界面模式   只跑内置服务，不建窗口不建托盘（-headless / MODELMUX_HEADLESS=1）
//
// 无界面模式不是为了省事——它让「服务本身能不能用」与「窗口能不能开」解耦。
// 没有它，任何一次端到端验证都必须弹出真实窗口，CI 与自动化测试无从下手。
//
// 退出时序严格有序，见 runExitSequence —— 漏任何一步都会留下残留
// （幽灵托盘图标 / 孤儿进程占端口 / 状态未落盘）。
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"modelmux/internal/config"
	"modelmux/internal/desktop"
	"modelmux/internal/logging"
	"modelmux/internal/orchestrator"
	"modelmux/internal/provider"
	"modelmux/internal/web"

	// 原生实现注册：空白导入即把 kind=workbuddy 的实现装进 internal/native 的
	// 注册表，编排器在 mode=native 的渠道上按 kind 取到它。
	//
	// 为什么放在 main 而不是 orchestrator 里：编排器只需要知道「有个 kind 要装配」，
	// 不该依赖具体上游（它要能同时装 workbuddy / zcode / trae，而这些只有 main
	// 知道这一版带了哪些）。装配点集中在这里，加一个原生实现就是加一行导入。
	_ "modelmux/internal/native/trae"
	_ "modelmux/internal/native/workbuddy"
	_ "modelmux/internal/native/zcode"
)

const appTitle = "ModelMux"

// 期望的逻辑窗口尺寸（96 DPI）；实际物理尺寸按所在显示器 DPI 换算。
const (
	wantWinW = 1200
	wantWinH = 760
)

func main() {
	headless := isHeadless()

	// ── 0) 根目录（配置在 <root>/config/，运行期数据在 <root>/data/）
	home := config.DataDir()
	if err := os.MkdirAll(home, 0o755); err != nil {
		abort(headless, nil, "无法创建根目录", err)
		return
	}

	// ── 1) 配置
	cfgPath := config.ConfigPath(home)
	cfg, warns, err := config.Load(cfgPath)
	if err != nil {
		abort(headless, nil, "读取配置失败", err)
		return
	}

	// ── 2) 日志（先于一切，后面所有异常都要能留下痕迹）
	logDir := cfg.Log.Dir
	if logDir == "" {
		logDir = config.DataPath(home, "logs")
	}
	lg, err := logging.New(cfg.Log.Level, logDir, cfg.Log.RingSize, cfg.Log.MaxFileMB)
	if err != nil {
		abort(headless, nil, "日志初始化失败", err)
		return
	}
	lg.Info("ModelMux 启动", "home", home, "config", cfgPath, "log_dir", logDir, "headless", headless)
	for _, w := range warns {
		lg.Warn("配置提示", "detail", w)
	}

	// ── 3) 单实例：只用于「已存在则唤出」，绝不据此拒绝启动。
	// 无界面模式跳过：它与桌面实例是两种运行形态，互斥反而会挡住自动化场景。
	if cfg.Tray.SingleInstance && !headless {
		first, release := desktop.AcquireSingleInstance(appTitle)
		if !first {
			lg.Info("已有实例在运行，已唤出其窗口；本进程退出")
			_ = lg.Close()
			return
		}
		defer release()
	}

	// ── 4) 动态端口 + 托管型 provider 编排
	alloc, err := orchestrator.NewPortAllocator(
		config.DataPath(home, "ports.json"), cfg.Ports.Reuse, cfg.Ports.MaxRetry)
	if err != nil {
		abort(headless, lg, "端口分配器初始化失败", err)
		return
	}
	orch := orchestrator.New(home, lg, alloc)
	orch.StartAll(context.Background(), cfg.Managed)
	_ = alloc.Save()

	// ── 5) 渠道注册表
	// 上游列表由 web 层按「配置 + 子进程实时状态」组装（见 web/upstreams.go），
	// 并在 Server.Start 里完成首次同步；这里只建一个空表。
	reg := provider.NewRegistry(lg)

	// ── 6) 内置 Web 服务（面板 + 对外 OpenAI 兼容出口 + dsh 插件数据源）
	srv := web.New(lg, orch, reg, home)
	srv.SetConfig(cfg, cfgPath)
	if _, err := srv.Start(); err != nil {
		abort(headless, lg, "内置服务启动失败", err)
		return
	}
	baseURL := srv.BaseURL()
	reg.PrefetchModels() // 后台拉模型目录，不阻塞启动

	// ── 7) 无界面模式：不建窗口、不建托盘，等退出信号
	if headless {
		quit := make(chan struct{})
		var once sync.Once
		srv.OnQuit = func() { once.Do(func() { close(quit) }) }
		lg.Info("已进入无界面模式", "panel", baseURL+"/", "api", baseURL+"/v1")
		<-quit
		shutdownServices(lg, orch, alloc, logDir, false)
		return
	}

	// ── 8) WebView2 壳
	desktop.EnsureBrowserArgs(lg)
	shell, err := desktop.NewShell(lg, appTitle, desktop.WebviewDataPath(home), wantWinW, wantWinH)
	if err != nil {
		lg.Error("创建窗口失败", "err", err.Error())
		orch.Shutdown(context.Background(), 8*time.Second)
		_ = lg.Close()
		desktop.MessageBox(appTitle,
			err.Error()+"\n\n详细日志：\n"+filepath.Join(logDir, "modelmux.log"), true)
		return
	}

	// 桌面桥：把「唤出窗口 / 打开外部链接」注入面板（须在 Navigate 之前）
	shell.BindNative()

	// ── 9) 生命周期 + 托盘
	lc := desktop.NewLifecycle(lg, shell.View(), cfg.Tray.MinimizeToTray)

	// baseURL 动态取：端口热切换后托盘的「打开面板 / 复制地址」必须跟新，
	// 捕获值拷贝会让托盘指向一个已经没人监听的旧端口。
	tray := desktop.NewTray(lg, func(cmd int) {
		// 托盘回调跑在托盘线程，所有窗口操作必须投回 UI 线程
		shell.Dispatch(func() { handleTrayCommand(cmd, lc, srv.BaseURL, home, lg) })
	})
	lc.SetTray(tray)

	if cfg.Tray.MinimizeToTray {
		if err := tray.Start(); err != nil {
			// 托盘起不来就不能拦关闭 —— 否则用户关窗后程序失联、只能去任务管理器杀
			lg.Warn("托盘初始化失败，本次将「关窗即退出」", "err", err.Error())
			lc.DisableMinimizeToTray()
		} else {
			lg.Info("托盘已启动：关窗将隐藏到托盘")
		}
	}
	lc.Install()

	// 退出入口统一：托盘「退出」与面板「退出」走同一条有序退出路径
	var quitOnce sync.Once
	quit := func() { quitOnce.Do(func() { shell.Dispatch(lc.Exit) }) }
	srv.OnQuit = quit

	// 关窗行为（最小化到托盘 / 直接退出）可在面板里热改，落盘由 web 层做。
	// 这里只负责把新行为推给窗口过程——它读的是 UI 线程上的状态，
	// 必须 Dispatch 回去，不能在 HTTP goroutine 里直接调。
	srv.OnCloseBehaviorChange = func(on bool) {
		shell.Dispatch(func() { lc.SetMinimizeToTray(on) })
	}

	// ── 10) 打开面板并进入消息循环
	shell.Navigate(baseURL + "/")
	shell.Run()

	// ── 11) 有序退出（含释放 WebView2）
	shell.Destroy()
	lg.Info("WebView2 已释放")
	shutdownServices(lg, orch, alloc, logDir, true)
}

// isHeadless 判断是否以无界面模式运行。
func isHeadless() bool {
	if v := strings.TrimSpace(os.Getenv("MODELMUX_HEADLESS")); v == "1" || strings.EqualFold(v, "true") {
		return true
	}
	for _, a := range os.Args[1:] {
		if a == "-headless" || a == "--headless" {
			return true
		}
	}
	return false
}

// handleTrayCommand 处理托盘菜单命令。运行在 UI 线程上。
//
// getBase 是动态取值函数而不是字符串：端口可在运行中热切换，
// 捕获字符串会让托盘指向一个已经没人监听的旧端口。
func handleTrayCommand(cmd int, lc *desktop.Lifecycle, getBase func() string, home string, lg *logging.Logger) {
	baseURL := getBase()
	switch cmd {
	case desktop.MenuShowWindow, desktop.MenuViewLogs:
		lc.ShowWindow()
	case desktop.MenuOpenPanel:
		if !desktop.OpenURL(baseURL) {
			lg.Warn("打开浏览器失败", "url", baseURL)
		}
	case desktop.MenuCopyURL:
		if desktop.SetClipboardText(baseURL) {
			lg.Info("已复制 API 地址到剪贴板", "url", baseURL)
		} else {
			lg.Warn("写入剪贴板失败")
		}
	case desktop.MenuOpenData:
		if !desktop.OpenPath(home) {
			lg.Warn("打开数据目录失败", "path", home)
		}
	case desktop.MenuQuit:
		lc.Exit()
	}
}

// shutdownServices 回收托管进程、落盘端口映射、收尾日志。
//
// withWindow=false 表示 WebView2 尚未创建（无界面模式），跳过其释放步骤。
func shutdownServices(lg *logging.Logger, orch *orchestrator.Orchestrator,
	alloc *orchestrator.PortAllocator, logDir string, withWindow bool) {

	// 回收全部托管型 provider —— 先礼后兵，超时强杀，绝不留孤儿进程占端口
	start := time.Now()
	orch.Shutdown(context.Background(), 8*time.Second)
	lg.Info("托管型 provider 已全部回收", "dur_ms", time.Since(start).Milliseconds())

	// 状态落盘（端口映射；provider 自身状态由各自的 Flush 负责）
	if err := alloc.Save(); err != nil {
		lg.Warn("端口映射落盘失败", "err", err.Error())
	}

	lg.Info("ModelMux 已退出", "log_dir", logDir, "with_window", withWindow)
	_ = lg.Close()
}

// abort 统一的启动期失败出口。
//
// 桌面形态用弹窗（用户双击时看不到 stderr），无界面形态打到 stderr（CI 能收集到）。
func abort(headless bool, lg *logging.Logger, msg string, err error) {
	if lg != nil {
		lg.Error(msg, "err", err.Error())
	}
	if headless {
		fmt.Fprintf(os.Stderr, "%s: %s\n", msg, err.Error())
		return
	}
	desktop.MessageBox(appTitle, msg+"：\n"+err.Error(), true)
}
