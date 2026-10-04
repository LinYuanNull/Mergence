#!/usr/bin/env python3
"""领取链路的真实验证：测试自己起一个假 zcode2api 网关，用「外部接管」把它接进
一个托管渠道，然后走完整的 Mergence 领取流程。

独立子进程模式已移除：托管渠道不再由 Mergence 拉起，而是接管一个**已经在运行**
的外部网关（渠道 env 里的 MERGENCE_EXTERNAL_URL）。所以这里由测试自己把假网关
起成独立服务 —— 与「用户已部署一套 zcode2api」的用法完全一致。

与 verify.py 的差别：那边只验「配置能存、没渠道时给明确原因」，
这边要验**真的领到了**——包括 Bearer 鉴权、回执翻译、1005 名额用完的
next_at 透传、密码错误时的提示。接口契约严格按 dengyie/zcode2api 仿真。
"""
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import time
import urllib.request

# 项目根：由本文件位置推导（test/ 的上一层），不写死本机绝对路径。
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HOME = os.path.join(ROOT, "test", "claim_home")
EXE = os.path.join(ROOT, "Mergence.exe")
FAKE_ZCODE = os.path.join(ROOT, "test", "fake_zcode.py")
# 用当前解释器；换机器/换 Python 位置不必改代码（可用 MERGENCE_PY 覆盖）。
PY = os.environ.get("MERGENCE_PY", sys.executable)
ZCODE_PORT = 18102
ADMIN_KEY = "fake-admin-key-123"

sys.stdout.reconfigure(encoding="utf-8")
results = []


def check(name, cond, detail=""):
    results.append((name, bool(cond), detail))
    print(f"[{'PASS' if cond else 'FAIL'}] {name}" + (f"\n        {detail}" if not cond and detail else ""),
          flush=True)


def post(base, path, obj, timeout=30):
    rq = urllib.request.Request(base + path, data=json.dumps(obj).encode(),
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(rq, timeout=timeout) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


def get(base, path, timeout=30):
    try:
        with urllib.request.urlopen(base + path, timeout=timeout) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


def wait_tcp(port, timeout=15):
    """等回环端口能连上（假网关起好了）。"""
    end = time.time() + timeout
    while time.time() < end:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.5):
                return True
        except OSError:
            time.sleep(0.2)
    return False


