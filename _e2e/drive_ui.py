#!/usr/bin/env python3
"""最小 CDP 客户端：用真实浏览器打开面板、点出编辑弹层、截图。

只用标准库（socket + base64 + struct）实现 WebSocket 客户端帧读写，
避免为一个截图脚本引入 playwright/puppeteer 这类重依赖。

之所以必须驱动真实点击：弹层与表单只在交互后才出现，
纯 --screenshot 拍不到，而这恰恰是最容易发生布局问题的部分。
"""
import base64
import json
import os
import random
import re
import shutil
import socket
import struct
import subprocess
import sys
import time
import urllib.request

# 项目根：由本文件位置推导（_e2e/ 的上一层），不写死本机绝对路径。
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HOME = os.path.join(ROOT, "_e2e", "ui_home")
EXE = os.path.join(ROOT, "ModelMux.exe")
FAKE = os.path.join(ROOT, "_e2e", "fake_upstream.py")
# 用当前解释器；换机器/换 Python 位置不必改代码（可用 MODELMUX_PY 覆盖）。
PY = os.environ.get("MODELMUX_PY", sys.executable)
EDGE = r"C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe"
FAKE_PORT = 18093
CDP_PORT = 9333

sys.stdout.reconfigure(encoding="utf-8")


class WS:
    def __init__(self, url):
        m = re.match(r"ws://([^:/]+):(\d+)(/.*)", url)
        host, port, path = m.group(1), int(m.group(2)), m.group(3)
        self.sock = socket.create_connection((host, port), timeout=30)
        key = base64.b64encode(bytes(random.getrandbits(8) for _ in range(16))).decode()
        req = (f"GET {path} HTTP/1.1\r\nHost: {host}:{port}\r\n"
               "Upgrade: websocket\r\nConnection: Upgrade\r\n"
               f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n")
        self.sock.sendall(req.encode())
        buf = b""
        while b"\r\n\r\n" not in buf:
            buf += self.sock.recv(4096)
        if b"101" not in buf.split(b"\r\n")[0]:
            raise RuntimeError("WebSocket 握手失败: " + buf[:200].decode("latin1"))
        self.buf = buf.split(b"\r\n\r\n", 1)[1]
        self.mid = 0

    def _recv_exact(self, n):
        while len(self.buf) < n:
            chunk = self.sock.recv(max(4096, n - len(self.buf)))
            if not chunk:
                raise RuntimeError("连接关闭")
            self.buf += chunk
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def send(self, text):
        payload = text.encode()
        header = bytearray([0x81])
        n = len(payload)
        if n < 126:
            header.append(0x80 | n)
        elif n < 65536:
            header.append(0x80 | 126)
            header += struct.pack(">H", n)
        else:
            header.append(0x80 | 127)
            header += struct.pack(">Q", n)
        mask = bytes(random.getrandbits(8) for _ in range(4))
        header += mask
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        self.sock.sendall(bytes(header) + masked)

    def recv(self):
        b0, b1 = self._recv_exact(2)
        opcode = b0 & 0x0F
        length = b1 & 0x7F
        if length == 126:
            length = struct.unpack(">H", self._recv_exact(2))[0]
        elif length == 127:
            length = struct.unpack(">Q", self._recv_exact(8))[0]
        data = self._recv_exact(length)
        if opcode == 0x8:
            raise RuntimeError("服务端关闭连接")
        if opcode == 0x9:  # ping
            return None
        return data.decode("utf-8", "replace")

    def call(self, method, params=None, timeout=30):
        self.mid += 1
        mid = self.mid
        self.send(json.dumps({"id": mid, "method": method, "params": params or {}}))
        end = time.time() + timeout
        while time.time() < end:
            msg = self.recv()
            if not msg:
                continue
            obj = json.loads(msg)
            if obj.get("id") == mid:
                if "error" in obj:
                    raise RuntimeError(f"{method} 出错：{obj['error']}")
                return obj.get("result", {})
        raise TimeoutError(method)

    def close(self):
        try:
            self.sock.close()
        except Exception:
            pass


def wait_json(url, timeout=30):
    end = time.time() + timeout
    while time.time() < end:
        try:
            with urllib.request.urlopen(url, timeout=3) as r:
                return json.loads(r.read())
        except Exception:
            time.sleep(0.4)
    return None


def main():
    if os.path.isdir(HOME):
        shutil.rmtree(HOME, ignore_errors=True)  # 仅限 _e2e 下的测试目录
    os.makedirs(HOME, exist_ok=True)

    fake = subprocess.Popen([PY, FAKE, str(FAKE_PORT)],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    mm = edge = None
    ws = None
    try:
        time.sleep(1.0)
        env = dict(os.environ)
        env["MODELMUX_HOME"] = HOME
        env["MODELMUX_HEADLESS"] = "1"
        mm = subprocess.Popen([EXE], env=env,
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

        log = os.path.join(HOME, "logs", "modelmux.log")
        port, deadline = None, time.time() + 25
        while time.time() < deadline and not port:
            if os.path.isfile(log):
                for line in open(log, encoding="utf-8", errors="replace"):
                    if "内置服务已监听" in line:
                        m = re.search(r"127\.0\.0\.1:(\d+)", line)
                        if m:
                            port = int(m.group(1))
                            break
            time.sleep(0.3)
        if not port:
            print("未取得端口")
            return 1
        base = f"http://127.0.0.1:{port}"
        print("端口", port)

        # 先加一个渠道，这样弹层里能预填出「编辑」的真实数据
        def post(path, obj):
            rq = urllib.request.Request(base + path, data=json.dumps(obj).encode(),
                                        headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(rq, timeout=30) as r:
                return json.loads(r.read())

        cfgs = [
            {"name": "", "display_name": "OpenRouter", "base_url": f"http://127.0.0.1:{FAKE_PORT}/chat/v1",
             "protocol": "chat", "model_prefix": "or", "api_keys": ["sk-or-v1-abcdefghijklmnop1234"],
             "models": ["fake-alpha", "fake-beta"], "enabled": True, "preset": "openrouter",
             "priority": 5, "headers": {"HTTP-Referer": "https://my.app"}},
            {"name": "", "display_name": "Anthropic 官方", "base_url": f"http://127.0.0.1:{FAKE_PORT}/anth",
             "protocol": "anthropic", "model_prefix": "claude", "api_keys": ["sk-ant-api03-xxxxxxxxxxxx"],
             "models": [{"id": "fake-claude", "context": 200000}], "enabled": True, "preset": "anthropic"},
        ]
        for c in cfgs:
            print("  建渠道:", post("/api/channels", c).get("name"))

        edge = subprocess.Popen([
            EDGE, "--headless=new", "--disable-gpu", "--no-sandbox",
            "--hide-scrollbars", "--force-device-scale-factor=1",
            "--window-size=1400,980",
            f"--remote-debugging-port={CDP_PORT}",
            f"--user-data-dir={os.path.join(HOME, 'edge-profile')}",
            base + "/",
        ], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

        targets = wait_json(f"http://127.0.0.1:{CDP_PORT}/json")
        page = next((t for t in (targets or []) if t.get("type") == "page"), None)
        if not page:
            print("未找到页面目标")
            return 1
        ws = WS(page["webSocketDebuggerUrl"])

        ws.call("Runtime.enable")
        ws.call("Page.enable")

        def evaljs(expr):
            r = ws.call("Runtime.evaluate", {"expression": expr, "returnByValue": True, "awaitPromise": True})
            return r.get("result", {}).get("value")

        def shot(name):
            r = ws.call("Page.captureScreenshot", {"format": "png"}, timeout=60)
            path = os.path.join(ROOT, "_e2e", "shots", name)
            with open(path, "wb") as f:
                f.write(base64.b64decode(r["data"]))
            print(f"  {name}: {os.path.getsize(path)} 字节")
            return path

        # 等首屏数据到位
        for _ in range(40):
            if evaljs("!!document.querySelector('#chCards .prov')"):
                break
            time.sleep(0.3)

        print("概览渲染的渠道卡数：", evaljs("document.querySelectorAll('#chCards .prov').length"))

        # ── 打开「添加渠道」弹层（新建：预设应为 openrouter 且每个区块展开状态正确）
        evaljs("document.querySelector('#btnAddCh').click()")
        time.sleep(0.6)
        print("添加弹层可见：", evaljs("!document.getElementById('chModal').hidden"))
        print("  预设选项数：", evaljs("document.querySelectorAll('#fPreset option').length"))
        print("  基础区块展开：", evaljs("document.querySelectorAll('#chModal .sec')[0].open"))
        print("  模型区块收起：", evaljs("!document.querySelectorAll('#chModal .sec')[1].open"))
        print("  Base URL 预填：", evaljs("document.getElementById('fBase').value"))
        shot("form_add.png")

        # 展开「模型」区块，看模型编辑区
        evaljs("document.querySelectorAll('#chModal .sec')[1].open = true")
        time.sleep(0.3)
        shot("form_models.png")

        # 展开「高级」区块
        evaljs("document.querySelectorAll('#chModal .sec')[2].open = true")
        time.sleep(0.3)
        shot("form_advanced.png")

        # ── 切到编辑模式：应回填真实数据、Key 显示掩码占位符
        evaljs("document.getElementById('chModal').hidden = true")
        evaljs("showPage('channels')")
        time.sleep(0.8)
        evaljs("document.querySelector('.chcard [data-act=\"edit\"]').click()")
        time.sleep(1.0)
        print("编辑弹层标题：", evaljs("document.getElementById('chFormTitle').textContent"))
        print("  回填 Base URL：", evaljs("document.getElementById('fBase').value"))
        print("  回填模型列表：", evaljs("document.getElementById('fModels').value").replace("\n", " | "))
        print("  Key 框留空：", evaljs("document.getElementById('fKeys').value === ''"))
        print("  Key 占位符：", evaljs("document.getElementById('fKeys').placeholder"))
        print("  回填自定义头：", evaljs("document.getElementById('fHeaders').value"))
        shot("form_edit.png")

        # ── 触发一次真实测试连接，看测试结果区
        evaljs("document.getElementById('btnTest').click()")
        for _ in range(60):
            txt = evaljs("document.getElementById('testSteps').textContent")
            if txt and "正在测试" not in txt:
                break
            time.sleep(0.4)
        print("测试结果：", (evaljs("document.getElementById('testSteps').textContent") or "")[:160])
        # 测试结果区在弹层下方，得先滚到底才看得到
        evaljs("document.querySelector('#chModal .modal-bd').scrollTop = 99999")
        time.sleep(0.4)
        shot("form_test.png")

        # ── 停在渠道列表页（便于人工核对）
        evaljs("document.getElementById('chModal').hidden = true")
        evaljs("showPage('channels')")
        time.sleep(0.5)

        # 控制台有没有报错（CSP 拦截、JS 异常都会在这里现形）
        errs = evaljs("(window.__errs||[]).length")
        print("页面错误计数：", errs)

    finally:
        if ws:
            ws.close()
        for p in (edge, mm, fake):
            if p and p.poll() is None:
                p.kill()
    return 0


if __name__ == "__main__":
    sys.exit(main())
