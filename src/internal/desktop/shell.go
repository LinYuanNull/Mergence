// shell.go WebView2 桌面壳。
//
// 这部分是从已跑通的 workbuddy-desktop 迁移并重组的，几个关键点保留原样：
//   - 必须注入 WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS，否则受限环境下内容区全白
//   - 必须以**显示器真实 DPI** 换算窗口物理尺寸，系统 DPI 与显示器 DPI 可能不一致
//   - 图标优先从 PE 资源取，不依赖外部 app.ico 文件
package desktop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"

	"mergence/internal/config"
	"mergence/internal/logging"
)

// defaultBrowserArgs WebView2 兜底 Chromium 参数。
//
// 在无独显 / 虚拟显示 / 受限会话的机器上，WebView2 的 GPU 与沙箱子进程会反复崩溃
// （表现为「窗口能弹出但内容区全白」，且不报任何错），实测必须加 --no-sandbox。
// 本应用只加载本机 127.0.0.1 的面板，风险可控。
const defaultBrowserArgs = "--no-sandbox --disable-gpu"

// Shell WebView2 窗口。
//
// 不存窗口标题：标题只在创建 webview2 时用一次（见 NewShell 的 Options），
// 之后没有任何读取方，留字段就是留死状态。
type Shell struct {
	lg   *logging.Logger
	view webview2.WebView
}

// EnsureBrowserArgs 在用户未显式设置时注入兜底参数。
//
// 必须在创建 WebView2 环境**之前**调用——loader 在环境创建时读取该变量。
func EnsureBrowserArgs(lg *logging.Logger) {
	if strings.TrimSpace(os.Getenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS")) != "" {
		lg.Info("沿用用户自定义的 WebView2 参数",
			"value", os.Getenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS"))
		return
	}
	_ = os.Setenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS", defaultBrowserArgs)
	lg.Info("已注入默认 WebView2 参数", "value", defaultBrowserArgs)
}

// NewShell 创建窗口。
//
// wantW/wantH 是**逻辑像素**（96 DPI 下的期望值）；实际物理尺寸在窗口创建后按显示器
// DPI 重算——go-webview2 的 WindowOptions 只接受物理像素。
func NewShell(lg *logging.Logger, title, dataPath string, wantW, wantH uint) (*Shell, error) {
	view := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		DataPath:  dataPath,
		WindowOptions: webview2.WindowOptions{
			Title:  title,
			Width:  wantW,
			Height: wantH,
			Center: true,
		},
	})
	if view == nil {
		return nil, fmt.Errorf("无法初始化 WebView2 窗口（请确认已安装 Microsoft Edge WebView2 运行时）")
	}
	s := &Shell{lg: lg, view: view}
	s.applyLayout(wantW, wantH)
	s.applyIcon()
	return s, nil
}

// View 暴露底层 webview，供生命周期管理使用。
func (s *Shell) View() webview2.WebView { return s.view }

// Navigate 打开面板。
func (s *Shell) Navigate(url string) {
	s.lg.Info("打开面板", "url", url)
	s.view.Navigate(url)
}

// Run 进入消息循环，直到窗口被真正关闭才返回。
func (s *Shell) Run() { s.view.Run() }

// Destroy 释放 WebView2 环境与用户数据目录句柄。
func (s *Shell) Destroy() { s.view.Destroy() }

// Dispatch 把函数投递到 UI 线程执行（跨线程操作窗口的唯一正规入口）。
func (s *Shell) Dispatch(f func()) { s.view.Dispatch(f) }

// BindNative 把原生能力注入面板的 JS 全局（桌面桥）。
//
// 参照 DSH 的 preload/contextBridge：渲染层不该自己做「网页做不到」的事
// （唤出被隐藏的窗口、用系统浏览器打开外部链接）。这里注入两个函数：
//
//	window.mmShowWindow()          唤出主窗口（关窗驻留托盘后从面板恢复）
//	window.mmOpenExternal(url)     用系统默认浏览器打开外部 URL
//
// 返回 Promise（WebView2 侧的约定）；失败只记日志不抛给渲染层——
// 桥接失败不该让面板功能不可用，退回网页行为即可。
func (s *Shell) BindNative() {
	if err := s.view.Bind("mmShowWindow", func() {
		s.view.Dispatch(func() { procShowWindow.Call(uintptr(s.view.Window()), swRestore) })
	}); err != nil {
		s.lg.Warn("注入 mmShowWindow 失败", "err", err.Error())
	}
	if err := s.view.Bind("mmOpenExternal", func(url string) {
		if url == "" {
			return
		}
		if !OpenURL(url) {
			s.lg.Warn("打开外部链接失败", "url", url)
		}
	}); err != nil {
		s.lg.Warn("注入 mmOpenExternal 失败", "err", err.Error())
	}
}

