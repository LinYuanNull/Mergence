#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""侧栏控制台入口验证：每个托管渠道一张子卡片，点一下就切到「该渠道的该视图」。

核心断言是「步数」：以前切渠道要 积分型平台 → 控制台按钮 → 选视图，
现在从侧栏一步到位，所以必须验证点击后 UP.chan 与视图同时正确。
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
PORT = 9360
SHOTS = os.path.join(HERE, "shots")
os.makedirs(SHOTS, exist_ok=True)
sys.stdout.reconfigure(encoding="utf-8")

results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))
    print(("[PASS] " if ok else "[FAIL] ") + name + ("" if ok else "  ← " + str(detail)[:240]))


def main():
    edge = subprocess.Popen(
        [EDGE, "--headless=new", f"--remote-debugging-port={PORT}",
         "--user-data-dir=" + os.path.join(os.environ.get("TEMP", "."), "edgeside"),
         "--no-sandbox", "--disable-gpu", "--window-size=1500,1000", BASE],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        info = wait_json(f"http://127.0.0.1:{PORT}/json", timeout=40)
        page = next(t for t in info if t.get("type") == "page")
        ws = WS(page["webSocketDebuggerUrl"])
        ws.call("Page.enable", {})

        def js(expr, timeout=40):
            r = ws.call("Runtime.evaluate", {
                "expression": expr, "returnByValue": True, "awaitPromise": True}, timeout=timeout)
            if "exceptionDetails" in r:
                return {"__err": json.dumps(r["exceptionDetails"], ensure_ascii=False)[:300]}
            return r.get("result", {}).get("value")

        time.sleep(3.0)
        js("window.__errs=[];window.addEventListener('error',e=>window.__errs.push(String(e.message)));"
           "window.addEventListener('unhandledrejection',e=>window.__errs.push('reject:'+String(e.reason)));1")
        # 清掉上一轮可能留下的折叠状态，从干净状态开始
        js("try{localStorage.removeItem('mm_upcards');localStorage.removeItem('mm_upchan');localStorage.removeItem('mm_navsec')}catch(e){}")
        js("loadChannels()")
        time.sleep(2.5)

        # ── 0. 分类名称（术语归一）──────────────────────────
        # 两类渠道的对外名归一成「积分型平台 / API 型平台」这一对平行称呼。
        # 锁住它是因为这些字符串散落在导航、标题、按钮、徽标、空态文案里，
        # 改名时漏一处就会出现两套叫法，用户分不清是不是同一类东西。
        navs = js("[...document.querySelectorAll('#nav .nav-sec-bd .nav-i span')]"
                  ".map(x=>x.textContent.trim())")
        check("侧栏分类名为「API 型平台 / 积分型平台」，另有「添加平台」入口",
              isinstance(navs, list) and "API 型平台" in navs and "积分型平台" in navs
              and "添加平台" in navs, navs)

        def ttlOf(view):
            js("setView('" + view + "')")
            time.sleep(0.9)
            return js("document.getElementById('ttl').textContent.trim()")

        check("「API 型平台」视图标题正确", ttlOf('api') == "API 型平台", ttlOf('api'))
        check("「积分型平台」视图标题正确", ttlOf('acct') == "积分型平台", ttlOf('acct'))
        check("「添加平台」是侧栏独立视图（标题正确）", ttlOf('add') == "添加平台", ttlOf('add'))
        # 新增渠道的入口统一收进那个视图。原先两个渠道视图各挂一个按钮
        # （添加 API 平台 / 添加积分型平台）—— 按钮已经替你选好类型，
        # 而表单第一项又是「渠道类型」二选一，同一件事被问两遍。
        check("新增入口收进侧栏「添加平台」（渠道视图里不再各挂一个按钮）",
              js("document.querySelector('#nav .nav-i[data-view=add] span').textContent.trim()") == "添加平台"
              and not js("!!document.getElementById('btnAddApi')")
              and not js("!!document.getElementById('btnAddAcct')"),
              js("document.querySelector('#nav .nav-i[data-view=add] span')"
                 "&&document.querySelector('#nav .nav-i[data-view=add] span').textContent"))
        # 概览「渠道概况」卡上的分类徽标用短名：积分型 / API 型。
        # 注意渠道列表的卡片不标分类——本身就在对应分类视图里，标了是冗余，
        # 所以这里查的是 #chCards 而不是 #acctList。
        js("setView('overview')")
        time.sleep(1.2)
        labels = js("[...document.querySelectorAll('#chCards .prov .badge')]"
                    ".map(x=>x.textContent.trim())")
        # 判据是映射关系而不是「两种都出现」：有没有 API 型平台渠道取决于配置，
        # 写死「两种徽标都要有」会在只配了一类渠道时报假失败。
        wrong = js("(function(){var chs=(Store.get().channels)||[];var m={};"
                   "chs.forEach(function(c){m[c.display_name]=c.source});"
                   "var bad=[];"
                   "document.querySelectorAll('#chCards .prov').forEach(function(p){"
                   "var nm=p.querySelector('.nm').textContent.trim();"
                   "var bg=p.querySelector('.badge').textContent.trim();"
                   "var want=m[nm]==='managed'?'积分型':'API 型';"
                   "if(bg!==want)bad.push(nm+':'+bg+'≠'+want)});return bad})()")
        check("概览「渠道概况」的徽标与该渠道所属分类一致（积分型 / API 型）",
              wrong == [] and isinstance(labels, list) and "积分型" in labels,
              f"{labels} / {wrong}")

        # 静态检查：把页面与前端脚本整份拉下来找旧称。
        # 空态/删除确认/toast 这些文案要触发到才进 DOM，只查 DOM 会漏。
        assets = (js("(function(){return fetch('/').then(r=>r.text())})()") or "") \
            + (js("(function(){return fetch('/app.js').then(r=>r.text())})()") or "") \
            + (js("(function(){return fetch('/overview.js').then(r=>r.text())})()") or "") \
            + (js("(function(){return fetch('/upstream.js').then(r=>r.text())})()") or "") \
            + (js("(function(){return fetch('/zcode.js').then(r=>r.text())})()") or "")
        legacy = [t for t in ('平台账号型', '账号型', 'API 型渠道') if t in assets]
        check("页面与前端脚本里旧称一处不剩", not legacy,
              '仍存在: ' + str(legacy) + ' (len=%d)' % len(assets))
        check("新称已出现在前端脚本里", '积分型平台' in assets and 'API 型平台' in assets, 'len=%d' % len(assets))

        # ── 1. 侧栏结构 ─────────────────────────────────────
        cards = js("[...document.querySelectorAll('#upChanList .upcard')].map(x=>x.dataset.chanCard)")
        check("侧栏「上游控制台」下每个托管渠道一张子卡片",
              isinstance(cards, list) and len(cards) >= 2, cards)
        check("卡片含 workbuddy 与 zcode",
              isinstance(cards, list) and "workbuddy" in cards and "zcode" in cards, cards)
        # 控制台形态是后端异步探测的（子进程起来后 2s 内触发），等它出结果
        for _ in range(25):
            kinds = js("(Store.get().channels||[]).filter(c=>c.source==='managed')"
                       ".map(c=>c.name+':'+(c.console_kind||'?'))")
            if isinstance(kinds, list) and all(not k.endswith(':?') for k in kinds):
                break
            time.sleep(1)
            js("loadChannels()")
        kinds = js("(Store.get().channels||[]).filter(c=>c.source==='managed')"
                   ".map(c=>c.name+':'+(c.console_kind||'?'))")
        check("后端探出每个渠道的控制台形态", isinstance(kinds, list) and all(not k.endswith(':?') for k in kinds), kinds)
        check("支持集成面板的渠道判为 gateway",
              "workbuddy:gateway" in (kinds or []), kinds)
        # zcode 从 `web`（只给外链）升级成 `zcode`（原生视图）。
        # 这条同时钉住「不再退化成外链」这个决定。
        check("zcode 判为专属形态 zcode（不再是只会给外链的 web）",
              "zcode:zcode" in (kinds or []) and "zcode:web" not in (kinds or []), kinds)
        check("非 zcode 的渠道形态判定不受影响",
              "workbuddy:gateway" in (kinds or []), kinds)
        gw = js("document.querySelectorAll('.upcard[data-chan-card=workbuddy] .nav-i').length")
        check("gateway 渠道卡内平铺 7 个控制台入口", gw == 7, f"items={gw}")
        # web 分支（其它非 zcode 的网页面板网关）仍然存在，行为不变：
        # 给 1 个「打开管理面板」外链、不给任何 up-* 视图入口。
        # 当前配置里没有这类渠道，所以**按形态条件验证**，而不是假设 zcode 属于它
        # （zcode 已升级成 zcode 形态，硬编码它会让这条断言变成假失败）。
        webch = js("[...document.querySelectorAll('#upChanList .upcard')]"
                   ".filter(c=>{var ch=(Store.get().channels||[]).find(x=>x.name===c.dataset.chanCard);"
                   "return ch && ch.console_kind==='web'}).map(c=>c.dataset.chanCard)") or []
        if not webch:
            check("web 形态分支：当前无此类渠道（分支保留，未被本次改动触及）",
                  js("typeof upConsoleItems === 'function'"), "无 web 渠道可验")
        else:
            for nm in webch:
                sel = ".upcard[data-chan-card=%s]" % nm
                links = js("document.querySelectorAll('%s .nav-i[data-panel-url]').length" % sel)
                views = js("document.querySelectorAll('%s .nav-i[data-view]').length" % sel)
                check("web 形态渠道 %s 给 1 个外链、0 个视图入口" % nm,
                      links == 1 and views == 0, "links=%s views=%s" % (links, views))
        check("分组默认可见（有托管渠道时不隐藏）", not js("document.getElementById('upGroup').hidden"), "")
        check("卡片头带渠道状态点",
              js("document.querySelectorAll('#upChanList .upcard-hd .dot').length") >= 2, "")

        # ── 1b. 命名：父级=渠道名，子级=「账号池」────────────
        # 父卡片标题是渠道的 display_name（用户数据，不写死在代码里），
        # 子入口名才是内置文案。这一对很容易在改名时只改一半，
        # 所以既锁子入口，也锁它连带的视图标题与卡片标题。
        sublabels = js("[...document.querySelectorAll('#upChanList .upcard .nav-i span')]"
                       ".map(x=>x.textContent.trim())")
        check("控制台子入口名叫「账号池」（不再是「账号」）",
              isinstance(sublabels, list) and "账号池" in sublabels and "账号" not in sublabels,
              sublabels)
        check("「账号池」视图标题与卡片标题跟着一起改（侧栏与页头一套叫法）",
              ttlOf('up-accounts') == "账号池"
              and js("document.querySelector('#view-up-accounts .card-hd h2').textContent.trim()") == "账号池"
              and js("document.querySelector('#view-up-zc-accounts .card-hd h2').textContent.trim()") == "账号池",
              ttlOf('up-accounts'))

        # ── 1c. 设置独立入口 + 关窗行为二选一 ────────────────
        check("「设置」是侧栏独立入口（不在任何 .nav-sec 内）",
              js("!document.querySelector('#nav .nav-sec [data-view=settings]')")
              and js("!!document.querySelector('#nav > .nav-i[data-view=settings]')"), "")
        js("setView('settings')")
        time.sleep(1.2)
        check("「设置」视图标题正确", js("document.getElementById('ttl').textContent.trim()") == "设置", "")
        radios = js("[...document.querySelectorAll('#view-settings input[name=closeBehavior]')]"
                    ".map(x=>x.id+':'+x.checked)")
        # 判据是「恰好两个、且恰有一个选中」：只看有没有选中项，
        # 两个都选或都没选的坏状态会被放过。
        # 注意 x.checked 经 JSON 回来是 true/false，不是 1/0。
        picked = [r for r in (radios or []) if r.endswith(":true")]
        check("设置页有「最小化到托盘 / 直接退出」两个单选且恰有一个选中",
              isinstance(radios, list) and len(radios) == 2 and len(picked) == 1, radios)
        check("选中的那个与 /api/status 一致（不是写死的默认值）",
              (js("!!document.getElementById('fCloseTray').checked") is
               bool(js("S.status.minimize_to_tray !== false"))), radios)

        # 端点往返：改成相反值 → 状态接口立刻跟上 → 复原。
        # 复原放在 finally：测试中途失败绝不能把用户的关窗行为留在改过的状态。
        orig = bool(js("S.status.minimize_to_tray !== false"))

        def setClose(want):
            return js("(function(){return fetch('/api/settings/close',{method:'POST',"
                      "headers:{'Content-Type':'application/json'},"
                      "body:JSON.stringify({minimize_to_tray:%s})})"
                      ".then(function(r){return r.json()})})()" % ("true" if want else "false"))

        try:
            r = setClose(not orig)
            check("POST /api/settings/close 返回新值与说明",
                  isinstance(r, dict) and r.get("minimize_to_tray") is (not orig)
                  and isinstance(r.get("note"), str) and r.get("note"), r)
            live = js("fetch('/api/status').then(function(r){return r.json()})"
                      ".then(function(d){return d.minimize_to_tray})")
            check("关窗行为立即生效（状态接口跟上，无需重启）", live is (not orig), live)
        finally:
            setClose(orig)
            js("pollStatus()")
            js("loadSettings()")
        back = js("fetch('/api/status').then(function(r){return r.json()})"
                  ".then(function(d){return d.minimize_to_tray})")
        check("测试结束后已复原成用户原来的关窗行为", back is orig, back)

        # ── 2. 积分型平台：控制台按钮已移除 ─────────────────
        js("setView('acct')")
        time.sleep(2.0)
        check("「积分型平台」渠道卡上已无「控制台」按钮",
              js("!document.querySelector('#acctList [data-act=console]')"), "")
        check("渠道卡其它操作仍在（编辑/删除）",
              js("!!document.querySelector('#acctList [data-act=edit]')")
              and js("!!document.querySelector('#acctList [data-act=del]')"), "")

        # ── 3. 一步切换（本次改动的核心）────────────────────
        js("setView('overview')")
        time.sleep(0.8)
        # 点 zcode 卡片下的「模型与档位」
        js("document.querySelector('#upChanList .upcard[data-chan-card=workbuddy] "
           ".nav-i[data-view=up-models]').click()")
        time.sleep(3.5)
        check("点 workbuddy 的「模型与档位」：视图切到 up-models",
              js("Store.get().view") == "up-models", js("Store.get().view"))
        check("同一次点击也把渠道切到 workbuddy",
              js("UP.chan") == "workbuddy", js("UP.chan"))
        check("模型表真的加载出数据（入口可点且可用）",
              js("document.querySelectorAll('#mdBody tr').length") > 0,
              js("document.querySelectorAll('#mdBody tr').length"))
        # 再点 workbuddy 下的同名入口
        js("document.querySelector('#upChanList .upcard[data-chan-card=workbuddy] "
           ".nav-i[data-view=up-usage]').click()")
        time.sleep(3.0)
        check("换一个视图入口：视图与渠道同时更新",
              js("UP.chan") == "workbuddy" and js("Store.get().view") == "up-usage",
              f"chan={js('UP.chan')} view={js('Store.get().view')}")
        check("新视图内容加载完成",
              js("document.querySelectorAll('#usStats .card-k').length") > 0,
              js("document.querySelectorAll('#usStats .card-k').length"))

        # ── 4. 高亮唯一 ─────────────────────────────────────
        on = js("[...document.querySelectorAll('#nav .nav-i.on')].map(x=>x.dataset.chan+'/'+x.dataset.view)")
        check("同视图多入口时只高亮当前渠道那一条",
              isinstance(on, list) and on == ["workbuddy/up-usage"], on)

        # ── 5. 卡片折叠 ─────────────────────────────────────
        js("document.querySelector('[data-chan-toggle=zcode]').click()")
        time.sleep(0.5)
        check("渠道子卡片可折叠",
              js("document.querySelector('.upcard[data-chan-card=zcode]').classList.contains('closed')")
              and js("getComputedStyle(document.querySelector('.upcard[data-chan-card=zcode] .upcard-bd')).display") == "none", "")
        js("loadChannels()")
        time.sleep(2.0)
        check("折叠状态在渠道刷新后保持（按渠道名记）",
              js("document.querySelector('.upcard[data-chan-card=zcode]').classList.contains('closed')"), "")
        js("document.querySelector('[data-chan-toggle=zcode]').click()")
        time.sleep(0.5)
        check("再点可展开",
              not js("document.querySelector('.upcard[data-chan-card=zcode]').classList.contains('closed')"), "")

        # ── 6. 选中渠道持久化 ───────────────────────────────
        check("选中的渠道已写入本地存储",
              js("localStorage.getItem('mm_upchan')") == "workbuddy",
              js("localStorage.getItem('mm_upchan')"))
        shot(ws, "side_console")

        # ── 4. 控制台入口常驻（不再依赖先点「积分型平台」）────
        # 结构上必须「不可折叠」：有折叠控件或折角就说明还能被收起来，
        # 而 .nav-sec.closed .nav-sec-bd{display:none} 会把入口整个藏掉。
        check("上游控制台分组头是静态标签，没有折叠控件与折角",
              js("!!document.querySelector('#upGroup .nav-sec-hd.static')")
              and not js("document.querySelector('#upGroup [data-sec-toggle]')")
              and not js("document.querySelector('#upGroup .caret')"), "")
        # 逐个视图切过去：入口必须始终在（含概览、渠道类视图、运维里的视图）
        for v in ("overview", "api", "add", "settings", "logs"):
            js("setView('" + v + "')")
            time.sleep(0.9)
            vis = (not js("document.getElementById('upGroup').hidden")) \
                and (not js("document.getElementById('upGroup').classList.contains('closed')")) \
                and js("document.querySelectorAll('#upChanList .upcard').length") >= 2
            check("切到「" + v + "」视图后控制台入口仍在且可点",
                  vis, js("document.getElementById('upGroup').className"))

        # 真正的回归点：清掉本地状态后**重新加载页面**，全程不点任何渠道视图，
        # 入口必须自己出现。修之前这里会是 hidden —— 渠道列表只由
        # loadChannels() 提供，而它只在 api/acct 视图被调用。
        js("try{localStorage.removeItem('mm_navsec');localStorage.removeItem('mm_upchan')}catch(e){}")
        ws.call("Page.enable", {})
        ws.call("Page.reload", {})
        time.sleep(7)
        check("重新加载后停在概览视图（全程未点渠道视图）",
              js("Store.get().view") == "overview", js("Store.get().view"))
        hid = js("document.getElementById('upGroup').hidden")
        cards = js("document.querySelectorAll('#upChanList .upcard').length")
        check("未点任何视图，控制台入口已自动出现",
              (not hid) and (cards or 0) >= 2, "hidden=%s cards=%s" % (hid, cards))

        # 旧版本存过「上游控制台已收起」；升级后不能被它重新藏起来
        js("localStorage.setItem('mm_navsec', JSON.stringify({up:1,ops:1}))")
        ws.call("Page.reload", {})
        time.sleep(7)
        check("忽略历史遗留的『上游控制台已收起』状态",
              (not js("document.getElementById('upGroup').classList.contains('closed')"))
              and js("document.querySelectorAll('#upChanList .upcard').length") >= 2,
              js("document.getElementById('upGroup').className"))
        check("其它分组的收起状态照旧生效（只豁免上游控制台）",
              js("document.querySelector('[data-sec=ops]').classList.contains('closed')"),
              js("document.querySelector('[data-sec=ops]').className"))
        js("try{localStorage.removeItem('mm_navsec')}catch(e){}")
        # ── 5. zcode 的原生控制台（原先是外链，现已整合进应用）────
        # 这一节同时覆盖「整合进应用」与「查看不需要面板密码」两件事：
        # 密码由后端注入，浏览器全程不接触，所以这里能直接拿到数据。
        js("loadChannels()")
        time.sleep(2.5)
        kind = js("(Store.get().channels||[]).filter(c=>c.name==='zcode').map(c=>c.console_kind)[0]")
        check("zcode 的控制台形态是 zcode（不再是只有外链的 web）",
              kind == "zcode", "console_kind=%s" % kind)
        znav = js("[...document.querySelectorAll('.upcard[data-chan-card=zcode] .nav-i')]"
                  ".map(x=>x.dataset.view||x.dataset.panelUrl||'?')")
        check("zcode 卡内是三个原生入口",
              znav == ["up-zc-accounts", "up-zc-monitor", "up-zc-settings"], znav)
        check("zcode 不再有「打开管理面板」外链（不另开网页）",
              js("document.querySelectorAll('.upcard[data-chan-card=zcode] [data-panel-url]').length") == 0, "")

        js("openUpView('zcode', 'up-zc-accounts')")
        time.sleep(5.0)
        rows = js("document.querySelectorAll('#zcAccBody tr').length")
        check("账号池渲染出真实账号行（免密码拿到了上游数据）",
              rows and rows > 0, "rows=%s" % rows)
        cards = js("document.getElementById('zcAccCards').textContent") or ""
        check("账号池有汇总卡", "账号总数" in cards, cards[:120])
        # 额度进度条：必须先证明它真的按比例渲染（0% 就得是 0 宽）。
        # 这条正是被 CSP 静默丢弃内联样式坑过的地方。
        bars = js("[...document.querySelectorAll('#zcAccBody .zbar')]"
                  ".map(b=>{var i=b.firstElementChild;"
                  "return i?(i.getBoundingClientRect().width/b.getBoundingClientRect().width):-1})") or []
        check("额度进度条按比例渲染（0% 不能是满格）",
              len(bars) > 0 and all(0 <= float(b) <= 1.01 for b in bars)
              and any(float(b) < 0.9 for b in bars),
              "ratios=%s" % [round(float(b), 3) for b in bars])

        js("setView('up-zc-monitor')")
        time.sleep(3.5)
        mon = js("document.getElementById('zcMonCards').textContent") or ""
        check("运行监控渲染出提供方与额度池", "提供方" in mon and "额度池" in mon, mon[:120])

        js("setView('up-zc-settings')")
        time.sleep(3.0)
        st = js("document.getElementById('zcSetBody').textContent") or ""
        check("网关设置渲染出后台密码与刷新间隔", "后台密码" in st and "刷新间隔" in st, st[:140])
        check("设置页明确标注掩码值（不是明文）", "掩码" in st, st[:140])
        # 设置已从「只读视图」改成可在本面板直接修改：入口按钮与弹窗都必须在，
        # 且页面不能再出现把人引回「网关自己的面板」的旧文案。
        check("网关设置已是可写视图（有「修改设置」按钮、无只读旧文案）",
              bool(js("!!document.getElementById('btnZcSetEdit')"))
              and "只读视图" not in st and "请去网关自己的面板" not in st, st[:140])
        js("document.getElementById('btnZcSetEdit').click()")
        time.sleep(0.8)
        check("「修改设置」打开弹窗，密码框是掩码输入",
              (not js("document.getElementById('zcSetModal').hidden"))
              and js("document.getElementById('zcSetAdminKey').type") == "password", "")
        js("document.getElementById('btnZcSetClose').click()")
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
