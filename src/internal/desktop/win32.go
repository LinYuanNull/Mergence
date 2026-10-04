// win32.go Win32 API 声明与共用小工具。
//
// 全部走 syscall.NewLazyDLL，不引入任何第三方依赖。
// 只声明本项目真正用到的部分，避免大而全的绑定层带来维护负担。
package desktop

import (
	"sync"
	"syscall"
	"unsafe"
)

// wndRegistry 把 Go 对象与 HWND 关联起来。
//
// 刻意**不用** SetWindowLongPtr(GWLP_USERDATA)：那条路子需要把 uintptr 再转回
// unsafe.Pointer，vet 会报 "possible misuse of unsafe.Pointer"，而且槽位容易和
// 上游（go-webview2、托盘窗口类）打架。用 Go 侧的表既类型安全，又顺手持有引用，
// 避免对象被 GC 回收。
var wndRegistry sync.Map // hwnd uintptr -> any

func registerWnd(hwnd uintptr, v any) {
	if hwnd != 0 {
		wndRegistry.Store(hwnd, v)
	}
}

func lookupWnd(hwnd uintptr) any {
	if hwnd == 0 {
		return nil
	}
	v, _ := wndRegistry.Load(hwnd)
	return v
}

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")

	procRegisterClassExW      = user32.NewProc("RegisterClassExW")
	procCreateWindowExW       = user32.NewProc("CreateWindowExW")
	procDefWindowProcW        = user32.NewProc("DefWindowProcW")
	procCallWindowProcW       = user32.NewProc("CallWindowProcW")
	procDestroyWindow         = user32.NewProc("DestroyWindow")
	procGetMessageW           = user32.NewProc("GetMessageW")
	procTranslateMessage      = user32.NewProc("TranslateMessage")
	procDispatchMessageW      = user32.NewProc("DispatchMessageW")
	procPostMessageW          = user32.NewProc("PostMessageW")
	procPostQuitMessage       = user32.NewProc("PostQuitMessage")
	procSetWindowLongPtrW     = user32.NewProc("SetWindowLongPtrW")
	procGetWindowLongPtrW     = user32.NewProc("GetWindowLongPtrW")
	procShowWindow            = user32.NewProc("ShowWindow")
	procIsWindowVisible       = user32.NewProc("IsWindowVisible")
	procSetForegroundWindow   = user32.NewProc("SetForegroundWindow")
	procBringWindowToTop      = user32.NewProc("BringWindowToTop")
	procFindWindowW           = user32.NewProc("FindWindowW")
	procSetWindowPos          = user32.NewProc("SetWindowPos")
	procGetSystemMetrics      = user32.NewProc("GetSystemMetrics")
	procLoadImageW            = user32.NewProc("LoadImageW")
	procLoadIconW             = user32.NewProc("LoadIconW")
	procSendMessageW          = user32.NewProc("SendMessageW")
	procGetDpiForWindow       = user32.NewProc("GetDpiForWindow")
	procMonitorFromWindow     = user32.NewProc("MonitorFromWindow")
	procGetMonitorInfoW       = user32.NewProc("GetMonitorInfoW")
	procGetCursorPos          = user32.NewProc("GetCursorPos")
	procCreatePopupMenu       = user32.NewProc("CreatePopupMenu")
	procAppendMenuW           = user32.NewProc("AppendMenuW")
	procTrackPopupMenu        = user32.NewProc("TrackPopupMenu")
	procDestroyMenu           = user32.NewProc("DestroyMenu")
	procRegisterWindowMessage = user32.NewProc("RegisterWindowMessageW")
	procAttachThreadInput     = user32.NewProc("AttachThreadInput")
	procGetWindowThreadProcID = user32.NewProc("GetWindowThreadProcessId")
	procGetForegroundWindow   = user32.NewProc("GetForegroundWindow")
	procMessageBoxW           = user32.NewProc("MessageBoxW")

	procShellNotifyIconW       = shell32.NewProc("Shell_NotifyIconW")
	procShellNotifyIconGetRect = shell32.NewProc("Shell_NotifyIconGetRect")

	procGetModuleHandleW   = kernel32.NewProc("GetModuleHandleW")
	procCreateMutexW       = kernel32.NewProc("CreateMutexW")
	procCloseHandle        = kernel32.NewProc("CloseHandle")
	procGetCurrentThreadID = kernel32.NewProc("GetCurrentThreadId")
	procGetLastError       = kernel32.NewProc("GetLastError")
)

