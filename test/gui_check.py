#!/usr/bin/env python3
"""GUI 形态回归验证：确认 P3 的改动没有破坏 P1 的窗口与托盘生命周期。

用隔离的 MODELMUX_HOME，不动用户的真实配置。

两个检查口径上的坑，这里都绕开了：
  - tasklist 的「没有匹配」提示是**本地化中文**，用 "INFO" 判断「无进程」会永远判错。
    这里改成按镜像名匹配行。
  - 托盘窗口是 message-only window（HWND_MESSAGE），**FindWindow / EnumWindows 都
    枚举不到**，用 FindWindow 判断「托盘没起来」是假阴性。这里改用
    Shell_NotifyIconGetRect —— 它以「通知区里到底有没有这个图标」为准，
    这也是唯一能验证「退出后不留幽灵图标」的手段。
"""
import ctypes
import ctypes.wintypes as wt
import json
import os
import re
import shutil
import subprocess
import sys
import time
import urllib.request

# 项目根：由本文件位置推导（test/ 的上一层），不写死本机绝对路径。
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HOME = os.path.join(ROOT, "test", "gui_home")
EXE = os.path.join(ROOT, "ModelMux.exe")
FAKE = os.path.join(ROOT, "test", "fake_upstream.py")
# 用当前解释器；换机器/换 Python 位置不必改代码（可用 MODELMUX_PY 覆盖）。
PY = os.environ.get("MODELMUX_PY", sys.executable)
FAKE_PORT = 18094
TITLE = "ModelMux"
ICON_ID = 1

sys.stdout.reconfigure(encoding="utf-8")

CONFIG = {
    "version": 2,
    "embedded_providers": [{
        "name": "or", "display_name": "OpenRouter", "enabled": True, "preset": "openrouter",
        "base_url": f"http://127.0.0.1:{FAKE_PORT}/chat/v1", "protocol": "chat",
        "model_prefix": "or/", "api_keys": ["sk-or-v1-abcdefghijklmnop1234"],
        "models": ["fake-alpha", "fake-beta"], "weight": 1,
    }],
}

u32 = ctypes.windll.user32
shell32 = ctypes.windll.shell32

fails = []


def ok(name, cond, detail=""):
    print(("[PASS] " if cond else "[FAIL] ") + name + (("  " + detail) if detail and not cond else ""))
    if not cond:
        fails.append(name)
    return cond


def proc_count(exe):
    """按镜像名匹配行数。不要用 tasklist 的提示文案判断——它随时可能是本地化中文。"""
    out = subprocess.run(["tasklist", "/FO", "CSV", "/NH"],
                         capture_output=True, text=True, errors="ignore").stdout
    return sum(1 for line in out.splitlines() if exe.lower() in line.lower())


class NOTIFYICONIDENTIFIER(ctypes.Structure):
    _fields_ = [("cbSize", wt.DWORD), ("hWnd", ctypes.c_void_p),
                ("uID", wt.UINT), ("guidItem", ctypes.c_byte * 16)]


def icon_rect(hwnd):
    """返回 (HRESULT, RECT)。S_OK(0) 表示图标确实在通知区里。"""
    nii = NOTIFYICONIDENTIFIER()
    nii.cbSize = ctypes.sizeof(NOTIFYICONIDENTIFIER)
    nii.hWnd = hwnd
    nii.uID = ICON_ID
    rc = wt.RECT()
    hr = shell32.Shell_NotifyIconGetRect(ctypes.byref(nii), ctypes.byref(rc))
    return hr, rc


def read_log(path):
    try:
        return open(path, encoding="utf-8", errors="replace").read()
    except Exception:
        return ""


def log_field(text, msg, field):
    for line in text.splitlines():
        if msg in line:
            m = re.search(r'"%s":(\d+)' % field, line)
            if m:
                return int(m.group(1))
    return None


