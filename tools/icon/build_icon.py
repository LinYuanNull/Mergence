"""从 WorkBuddy 官方图标生成 WorkBuddy2API 的应用图标。

素材来源：WorkBuddy 安装目录自带的 resources/icon.ico —— 里面已经是官方渲染好的
11 个尺寸（16/20/24/28/32/40/48/64/96/128/256），全部 PNG 编码，质量优于自己缩放。
本脚本直接复用这些尺寸，可选叠加一个「API 控制台」角标，最后重新打包成 ICO。

用法:
  python build_icon.py <官方icon.ico> <输出.ico> [--badge] [--preview 预览.png]
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import iconlib as il

INK = (6, 50, 42)      # 深墨绿角标底色
WHITE = (255, 255, 255)


BADGE_POS = "tl"  # tl = 左上角（右下角会盖住吉祥物的脸）


def _badge_color(px, py, S):
    """返回该点应被角标覆盖的颜色，None 表示保持原图。"""
    if BADGE_POS == "tl":
        cx, cy = 0.298 * S, 0.298 * S
    else:
        cx, cy = 0.752 * S, 0.752 * S
    r = 0.156 * S
    ring = 0.024 * S
    d2 = (px - cx) ** 2 + (py - cy) ** 2
    if d2 > (r + ring) ** 2:
        return None
    # 深色圆内画白色提示符 ">_"（终端 / API 语义）
    if d2 <= r * r:
        w = 0.038 * S
        p1 = (cx - 0.064 * S, cy - 0.052 * S)
        pm = (cx - 0.004 * S, cy)
        p3 = (cx - 0.064 * S, cy + 0.052 * S)
        u1 = (cx + 0.024 * S, cy + 0.052 * S)
        u2 = (cx + 0.090 * S, cy + 0.052 * S)
        for a, b in ((p1, pm), (pm, p3), (u1, u2)):
            if il.distance_to_segment(px, py, a[0], a[1], b[0], b[1]) <= w / 2:
                return WHITE
        return INK
    return WHITE  # 白色外环


def apply_badge(rgba, S, ss=4):
    """在 RGBA 缓冲上叠加角标（ss×ss 超采样抗锯齿）。"""
    n = ss * ss
    for y in range(S):
        for x in range(S):
            hits = 0
            sr = sg = sb = 0
            for sy in range(ss):
                for sx in range(ss):
                    c = _badge_color(x + (sx + 0.5) / ss, y + (sy + 0.5) / ss, S)
                    if c is not None:
                        hits += 1
                        sr += c[0]
                        sg += c[1]
                        sb += c[2]
            if not hits:
                continue
            il.blend_px(rgba, S, S, x, y,
                        (sr // hits, sg // hits, sb // hits), hits / n)


def make_preview(entries, zoom_pairs, dest, pad=10, bg=(238, 240, 242)):
    """把若干尺寸放大拼成一张预览图，便于肉眼检查细节。"""
    cells = []
    for size, png in entries:
        if size not in dict(zoom_pairs):
            continue
        z = dict(zoom_pairs)[size]
        w, h, rgba = il.decode_png(png)
        cells.append((size, w * z, h * z, _nearest_zoom(rgba, w, h, z)))

    W = sum(c[1] for c in cells) + pad * (len(cells) + 1)
    H = max(c[2] for c in cells) + pad * 2
    buf = bytearray()
    for y in range(H):
        for x in range(W):
            buf += bytes(bg) + b"\xff"
    x0 = pad
    for _size, cw, ch, crgba in cells:
        y0 = pad
        for y in range(ch):
            for x in range(cw):
                o = (y * cw + x) * 4
                a = crgba[o + 3] / 255.0
                if a <= 0:
                    continue
                do = ((y0 + y) * W + (x0 + x)) * 4
                for k in range(3):
                    buf[do + k] = int(round(crgba[o + k] * a + buf[do + k] * (1 - a)))
        x0 += cw + pad
    open(dest, "wb").write(il.encode_png(W, H, buf))


def _nearest_zoom(rgba, w, h, z):
    out = bytearray(w * z * h * z * 4)
    for y in range(h * z):
        sy = y // z
        for x in range(w * z):
            sx = x // z
            so = (sy * w + sx) * 4
            do = (y * w * z + x) * 4
            out[do:do + 4] = rgba[so:so + 4]
    return out


def main():
    src = sys.argv[1]
    dst = sys.argv[2]
    with_badge = "--badge" in sys.argv
    preview = None
    if "--preview" in sys.argv:
        preview = sys.argv[sys.argv.index("--preview") + 1]

    entries = il.read_ico(src)
    print("源图标尺寸:", [s for s, _ in entries])

    out_entries = []
    for size, png in entries:
        if not with_badge:
            out_entries.append((size, png))
            continue
        w, h, rgba = il.decode_png(png)
        apply_badge(rgba, w)
        out_entries.append((size, il.encode_png(w, h, rgba)))

    open(dst, "wb").write(il.build_ico(out_entries))
    print("写出 %s (%d 字节, %d 个尺寸)%s"
          % (dst, os.path.getsize(dst), len(out_entries), " [带角标]" if with_badge else ""))

    if preview:
        zoom = {16: 4, 32: 4, 48: 4, 64: 3, 128: 2, 256: 1}
        make_preview(out_entries, zoom.items(), preview)
        print("预览图:", preview)


if __name__ == "__main__":
    main()
