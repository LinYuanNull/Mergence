#!/usr/bin/env python3
"""对运行中的 ModelMux 新界面截图：概览 / 渠道两大区块 / 设置。"""
import base64
import json
import os
import re
import socket
import struct
import subprocess
import sys
import time
import urllib.request

sys.stdout.reconfigure(encoding="utf-8")

EDGE = r"C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe"
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "shots")
os.makedirs(OUT, exist_ok=True)

# 取端口（日志累积，取最后一次）
log = os.path.join(os.environ["LOCALAPPDATA"], "ModelMux", "logs", "modelmux.log")
port = None
for line in open(log, encoding="utf-8", errors="replace"):
    m = re.search(r'"api":"http://127\.0\.0\.1:(\d+)/v1"', line)
    if m:
        port = int(m.group(1))
print("端口 =", port)
URL = f"http://127.0.0.1:{port}/"

proc = subprocess.Popen([
    EDGE, "--headless=new", "--disable-gpu",
    "--remote-debugging-port=9341", "--window-size=1280,860",
    "--no-first-run", URL,
], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

try:
    ws = None
    for _ in range(50):
        try:
            with urllib.request.urlopen("http://127.0.0.1:9341/json", timeout=2) as r:
                tabs = json.load(r)
            pages = [t for t in tabs if t.get("type") == "page"]
            if pages:
                ws = pages[0]["webSocketDebuggerUrl"]
                break
        except Exception:
            pass
        time.sleep(0.3)
    assert ws, "拿不到 CDP 端点"

    s = socket.create_connection(("127.0.0.1", 9341), timeout=10)
    import hashlib
    key = base64.b64encode(os.urandom(16)).decode()
    path = ws.split("9341")[1]
    s.sendall((f"GET {path} HTTP/1.1\r\nHost: 127.0.0.1:9341\r\nUpgrade: websocket\r\n"
               f"Connection: Upgrade\r\nSec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n").encode())
    buf = b""
    while b"\r\n\r\n" not in buf:
        buf += s.recv(4096)
    rest = buf.split(b"\r\n\r\n", 1)[1]
    mid = [0]

    def send(method, params=None):
        mid[0] += 1
        p = json.dumps({"id": mid[0], "method": method, "params": params or {}}).encode()
        mask = os.urandom(4)
        ln = len(p)
        if ln < 126:
            hdr = struct.pack("!BB", 0x81, 0x80 | ln)
        else:
            hdr = struct.pack("!BBH", 0x81, 0x80 | 126, ln)
        s.sendall(hdr + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(p)))

    def recv_msg(timeout=20):
        s.settimeout(timeout)
        data = rest
        while True:
            if len(data) >= 2:
                b2 = data[1] & 0x7F
                off = 2
                if b2 < 126:
                    ln = b2
                elif b2 == 126:
                    if len(data) < 4:
                        data += s.recv(65536); continue
                    ln = struct.unpack("!H", data[2:4])[0]; off = 4
                else:
                    if len(data) < 10:
                        data += s.recv(65536); continue
                    ln = struct.unpack("!Q", data[2:10])[0]; off = 10
                if len(data) >= off + ln:
                    out = data[off:off + ln]
                    data = data[off + ln:]
                    return json.loads(out.decode("utf-8", "replace"))
            c = s.recv(65536)
            if not c:
                return None
            data += c

    def wait(i):
        dl = time.time() + 20
        while time.time() < dl:
            m = recv_msg()
            if m and m.get("id") == i:
                return m
        return None

    send("Runtime.enable"); wait(mid[0])
    send("Page.enable"); wait(mid[0])
    time.sleep(2.5)

    def ev(expr):
        i = mid[0] + 1
        send("Runtime.evaluate", {"expression": expr, "returnByValue": True})
        m = wait(i)
        return m.get("result", {}).get("result", {}).get("value") if m else None

    def shot(name):
        i = mid[0] + 1
        send("Page.captureScreenshot", {"format": "png"})
        m = wait(i)
        if m and m.get("result", {}).get("data"):
            with open(os.path.join(OUT, name), "wb") as f:
                f.write(base64.b64decode(m["result"]["data"]))
            print("已保存", name)

    # 1) 概览
    ev("showPage('overview')")
    time.sleep(0.8)
    shot("r_overview.png")

    # 2) 渠道页两大区块 + 展开账号管理
    ev("showPage('channels')")
    time.sleep(0.6)
    # 展开 WorkBuddy 的账号管理区
    ev("document.querySelector('.chcard .acct-toggle, [data-act=\\'accounts\\']')?.click()")
    time.sleep(2.0)
    shot("r_channels.png")

    # 3) 设置页
    ev("showPage('settings')")
    time.sleep(0.6)
    shot("r_settings.png")

    # 概要信息
    txt = ev("document.body.innerText.slice(0, 900)")
    print("---- 页面文本 ----")
    print(txt)
finally:
    proc.terminate()
