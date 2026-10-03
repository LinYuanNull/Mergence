"""生成两个图标方案的对比预览图（多尺寸 1:1 排列）。

用法: python compare_icons.py <A.ico> <B.ico> <输出.png>
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import iconlib as il

SIZES = [256, 128, 64, 48, 32, 16]
PAD = 12
ROW_GAP = 18
BG = (245, 246, 247)


def main():
    a, b, dest = sys.argv[1], sys.argv[2], sys.argv[3]
    rows = []
    for path in (a, b):
        ents = dict(il.read_ico(path))
        rows.append([(s, ents[s]) for s in SIZES if s in ents])

    row_w = sum(s for s, _ in rows[0]) + PAD * (len(rows[0]) + 1)
    W = row_w
    H = PAD + sum(256 for _ in rows) + ROW_GAP * (len(rows) - 1) + PAD

    buf = bytearray()
    for _y in range(H):
        for _x in range(W):
            buf += bytes(BG) + b"\xff"

    y0 = PAD
    for row in rows:
        x0 = PAD
        for size, png in row:
            w, h, rgba = il.decode_png(png)
            for y in range(h):
                for x in range(w):
                    o = (y * w + x) * 4
                    al = rgba[o + 3] / 255.0
                    if al <= 0:
                        continue
                    do = ((y0 + y) * W + (x0 + x)) * 4
                    for k in range(3):
                        buf[do + k] = int(round(rgba[o + k] * al + buf[do + k] * (1 - al)))
            x0 += size + PAD
        y0 += 256 + ROW_GAP

    open(dest, "wb").write(il.encode_png(W, H, buf))
    print("对比图写出:", dest, "%dx%d" % (W, H))


if __name__ == "__main__":
    main()
