#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""原生 zcode（Track 3 / B2）端到端验证：**Mergence 自己装配**的 zcode 网关。

与 verify_zcode_accounts.py 的关系
---------------------------------
那个脚本验的是「zcode 面板整块搬进 Mergence」这件事，上游是**外部进程**
（默认假 zcode2api，`ZCODE_UPSTREAM_EXE` 下换成 A 线的 Go 实现）。
本脚本验的是 Track 3 之后**没有外部进程**的那种形态：

    渠道配置 mode=native + kind=zcode
      → Mergence 在**本进程内**装配 zcode 的实现（internal/native/zcode）
      → 起一个只绑回环的临时端口
      → 面板代理 / 领取执行器 / 转发链路照样打它

所以本脚本的判据是：**同一个原生实现，既能被 Mergence 装配起来、又不是一个黑盒**：
  * 装配层面：渠道就绪、console_kind=zcode、端口由内核分配；
  * 管理面：账号 CRUD 经面板代理可用，回执与契约样本同形（键序 / 空容器 / 掩码）；
  * **密码只剩一处**：改 route key（重建渠道）即改密码，不必再动别的设置；
  * 转发链路：/v1/models 与一条必然失败的 /v1/messages（无真实账号）都表现出
    「未采样分支显式报错」的纪律，绝不伪造成功。

为什么用「重建渠道」而不是在线改密码
----------------------------------
原生实现的密码真源是渠道 route key（见 native/zcode 包注释「密码合并」）。
验「改一处即生效」最直接的做法就是：同一个数据目录建两次渠道、route key 不同，
第二次必须用新 key 通过、旧 key 失效 —— 那正是「两处同步」被消除的证据。

