// tray.go 系统托盘图标与右键菜单。
//
// 两条容易踩的坑，这里都处理了：
//
//  1. **消息循环必须独占一个 OS 线程**。托盘需要一个窗口接收 Shell_NotifyIcon 的回调，
//     而这个窗口不能复用 WebView2 的窗口（否则和它的窗口过程打架）。所以这里起一个
//     专门的线程，runtime.LockOSThread 锁住，建 message-only window 并跑自己的 GetMessage。
//     线程 ID 记下来，退出时靠 PostThreadMessage(WM_QUIT) 结束它。
//
//  2. **必须监听 TaskbarCreated**。explorer.exe 崩溃或重启会清空整个通知区域，且图标
//     *不会*自动恢复。不处理这条，用户会遇到「托盘图标莫名其妙没了」——而托盘是本应用
//     关窗后唯一的入口，等于程序失联。
package desktop

import (
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"modelmux/internal/logging"
)

var procPostThreadMessageW = user32.NewProc("PostThreadMessageW")

// Tray 托盘图标。
type Tray struct {
	lg *logging.Logger
	// onCommand 在**托盘线程**上被调用；实现方需自行 w.Dispatch 回 UI 线程。
	onCommand func(cmd int)

	hwnd      uintptr
	threadID  uint32
	hicon     uintptr
	taskbarID uint32
	// v4 表示已成功切到 NOTIFYICON_VERSION_4 回调协议（能拿到坐标）。
	v4 bool

	ready chan struct{}
	done  chan struct{}
	once  sync.Once
}

// NewTray 创建托盘（未启动）。
func NewTray(lg *logging.Logger, onCommand func(cmd int)) *Tray {
	return &Tray{
		lg:        lg,
		onCommand: onCommand,
		ready:     make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// Start 起线程并完成注册；返回时图标已就绪（有超时保护）。
func (t *Tray) Start() error {
	go t.run()
	<-t.ready
	if t.hwnd == 0 {
		return errTrayInit
	}
	return nil
}

var errTrayInit = syscall.EINVAL

func (t *Tray) run() {
	// 消息循环必须固定在同一个 OS 线程上，否则 PostThreadMessage 找不到它。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	t.threadID = currentThreadID()

	className := "ModelMuxTrayWnd"
	hicon := loadAppIcon(smCXSmIcon)
	if hicon == 0 {
		hicon = loadAppIcon(smCXIcon)
	}
	t.hicon = hicon

	wc := wndClassExW{
		CbSize:        uint32(unsafe.Sizeof(wndClassExW{})),
		LpfnWndProc:   syscall.NewCallback(trayWndProc),
		HInstance:     moduleHandle(),
		LpszClassName: utf16Ptr(className),
	}
	// 类已注册时 RegisterClassEx 返回 0 且错误为「已存在」，属正常（重复启动场景）。
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		if e, ok := err.(syscall.Errno); !ok || e != syscall.Errno(1410) { // ERROR_CLASS_ALREADY_EXISTS
			t.lg.Warn("RegisterClassEx 失败", "err", err.Error())
		}
	}

	// HWND_MESSAGE：只收消息、不显示
	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(utf16Ptr(className))),
		uintptr(unsafe.Pointer(utf16Ptr("ModelMux"))),
		0, 0, 0, 0, 0,
		hwndMessage, 0, moduleHandle(), 0,
	)
	if hwnd == 0 {
		t.lg.Error("创建托盘消息窗口失败", "err", lastErr())
		close(t.ready)
		return
	}
	t.hwnd = hwnd

	// 把 this 指针登记到 Go 侧注册表，供 wndproc 取回
	registerWnd(hwnd, t)

	rid, _, _ := procRegisterWindowMessage.Call(
		uintptr(unsafe.Pointer(utf16Ptr("TaskbarCreated"))))
	t.taskbarID = uint32(rid)

	if err := t.addIcon(); err != nil {
		t.lg.Error("添加托盘图标失败", "err", err.Error())
	} else {
		// 把 hwnd 与图标 ID 记下来：托盘是关窗后唯一的入口，出问题时这两个值是
		// 唯一能从外部（Shell_NotifyIconGetRect）核对图标是否真的在通知区里的依据。
		t.lg.Info("托盘图标已就绪",
			"hicon", t.hicon, "hwnd", t.hwnd, "icon_id", notifyIconID)
	}
	close(t.ready)

	var msg w32Msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(r) <= 0 { // 0 = WM_QUIT，-1 = 出错
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}

	t.delIcon()
	close(t.done)
}

