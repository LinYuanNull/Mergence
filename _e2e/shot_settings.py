#!/usr/bin/env python3
"""截设置页（含限时套餐领取卡片）。设置页是隐藏的 view，纯 --screenshot 拍不到。"""
import base64, json, os, subprocess, sys, time, urllib.request
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from drive_ui import EDGE, WS, wait_json
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "shots", "settings_claim.png")
PORT = 9341
BASE = "http://127.0.0.1:1234/"
sys.stdout.reconfigure(encoding="utf-8")

edge = subprocess.Popen([EDGE, "--headless=new", f"--remote-debugging-port={PORT}",
    "--user-data-dir=" + os.path.join(os.environ.get("TEMP", "."), "edgeshot"),
    "--no-sandbox", "--disable-gpu", "--window-size=1180,1000", BASE],
    stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
try:
    info = wait_json(f"http://127.0.0.1:{PORT}/json", timeout=30)
    page = next(t for t in info if t.get("type") == "page")
    ws = WS(page["webSocketDebuggerUrl"])
    def js(expr):
        r = ws.call("Runtime.evaluate", {"expression": expr, "returnByValue": True})
        return r.get("result", {}).get("value")
    time.sleep(2.5)
    js("document.querySelector('[data-view=settings]').click()")
    time.sleep(2.0)
    print("领取卡片存在:", js("!!document.getElementById('fClaimOn')"))
    print("状态文案:", js("document.getElementById('claimState').textContent"))
    shot = base64.b64decode(ws.call("Page.captureScreenshot", {"format": "png"})["data"])
    open(OUT, "wb").write(shot)
    print("截图:", OUT)
finally:
    edge.terminate()
