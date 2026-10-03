#!/usr/bin/env python3
"""构建 ModelMux.exe：同步前端资源 -> 编译。

**为什么需要这个脚本**：前端真源在仓库根的 `web/`，但 `go:embed`
不允许 `..`，所以编译器只能读 `src/internal/assets/` 里的副本。
两者之间必须有人负责同步 —— 否则会出现「改了 web/ 却在跑旧前端」
这种最难查的静默不一致（页面照样打开，只是内容不对）。

    python tools/release/build.py             # 同步 + 编译
    python tools/release/build.py --check     # 只校验两边是否一致
    python tools/release/build.py --no-build  # 只同步，不编译

go 可执行文件按 `GO` / `MODELMUX_GO` 环境变量 -> PATH -> GOROOT ->
常见安装位置 的顺序查找，不写死本机路径。
"""
import hashlib
import os
import shutil
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
# 本脚本在 tools/release/ 下，仓库根是 HERE 的上两级。
ROOT = os.path.abspath(os.environ.get("MODELMUX_SRC")
                       or os.path.dirname(os.path.dirname(HERE)))

WEB = os.path.join(ROOT, "web")
EMBED = os.path.join(ROOT, "src", "internal", "assets")
SRC = os.path.join(ROOT, "src")
OUT = os.path.join(ROOT, "ModelMux.exe")

# 前端资源清单：两边必须同名同内容。
ASSETS = ["index.html", "theme.js", "app.js", "upstream.js",
          "overview.js", "zcode.js", "app.css"]


def find_go():
    for var in ("GO", "MODELMUX_GO"):
        p = os.environ.get(var)
        if p and os.path.isfile(p):
            return p
    p = shutil.which("go")
    if p:
        return p
    cands = []
    root = os.environ.get("GOROOT")
    if root:
        cands.append(os.path.join(root, "bin", "go.exe"))
    for drive in ("C:", "D:", "E:", "F:"):
        base = drive + os.sep
        cands.append(os.path.join(base, "Go", "bin", "go.exe"))
        cands.append(os.path.join(base, "Program Files", "Go", "bin", "go.exe"))
    cands.append(os.path.expandvars(r"%LOCALAPPDATA%\Programs\Go\bin\go.exe"))
    for c in cands:
        if os.path.isfile(c):
            return c
    return None


def digest(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


def sync_assets(check_only):
    """把 web/ 同步到 src/internal/assets/。返回 (同步数, 不一致清单)。"""
    if not os.path.isdir(WEB):
        print("错误：前端真源目录不存在：" + WEB)
        sys.exit(2)
    missing = [f for f in ASSETS if not os.path.isfile(os.path.join(WEB, f))]
    if missing:
        print("错误：web/ 缺少文件：" + ", ".join(missing))
        sys.exit(2)

    copied, drift = 0, []
    for f in ASSETS:
        src, dst = os.path.join(WEB, f), os.path.join(EMBED, f)
        if os.path.isfile(dst) and digest(src) == digest(dst):
            continue
        drift.append(f)
        if not check_only:
            shutil.copyfile(src, dst)
            copied += 1
    return copied, drift


def main():
    check_only = "--check" in sys.argv
    no_build = "--no-build" in sys.argv

    copied, drift = sync_assets(check_only)
    if check_only:
        if drift:
            print("前端不一致（%d 个）：%s" % (len(drift), ", ".join(drift)))
            print("跑 python tools/release/build.py 可同步后编译。")
            sys.exit(1)
        print("前端一致：web/ 与 src/internal/assets/ 逐字节相同（%d 个）" % len(ASSETS))
        return

    if copied:
        print("已同步 %d 个前端文件到 src/internal/assets/：%s"
              % (copied, ", ".join(drift)))
    else:
        print("前端已是最新，无需同步（%d 个）" % len(ASSETS))

    if no_build:
        return

    go = find_go()
    if not go:
        print("找不到 go 可执行文件。请设置 GO 环境变量，或把 go 加进 PATH。")
        sys.exit(2)

    cmd = [go, "build", "-trimpath",
           "-ldflags=-s -w -H windowsgui", "-o", OUT, "."]
    print("编译：" + " ".join(cmd))
    rc = subprocess.call(cmd, cwd=SRC)
    if rc != 0:
        sys.exit(rc)

    size = os.path.getsize(OUT)
    print("产物：%s  %d 字节  sha256=%s"
          % (OUT, size, digest(OUT)[:16]))


if __name__ == "__main__":
    main()
