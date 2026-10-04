#!/usr/bin/env python3
"""把真实的 WorkBuddy 上游作为「托管型渠道」接入 Mergence，并验证整条链路。

独立子进程模式已移除：WorkBuddy 现在是**进程内原生**渠道（内置实现由 kind
选中，Mergence 自己装配上游 handler 并在本进程内起服务）。所以本脚本不再需要
wb2api.exe，也不再写 command / args / dir / port_env_var。

账号从原生实例自己的数据目录读取（<运行根>/data/instances/workbuddy/data/auths）。
该目录为空时会明确「跳过」而不是报错——按迁移约定，账号不复制、由用户在控制台
重新登录后才有。用 WB_DATA_DIR 可指向一个已有的原生实例数据目录。

会用一次 max_tokens=1 的真实请求验证账号可用 —— 消耗可忽略，但会真实走上游。
注意：本脚本会重写运行根里的真实配置（慎跑）。
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

# 原生 workbuddy 的数据目录：渠道未设 data_dir 时即
# <运行根>/data/instances/<渠道名>/data（见 internal/config.DataDir 与
# internal/native/workbuddy）。账号落在它下面的 auths/。
WB_DATA = os.environ.get("WB_DATA_DIR") or os.path.join(
    HOME, "data", "instances", "workbuddy", "data")
WB_AUTHS = os.path.join(WB_DATA, "auths")

sys.stdout.reconfigure(encoding="utf-8")

WB_PROVIDER = {
    "name": "workbuddy",
    "display_name": "WorkBuddy 账号",
    "enabled": True,
    "preset": "workbuddy",
    # 进程内原生：内置实现由 kind 选中；不再有可执行文件 / 命令行 / 工作目录 /
    # 端口环境变量 / 就绪超时（那些字段连同端口分配器一起删掉了）。
    "kind": "workbuddy",
    "mode": "native",
    "health_path": "/healthz",
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
    # 原生实例没有账号就跳过：独立子进程模式移除后，账号不再从 wb2api 目录读取，
    # 按迁移约定由用户在控制台重新登录。这是预期状态，不是回归。
    n_auth = len(os.listdir(WB_AUTHS)) if os.path.isdir(WB_AUTHS) else 0
    if n_auth == 0:
        print("跳过：原生 workbuddy 实例还没有账号 ——", WB_AUTHS)
        print("      独立子进程模式已移除，账号不再从 wb2api 目录读取；")
        print("      请先在 Mergence 面板的「控制台」里登录 WorkBuddy 账号，再跑本脚本。")
        print("      （或用 WB_DATA_DIR 指向一个已有的原生实例数据目录。）")
        return 0
    print("原生 workbuddy 数据目录 =", WB_DATA, "，账号数 =", n_auth)

    cfg = json.load(open(CFG, encoding="utf-8"))
    cfg["managed_providers"] = [WB_PROVIDER]
    json.dump(cfg, open(CFG, "w", encoding="utf-8"), ensure_ascii=False, indent=2)
    print("已把 WorkBuddy 写入托管型渠道配置（kind=workbuddy, mode=native）")

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

        # 等原生 workbuddy 就绪
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
        print("  本地服务地址:", chan.get("base_url"))
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

        # 真实请求：max_tokens=1，验证整条链路（Mergence → 原生 workbuddy → 上游）
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