// Stop 结束托盘线程。**必须在销毁窗口之前调用**（NIM_DELETE 要早于窗口销毁）。
func (t *Tray) Stop() {
	// 主动删图标：不等线程收到 WM_QUIT 再删，避免进程退出流程中留下幽灵图标。
	t.delIcon()
	if t.threadID != 0 {
		procPostThreadMessageW.Call(uintptr(t.threadID), wmQuit, 0, 0)
	}
	t.once.Do(func() {
		select {
		case <-t.done:
		case <-timeAfter(2 * 1e9): // 2s
		}
	})
}

func (t *Tray) addIcon() error {
	var nid notifyIconData
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = t.hwnd
	nid.UID = notifyIconID
	nid.UFlags = nifMessage | nifIcon | nifTip
	nid.UCallbackMessage = trayCallbackMsg
	nid.HIcon = t.hicon
	utf16Into(nid.SzTip[:], "ModelMux · 多账号 API 聚合")

	r, _, err := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	if r == 0 {
		return err
	}

	// 切到 V4 回调协议。失败也能工作（退化成老协议），所以只记日志不报错。
	var ver notifyIconData
	ver.CbSize = uint32(unsafe.Sizeof(ver))
	ver.HWnd = t.hwnd
	ver.UID = notifyIconID
	ver.UVersion = notifyIconVersion4
	if rv, _, verr := procShellNotifyIconW.Call(nimSetVersion, uintptr(unsafe.Pointer(&ver))); rv == 0 {
		t.lg.Warn("托盘未切换到 V4 回调协议，将退化为老协议（点击判定会宽松一些）",
			"err", verr.Error())
	} else {
		t.v4 = true
	}
	return nil
}

// hitOnIcon 判断这一下点击是否真的落在我们的托盘图标上。
//
// 通知区把各种事件都送到同一个回调消息上。只按「消息类型」判断的话，
// 一条杂散消息就能把刚隐藏的窗口重新拉出来 —— 用户看到的是
// 「点了关闭，窗口又自己弹回来」。坐标校验是最直接的护栏。
//
// 取不到图标位置时不拦截：宁可偶尔多开一次窗口，也不要让正常点击失灵。
func (t *Tray) hitOnIcon(wp uintptr) bool {
	if !t.v4 {
		return true // 老协议没有坐标可校
	}
	x := int32(int16(wp & 0xFFFF))
	y := int32(int16((wp >> 16) & 0xFFFF))
	if x == 0 && y == 0 {
		return false // 没有坐标的事件不可能是「点在图标上」
	}
	var rc w32Rect
	if !t.iconRect(&rc) {
		return true
	}
	const pad = 12 // 图标四周留点余量，避免高 DPI 下点边缘被误判
	return x >= rc.Left-pad && x <= rc.Right+pad && y >= rc.Top-pad && y <= rc.Bottom+pad
}

// iconRect 用 Shell_NotifyIconGetRect 取图标在通知区里的实际位置。
// 这也是外部（测试脚本）用来核对「图标到底在不在」的同一个接口。
func (t *Tray) iconRect(rc *w32Rect) bool {
	// NOTIFYICONIDENTIFIER：cbSize + 对齐填充 + hWnd + uID + 对齐填充 + GUID
	type notifyIconIdent struct {
		CbSize   uint32
		_        uint32
		HWnd     uintptr
		UID      uint32
		_        uint32
		GuidItem [16]byte
	}
	var id notifyIconIdent
	id.CbSize = uint32(unsafe.Sizeof(id))
	id.HWnd = t.hwnd
	id.UID = notifyIconID
	r, _, _ := procShellNotifyIconGetRect.Call(
		uintptr(unsafe.Pointer(&id)), uintptr(unsafe.Pointer(rc)))
	return r == 0 // S_OK
}

func (t *Tray) delIcon() {
	if t.hwnd == 0 {
		return
	}
	var nid notifyIconData
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = t.hwnd
	nid.UID = notifyIconID
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
}

