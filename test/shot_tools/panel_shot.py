#!/usr/bin/env python3
"""对**正在运行**的 ModelMux 面板截图（不改它的状态）。

用法：python panel_shot.py <port> <outdir>
通过 Edge headless + CDP 打开面板、切到「渠道」页签后截图。
"""
import base64
import json
import os
import socket
import subprocess
import sys
import time
import urllib.request

sys.stdout.reconfigure(encoding="utf-8")

# 本文件在 test/shot_tools/ 下：drive_ui.py 在上一级。
_TEST = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
sys.path.insert(0, _TEST)
from drive_ui import EDGE  # noqa: E402  共享的浏览器定位
PORT = sys.argv[1]
OUT = sys.argv[2]
URL = "http://127.0.0.1:%s/" % PORT

DBG = 9333
proc = subprocess.Popen([
    EDGE, "--headless=new", "--disable-gpu",
    "--remote-debugging-port=%d" % DBG,
    "--window-size=1280,800", "--no-first-run", URL,
], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

try:
    ws_url = None
    for _ in range(50):
        try:
            with urllib.request.urlopen("http://127.0.0.1:%d/json" % DBG, timeout=2) as r:
                tabs = json.load(r)
            pages = [t for t in tabs if t.get("type") == "page" and "127.0.0.1" in t.get("url", "")]
            if pages:
                ws_url = pages[0]["webSocketDebuggerUrl"]
                break
        except Exception:
            pass
        time.sleep(0.3)
    if not ws_url:
        print("无法取得 CDP 调试端点")
        raise SystemExit(1)

    # 纯标准库的 WebSocket 客户端（只够发命令收事件，够用）
    s = socket.create_connection(("127.0.0.1", DBG), timeout=10)
    import hashlib
    import os
    import struct

    key = base64.b64encode(os.urandom(16)).decode()
    req = ("GET %s HTTP/1.1\r\nHost: 127.0.0.1:%d\r\n"
           "Upgrade: websocket\r\nConnection: Upgrade\r\n"
           "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n"
           % (ws_url.split("127.0.0.1:%d" % DBG)[1], DBG, key))
    s.sendall(req.encode())
    buf = b""
    while b"\r\n\r\n" not in buf:
        buf += s.recv(4096)
    head, rest = buf.split(b"\r\n\r\n", 1)

    mid = [0]

    def send(method, params=None):
        mid[0] += 1
        payload = json.dumps({"id": mid[0], "method": method, "params": params or {}}).encode()
        mask = os.urandom(4)
        ln = len(payload)
        if ln < 126:
            hdr = struct.pack("!BB", 0x81, 0x80 | ln)
        elif ln < 65536:
            hdr = struct.pack("!BBH", 0x81, 0x80 | 126, ln)
        else:
            hdr = struct.pack("!BBQ", 0x81, 0x80 | 127, ln)
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        s.sendall(hdr + mask + masked)

    def recv_msg(timeout=15):
        s.settimeout(timeout)
        data = rest
        while True:
            if len(data) >= 2:
                b1, b2 = data[0], data[1]
                ln = b2 & 0x7F
                off = 2
                if ln == 126:
                    if len(data) < 4:
                        data += s.recv(65536)
                        continue
                    ln = struct.unpack("!H", data[2:4])[0]
                    off = 4
                elif ln == 127:
                    if len(data) < 10:
                        data += s.recv(65536)
                        continue
                    ln = struct.unpack("!Q", data[2:10])[0]
                    off = 10
                if len(data) >= off + ln:
                    payload = data[off:off + ln]
                    data = data[off + ln:]
                    return json.loads(payload.decode("utf-8", "replace"))
            chunk = s.recv(65536)
            if not chunk:
                return None
            data += chunk

    def wait_id(i):
        deadline = time.time() + 20
        while time.time() < deadline:
            m = recv_msg()
            if m is None:
                return None
            if m.get("id") == i:
                return m
        return None

    send("Runtime.enable")
    wait_id(mid[0])
    send("Page.enable")
    wait_id(mid[0])
    time.sleep(2.0)

    def evaluate(expr):
        i = mid[0] + 1
        send("Runtime.evaluate", {"expression": expr, "returnByValue": True})
        m = wait_id(i)
        if not m:
            return None
        return m.get("result", {}).get("result", {}).get("value")

    # 切到渠道页
    evaluate("document.querySelector('[data-nav=\"channels\"]').click()")
    time.sleep(1.2)
    send("Emulation.setDeviceMetricsOverride",
         {"width": 1280, "height": 860, "deviceScaleFactor": 1, "mobile": False})
    time.sleep(0.6)
    i = mid[0] + 1
    send("Page.captureScreenshot", {"format": "png"})
    m = wait_id(i)
    if m and m.get("result", {}).get("data"):
        with open(OUT, "wb") as f:
            f.write(base64.b64decode(m["result"]["data"]))
        print("已保存", OUT)
    else:
        print("截图失败")
finally:
    proc.terminate()
