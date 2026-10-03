#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""布局改造验证：侧栏折叠 / 概览拆分 / 渠道嵌套编辑。

这几项都是「交互态」——纯截图看不出「点开之后还在不在原位」，
必须真的点一遍再断言 DOM。
"""
import base64
import json
import os
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from drive_ui import WS, wait_json  # noqa: E402

EDGE = r"C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe"
BASE = "http://127.0.0.1:1234/"
PORT = 9352
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
         "--user-data-dir=" + os.path.join(os.environ.get("TEMP", "."), "edgelay"),
         "--no-sandbox", "--disable-gpu", "--window-size=1600,1100", BASE],
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

        time.sleep(2.8)
        js("window.__errs=[];window.addEventListener('error',e=>window.__errs.push(String(e.message)));"
           "window.addEventListener('unhandledrejection',e=>window.__errs.push('reject:'+String(e.reason)));1")

        # ── 1. 侧栏 ─────────────────────────────────────────
        check("侧栏整体收起按钮已移除", js("!document.getElementById('btnCollapse')"), "")
        w = js("document.getElementById('sidebar').getBoundingClientRect().width")
        check("侧栏宽度收窄到 208px", w and 200 <= w <= 216, f"width={w}")
        check("侧栏没有 collapsed 类（功能已取消）",
              js("!document.getElementById('sidebar').className.includes('collapsed')"), "")
        n = js("document.querySelectorAll('.nav-sec').length")
        check("导航分组结构不变（3 组，其中 2 组可折叠）", n == 3, f"groups={n}")
        check("上游控制台是常驻分组（无折叠控件）",
              not js("document.querySelector('#upGroup [data-sec-toggle]')"), "")
        # 折叠与恢复
        js("document.querySelector('[data-sec-toggle=ops]').click()")
        time.sleep(0.4)
        closed = js("document.querySelector('[data-sec=ops]').classList.contains('closed')")
        hidden_bd = js("getComputedStyle(document.querySelector('[data-sec=ops] .nav-sec-bd')).display")
        check("点分组标题可收起（子项隐藏）", closed and hidden_bd == "none", f"closed={closed} disp={hidden_bd}")
        js("document.querySelector('[data-sec-toggle=ops]').click()")
        time.sleep(0.4)
        check("再点可恢复展开",
              js("!document.querySelector('[data-sec=ops]').classList.contains('closed')"), "")
        # 进入某视图时，其所在分组不能被收起状态挡住
        js("document.querySelector('[data-sec-toggle=ops]').click()")  # 先收起
        time.sleep(0.3)
        js("setView('logs')")
        time.sleep(0.6)
        check("切到「日志」时所在分组自动展开（避免导航「消失」）",
              js("!document.querySelector('[data-sec=ops]').classList.contains('closed')"), "")
        # 「设置」是侧栏独立入口（不在任何 .nav-sec 内），所以收起运维不该让它消失。
        # 判据用「父节点」而不是「文案」：文案可能被别的入口复用。
        check("「设置」不在任何导航分组内（是独立入口）",
              js("!document.querySelector('#nav .nav-sec [data-view=settings]')")
              and js("!!document.querySelector('#nav > .nav-i[data-view=settings]')"), "")
        js("document.querySelector('[data-sec-toggle=ops]').click()")  # 再收起运维
        time.sleep(0.4)
        check("收起运维分组后「设置」入口仍可见",
              js("getComputedStyle("
                 "document.querySelector('#nav > .nav-i[data-view=settings]')).display") != "none", "")
        js("setView('settings')")
        time.sleep(0.6)
        check("收起运维时切到「设置」依然可见（它是独立入口，不依赖分组展开）",
              js("getComputedStyle("
                 "document.querySelector('#nav > .nav-i[data-view=settings]')).display") != "none", "")
        js("document.querySelector('[data-sec-toggle=ops]').click()")  # 恢复展开，留给后续
        time.sleep(0.4)
        shot(ws, "lay_sidebar")

        # ── 2. 概览拆分 ─────────────────────────────────────
        js("setView('overview')")
        time.sleep(3.5)
        folds = js("document.querySelectorAll('#mtBody .fold').length")
        # 概览现在只放「本机转发」一块：平台账号的指标已整合进控制台，
        # 首屏并排两个口径最容易让人把它们相加。
        check("概览只剩「本机转发」一块独立面板", folds == 1, f"folds={folds}")
        check("概览里不再有平台账号面板（mt-up-*）",
              js("document.querySelectorAll('#mtBody [data-section^=\"mt-up-\"]').length") == 0,
              js("[...document.querySelectorAll('#mtBody .fold')].map(x=>x.dataset.section)"))
        check("平台账号的模块不再出现在概览 DOM 里",
              "平台账号" not in (js("document.getElementById('mtBody').textContent") or ""),
              (js("document.getElementById('mtBody').textContent") or "")[:160])
        check("静态卡也变成可收起面板（对外出口 / 渠道概况）",
              js("document.querySelectorAll('#view-overview > .card[data-collapsible]').length") == 2,
              js("document.querySelectorAll('#view-overview > .card[data-collapsible]').length"))
        check("每块面板各带一个折叠头",
              js("document.querySelectorAll('#mtBody [data-fold-toggle]').length") == folds,
              js("document.querySelectorAll('#mtBody [data-fold-toggle]').length"))
        # 折叠第一块
        js("document.querySelector('#mtBody [data-fold-toggle]').click()")
        time.sleep(0.4)
        check("点折叠头可收起该面板",
              js("document.querySelector('#mtBody .fold').classList.contains('closed')")
              and js("getComputedStyle(document.querySelector('#mtBody .fold .fold-bd')).display") == "none",
              js("getComputedStyle(document.querySelector('#mtBody .fold .fold-bd')).display"))
        # 收起状态要能跨重渲染保持
        first_key = js("document.querySelector('#mtBody .fold').dataset.section")
        js("loadMetrics(true)")
        time.sleep(3.0)
        check("刷新指标后收起状态仍保持（按面板记）",
              js(f"document.querySelector('#mtBody .fold[data-section=\"{first_key}\"]').classList.contains('closed')"), first_key)
        js("document.querySelector('#mtBody [data-fold-toggle]').click()")
        time.sleep(0.3)
        check("再点可展开回去",
              js("!document.querySelector('#mtBody .fold').classList.contains('closed')"), "")
        shot(ws, "lay_overview")

        # ── 3. 渠道嵌套编辑 ─────────────────────────────────
        js("setView('acct')")
        time.sleep(2.5)
        check("渠道视图是「列表 + 编辑槽」两栏结构",
              js("!!document.querySelector('#view-acct .chsplit .ch-editor')"), "")
        check("未编辑时编辑槽为空（不占位）",
              js("document.querySelector('#acctEditor').children.length") == 0,
              js("document.querySelector('#acctEditor').children.length"))
        has_edit = js("!!document.querySelector('#acctList [data-act=edit]')")
        check("渠道小面板有「编辑」入口", has_edit, has_edit)
        if has_edit:
            js("document.querySelector('#acctList [data-act=edit]').click()")
            time.sleep(1.5)
            check("点编辑后表单挂进编辑槽（不再是全屏弹层）",
                  js("document.querySelector('#acctEditor').contains(document.getElementById('chModal'))"), "")
            check("弹层转为内联面板（inline 类）",
                  js("document.getElementById('chModal').classList.contains('inline')"), "")
            check("进入编辑态后列表与编辑区左右分栏",
                  js("document.getElementById('acctSplit').classList.contains('editing')"), "")
            pos = js("getComputedStyle(document.getElementById('chModal')).position")
            check("编辑区是普通文档流（position 非 fixed）", pos == "static", pos)
            check("左右分栏生效（编辑槽在列表右侧）",
                  js("(function(){var a=document.querySelector('#acctList').closest('.card').getBoundingClientRect();"
                     "var b=document.getElementById('acctEditor').getBoundingClientRect();return b.left>a.right-4})()"), "")
            shot(ws, "lay_channel_edit")
            js("document.getElementById('btnCloseCh').click()")
            time.sleep(0.8)
            check("关闭后回到纯列表（编辑态撤销）",
                  js("!document.getElementById('acctSplit').classList.contains('editing')")
                  and js("document.getElementById('chModal').hidden"), "")
            # 切到别的视图时不应把表单遗留过去
            js("setView('acct')")
            time.sleep(0.6)
            js("document.querySelector('#acctList [data-act=edit]').click()")
            time.sleep(1.2)
            js("setView('overview')")
            time.sleep(1.2)
            check("切走视图时编辑区自动收起（表单不跟着跑）",
                  js("document.getElementById('chModal').hidden")
                  and js("document.querySelectorAll('.chsplit.editing').length") == 0, "")
        # ── 3b. 模板下拉必须反映渠道的真实模板 ───────────────
        # 曾经的缺陷：托管渠道的 preset 从来没被写进配置（toManaged 漏了它），
        # raw 接口回空 → <select> 停在第一个选项 → **zcode 渠道显示成
        # 「WorkBuddy 网关（wb2api）」**。所以这里不能只断言「有下拉」，
        # 必须断言它选中的是 zcode 而不是列表里的第一个。
        js("setView('acct')")
        time.sleep(2.0)
        zc = "document.querySelector('#acctList [data-name=zcode] [data-act=edit]')"
        if not js("!!" + zc):
            check("zcode 渠道存在且可编辑（跳过模板断言）", False, "未找到 zcode 渠道卡片")
        else:
            js(zc + ".click()")
            time.sleep(3.0)
            val = js("document.getElementById('fPreset').value")
            label = js("document.getElementById('fPreset').selectedOptions[0].textContent") or ""
            check("zcode 渠道的模板下拉选中 zcode", val == "zcode", "value=%s" % val)
            check("下拉显示的是 ZCode 网关标签（不是下拉里的第一项）",
                  "zcode2api" in label, label)
            check("下拉没有误显示 WorkBuddy 模板", "WorkBuddy" not in label, label)
            check("托管型下拉带「未记录模板」占位项（判不出时不显示错的）",
                  js("!!document.querySelector('#fPreset option[value=\"\"]')"),
                  js("document.querySelector('#fPreset option[value=\"\"]').textContent"))
            # 模板不能覆盖表单里用户已保存的真实值
            cmd = (js("document.getElementById('fCmd').value") or "")
            hp = js("document.getElementById('fHealthM').value")
            check("表单字段仍是渠道真实值（模板未覆盖命令/探活路径）",
                  "python" in cmd.lower() and hp == "/meta",
                  "cmd=%s health=%s" % (cmd[:60], hp))
            # preset 是推断来的，界面上要说清来源，别让人以为是自己填的
            hint = js("document.getElementById('presetHint').textContent") or ""
            check("界面上标明了模板来源有推断成分", "推断" in hint, hint[:160])
            js("document.getElementById('btnCloseCh').click()")
            time.sleep(0.8)
            js("setView('acct')")
            time.sleep(0.8)

        # ── 4. 模型与档位表格保持原样 ───────────────────────
        # 上游视图要先绑定渠道才会加载数据
        js("selectUpstream('workbuddy')")
        time.sleep(1.0)
        js("setView('up-models')")
        time.sleep(3.5)
        head = js("[...document.querySelectorAll('#view-up-models thead th')].map(x=>x.textContent).join('|')")
        check("模型与档位表格列未改动",
              head == "模型|积分倍率|默认档|思考档位|上下文|最大输出", head)
        mrows = js("document.querySelectorAll('#mdBody tr').length")
        check("模型表仍渲染出数据行", mrows and mrows > 0, f"rows={mrows}")

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