def main():
    if os.path.isdir(HOME):
        shutil.rmtree(HOME, ignore_errors=True)
    os.makedirs(os.path.join(HOME, "config"), exist_ok=True)
    with open(os.path.join(HOME, "config", "modelmux.json"), "w", encoding="utf-8") as f:
        json.dump(CONFIG, f, ensure_ascii=False, indent=2)

    fake = subprocess.Popen([PY, FAKE, str(FAKE_PORT)],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    mm = None
    try:
        time.sleep(1.0)
        env = dict(os.environ)
        env["MODELMUX_HOME"] = HOME
        env.pop("MODELMUX_HEADLESS", None)
        mm = subprocess.Popen([EXE], env=env)

        log = os.path.join(HOME, "data", "logs", "modelmux.log")
        port, deadline = None, time.time() + 30
        while time.time() < deadline and not port:
            for line in read_log(log).splitlines():
                if "内置服务已监听" in line:
                    m = re.search(r"127\.0\.0\.1:(\d+)", line)
                    if m:
                        port = int(m.group(1))
                        break
            time.sleep(0.4)
        ok("内置服务已监听", bool(port), "未取得端口")
        print("      端口 =", port)

        # ── 主窗口
        hwnd = 0
        for _ in range(40):
            hwnd = u32.FindWindowW(None, TITLE)
            if hwnd and u32.IsWindowVisible(hwnd):
                break
            time.sleep(0.5)
        ok("主窗口已显示", bool(hwnd), f"hwnd={hwnd}")
        print("      hwnd =", hex(hwnd) if hwnd else "-")

        # ── WebView2 渲染
        wv = proc_count("msedgewebview2.exe")
        ok("WebView2 子进程已起", wv >= 1, f"{wv} 个")
        print("      msedgewebview2 进程数 =", wv)

        # ── 托盘图标：以「通知区里有没有」为准
        #
        # 必须轮询等这行日志落盘，不能只读一次：主窗口在 NewShell 时就已可见，
        # 而托盘是随后（tray.Start）才起的，两者之间有一个窗口期。此时读到的
        # 日志里还没有「托盘图标已就绪」，会把它误判成「托盘没起来」——
        # 表现就是这条断言偶发失败，重跑又过了。
        hicon = tray_hwnd = None
        for _ in range(50):  # 最多等 10 秒
            text = read_log(log)
            hicon = log_field(text, "托盘图标已就绪", "hicon")
            tray_hwnd = log_field(text, "托盘图标已就绪", "hwnd")
            if hicon and tray_hwnd:
                break
            time.sleep(0.2)
        ok("托盘图标句柄已加载", bool(hicon), f"hicon={hicon}")
        print("      hicon =", hicon, " 托盘窗口 =", hex(tray_hwnd) if tray_hwnd else "-")

        hr, rc = icon_rect(tray_hwnd) if tray_hwnd else (1, None)
        ok("托盘图标确实在通知区（Shell_NotifyIconGetRect）", hr == 0,
           f"HRESULT=0x{hr & 0xFFFFFFFF:08X}")
        if hr == 0:
            print(f"      图标位置 ({rc.left},{rc.top})-({rc.right},{rc.bottom})")

        # ── 内容区渲染（可用 SKIP_SHOT=1 跳过，用于确认截图步骤是否干扰后续断言）
        time.sleep(1.5)
        if os.environ.get("SKIP_SHOT") != "1":
            shot = os.path.join(ROOT, "test", "shots", "gui_window.png")
            r = subprocess.run([PY, os.path.join(ROOT, "tools", "win", "capture_window.py"), TITLE, shot],
                               capture_output=True, text=True, encoding="utf-8", errors="replace")
            print("      截图：", (r.stdout or r.stderr).strip().replace("\n", " | ")[:160])
        else:
            print("      （已跳过截图）")

        # ── 关窗 = 隐藏（P1 核心行为）
        u32.PostMessageW(hwnd, 0x0010, 0, 0)  # WM_CLOSE

        # 不只查一瞬间，而是持续观察 5 秒：早期实现里窗口隐藏后会被一条
        # 杂散托盘消息重新显示出来，只查一次是抓不到的（表现为偶发通过）。
        time.sleep(0.6)
        stable, flipped = True, ""
        for _ in range(22):
            vis = bool(u32.IsWindowVisible(hwnd))
            exists = bool(u32.IsWindow(hwnd))
            if vis or not exists:
                stable = False
                flipped = f"visible={vis} exists={exists}"
                break
            time.sleep(0.2)
        alive = proc_count("ModelMux.exe")
        ok("关窗后只隐藏、进程继续（且 5 秒内不自己弹回来）",
           alive == 1 and stable, f"进程 {alive} 个；{flipped or '稳定'}")
        ok("隐藏后日志未出现「窗口已恢复显示」",
           "窗口已恢复显示" not in read_log(log),
           "窗口被杂散托盘消息重新显示了")

        # 隐藏状态下托盘图标应仍然可用（否则用户就彻底找不到入口了）
        hr2, _ = icon_rect(tray_hwnd) if tray_hwnd else (1, None)
        ok("隐藏后托盘图标仍在", hr2 == 0, f"HRESULT=0x{hr2 & 0xFFFFFFFF:08X}")

        # ── 面板触发退出
        try:
            rq = urllib.request.Request(f"http://127.0.0.1:{port}/api/quit",
                                        data=b"{}", method="POST")
            urllib.request.urlopen(rq, timeout=8).read()
        except Exception as e:
            print("      退出请求异常（可能已退出）：", e)

        deadline = time.time() + 25
        while time.time() < deadline and proc_count("ModelMux.exe") > 0:
            time.sleep(0.5)
        left = proc_count("ModelMux.exe")
        ok("退出后进程完全结束", left == 0, f"仍有 {left} 个")

        # ── 幽灵图标：进程没了，通知区里不该还留着
        gone, last_hr = False, None
        for _ in range(20):
            hr3, _ = icon_rect(tray_hwnd) if tray_hwnd else (1, None)
            last_hr = hr3
            if hr3 != 0:
                gone = True
                break
            time.sleep(0.4)
        ok("退出后通知区无残留图标", gone,
           f"HRESULT 始终为 0x{(last_hr or 0) & 0xFFFFFFFF:08X}（图标未移除）")

        tail = [l for l in read_log(log).splitlines()][-4:]
        print("\n      日志尾部：")
        for l in tail:
            print("       ", l[:150])

    finally:
        if mm and mm.poll() is None:
            mm.kill()
        fake.terminate()

    print("\n" + ("=" * 46) + "\n" + ("全部通过" if not fails else "失败项：" + "; ".join(fails)))
    return 0 if not fails else 1


if __name__ == "__main__":
    sys.exit(main())
