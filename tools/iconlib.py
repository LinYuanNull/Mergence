"""纯标准库图像工具：PNG 解码/编码、ICO 读写、抗锯齿绘制。

本机 pip 装不了 Pillow（索引拉不到包），所以这里用 zlib + struct 自己实现。
只支持实际用到的格式：8 位深、非隔行、颜色类型 0/2/3/4/6。
"""
import struct
import zlib

# ---------------------------------------------------------------- PNG 解码


def decode_png(data):
    """解码 PNG，返回 (w, h, bytearray RGBA)。"""
    if data[:8] != b"\x89PNG\r\n\x1a\n":
        raise ValueError("not a PNG")
    pos = 8
    idat = bytearray()
    w = h = bd = ct = None
    plte = None
    trns = None
    while pos < len(data):
        ln = struct.unpack(">I", data[pos:pos + 4])[0]
        typ = data[pos + 4:pos + 8]
        chunk = data[pos + 8:pos + 8 + ln]
        pos += 12 + ln
        if typ == b"IHDR":
            w, h, bd, ct, _comp, _filt, inter = struct.unpack(">IIBBBBB", chunk)
            if inter:
                raise ValueError("interlaced PNG not supported")
        elif typ == b"PLTE":
            plte = chunk
        elif typ == b"tRNS":
            trns = chunk
        elif typ == b"IDAT":
            idat += chunk
        elif typ == b"IEND":
            break
    if bd != 8:
        raise ValueError("only 8-bit PNG supported, got %s" % bd)

    raw = zlib.decompress(bytes(idat))
    ch = {0: 1, 2: 3, 3: 1, 4: 2, 6: 4}[ct]
    stride = w * ch
    out = bytearray(h * stride)
    prev = bytearray(stride)
    p = 0
    for y in range(h):
        f = raw[p]
        p += 1
        line = bytearray(raw[p:p + stride])
        p += stride
        if f == 1:
            for i in range(ch, stride):
                line[i] = (line[i] + line[i - ch]) & 255
        elif f == 2:
            for i in range(stride):
                line[i] = (line[i] + prev[i]) & 255
        elif f == 3:
            for i in range(stride):
                a = line[i - ch] if i >= ch else 0
                line[i] = (line[i] + ((a + prev[i]) >> 1)) & 255
        elif f == 4:
            for i in range(stride):
                a = line[i - ch] if i >= ch else 0
                b = prev[i]
                c = prev[i - ch] if i >= ch else 0
                pa, pb, pc = abs(b - c), abs(a - c), abs(a + b - 2 * c)
                pr = a if (pa <= pb and pa <= pc) else (b if pb <= pc else c)
                line[i] = (line[i] + pr) & 255
        out[y * stride:(y + 1) * stride] = line
        prev = line

    if ct == 6:
        return w, h, out
    rgba = bytearray(w * h * 4)
    if ct == 2:
        for i in range(w * h):
            o, s = i * 4, i * 3
            rgba[o:o + 3] = out[s:s + 3]
            rgba[o + 3] = 255
    elif ct == 0:
        for i in range(w * h):
            v = out[i]
            rgba[i * 4:i * 4 + 3] = bytes((v, v, v))
            rgba[i * 4 + 3] = 255
    elif ct == 4:
        for i in range(w * h):
            v, a = out[i * 2], out[i * 2 + 1]
            rgba[i * 4:i * 4 + 3] = bytes((v, v, v))
            rgba[i * 4 + 3] = a
    elif ct == 3:
        for i in range(w * h):
            idx = out[i]
            rgba[i * 4:i * 4 + 3] = plte[idx * 3:idx * 3 + 3]
            rgba[i * 4 + 3] = trns[idx] if (trns and idx < len(trns)) else 255
    else:
        raise ValueError("unsupported color type %s" % ct)
    return w, h, rgba


# ---------------------------------------------------------------- PNG 编码


