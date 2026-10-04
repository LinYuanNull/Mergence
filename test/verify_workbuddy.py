#!/usr/bin/env python3
"""把真实的 WorkBuddy 网关（wb2api.exe）作为「托管型渠道」接入 Mergence，并验证。

这是把「P5 搬迁」换成「托管桥接」之后要做的那一次真机验证：
不搬 wb2api 的源码，只把它当成一个独立进程接进路由，看整条链路能不能通。

会用一次 max_tokens=1 的真实请求验证账号可用 —— 消耗可忽略，但会真实走上游。
"""
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request

# 项目根：由本文件位置推导（test/ 的上一层），不写死本机绝对路径。
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
EXE = os.path.join(ROOT, "Mergence.exe")


# 真实数据目录：与 internal/config.DataDir() 同序 —— MERGENCE_HOME
# → exe 同级（实测可写，便携形态的默认）→ %LOCALAPPDATA%\Mergence。
def real_home():
    v = os.environ.get("MERGENCE_HOME", "").strip()
    if v:
        return v
    try:
        with tempfile.NamedTemporaryFile(dir=ROOT, prefix=".wt", delete=True):
            pass
        return ROOT
    except OSError:
        return os.path.join(os.environ.get("LOCALAPPDATA", ""), "Mergence")


HOME = real_home()
CFG = os.path.join(HOME, "config", "mergence.json")

WB_DIR = os.environ.get(
    "WB_DIR", os.path.join(os.path.dirname(ROOT), "workbuddy2api-panel"))
WB_EXE = os.path.join(WB_DIR, "wb2api.exe")

sys.stdout.reconfigure(encoding="utf-8")

WB_PROVIDER = {
    "name": "workbuddy",
    "display_name": "WorkBuddy 账号",
    "enabled": True,
    "preset": "workbuddy",
    "command": "wb2api.exe",
    "args": [],
    "dir": WB_DIR,
    "port_env_var": "WB2A_LISTEN",
    "health_path": "/healthz",
    "ready_timeout": "60s",
    "shutdown_grace": "8s",
    "panel_path": "/panel/",
    "route": {
        "model_prefix": "wb/",
        "protocol": "chat",
        "path_prefix": "/v1",
        "models_path": "/models",
        "api_key": "1234",
        "models": [],
        "weight": 1,
        "priority": 0,
        "retries": 1,
        "timeout": "120s",
    },
    "env": {
        # 账号目录沿用 wb2api 自己的（账号是用户资产，不该复制一份）；
        # 但池状态另指一份，避免与单独运行的 wb2api 实例互相覆盖。
        "WB2A_STATE_FILE": "./data/state.mergence.json",
    },
}


ACCESS_KEY = ""   # /v1 出口现在要求 Bearer（Key 自动生成，从 /api/status 取）


def req(method, url, obj=None, timeout=30, raw=False):
    data = None
    headers = {}
    if ACCESS_KEY and "/v1/" in url:
        headers["Authorization"] = "Bearer " + ACCESS_KEY
    if obj is not None:
        data = json.dumps(obj).encode()
        headers["Content-Type"] = "application/json"
    rq = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(rq, timeout=timeout) as r:
            b = r.read()
            return r.status, (b.decode("utf-8", "replace") if raw else json.loads(b or b"{}"))
    except urllib.error.HTTPError as e:
        b = e.read()
        try:
            return e.code, json.loads(b or b"{}")
        except Exception:
            return e.code, {"raw": b.decode("utf-8", "replace")}


