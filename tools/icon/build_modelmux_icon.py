"""把 ModelMux 的 PNG logo 转成多尺寸 BMP-ICO + 预览图。

处理链：解码 1440×1440 RGBA → 洪泛抠掉纯白底 → 按内容裁剪 → 预乘盒式缩放
→ 生成 16/32/48/64/128/256 → BMP 编码打包 ICO。

用法:
  python build_modelmux_icon.py <src.rgba 或 .png> <out.ico> [--preview out.png]
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import iconlib as il

SIZES = [16, 32, 48, 64, 128, 256]


def load_source(path):
    if path.endswith(".rgba"):
        return 1440, 1440, bytearray(open(path, "rb").read())
    return il.decode_png(open(path, "rb").read())


def build_preview(size_pngs, dest):
    """两行对照：深色底 / 浅色底，各尺寸统一放大到 128px 高，便于肉眼判清晰度。"""
    CELL, PAD, GAP = 128, 14, 10
    rows = [(38, 38, 38), (245, 246, 247)]   # 深底、浅底
    W = PAD * 2 + CELL * len(size_pngs) + GAP * (len(size_pngs) - 1)
    H = PAD * 2 + CELL * len(rows) + GAP * (len(rows) - 1)
    buf = bytearray()
    for _ in range(W * H):
        buf += b"\xe8\xea\xec\xff"

    for ri, bg in enumerate(rows):
        y0 = PAD + ri * (CELL + GAP)
        x0 = PAD
        for _size, png in size_pngs:
            w, h, rgba = il.decode_png(png)
            # 最近邻放大到 CELL×CELL（看清像素，不做平滑美化）
            for y in range(CELL):
                sy = y * h // CELL
                for x in range(CELL):
                    sx = x * w // CELL
                    o = (sy * w + sx) * 4
                    a = rgba[o + 3] / 255.0
                    do = ((y0 + y) * W + (x0 + x)) * 4
                    for k in range(3):
                        buf[do + k] = int(round(rgba[o + k] * a + bg[k] * (1 - a)))
                    buf[do + 3] = 255
            x0 += CELL + GAP
    open(dest, "wb").write(il.encode_png(W, H, buf))
    print("预览图: %s (%dx%d)" % (dest, W, H))


def main():
    src, dst = sys.argv[1], sys.argv[2]
    preview = None
    if "--preview" in sys.argv:
        preview = sys.argv[sys.argv.index("--preview") + 1]

    w, h, rgba = load_source(src)
    print("源: %dx%d" % (w, h))

    rgba, removed = il.remove_flat_bg(rgba, w, h, tol=24)
    print("抠掉背景像素: %d (%.1f%%)" % (removed, removed / (w * h) * 100))

    box = il.bbox_alpha(rgba, w, h)
    print("内容包围盒: x %d..%d  y %d..%d  (%dx%d)"
          % (box[0], box[2], box[1], box[3], box[2] - box[0] + 1, box[3] - box[1] + 1))

    pairs = []
    for s in SIZES:
        pairs.append((s, il.fit_into_square(rgba, w, h, s, pad_ratio=0.06)))
        print("  生成 %3dpx" % s)

    ico = il.build_ico_bmp(pairs)
    open(dst, "wb").write(ico)
    print("写出 %s (%d 字节, BMP 编码, %d 个尺寸)" % (dst, len(ico), len(pairs)))

    if preview:
        build_preview([(s, il.encode_png(s, s, r)) for s, r in pairs], preview)


if __name__ == "__main__":
    main()