def encode_png(w, h, rgba):
    raw = bytearray()
    stride = w * 4
    for y in range(h):
        raw.append(0)  # filter: none
        raw += rgba[y * stride:(y + 1) * stride]

    def chunk(typ, data):
        return (struct.pack(">I", len(data)) + typ + data
                + struct.pack(">I", zlib.crc32(typ + data) & 0xFFFFFFFF))

    return (b"\x89PNG\r\n\x1a\n"
            + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 6, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(bytes(raw), 9))
            + chunk(b"IEND", b""))


# ---------------------------------------------------------------- ICO 读写


def read_ico(path):
    """读取 ICO，返回 [(size, png_bytes)]，只取 PNG 载荷的条目。"""
    d = open(path, "rb").read()
    _res, typ, n = struct.unpack("<HHH", d[:6])
    if typ != 1:
        raise ValueError("not an ICO")
    out = []
    off = 6
    for _ in range(n):
        w, _h, _c, _r, _pl, _bc, sz, pos = struct.unpack("<BBBBHHII", d[off:off + 16])
        off += 16
        payload = d[pos:pos + sz]
        if payload[:4] == b"\x89PNG":
            out.append((w or 256, payload))
    return out


def build_ico(entries):
    """entries: [(size, png_bytes)] -> ICO 字节。"""
    out = struct.pack("<HHH", 0, 1, len(entries))
    offset = 6 + 16 * len(entries)
    dirs = b""
    for size, png in entries:
        v = 0 if size >= 256 else size
        dirs += struct.pack("<BBBBHHII", v, v, 0, 0, 1, 32, len(png), offset)
        offset += len(png)
    out += dirs
    for _s, png in entries:
        out += png
    return out


# ---------------------------------------------------------------- 绘制


def blend_px(buf, w, h, x, y, color, alpha):
    """按 alpha(0..1) 把 color 混合到 (x,y)。"""
    if alpha <= 0 or x < 0 or y < 0 or x >= w or y >= h:
        return
    o = (y * w + x) * 4
    da = buf[o + 3] / 255.0
    sa = alpha
    oa = sa + da * (1 - sa)
    if oa <= 0:
        return
    for k in range(3):
        buf[o + k] = int(round((color[k] * sa + buf[o + k] * da * (1 - sa)) / oa))
    buf[o + 3] = int(round(oa * 255))


def fill_shape(buf, w, h, inside, color, ss=4):
    """用 ss×ss 超采样按 coverage 填充由 inside(x,y)->bool 定义的形状。"""
    n = ss * ss
    for y in range(h):
        for x in range(w):
            hit = 0
            for sy in range(ss):
                for sx in range(ss):
                    if inside(x + (sx + 0.5) / ss, y + (sy + 0.5) / ss):
                        hit += 1
            if hit:
                blend_px(buf, w, h, x, y, color, hit / n)


def distance_to_segment(px, py, x1, y1, x2, y2):
    dx, dy = x2 - x1, y2 - y1
    L2 = dx * dx + dy * dy
    if L2 == 0:
        return ((px - x1) ** 2 + (py - y1) ** 2) ** 0.5
    t = max(0.0, min(1.0, ((px - x1) * dx + (py - y1) * dy) / L2))
    return ((px - (x1 + t * dx)) ** 2 + (py - (y1 + t * dy)) ** 2) ** 0.5


# ---------------------------------------------------------------- 抠图 / 缩放


