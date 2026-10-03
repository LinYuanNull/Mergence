#!/usr/bin/env python3
"""全量快照备份：源码 + 配置 + 编译产物。

用途是「文件别丢」，所以**只排除可再生的运行期缓存**，其余一律原样打包。

    python tools/release/backup_project.py          # 输出到 <仓库上级>/_backups/
    BACKUP_DIR=D:/somewhere python tools/release/backup_project.py
    MODELMUX_SRC=D:/path/to/modelmux python tools/release/backup_project.py

路径全部可推导/可覆盖，不写死本机绝对路径。
"""
import hashlib
import io
import os
import sys
import time
import zipfile

HERE = os.path.dirname(os.path.abspath(__file__))
# 本脚本在 tools/release/ 下，仓库根是 HERE 的上两级。
SRC = os.path.abspath(os.environ.get("MODELMUX_SRC") or os.path.dirname(os.path.dirname(HERE)))

# 默认放在仓库的同级目录，避免备份被一起打包/误删
OUT_DIR = os.path.abspath(os.environ.get("BACKUP_DIR")
                          or os.path.join(os.path.dirname(SRC), "_backups"))

# 名字命中即整棵剪掉
PRUNE_DIRS = {
    "__pycache__",
    # 运行期数据：可重建，且 cache 是几十 MB 的 WebView2 缓存
    os.path.join("data", "cache"),
    os.path.join("data", "logs"),
    os.path.join("data", "instances"),
    # 测试跑出来的隔离根目录（各脚本自己的 MODELMUX_HOME）
    os.path.join("test", "home"),
    os.path.join("test", "gui_home"),
    os.path.join("test", "ui_home"),
    os.path.join("test", "claim_home"),
    os.path.join("test", "shots"),
}
# 文件名命中即跳过
PRUNE_FILES = {"ModelMux_new.exe~", "_t.exe", "mg.pid"}

# 设计文档现在就在仓库内的 docs/ 下，os.walk 会原样打包，无需再单独补。


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

    exe = os.path.join(SRC, "ModelMux.exe")
    exe_hash = sha256(exe) if os.path.exists(exe) else "(缺失)"

    info = [
        "ModelMux 快照",
        "生成时间: %s" % stamp,
        "源目录  : %s" % SRC,
        "编译产物: ModelMux.exe  sha256=%s" % exe_hash,
        "",
        "内容: 源码(src/) + 用户配置(config/) + 编译产物(ModelMux.exe) + 前端真源(web/) + 设计文档(docs/)",
        "已排除(可再生缓存): %s" % ", ".join(sorted(skipped_dirs)),
        "已排除(陈旧/未知二进制): %s" % ", ".join(sorted(skipped_files)),
        "",
        "恢复: 解到任意目录后 `python tools/release/build.py`；",
        "      等价于 `go build -C src -trimpath -ldflags=\"-s -w -H windowsgui\" -o ../ModelMux.exe .`",
        "注意: _ref/ 已移出仓库（内含真实账号 token），本快照不含它。",
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
