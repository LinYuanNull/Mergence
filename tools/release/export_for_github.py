#!/usr/bin/env python3
"""导出「可提交 GitHub」的干净副本。

原则：**白名单式**复制（deny-by-default）。只列进来的才出去，
避免将来新增文件时不小心把数据或构建产物带进公开仓库。

绝不外带的：
  config/ data/       本机运行期产物 —— 配置含真实 access_key 与渠道密钥，data 含日志与用量
  *.exe / *.exe~      编译产物与编辑器残留
  test 运行期目录      Edge/WebView2 缓存、隔离 HOME、截图
  历史归档            _ref/（接口转储，可能含真实 token）与 backups/ 已移出仓库、只存在于仓库之外

    python tools/release/export_for_github.py
    GITHUB_EXPORT_DIR=D:/tmp/export python tools/release/export_for_github.py

目标目录已存在时默认中止，避免误覆盖；确认要「同步进已有仓库」时设：
    GITHUB_EXPORT_INTO=1 python tools/release/export_for_github.py
（该模式下只覆盖同名文件，不动 .git；**源里已删除的文件会从目标里一并清掉**——
因为只增不删会让「已从白名单撤下的文件」永久留在公开仓库里。要保留它们设
GITHUB_EXPORT_NO_PRUNE=1。清理范围 = DIRS 列出的目录 + 目标根目录；
根目录按同一份白名单收敛，但点开头的隐藏项（.git / .github / .gitattributes）一律保留。）
"""
import os
import shutil
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
# 本脚本在 tools/release/ 下，仓库根是 HERE 的上两级。
SRC = os.path.abspath(os.environ.get("MODELMUX_SRC") or os.path.dirname(os.path.dirname(HERE)))
DST = os.path.abspath(os.environ.get("GITHUB_EXPORT_DIR")
                      or os.path.join(os.path.dirname(SRC), "modelmux-github"))
INTO = os.environ.get("GITHUB_EXPORT_INTO") == "1"
NO_PRUNE = os.environ.get("GITHUB_EXPORT_NO_PRUNE") == "1"

# 根目录下的单文件（源码在 src/、文档在 docs/，根目录只留这几样）
ROOT_FILES = [".gitignore", "README.md", "LICENSE"]

# 目录 + 允许的扩展名（None = 允许全部，EXCLUDE_NAMES 仍生效）
DIRS = {
    "src": None,                            # Go 源码 + 构建资源（app.ico / appres.syso / winres）
    "web": {".html", ".js", ".css"},        # 面板前端（真源）
    "docs": {".md"},
    "tools": {".py", ".vbs", ".go", ".mod", ".sum", ".md"},
    "test": {".py"},
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
        elif fn not in (".gitignore", "README.md", "LICENSE"):
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

    # 清理「源里已经没有了、但目标里还留着」的文件。
    # 只增不删的后果：从白名单撤下的文件会永久留在公开仓库里，而它可能正含
    # 本机路径或残留配置——正是这次踩到的坑。清理不碰 .git；范围见下。
    pruned = []
    want = set(copied)
    if INTO and not NO_PRUNE:
        for d in DIRS:
            droot = os.path.join(DST, d)
            if not os.path.isdir(droot):
                continue
            for dirpath, dirnames, filenames in os.walk(droot, topdown=False):
                dirnames[:] = [x for x in dirnames if x != ".git"]
                for fn in filenames:
                    rel = os.path.relpath(os.path.join(dirpath, fn), DST)
                    if rel not in want:
                        os.remove(os.path.join(dirpath, fn))
                        pruned.append(rel)
                if dirpath != droot and not os.listdir(dirpath):
                    os.rmdir(dirpath)

        # 根目录同样按白名单收敛。留在这里的旧结构最容易被忽略：
        # 例如 main.go / go.mod 搬进 src/ 后，目标根目录那几份旧文件不在任何
        # DIRS 里，上面的清理永远碰不到，于是它们会一直留在公开仓库。
        # 判据与 DIRS 一致（白名单）：不在 ROOT_FILES、不是 DIRS 的目录、
        # 也不是点开头的隐藏项（.git / .github / .gitattributes）——一律删。
        for fn in sorted(os.listdir(DST)):
            if fn.startswith(".") or fn in ROOT_FILES or fn in DIRS:
                continue
            p = os.path.join(DST, fn)
            is_dir = os.path.isdir(p)
            if is_dir:
                shutil.rmtree(p)
            else:
                os.remove(p)
            pruned.append(fn + "/ (根目录)" if is_dir else fn + " (根目录)")

    print("源   :", SRC)
    print("目标 :", DST)
    print("复制 :", len(copied), "个文件")
    print("跳过 :", len(skipped), "个（敏感/缓存/产物）")
    for s in sorted(skipped):
        print("   -", s)
    if INTO and not NO_PRUNE:
        print("清理 :", len(pruned), "个（源里已删除，目标里清掉）")
        for s in sorted(pruned):
            print("   -", s)


if __name__ == "__main__":
    main()