def remove_flat_bg(rgba, w, h, tol=24):
    """把「与四边连通、且接近纯白」的像素置为全透明。

    用边界洪泛（flood fill）而不是纯颜色阈值：logo 内部也可能有接近白色的
    区域（高光、内部留白），只按颜色删会把这些洞也一起扣掉。
    tol 为距纯白的切比雪夫距离上限。
    """
    from collections import deque

    n = w * h
    near = bytearray(n)
    for i in range(n):
        o = i * 4
        if (255 - rgba[o] <= tol and 255 - rgba[o + 1] <= tol
                and 255 - rgba[o + 2] <= tol):
            near[i] = 1

    bg = bytearray(n)
    dq = deque()
    for x in range(w):
        for y in (0, h - 1):
            i = y * w + x
            if near[i] and not bg[i]:
                bg[i] = 1
                dq.append(i)
    for y in range(h):
        for x in (0, w - 1):
            i = y * w + x
            if near[i] and not bg[i]:
                bg[i] = 1
                dq.append(i)

    while dq:
        i = dq.popleft()
        y, x = divmod(i, w)
        if x > 0:
            j = i - 1
            if near[j] and not bg[j]:
                bg[j] = 1; dq.append(j)
        if x < w - 1:
            j = i + 1
            if near[j] and not bg[j]:
                bg[j] = 1; dq.append(j)
        if y > 0:
            j = i - w
            if near[j] and not bg[j]:
                bg[j] = 1; dq.append(j)
        if y < h - 1:
            j = i + w
            if near[j] and not bg[j]:
                bg[j] = 1; dq.append(j)

    out = bytearray(rgba)
    removed = 0
    for i in range(n):
        if bg[i]:
            o = i * 4
            out[o] = out[o + 1] = out[o + 2] = out[o + 3] = 0
            removed += 1
    return out, removed


def bbox_alpha(rgba, w, h, thr=24):
    """返回不透明内容的包围盒 (x0,y0,x1,y1)，全透明时返回 None。"""
    x0, y0, x1, y1 = w, h, -1, -1
    for y in range(h):
        base = y * w * 4
        for x in range(w):
            if rgba[base + x * 4 + 3] > thr:
                if x < x0: x0 = x
                if x > x1: x1 = x
                if y < y0: y0 = y
                if y > y1: y1 = y
    if x1 < 0:
        return None
    return x0, y0, x1, y1


def crop(rgba, w, h, box):
    x0, y0, x1, y1 = box
    cw, ch = x1 - x0 + 1, y1 - y0 + 1
    out = bytearray(cw * ch * 4)
    for y in range(ch):
        so = ((y + y0) * w + x0) * 4
        do = y * cw * 4
        out[do:do + cw * 4] = rgba[so:so + cw * 4]
    return cw, ch, out


def resize_premul(rgba, w, h, dw, dh):
    """预乘 alpha 的盒式缩放。

    必须预乘：否则透明像素的 RGB（往往是白色残留）会被平均进来，边缘出现白边/
    黑边光晕。盒式平均在大比例缩小时本身就等价于抗锯齿，边缘足够平滑。
    """
    out = bytearray(dw * dh * 4)
    xr = w / dw
    yr = h / dh
    for dy in range(dh):
        sy0, sy1 = int(dy * yr), max(int((dy + 1) * yr), int(dy * yr) + 1)
        if sy1 > h: sy1 = h
        for dx in range(dw):
            sx0, sx1 = int(dx * xr), max(int((dx + 1) * xr), int(dx * xr) + 1)
            if sx1 > w: sx1 = w
            sa = sr = sg = sb = 0
            for sy in range(sy0, sy1):
                base = sy * w * 4
                for sx in range(sx0, sx1):
                    o = base + sx * 4
                    a = rgba[o + 3]
                    if a:
                        sa += a
                        sr += rgba[o] * a
                        sg += rgba[o + 1] * a
                        sb += rgba[o + 2] * a
            o = (dy * dw + dx) * 4
            cnt = (sy1 - sy0) * (sx1 - sx0)
            if sa == 0:
                continue
            out[o] = sr // sa
            out[o + 1] = sg // sa
            out[o + 2] = sb // sa
            out[o + 3] = sa // cnt
    return out


def fit_into_square(rgba, w, h, size, pad_ratio=0.06):
    """等比缩放内容并居中放进 size×size 透明画布（含内边距）。"""
    box = bbox_alpha(rgba, w, h)
    if box is None:
        return bytearray(size * size * 4)
    cw, ch, cropped = crop(rgba, w, h, box)
    avail = size * (1 - pad_ratio * 2)
    scale = min(avail / cw, avail / ch)
    tw, th = max(1, int(round(cw * scale))), max(1, int(round(ch * scale)))
    small = resize_premul(cropped, cw, ch, tw, th)
    canvas = bytearray(size * size * 4)
    ox, oy = (size - tw) // 2, (size - th) // 2
    for y in range(th):
        so = y * tw * 4
        do = ((oy + y) * size + ox) * 4
        canvas[do:do + tw * 4] = small[so:so + tw * 4]
    return canvas


