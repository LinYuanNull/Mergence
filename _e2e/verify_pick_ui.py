#!/usr/bin/env python3
"""「拉取上游模型」全选 + 加入结果弹窗的 UI 验证。

必须驱动真实点击：全选的三态、已加入标记、结果弹窗都只在交互后出现，
纯 --screenshot 拍不到。

CDP 客户端复用 drive_ui.py 的实现（纯标准库 WebSocket，不引 playwright）。
"""
import json
import os
import re
import shutil
import subprocess
import sys
import time
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from drive_ui import WS, wait_json  # noqa: E402  复用 CDP 客户端

# 项目根：由本文件位置推导（_e2e/ 的上一层），不写死本机绝对路径。
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HOME = os.path.join(ROOT, "_e2e", "ui_home")
EXE = os.path.join(ROOT, "ModelMux.exe")
FAKE = os.path.join(ROOT, "_e2e", "fake_upstream.py")
SHOTS = os.path.join(ROOT, "_e2e", "shots")
# 用当前解释器；换机器/换 Python 位置不必改代码（可用 MODELMUX_PY 覆盖）。
PY = os.environ.get("MODELMUX_PY", sys.executable)
EDGE = r"C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe"
FAKE_PORT = 18094
CDP_PORT = 9335

sys.stdout.reconfigure(encoding="utf-8")
results = []


def check(name, cond, detail=""):
    results.append((name, bool(cond), detail))
    print(f"[{'PASS' if cond else 'FAIL'}] {name}" + (f"\n        {detail}" if not cond and detail else ""),
          flush=True)


