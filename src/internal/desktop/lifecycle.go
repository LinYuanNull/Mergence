// lifecycle.go 窗口生命周期：拦截关闭 → 隐藏到托盘；托盘「退出」→ 有序释放。
//
// 核心难点是**拦截 WM_CLOSE**：go-webview2 的窗口过程收到 WM_CLOSE 时直接
// DestroyWindow，不给外部干预的机会。所以这里做窗口子类化——
// SetWindowLongPtrW(hwnd, GWLP_WNDPROC, ours) 换成我们的过程，
// 只拦 WM_CLOSE，其余消息一律 CallWindowProcW 转发给原过程。
//
// 「只拦一个消息，其余全部转发」是子类化不出问题的关键：任何自己去重新实现
// 默认行为的做法，都会在某个边角消息上和上游打架。
package desktop

import (
	"sync/atomic"
	"syscall"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"

	"mergence/internal/logging"
)

// Lifecycle 窗口生命周期管理器。
type Lifecycle struct {
	lg   *logging.Logger
	view webview2.WebView
	tray *Tray

	hwnd        uintptr
	origWndProc uintptr

	// minimizeToTray 关窗时是隐藏到托盘还是直接退出。
	//
	// 必须用 atomic：窗口过程在 UI 线程读它，而面板改设置是从 HTTP goroutine
	// 写进来的（OnCloseBehaviorChange）。普通 bool 在这里是数据竞争，症状是
	// 「改了没生效」或偶发地把窗口吞掉——两者都比报错更难查。
	minimizeToTray atomic.Bool

	quitting atomic.Bool
	hidden   atomic.Bool
}

// NewLifecycle 创建生命周期管理器。
func NewLifecycle(lg *logging.Logger, view webview2.WebView, minimizeToTray bool) *Lifecycle {
	l := &Lifecycle{lg: lg, view: view}
	l.minimizeToTray.Store(minimizeToTray)
	return l
}

// SetTray 注入托盘（在 Install 之前调用）。
func (l *Lifecycle) SetTray(t *Tray) { l.tray = t }

// SetMinimizeToTray 热改「关窗行为」，立即生效。
//
// 必须在 UI 线程调用：窗口过程与它读的是同一份状态，调用方负责切线程。
// 幂等——值没变时不重复记日志。
func (l *Lifecycle) SetMinimizeToTray(on bool) {
	if l.minimizeToTray.Load() == on {
		return
	}
	l.minimizeToTray.Store(on)
	if on {
		l.lg.Info("关窗行为已改为：隐藏到托盘（进程继续运行）")
	} else {
		l.lg.Info("关窗行为已改为：直接退出应用")
	}
}

// DisableMinimizeToTray 关闭「关窗驻留托盘」。
//
// 托盘初始化失败时**必须**调用：否则用户关窗后进程既没退出、托盘里又找不到，
// 只能去任务管理器杀——这比「关窗即退出」糟糕得多。
func (l *Lifecycle) DisableMinimizeToTray() { l.SetMinimizeToTray(false) }

// Install 子类化窗口。必须在窗口创建之后、进入消息循环之前调用。
func (l *Lifecycle) Install() {
	hwnd := uintptr(l.view.Window())
	if hwnd == 0 {
		l.lg.Error("拿不到窗口句柄，无法安装关闭拦截——关窗将直接退出")
		return
	}
	l.hwnd = hwnd

	// 登记到 Go 侧注册表（不用 GWLP_USERDATA，理由见 win32.go）
	registerWnd(hwnd, l)

	prev, _, _ := procSetWindowLongPtrW.Call(hwnd, gwlpWndProc,
		syscall.NewCallback(lifecycleWndProc))
	if prev == 0 {
		l.lg.Error("SetWindowLongPtr 子类化失败", "err", lastErr())
		return
	}
	l.origWndProc = prev
	// 日志按实际行为措辞：这个值可以在面板里热改，写死「将隐藏到托盘」会让
	// 排查「我明明选了直接退出」的人被日志带偏。
	if l.minimizeToTray.Load() {
		l.lg.Info("已安装关闭拦截：关窗将隐藏到托盘", "hwnd", hwnd, "minimize_to_tray", true)
	} else {
		l.lg.Info("已安装窗口过程：关窗将直接退出应用（不拦截关闭）", "hwnd", hwnd, "minimize_to_tray", false)
	}
}

// Quitting 是否正在退出（子类化过程据此决定「隐藏」还是「真关」）。
func (l *Lifecycle) Quitting() bool { return l.quitting.Load() }

// Hidden 窗口当前是否处于隐藏（托盘驻留）状态。
func (l *Lifecycle) Hidden() bool { return l.hidden.Load() }