# ---------------------------------------------------------------- BMP 编码的 ICO


def _bmp_entry(rgba, size):
    """把 RGBA 打包成 ICO 内嵌的 BMP（BITMAPINFOHEADER + BGRA + AND 掩码）。"""
    stride = size * 4
    header = struct.pack("<IiiHHIIiiII", 40, size, size * 2, 1, 32, 0,
                         stride * size, 0, 0, 0, 0)
    xor = bytearray(stride * size)
    for y in range(size):
        sy = size - 1 - y          # BMP 行序自下而上
        so = sy * stride
        do = y * stride
        for x in range(size):
            o = so + x * 4
            d = do + x * 4
            xor[d] = rgba[o + 2]       # B
            xor[d + 1] = rgba[o + 1]   # G
            xor[d + 2] = rgba[o]       # R
            xor[d + 3] = rgba[o + 3]   # A
    mask_row = ((size + 31) // 32) * 4
    return header + bytes(xor) + bytes(mask_row * size)


def build_ico_bmp(sizes_rgba):
    """sizes_rgba: [(size, rgba)] -> BMP 编码的 ICO 字节。

    BMP 而非 PNG 编码：兼容性最好（PNG 内嵌 ICO 需 Vista+），且 go-winres
    的图标解码器只认 BMP。
    """
    entries = [(s, _bmp_entry(r, s)) for s, r in sizes_rgba]
    out = struct.pack("<HHH", 0, 1, len(entries))
    offset = 6 + 16 * len(entries)
    dirs = b""
    for size, data in entries:
        v = 0 if size >= 256 else size
        dirs += struct.pack("<BBBBHHII", v, v, 0, 0, 1, 32, len(data), offset)
        offset += len(data)
    out += dirs
    for _s, data in entries:
        out += data
    return out


def drop_small_blobs(rgba, w, h, alpha_thr=24, min_frac=0.004):
    """连通域分析：把面积占比小于 min_frac 的孤立小块置为透明。

    用于去掉 logo 周围的装饰小圆点——它们会把内容包围盒撑大，导致主体在小尺寸
    下被挤小。返回 (新 rgba, 保留的连通域数, 丢弃的连通域数)。
    """
    from collections import deque

    n = w * h
    seen = bytearray(n)
    keep = bytearray(n)
    comps = []

    for start in range(n):
        if seen[start] or rgba[start * 4 + 3] <= alpha_thr:
            continue
        seen[start] = 1
        dq = deque([start])
        members = [start]
        while dq:
            i = dq.popleft()
            y, x = divmod(i, w)
            if x > 0:
                j = i - 1
                if not seen[j] and rgba[j * 4 + 3] > alpha_thr:
                    seen[j] = 1; dq.append(j); members.append(j)
            if x < w - 1:
                j = i + 1
                if not seen[j] and rgba[j * 4 + 3] > alpha_thr:
                    seen[j] = 1; dq.append(j); members.append(j)
            if y > 0:
                j = i - w
                if not seen[j] and rgba[j * 4 + 3] > alpha_thr:
                    seen[j] = 1; dq.append(j); members.append(j)
            if y < h - 1:
                j = i + w
                if not seen[j] and rgba[j * 4 + 3] > alpha_thr:
                    seen[j] = 1; dq.append(j); members.append(j)
        comps.append(members)

    limit = n * min_frac
    kept = dropped = 0
    for members in comps:
        if len(members) >= limit:
            kept += 1
            for i in members:
                keep[i] = 1
        else:
            dropped += 1

    out = bytearray(rgba)
    for i in range(n):
        if not keep[i]:
            o = i * 4
            out[o] = out[o + 1] = out[o + 2] = out[o + 3] = 0
    return out, kept, dropped