def main():
    if os.path.isdir(HOME):
        shutil.rmtree(HOME, ignore_errors=True)
    os.makedirs(HOME, exist_ok=True)
    os.makedirs(SHOTS, exist_ok=True)

    fake = subprocess.Popen([PY, FAKE, str(FAKE_PORT)],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    mm = edge = ws = None
    try:
        time.sleep(1.0)
        env = dict(os.environ)
        env["MODELMUX_HOME"] = HOME
        env["MODELMUX_HEADLESS"] = "1"
        mm = subprocess.Popen([EXE], env=env,
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

        log = os.path.join(HOME, "logs", "modelmux.log")
        port, deadline = None, time.time() + 25
        while time.time() < deadline and not port:
            if os.path.isfile(log):
                for line in open(log, encoding="utf-8", errors="replace"):
                    if "内置服务已监听" in line:
                        m = re.search(r"127\.0\.0\.1:(\d+)", line)
                        if m:
                            port = int(m.group(1))
                            break
            time.sleep(0.3)
        if not port:
            print("未取得端口")
            return 1
        base = f"http://127.0.0.1:{port}"

        def post(path, obj):
            rq = urllib.request.Request(base + path, data=json.dumps(obj).encode(),
                                        headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(rq, timeout=30) as r:
                return json.loads(r.read())

        # 渠道：先建一个空的，编辑时才有「拉取上游模型」可用
        post("/api/channels", {
            "name": "", "display_name": "FakeChat", "preset": "custom",
            "base_url": f"http://127.0.0.1:{FAKE_PORT}/chat/v1",
            "protocol": "chat", "model_prefix": "fc", "api_keys": ["good"],
            "models": ["fake-alpha"], "enabled": True,
        })

        # 启动浏览器并接管
        edge = subprocess.Popen([
            EDGE, "--headless=new", f"--remote-debugging-port={CDP_PORT}",
            "--user-data-dir=" + os.path.join(HOME, "edge"),
            "--no-sandbox", "--disable-gpu", "--window-size=1280,900",
            base + "/",
        ], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        info = wait_json(f"http://127.0.0.1:{CDP_PORT}/json", timeout=30)
        if not info:
            print("CDP 未就绪")
            return 1
        page = next(t for t in info if t.get("type") == "page")
        ws = WS(page["webSocketDebuggerUrl"])
        def js(expr, timeout=30):
            """求值并取回值。走 WS.call（它自己管 id 与异常）。"""
            r = ws.call("Runtime.evaluate", {
                "expression": expr, "awaitPromise": True, "returnByValue": True,
            }, timeout=timeout)
            if "exceptionDetails" in r:
                return {"__err": json.dumps(r["exceptionDetails"], ensure_ascii=False)[:400]}
            return r.get("result", {}).get("value")

        def wait_for(expr, timeout=25, label=""):
            end = time.time() + timeout
            last = None
            while time.time() < end:
                last = js(expr)
                if last:
                    return last
                time.sleep(0.3)
            print(f"  [超时] {label or expr}  最后值={last}")
            return None

        time.sleep(2.5)

        # ── 打开渠道编辑弹层
        js("document.querySelector('[data-view=api]').click()")
        time.sleep(1.0)
        js("(function(){var b=[...document.querySelectorAll('button')].find(x=>x.textContent.trim()==='编辑');if(b)b.click();return !!b})()")
        ok = wait_for("!document.getElementById('chModal').hidden", label="编辑弹层")
        check("渠道编辑弹层打开", bool(ok))

        # 展开「模型与密钥」分组（拉取区在里面）
        js("(function(){var s=[...document.querySelectorAll('#chModal details.sec summary')].find(x=>x.textContent.includes('模型'));if(s)s.click();return !!s})()")
        time.sleep(0.5)

        # ── 拉取模型
        js("document.getElementById('btnFetch').click()")
        got = wait_for("document.querySelectorAll('#chips input').length > 0", label="拉取到模型")
        n_models = js("document.querySelectorAll('#chips input').length") or 0
        check("拉取到模型列表", n_models >= 3, f"数量={n_models}")

        # 规范化后的展示名
        names = js("[...document.querySelectorAll('#chips .chipsel')].map(l=>l.textContent.replace('Free','').trim())") or []
        raw_chips = js("[...document.querySelectorAll('#chips .chipsel')].map(l=>l.textContent)") or []
        # -f（hy4-preview-f）→ 夜间徽标；-x（hy3-x）→ 限时徽标
        check("夜间免费（-f）打「夜间」徽标",
              any("夜间" in t and "Preview" in t for t in raw_chips), str(raw_chips))
        check("限时免费（-x）打「限时」徽标",
              any("限时" in t and "Hy3" in t for t in raw_chips), str(raw_chips))
        # 夜间徽标必须带时段说明（title），否则用户不知道什么时候能用
        titles = js("[...document.querySelectorAll('#chips .chipsel .badge')].map(b=>({t:b.textContent,title:b.title}))") or []
        check("夜间徽标带时段说明与当前状态",
              any(t["t"].startswith("夜间") and "23:00" in (t["title"] or "")
                  for t in titles), str(titles))
        check("K+数字模型的 K 大写（Kimi-K2）",
              any("Kimi-K2" in t for t in names), str(names))
        check("auto 的 A 大写（Auto）", "Auto" in names, str(names))

        # ── 全选：三态
        js("document.getElementById('fSelectAll').click()")
        time.sleep(0.4)
        st = js("({checked: document.getElementById('fSelectAll').checked, ind: document.getElementById('fSelectAll').indeterminate, on: document.querySelectorAll('#chips input:checked').length})")
        check("全选勾上全部模型",
              st and st["checked"] and st["on"] == n_models and not st["ind"], str(st))
        label = js("document.getElementById('selectAllLabel').textContent")
        check("全选标签显示当前/总数", label and "当前" in label and str(n_models) in label, str(label))

        # 取消全选
        js("document.getElementById('fSelectAll').click()")
        time.sleep(0.4)
        st2 = js("({on: document.querySelectorAll('#chips input:checked').length, checked: document.getElementById('fSelectAll').checked})")
        check("再次点击可取消全选", st2 and st2["on"] == 0 and not st2["checked"], str(st2))

        # 部分勾选 → indeterminate
        js("document.querySelectorAll('#chips input')[0].click()")
        time.sleep(0.4)
        st3 = js("({ind: document.getElementById('fSelectAll').indeterminate})")
        check("部分勾选时全选框为半选态", st3 and st3["ind"] is True, str(st3))

        # ── 勾选两个命中规范化规则的模型：加入 → 弹窗
        js("(function(){var b=[...document.querySelectorAll('#chips input')];var want=['deepseek-v4-pro-cn','hy3-x'];b.forEach(x=>{x.checked=want.indexOf(x.value)>=0;x.dispatchEvent(new Event('change'))});return 1})()")
        time.sleep(0.3)
        js("document.getElementById('btnAddPicked').click()")
        time.sleep(0.6)
        vis = js("!document.getElementById('pickModal').hidden")
        check("加入后弹出结果弹窗", bool(vis))
        body = js("document.getElementById('pickBody').textContent.replace(/\\s+/g,' ').trim()") or ""
        check("弹窗列出本次已加入的模型", "本次已加入" in body, body[:300])
        check("弹窗列出尚未加入的模型", "尚未加入" in body, body[:300])
        # 条目必须真的有内容：曾经把「未勾选」区传成字符串数组，
        # 渲染函数取不到 name，5 个条目全成了空 span（标题在、内容空）。
        items = js("[...document.querySelectorAll('#pickBody .pick-i')].map(x=>x.textContent.trim())") or []
        check("弹窗条目渲染出实际模型名（非空）",
              len(items) >= 3 and all(i for i in items), str(items))
        # 免费模型在弹窗里也要带徽标（这是选择时要权衡的信息）
        check("弹窗内保留免费类型徽标",
              any("夜间" in i or "限时" in i for i in items), str(items))
        summary = js("document.getElementById('pickSummary').textContent") or ""
        check("弹窗底部有计数汇总", "加入" in summary and "未勾选" in summary, summary)

        # 写入的模型列表
        mlist = js("document.getElementById('fModels').value") or ""
        check("命中规则的模型写入「真实ID => 展示名」",
              "deepseek-v4-pro-cn => Deepseek-V4-Pro" in mlist
              and "hy3-x => Hy3-Free" in mlist, mlist[:300])

        import base64
        shot = base64.b64decode(ws.call("Page.captureScreenshot",
                                       {"format": "png"})["data"])
        if shot:
            p = os.path.join(SHOTS, "pick_modal.png")
            open(p, "wb").write(shot)
            print("截图:", p)

        # 关闭弹窗
        js("document.getElementById('btnPickOk').click()")
        time.sleep(0.4)
        check("弹窗可关闭", js("document.getElementById('pickModal').hidden") is True)

        # ── 已加入的模型仍在列表里，并带「已加入」标记
        vis2 = js("(function(){var b=[...document.querySelectorAll('#chips .chipsel')].find(l=>l.textContent.includes('已加入'));return b?b.textContent.trim():''})()")
        check("已加入的模型仍显示在拉取列表中", bool(vis2), str(vis2))
        still = js("document.querySelectorAll('#chips input').length")
        check("列表数量不因已加入而减少", still == n_models, f"{still} vs {n_models}")

        # ── 全部已加入的情形
        js("(function(){var b=document.querySelectorAll('#chips input');b.forEach(x=>{if(!x.checked)x.dispatchEvent(new Event('change'))});b[0].checked=true;b[0].dispatchEvent(new Event('change'));return 1})()")
        time.sleep(0.3)
        # 只勾一个「已加入」的模型 → 本次无新增
        js("document.getElementById('btnAddPicked').click()")
        time.sleep(0.6)
        body2 = js("document.getElementById('pickBody').textContent.replace(/\\s+/g,' ').trim()") or ""
        check("全部已加入时给出对应提示",
              "无需重复加入" in body2 or "已在列表中" in body2, body2[:200])
        js("document.getElementById('btnPickOk').click()")

    finally:
        for p in (ws, edge, mm, fake):
            try:
                if p:
                    p.terminate() if p is not mm else p.kill()
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
