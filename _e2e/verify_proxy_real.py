#!/usr/bin/env python3
"""真实验证：WorkBuddy 渠道的免密钥账号管理代理。

通过 ModelMux 代理读取真实 wb2api 的 /panel/api/overview，
核对账号数据（与 wb2api 面板一致：昵称/积分/冷却/停用）。
"""
import json
import os
import re
import sys
import time
import urllib.request

sys.stdout.reconfigure(encoding="utf-8")
HOME = os.path.join(os.environ["LOCALAPPDATA"], "ModelMux")
LOG = os.path.join(HOME, "logs", "modelmux.log")

# 只看本次启动之后新增的日志（日志是累积的）
start_offset = os.path.getsize(LOG) if os.path.isfile(LOG) else 0

env = dict(os.environ, MODELMUX_HEADLESS="1")
mm = __import__("subprocess").Popen(
    [os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "ModelMux.exe")],
    env=env, cwd=os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

results = []


def check(name, ok, detail=""):
    results.append((name, ok, detail))
    print("  [%s] %s%s" % ("PASS" if ok else "FAIL", name, (" · " + detail) if detail else ""))


try:
    port = None
    deadline = time.time() + 40
    while time.time() < deadline and not port:
        if os.path.isfile(LOG) and os.path.getsize(LOG) >= start_offset:
            with open(LOG, encoding="utf-8", errors="replace") as f:
                f.seek(start_offset)
                for line in f.read().splitlines():
                    m = re.search(r'"api":"http://127\.0\.0\.1:(\d+)/v1"', line)
                    if m:
                        port = int(m.group(1))
        if not port:
            time.sleep(0.4)
    check("服务已启动（取到动态端口）", bool(port), f"port={port}")
    base = f"http://127.0.0.1:{port}"

    def req(path, method="GET", obj=None, timeout=30):
        data = json.dumps(obj).encode() if obj is not None else None
        headers = {"Content-Type": "application/json"} if data else {}
        r = urllib.request.Request(base + path, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(r, timeout=timeout) as resp:
                return resp.status, json.loads(resp.read() or b"{}")
        except urllib.error.HTTPError as e:
            try:
                return e.code, json.loads(e.read() or b"{}")
            except Exception:
                return e.code, {}

    st, d = req("/api/status")
    check("状态接口 200", st == 200)
    key = d.get("access_key") or ""
    check("access_key 自动生成", len(key) == 32, f"len={len(key)}")

    # 等渠道就绪
    ready, chan = False, None
    for _ in range(60):
        st, d = req("/api/channels")
        for c in d.get("channels") or []:
            if c.get("source") == "managed" and c.get("ready"):
                ready, chan = True, c
                break
        if ready:
            break
        time.sleep(1)
    check("WorkBuddy 渠道就绪", ready, f"name={chan and chan.get('name')}")

    name = chan["name"]

    # ── 免密钥账号管理代理（真实 wb2api /panel/api/overview）──
    st, d = req(f"/api/channels/{name}/upstream/overview", timeout=30)
    check("管理代理 200（真实 wb2api）", st == 200, f"st={st}")
    accts = d.get("accounts") or []
    check("账号列表非空", len(accts) >= 1, f"共 {d.get('total')} 个账号")

    if accts:
        a = accts[0]
        nick = a.get("nickname") or ""
        credits = a.get("credits")
        # 与已知真实数据核对（wb2api 面板同一份数据）
        # 昵称不能硬编码：用户随时会加减账号。只验证「非空且来自真实数据」。
        check("账号昵称非空（来自真实上游）", bool(nick), f"nickname={nick!r}")
        check("积分为数值", isinstance(credits, int), f"credits={credits} total={a.get('credits_total')}")
        check("账号字段齐全（uid/冷却/停用/签到）",
              all(k in a for k in ("uid", "cooling", "disabled", "checkin_done")),
              json.dumps({k: a.get(k) for k in ("uid", "cooling", "disabled", "checkin_done")},
                         ensure_ascii=False))
        print()
        print("  真实账号（经代理，未输任何密钥）：")
        for a in accts:
            state = ("已停用" if a.get("disabled")
                     else ("冷却中 %ss" % a.get("cool_remaining_sec", "?") if a.get("cooling") else "可用"))
            print("    %-20s 积分 %-8s 状态=%s 域=%s 签到=%s" % (
                a.get("nickname") or a.get("uid"), a.get("credits"),
                state, a.get("realm") or "-", "✓" if a.get("checkin_done") else "✗"))

    # 内嵌型渠道的代理应 501（先加一个临时的再删）
    st, d = req("/api/channels", method="POST", obj={
        "kind": "embedded", "name": "", "display_name": "TmpEmbed", "enabled": False,
        "base_url": "http://127.0.0.1:1", "protocol": "chat", "model_prefix": "tmp/",
        "models": ["x"], "api_keys": ["k"],
    })
    tmp = d.get("name", "")
    st, d = req(f"/api/channels/{tmp}/upstream/overview")
    check("内嵌型渠道代理返回 501 not_managed", st == 501, f"st={st}")
    req(f"/api/channels?name={tmp}", method="DELETE")

finally:
    try:
        req("/api/quit", method="POST")
    except Exception:
        pass
    time.sleep(3)
    if mm.poll() is None:
        mm.kill()

passed = sum(1 for _, ok, _ in results if ok)
print(f"\n{'=' * 46}\n{passed}/{len(results)} 通过")
sys.exit(0 if passed == len(results) else 1)
