// osshell.go 托盘菜单用到的两项系统能力：用默认程序打开、写剪贴板。
package desktop

import (
	"syscall"
	"unsafe"
)

var (
	procShellExecuteW    = shell32.NewProc("ShellExecuteW")
	procOpenClipboard    = user32.NewProc("OpenClipboard")
	procEmptyClipboard   = user32.NewProc("EmptyClipboard")
	procSetClipboardData = user32.NewProc("SetClipboardData")
	procCloseClipboard   = user32.NewProc("CloseClipboard")
	procGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	procGlobalLock       = kernel32.NewProc("GlobalLock")
	procGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
	procRtlMoveMemory    = kernel32.NewProc("RtlMoveMemory")
)

const (
	swShownormal  = 1
	gmemMoveable  = 0x0002
	cfUnicodeText = 13
)

// OpenURL 用系统默认浏览器打开链接。
func OpenURL(url string) bool {
	r, _, _ := procShellExecuteW.Call(0,
		uintptr(unsafe.Pointer(utf16Ptr("open"))),
		uintptr(unsafe.Pointer(utf16Ptr(url))),
		0, 0, swShownormal)
	return r > 32
}

// OpenPath 用资源管理器打开目录或文件。
func OpenPath(path string) bool {
	return OpenURL(path)
}

// SetClipboardText 写入剪贴板文本。
//
// 拷贝用 Win32 的 RtlMoveMemory 而不是 Go 的 unsafe.Slice：GlobalLock 返回的是
// uintptr，在 Go 侧把它转成 unsafe.Pointer 属于「跨 GC 的裸指针」，vet 会告警；
// 交给 Win32 直接按目标地址拷字节，绕开这个问题。
//
// 注意 GlobalAlloc 的内存所有权在 SetClipboardData 成功后会转移给系统，
// 因此**不能**在成功后再 GlobalFree——那会提前释放剪贴板数据。
func SetClipboardText(text string) bool {
	if r, _, _ := procOpenClipboard.Call(0); r == 0 {
		return false
	}
	defer procCloseClipboard.Call()

	if r, _, _ := procEmptyClipboard.Call(); r == 0 {
		return false
	}
	u := syscall.StringToUTF16(text)
	size := uintptr(len(u) * 2)

	h, _, _ := procGlobalAlloc.Call(gmemMoveable, size)
	if h == 0 {
		return false
	}
	p, _, _ := procGlobalLock.Call(h)
	if p == 0 {
		return false
	}
	procRtlMoveMemory.Call(p, uintptr(unsafe.Pointer(&u[0])), size)
	procGlobalUnlock.Call(h)

	if r, _, _ := procSetClipboardData.Call(cfUnicodeText, h); r == 0 {
		// 失败时所有权仍在本进程，需要自己释放
		return false
	}
	return true
}