def start_fake(cmd, port, timeout=15):
    """起假网关并等它就绪。外部接管下假网关归测试所有。"""
    p = subprocess.Popen(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if not wait_tcp(port, timeout):
        raise RuntimeError(f"假网关未在 {timeout}s 内监听 {port}：{cmd}")
    return p


def wait_port(proc, log, timeout=30):
    """等 Mergence 自己的面板端口（从日志里取）。"""
    end = time.time() + timeout
    while time.time() < end:
        if os.path.isfile(log):
            try:
                for line in open(log, encoding="utf-8", errors="replace"):
                    if "内置服务已监听" in line:
                        m = re.search(r"127\.0\.0\.1:(\d+)", line)
                        if m:
                            return int(m.group(1))
            except Exception:
                pass
        if proc.poll() is not None:
            return None
        time.sleep(0.3)
    return None


def channel_cfg(name, api_key):
    """一条「外部接管假 zcode 网关」的托管渠道配置。

    api_key 就是网关后台密码（原生型下两者合并成 route key）。
    """
    return {
        "kind": "managed", "name": name, "display_name": "ZCode 网关",
        "preset": "zcode", "gateway_kind": "zcode",
        "enabled": True, "expose": True,
        "health_path": "/meta", "panel_path": "/admin/",
        "model_prefix": "zcode-", "protocol": "chat",
        "models": ["glm-4.6"], "api_keys": [api_key],
        "env": {"MERGENCE_EXTERNAL_URL": f"http://127.0.0.1:{ZCODE_PORT}"},
    }


def wait_ready(base, name, tries=40):
    for _ in range(tries):
        time.sleep(0.5)
        _st, ch = get(base, "/api/channels")
        for c in ch.get("channels", []):
            if c.get("name") == name and c.get("ready"):
                return True
    return False


def main():
    if os.path.isdir(HOME):
        shutil.rmtree(HOME, ignore_errors=True)
    os.makedirs(os.path.join(HOME, "config"), exist_ok=True)

    # 假网关由**测试自己**起成独立服务；Mergence 通过外部接管把它接进来。
    fake = start_fake([PY, FAKE_ZCODE, str(ZCODE_PORT)], ZCODE_PORT)

    env = dict(os.environ)
    env["MERGENCE_HOME"] = HOME
    env["MERGENCE_HEADLESS"] = "1"
    mm = subprocess.Popen([EXE], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    base = None
    try:
        port = wait_port(mm, os.path.join(HOME, "data", "logs", "mergence.log"))
        if not port:
            print("Mergence 未启动")
            return 1
        base = f"http://127.0.0.1:{port}"
        print("Mergence 端口", port)

        print("→ 创建托管渠道（外部接管假 zcode 网关）", flush=True)
        st, d = post(base, "/api/channels", channel_cfg("", ADMIN_KEY), timeout=60)
        check("托管渠道创建成功（zcode 预设 + 外部接管）", st == 200 and d.get("ok"), f"{st} {d}")
        ch_name = d.get("name", "")

        check("zcode 渠道就绪（探活 /meta 命中）", wait_ready(base, ch_name),
              f"渠道 {ch_name} 未就绪")

        # 打开开关 + 填后台密码
        st, d = post(base, "/api/claim/config", {
            "enabled": True, "at": "12:01", "window": 4,
            "channel": "", "admin_key": ADMIN_KEY,
        })
        check("领取开关已打开且密码已填", st == 200 and d.get("enabled") and d.get("configured"),
              f"{st} {d}")

        # 自动识别：应挑中这个 zcode 渠道
        st, d = post(base, "/api/claim/now", {}, timeout=60)
        check("手动领取成功（走真实 HTTP 契约）",
              st == 200 and d.get("ok") == 2 and d.get("fail") == 0,
              f"{st} {json.dumps(d, ensure_ascii=False)[:300]}")
        check("回执带账号明细与套餐名",
              len(d.get("outcomes") or []) == 2  # 明细不能为空
              and all(o.get("account") for o in d["outcomes"])
              and (d["outcomes"][0].get("plan") or "").startswith("限时免费"),
              json.dumps(d.get("outcomes"), ensure_ascii=False)[:200])
        check("结果标明是手动触发", d.get("trigger") == "manual", str(d.get("trigger")))

        # 状态接口能看到这次结果
        st, s2 = get(base, "/api/claim")
        check("状态接口回显最近一次结果",
              st == 200 and s2.get("last", {}).get("ok") == 2
              and s2.get("today_done") is True,
              f"{st} {json.dumps(s2, ensure_ascii=False)[:200]}")

        # 后台密码（= 渠道 route key）错误 → 必须明确提示，不能静默
        post(base, "/api/channels", channel_cfg(ch_name, "wrong-key"), timeout=60)
        wait_ready(base, ch_name)
        st, d = post(base, "/api/claim/now", {}, timeout=60)
        check("后台密码错误时明确报错",
              st == 200 and "密码" in (d.get("error") or "") + (d.get("skipped") or ""),
              f"{st} {json.dumps(d, ensure_ascii=False)[:200]}")

        # 恢复正确密码，并把假网关换成「名额用完」模式（重起外部网关）
        post(base, "/api/channels", channel_cfg(ch_name, ADMIN_KEY), timeout=60)
        wait_ready(base, ch_name)
        fake.terminate()
        try:
            fake.wait(timeout=10)
        except Exception:
            pass
        fake = start_fake([PY, FAKE_ZCODE, str(ZCODE_PORT), "--fail-code", "1005"], ZCODE_PORT)
        st, d = post(base, "/api/channels/action", {"name": ch_name, "action": "restart"}, timeout=60)
        check("切到「名额用完」模式（重起外部网关 + 重启渠道）", st == 200 and d.get("ok"),
              f"{st} {json.dumps(d, ensure_ascii=False)[:200]}")
        wait_ready(base, ch_name)
        st, d = post(base, "/api/claim/now", {}, timeout=60)
        check("名额用完时逐账号回执失败",
              st == 200 and d.get("ok") == 0 and d.get("fail") == 2,
              f"{st} {json.dumps(d, ensure_ascii=False)[:300]}")
        outs = d.get("outcomes") or []
        check("回执透传名额恢复时间 next_at（用户据此知道何时再试）",
              len(outs) == 2 and all(o.get("next_at") for o in outs),
              json.dumps(outs, ensure_ascii=False)[:200])

    finally:
        for p in (mm, fake):
            try:
                if p and p.poll() is None:
                    p.terminate()
            except Exception:
                pass

    passed = sum(1 for _, ok, _ in results if ok)
    print(f"\n{'=' * 46}\n{passed}/{len(results)} 通过")
    failed = [n for n, ok, _ in results if not ok]
    if failed:
        print("失败项：\n  - " + "\n  - ".join(failed))
    return 0 if passed == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())