def main():
    if not os.path.isfile(WB_EXE):
        print("找不到 wb2api.exe：", WB_EXE)
        return 1
    print("wb2api.exe =", WB_EXE)

    cfg = json.load(open(CFG, encoding="utf-8"))
    cfg["managed_providers"] = [WB_PROVIDER]
    json.dump(cfg, open(CFG, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
    print("已把 WorkBuddy 写入托管型渠道配置")

    log = os.path.join(HOME, "data", "logs", "mergence.log")

    # 日志是**累积**的：只看本次启动之后新增的部分。
    # 不然会扫到上一次运行留下的端口，连过去必然是「渠道列表里没有 workbuddy」。
    start_offset = os.path.getsize(log) if os.path.isfile(log) else 0

    env = dict(os.environ)
    env["MERGENCE_HEADLESS"] = "1"
    mm = subprocess.Popen([EXE], env=env)
    try:
        port = None
        deadline = time.time() + 40
        while time.time() < deadline and not port:
            if os.path.isfile(log) and os.path.getsize(log) >= start_offset:
                with open(log, encoding="utf-8", errors="replace") as f:
                    f.seek(start_offset)
                    fresh = f.read()
                for line in fresh.splitlines():
                    m = re.search(r'"api":"http://127\.0\.0\.1:(\d+)/v1"', line)
                    if m:
                        port = int(m.group(1))
            time.sleep(0.4)
        if not port:
            print("没取到 Mergence 端口，进程是否退出？")
            return 1
        base = f"http://127.0.0.1:{port}"
        print("Mergence 端口 =", port)

        # /v1 现在要求 Bearer：先取自动生成的 Key
        st, d = req("GET", f"http://127.0.0.1:{port}/api/status")
        assert st == 200, f"状态接口失败：{st}"
        globals()["ACCESS_KEY"] = d.get("access_key") or ""
        assert ACCESS_KEY, "access_key 缺失"
        print("access_key 已取得（自动生成）")

        # 等 WorkBuddy 子进程就绪
        chan, deadline = None, time.time() + 90
        while time.time() < deadline:
            st, d = req("GET", base + "/api/channels")
            chan = next((c for c in d.get("channels", []) if c["name"] == "workbuddy"), None)
            if chan and chan.get("ready"):
                break
            time.sleep(1.0)

        if not chan:
            print("渠道列表里没有 workbuddy")
            return 1
        print("\n=== 渠道状态 ===")
        print("  来源:", chan.get("source"), " 就绪:", chan.get("ready"))
        print("  子进程地址:", chan.get("base_url"))
        print("  自带面板:", chan.get("panel_url"))
        if not chan.get("ready"):
            print("  未就绪原因:", chan.get("ready_reason"))
            print("\n=== 日志尾部 ===")
            tail = open(log, encoding="utf-8", errors="replace").read().splitlines()[-12:]
            for l in tail:
                print("   ", l[:220])
            return 1

        # 等模型目录被抓到
        models = []
        deadline = time.time() + 30
        while time.time() < deadline:
            st, d = req("GET", base + "/v1/models")
            models = [m["id"] for m in d.get("data", []) if m["id"].startswith("wb/")]
            if models:
                break
            time.sleep(1.0)
        print("\n=== 聚合模型（%d 个，前 8 个）===" % len(models))
        for m in models[:8]:
            print("   ", m)

        if not models:
            print("没有拉到任何 wb/ 模型 —— 账号可能未加载或上游目录为空")
            return 1

        # 真实请求：max_tokens=1，验证整条链路（Mergence → wb2api → WorkBuddy）
        target = models[0]
        print(f"\n=== 真实调用（max_tokens=1）：{target} ===")
        st, d = req("POST", base + "/v1/chat/completions", {
            "model": target,
            "messages": [{"role": "user", "content": "ping"}],
            "max_tokens": 1,
        }, timeout=120)
        print("  HTTP", st)
        if st == 200:
            ch = (d.get("choices") or [{}])[0]
            print("  model:", d.get("model"))
            print("  usage:", d.get("usage"))
            print("  finish_reason:", ch.get("finish_reason"))
            content = (ch.get("message") or {}).get("content") or ""
            print("  内容片段:", repr(content[:80]))
            print("\n[PASS] WorkBuddy 账号已通过 Mergence 对外提供服务")
        else:
            print("  响应:", json.dumps(d, ensure_ascii=False)[:600])
            print("\n[FAIL] 真实调用未成功")
            return 1

        # 顺带确认「上游面板可直达」
        st, _ = req("GET", chan.get("panel_url") or "", raw=True, timeout=10) if chan.get("panel_url") else (0, "")
        print("  上游面板可访问:", st == 200, chan.get("panel_url"))

        print("\n=== 退出 ===")
        req("POST", base + "/api/quit", {})
        try:
            mm.wait(timeout=25)
            print("Mergence 已退出")
        except subprocess.TimeoutExpired:
            print("Mergence 未在 25s 内退出")
            return 1
    finally:
        if mm.poll() is None:
            mm.kill()
    return 0


if __name__ == "__main__":
    sys.exit(main())
