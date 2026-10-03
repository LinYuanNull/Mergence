// singleinstance.go 单实例保障。
//
// 现实中用户会反复双击快捷方式。没有单实例会出现多个进程抢同一批端口、状态互相覆盖
// （ports.json 被并发改写、同一 provider 被拉起两次）。
//
// 策略刻意保守：**互斥体只用于「已存在则唤出」，绝不据此拒绝启动**。
// 若持有者已崩溃、或唤出失败，就正常新建进程——「不让我启动」比「多开一个」
// 对用户的伤害大得多。
package desktop

import (
	"syscall"
	"unsafe"
)

// 用 Local\ 而不是 Global\：桌面应用按会话隔离即可，且 Global\ 需要
// SeCreateGlobalPrivilege，普通用户下可能直接失败。
const singleInstanceMutex = `Local\ModelMux.SingleInstance`

// AcquireSingleInstance 尝试获取单实例锁。
//
// 返回 first=true 表示本进程是第一个实例，release 用于退出时释放句柄。
// 返回 first=false 表示已有实例在跑（此时已尽力唤出其窗口）。
func AcquireSingleInstance(windowTitle string) (first bool, release func()) {
	name := utf16Ptr(singleInstanceMutex)
	// 必须用 Call 返回的 errno 判断，不能另调 GetLastError：
	// Go 运行时的 syscall 封装已经把错误码取出来了，事后再调一次拿到的是
	// 期间其它调用覆盖过的值，会漏判。
	h, _, err := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		// 互斥体都创建不出来：不阻断启动，按首实例处理
		return true, func() {}
	}
	if e, ok := err.(syscall.Errno); ok && uintptr(e) == errorAlreadyExists {
		procCloseHandle.Call(h)
		bringExistingToFront(windowTitle)
		return false, func() {}
	}
	return true, func() { procCloseHandle.Call(h) }
}

// bringExistingToFront 找到已有实例的主窗口并唤出。
func bringExistingToFront(title string) bool {
	hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(utf16Ptr(title))))
	if hwnd == 0 {
		return false
	}
	// 若已隐藏到托盘，先恢复
	procShowWindow.Call(hwnd, swRestore)
	procShowWindow.Call(hwnd, swShow)
	bringToFront(hwnd)
	return true
}
