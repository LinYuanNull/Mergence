#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""布局改造验证：侧栏折叠 / 概览拆分 / 渠道嵌套编辑 / 添加平台视图。

这几项都是「交互态」——纯截图看不出「点开之后还在不在原位」，
必须真的点一遍再断言 DOM。

数据依赖：3 / 3b 与 3c 末尾那几项需要实例里**已有渠道**（3b 还要求有个
名为 zcode 的托管渠道）；渠道池为空时这些项会打印 [跳过] 而不是算失败，
总数会相应少几项 —— 这是数据条件，不是回归。
"""
import base64
import json
import os
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from drive_ui import EDGE, WS, wait_json  # noqa: E402
BASE = "http://127.0.0.1:1234/"
PORT = 9352
SHOTS = os.path.join(HERE, "shots")
os.makedirs(SHOTS, exist_ok=True)
sys.stdout.reconfigure(encoding="utf-8")

results = []


def check(name, ok, detail=""):
    # js() 在表达式抛异常时返回 {"__err": ...}。dict 恒为真值 —— 不显式挡掉的话，
    # 一个「元素不存在 → TypeError」的断言会静默 PASS，跑出来一片绿其实什么都没测到。
    if isinstance(ok, dict) and "__err" in ok:
        detail = detail or ok["__err"]
        ok = False
    results.append((name, bool(ok), detail))
    print(("[PASS] " if ok else "[FAIL] ") + name + ("" if ok else "  ← " + str(detail)[:220]))


def _invis(mid):
    """元素在页面上是否「真的不可见」：display 为 none 且渲染高度为 0。

    只断言 hidden 属性是不够的 —— 曾经的缺陷正是「属性为 true，但 .inline 的
    display:block（类选择器）压过了浏览器对 [hidden] 的 display:none（属性
    选择器）」，属性断言照样通过，面板却明晃晃显示在渠道列表正下方。
    """
    return ("(function(){var m=document.getElementById(%r);if(!m)return false;"
            "return getComputedStyle(m).display==='none'"
            "&&m.getBoundingClientRect().height===0})()" % mid)


def _probe(mid):
    """诊断串：hidden 属性 / 计算样式 / 渲染高度 / .inline 类一次取回，
    失败时能直接看出是哪一层没生效。"""
    return ("(function(){var m=document.getElementById(%r);if(!m)return 'missing';"
            "var cs=getComputedStyle(m),r=m.getBoundingClientRect();"
            "return 'hidden='+m.hidden+' display='+cs.display+' h='+r.height"
            "+' inline='+m.classList.contains('inline')})()" % mid)


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
        check("导航分组结构不变（3 组）", n == 3, f"groups={n}")
        # 「控制台」改回可折叠：原先它被做成常驻不可收起（理由是入口要随时可见），
        # 但同样的入口现在在主面板控制台的页签栏里都有一份，侧栏这份只是快捷跳转，
        # 渠道一多就会把侧栏撑满，所以允许收起。3 组都必须有折叠控件。
        check("三个分组都可折叠（控制台不再被钉死为常驻）",
              js("document.querySelectorAll('.nav-sec [data-sec-toggle]').length") == 3,
              js("[...document.querySelectorAll('.nav-sec')].map(x=>x.dataset.sec+':'+!!x.querySelector('[data-sec-toggle]')).join(',')"))
        js("document.querySelector('[data-sec-toggle=up]').click()")
        time.sleep(0.4)
        check("控制台分组可收起（子项隐藏）",
              js("document.querySelector('[data-sec=up]').classList.contains('closed')")
              and js("getComputedStyle(document.querySelector('#upGroup .nav-sec-bd')).display") == "none",
              js("document.getElementById('upGroup').className"))
        js("document.querySelector('[data-sec-toggle=up]').click()")
        time.sleep(0.4)
        check("控制台分组可再展开（入口找得回来）",
              not js("document.querySelector('[data-sec=up]').classList.contains('closed')"), "")
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
            # 判据必须是「实际不可见」，不能只看 hidden 属性：
            # .inline 的 display:block 曾经压过 [hidden] 的 display:none，
            # 于是 hidden=true 之后面板仍继续渲染在渠道列表正下方。
            js("document.getElementById('btnCloseCh').click()")
            time.sleep(0.8)
            check("点关闭后编辑态撤销（两栏退回单列）",
                  js("!document.getElementById('acctSplit').classList.contains('editing')"), "")
            check("点关闭后编辑面板实际不可见（display:none 且高度 0）",
                  js(_invis("chModal")), js(_probe("chModal")))
            check("点关闭后面板不再留在编辑槽里（槽位为空、不占位）",
                  js("document.querySelector('#acctEditor').children.length") == 0
                  and js("document.querySelector('#acctEditor').getBoundingClientRect().height") == 0,
                  js(_probe("chModal")))
            check("点关闭后 chModal 回到 body 原址（不再占据视图内位置）",
                  js("document.getElementById('chModal').parentElement === document.body"),
                  js("document.getElementById('chModal').parentElement"
                     "&&document.getElementById('chModal').parentElement.id"))

            # ── 关闭的第二条路径：ESC ──────────────────────────
            # 它以前只设 hidden、不走 unmount：既不摘 .inline 也不撤销 .editing，
            # 是同一个缺陷的另一半。必须单独验。
            js("document.querySelector('#acctList [data-act=edit]').click()")
            time.sleep(1.2)
            check("（ESC 前置）再次点开后面板确实是内联可见面板",
                  js("document.getElementById('chModal').classList.contains('inline')")
                  and js("getComputedStyle(document.getElementById('chModal')).display") == "block",
                  js(_probe("chModal")))
            js("document.dispatchEvent(new KeyboardEvent('keydown',{key:'Escape',bubbles:true}))")
            time.sleep(0.8)
            check("按 ESC 关闭后编辑面板实际不可见",
                  js(_invis("chModal")), js(_probe("chModal")))
            check("按 ESC 关闭后分栏也回单列（不留 editing 残留）",
                  js("document.querySelectorAll('.chsplit.editing').length") == 0,
                  js("document.querySelectorAll('.chsplit.editing').length"))
            check("按 ESC 关闭后编辑槽仍为空",
                  js("document.querySelector('#acctEditor').children.length") == 0,
                  js("document.querySelector('#acctEditor').children.length"))
            # 切到别的视图时不应把表单遗留过去
            js("setView('acct')")
            time.sleep(0.6)
            js("document.querySelector('#acctList [data-act=edit]').click()")
            time.sleep(1.2)
            js("setView('overview')")
            time.sleep(1.2)
            check("切走视图时编辑区自动收起（表单不跟着跑）",
                  js(_invis("chModal"))
                  and js("!document.getElementById('chModal').closest('.view')"),
                  js(_probe("chModal")))

        # ── 3c. 「添加平台」＝侧栏里的独立视图 ──────────────
        # 改造前：两个渠道视图各挂一个「添加」按钮（添加 API 平台 / 添加积分型
        # 平台）。按钮已经替你选好了类型，进表单第一项却又是「渠道类型」二选一
        # —— 同一件事被问两遍。现在类型只在这个视图里选一次，渠道视图退化成
        # 只读列表。本节不依赖已有渠道数据，渠道池为空时照样能验。
        addnav = "#nav .nav-i[data-view=add]"
        check("侧栏「渠道」分组里有「添加平台」独立入口",
              js("!!document.querySelector('%s')" % addnav)
              and js("document.querySelector('%s span').textContent.trim()" % addnav) == "添加平台",
              js("document.querySelector('%s span')"
                 "&&document.querySelector('%s span').textContent" % (addnav, addnav)))
        check("两个渠道视图里的「添加」按钮已移除（入口不再重复）",
              not js("!!document.getElementById('btnAddApi')")
              and not js("!!document.getElementById('btnAddAcct')"), "")
        js("document.querySelector('%s').click()" % addnav)
        time.sleep(1.6)
        check("点侧栏入口切到「添加平台」且导航高亮",
              js("Store.get().view") == "add"
              and js("document.querySelector('%s').classList.contains('on')" % addnav),
              js("Store.get().view"))
        check("视图标题与副标题都是新建语境",
              js("document.getElementById('ttl').textContent.trim()") == "添加平台"
              and "新建" in (js("document.getElementById('subMeta').textContent") or ""),
              js("document.getElementById('ttl').textContent + ' / ' "
                 "+ document.getElementById('subMeta').textContent"))
        check("进视图即挂出表单（不需要先点任何按钮）",
              js("document.getElementById('chModal').parentElement.id") == "addEditor"
              and not js(_invis("chModal")), js(_probe("chModal")))
        check("表单是内联普通文档流（不是盖住整屏的弹层）",
              js("document.getElementById('addEditor')"
                 ".contains(document.getElementById('chModal'))")
              and js("getComputedStyle(document.getElementById('chModal')).position") == "static",
              js("getComputedStyle(document.getElementById('chModal')).position"))
        check("表单标题跟着视图叫「添加平台」",
              js("document.getElementById('chFormTitle').textContent.trim()") == "添加平台",
              js("document.getElementById('chFormTitle').textContent"))
        # 独立视图里不该有「关闭」：关掉只会把当前这个空视图留在原地，
        # 不像弹层那样有关掉的对象。
        check("添加平台视图里表单不显示「关闭」按钮",
              js("document.getElementById('btnCloseCh').hidden"), js(_probe("chModal")))
        check("「渠道类型」可选（不再是进表单前的重复提问）",
              js("document.querySelectorAll('#chKind .seg-b').length") == 2
              and not js("document.getElementById('chKind').classList.contains('locked')"), "")
        js("document.querySelector('#chKind .seg-b[data-kind=managed]').click()")
        time.sleep(0.9)
        check("切到积分型：托管专属字段显示、API 专属字段隐藏",
              js("getComputedStyle(document.querySelector('#chModal .konly[data-kind=managed]')).display") != "none"
              and js("getComputedStyle(document.querySelector('#chModal .konly[data-kind=embedded]')).display") == "none",
              js("document.getElementById('kindHint').textContent"))
        check("类型提示文案随之更新",
              "积分型" in (js("document.getElementById('kindHint').textContent") or ""),
              js("document.getElementById('kindHint').textContent"))
        # ESC / 点空白在这两种视图里含义不同：分栏编辑态是「关掉弹层」，
        # 独立视图里那等于把当前页面弄空，必须不生效。
        js("document.dispatchEvent(new KeyboardEvent('keydown',{key:'Escape',bubbles:true}))")
        time.sleep(0.7)
        check("按 ESC 不会把「添加平台」的整页表单收掉",
              js("document.getElementById('chModal').parentElement.id") == "addEditor"
              and not js(_invis("chModal")), js(_probe("chModal")))
        js("document.getElementById('chModal').click()")
        time.sleep(0.7)
        check("点表单外空白也不会把整页表单收掉",
              js("document.getElementById('chModal').parentElement.id") == "addEditor"
              and not js(_invis("chModal")), js(_probe("chModal")))
        # 同分组内互切：表单是单例、跟着分组走，填了一半的草稿不该被清空
        js("document.getElementById('fName').value = '草稿渠道'")
        js("setView('api')")
        time.sleep(1.3)
        check("切到渠道视图后表单不在别处露头（编辑槽仍是空的、左列不被挤窄）",
              js("document.querySelector('#apiEditor').children.length") == 0
              and not js("document.querySelector('#apiSplit').classList.contains('editing')"),
              js("document.querySelector('#apiSplit').className"))
        js("document.querySelector('%s').click()" % addnav)
        time.sleep(1.4)
        check("切回来保留填了一半的草稿（不重置成空表单）",
              js("document.getElementById('fName').value") == "草稿渠道",
              js("document.getElementById('fName').value"))
        shot(ws, "lay_add_view")
        # 离开「渠道」分组才释放表单
        js("setView('overview')")
        time.sleep(1.3)
        check("离开渠道分组后表单被收回（不留痕在其它视图里）",
              js(_invis("chModal"))
              and js("!document.getElementById('chModal').closest('.view')"),
              js(_probe("chModal")) + " parent=" + str(
                  js("document.getElementById('chModal').parentElement.id")))
        js("document.querySelector('%s').click()" % addnav)
        time.sleep(1.4)
        check("重新进入时表单自动回来（进这个视图就是要新建）",
              js("document.getElementById('chModal').parentElement.id") == "addEditor"
              and not js(_invis("chModal")), js(_probe("chModal")))
        # 判据是「草稿没了、回到新建态」，**不是「字段为空」**：新建内嵌渠道会走
        # applyPreset，把 #fName 预填成第一个预设的标签（「自定义 OpenAI 兼容
        # 端点（空白）」）。断空串在这里恒假 —— 那是判据错，不是回归。
        check("重新进入后是一张新表单（上一份草稿随退出分组释放了）",
              js("document.getElementById('fName').value") != "草稿渠道"
              and js("document.getElementById('chFormTitle').textContent.trim()") == "添加平台",
              js("document.getElementById('fName').value"))
        # 从编辑态切进「添加平台」：要换成干净的新表单，且原视图不能留下
        # 「编辑中」的两栏态（表单被搬走了、左列却还挤窄着，右边一片空）。
        if js("!!document.querySelector('#apiList [data-act=edit]')"):
            js("setView('api')")
            time.sleep(1.4)
            js("document.querySelector('#apiList [data-act=edit]').click()")
            time.sleep(1.5)
            edit_title = js("document.getElementById('chFormTitle').textContent.trim()")
            edit_name = js("document.getElementById('fName').value") or ""
            js("document.querySelector('%s').click()" % addnav)
            time.sleep(1.5)
            check("（前置）编辑态表单标题带渠道名", "编辑渠道" in (edit_title or ""), edit_title)
            check("（前置）编辑态表单填的是被编辑渠道的名字",
                  edit_name != "", edit_name)
            # 同前：新建态会被预设预填 #fName，所以判据是「名字换了」而不是「空了」。
            check("从编辑态切进「添加平台」得到的是新表单，不是接着编辑",
                  js("document.getElementById('chFormTitle').textContent.trim()") == "添加平台"
                  and js("document.getElementById('fName').value") != edit_name,
                  js("document.getElementById('chFormTitle').textContent")
                  + " / name=" + str(js("document.getElementById('fName').value")))
            check("原先的编辑视图不残留「编辑中」（左列不该被挤窄）",
                  js("document.querySelectorAll('.chsplit.editing').length") == 0,
                  js("document.querySelectorAll('.chsplit.editing').length"))
        else:
            print("  [跳过] 从编辑态切进「添加平台」的 3 项：本实例没有可编辑的渠道")
        js("setView('overview')")
        time.sleep(1.0)
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
            # 标签文案随「zcode 改为内置原生」更新过（旧文案带 zcode2api 字样，
            # 见 provider/presets.go 的 Label: "ZCode 账号网关（内置原生）"）。
            # 断言意图不变：选中的必须是 ZCode 网关，而不是下拉里的第一项
            # （第一项是 WorkBuddy —— 下一条断言专门挡住那个回归）。
            check("下拉显示的是 ZCode 网关标签（不是下拉里的第一项）",
                  "ZCode" in label, label)
            check("下拉没有误显示 WorkBuddy 模板", "WorkBuddy" not in label, label)
            check("托管型下拉带「未记录模板」占位项（判不出时不显示错的）",
                  js("!!document.querySelector('#fPreset option[value=\"\"]')"),
                  js("document.querySelector('#fPreset option[value=\"\"]').textContent"))
            # 模板不能覆盖表单里用户已保存的真实值。
            # 独立子进程模式移除后，托管渠道的「真实值」落在探活路径与环境变量上，
            # 已删掉的启动命令字段不再是判据。
            envv = (js("document.getElementById('fEnv').value") or "")
            hp = js("document.getElementById('fHealthM').value")
            check("表单字段仍是渠道真实值（模板未覆盖探活路径/环境变量）",
                  hp == "/meta" and "MERGENCE_EXTERNAL_URL" in envv,
                  "env=%s health=%s" % (envv[:60], hp))
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
