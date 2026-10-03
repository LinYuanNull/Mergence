"""抓取指定标题的窗口截图（纯 ctypes + GDI，不依赖 Pillow）。

用法: python capture_window.py "<窗口标题>" <输出.png>
"""
import ctypes
import ctypes.wintypes as wt
import sys
import time
import zlib
import struct

user32 = ctypes.windll.user32
gdi32 = ctypes.windll.gdi32

SRCCOPY = 0x00CC0020
DIB_RGB_COLORS = 0
PW_RENDERFULLCONTENT = 0x00000002


class BITMAPINFOHEADER(ctypes.Structure):
    _fields_ = [
        ("biSize", wt.DWORD), ("biWidth", ctypes.c_long), ("biHeight", ctypes.c_long),
        ("biPlanes", wt.WORD), ("biBitCount", wt.WORD), ("biCompression", wt.DWORD),
        ("biSizeImage", wt.DWORD), ("biXPelsPerMeter", ctypes.c_long),
        ("biYPelsPerMeter", ctypes.c_long), ("biClrUsed", wt.DWORD), ("biClrImportant", wt.DWORD),
    ]


class BITMAPINFO(ctypes.Structure):
    _fields_ = [("bmiHeader", BITMAPINFOHEADER), ("bmiColors", wt.DWORD * 3)]


def png_encode(w, h, bgra):
    raw = bytearray()
    stride = w * 4
    for y in range(h):
        raw.append(0)
        row = bgra[y * stride:(y + 1) * stride]
        for x in range(w):
            o = x * 4
            raw += bytes((row[o + 2], row[o + 1], row[o], 255))

    def chunk(typ, data):
        return (struct.pack(">I", len(data)) + typ + data
                + struct.pack(">I", zlib.crc32(typ + data) & 0xFFFFFFFF))

    return (b"\x89PNG\r\n\x1a\n"
            + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 6, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(bytes(raw), 6))
            + chunk(b"IEND", b""))


def main():
    title = sys.argv[1]
    out = sys.argv[2]
    hwnd = user32.FindWindowW(None, title)
    if not hwnd:
        print("WINDOW_NOT_FOUND:", title)
        return 1
    # SetForegroundWindow 对后台进程常被系统拒绝，改用 SetWindowPos 置顶抬升，
    # 否则屏幕 BitBlt 抓到的可能是压在它上面的其它窗口。
    HWND_TOPMOST, HWND_NOTOPMOST = -1, -2
    SWP_NOMOVE, SWP_NOSIZE, SWP_SHOWWINDOW = 0x0002, 0x0001, 0x0040
    user32.SetWindowPos(hwnd, HWND_TOPMOST, 0, 0, 0, 0,
                        SWP_NOMOVE | SWP_NOSIZE | SWP_SHOWWINDOW)
    user32.SetForegroundWindow(hwnd)
    time.sleep(0.8)

    rect = wt.RECT()
    user32.GetWindowRect(hwnd, ctypes.byref(rect))
    w = rect.right - rect.left
    h = rect.bottom - rect.top
    print("window size = %dx%d" % (w, h))

    hdc = user32.GetWindowDC(hwnd)
    mdc = gdi32.CreateCompatibleDC(hdc)
    bmp = gdi32.CreateCompatibleBitmap(hdc, w, h)
    gdi32.SelectObject(mdc, bmp)
    # WebView2 走 DirectComposition 硬件合成，PrintWindow 抓到的内容区恒为空白，
    # 因此直接从屏幕 DC 按窗口矩形 BitBlt——这才是用户实际看到的画面。
    screen_dc = user32.GetDC(0)
    ok = gdi32.BitBlt(mdc, 0, 0, w, h, screen_dc, rect.left, rect.top, SRCCOPY)
    print("BitBlt(screen) =", ok)
    user32.ReleaseDC(0, screen_dc)
    if not ok:
        ok = user32.PrintWindow(hwnd, mdc, PW_RENDERFULLCONTENT)
        print("fallback PrintWindow =", ok)

    bi = BITMAPINFO()
    bi.bmiHeader.biSize = ctypes.sizeof(BITMAPINFOHEADER)
    bi.bmiHeader.biWidth = w
    bi.bmiHeader.biHeight = -h  # top-down
    bi.bmiHeader.biPlanes = 1
    bi.bmiHeader.biBitCount = 32
    bi.bmiHeader.biCompression = 0
    buf = ctypes.create_string_buffer(w * h * 4)
    gdi32.GetDIBits(mdc, bmp, 0, h, buf, ctypes.byref(bi), DIB_RGB_COLORS)

    with open(out, "wb") as f:
        f.write(png_encode(w, h, buf.raw))

    gdi32.DeleteObject(bmp)
    gdi32.DeleteDC(mdc)
    user32.ReleaseDC(hwnd, hdc)
    print("saved", out)
    return 0


if __name__ == "__main__":
    sys.exit(main())
