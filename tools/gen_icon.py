"""生成 WorkBuddy2API 应用图标 (app.ico)。

纯标准库实现（zlib + struct），不依赖 Pillow。
绘制内容：圆角方形底 + 三条白色横杠（表征"面板/清单"），
先用 SS 倍超采样再盒式降采样，保证小尺寸下边缘干净。

用法: python gen_icon.py <输出路径.ico>
"""
import os
import struct
import sys
import zlib

SS = 4  # 超采样倍数
SIZES = [16, 24, 32, 48, 64, 128, 256]

BG = (0x18, 0x5F, 0xA5, 255)     # c-blue 600
BAR = (0xFF, 0xFF, 0xFF, 255)    # 白
# 单位坐标 (0..1) 的横杠：x0, y0, x1, y1, 圆角
BARS = [
    (0.24, 0.295, 0.760, 0.395, 0.050),
    (0.24, 0.450, 0.760, 0.550, 0.050),
    (0.24, 0.605, 0.575, 0.705, 0.050),
]


def in_rounded_rect(px, py, x0, y0, x1, y1, r):
    """点是否落在圆角矩形内。"""
    if x0 <= px <= x1 and y0 + r <= py <= y1 - r:
        return True
    if y0 <= py <= y1 and x0 + r <= px <= x1 - r:
        return True
    for cx, cy in ((x0 + r, y0 + r), (x1 - r, y0 + r), (x0 + r, y1 - r), (x1 - r, y1 - r)):
        if (px - cx) ** 2 + (py - cy) ** 2 <= r * r:
            return True
    return False


def render(size):
    """渲染 size×size RGBA 像素（bytes）。"""
    n = size * SS
    buf = bytearray(n * n * 4)

    bg = (0.06, 0.06, 0.94, 0.94, 0.20)
    bars = BARS

    for y in range(n):
        uy = (y + 0.5) / n
        for x in range(n):
            ux = (x + 0.5) / n
            color = None
            for b in bars:
                if in_rounded_rect(ux, uy, *b):
                    color = BAR
                    break
            if color is None and in_rounded_rect(ux, uy, *bg):
                color = BG
            if color is None:
                continue
            o = (y * n + x) * 4
            buf[o:o + 4] = bytes(color)

    # 盒式降采样
    out = bytearray(size * size * 4)
    area = SS * SS
    for y in range(size):
        for x in range(size):
            r = g = b = a = 0
            for dy in range(SS):
                row = (y * SS + dy) * n
                for dx in range(SS):
                    o = (row + x * SS + dx) * 4
                    pa = buf[o + 3]
                    r += buf[o] * pa
                    g += buf[o + 1] * pa
                    b += buf[o + 2] * pa
                    a += pa
            o = (y * size + x) * 4
            if a == 0:
                out[o:o + 4] = b"\x00\x00\x00\x00"
            else:
                out[o] = r // a
                out[o + 1] = g // a
                out[o + 2] = b // a
                out[o + 3] = a // area
    return bytes(out)


def png_encode(size, rgba):
    raw = bytearray()
    stride = size * 4
    for y in range(size):
        raw.append(0)  # filter: none
        raw += rgba[y * stride:(y + 1) * stride]

    def chunk(typ, data):
        return (struct.pack(">I", len(data)) + typ + data
                + struct.pack(">I", zlib.crc32(typ + data) & 0xFFFFFFFF))

    return (b"\x89PNG\r\n\x1a\n"
            + chunk(b"IHDR", struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(bytes(raw), 9))
            + chunk(b"IEND", b""))


def build_ico():
    entries = []
    for s in SIZES:
        entries.append((s, png_encode(s, render(s))))
    # ICONDIR
    out = struct.pack("<HHH", 0, 1, len(entries))
    offset = 6 + 16 * len(entries)
    dirs = b""
    for s, png in entries:
        w = 0 if s >= 256 else s
        dirs += struct.pack("<BBBBHHII", w, w, 0, 0, 1, 32, len(png), offset)
        offset += len(png)
    out += dirs
    for _, png in entries:
        out += png
    return out


if __name__ == "__main__":
    dest = sys.argv[1] if len(sys.argv) > 1 else "app.ico"
    data = build_ico()
    with open(dest, "wb") as f:
        f.write(data)
    print("wrote %s (%d bytes, sizes=%s)" % (dest, len(data), SIZES))