func lifecycleWndProc(hwnd, msg, wp, lp uintptr) uintptr {
	l, _ := lookupWnd(hwnd).(*Lifecycle)
	if l == nil || l.origWndProc == 0 {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
		return r
	}

	if uint32(msg) == wmClose && !l.quitting.Load() && l.minimizeToTray.Load() {
		l.HideToTray()
		return 0 // 吞掉这次关闭，窗口不动、进程不退
	}

	// 其余消息（含真退出时的 WM_CLOSE）全部交还原过程
	r, _, _ := procCallWindowProcW.Call(l.origWndProc, hwnd, msg, wp, lp)
	return r
}

// HideToTray 隐藏窗口并驻留托盘。
//
// 这里**刻意不弹任何气泡/通知**。隐藏窗口是用户自己按下的最小化/关闭动作，
// 系统却回一条「X 正在后台运行」的通知横幅，等于替用户凭空打断他；
// 托盘图标本身已经说明了「程序还在」，托盘右键菜单也一直有「显示主窗口」。
//
// 早期实现会在首次隐藏时弹一次气泡（配置项 show_balloon_on_first_hide），
// 现已连配置项一起删除：把它改成默认 false 是没用的——用户既有配置里
// 已经写着 true，老配置照样会弹。要彻底静默就得移除这个能力本身。
// 需要确认程序是否还在，看托盘图标即可。
func (l *Lifecycle) HideToTray() {
	if l.hwnd == 0 {
		return
	}
	procShowWindow.Call(l.hwnd, swHide)
	l.hidden.Store(true)
	l.lg.Info("窗口已隐藏到托盘（进程继续运行）")
}

// ShowWindow 恢复并置前窗口。
func (l *Lifecycle) ShowWindow() {
	if l.hwnd == 0 {
		return
	}
	procShowWindow.Call(l.hwnd, swRestore)
	procShowWindow.Call(l.hwnd, swShow)
	bringToFront(l.hwnd)
	l.hidden.Store(false)
	l.lg.Info("窗口已恢复显示")
}

// Exit 执行有序退出（由托盘「退出」调用）。
//
// 这里只做**前三步**，剩下的由 main 在 Run() 返回后继续：
//
//  1. 置 quitting —— 否则 WM_CLOSE 又被拦成「隐藏」，永远退不出去
//  2. 删托盘图标 —— 必须最早，否则崩溃/卡住时会在通知区留幽灵图标
//  3. 真正关闭窗口 —— 让消息循环结束、Run() 返回
func (l *Lifecycle) Exit() {
	if !l.quitting.CompareAndSwap(false, true) {
		return // 已经在退了
	}
	l.lg.Info("收到退出请求，开始有序退出")

	// 2) 移除托盘图标（最早）
	if l.tray != nil {
		l.tray.Stop()
	}

	// 3) 真正关闭窗口
	if l.hwnd != 0 {
		procPostMessageW.Call(l.hwnd, wmClose, 0, 0)
	}
}

// bringToFront 把窗口可靠地切到前台。
//
// 直接 SetForegroundWindow 常被系统拒绝（调用方不是前台进程时）。必须先把本线程
// 与当前前台窗口的线程 AttachThreadInput 挂接起来，才拿得到前台权限。
func bringToFront(hwnd uintptr) {
	fg, _, _ := procGetForegroundWindow.Call()
	if fg == 0 || fg == hwnd {
		procSetForegroundWindow.Call(hwnd)
		procBringWindowToTop.Call(hwnd)
		return
	}
	myTID := currentThreadID()
	var fgTID uint32
	procGetWindowThreadProcID.Call(fg, uintptr(unsafe.Pointer(&fgTID)))
	if fgTID == 0 {
		return
	}
	procAttachThreadInput.Call(uintptr(myTID), uintptr(fgTID), 1)
	procBringWindowToTop.Call(hwnd)
	procSetForegroundWindow.Call(hwnd)
	// 顺带取消可能的置顶残留
	procSetWindowPos.Call(hwnd, hwndNoTopMost, 0, 0, 0, 0, swpNoMove|swpNoSize)
	procAttachThreadInput.Call(uintptr(myTID), uintptr(fgTID), 0)
}

// MessageBox 原生错误框（GUI 子系统程序没有控制台，出错只能靠弹窗）。
func MessageBox(title, text string, isErr bool) {
	flags := uintptr(mbOK | mbTopMost)
	if isErr {
		flags |= mbIconError
	}
	procMessageBoxW.Call(0,
		uintptr(unsafe.Pointer(utf16Ptr(text))),
		uintptr(unsafe.Pointer(utf16Ptr(title))),
		flags)
}
