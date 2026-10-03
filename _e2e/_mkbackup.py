#!/usr/bin/env python3
"""备份 1/2：防丢失快照。

内容 = 源码 + 配置 + 编译产物（ModelMux.exe）。
**只排除可再生的运行期缓存**，其余一律原样打包，因为这一份的目的是「文件别丢」。

刻意排除的只有三类：
  1. _e2e 下测试跑出来的运行期目录（Edge/WebView2 缓存、隔离 HOME、截图）——共 51MB，纯缓存；
  2. __pycache__（py_compile 产物）；
  3. 两个陈旧/来历不明的二进制：ModelMux_new.exe~（编辑器备份残留）、_t.exe（未知测试产物）。
     —— 注意：这两项**未**纳入，如需一并保留请明确告知。
"""
import hashlib
import io
import os
import sys
import zipfile

SRC = r"D:\AiWork\WorkBuddy\modelmux"
PARENT = r"D:\AiWork\WorkBuddy"
OUT_DIR = r"D:\AiWork\_backups"
STAMP = sys.argv[1]  # 由调用方用 date 生成，脚本内不自算时间

# 目录名命中即整棵剪掉
PRUNE_DIRS = {
    "__pycache__",
    os.path.join("_e2e", "home"),
    os.path.join("_e2e", "gui_home"),
    os.path.join("_e2e", "ui_home"),
    os.path.join("_e2e", "claim_home"),
    os.path.join("_e2e", "shots"),
}
# 文件名命中即跳过
PRUNE_FILES = {"ModelMux_new.exe~", "_t.exe"}

# 父目录下的设计文档，一并归档到 docs/
EXTRA_DOCS = ["ModelMux-设计方案.md", "ModelMux-面板整合与桌面化.md"]


def sha256(path, buf=1 << 20):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while True:
            b = f.read(buf)
            if not b:
                break
            h.update(b)
    return h.hexdigest()


def main():
    os.makedirs(OUT_DIR, exist_ok=True)
    out = os.path.join(OUT_DIR, "modelmux-full-%s.zip" % STAMP)

    files = []          # (磁盘绝对路径, zip 内相对路径)
    skipped_dirs = []   # 记录被剪掉的目录及其体量，写进 INFO 好交代
    skipped_files = []

    for dirpath, dirnames, filenames in os.walk(SRC):
        rel = os.path.relpath(dirpath, SRC)
        rel = "" if rel == "." else rel
        keep = []
        for d in dirnames:
            cand = os.path.join(rel, d) if rel else d
            if cand in PRUNE_DIRS or d == "__pycache__":
                skipped_dirs.append(cand)
                continue
            keep.append(d)
        dirnames[:] = keep
        for fn in filenames:
            if fn in PRUNE_FILES:
                skipped_files.append(os.path.join(rel, fn) if rel else fn)
                continue
            abs_p = os.path.join(dirpath, fn)
            inner = os.path.join("modelmux", rel, fn) if rel else os.path.join("modelmux", fn)
            files.append((abs_p, inner.replace(os.sep, "/")))

    for doc in EXTRA_DOCS:
        p = os.path.join(PARENT, doc)
        if os.path.exists(p):
            files.append((p, "docs/" + doc))

    exe = os.path.join(SRC, "ModelMux.exe")
    exe_hash = sha256(exe) if os.path.exists(exe) else "(缺失)"

    info = [
        "ModelMux 防丢失快照",
        "生成时间: %s" % STAMP,
        "源目录  : %s" % SRC,
        "编译产物: ModelMux.exe  sha256=%s" % exe_hash,
        "",
        "内容: 源码 + 配置(winres/app.ico/appres.syso/_ref 数据转储) + 编译产物 + 设计文档",
        "",
        "已排除(可再生缓存): %s" % ", ".join(sorted(skipped_dirs)),
        "已排除(陈旧/未知二进制): %s" % ", ".join(sorted(skipped_files)),
        "",
        "恢复: 整个 modelmux/ 解回任意目录后 `go build -trimpath -ldflags=\"-s -w -H windowsgui\" -o ModelMux.exe .`",
        "注意: _ref/ 内含真实账号 token 与计费数据，仅作本机备份，切勿公开或提交。",
    ]

    total = 0
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED, compresslevel=6) as z:
        z.writestr("BACKUP-INFO.txt", "\n".join(info) + "\n")
        for abs_p, inner in files:
            z.write(abs_p, inner)
            total += os.path.getsize(abs_p)

    print("输出 :", out)
    print("条目 :", len(files), "个文件")
    print("原始 : %.1f MB" % (total / 1048576))
    print("压缩 : %.1f MB" % (os.path.getsize(out) / 1048576))
    print("排除 :", sorted(skipped_dirs))
    print("     ", sorted(skipped_files))


if __name__ == "__main__":
    main()