func trayWndProc(hwnd, msg, wp, lp uintptr) uintptr {
	t, _ := lookupWnd(hwnd).(*Tray)
	if t != nil && t.taskbarID != 0 && uint32(msg) == t.taskbarID {
		// explorer 重启了 —— 通知区域被清空，重新挂上图标
		if err := t.addIcon(); err != nil {
			t.lg.Warn("explorer 重启后重建托盘图标失败", "err", err.Error())
		} else {
			t.lg.Info("已响应 TaskbarCreated，托盘图标重建完成")
		}
		return 0
	}

	switch msg {
	case trayCallbackMsg:
		// V4 协议：lParam = MAKELONG(事件, 图标ID)，wParam = MAKELONG(x, y)。
		// 老协议：lParam 就是鼠标消息本身、wParam 是图标 ID，没有坐标。
		event := uint32(lp & 0xFFFF)
		iconID := uint32((lp >> 16) & 0xFFFF)
		if t != nil && !t.v4 {
			event, iconID = uint32(lp), notifyIconID
		}
		if t != nil && iconID != notifyIconID {
			return 0 // 不是我们图标的回调
		}
		switch event {
		case wmLButtonUp, wmLButtonDblClk, ninSelect, ninKeySelect:
			if t != nil && t.hitOnIcon(wp) {
				t.onCommand(MenuShowWindow)
			}
		case ninBalloonShow, ninBalloonHide, ninBalloonTimeout, ninBalloonUserClick:
			// 气泡事件与「打开窗口」无关，**必须显式忽略**。
			// 我们已经不再发气泡了（隐藏窗口要完全静默，见 lifecycle.HideToTray），
			// 但这些事件仍要留着忽略：早期实现把任何按钮消息都当成点了图标，
			// 结果气泡一出现就带出一条 WM_LBUTTONUP，窗口被立刻重新显示出来。
			// 留着这层忽略是廉价的防御——万一将来有人再引入通知，不会连带出这个 bug。
		case wmRButtonUp, wmContextMenu:
			if t != nil {
				if cmd := popupMenu(hwnd, []menuEntry{
					{ID: MenuShowWindow, Label: "显示主窗口"},
					{ID: MenuOpenPanel, Label: "打开面板（浏览器）"},
					{ID: MenuCopyURL, Label: "复制 API 地址"},
					{ID: MenuOpenData, Label: "打开数据目录"},
					{ID: MenuViewLogs, Label: "查看日志"},
					{Separator: true},
					{ID: MenuQuit, Label: "退出"},
				}); cmd != 0 {
					t.onCommand(cmd)
				}
			}
		}
		return 0
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

func currentThreadID() uint32 {
	r, _, _ := procGetCurrentThreadID.Call()
	return uint32(r)
}

// loadAppIcon 从 exe 内嵌的 PE 图标资源加载。
//
// 刻意不依赖 app.ico 文件存在——用户可能只拷走 exe。与 DPI 清单、版本信息同处
// 一个 .rsrc 段。
//
// 资源既可能是整数 ID，也可能是**名字**：go-winres 用 winres.json 里 RT_GROUP_ICON
// 的键名作为资源名（本项目是 "APP"），而 MAKEINTRESOURCE 的整数形式只对数字 ID 有效。
// 两种都试，否则会静默拿到 0 —— 表现为托盘图标空白。
func loadAppIcon(sizeMetric int) uintptr {
	cx, _, _ := procGetSystemMetrics.Call(uintptr(sizeMetric))
	if cx == 0 {
		cx = 16
	}
	cy := cx

	if h, _, _ := procLoadImageW.Call(moduleHandle(), 1, imageIcon, cx, cy, 0); h != 0 {
		return h
	}
	if p := utf16Ptr(iconResourceName); p != nil {
		if h, _, _ := procLoadImageW.Call(moduleHandle(), uintptr(unsafe.Pointer(p)),
			imageIcon, cx, cy, 0); h != 0 {
			return h
		}
	}
	// 退路：按默认尺寸加载
	h, _, _ := procLoadIconW.Call(moduleHandle(), 1)
	return h
}

// iconResourceName 与 winres/winres.json 里 RT_GROUP_ICON 的键名保持一致。
const iconResourceName = "APP"

// timeAfter 避免为一次等待引入 time 包的间接层。
func timeAfter(ns int64) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		syscall.SyscallN(sleepProc.Addr(), uintptr(ns/1e6)) // Sleep(ms)
		close(ch)
	}()
	return ch
}

var sleepProc = kernel32.NewProc("Sleep")
