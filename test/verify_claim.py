#!/usr/bin/env python3
"""领取链路的真实验证：起一个假 zcode2api 网关 + 一个托管渠道指向它，
然后走完整的 ModelMux 领取流程。

与 verify.py 的差别：那边只验「配置能存、没渠道时给明确原因」，
这边要验**真的领到了**——包括 Bearer 鉴权、回执翻译、1005 名额用完的
next_at 透传、密码错误时的提示。接口契约严格按 dengyie/zcode2api 仿真。
"""
import json
import os
import re
import shutil
import subprocess
import sys
import time
import urllib.request

# 项目根：由本文件位置推导（test/ 的上一层），不写死本机绝对路径。
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HOME = os.path.join(ROOT, "test", "claim_home")
EXE = os.path.join(ROOT, "ModelMux.exe")
FAKE_ZCODE = os.path.join(ROOT, "test", "fake_zcode.py")
# 用当前解释器；换机器/换 Python 位置不必改代码（可用 MODELMUX_PY 覆盖）。
PY = os.environ.get("MODELMUX_PY", sys.executable)
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


def wait_port(proc, log, timeout=30):
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


def main():
    if os.path.isdir(HOME):
        shutil.rmtree(HOME, ignore_errors=True)
    os.makedirs(os.path.join(HOME, "config"), exist_ok=True)

    # 假网关**由 ModelMux 托管启动**（和真实用法一致）：它注入 ZCODE_PORT，
    # 假网关据此监听。不手动起是因为托管渠道的地址由 ModelMux 分配，
    # 手动起的固定端口对不上。
    gz = None

    env = dict(os.environ)
    env["MODELMUX_HOME"] = HOME
    env["MODELMUX_HEADLESS"] = "1"
    mm = subprocess.Popen([EXE], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    base = None
    try:
        port = wait_port(mm, os.path.join(HOME, "data", "logs", "modelmux.log"))
        if not port:
            print("ModelMux 未启动")
            return 1
        base = f"http://127.0.0.1:{port}"
        print("ModelMux 端口", port)

        # 托管渠道：直接指向假网关（固定端口，省掉子进程编排）
        print("→ 创建托管渠道（会拉起子进程，给足超时）", flush=True)
        st, d = post(base, "/api/channels", {
            "kind": "managed",
            "name": "", "display_name": "ZCode 网关", "preset": "zcode",
            "command": PY, "args": [FAKE_ZCODE, str(ZCODE_PORT)],
            "dir": os.path.dirname(FAKE_ZCODE), "enabled": True, "expose": False,
            "port_env_var": "ZCODE_PORT", "health_path": "/meta", "panel_path": "/admin/",
            "model_prefix": "zcode-", "expose": True, "protocol": "chat",
            "models": ["glm-4.6"],
        }, timeout=150)
        check("托管渠道创建成功（zcode 预设）", st == 200 and d.get("ok"), f"{st} {d}")
        ch_name = d.get("name", "")

        # 等编排器把子进程拉起来
        ready = False
        for _ in range(40):
            time.sleep(0.5)
            st2, ch = get(base, "/api/channels")
            for c in ch.get("channels", []):
                if c.get("name") == ch_name and c.get("ready"):
                    ready = True
                    break
            if ready:
                break
        check("zcode 子进程就绪（探活 /meta 命中）", ready,
              json.dumps(ch, ensure_ascii=False)[:200])

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

        # 后台密码错误 → 必须明确提示，不能静默
        post(base, "/api/claim/config", {"admin_key": "wrong-key"})
        st, d = post(base, "/api/claim/now", {}, timeout=60)
        check("后台密码错误时明确报错",
              st == 200 and "密码" in (d.get("error") or "") + (d.get("skipped") or ""),
              f"{st} {json.dumps(d, ensure_ascii=False)[:200]}")

        # 恢复正确密码，把子进程换成「名额用完」模式（改 args 会触发重启）
        post(base, "/api/claim/config", {"admin_key": ADMIN_KEY})
        st, d = post(base, "/api/channels", {
            "kind": "managed", "name": ch_name, "display_name": "ZCode 网关",
            "preset": "zcode", "command": PY, "args": [FAKE_ZCODE, "--fail-code", "1005"],
            "dir": os.path.dirname(FAKE_ZCODE), "enabled": True, "expose": False,
            "port_env_var": "ZCODE_PORT", "health_path": "/meta", "panel_path": "/admin/",
            "model_prefix": "zcode-", "expose": True, "protocol": "chat",
            "models": ["glm-4.6"],
        }, timeout=150)
        check("切到「名额用完」模式", st == 200 and d.get("ok"),
              f"{st} {json.dumps(d, ensure_ascii=False)[:200]}")
        for _ in range(40):
            time.sleep(0.5)
            st2, ch2 = get(base, "/api/channels")
            if any(c.get("name") == ch_name and c.get("ready") for c in ch2.get("channels", [])):
                break
        st, d = post(base, "/api/claim/now", {}, timeout=60)
        check("名额用完时逐账号回执失败",
              st == 200 and d.get("ok") == 0 and d.get("fail") == 2,
              f"{st} {json.dumps(d, ensure_ascii=False)[:300]}")
        outs = d.get("outcomes") or []
        check("回执透传名额恢复时间 next_at（用户据此知道何时再试）",
              len(outs) == 2 and all(o.get("next_at") for o in outs),
              json.dumps(outs, ensure_ascii=False)[:200])

    finally:
        for p in (mm, gz):
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