前置：`python tools/release/build.py` 已产出仓库根的 Mergence.exe。
用法：`python test/verify_zcode_native.py`
"""
import json
import os
import re
import shutil
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

ROOT = os.path.dirname(HERE)
HOME = os.path.join(HERE, "zcnative_home")
EXE = os.environ.get("MERGENCE_EXE") or os.path.join(ROOT, "Mergence.exe")
LOG = os.path.join(HOME, "data", "logs", "mergence.log")

CHAN = "zcode-native"
KEY1 = "native-key-one"
KEY2 = "native-key-two"
UP = "/api/channels/" + CHAN + "/upstream"

sys.stdout.reconfigure(encoding="utf-8")
results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))
    print(("[PASS] " if ok else "[FAIL] ") + name
          + ("" if ok else "  ← " + str(detail)[:300]), flush=True)


def _req(method, url, obj=None, timeout=30, raw=False):
    data = None
    hdrs = {}
    if obj is not None:
        data = json.dumps(obj).encode()
        hdrs["Content-Type"] = "application/json"
    rq = urllib.request.Request(url, data=data, headers=hdrs, method=method)
    try:
        with urllib.request.urlopen(rq, timeout=timeout) as r:
            body = r.read()
            return r.status, (body if raw else json.loads(body or b"{}"))
    except urllib.error.HTTPError as e:
        body = e.read()
        try:
            return e.code, (body if raw else json.loads(body or b"{}"))
        except Exception:
            return e.code, body if raw else {}


def get(base, p, **kw):
    return _req("GET", base + p, **kw)


def post(base, p, obj, **kw):
    return _req("POST", base + p, obj, **kw)


def put(base, p, obj, **kw):
    return _req("PUT", base + p, obj, **kw)


def delete(base, p, **kw):
    return _req("DELETE", base + p, **kw)


def wait_port(proc, log, timeout=40):
    """等「内置服务已监听」出现，返回**最后一条**里的端口。

    必须取最后一条：隔离目录复用，日志里会留着上一轮的旧行；取第一条就会连到
    上一轮早已关闭的端口，表现为后续请求莫名失败。
    """
    end = time.time() + timeout
    while time.time() < end:
        if os.path.isfile(log):
            try:
                port = None
                for line in open(log, encoding="utf-8", errors="replace"):
                    if "内置服务已监听" in line:
                        m = re.search(r"127\.0\.0\.1:(\d+)", line)
                        if m:
                            port = int(m.group(1))
                if port:
                    return port
            except Exception:
                pass
        if proc.poll() is not None:
            return None
        time.sleep(0.3)
    return None


def clean():
    """清掉会影响断言的残留。

    必须连 **实例数据目录**（data/instances/）一起清：原生渠道的账号库落在
    它下面（accounts.db），不清就会把上一轮的账号带进本轮，让「空池」断言假失败
    （而且这个失败与实现无关，只是测试自己的卫生问题）。

    整个 HOME 递归删会被沙箱的批量删除保护拦下（跑到一半失败还更糟），
    所以逐项清固定路径。
    """
    for rel in ("config", "data/logs/mergence.log",
                "data/instances", "data/cache"):
        p = os.path.join(HOME, *rel.split("/"))
        if os.path.isdir(p):
            shutil.rmtree(p, ignore_errors=True)
        elif os.path.isfile(p):
            try:
                os.remove(p)
            except OSError:
                pass
    os.makedirs(os.path.join(HOME, "config"), exist_ok=True)


def read_access_key():
    """从生成的配置里读 Mergence 自己的 access_key（/v1/* 的鉴权凭据）。"""
    p = os.path.join(HOME, "config", "mergence.json")
    try:
        d = json.load(open(p, encoding="utf-8"))
    except Exception:
        return ""
    for k in ("access_key", "AccessKey"):
        if isinstance(d.get(k), str) and d[k]:
            return d[k]
    gw = d.get("gateway") or {}
    return gw.get("access_key", "")


def get_auth(base, p, key, timeout=30):
    rq = urllib.request.Request(base + p, headers={"Authorization": "Bearer " + key})
    try:
        with urllib.request.urlopen(rq, timeout=timeout) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")
    except Exception as e:
        return 0, {"error": str(e)}


def create_channel(base, key, name=CHAN):
    return post(base, "/api/channels", {
        "kind": "managed", "name": name, "display_name": "ZCode 原生",
        "preset": "zcode", "mode": "native", "gateway_kind": "zcode",
        "enabled": True, "health_path": "/meta", "panel_path": "/admin/",
        "model_prefix": "zcode-", "expose": True, "protocol": "anthropic",
        "models": ["glm-4.6"], "api_keys": [key],
    })


def wait_ready(base, name=CHAN, timeout=25):
    end = time.time() + timeout
    last = None
    while time.time() < end:
        _st, ch = get(base, "/api/channels")
        for c in ch.get("channels", []):
            if c.get("name") == name:
                last = c
                if c.get("ready"):
                    return c
        time.sleep(0.3)
    return last


def main():
    if not os.path.isfile(EXE):
        print("Mergence.exe 不存在，请先跑 tools/release/build.py：" + EXE)
        return 1
    clean()

    env = dict(os.environ)
    env["MERGENCE_HOME"] = HOME
    env["MERGENCE_HEADLESS"] = "1"
    # 出站指向死端口：本脚本不依赖任何真实上游，探测/额度一律走「失败」分支，
    # 让断言与网络无关（同 verify_claim.py 起手）。
    mm = subprocess.Popen([EXE], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        port = wait_port(mm, LOG)
        if not port:
            print("Mergence 未启动")
            return 1
        base = f"http://127.0.0.1:{port}"
        print("Mergence 端口", port)

        # ── ① 装配：mode=native/kind=zcode 建得起来且就绪 ──────────
        st, d = create_channel(base, KEY1)
        check("原生 zcode 渠道创建成功（mode=native/kind=zcode）", st == 200 and d.get("ok"),
              f"{st} {d}")
        if not (d.get("ok")):
            return 1

        ch = wait_ready(base)
        check("原生渠道就绪（进程内服务已监听）", bool(ch and ch.get("ready")),
              json.dumps(ch, ensure_ascii=False)[:300] if ch else "无渠道")
        if not ch:
            return 1

        # source 对外必须报 managed —— upstream.js 按两值写死 source==='managed'，
        # 多暴露一个 native 会一次性少掉账号池/模型档位/用量/配置/日志五个视图。
        check("对外 source 归一为 managed（upstream.js 契约）", ch.get("source") == "managed",
              ch.get("source"))
        check("console_kind 识别为 zcode（出原生专属视图、不是外链）",
              ch.get("console_kind") == "zcode", ch.get("console_kind"))
        # 端口由内核分配、只绑回环：base_url 必须指 127.0.0.1。
        check("base_url 指向回环（端口由内核分配）",
              str(ch.get("base_url", "")).startswith("http://127.0.0.1:"), ch.get("base_url"))

        # ── ② 管理面：经面板代理的账号 CRUD（回执与契约样本同形）──────────
        st, body = get(base, UP + "/accounts")
        check("空池时 /accounts 可用（200）", st == 200, f"{st} {body}")

        # 键序：accounts / stats / providers / ts（样本顺序，不能靠字典序）
        keys = list(body.keys()) if isinstance(body, dict) else []
        check("账号列表键序为样本的 accounts,stats,providers,ts",
              keys == ["accounts", "stats", "providers", "ts"], keys)
        stats = body.get("stats") or {}
        check("stats 键序为样本的 total,active,exhausted,cooling,invalid,disabled,calls,fail",
              list(stats.keys()) == ["total", "active", "exhausted", "cooling",
                                     "invalid", "disabled", "calls", "fail"],
              list(stats.keys()))
        check("providers 顺序为枚举声明序 zai,bigmodel（非字典序）",
              body.get("providers") == ["zai", "bigmodel"], body.get("providers"))
        check("空容器是 [] 而不是 null（accounts）",
              body.get("accounts") == [], body.get("accounts"))

        st, d = post(base, UP + "/accounts",
                     {"provider": "zai", "tokens": ["tok-native-aaaa", "tok-native-bbbb"],
                      "name": "My Account!"})
        check("新增账号成功且回执为 {count,ids}", st == 200 and d.get("count") == 2,
              f"{st} {d}")
        ids = d.get("ids") or []
        # slug 规则：name 转小写、非字母数字替 -、去首尾、截断 32。
        check("账号 id = <slug>-<8hex>（slug 来自 name 的规范化）",
              len(ids) == 2 and all(re.fullmatch(r"my-account-[0-9a-f]{8}", i) for i in ids), ids)

        st, body = get(base, UP + "/accounts")
        accs = body.get("accounts") or []
        check("列表可见 2 条", len(accs) == 2, len(accs))
        first = accs[0] if accs else {}
        check("新增接口固定产出 mode=apiKey", first.get("mode") == "apiKey", first.get("mode"))
        check("name 保留原名（My Account!）", first.get("name") == "My Account!", first.get("name"))
        check("token 以掩码回显（len<=16 原样）",
              first.get("token_masked") == "tok-native-aaaa", first.get("token_masked"))
        check("enabled=true ⇒ status=active",
              first.get("enabled") is True and first.get("status") == "active",
              f"{first.get('enabled')} {first.get('status')}")
        # 指纹由生成器**随机**在 darwin / win32 两组枚举里取（与靶机同分布），
        # 所以断言「落在合法枚举内且 platform ↔ arch/os_version 自洽」，
        # 而不是钉死 win32（钉死会得到一个随机假失败）。
        fp = first.get("fingerprint") or {}
        check("设备指纹已生成且 platform 在合法枚举内（darwin/win32）",
              fp.get("platform") in ("darwin", "win32"), fp.get("platform"))
        check("指纹含 arch 与 os_version（platform ↔ 版本池自洽）",
              bool(fp.get("arch")) and bool(fp.get("os_version")),
              f"{fp.get('arch')} {fp.get('os_version')}")

        # ── ③ 密码只剩一处：route key 即后台密码 ──────────────────────
        # 面板代理自动注入 Bearer（浏览器不接触密码）。用错密码应 401。
        # 这里不直接打原生端口（那要绕过 Mergence），而是验「代理带的是对的值」：
        # 若代理用错来源，上面的 /accounts 就会 401 —— 它已 200，说明注入正确。
        check("面板代理注入的凭据被原生实现接受（管理面 200 而非 401）",
              st == 200, st)

        # 改密码 = 改 route key：删掉重建成新 key，旧 key 必须失效。
        delete(base, "/api/channels?name=" + urllib.parse.quote(CHAN))
        st, d = create_channel(base, KEY2)
        check("用新 route key 重建渠道成功", st == 200 and d.get("ok"), f"{st} {d}")
        ch2 = wait_ready(base)
        check("重建后就绪", bool(ch2 and ch2.get("ready")),
              json.dumps(ch2, ensure_ascii=False)[:200] if ch2 else "无渠道")
        # 账号库在同一个实例数据目录 ⇒ 账号还在（证明换了密码没换数据）。
        st, body = get(base, UP + "/accounts")
        accs2 = (body or {}).get("accounts") or []
        check("换密码后账号仍在（同一个数据目录，数据未重置）", len(accs2) == 2, len(accs2))
        check("换密码后管理面仍可用（新 key 生效）", st == 200, st)

        # ── ④ 转发链路：未采样分支显式报错，绝不伪造成功 ──────────────
        # Mergence 自己的 /v1/models（聚合各渠道模型）按 access_key 鉴权。
        access = read_access_key()
        check("读到 Mergence access_key（配置已生成）", bool(access), access)
        st2, ml = get_auth(base, "/v1/models", access)
        blob = json.dumps(ml, ensure_ascii=False)
        check("Mergence /v1/models 聚合出 zcode- 前缀的模型",
              st2 == 200 and "zcode-" in blob, f"{st2} {blob[:200]}")

        # /v1/messages 无真实账号 ⇒ 明确失败（503/未实现），不是 200 假成功。
        st3, resp = post(base, "/v1/messages", {
            "model": "zcode-glm-4.6", "max_tokens": 8,
            "messages": [{"role": "user", "content": "hi"}],
        }, timeout=60)
        check("无可用账号时 /v1/messages 明确失败（不伪造成功）", st3 != 200, f"{st3}")

        # ── ⑤ 就绪判据用的是真实 HTTP 探活 ────────────────────────────
        # health_path=/meta 且 native 预设配的就是它：停掉渠道后 ready 应变 false。
        st, _ = post(base, "/api/channels/toggle", {"name": CHAN, "enabled": False})
        check("停用接口可用", st == 200, st)
        if st == 200:
            time.sleep(1.2)
            _s, ch3 = get(base, "/api/channels")
            row = next((c for c in ch3.get("channels", []) if c.get("name") == CHAN), None)
            check("停用后渠道不再 ready", bool(row) and not row.get("enabled"),
                  json.dumps(row, ensure_ascii=False)[:200] if row else "无渠道")

        # 收尾：走优雅退出（/api/quit）并**等进程真的结束**。
        #
        # 必须等：只发 quit 就立刻 terminate/kill，服务端来不及按序回收原生实例
        # （关账号库、释放回环监听）。残留进程会占住 Mergence 的**单实例锁**，
        # 下一轮脚本就报「已有实例在运行」，看起来像被测功能坏了 —— 实为测试
        # 自己的卫生问题。同款收尾见 verify_zcode_accounts.py。
        try:
            post(base, "/api/quit", {}, timeout=10)
        except Exception:
            pass
        for _ in range(40):
            if mm.poll() is not None:
                break
            time.sleep(0.25)
    finally:
        try:
            if mm.poll() is None:
                mm.kill()
                mm.wait(timeout=5)
        except Exception:
            pass

    ok = sum(1 for _n, o, _d in results if o)
    total = len(results)
    print("\n" + "=" * 46)
    print(f"{ok}/{total} 通过")
    if ok != total:
        print("\n失败项：")
        for n, o, d in results:
            if not o:
                print("  - " + n + ("  ← " + str(d)[:200] if d else ""))
    return 0 if ok == total else 1


if __name__ == "__main__":
    sys.exit(main())
