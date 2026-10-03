#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""主题闪烁修复验证。

思路：注入一个比页面任何脚本都早执行的监视器，记录 data-theme 的
每一次写入及其时机。判据不是「最终颜色对不对」（那本来就对），
而是「有没有在 body 已经出现之后才改主题」——那一步就是用户看到的闪烁。
"""
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
PORT = 9354
sys.stdout.reconfigure(encoding="utf-8")

results = []


def check(name, ok, detail=""):
    results.append((name, bool(ok), detail))
    print(("[PASS] " if ok else "[FAIL] ") + name + ("" if ok else "  ← " + str(detail)[:260]))


# 比页面内联脚本更早执行：记录 data-theme 的每一次写入与当时 body 是否存在
WATCHER = r"""
(function () {
  window.__themeLog = [];
  var t0 = Date.now();
  function snap(tag) {
    try {
      window.__themeLog.push({
        tag: tag, t: Date.now() - t0,
        theme: document.documentElement && document.documentElement.getAttribute('data-theme'),
        cs: (document.documentElement && document.documentElement.style.colorScheme) || '',
        body: !!document.body
      });
    } catch (e) {}
  }
  snap('inject');
  var orig = Element.prototype.setAttribute;
  Element.prototype.setAttribute = function (n, v) {
    if (n === 'data-theme') {
      window.__themeLog.push({ tag: 'set:' + v, t: Date.now() - t0, theme: v,
                               body: !!document.body, cs: '' });
    }
    return orig.apply(this, arguments);
  };
  document.addEventListener('DOMContentLoaded', function () { snap('dcl'); });
  window.addEventListener('load', function () { snap('load'); });
})();
"""


def main():
    edge = subprocess.Popen(
        [EDGE, "--headless=new", f"--remote-debugging-port={PORT}",
         "--user-data-dir=" + os.path.join(os.environ.get("TEMP", "."), "edgetheme"),
         "--no-sandbox", "--disable-gpu", "--window-size=1200,900", BASE],
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

        # 静态检查：脚本必须在 head 内、样式表之前、且是同步的
        html = js("document.documentElement.outerHTML") or ""
        head = html[:html.find("<body")] if "<body" in html else html
        # 主题脚本必须是同源外链：响应头有 script-src 'self' 的 CSP，
        # 内联脚本会被直接拦掉（这才是原先「加了脚本仍然闪」的真因）。
        script_at = head.find("/theme.js")
        link_at = head.find('rel="stylesheet"')
        check("首帧主题脚本由 <head> 以 /theme.js 外链引入",
              script_at >= 0, f"at={script_at}")
        check("脚本位于样式表之前（避免先按默认色绘制）",
              0 <= script_at < link_at, f"script={script_at} link={link_at}")
        seg = head[script_at:link_at] if script_at >= 0 else ""
        check("脚本是同步加载（无 defer/async）",
              "defer" not in seg and "async" not in seg, seg[:120])

        # Page.enable 必须先调：没它 addScriptToEvaluateOnNewDocument 与
        # Page.reload 都会被静默忽略——测试会「跑完但什么都没测」。
        ws.call("Page.enable", {})
        ws.call("Page.addScriptToEvaluateOnNewDocument", {"source": WATCHER})

        def probe(store, sys_scheme, label):
            """设置存储 + 模拟系统偏好 → 重载 → 读时间线。"""
            js("try{localStorage.removeItem('mm-theme')}catch(e){}")
            if store:
                js(f"try{{localStorage.setItem('mm-theme','{store}')}}catch(e){{}}")
            ws.call("Emulation.setEmulatedMedia",
                    {"features": [{"name": "prefers-color-scheme", "value": sys_scheme}]})
            ws.call("Page.reload", {})
            time.sleep(3.0)
            log = js("window.__themeLog") or []
            final = js("document.documentElement.dataset.theme")
            cs = js("document.documentElement.style.colorScheme")
            return log, final, cs

        # ── A. 存 dark + 系统亮色（用户遇到的正是这个组合）──────────
        log, final, cs = probe("dark", "light", "A")
        check("A 存 dark + 系统亮色：最终主题为 dark", final == "dark", f"final={final}")
        sets = [e for e in log if str(e.get("tag", "")).startswith("set:")]
        late = [e for e in sets if e.get("body")]
        check("A 主题写入发生在 body 出现之前（即首帧前）", not late, json.dumps(late, ensure_ascii=False))
        check("A 只写入一次（不存在 auto→dark 的二次切换）", len(sets) == 1,
              json.dumps(sets, ensure_ascii=False))
        # 注入脚本跑在文档创建的最初时刻，此时 <html> 可能还没解析出来
        # （getAttribute 返回 null）。关键是「还没有任何主题写入」，
        # 而不是它恰好等于 auto。
        first_theme = next((e["theme"] for e in log if e["tag"] == "inject"), "MISSING")
        check("A 文档创建时尚未发生主题写入",
              first_theme in (None, "auto"), str(first_theme))
        check("A color-scheme 首帧即 dark（避免样式到达前铺白底）",
              cs == "dark", f"colorScheme={cs}")
        dcl = next((e for e in log if e["tag"] == "dcl"), None)
        check("A DOMContentLoaded 时主题已是 dark",
              dcl and dcl.get("theme") == "dark", json.dumps(dcl, ensure_ascii=False))

        # ── B. 存 light + 系统暗色（反向）────────────────────────────
        log, final, cs = probe("light", "dark", "B")
        check("B 存 light + 系统暗色：最终主题为 light", final == "light", f"final={final}")
        sets = [e for e in log if str(e.get("tag", "")).startswith("set:")]
        late = [e for e in sets if e.get("body")]
        check("B 写入同样发生在首帧前", not late and len(sets) == 1,
              json.dumps(sets, ensure_ascii=False))
        check("B color-scheme 首帧即 light", cs == "light", f"colorScheme={cs}")

        # ── C. 无存储 → 跟随系统 ─────────────────────────────────────
        log, final, cs = probe(None, "light", "C")
        check("C 无存储时保持 auto（跟随系统）", final == "auto", f"final={final}")
        check("C color-scheme 跟随系统亮色", cs == "light", f"colorScheme={cs}")
        sets = [e for e in log if str(e.get("tag", "")).startswith("set:")]
        check("C 无存储时不产生任何主题写入（无切换）", not sets,
              json.dumps(sets, ensure_ascii=False))

        # ── D. 脏数据不能把界面带崩 ──────────────────────────────────
        log, final, cs = probe("blue", "dark", "D")
        check("D localStorage 脏值被白名单挡掉（保持 auto）", final == "auto", f"final={final}")

        passed = sum(1 for _, ok, _ in results if ok)
        print("\n" + "=" * 46 + f"\n{passed}/{len(results)} 通过")
        failed = [n for n, ok, _ in results if not ok]
        if failed:
            print("失败项：\n  - " + "\n  - ".join(failed))
        return 0 if passed == len(results) else 1
    finally:
        edge.terminate()


if __name__ == "__main__":
    sys.exit(main())
