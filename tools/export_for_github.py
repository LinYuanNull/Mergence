#!/usr/bin/env python3
"""导出「可提交 GitHub」的干净副本。

原则：**白名单式**复制（deny-by-default）。只列进来的才出去，
避免将来新增文件时不小心把数据或构建产物带进公开仓库。

绝不外带的：
  _ref/               接口响应转储，可能含真实账号 token、user_plan_id、计费额度
  internal/web/.tmp   测试写出的残留配置，可能含真实 access_key
  backups/ _backup/   历史快照与数据包
  *.exe / *.exe~      编译产物与编辑器残留
  _e2e 运行期目录      Edge/WebView2 缓存、隔离 HOME、截图

    python tools/export_for_github.py
    GITHUB_EXPORT_DIR=D:/tmp/export python tools/export_for_github.py

目标目录已存在时默认中止，避免误覆盖；确认要「同步进已有仓库」时设：
    GITHUB_EXPORT_INTO=1 python tools/export_for_github.py
（该模式下只覆盖同名文件，不动 .git，也不删除目标里的其它文件。）
"""
import os
import shutil
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
SRC = os.path.abspath(os.environ.get("MODELMUX_SRC") or os.path.dirname(HERE))
DST = os.path.abspath(os.environ.get("GITHUB_EXPORT_DIR")
                      or os.path.join(os.path.dirname(SRC), "modelmux-github"))
INTO = os.environ.get("GITHUB_EXPORT_INTO") == "1"

# 根目录下的单文件
ROOT_FILES = ["go.mod", "go.sum", "main.go", "app.ico", "appres.syso",
              ".gitignore", "README.md", "LICENSE", "CHANGELOG.md"]

# 目录 + 允许的扩展名（None = 允许全部，EXCLUDE_NAMES 仍生效）
DIRS = {
    "internal": None,
    "winres": None,
    "tools": {".py", ".vbs", ".go", ".mod", ".sum", ".md"},
    "_e2e": {".py"},
}

EXCLUDE_NAMES = {".tmp", "ModelMux.exe", "ModelMux_new.exe~", "_t.exe"}
EXCLUDE_DIRS = {"__pycache__", "home", "gui_home", "ui_home", "claim_home", "shots"}


def main():
    if os.path.isdir(DST) and not INTO:
        sys.exit("ABORT: 目标目录已存在，请先确认后删除：%s\n"
                 "（若确实要同步进已有仓库，设 GITHUB_EXPORT_INTO=1）" % DST)

    copied, skipped = [], []
    os.makedirs(DST, exist_ok=True)

    for fn in ROOT_FILES:
        s = os.path.join(SRC, fn)
        if os.path.exists(s):
            shutil.copy2(s, os.path.join(DST, fn))
            copied.append(fn)
        elif fn not in (".gitignore", "README.md", "LICENSE", "CHANGELOG.md"):
            skipped.append(fn + " (源缺失)")

    for d, exts in DIRS.items():
        sroot = os.path.join(SRC, d)
        if not os.path.isdir(sroot):
            skipped.append(d + " (源缺失)")
            continue
        for dirpath, dirnames, filenames in os.walk(sroot):
            dirnames[:] = [x for x in dirnames if x not in EXCLUDE_DIRS]
            for fn in filenames:
                rel = os.path.relpath(os.path.join(dirpath, fn), SRC)
                if fn in EXCLUDE_NAMES or fn.startswith("."):
                    skipped.append(rel)
                    continue
                if exts is not None and os.path.splitext(fn)[1].lower() not in exts:
                    skipped.append(rel)
                    continue
                dstp = os.path.join(DST, rel)
                os.makedirs(os.path.dirname(dstp), exist_ok=True)
                shutil.copy2(os.path.join(dirpath, fn), dstp)
                copied.append(rel)

    print("源   :", SRC)
    print("目标 :", DST)
    print("复制 :", len(copied), "个文件")
    print("跳过 :", len(skipped), "个（敏感/缓存/产物）")
    for s in sorted(skipped):
        print("   -", s)


if __name__ == "__main__":
    main()
