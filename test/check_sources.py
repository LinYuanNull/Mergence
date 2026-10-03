#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""源码级不变式检查。

放这里的都是「断言抓不到、只有读源码才能发现」的东西。目前两条：

**1. 前端 JS 里不许出现真正的内联 style 属性。**

为什么需要它：首页 CSP 是 `style-src 'self'`（没有 `'unsafe-inline'`），
浏览器会**静默丢弃** markup 中解析出来的 `style="..."` —— DOM 里属性还在、
`getAttribute('style')` 也能读到，但完全不影响渲染。表现为图表整张空白、
进度条永远满格，而且**不报任何错**。这个坑已经踩过两次（用量时序图、zcode 额度条），
所以用一条静态检查把它钉住。

正解：markup 写 `data-style="..."`，渲染后调 `app.js` 的 `paintStyles(容器)`
（CSSOM 设置不受该限制）。

注意判据要用正则排除 `data-style=`（它天然含 `style=` 子串），并跳过注释行。

**2. 桌面层不许出现会产生通知的调用（最小化 / 隐藏窗口必须完全静默）。**

用户要求：最小化 ModelMux 时不弹任何系统提示——横幅、气泡、toast 都不行。
这条之所以要静态钉住，是因为通知一旦漏出去，**没有任何测试会失败**：
气泡弹不弹取决于用户的系统通知设置与「首次隐藏」状态，e2e 断言既抓不到
（弹窗在另一个进程/notifications 宿主里），也不该去断言（那就变成在测 Windows）。

所以改为守住**能力本身不存在**：`nifInfo`（NIF_INFO，气泡/通知的开关标志）
不得被重新引入，`Shell_NotifyIcon` 只允许用于增删改图标。
"""
import io
import os
import re
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
# 前端资源在 src/internal/assets/（2026-10-03 从 src/internal/web/ 迁出，见 assets/embed.go）
WEB = os.path.join(os.path.dirname(HERE), "src", "internal", "assets")
DESKTOP = os.path.join(os.path.dirname(HERE), "src", "internal", "desktop")
REPO = os.path.dirname(HERE)

# 真正的内联 style 属性：`style=` 前面不是 `-`（排除 data-style）也不是标识符字符
INLINE_STYLE = re.compile(r"(^|[^a-zA-Z0-9_-])style\s*=")
# 只扫前端脚本；index.html 的静态标记里目前没有内联样式，一并扫上更稳
TARGETS = ["app.js", "upstream.js", "overview.js", "zcode.js", "index.html"]

# 通知能力相关：NIF_INFO 是 Shell_NotifyIcon 里打开气泡/通知横幅的标志位。
# 桌面层任何地方（除注释）都不该再出现它。
NIF_INFO = re.compile(r"\bnifInfo\b|\bNIF_INFO\b")
# 其它会主动弹用户一脸的 API，一并禁掉
NOTIFY_APIS = re.compile(r"\bShowBalloonOnFirstHide\b|\bballoonOnHide\b|"
                         r"\bToastNotification\b|\bShowToast\b")

results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))
    print(("[PASS] " if ok else "[FAIL] ") + name + ("" if ok else "  ← " + str(detail)[:240]))


def main():
    sys.stdout.reconfigure(encoding="utf-8")
    total_hits = []
    scanned = 0
    for name in TARGETS:
        path = os.path.join(WEB, name)
        if not os.path.exists(path):
            continue
        scanned += 1
        for i, line in enumerate(io.open(path, encoding="utf-8", newline=""), 1):
            stripped = line.strip()
            # 跳过注释行：说明文字里出现 style="..." 是在讲反模式，不是真的用了它
            if stripped.startswith(("*", "//", "/*", "<!--")):
                continue
            if INLINE_STYLE.search(line):
                total_hits.append(f"{name}:{i}: {stripped[:100]}")

    check("扫过前端脚本与页面（至少 4 个）", scanned >= 4, "scanned=%d" % scanned)
    check("JS/HTML 里没有真正的内联 style 属性（会被 CSP 静默丢弃）",
          not total_hits, "\n        ".join(total_hits[:8]))

    # 反向确认：约定属性确实在用，避免「改了名字但没人调 paintStyles」
    used = 0
    for name in TARGETS:
        path = os.path.join(WEB, name)
        if os.path.exists(path):
            used += io.open(path, encoding="utf-8", newline="").read().count("data-style=")
    check("data-style 约定确实在被使用", used > 0, "count=%d" % used)

    app = io.open(os.path.join(WEB, "app.js"), encoding="utf-8", newline="").read()
    check("app.js 提供 paintStyles 且内部走 CSSOM",
          "function paintStyles" in app and "setProperty" in app, "")

    # ── 桌面层静默不变式：最小化/隐藏窗口不得触发任何通知 ──
    # 扫 src/internal/desktop 全部 .go + main.go（NewLifecycle 的调用方在 main）
    go_files = [f for f in sorted(os.listdir(DESKTOP)) if f.endswith(".go")]
    check("扫过桌面层 Go 源码（至少 4 个文件）", len(go_files) >= 4, "n=%d" % len(go_files))
    go_files = go_files + ["main.go"]  # 相对 REPO 定位

    nif_hits, api_hits = [], []
    for f in go_files:
        path = os.path.join(DESKTOP, f) if f != "main.go" else os.path.join(REPO, f)
        if not os.path.exists(path):
            continue
        for i, line in enumerate(io.open(path, encoding="utf-8", newline=""), 1):
            stripped = line.strip()
            # 跳过注释：win32.go 里留了注释说明「为什么故意不定义这些常量」，
            # 那是解释反模式，不是真的在用它。
            if stripped.startswith(("*", "//", "/*")):
                continue
            if NIF_INFO.search(line):
                nif_hits.append(f"{f}:{i}: {stripped[:100]}")
            if NOTIFY_APIS.search(line):
                api_hits.append(f"{f}:{i}: {stripped[:100]}")

    check("桌面层没有重新引入 NIF_INFO（气泡/通知横幅的开关）",
          not nif_hits, "\n        ".join(nif_hits[:8]))
    check("配置与生命周期里没有残留的气泡开关",
          not api_hits, "\n        ".join(api_hits[:8]))

    # 反向确认：托盘图标功能本身还在（别为了静默把托盘也删了）
    tray_src = io.open(os.path.join(DESKTOP, "tray.go"), encoding="utf-8", newline="").read()
    check("托盘图标本身仍然保留（静默 ≠ 没有托盘）",
          "procShellNotifyIconW.Call(nimAdd" in tray_src, "")

    passed = sum(1 for _, ok, _ in results if ok)
    print("\n" + "=" * 46 + f"\n{passed}/{len(results)} 通过")
    failed = [n for n, ok, _ in results if not ok]
    if failed:
        print("失败项：\n  - " + "\n  - ".join(failed))
    return 0 if passed == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())
