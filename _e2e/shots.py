#!/usr/bin/env python3
"""用真实浏览器渲染面板并截图，验证前端确实能工作。

不用 WebView2 而是 Edge headless：面板是 ModelMux 自己提供的页面，
只要浏览器能访问到那个端口，同源请求就能通过 —— 与窗口里看到的完全一致。

--virtual-time-budget 是关键：SPA 首屏要等几个 fetch 回来才渲染，
普通截图会在数据到位前就拍下去，拍出一片空白。
"""
import json
import os
import re
import shutil
import subprocess
import sys
import time

# 项目根：由本文件位置推导（_e2e/ 的上一层），不写死本机绝对路径。
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HOME = os.path.join(ROOT, "_e2e", "shots_home")
EXE = os.path.join(ROOT, "ModelMux.exe")
FAKE = os.path.join(ROOT, "_e2e", "fake_upstream.py")
# 用当前解释器；换机器/换 Python 位置不必改代码（可用 MODELMUX_PY 覆盖）。
PY = os.environ.get("MODELMUX_PY", sys.executable)
EDGE = r"C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe"
FAKE_PORT = 18092

sys.stdout.reconfigure(encoding="utf-8")

CONFIG = {
    "version": 2,
    "log": {"level": "info"},
    "embedded_providers": [
        {
            "name": "openrouter", "display_name": "OpenRouter", "enabled": True,
            "preset": "openrouter",
            "base_url": f"http://127.0.0.1:{FAKE_PORT}/chat/v1",
            "protocol": "chat", "model_prefix": "or/",
            "api_keys": ["sk-or-v1-abcdefghijklmnop1234", "sk-or-v1-zzzzzzzzzzzzzzzz9999"],
            "models": ["fake-alpha", "fake-beta", {"id": "upstream-x", "alias": "x-alias"}],
            "weight": 1, "priority": 5,
        },
        {
            "name": "claude", "display_name": "Anthropic 官方", "enabled": True,
            "preset": "anthropic",
            "base_url": f"http://127.0.0.1:{FAKE_PORT}/anth",
            "protocol": "anthropic", "model_prefix": "claude/",
            "api_keys": ["sk-ant-api03-xxxxxxxxxxxx"],
            "models": [{"id": "fake-claude", "context": 200000}],
            "weight": 2, "priority": 1,
        },
        {
            "name": "locals", "display_name": "本地 vLLM", "enabled": False,
            "preset": "vllm",
            "base_url": "http://127.0.0.1:8000/v1",
            "protocol": "chat", "model_prefix": "local/",
            "models": [],
        },
    ],
}


def main():
    if os.path.isdir(HOME):
        shutil.rmtree(HOME, ignore_errors=True)  # 仅限 _e2e 下的测试目录
    os.makedirs(HOME, exist_ok=True)
    with open(os.path.join(HOME, "modelmux.json"), "w", encoding="utf-8") as f:
        json.dump(CONFIG, f, ensure_ascii=False, indent=2)

    fake = subprocess.Popen([PY, FAKE, str(FAKE_PORT)],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    mm = None
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
                txt = open(log, encoding="utf-8", errors="replace").read()
                for line in txt.splitlines():
                    if "内置服务已监听" in line:
                        m = re.search(r"127\.0\.0\.1:(\d+)", line)
                        if m:
                            port = int(m.group(1))
                            break
            time.sleep(0.3)
        if not port:
            print("未取得端口")
            return 1
        print(f"端口 {port}")

        # 先拿到 req_id，让日志页有内容可看
        import urllib.request
        for model in ("or/fake-alpha", "claude/fake-claude"):
            body = json.dumps({"model": model, "messages": [{"role": "user", "content": "hi"}]}).encode()
            rq = urllib.request.Request(f"http://127.0.0.1:{port}/v1/chat/completions",
                                        data=body, headers={"Content-Type": "application/json"})
            try:
                urllib.request.urlopen(rq, timeout=20).read()
            except Exception as e:
                print("预热请求失败：", e)

        os.makedirs(os.path.join(ROOT, "_e2e", "shots"), exist_ok=True)
        for name, route in (("overview", ""), ("channels", "#channels"), ("logs", "#logs")):
            out = os.path.join(ROOT, "_e2e", "shots", f"{name}.png")
            if os.path.exists(out):
                os.remove(out)
            subprocess.run([
                EDGE, "--headless=new", "--disable-gpu", "--no-sandbox",
                "--hide-scrollbars", "--force-device-scale-factor=1",
                "--virtual-time-budget=9000", "--window-size=1400,980",
                f"--screenshot={out}",
                f"http://127.0.0.1:{port}/{route}",
            ], capture_output=True, timeout=90)
            print(f"{name}: {'OK' if os.path.exists(out) else '失败'} "
                  f"{os.path.getsize(out) if os.path.exists(out) else 0} 字节")

        print("\n配置目录：", HOME)
    finally:
        if mm and mm.poll() is None:
            mm.kill()
        fake.terminate()
    return 0


if __name__ == "__main__":
    sys.exit(main())
