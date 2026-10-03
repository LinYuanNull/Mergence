#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""上游控制台逐视图验证：连真实运行的 ModelMux，驱动界面检查渲染与报错。

为什么不是纯截图：截图只能看「像不像」，看不出「用量表读的是不存在的字段」
这类问题——那种 bug 渲染出来是一张空表，截图里和「没数据」长得一样。
这里对每个视图断言关键内容的条数。
"""
import base64
import json
import os
import re
import subprocess
import sys
import time
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from drive_ui import EDGE, WS, wait_json  # noqa: E402
BASE = "http://127.0.0.1:1234/"
PORT = 9350
SHOTS = os.path.join(HERE, "shots")
os.makedirs(SHOTS, exist_ok=True)
sys.stdout.reconfigure(encoding="utf-8")

results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))
    print(("[PASS] " if ok else "[FAIL] ") + name + ("" if ok else "  ← " + str(detail)[:220]))


def main():
    edge = subprocess.Popen(
        [EDGE, "--headless=new", f"--remote-debugging-port={PORT}",
         "--user-data-dir=" + os.path.join(os.environ.get("TEMP", "."), "edgeup"),
         "--no-sandbox", "--disable-gpu", "--window-size=1600,1150", BASE],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        info = wait_json(f"http://127.0.0.1:{PORT}/json", timeout=40)
        page = next(t for t in info if t.get("type") == "page")
        ws = WS(page["webSocketDebuggerUrl"])

        def js(expr, timeout=40):
            r = ws.call("Runtime.evaluate", {
                "expression": expr, "returnByValue": True, "awaitPromise": True}, timeout=timeout)
            if "exceptionDetails" in r:
                return {"__err": json.dumps(r["exceptionDetails"], ensure_ascii=False)[:300]}
            return r.get("result", {}).get("value")

        time.sleep(2.5)
        js("window.__errs=[];window.addEventListener('error',e=>window.__errs.push(String(e.message)));"
           "window.addEventListener('unhandledrejection',e=>window.__errs.push('reject:'+String(e.reason)));1")

        # 控制台入口已搬到侧栏（渠道卡上的「控制台」按钮按需求移除）
        js("loadChannels()")
        time.sleep(2.5)
        js("setView('acct')")
        time.sleep(1.2)
        check("积分型平台的渠道卡上不再有「控制台」按钮",
              not js("document.querySelector('#acctList [data-act=console]')"), "")
        js("document.querySelector('#upChanList .upcard[data-chan-card=workbuddy] "
           ".nav-i[data-view=up-accounts]').click()")
        time.sleep(3.0)
        check("侧栏渠道卡片的一步入口已绑定到 workbuddy 渠道",
              js("UP.chan") == "workbuddy", js("UP.chan"))

        # ── 账号池 ──────────────────────────────────────────
        js("setView('up-accounts')")
        time.sleep(3.0)
        rows = js("document.querySelectorAll('#accBody tr').length")
        check("账号池渲染出账号行", rows and rows > 0, f"rows={rows}")
        check("账号表有「用量」列（token/延迟/速率）",
              js("document.querySelectorAll('#accBody .usage-line').length") == rows,
              js("document.querySelectorAll('#accBody .usage-line').length"))
        check("账号积分带消耗进度条",
              js("document.querySelectorAll('#accBody .cred-bar').length") == rows,
              js("document.querySelectorAll('#accBody .cred-bar').length"))
        stats = js("document.querySelectorAll('#upStat .card-k').length")
        check("账号池顶部 6 张统计卡", stats == 6, f"stats={stats}")
        hint = js("document.getElementById('upAccHint').textContent") or ""
        check("统计摘要含版本与运行时长",
              ("v" in hint and "运行" in hint), hint[:160])
        shot(ws, "up_accounts")

        # ── 模型与档位（顺序 / 倍率 / 档位 / 能力）──────────
        js("setView('up-models')")
        time.sleep(3.5)
        mrows = js("document.querySelectorAll('#mdBody tr').length")
        check("模型表渲染 19 个模型", mrows == 19, f"rows={mrows}")
        dom = js("[...document.querySelectorAll('#mdBody tr .nm')].map(x=>x.textContent)")
        up = js("(UP.models||[]).map(m=>m.id)")
        check("默认排序保持上游返回顺序（不重排）", dom == up, f"dom[:3]={dom[:3]} up[:3]={up[:3]}")
        check("倍率已清洗（无 '0.03 credits' 这类残留）",
              js("!/credits/.test(document.getElementById('mdBody').textContent)"),
              (js("document.getElementById('mdBody').textContent") or "")[:200])
        check("模型行带能力徽标",
              js("document.querySelectorAll('#mdBody .caps .badge').length") > 0,
              js("document.querySelectorAll('#mdBody .caps .badge').length"))
        check("思考档位以徽标列出",
              js("document.querySelectorAll('#mdBody .efs .badge').length") > 0,
              js("document.querySelectorAll('#mdBody .efs .badge').length"))
        check("筛选器齐全（域/能力/档位/价格/排序/重置）",
              all(js(f"!!document.getElementById('{i}')") for i in
                  ("mdQ", "mdRealm", "mdCap", "mdEffort", "mdPromo", "mdSort", "mdReset")), "")
        # 域筛选：上游表里没有 realm 字段，必须按 ID 前缀判断
        js("document.getElementById('mdRealm').value='cn';document.getElementById('mdRealm').dispatchEvent(new Event('change'))")
        time.sleep(0.6)
        cn = js("document.querySelectorAll('#mdBody tr').length")
        check("按域筛选可用（cn 命中而非清空）", cn and cn > 0, f"cn={cn}")
        # 能力筛选
        js("document.getElementById('mdRealm').value='';document.getElementById('mdRealm').dispatchEvent(new Event('change'))")
        js("document.getElementById('mdCap').value='vision';document.getElementById('mdCap').dispatchEvent(new Event('change'))")
        time.sleep(0.5)
        vis = js("document.querySelectorAll('#mdBody tr').length")
        check("按能力筛选可用（视觉）", vis and vis > 0, f"vision={vis}")
        js("document.getElementById('mdReset').click()")
        time.sleep(0.5)
        check("重置后回到全部 19 个",
              js("document.querySelectorAll('#mdBody tr').length") == 19,
              js("document.querySelectorAll('#mdBody tr').length"))
        shot(ws, "up_models")

        # ── 积分构成 ───────────────────────────────────────
        js("setView('up-packages')")
        time.sleep(3.0)
        check("积分到期分布有批次行",
              js("document.querySelectorAll('#pkExpiry .exp-row').length") > 0,
              js("document.querySelectorAll('#pkExpiry .exp-row').length"))
        check("账号对比卡渲染",
              js("document.querySelectorAll('#pkSummary .pk-acct').length") > 0,
              js("document.querySelectorAll('#pkSummary .pk-acct').length"))
        check("批次明细表存在",
              js("document.querySelectorAll('#pkDetail table tbody tr').length") > 0,
              js("document.querySelectorAll('#pkDetail table tbody tr').length"))
        shot(ws, "up_packages")

        # ── 用量 ───────────────────────────────────────────
        js("setView('up-usage')")
        time.sleep(3.2)
        kpi = js("document.querySelectorAll('#usStats .card-k').length")
        # 9 张：8 张原有 + 从概览搬过来的「实际花费估算」
        check("用量总览 9 张 KPI（含花费估算、积分/1M、缓存命中率、延迟、速率）",
              kpi == 9, f"kpi={kpi}")
        txt = js("document.getElementById('usStats').textContent") or ""
        check("KPI 含「积分 / 1M Token」口径", "积分 / 1M" in txt, txt[:200])
        check("KPI 含缓存命中率口径", "缓存命中率" in txt, txt[:200])
        # 「实际花费估算」是从概览整合过来的那一项：积分是平台口径，
        # 人民币才是能跨渠道对比的数。workbuddy 有平台默认价（0.05），
        # 所以这里必须是真金额，不能是「—」。
        check("KPI 含「实际花费估算」并算出了金额",
              "实际花费估算" in txt and "¥" in txt, txt[:260])
        check("花费估算标明单价来源（平台默认价 / 已配置）",
              ("平台默认价" in txt) or ("积分单价" in txt), txt[:260])
        cols = js("document.querySelectorAll('#usChart .uschart-col').length")
        check("Token 时序图渲染", cols and cols > 0, f"cols={cols}")
        # 只数元素会漏掉「容器在、柱子高 0」的空图：必须量实际高度。
        # 判据是「有量的柱子都至少 2px」（桶之间差异大时按比例算会小于 1px）。
        check("用量视图当前可见（柱子高度要在可见状态下量）",
              js("!document.getElementById('view-up-usage').hidden"),
              js("Store.get().view"))
        # 必须量**渲染后的几何尺寸**，不能读 innerHTML 里的字符串。
        #
        # 教训：这里原先读的是 innerHTML 里的 `height:Npx`，字符串一直是对的、
        # 断言一直全绿，但浏览器因 CSP（style-src 'self'，没有 'unsafe-inline'）
        # **会静默丢弃 markup 里的 style 属性** —— 实际渲染高度全是 0，
        # 整张时序图是空白的。渲染层被丢弃这件事，只有量尺寸才看得见。
        #
        # 渲染后柱高改由 CSSOM 写入（见 app.js 的 paintStyles）。
        heights = js("[...document.querySelectorAll('#usChart .uschart-col i')]"
                     ".map(i=>i.getBoundingClientRect().height)") or []
        vals = [float(h or 0) for h in heights]
        check("时序图柱子在**渲染后**有非零高度（不是空图）",
              len(vals) > 0 and any(v >= 2 for v in vals),
              "heights=%s" % [round(v, 1) for v in vals[:8]])
        # 渲染完成后不该再有未处理的 data-style（说明 paintStyles 被漏调了）
        left = js("document.querySelectorAll('[data-style]').length")
        check("渲染后没有残留未处理的 data-style", left == 0, "left=%s" % left)

        tabs = js("document.querySelectorAll('#usDimTabs button').length")
        check("用量明细三视页签（按账号/模型/域）", tabs == 3, f"tabs={tabs}")
        drow = js("document.querySelectorAll('#usDimBody tr').length")
        check("用量明细有数据行（原实现读 buckets 恒空）", drow and drow > 0, f"rows={drow}")
        head = js("document.getElementById('usDimHead').textContent") or ""
        check("明细表头含失败/延迟/速率列",
              ("失败" in head and "均延迟" in head and "均速率" in head), head[:200])
        ctab = js("document.querySelectorAll('#usCreditTabs button').length")
        check("积分扣除历史两视页签", ctab == 2, f"tabs={ctab}")
        check("积分扣除有数据行",
              js("document.querySelectorAll('#usCreditBody tr').length") > 0,
              js("document.querySelectorAll('#usCreditBody tr').length"))
        chead = js("document.getElementById('usCreditHead').textContent") or ""
        check("积分扣除历史带「花费估算」列", "花费估算" in chead, chead[:200])
        cstat = js("document.getElementById('usCreditStats').textContent") or ""
        check("积分扣除历史有合计花费估算卡",
              "合计花费估算" in cstat and "¥" in cstat, cstat[:200])
        # 每行也要有金额，不能只有表头
        crow = js("[...document.querySelectorAll('#usCreditBody tr')]"
                  ".some(r=>r.textContent.includes('¥'))")
        check("积分扣除历史的数据行里带金额", crow, str(crow))
        check("积分扣除表头含「积分 / 1M Token」与缓存命中率",
              ("积分 / 1M" in chead and "缓存命中率" in chead), chead[:200])
        # 切维度
        js("document.querySelector('#usDimTabs button[data-dim=realm]').click()")
        time.sleep(0.6)
        check("切到「按域」后表头变域",
              "域" in (js("document.getElementById('usDimHead').textContent") or ""),
              js("document.getElementById('usDimHead').textContent"))
        shot(ws, "up_usage")

        # ── 上游配置 ───────────────────────────────────────
        js("setView('up-config')")
        time.sleep(3.0)
        n = js("document.querySelectorAll('#view-up-config [data-cfg]').length")
        check("配置表单渲染 60 个字段（与上游配置一一对应）", n == 60, f"fields={n}")
        groups = js("[...document.querySelectorAll('#view-up-config .card-hd h2')].map(x=>x.textContent)")
        check("配置按上游分组（服务/定时任务/账号池与流量治理/上游与高级/日志）",
              groups == ["服务", "定时任务", "账号池与流量治理", "上游与高级", "日志"], groups)
        filled = js("[...document.querySelectorAll('#view-up-config input')].filter(x=>x.value!==''||x.checked).length")
        check("配置值已从上游灌入（非空）", filled > 30, f"filled={filled}")
        check("密钥默认掩码显示",
              (js("document.querySelector('[data-cfg=\"api_key\"]').value") or "").startswith("•"),
              js("document.querySelector('[data-cfg=\"api_key\"]').value"))
        check("时段数组按逗号展开",
              js("document.querySelector('[data-cfg=\"schedule.checkin_hours\"]').value") == "9, 21",
              js("document.querySelector('[data-cfg=\"schedule.checkin_hours\"]').value"))
        check("开关为 toggle 结构（input+span+b）",
              js("!!document.querySelector('#view-up-config label.switch span')"), "")
        shot(ws, "up_config")

        # ── 日志 / 请求监控 ────────────────────────────────
        js("setView('up-logs')")
        time.sleep(3.2)
        rm = js("document.querySelectorAll('#rmCards .card-k').length")
        check("请求监控 7 张卡（含成功率/HTTP 成功率/平均耗时）", rm == 7, f"cards={rm}")
        rmtext = js("document.getElementById('rmCards').textContent") or ""
        check("监控含 HTTP 成功率口径", "HTTP 成功率" in rmtext, rmtext[:160])
        check("归档状态已显示",
              "归档" in (js("document.getElementById('rmArchive').textContent") or ""),
              js("document.getElementById('rmArchive').textContent"))
        check("运行日志渲染（字段 ch/text 已对齐）",
              js("document.querySelectorAll('#ulBox .logline').length") > 0,
              js("document.querySelectorAll('#ulBox .logline').length"))
        req_rows = js("document.querySelectorAll('#ulReqBody tr').length")
        check("请求记录有数据行", req_rows and req_rows > 0, f"rows={req_rows}")
        rhead = js("document.querySelector('#view-up-logs table thead').textContent") or ""
        check("请求记录含来源 IP 与 User-Agent 列",
              ("来源 IP" in rhead and "User-Agent" in rhead), rhead[:200])
        shot(ws, "up_logs")

        errs = js("window.__errs") or []
        check("全程无 JS 运行时错误", not errs, str(errs)[:300])

        passed = sum(1 for _, ok, _ in results if ok)
        print("\n" + "=" * 46 + f"\n{passed}/{len(results)} 通过")
        failed = [n for n, ok, _ in results if not ok]
        if failed:
            print("失败项：\n  - " + "\n  - ".join(failed))
        return 0 if passed == len(results) else 1
    finally:
        edge.terminate()


def shot(ws, name):
    try:
        data = base64.b64decode(ws.call("Page.captureScreenshot", {"format": "png"})["data"])
        with open(os.path.join(SHOTS, name + ".png"), "wb") as f:
            f.write(data)
    except Exception as e:
        print("  截图失败：", e)


if __name__ == "__main__":
    sys.exit(main())
