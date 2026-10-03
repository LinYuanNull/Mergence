"""解析 PE 文件的 .rsrc 节，列出所有资源类型与条目数。

用于验证图标 / 版本信息 / 清单是否真的编译进了 exe。
用法: python pe_resources.py <exe>
"""
import struct
import sys

RT_NAMES = {
    1: "RT_CURSOR", 2: "RT_BITMAP", 3: "RT_ICON", 4: "RT_MENU", 5: "RT_DIALOG",
    6: "RT_STRING", 7: "RT_FONTDIR", 8: "RT_FONT", 9: "RT_ACCELERATOR",
    10: "RT_RCDATA", 11: "RT_MESSAGETABLE", 12: "RT_GROUP_CURSOR",
    14: "RT_GROUP_ICON", 16: "RT_VERSION", 24: "RT_MANIFEST",
}


def main(path):
    d = open(path, "rb").read()
    if d[:2] != b"MZ":
        print("not a PE")
        return 1
    pe = struct.unpack("<I", d[0x3C:0x40])[0]
    if d[pe:pe + 4] != b"PE\0\0":
        print("bad PE signature")
        return 1

    coff = pe + 4
    nsec = struct.unpack("<H", d[coff + 2:coff + 4])[0]
    opt_size = struct.unpack("<H", d[coff + 16:coff + 18])[0]
    opt = coff + 20
    magic = struct.unpack("<H", d[opt:opt + 2])[0]
    dd_off = opt + (112 if magic == 0x20B else 96)
    rsrc_rva, rsrc_size = struct.unpack("<II", d[dd_off + 2 * 8:dd_off + 2 * 8 + 8])
    print("PE32%s  段数=%d  .rsrc RVA=0x%X 大小=%d"
          % ("+" if magic == 0x20B else "", nsec, rsrc_rva, rsrc_size))
    if rsrc_rva == 0:
        print("没有资源目录")
        return 1

    # 段表 -> 把 RVA 转成文件偏移
    secs = []
    sh = opt + opt_size
    for i in range(nsec):
        o = sh + i * 40
        name = d[o:o + 8].rstrip(b"\0").decode("ascii", "replace")
        vsize, vaddr, rsize, raw = struct.unpack("<IIII", d[o + 8:o + 24])
        secs.append((name, vaddr, vsize, raw, rsize))

    def rva2off(rva):
        for _n, va, vs, raw, rs in secs:
            if va <= rva < va + max(vs, rs):
                return raw + (rva - va)
        return None

    base = rva2off(rsrc_rva)
    if base is None:
        print("无法定位 .rsrc 文件偏移")
        return 1

    def walk(off, level, path):
        n_named, n_id = struct.unpack("<HH", d[off + 12:off + 16])
        total = n_named + n_id
        for i in range(total):
            e = off + 16 + i * 8
            name_val, off_val = struct.unpack("<II", d[e:e + 8])
            if level == 0:
                label = RT_NAMES.get(name_val, "type_%d" % name_val)
            elif level == 1:
                label = str(name_val)
            else:
                label = "lang_0x%X" % name_val
            newpath = path + [label]
            if off_val & 0x80000000:  # 子目录
                walk(base + (off_val & 0x7FFFFFFF), level + 1, newpath)
            else:
                de = base + off_val
                data_rva, size = struct.unpack("<II", d[de:de + 8])
                if level == 0:
                    print("  %-16s 条目=" % label, end="")
                    print(path and "" or "", end="")
                    print("(见下)")
                if level == 2:
                    print("      %s  数据大小=%d 字节" % (" / ".join(newpath), size))

    print("\n资源类型：")
    walk(base, 0, [])
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1]))
