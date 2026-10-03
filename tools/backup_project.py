#!/usr/bin/env python3
"""全量快照备份：源码 + 配置 + 编译产物。

用途是「文件别丢」，所以**只排除可再生的运行期缓存**，其余一律原样打包。

    python tools/backup_project.py                  # 输出到 <仓库上级>/_backups/
    BACKUP_DIR=D:/somewhere python tools/backup_project.py
    MODELMUX_SRC=D:/path/to/modelmux python tools/backup_project.py

路径全部可推导/可覆盖，不写死本机绝对路径。
"""
import hashlib
import io
import os
import sys
import time
import zipfile

HERE = os.path.dirname(os.path.abspath(__file__))
SRC = os.path.abspath(os.environ.get("MODELMUX_SRC") or os.path.dirname(HERE))

# 默认放在仓库的同级目录，避免备份被一起打包/误删
OUT_DIR = os.path.abspath(os.environ.get("BACKUP_DIR")
                          or os.path.join(os.path.dirname(SRC), "_backups"))

# 名字命中即整棵剪掉
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

# 同级目录下的设计文档，一并归档到 docs/
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
    stamp = os.environ.get("STAMP") or time.strftime("%Y%m%d-%H%M%S")
    os.makedirs(OUT_DIR, exist_ok=True)
    out = os.path.join(OUT_DIR, "modelmux-full-%s.zip" % stamp)

    files, skipped_dirs, skipped_files = [], [], []
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
            inner = "/".join(x for x in ("modelmux", rel.replace(os.sep, "/"), fn) if x)
            files.append((os.path.join(dirpath, fn), inner))

    for doc in EXTRA_DOCS:
        p = os.path.join(os.path.dirname(SRC), doc)
        if os.path.exists(p):
            files.append((p, "docs/" + doc))

    exe = os.path.join(SRC, "ModelMux.exe")
    exe_hash = sha256(exe) if os.path.exists(exe) else "(缺失)"

    info = [
        "ModelMux 快照",
        "生成时间: %s" % stamp,
        "源目录  : %s" % SRC,
        "编译产物: ModelMux.exe  sha256=%s" % exe_hash,
        "",
        "内容: 源码 + 配置(winres/app.ico/appres.syso/_ref) + 编译产物 + 设计文档",
        "已排除(可再生缓存): %s" % ", ".join(sorted(skipped_dirs)),
        "已排除(陈旧/未知二进制): %s" % ", ".join(sorted(skipped_files)),
        "",
        "恢复: modelmux/ 解到任意目录后 `go build -trimpath -ldflags=\"-s -w -H windowsgui\" -o ModelMux.exe .`",
        "注意: _ref/ 内可能含真实账号 token 与计费数据，仅作本机备份，切勿公开。",
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
    print("排除 :", sorted(skipped_dirs), sorted(skipped_files))


if __name__ == "__main__":
    main()
