"""从 exe 中抽取 Windows 图标（验证 PE 图标资源确实嵌入成功）。

用 ExtractIconExW 取系统为该 exe 关联的图标，再经 GetIconInfo + GetDIBits
转成 RGBA PNG。这样验证的是「资源管理器/快捷方式实际会显示的那个图标」，
而不是运行时用 WM_SETICON 设的窗口图标。

用法: python extract_exe_icon.py <exe路径> <输出.png> [尺寸]
"""
import ctypes
import ctypes.wintypes as wt
import os
import struct
import sys
import zlib

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import iconlib as il

user32 = ctypes.windll.user32
gdi32 = ctypes.windll.gdi32
shell32 = ctypes.windll.shell32

# 句柄必须显式声明为 void*：ctypes 默认按 int 处理，64 位句柄会 OverflowError。
_P = ctypes.c_void_p
gdi32.GetObjectW.argtypes = [_P, ctypes.c_int, _P]
gdi32.SelectObject.argtypes = [_P, _P]
gdi32.CreateCompatibleDC.argtypes = [_P]
gdi32.CreateCompatibleDC.restype = _P
gdi32.DeleteDC.argtypes = [_P]
gdi32.DeleteObject.argtypes = [_P]
gdi32.GetDIBits.argtypes = [_P, _P, ctypes.c_uint, ctypes.c_uint, _P, _P, ctypes.c_uint]
user32.GetDC.argtypes = [_P]
user32.GetDC.restype = _P
user32.ReleaseDC.argtypes = [_P, _P]
user32.DestroyIcon.argtypes = [_P]
shell32.ExtractIconExW.argtypes = [ctypes.c_wchar_p, ctypes.c_int, _P, _P, ctypes.c_uint]


class ICONINFO(ctypes.Structure):
    _fields_ = [("fIcon", wt.BOOL), ("xHotspot", wt.DWORD), ("yHotspot", wt.DWORD),
                ("hbmMask", wt.HBITMAP), ("hbmColor", wt.HBITMAP)]


class BITMAP(ctypes.Structure):
    _fields_ = [("bmType", ctypes.c_long), ("bmWidth", ctypes.c_long),
                ("bmHeight", ctypes.c_long), ("bmWidthBytes", ctypes.c_long),
                ("bmPlanes", wt.WORD), ("bmBitsPixel", wt.WORD), ("bmBits", ctypes.c_void_p)]


class BITMAPINFOHEADER(ctypes.Structure):
    _fields_ = [("biSize", wt.DWORD), ("biWidth", ctypes.c_long), ("biHeight", ctypes.c_long),
                ("biPlanes", wt.WORD), ("biBitCount", wt.WORD), ("biCompression", wt.DWORD),
                ("biSizeImage", wt.DWORD), ("biXPelsPerMeter", ctypes.c_long),
                ("biYPelsPerMeter", ctypes.c_long), ("biClrUsed", wt.DWORD),
                ("biClrImportant", wt.DWORD)]


class BITMAPINFO(ctypes.Structure):
    _fields_ = [("bmiHeader", BITMAPINFOHEADER), ("bmiColors", wt.DWORD * 3)]


def hicon_to_rgba(hicon):
    info = ICONINFO()
    if not user32.GetIconInfo(hicon, ctypes.byref(info)):
        raise OSError("GetIconInfo failed")
    bm = BITMAP()
    gdi32.GetObjectW(info.hbmColor, ctypes.sizeof(bm), ctypes.byref(bm))
    w, h = bm.bmWidth, bm.bmHeight

    hdc = user32.GetDC(0)
    mdc = gdi32.CreateCompatibleDC(hdc)
    gdi32.SelectObject(mdc, info.hbmColor)

    bi = BITMAPINFO()
    bi.bmiHeader.biSize = ctypes.sizeof(BITMAPINFOHEADER)
    bi.bmiHeader.biWidth = w
    bi.bmiHeader.biHeight = -h
    bi.bmiHeader.biPlanes = 1
    bi.bmiHeader.biBitCount = 32
    bi.bmiHeader.biCompression = 0
    buf = ctypes.create_string_buffer(w * h * 4)
    gdi32.GetDIBits(mdc, info.hbmColor, 0, h, buf, ctypes.byref(bi), 0)

    rgba = bytearray(buf.raw)
    # 若 alpha 全 0（老式图标无 alpha），退化为不透明
    if all(rgba[i] == 0 for i in range(3, len(rgba), 4)):
        for i in range(3, len(rgba), 4):
            rgba[i] = 255

    gdi32.DeleteDC(mdc)
    user32.ReleaseDC(0, hdc)
    # 句柄必须显式按 void* 传，否则 ctypes 会按 int 处理并溢出
    for h in (info.hbmColor, info.hbmMask):
        if h:
            try:
                gdi32.DeleteObject(ctypes.c_void_p(h))
            except Exception:
                pass
    return w, h, rgba


def main():
    exe, dest = sys.argv[1], sys.argv[2]
    large = (wt.HICON * 1)()
    small = (wt.HICON * 1)()
    n = shell32.ExtractIconExW(exe, 0, large, small, 1)
    print("ExtractIconExW 返回图标数:", n)
    if n <= 0:
        print("FAILED: exe 内没有图标资源")
        return 1
    hicon = large[0] or small[0]
    w, h, rgba = hicon_to_rgba(hicon)
    print("抽取到图标尺寸: %dx%d" % (w, h))
    open(dest, "wb").write(il.encode_png(w, h, rgba))
    print("写出:", dest)
    user32.DestroyIcon(large[0])
    user32.DestroyIcon(small[0])
    return 0


if __name__ == "__main__":
    sys.exit(main())