// applyLayout 按窗口所在显示器的 DPI 与工作区，把窗口调成合适的物理尺寸并居中。
//
// 为什么必须放在**窗口创建之后**：本机实测 GetDpiForSystem()=96 而
// GetDpiForWindow()=192（显示器 200% 缩放）。若直接写死物理像素，窗口只会占屏幕一半不到。
func (s *Shell) applyLayout(wantW, wantH uint) {
	hwnd := uintptr(s.view.Window())
	if hwnd == 0 {
		return
	}
	dpi, _, _ := procGetDpiForWindow.Call(hwnd)
	if dpi == 0 {
		dpi = 96
	}
	scale := float64(dpi) / 96.0

	workX, workY, workW, workH := 0, 0, 0, 0
	if mon, _, _ := procMonitorFromWindow.Call(hwnd, monitorDefaultToNearest); mon != 0 {
		var mi w32MonitorInfo
		mi.CbSize = uint32(unsafe.Sizeof(mi))
		if ok, _, _ := procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi))); ok != 0 {
			workX, workY = int(mi.RcWork.Left), int(mi.RcWork.Top)
			workW = int(mi.RcWork.Right - mi.RcWork.Left)
			workH = int(mi.RcWork.Bottom - mi.RcWork.Top)
		}
	}
	if workW <= 0 || workH <= 0 {
		workW, workH = screenSize()
	}

	pw := int(float64(wantW) * scale)
	ph := int(float64(wantH) * scale)
	if workW > 80 && pw > workW-40 {
		pw = workW - 40
	}
	if workH > 80 && ph > workH-40 {
		ph = workH - 40
	}
	if pw < 480 {
		pw = 480
	}
	if ph < 360 {
		ph = 360
	}

	x := workX + (workW-pw)/2
	y := workY + (workH-ph)/2
	procSetWindowPos.Call(hwnd, 0, uintptr(x), uintptr(y),
		uintptr(pw), uintptr(ph), swpNoZOrder|swpShowWindow)

	s.lg.Info("窗口尺寸已按显示器 DPI 校准",
		"physical_w", pw, "physical_h", ph,
		"logical_w", wantW, "logical_h", wantH,
		"dpi", dpi, "scale_pct", int(scale*100))
}

// applyIcon 设置窗口与任务栏图标。
//
// 优先从 PE 资源取（exe 自带，不依赖外部文件）；取不到再退回同目录的 app.ico。
func (s *Shell) applyIcon() {
	hwnd := uintptr(s.view.Window())
	if hwnd == 0 {
		return
	}
	big := loadAppIcon(smCXIcon)
	small := loadAppIcon(smCXSmIcon)
	if big == 0 && small == 0 {
		// 退路：从磁盘上的 app.ico 加载
		if exe, err := os.Executable(); err == nil {
			ico := filepath.Join(filepath.Dir(exe), "app.ico")
			if _, err := os.Stat(ico); err == nil {
				if p := utf16Ptr(ico); p != nil {
					if h, _, _ := procLoadImageW.Call(0, uintptr(unsafe.Pointer(p)),
						imageIcon, 0, 0, lrLoadFromFile); h != 0 {
						big = h
					}
				}
			}
		}
	}
	if big != 0 {
		procSendMessageW.Call(hwnd, wmSetIcon, iconBig, big)
	}
	if small != 0 {
		procSendMessageW.Call(hwnd, wmSetIcon, iconSmall, small)
	}
	if big == 0 && small == 0 {
		s.lg.Warn("未能加载窗口图标（PE 资源与 app.ico 都不可用）")
	}
}

const (
	iconSmall = 0
	iconBig   = 1
)

// WebviewDataPath WebView2 独立数据目录（<root>/data/cache）：与日常浏览器隔离
// 但持久化，因此面板的设置（含 API Key）会被记住。
//
// 目录名用 cache 而不是 webview2：它对上层只是「壳的缓存」，换壳实现时不该改目录名。
func WebviewDataPath(home string) string {
	p := config.DataPath(home, "cache")
	if err := os.MkdirAll(p, 0o755); err != nil {
		return ""
	}
	return p
}
