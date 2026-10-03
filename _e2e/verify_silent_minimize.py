#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""最小化静默性验证：最小化 / 隐藏窗口时**不得产生任何系统通知**。

##判据为什么是「通知数据库」

气泡/横幅由**另一个进程**（ShellExperienceHost / ActionCenter）在系统层面弹出，
ModelMux 自己不打任何日志，所以读日志、抓进程都判不出来。

试过枚举通知窗口（类名 NotifyIconView / ToastWindow 等），**抓不到**——实测
气泡不一定会以可见顶层窗口出现（本机 EnableAutoTray=0 时根本不显示）。
那种写法是**假通过**：断言永远绿，因为检测器压根看不见气泡。

现在用的判据是：
    %LOCALAPPDATA%\\Microsoft\\Windows\\Notifications\\wpndatabase.db
    的 Notification 表行数。

这个判据是**被对照实验验证过的**：另写一个只发NIF_INFO、不做任何别的
探针程序跑一次，行数必然 +1（13 -> 14），且 Shell_NotifyIconW 返回 1。
也就是说「模型弹了气泡 -> 这里 +1」这条因果链是确凿的；
而 ModelMux 最小化后 +0，就是真的没有。

注意：这**不依赖气泡是否可见**。系统通知即使被静音、不显示，
投递记录依然入库 —— 这正是我们要的：我们禁止的是「发出通知」这个行为本身。

## 不用 tasklist 取 PID

`tasklist` 输出是系统 ANSI 代码页（中文 Windows 为 GBK），
subprocess(text=True) 按 utf-8 解码会炸 UnicodeDecodeError。改用 ToolHelp32快照。
"""
import ctypes
import ctypes.wintypes as wt
import os
import sqlite3
import sys
import time

user32 = ctypes.WinDLL("user32", use_last_error=True)
kernel32 = ctypes.WinDLL("kernel32", use_last_error=True)
# restype 必须显式声明：默认 c_int 会把 64 位 HWND 截断成 32 位，后续调用拿到垃圾句柄
EnumWindowsProc = ctypes.WINFUNCTYPE(wt.BOOL, wt.HWND, wt.LPARAM)

DB = os.path.expandvars(
    r"%LOCALAPPDATA%\Microsoft\Windows\Notifications\wpndatabase.db")

SW_HIDE, SW_MINIMIZE, SW_RESTORE = 0, 6, 9
WM_CLOSE = 0x0010

results = []


def ok(name, cond, detail=""):
    results.append((name, bool(cond)))
    print(("[PASS] " if cond else "[FAIL] ") + name +
          ("" if cond else "  <- " + str(detail)[:200]))


def notif_count():
    """通知库里的投递记录数。只读打开，绝不写入用户的通知数据库。"""
    if not os.path.exists(DB):
        return -1
    con = sqlite3.connect("file:" + DB.replace("\\", "/") + "?mode=ro", uri=True)
    try:
        return con.execute("select count(*) from Notification").fetchone()[0]
    finally:
        con.close()


def app_pid():
    """ToolHelp32 快照找 ModelMux.exe 的 PID（理由见文件头）。"""
    class PROCESSENTRY32(ctypes.Structure):
        _fields_ = [
            ("dwSize", wt.DWORD), ("cntUsage", wt.DWORD),
            ("th32ProcessID", wt.DWORD), ("th32DefaultHeapID", ctypes.POINTER(ctypes.c_ulong)),
            ("th32ModuleID", wt.DWORD), ("cntThreads", wt.DWORD),
            ("th32ParentProcessID", wt.DWORD), ("pcPriClassBase", ctypes.c_long),
            ("dwFlags", wt.DWORD), ("szExeFile", wt.WCHAR * 260),
        ]

    snap = kernel32.CreateToolhelp32Snapshot(0x00000002, 0)
    if snap == -1:
        return 0
    pe = PROCESSENTRY32()
    pe.dwSize = ctypes.sizeof(PROCESSENTRY32)
    found = 0
    if kernel32.Process32FirstW(snap, ctypes.byref(pe)):
        while True:
            if pe.szExeFile.lower() == "modelmux.exe":
                found = pe.th32ProcessID
                break
            if not kernel32.Process32NextW(snap, ctypes.byref(pe)):
                break
    kernel32.CloseHandle(snap)
    return found


def main_window(target_pid):
    """按「标题 == ModelMux」找主窗口。

    不按类名找：go-webview2 注册的类名是 `webview`（不是 Chrome_WidgetWin_*），
    而且同一进程里还有 IME 之类的辅助窗口，按标题更准。
    """
    hits = []
    pid = wt.DWORD()

    def cb(hwnd, _):
        user32.GetWindowThreadProcessId(hwnd, ctypes.byref(pid))
        if pid.value == target_pid:
            buf = ctypes.create_unicode_buffer(256)
            user32.GetWindowTextW(hwnd, buf, 256)
            if buf.value == "ModelMux":
                hits.append(hwnd)
        return True

    user32.EnumWindows(EnumWindowsProc(cb), 0)
    return hits


def main():
    sys.stdout.reconfigure(encoding="utf-8")

    pid = app_pid()
    ok("ModelMux 正在运行", pid, "pid=%d" % pid)
    if not pid:
        return 1

    wins = main_window(pid)
    ok("找到主窗口", bool(wins), "n=%d" % len(wins))
    if not wins:
        return 1
    hwnd = wins[0]

    base = notif_count()
    ok("通知数据库可读（判据可用）", base >= 0, "count=%s" % base)
    if base < 0:
        print("      没这台机器的通知数据库，无法判定；请改用 check_sources.py 的静态守卫")
        return 1

    # 1) 真实最小化：用户点标题栏最小化按钮走的就是这条
    user32.ShowWindow(hwnd, SW_MINIMIZE)
    time.sleep(3.0)
    ok("最小化后进程仍在运行", app_pid() == pid, "pid=%s" % app_pid())
    n1 = notif_count()
    ok("最小化不产生任何通知", n1 == base, "before=%d after=%d" % (base, n1))

    # 2) 隐藏到托盘 —— 旧实现在这里弹「ModelMux仍在后台运行」气泡
    #用 WM_CLOSE 而不是 SW_HIDE：只有 WM_CLOSE 才会走我们自己的
    # HideToTray()（子类化窗口过程拦截），SW_HIDE 是绕过业务代码的裸调用。
    user32.PostMessageW(hwnd, WM_CLOSE, 0, 0)
    time.sleep(3.5)
    n2 = notif_count()
    ok("隐藏到托盘（WM_CLOSE -> HideToTray）不产生通知", n2 == base,
       "before=%d after=%d" % (base, n2))
    # 确认窗口真的隐藏了，否则这条断言是空跑
    ok("窗口确实已隐藏（确保断言不是空跑）",
       not user32.IsWindowVisible(hwnd), "IsWindowVisible=%d" % user32.IsWindowVisible(hwnd))

    # 3) 恢复，确认恢复过程也不弹
    user32.ShowWindow(hwnd, SW_RESTORE)
    time.sleep(2.0)
    n3 = notif_count()
    ok("恢复窗口时不产生通知", n3 == base, "before=%d after=%d" % (base, n3))

    passed = sum(1 for _, c in results if c)
    print("\n" + "=" * 46 + "\n%d/%d 通过" % (passed, len(results)))
    failed = [n for n, c in results if not c]
    if failed:
        print("失败项：\n  - " + "\n  - ".join(failed))
    return 0 if passed == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())
