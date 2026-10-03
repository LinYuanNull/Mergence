#!/usr/bin/env python3
"""对运行中的 ModelMux 桌面布局截图：侧栏 / 概览 / 渠道 / 上游控制台各视图 / 设置。"""
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

log = os.path.join(os.environ["LOCALAPPDATA"], "ModelMux", "logs", "modelmux.log")
port = None
for line in open(log, encoding="utf-8", errors="replace"):
    m = re.search(r'"api":"http://127\.0\.0\.1:(\d+)/v1"', line)
    if m:
        port = int(m.group(1))
print("端口 =", port)
if not port:
    sys.exit("取不到面板端口")

proc = subprocess.Popen([
    EDGE, "--headless=new", "--disable-gpu",
    "--remote-debugging-port=9342", "--window-size=1400,900",
    "--no-first-run", f"http://127.0.0.1:{port}/",
], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def die(msg):
    print("错误:", msg)


try:
    ws = None
    for _ in range(60):
        try:
            with urllib.request.urlopen("http://127.0.0.1:9342/json", timeout=2) as r:
                tabs = json.load(r)
            pages = [t for t in tabs if t.get("type") == "page"]
            if pages:
                ws = pages[0]["webSocketDebuggerUrl"]
                break
        except Exception:
            pass
        time.sleep(0.3)
    if not ws:
        die("拿不到 CDP 端点")
        sys.exit(1)

    s = socket.create_connection(("127.0.0.1", 9342), timeout=10)
    key = base64.b64encode(os.urandom(16)).decode()
    path = ws.split("9342")[1]
    s.sendall((f"GET {path} HTTP/1.1\r\nHost: 127.0.0.1:9342\r\nUpgrade: websocket\r\n"
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
        hdr = struct.pack("!BB", 0x81, 0x80 | ln) if ln < 126 else struct.pack("!BBH", 0x81, 0x80 | 126, ln)
        s.sendall(hdr + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(p)))

    def recv_msg(timeout=25):
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
        dl = time.time() + 25
        while time.time() < dl:
            m = recv_msg()
            if m and m.get("id") == i:
                return m
        return None

    send("Runtime.enable"); wait(mid[0])
    send("Page.enable"); wait(mid[0])
    time.sleep(3.0)

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
            print("  已保存", name)
        else:
            die("截图失败 " + name)

    print("检查导航与渲染：")
    # 侧栏 slot 导航（无 hash）
    navs = ev("Array.from(document.querySelectorAll('#nav .nav-i')).map(a=>a.dataset.view).join(',')")
    print("  侧栏 slot:", navs)
    print("  location.hash =", repr(ev("location.hash")), "(应为空)")
    print("  原生桥 mmShowWindow =", ev("typeof window.mmShowWindow"))

    # 1) 概览
    ev("setView('overview')"); time.sleep(1.0)
    shot("d_overview.png")

    # 2) 平台账号型
    ev("setView('acct')"); time.sleep(1.2)
    shot("d_acct.png")

    # 3) 上游控制台：账号池（真实数据）
    # 渠道表只在切到渠道视图时加载，所以先确保 acct 页已渲染再取渠道名
    ev("setView('acct')"); time.sleep(1.5)
    ch = ev("(S.channels.find(c=>c.source==='managed')||{}).name || ''")
    print("  托管渠道:", ch)
    if ch:
        ev(f"selectUpstream({json.dumps(ch)})")
        time.sleep(2.5)
        shot("d_up_accounts.png")
        rows = ev("document.querySelectorAll('#accBody tr').length")
        print("  账号表行数:", rows)

        # 任务中心
        ev("setView('up-tasks')"); time.sleep(1.8)
        shot("d_up_tasks.png")
        # 模型
        ev("setView('up-models')"); time.sleep(1.8)
        shot("d_up_models.png")
        rows = ev("document.querySelectorAll('#mdBody tr').length")
        print("  模型表行数:", rows)
        if rows == 0:
            print("  [调试] UP.models =", ev("JSON.stringify((UP.models||[]).slice(0,2))")[:200])
            print("  [调试] mdEmpty 文本 =", ev("document.getElementById('mdEmpty').textContent"))
            print("  [调试] mdHint =", ev("document.getElementById('mdHint').textContent"))
            print("  [调试] 直接再取一次 =", ev("upApi('models').then(d=>JSON.stringify(Object.keys(d)))"))
        # 配置
        ev("setView('up-config')"); time.sleep(2.0)
        shot("d_up_config.png")
        fields = ev("document.querySelectorAll('#cfgForm .cfg-row').length")
        print("  配置字段数:", fields)
        # 上游日志
        ev("setView('up-logs')"); time.sleep(2.0)
        shot("d_up_logs.png")

    # 4) 设置
    ev("setView('settings')"); time.sleep(1.0)
    shot("d_settings.png")
finally:
    proc.terminate()
