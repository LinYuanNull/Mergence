#!/usr/bin/env python3
"""生成「可提交 GitHub」的干净副本。

原则：**白名单式**复制（deny-by-default）。只列进来的才出去，
避免将来新增文件时不小心把数据/产物带进公开仓库。

绝不外带的：
  _ref/        接口响应转储，内含真实账号 token、user_plan_id、计费额度
  internal/web/.tmp  残留配置文件，内含真实 access_key（且没有任何代码引用它）
  backups/ _backup/  历史快照与数据包
  任何 *.exe / *.exe~  编译产物与编辑器残留
  _e2e 运行期目录（Edge/WebView2 缓存、隔离 HOME、截图）
"""
import io
import os
import shutil
import sys

SRC = r"D:\AiWork\WorkBuddy\modelmux"
DST = r"D:\AiWork\modelmux-github"

# 根目录下的单文件
ROOT_FILES = ["go.mod", "go.sum", "main.go", "app.ico", "appres.syso"]

# 目录 + 允许的扩展名（None = 允许全部，但下面 EXCLUDE_NAMES 仍生效）
DIRS = {
    "internal": None,
    "winres": None,
    "tools": {".py", ".vbs", ".go", ".mod", ".sum", ".md"},
    "_e2e": {".py"},
}

# 任何层级命中即跳过
EXCLUDE_NAMES = {".tmp", "ModelMux.exe", "ModelMux_new.exe~", "_t.exe"}
EXCLUDE_DIRS = {"__pycache__", "home", "gui_home", "ui_home", "claim_home", "shots"}


def main():
    if os.path.isdir(DST):
        sys.exit("ABORT: 目标目录已存在，请先手工确认后删除：%s" % DST)

    copied, skipped = [], []
    os.makedirs(DST)

    for fn in ROOT_FILES:
        s = os.path.join(SRC, fn)
        if os.path.exists(s):
            shutil.copy2(s, os.path.join(DST, fn))
            copied.append(fn)
        else:
            skipped.append(fn + " (源缺失)")

    for d, exts in DIRS.items():
        sroot = os.path.join(SRC, d)
        if not os.path.isdir(sroot):
            skipped.append(d + " (源缺失)")
            continue
        for dirpath, dirnames, filenames in os.walk(sroot):
            dirnames[:] = [x for x in dirnames if x not in EXCLUDE_DIRS]
            for fn in filenames:
                if fn in EXCLUDE_NAMES or fn.startswith("."):
                    skipped.append(os.path.relpath(os.path.join(dirpath, fn), SRC))
                    continue
                if exts is not None and os.path.splitext(fn)[1].lower() not in exts:
                    skipped.append(os.path.relpath(os.path.join(dirpath, fn), SRC))
                    continue
                rel = os.path.relpath(os.path.join(dirpath, fn), SRC)
                dstp = os.path.join(DST, rel)
                os.makedirs(os.path.dirname(dstp), exist_ok=True)
                shutil.copy2(os.path.join(dirpath, fn), dstp)
                copied.append(rel)

    print("目标 :", DST)
    print("复制 :", len(copied), "个文件")
    for c in sorted(copied):
        print("   +", c)
    print("跳过 :", len(skipped), "个（含敏感/缓存/产物）")
    for s in sorted(skipped):
        print("   -", s)


if __name__ == "__main__":
    main()