// 窗口消息与样式
const (
	wmDestroy       = 0x0002
	wmClose         = 0x0010
	wmSize          = 0x0005
	wmSetIcon       = 0x0080
	wmApp           = 0x8000 // WM_APP
	wmLButtonUp     = 0x0202
	wmRButtonUp     = 0x0205
	wmContextMenu   = 0x007B
	wmCommand       = 0x0111
	wmQuit          = 0x0012
	wmLButtonDblClk = 0x0203

	cwUseDefault = 0x80000000

	swHide    = 0
	swShow    = 5
	swRestore = 9

	hwndMessage   = ^uintptr(2) // -3，message-only window
	hwndTopMost   = ^uintptr(0) // -1
	hwndNoTopMost = ^uintptr(1) // -2

	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpNoZOrder   = 0x0004
	swpShowWindow = 0x0040

	gwlpWndProc = ^uintptr(3) // -4

	smCXScreen = 0
	smCYScreen = 1
	smCXSmIcon = 49
	smCYSmIcon = 50
	smCXIcon   = 11
	smCYIcon   = 12

	imageIcon      = 1
	lrLoadFromFile = 0x00000010

	errorAlreadyExists = 183

	mbOK        = 0x00000000
	mbIconError = 0x00000010
	mbTopMost   = 0x00040000

	// 托盘
	nimAdd        = 0x00000000
	nimModify     = 0x00000001
	nimDelete     = 0x00000002
	nimSetVersion = 0x00000004
	// notifyIconVersion4 用「结构化事件 + 坐标」的回调协议。
	//
	// 老协议把「鼠标消息」直接当 lParam 送过来，于是任何一条杂散消息都会被
	// 当成「用户点了图标」。V4 之后 lParam = MAKELONG(事件, 图标ID)、
	// wParam = MAKELONG(x, y)，我们才有依据判断这一下到底点在哪儿。
	notifyIconVersion4 = 4

	// 通知事件（NOTIFYICON_VERSION_4 之后 lParam 的低字）
	ninSelect           = 0x0400 // WM_USER + 0
	ninKeySelect        = 0x0401
	ninBalloonShow      = 0x0402
	ninBalloonHide      = 0x0403
	ninBalloonTimeout   = 0x0404
	ninBalloonUserClick = 0x0405
	nifMessage          = 0x00000001
	nifIcon             = 0x00000002
	nifTip              = 0x00000004
	notifyIconID        = 1
	trayCallbackMsg     = wmApp + 1

	// 注意：NIF_INFO / NIIF_* 这些「弹气泡」的标志常量**故意不再定义**。
	// 隐藏窗口必须完全静默（见 lifecycle.HideToTray），托盘只做图标 + 菜单。
	// 需要提醒用户时用托盘右键菜单，不要用通知横幅替用户打断他。

	// 菜单
	mfString       = 0x00000000
	mfSeparator    = 0x00000800
	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	monitorDefaultToNearest = 2
)

// 托盘菜单项 ID（导出：main 的命令分发需要引用）
const (
	MenuShowWindow = 1001
	MenuOpenPanel  = 1002
	MenuCopyURL    = 1003
	MenuOpenData   = 1004
	MenuViewLogs   = 1005
	MenuQuit       = 1099
)

type w32Rect struct{ Left, Top, Right, Bottom int32 }

type w32Point struct{ X, Y int32 }

type w32MinMaxInfo struct {
	PtReserved     w32Point
	PtMaxSize      w32Point
	PtMaxPosition  w32Point
	PtMinTrackSize w32Point
	PtMaxTrackSize w32Point
}

type w32MonitorInfo struct {
	CbSize    uint32
	RcMonitor w32Rect
	RcWork    w32Rect
	DwFlags   uint32
}

type wndClassExW struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type w32Msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      w32Point
}

// notifyIconData 对应 NOTIFYICONDATAW（Vista+ 完整布局）。
// CbSize 用 unsafe.Sizeof 取得，实测须为 976 才算对齐正确。
//
// SzInfo / SzInfoTitle / HBalloonIcon / DwInfoFlags 是**气泡通知**用的字段，
// 我们已经不弹气泡了，但这些字段**必须保留**：它们撑起 NOTIFYICONDATAW 的
// 内存布局，删掉任何一个都会让 unsafe.Sizeof 算出的 CbSize 变小，Windows
// 收到尺寸不对的结构体后会直接拒绝，表现为**托盘图标整个出不来**。
// 字段留空（Go 的零值）即可，不会有副作用。
type notifyIconData struct {
	CbSize           uint32
	_                uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	_                uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [16]byte
	HBalloonIcon     uintptr
}

// ---------------------------------------------------------------- 小工具

func utf16Ptr(s string) *uint16 {
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return nil
	}
	return p
}

func utf16Into(dst []uint16, s string) {
	n := len(dst)
	if n == 0 {
		return
	}
	copy(dst[:n-1], syscall.StringToUTF16(s)[:min(len(syscall.StringToUTF16(s))-1, n-1)])
	dst[n-1] = 0
}

func moduleHandle() uintptr {
	h, _, _ := procGetModuleHandleW.Call(0)
	return h
}

// lastErr 取最近一次 Win32 错误码。
func lastErr() uintptr {
	e, _, _ := procGetLastError.Call()
	return e
}

func screenSize() (int, int) {
	w, _, _ := procGetSystemMetrics.Call(smCXScreen)
	h, _, _ := procGetSystemMetrics.Call(smCYScreen)
	return int(w), int(h)
}

// popupMenu 弹出右键菜单并返回被选中的命令 ID（0 表示取消）。
func popupMenu(hwnd uintptr, items []menuEntry) int {
	hmenu, _, _ := procCreatePopupMenu.Call()
	if hmenu == 0 {
		return 0
	}
	defer procDestroyMenu.Call(hmenu)

	for _, it := range items {
		if it.Separator {
			procAppendMenuW.Call(hmenu, mfSeparator, 0, 0)
			continue
		}
		procAppendMenuW.Call(hmenu, mfString, uintptr(it.ID), uintptr(unsafe.Pointer(utf16Ptr(it.Label))))
	}

	var pt w32Point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// TrackPopupMenu 前必须把窗口设为前台，否则点菜单外部不会消失
	procSetForegroundWindow.Call(hwnd)
	cmd, _, _ := procTrackPopupMenu.Call(hmenu,
		uintptr(tpmRightButton|tpmReturnCmd),
		uintptr(pt.X), uintptr(pt.Y), 0, hwnd, 0)
	procPostMessageW.Call(hwnd, 0, 0, 0) // 关闭菜单后补一条空消息，修复「菜单残留」
	return int(cmd)
}

type menuEntry struct {
	ID        int
	Label     string
	Separator bool
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
