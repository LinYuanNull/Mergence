#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""驱动器：起一个 1234 常驻隔离实例（gateway + zcode 两个渠道各一），
在同一次进程内跑 verify_side_console.py 与 verify_layout_ui.py，最后 quit。

这两个套件的 BASE 硬编码 127.0.0.1:1234，而它们**不自起实例**；1234 上的实例
若由 Bash 的 `&` 起，只活到本次 Bash 调用结束 ⇒ 必须把「起实例 + 跑套件 + quit」
写在同一次调用里，本脚本就是为了把那套手工配方固化下来。

用法：
    python test/run_ui1234.py                 # 两个套件都跑
    ONLY=layout python test/run_ui1234.py     # 只跑 verify_layout_ui.py（A/B 用）
环境变量：
    MERGENCE_EXE  指向被测 exe（缺省用仓库根的 Mergence.exe）
    MERGENCE_PY   指向 python 解释器（缺省用当前解释器）
"""
import json
import os
import re
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
EXE = os.environ.get("MERGENCE_EXE") or os.path.join(ROOT, "Mergence.exe")
HOME = os.path.join(HERE, "ui1234_home")
PY = os.environ.get("MERGENCE_PY") or sys.executable
FAKE_MG = os.path.join(HERE, "fake_managed_gateway.py")
FAKE_ZC = os.path.join(HERE, "fake_zcode.py")
ZCODE_ADMIN_KEY = "fake-admin-key-123"
ZCODE_PORT = 18107          # 假 zcode 网关
MG_PORT = 18108             # 假通用网关（外部接管：测试自己起，Mergence 不拉起）
MG_PIDFILE = os.path.join(HERE, "ui1234_mg.pid")
sys.stdout.reconfigure(encoding="utf-8")


def req(method, url, obj=None, timeout=120):
    data = json.dumps(obj).encode() if obj is not None else None
    rq = urllib.request.Request(url, data=data,
                                headers={"Content-Type": "application/json"},
                                method=method)
    try:
        with urllib.request.urlopen(rq, timeout=timeout) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read() or b"{}")
        except Exception:
            return e.code, {}


def wait_tcp(port, timeout=15):
    """等某个回环端口能连上（假网关起好了）。"""
    import socket
    end = time.time() + timeout
    while time.time() < end:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.5):
                return True
        except OSError:
            time.sleep(0.2)
    return False


def start_fake(cmd, port, env_extra=None, timeout=15):
    """起一个假网关并等它就绪。

    独立子进程模式移除后，托管渠道改走「外部接管」：假网关由**测试自己**起成
    独立服务（和用户自己部署一套网关的情形一致），再用 MERGENCE_EXTERNAL_URL
    把它接进 Mergence。所以测试要负责它的生命周期。
    """
    env = dict(os.environ)
    if env_extra:
        env.update(env_extra)
    p = subprocess.Popen(cmd, env=env,
                         stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if not wait_tcp(port, timeout):
        raise RuntimeError(f"假网关未在 {timeout}s 内监听 {port}：{cmd}")
    return p


def external_env(port):
    """构造「外部接管」渠道的 env：告诉 Mergence 上游已经在哪。"""
    return {"MERGENCE_EXTERNAL_URL": f"http://127.0.0.1:{port}"}


def wait_port(log, timeout=40):
    end = time.time() + timeout
    port = None
    while time.time() < end:
        if os.path.isfile(log):
            for line in open(log, encoding="utf-8", errors="replace"):
                if "内置服务已监听" in line:
                    m = re.search(r"127\.0\.0\.1:(\d+)", line)
                    if m:
                        port = int(m.group(1))
            if port:
                return port
        time.sleep(0.3)
    return None


def clean():
    for rel in ("config", "data/logs/mergence.log", "data/usage"):
        p = os.path.join(HOME, *rel.split("/"))
        if os.path.isdir(p):
            shutil.rmtree(p, ignore_errors=True)
        elif os.path.isfile(p):
            try:
                os.remove(p)
            except OSError:
                pass
    os.makedirs(os.path.join(HOME, "config"), exist_ok=True)
    os.makedirs(os.path.join(HOME, "data", "usage"), exist_ok=True)


def main():
    clean()
    # 预置配置：panel_port 钉死 1234（两个 UI 套件的 BASE 硬编码它），
    # claim.admin_key 与假 zcode 网关的后台密码一致。
    cfg = {
        "version": 3,
        "ports": {"panel_port": 1234},
        "claim": {"enabled": False, "at": "12:01", "window": 4,
                  "channel": "", "admin_key": ZCODE_ADMIN_KEY},
    }
    with open(os.path.join(HOME, "config", "mergence.json"), "w",
              encoding="utf-8") as f:
        json.dump(cfg, f, ensure_ascii=False, indent=2)

    # 种一条今日用量，喂饱概览的 `.fold` 断言（否则 has_data=false → 空态）
    today = time.strftime("%Y-%m-%d")
    rec = {"time": time.strftime("%Y-%m-%dT%H:%M:%S+08:00"), "channel": "workbuddy",
           "source": "managed", "model": "mg/wb-alpha", "upstream_model": "wb-alpha",
           "usage": {"prompt_tokens": 12, "completion_tokens": 30, "total_tokens": 42},
           "ok": True, "status": 200, "duration_ms": 120, "stream": False}
    with open(os.path.join(HOME, "data", "usage", "usage-%s.jsonl" % today), "w",
              encoding="utf-8") as f:
        f.write(json.dumps(rec, ensure_ascii=False) + "\n")

    env = dict(os.environ)
    env["MERGENCE_HOME"] = HOME
    env["MERGENCE_HEADLESS"] = "1"
    for k in list(env):
        if k.lower().endswith("_proxy"):
            env.pop(k, None)
    env["NO_PROXY"] = "127.0.0.1,localhost"
    env["no_proxy"] = "127.0.0.1,localhost"

    # 假网关由**测试自己**起成独立服务；Mergence 通过「外部接管」把它们接进来
    # （独立子进程模式已移除，Mergence 不再拉起任何上游）。
    if os.path.exists(MG_PIDFILE):
        os.remove(MG_PIDFILE)
    fake_mg = start_fake([PY, FAKE_MG], MG_PORT,
                         env_extra={"PORT": str(MG_PORT), "MG_PIDFILE": MG_PIDFILE})
    fake_zc = start_fake([PY, FAKE_ZC, str(ZCODE_PORT)], ZCODE_PORT)

    mm = subprocess.Popen([EXE], env=env,
                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        port = wait_port(os.path.join(HOME, "data", "logs", "mergence.log"))
        if not port:
            print("!! Mergence 未启动")
            return 1
        base = "http://127.0.0.1:%d" % port
        print("== Mergence 端口", port)

        # gateway 渠道：名字必须是 workbuddy（两个套件都按此名取数据）。
        # gateway_kind=workbuddy 决定控制台形态为通用 gateway 视图。
        gw = {
            "kind": "managed", "name": "workbuddy", "display_name": "WorkBuddy",
            "gateway_kind": "workbuddy", "enabled": True,
            "health_path": "/healthz", "panel_path": "/panel/",
            "expose": True, "protocol": "chat",
            "model_prefix": "mg", "api_keys": ["1234"],
            "models": ["wb-alpha", "wb-beta"], "weight": 1,
            "env": external_env(MG_PORT),
        }
        st, d = req("POST", base + "/api/channels", gw)
        print("== 建 gateway 渠道 workbuddy:", st, d.get("ok"), d.get("start_error", ""))

        # zcode 渠道：gateway_kind=zcode 决定控制台形态与面板前缀；
        # preset 留空 ⇒ 模拟「升级前的老配置」⇒ 服务端回 preset_inferred=true，
        # 界面才会显示「模板…推断」那句（verify_layout_ui 3c 的那条断言依赖它）。
        # 路由密钥 = 假 zcode 网关的后台密码（原生型下两者合一）。
        zc = {
            "kind": "managed", "name": "zcode", "display_name": "ZCode 网关",
            "preset": "", "gateway_kind": "zcode", "enabled": True,
            "health_path": "/meta", "panel_path": "/admin/",
            "model_prefix": "zcode-",
            "expose": True, "protocol": "chat", "models": ["glm-4.6"],
            "api_keys": [ZCODE_ADMIN_KEY],
            "env": external_env(ZCODE_PORT),
        }
        st, d = req("POST", base + "/api/channels", zc)
        print("== 建 zcode 渠道:", st, d.get("ok"), d.get("start_error", ""))

        # 等两个渠道就绪
        for _ in range(80):
            _, dd = req("GET", base + "/api/channels")
            chans = {c["name"]: c for c in dd.get("channels", [])}
            if len(chans) >= 2 and all(c.get("ready") for c in chans.values()):
                break
            time.sleep(0.5)
        _, dd = req("GET", base + "/api/channels")
        for c in dd.get("channels", []):
            print("   渠道 %-10s ready=%-5s kind=%-9s console=%s" % (
                c.get("name"), c.get("ready"), c.get("kind"), c.get("console_kind")))

        # 跑两个套件（ONLY 环境变量可只跑一个，用于 A/B）
        only = os.environ.get("ONLY")
        names = [n for n in ("verify_side_console.py", "verify_layout_ui.py")
                 if not only or only in n]
        env2 = dict(env)
        for name in names:
            p = os.path.join(HERE, name)
            print("\n" + "=" * 70)
            print("== 跑 " + name)
            print("=" * 70)
            r = subprocess.run([PY, p], env=env2, capture_output=True, text=True,
                               encoding="utf-8", errors="replace")
            out = r.stdout or ""
            print(out)
            if r.stderr:
                print("--- stderr ---\n" + r.stderr)
            npass = out.count("[PASS]")
            nfail = out.count("[FAIL]")
            nskip = out.count("[跳过]")
            print("== %s: PASS=%d FAIL=%d 跳过=%d rc=%d" % (name, npass, nfail, nskip,
                                                            r.returncode))

        # 收尾：优雅退出
        try:
            req("POST", base + "/api/quit", {}, timeout=10)
            print("== 已 POST /api/quit")
        except Exception as e:
            print("== quit 失败:", e)
        time.sleep(1.0)
        return 0
    finally:
        # 假网关归测试所有，收尾时要自己收掉（Mergence 不管它们）。
        for p in (mm, fake_mg, fake_zc):
            try:
                if p and p.poll() is None:
                    p.terminate()
            except Exception:
                pass


if __name__ == "__main__":
    sys.exit(main())
