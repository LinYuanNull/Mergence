#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""zcode 账号面板端到端验证：真界面 + 真代理 + **有状态**的假网关。

为什么必须有状态：把 zcode 账号面板整块搬进 ModelMux，价值就在「能在本面板
里增删改」——只读渲染是搬进来之前就有的能力。假网关若每次吐同一份硬编码列表，
「保存返回 200 但列表没变」这类 bug 就测不出来。所以 test/fake_zcode.py 维护了
一份内存账号池，写操作必须能被随后的 GET /admin/api/accounts 读回。

链路（每一步都是真实 HTTP，断言读的是**渲染后的 DOM 文本**，不是 innerHTML 字符串）：
    Edge(headless) → ModelMux 面板 → /api/channels/<chan>/upstream/*
                   → ModelMux 服务端注入后台密码 → 假 zcode2api 的 /admin/api/*

覆盖：账号渲染 / 导出（含下载文件字节）/ 领取预览+领取 / 新增 / 编辑改名 /
      启停 / 全量刷新 / 导入 / 删除（确认框）/ 设备码登录轮询到成功。

与 verify_claim.py 的差别：那边验的是「定时领取器」的后端链路；这边验的是
账号面板的**界面交互**，也就是用户实际点得到的那一层。
"""
import base64
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
from drive_ui import EDGE, WS, wait_json  # noqa: E402

ROOT = os.path.dirname(HERE)
HOME = os.path.join(HERE, "zcacc_home")
# 允许指向别的构建产物（例如仓库根的 ModelMux.exe 正被用户实例占用时，
# 可以先 build 到临时路径再拿来验，不必先杀掉用户正在用的实例）。
EXE = os.environ.get("MODELMUX_EXE") or os.path.join(ROOT, "ModelMux.exe")
FAKE_ZCODE = os.path.join(HERE, "fake_zcode.py")
PY = os.environ.get("MODELMUX_PY", sys.executable)

ZCODE_PORT = 18103
ZCODE_ADMIN_KEY = "fake-admin-key-123"
CDP_PORT = 9361
DL = os.path.join(HOME, "downloads")

# 假网关种子的两个 JWT token：导出必须原样带出来（证明字节通路没被动过）
SEED_TOKENS = {"eyJhbGciOiJI.eyJzdWIiOiJhIn0.sigaaa",
               "eyJhbGciOiJI.eyJzdWIiOiJiIn0.sigbbb"}

# ── 可选：把托管渠道的上游从「有状态假网关」换成**真实 Go 实现**（A3 验收口径）──
#
# 不设 ZCODE_UPSTREAM_EXE 时一切照旧：假网关自带种子账号与额度，44 项全绿。
# 设成 zcode2api-go.exe 的路径即切到 Go 实现（见 zcode2api-go 仓库的 A3）。
#
# 切过去后**必然有若干项期望值不同**，这不是回归，是两份上游本就不同：
#   1. Go 实现从空池启动，而 ModelMux 对 account_count === 0 的渠道整体隐藏 ⇒
#      必须先经代理灌入账号，侧栏入口才会出现（脚本自动灌，见下）。
#   2. 账号名由 Go 侧按「提供方-序号」自动生成（zai-1 / zai-2），不是假网关的「账号甲/乙」。
#   3. 刚灌入的账号上游还没查过额度 ⇒ `quota` 是空对象（与靶机一致），面板渲染「—」。
#   4. A5/A6 尚未实现的分支（JWT 额度刷新 / 领取 / 设备码登录）显式报错 ——
#      这些步骤改为断言「显式失败」，而不是跳过：把「我们还不支持」也钉成可观测行为。
UPSTREAM_EXE = os.environ.get("ZCODE_UPSTREAM_EXE") or ""
GO_MODE = bool(UPSTREAM_EXE)
# Go 实现要把上游自带面板目录指过去才有多余能力；ModelMux 走的是自己的面板，
# 这个值只影响 Go 进程自己能不能提供 /admin/*，留空也能跑。
PANEL_DIR = os.environ.get("ZCODE_PANEL_DIR") or ""

sys.stdout.reconfigure(encoding="utf-8")
results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))
    print(("[PASS] " if ok else "[FAIL] ") + name
          + ("" if ok else "  ← " + str(detail)[:260]), flush=True)


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


def delete(base, path, timeout=60):
    rq = urllib.request.Request(base + path, method="DELETE")
    try:
        with urllib.request.urlopen(rq, timeout=timeout) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


def put(base, path, obj, timeout=60):
    rq = urllib.request.Request(base + path, data=json.dumps(obj).encode(),
                                headers={"Content-Type": "application/json"},
                                method="PUT")
    try:
        with urllib.request.urlopen(rq, timeout=timeout) as r:
            return r.status, json.loads(r.read() or b"{}")
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read() or b"{}")


def wait_port(proc, log, timeout=30):
    """等「内置服务已监听」出现，返回**最后一条**里的端口。

    必须取最后一条：同名的固定隔离目录会被复用，日志里会留着上一轮的旧行；
    取第一条就会连到上一轮早已关闭的端口，表现为后续请求莫名失败。
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


def wait_file(folder, suffix, timeout=20):
    end = time.time() + timeout
    while time.time() < end:
        try:
            for f in os.listdir(folder):
                if f.endswith(suffix) and not f.endswith(".crdownload"):
                    p = os.path.join(folder, f)
                    if os.path.getsize(p) > 0:
                        return p
        except FileNotFoundError:
            pass
        time.sleep(0.3)
    return None


def main():
    # 固定隔离目录（不用时间戳，便于排查）。只清「会影响断言」的四样东西：
    #   config/                    渠道与设置（否则上一轮的渠道残留）
    #   data/ports.json            端口映射（否则 Reuse 让端口在几轮之间漂移）
    #   data/logs/modelmux.log     监听日志（否则 wait_port 可能读到上一轮的旧端口）
    #   downloads/                 上一轮导出的文件（否则会拿旧文件当本轮结果）
    # 整个 HOME 递归删会被沙箱的批量删除保护拦下（跑到一半失败还更糟）；
    # edge-profile 留着反而省一次浏览器冷启动。
    for rel in ("config", "data/logs/modelmux.log", "data/ports.json", "downloads"):
        p = os.path.join(HOME, *rel.split("/"))
        if os.path.isdir(p):
            shutil.rmtree(p, ignore_errors=True)
        elif os.path.isfile(p):
            try:
                os.remove(p)
            except OSError:
                pass
    os.makedirs(os.path.join(HOME, "config"), exist_ok=True)
    os.makedirs(DL, exist_ok=True)

    env = dict(os.environ)
    env["MODELMUX_HOME"] = HOME
    env["MODELMUX_HEADLESS"] = "1"
    mm = subprocess.Popen([EXE], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    edge = None
    ws = None
    base = None
    try:
        port = wait_port(mm, os.path.join(HOME, "data", "logs", "modelmux.log"))
        if not port:
            print("ModelMux 未启动")
            return 1
        base = f"http://127.0.0.1:{port}"
        print("ModelMux 端口", port)

        # 上一轮若被沙箱拦下了 rmtree，HOME 里会残留上一轮的渠道配置。先清空，
        # 让脚本在任何残留状态下都从「零渠道」开始（幂等，不靠删目录成功）。
        st0, ch0 = get(base, "/api/channels")
        for c in (ch0.get("channels") or []):
            delete(base, "/api/channels?name=" + urllib.parse.quote(c.get("name", "")))

        # ── 托管渠道：ModelMux 拉起上游（默认假 zcode2api；GO_MODE 下换成 Go 实现）
        # 端口由编排器分配并经 port_env_var 注入 ZCODE_PORT —— Go 实现读同名变量。
        if GO_MODE:
            if not os.path.isfile(UPSTREAM_EXE):
                print("ZCODE_UPSTREAM_EXE 指向的文件不存在：" + UPSTREAM_EXE)
                return 1
            up_home = os.path.join(HOME, "go-upstream")
            shutil.rmtree(up_home, ignore_errors=True)
            os.makedirs(up_home, exist_ok=True)
            up_cmd = UPSTREAM_EXE
            up_args = ["serve", "--host", "127.0.0.1",
                       "--data-dir", os.path.join(up_home, "data"),
                       "--admin-key", ZCODE_ADMIN_KEY,
                       # 给一个网关 Key：面板的设置页要求 `gateway_key_masked` 非空
                       # （「密钥以掩码回显」一项），空密钥会渲染成空串。
                       "--gateway-key", "sk-go-gateway-abcdef123456"]
            if PANEL_DIR:
                up_args += ["--panel-dir", PANEL_DIR]
            up_dir = up_home
        else:
            up_cmd = PY
            up_args = [FAKE_ZCODE, str(ZCODE_PORT)]
            up_dir = os.path.dirname(FAKE_ZCODE)

        st, d = post(base, "/api/channels", {
            "kind": "managed", "name": "", "display_name": "ZCode 网关", "preset": "zcode",
            "command": up_cmd, "args": up_args,
            "dir": up_dir, "enabled": True,
            "port_env_var": "ZCODE_PORT", "health_path": "/meta", "panel_path": "/admin/",
            "model_prefix": "zcode-", "expose": True, "protocol": "chat",
            "models": ["glm-4.6"],
        }, timeout=150)
        check("托管渠道创建成功（zcode 预设）", st == 200 and d.get("ok"), f"{st} {d}")
        ch_name = d.get("name", "")
        if not ch_name:
            print("渠道名缺失，后续无法继续")
            return 1

        # 等子进程就绪 + console_kind 探成 zcode
        console_kind = ""
        for _ in range(50):
            time.sleep(0.4)
            st2, ch = get(base, "/api/channels")
            for c in ch.get("channels", []):
                if c.get("name") == ch_name and c.get("ready"):
                    console_kind = c.get("console_kind") or ""
            if console_kind == "zcode":
                break
        check("渠道就绪且控制台类型识别为 zcode", console_kind == "zcode", f"kind={console_kind!r}")

        # 填后台密码（面板代理的凭据来源就是它，不是渠道 route 的 Key）
        st, d = post(base, "/api/claim/config", {"enabled": True, "at": "12:01",
                                                 "window": 4, "channel": "", "admin_key": ZCODE_ADMIN_KEY})
        check("后台密码已写入（面板代理据此注入 Bearer）", st == 200 and d.get("configured"), f"{st} {d}")

        upq = "/api/channels/" + urllib.parse.quote(ch_name) + "/upstream"

        if GO_MODE:
            # Go 实现从**空池**启动，而 ModelMux 对 account_count === 0 的托管渠道
            # 按设计整体隐藏（侧栏入口根本不出现）。所以先经面板代理灌入两个账号；
            # 种子用与假网关相同的 JWT，导出断言才能原样沿用。
            st, sd = post(base, upq + "/accounts",
                          {"provider": "zai", "tokens": sorted(SEED_TOKENS)})
            check("经面板代理向 Go 实现灌入 2 个种子账号",
                  st == 200 and sd.get("count") == 2, f"{st} {sd}")

        # ── 起浏览器驱动真实界面
        edge = subprocess.Popen([
            EDGE, "--headless=new", "--disable-gpu", "--no-sandbox", "--hide-scrollbars",
            "--force-device-scale-factor=1", "--window-size=1500,1100",
            f"--remote-debugging-port={CDP_PORT}",
            f"--user-data-dir={os.path.join(HOME, 'edge-profile')}",
            base + "/",
        ], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

        info = wait_json(f"http://127.0.0.1:{CDP_PORT}/json", timeout=40)
        page = next((t for t in (info or []) if t.get("type") == "page"), None)
        if not page:
            print("未找到页面目标")
            return 1
        ws = WS(page["webSocketDebuggerUrl"])
        ws.call("Runtime.enable")
        ws.call("Page.enable")

        def js(expr, timeout=40):
            r = ws.call("Runtime.evaluate",
                        {"expression": expr, "returnByValue": True, "awaitPromise": True},
                        timeout=timeout)
            if "exceptionDetails" in r:
                return {"__err": json.dumps(r["exceptionDetails"], ensure_ascii=False)[:300]}
            return r.get("result", {}).get("value")

        def wait_true(expr, timeout=25, interval=0.3):
            end = time.time() + timeout
            last = None
            while time.time() < end:
                last = js(expr)
                if last:
                    return last
                time.sleep(interval)
            return last

        time.sleep(2.5)
        js("window.__errs=[];window.addEventListener('error',e=>window.__errs.push(String(e.message)));"
           "window.addEventListener('unhandledrejection',e=>window.__errs.push('reject:'+String(e.reason)));1")

        # 下载要让浏览器真的落盘，才能断言导出文件的字节
        dl_ok = True
        try:
            ws.call("Browser.setDownloadBehavior",
                    {"behavior": "allow", "downloadPath": DL, "eventsEnabled": True})
        except Exception as e:
            dl_ok = False
            print("  下载行为设置失败：", e)

        # ── 入口：侧栏渠道卡里的 zcode 控制台（证明 console_kind 一路传到界面）
        js("loadChannels()")
        time.sleep(2.5)
        nav_sel = f"#upChanList [data-view=up-zc-accounts]"
        check("侧栏出现 zcode 的「账号池」入口（入口绑定到该渠道）",
              bool(wait_true(f"!!document.querySelector('{nav_sel}')", timeout=15)),
              js(f"document.getElementById('upChanList') && document.getElementById('upChanList').textContent"))
        js("document.querySelector('%s').click()" % nav_sel)
        js("openUpView(%s,'up-zc-accounts')" % json.dumps(ch_name))

        def rows():
            return js("document.querySelectorAll('#zcAccBody tr').length") or 0

        def row_texts():
            return js("[...document.querySelectorAll('#zcAccBody tr')].map(r=>r.textContent)") or []

        def row_idx(name):
            return js("[...document.querySelectorAll('#zcAccBody tr')]"
                      ".findIndex(r=>r.textContent.includes(%s))" % json.dumps(name))

        wait_true("document.querySelectorAll('#zcAccBody tr').length>0", timeout=20)
        check("账号池渲染出 2 个种子账号", rows() == 2, f"rows={rows()}")
        texts = row_texts()
        seed_names = ("zai-1", "zai-2") if GO_MODE else ("账号甲", "账号乙")
        check("种子账号名与状态渲染正确（%s）" % " / ".join(seed_names),
              all(any(n in t for t in texts) for n in seed_names),
              str(texts)[:240])
        check("每行有 4 个行内操作（刷新/停用/编辑/删除）",
              js("document.querySelectorAll('#zcAccBody tr:first-child [data-zop]').length") == 4,
              js("document.querySelectorAll('#zcAccBody tr:first-child [data-zop]').length"))
        check("账号表头含「操作」列",
              "操作" in (js("document.querySelector('#view-up-zc-accounts thead').textContent") or ""),
              js("document.querySelector('#view-up-zc-accounts thead').textContent"))
        if GO_MODE:
            # 刚灌入的账号上游还没查过额度 ⇒ `quota` 是空对象（与靶机一致），
            # 面板渲染成「—」。凭空长出一条 0% 的进度条才是 bug
            # ——那是假网关自己塞的种子数据。
            check("无额度数据时渲染「—」而不是空进度条",
                  js("document.querySelectorAll('#zcAccBody .zbar i').length") == 0
                  and js("document.querySelectorAll('#zcAccBody td.credits .dim').length") == 2,
                  f"bars={js('document.querySelectorAll(\"#zcAccBody .zbar i\").length')} "
                  f"dim={js('document.querySelectorAll(\"#zcAccBody td.credits .dim\").length')}")
        else:
            check("额度列渲染进度条与数字",
                  js("document.querySelectorAll('#zcAccBody .zbar i').length") == 2,
                  js("document.querySelectorAll('#zcAccBody .zbar i').length"))
        check("统计卡渲染（账号总数 / 启用）",
              js("document.querySelectorAll('#zcAccCards .card-k').length") >= 2,
              js("document.querySelectorAll('#zcAccCards .card-k').length"))
        # 进度条宽度靠 CSSOM 注入（CSP 无 unsafe-inline，markup 里的 style 会被丢弃）
        check("进度条宽度已写进 CSSOM（不是被 CSP 丢掉的空条）",
              js("[...document.querySelectorAll('#zcAccBody .zbar i')]"
                 ".every(i=>i.style.width && i.style.width!=='')"),
              js("[...document.querySelectorAll('#zcAccBody .zbar i')].map(i=>i.getAttribute('style'))"))

        # ── 导出：点按钮 → 浏览器落盘 → 断言文件字节
        js("document.getElementById('btnZcExport').click()")
        fp = wait_file(DL, ".json", timeout=20) if dl_ok else None
        got = None
        if fp:
            try:
                got = json.loads(open(fp, encoding="utf-8").read())
            except Exception as e:
                got = {"__parse_err": str(e)}
        check("导出触发真实下载且是可解析的 JSON", isinstance(got, dict) and "providers" in got,
              f"file={fp} got={str(got)[:160]}")
        zc_entries = ((got or {}).get("providers") or {}).get("zai") or []
        if GO_MODE:
            # 真上游（与 Go 实现）导出的是**对象条目** `{name, mode, secret}`，
            # 与靶机样本 19-export 一致；假网关宽松地导字符串数组。
            check("导出内容与上游账号池逐字节一致（2 个 JWT token）",
                  isinstance(got, dict)
                  and {e.get("secret") for e in zc_entries if isinstance(e, dict)} == SEED_TOKENS
                  and {e.get("mode") for e in zc_entries if isinstance(e, dict)} == {"jwt"},
                  f"providers={str((got or {}).get('providers'))[:200]}")
        else:
            check("导出内容与上游账号池逐字节一致（2 个 JWT token）",
                  isinstance(got, dict) and set(zc_entries) == SEED_TOKENS,
                  f"providers={str((got or {}).get('providers'))[:200]}")

        # ── 领取：预览 → 全部领取
        js("document.getElementById('btnZcClaim').click()")
        time.sleep(0.4)
        check("领取弹窗打开", js("!document.getElementById('zcClaimModal').hidden"),
              js("document.getElementById('zcClaimModal').hidden"))
        js("document.getElementById('btnZcClaimPreview').click()")
        if GO_MODE:
            # 领取要解人机验证（A6），Go 侧显式报错。断言面板确实显示了失败，
            # 而不是「预览 0 个账号」这种看起来正常、实则静默失败的状态。
            hit = wait_true("(document.getElementById('zcClaimEmpty').textContent||'')"
                            ".includes('预览失败')", timeout=25)
            emsg = js("document.getElementById('zcClaimEmpty').textContent")
            check("领取预览（属 A5/A6）显式失败而非静默空结果", bool(hit), emsg)
            st, sd = get(base, upq + "/claim/preview")
            check("代理链路上确认领取预览显式报「尚未实现」",
                  st == 501 and "尚未实现" in str(sd.get("detail") or ""), f"{st} {sd}")
            js("document.getElementById('btnZcClaimAll').click()")
            hit = wait_true("(document.getElementById('zcClaimMsg').textContent||'')"
                            ".includes('领取失败')", timeout=25)
            check("全部领取（属 A5/A6）显式失败而非假装成功",
                  bool(hit) and "成功" not in (js("document.getElementById('zcClaimMsg').textContent") or ""),
                  js("document.getElementById('zcClaimMsg').textContent"))
            st, sd = post(base, upq + "/claim", {})
            check("代理链路上确认领取显式报「尚未实现」",
                  st == 501 and "尚未实现" in str(sd.get("detail") or ""), f"{st} {sd}")
            js("document.getElementById('btnZcClaimOk').click()")
        else:
            wait_true("document.querySelectorAll('#zcClaimBody tr').length>0", timeout=20)
            check("预览列出 2 个可领账号",
                  js("document.querySelectorAll('#zcClaimBody tr').length") == 2,
                  js("document.querySelectorAll('#zcClaimBody tr').length"))
            check("预览提示含账号数与套餐数",
                  "2 个账号" in (js("document.getElementById('zcClaimHint').textContent") or ""),
                  js("document.getElementById('zcClaimHint').textContent"))
            js("document.getElementById('btnZcClaimAll').click()")
            wait_true("(document.getElementById('zcClaimMsg').textContent||'').includes('成功')", timeout=25)
            check("全部领取回执显示成功 2 / 失败 0",
                  "成功 2" in (js("document.getElementById('zcClaimMsg').textContent") or "")
                  and "失败 0" in (js("document.getElementById('zcClaimMsg').textContent") or ""),
                  js("document.getElementById('zcClaimMsg').textContent"))
            check("领取结果逐账号摊开（2 条明细）",
                  js("document.querySelectorAll('#zcClaimResult .ep-row').length") == 2,
                  js("document.querySelectorAll('#zcClaimResult .ep-row').length"))
            js("document.getElementById('btnZcClaimOk').click()")

        # ── 新增账号（粘贴 API Key）
        js("document.getElementById('btnZcAdd').click()")
        time.sleep(0.4)
        check("添加弹窗打开且提供方下拉已从上游灌入",
              js("!document.getElementById('zcAccModal').hidden")
              and js("[...document.querySelectorAll('#zcAccProvider option')].map(o=>o.value).includes('zai')"),
              js("[...document.querySelectorAll('#zcAccProvider option')].map(o=>o.value)"))
        js("document.getElementById('zcAccTokens').value='sk-ui-new-1'")
        js("document.getElementById('btnZcAccSave').click()")
        wait_true("document.getElementById('zcAccModal').hidden && "
                  "document.querySelectorAll('#zcAccBody tr').length===3", timeout=25)
        check("新增后弹窗关闭且列表变为 3 行", rows() == 3, f"rows={rows()}")
        idx = row_idx("zai-3")
        check("新账号按「提供方-序号」自动命名且识别为 API Key 模式",
              idx >= 0 and "API Key" in row_texts()[idx],
              f"idx={idx} texts={str(row_texts())[:240]}")

        # ── 编辑改名（走 PUT）
        js("document.querySelectorAll('#zcAccBody tr')[%d]"
           ".querySelector('[data-zop=edit]').click()" % idx)
        time.sleep(0.4)
        check("编辑弹窗进入编辑态（隐藏页签、露出「新的 Token」）",
              js("!document.getElementById('zcAccModal').hidden")
              and js("document.getElementById('zcAccTabs').hidden")
              and js("!document.getElementById('zcAccNewTokField').hidden"),
              f"tabs={js('document.getElementById(\"zcAccTabs\").hidden')} "
              f"ntf={js('document.getElementById(\"zcAccNewTokField\").hidden')}")
        js("document.getElementById('zcAccName').value='UI 改名'")
        js("document.getElementById('btnZcAccSave').click()")
        wait_true("document.getElementById('zcAccModal').hidden", timeout=25)
        wait_true("[...document.querySelectorAll('#zcAccBody tr')]"
                  ".some(r=>r.textContent.includes('UI 改名'))", timeout=20)
        check("编辑改名生效（列表出现「UI 改名」）", row_idx("UI 改名") >= 0,
              str(row_texts())[:240])

        # ── 启停
        idx = row_idx("UI 改名")
        js("document.querySelectorAll('#zcAccBody tr')[%d]"
           ".querySelector('[data-zop=toggle]').click()" % idx)
        wait_true("(document.querySelectorAll('#zcAccBody tr')[%d]||{}).textContent"
                  "&& document.querySelectorAll('#zcAccBody tr')[%d].textContent.includes('停用')"
                  % (idx, idx), timeout=25)
        check("停用后该行显示「停用」（且按钮变「启用」）",
              "停用" in row_texts()[idx] and "启用" in row_texts()[idx],
              row_texts()[idx][:200])

        # ── 全量刷新（结果走 toast：提示行是「账号摘要」，会被随后的重载覆盖）
        js("document.getElementById('btnZcRefreshAll').click()")
        if GO_MODE:
            # 池里有两个 JWT 账号 ⇒ 全量刷新必须打上游（属 A5），Go 侧显式报错。
            # 断言「显式失败」而不是跳过：面板不能出现「刷新完成」这种假回执。
            hit = wait_true("(document.getElementById('toast').textContent||'').includes('刷新失败')",
                            timeout=25)
            tmsg = js("document.getElementById('toast').textContent")
            check("全量刷新（含 JWT，属 A5）显式失败而非假装成功",
                  bool(hit) and "刷新失败" in (tmsg or "")
                  and "刷新完成" not in (tmsg or ""), tmsg)
            st, sd = post(base, upq + "/accounts/refresh", {"all": True})
            check("代理链路上确认全量刷新显式报「尚未实现」",
                  st == 501 and "尚未实现" in str(sd.get("detail") or ""), f"{st} {sd}")
        else:
            hit = wait_true("(document.getElementById('toast').textContent||'').includes('刷新完成')", timeout=25)
            tmsg = js("document.getElementById('toast').textContent")
            check("全量刷新回执以 toast 呈现（含刷新数量）",
                  bool(hit) and "刷新" in (tmsg or "") and "个" in (tmsg or ""), tmsg)
        check("提示行仍是账号摘要（刷新结果没有把它污染掉）",
              "个账号" in (js("document.getElementById('zcAccHint').textContent") or ""),
              js("document.getElementById('zcAccHint').textContent"))

        # ── 导入（JSON body，不是 multipart）
        js("document.getElementById('btnZcImport').click()")
        time.sleep(0.3)
        # 导入格式：真上游（与 Go 实现）只接受**对象条目** `{name, mode, secret}`
        # ——这正是导出写出来的形状。假网关宽松地也收字符串数组，脚本早先就写了
        # 字符串；切到 Go 时改成对象条目，与真实契约对齐。
        if GO_MODE:
            import_payload = {"version": 1, "providers": {"zai": [
                {"name": "imp-a", "mode": "apiKey", "secret": "sk-imp-a"},
                {"name": "imp-b", "mode": "apiKey", "secret": "sk-imp-b"},
            ]}}
        else:
            import_payload = {"version": 1, "providers": {"zai": ["sk-imp-a", "sk-imp-b"]}}
        js("document.getElementById('zcImportText').value=" + json.dumps(json.dumps(import_payload)))
        js("document.getElementById('btnZcImportDo').click()")
        wait_true("document.getElementById('zcImportModal').hidden && "
                  "document.querySelectorAll('#zcAccBody tr').length===5", timeout=25)
        check("导入 2 个账号后列表变为 5 行", rows() == 5, f"rows={rows()}")

        # ── 删除（确认框；用 confirm 直接放行，避免 headless 卡在对话框）
        js("window.confirm=()=>true")
        idx = row_idx("UI 改名")
        js("document.querySelectorAll('#zcAccBody tr')[%d]"
           ".querySelector('[data-zop=remove]').click()" % idx)
        wait_true("document.querySelectorAll('#zcAccBody tr').length===4", timeout=25)
        check("删除后列表回到 4 行且「UI 改名」已消失",
              rows() == 4 and row_idx("UI 改名") < 0, f"rows={rows()} idx={row_idx('UI 改名')}")

        # ── 设备码登录：发起 → 轮询到 ready → 账号入池
        js("window.open=()=>null")  # headless 下不让 desktopOpenExternal 真去开标签页
        js("document.getElementById('btnZcAdd').click()")
        time.sleep(0.3)
        js("document.getElementById('zcAccName').value='UI 设备码账号'")
        js("document.querySelector('#zcAccTabs .seg-b[data-zt=login]').click()")
        ws.call("Runtime.evaluate", {"expression": "1"})  # 让上一步的事件循环跑完
        check("切到登录页签后粘贴框隐藏、登录框出现",
              js("document.getElementById('zcTabPaste').hidden")
              and js("!document.getElementById('zcTabLogin').hidden"),
              f"paste={js('document.getElementById(\"zcTabPaste\").hidden')}")
        js("document.getElementById('btnZcLoginStart').click()")
        if GO_MODE:
            # 设备码登录（A5/A6）Go 侧显式报错：面板应显示「发起失败」，
            # 而不是停在「等待你在浏览器完成授权…」让用户白等。
            hit = wait_true("(document.getElementById('zcLoginState').textContent||'')"
                            ".includes('发起失败')", timeout=25)
            check("设备码登录发起（属 A5/A6）显式失败而非静默等待",
                  bool(hit) and "授权成功" not in (js("document.getElementById('zcLoginState').textContent") or ""),
                  js("document.getElementById('zcLoginState').textContent"))
            st, sd = post(base, upq + "/login/start", {})
            # 登录发起按样本走 **502**（`{"detail":"登录初始化失败: …"}`）——上游不可达
            # 是这条分支的既定形态，A3 用同一个形态承载「需要上游调用（属于 A5）」。
            # 同时钉住「不伪造 flow_id」：失败体里绝不能出现 flow_id / authorize_url。
            check("代理链路上确认登录发起显式报错且不伪造 flow_id（502 形态）",
                  st == 502 and "A5" in str(sd.get("detail") or "")
                  and "flow_id" not in sd, f"{st} {sd}")
            js("document.getElementById('btnZcAccClose').click()")  # 关掉弹窗，后续步骤才点得到
        else:
            wait_true("(document.getElementById('zcLoginState').textContent||'').includes('授权成功')", timeout=25)
            check("登录轮询到「授权成功」",
                  "授权成功" in (js("document.getElementById('zcLoginState').textContent") or ""),
                  js("document.getElementById('zcLoginState').textContent"))
            wait_true("document.querySelectorAll('#zcAccBody tr').length===5", timeout=20)
            check("授权成功后账号入池（列表 5 行且含「UI 设备码账号」）",
                  rows() == 5 and row_idx("UI 设备码账号") >= 0,
                  f"rows={rows()} idx={row_idx('UI 设备码账号')}")

        # ── 网关设置：从「只读视图」变成面板内可直接修改
        # 三条通路分别验：① 非密项走面板代理；② 监控清空走面板代理；
        # ③ 后台密码走「一次调用同步两处」的原生接口。它们分属不同实现，
        # 混在一起只验一条会漏掉「另一个方向其实没通」。
        # （upq 在渠道就绪后已定义。）

        # 静态真源：把人引回网关自己面板的旧文案必须消失，新控件必须存在。
        # 注意静态出口是 "/"（服务端把 index.html 挂在根上），不是 "/index.html"。
        html = urllib.request.urlopen(base + "/", timeout=20).read().decode("utf-8")
        check("面板 HTML 不再出现「只读视图 / 请去网关自己的面板」",
              "只读视图" not in html and "请去网关自己的面板" not in html,
              "旧文案仍在静态资源里")
        check("面板 HTML 已有「修改设置 / 清空监控」控件与设置弹窗",
              all(k in html for k in ("btnZcSetEdit", "btnZcMonClear", "zcSetModal")),
              "控件缺失")

        # 界面链路：侧栏入口 → 「修改设置」→ 弹窗真的打开。
        # 只验静态资源里有按钮是不够的——id 拼错、事件没绑上，静态断言都看不出来。
        js("document.querySelector('a.nav-i[data-view=\"up-zc-settings\"]').click()")
        wait_true("!document.getElementById('view-up-zc-settings').hidden", timeout=15)
        js("document.getElementById('btnZcSetEdit').click()")
        wait_true("document.getElementById('zcSetModal').hidden===false", timeout=15)
        check("点「修改设置」真的打开设置弹窗，且密码框是掩码输入",
              (not js("document.getElementById('zcSetModal').hidden"))
              and js("document.getElementById('zcSetAdminKey').type") == "password",
              "弹窗未打开或密码框类型不是 password")
        js("document.getElementById('btnZcSetClose').click()")

        # ① 设置读写：先读回当前值，改一个数值项，再读回确认真的落库
        st, s0 = get(base, upq + "/settings")
        check("读取网关设置成功（密钥以掩码回显）",
              st == 200 and s0.get("gateway_key_masked"), f"{st} {s0}")
        new_q = int(s0.get("quota_refresh_interval") or 0) + 7
        st, d = put(base, upq + "/settings", {"quota_refresh_interval": new_q})
        check("通过面板代理写网关设置返回成功", st == 200, f"{st} {d}")
        st, s1 = get(base, upq + "/settings")
        check("写后读回：数值项已按提交值落库",
              int(s1.get("quota_refresh_interval") or 0) == new_q,
              f"want={new_q} got={s1.get('quota_refresh_interval')}")
        # 掩码值原样回提交不得把密钥写坏（上游对含 … 的值显式跳过）
        st, d = put(base, upq + "/settings",
                    {"gateway_key": s1.get("gateway_key_masked") or "sk-a…b"})
        st, s2 = get(base, upq + "/settings")
        check("掩码密钥原样提交不会被写坏",
              (s2.get("gateway_key_masked") or "") == (s1.get("gateway_key_masked") or ""),
              f"{s1.get('gateway_key_masked')} → {s2.get('gateway_key_masked')}")

        # ② 监控清空：先确认有数据，清空之后必须真的为空
        st, m0 = get(base, upq + "/monitoring")
        n0 = len(m0.get("entries") or [])
        if GO_MODE:
            # Go 侧的监控环形缓冲只由**网关转发**写入，而转发链路属 A4 ⇒ 此刻必然为空。
            # 这里只能验「通路可用 + 空态形状正确」；写入端随 A4 落地。
            check("监控接口可读且空态形状正确（entries 是数组、keep 有值）",
                  st == 200 and isinstance(m0.get("entries"), list)
                  and int(m0.get("keep") or 0) > 0,
                  f"{st} keep={m0.get('keep')} entries={type(m0.get('entries')).__name__}")
        st, d = post(base, upq + "/monitoring/clear", {})
        check("通过面板代理清空监控返回成功", st == 200, f"{st} {d}")
        st, m1 = get(base, upq + "/monitoring")
        if GO_MODE:
            check("清空后监控为空", len(m1.get("entries") or []) == 0,
                  f"n1={len(m1.get('entries') or [])}")
        else:
            check("清空后监控确实为空（n0=%d）" % n0,
                  n0 > 0 and len(m1.get("entries") or []) == 0,
                  f"n0={n0} n1={len(m1.get('entries') or [])}")

        # ③ 后台密码：一次调用同时落「网关侧」与「本机侧」
        new_key = "sync-key-9911"
        st, d = post(base, "/api/channels/admin-key",
                     {"name": ch_name, "admin_key": new_key}, timeout=60)
        check("后台密码同步接口返回 ok 且声明两处都已同步",
              st == 200 and d.get("ok") and set(d.get("synced") or []) == {"gateway", "modelmux"},
              f"{st} {d}")
        # 本机侧的直接证据：配置文件里的 claim.admin_key 必须是新值（原子落盘的产物）
        cfgp = os.path.join(HOME, "config", "modelmux.json")
        try:
            saved = json.load(open(cfgp, encoding="utf-8"))
        except Exception as e:
            saved = {"_err": str(e)}
        check("本机侧 Claim.AdminKey 已同步为新值（读配置文件为证）",
              ((saved.get("claim") or {}).get("admin_key") or "") == new_key,
              f"{(saved.get('claim') or {}).get('admin_key')!r}")
        # 网关侧的证据：假上游已改用新密码鉴权，代理注入的 Bearer 也必须是新值才可能 200。
        # 两边任意一边没改，这条都会失败——这正是「两处同步」这个不变式的判据。
        st, s3 = get(base, upq + "/settings")
        check("改密后用新凭据仍能读网关设置（网关侧与本机侧确实一致）",
              st == 200 and "quota_refresh_interval" in s3, f"{st} {s3}")

        errs = js("window.__errs") or []
        check("全程无 JS 运行时错误", not errs, str(errs)[:300])

    finally:
        if ws:
            ws.close()
        # 走优雅退出：/api/quit 会让服务端按序回收托管子进程（假网关）。
        # 直接 terminate 主进程会把它拉起的子进程留成孤儿，孤儿继续占着
        # 编排器分配的那个端口，下一轮只能换端口——端口一轮一漂就是这样来的。
        if mm and mm.poll() is None and base:
            try:
                post(base, "/api/quit", {})
            except Exception:
                pass
            for _ in range(40):
                if mm.poll() is not None:
                    break
                time.sleep(0.25)
        for p in (edge, mm):
            try:
                if p and p.poll() is None:
                    p.kill()
            except Exception:
                pass

    passed = sum(1 for _, ok, _ in results if ok)
    print("\n" + "=" * 46 + f"\n{passed}/{len(results)} 通过")
    failed = [n for n, ok, _ in results if not ok]
    if failed:
        print("失败项：\n  - " + "\n  - ".join(failed))
    return 0 if passed == len(results) else 1


if __name__ == "__main__":
    sys.exit(main())
