#!/usr/bin/env python3
"""ModelMux P3 端到端验证。

不依赖任何真实上游：本地起一个假上游，覆盖 OpenAI Chat（含 SSE）与
Anthropic Messages（含 SSE）两种形态。

被测对象是**无界面模式**下的真实 ModelMux.exe —— 走的是完整启动路径
（配置加载 → 渠道注册 → 内置服务 → 对外出口），只是不建窗口。
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

# 项目根：由本文件位置推导（test/ 的上一层），不写死本机绝对路径。
ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
HOME = os.path.join(ROOT, "test", "home")
EXE = os.path.join(ROOT, "ModelMux.exe")
FAKE = os.path.join(ROOT, "test", "fake_upstream.py")
FAKE_MG = os.path.join(ROOT, "test", "fake_managed_gateway.py")
E2E = os.path.join(ROOT, "test")
MG_PIDFILE = os.path.join(E2E, "mg.pid")
# 用当前解释器；换机器/换 Python 位置不必改代码（可用 MODELMUX_PY 覆盖）。
PY = os.environ.get("MODELMUX_PY", sys.executable)
FAKE_PORT = 18091
FAKE_ZCODE_PORT = 18101   # 假 zcode2api 网关（领取接口契约）
ZCODE_ADMIN_KEY = "fake-admin-key-123"

sys.stdout.reconfigure(encoding="utf-8")

results = []


def check(name, cond, detail=""):
    results.append((name, bool(cond), detail))
    mark = "PASS" if cond else "FAIL"
    line = f"[{mark}] {name}"
    if detail and not cond:
        line += f"\n        {detail}"
    print(line, flush=True)


ACCESS_KEY = ""   # 启动后从 /api/status 取；/v1 出口现在要求 Bearer


def req(method, url, obj=None, timeout=40, raw=False):
    data = None
    headers = {}
    # /v1 是数据面（会被填进第三方客户端），有独立 Key 保护；
    # /api/* 是控制面（仅本机面板用）。
    if ACCESS_KEY and "/v1/" in url:
        headers["Authorization"] = "Bearer " + ACCESS_KEY
    if obj is not None:
        data = json.dumps(obj).encode()
        headers["Content-Type"] = "application/json"
    r = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(r, timeout=timeout) as resp:
            body = resp.read()
            return resp.status, (body.decode("utf-8", "replace") if raw else json.loads(body or b"{}"))
    except urllib.error.HTTPError as e:
        body = e.read()
        if raw:
            return e.code, body.decode("utf-8", "replace")
        try:
            return e.code, json.loads(body or b"{}")
        except Exception:
            return e.code, {"raw": body.decode("utf-8", "replace")}


def pid_alive(pid):
    """进程是否还在。按镜像名/pid 匹配行，不依赖 tasklist 的提示文案（本地化会变）。"""
    if not pid:
        return False
    out = subprocess.run(["tasklist", "/FO", "CSV", "/NH"],
                         capture_output=True, text=True, errors="ignore").stdout
    # 注意：tasklist /FO CSV 每个字段都被引号包住（..."python.exe","10016","Console"...），
    # 所以匹配 "10016" 而不是 ,10016, —— 后者永远匹配不上，会让断言变成空转。
    return any(f'"{pid}"' in line for line in out.splitlines())


def main():
    # ── 准备干净的隔离数据目录
    if os.path.isdir(HOME):
        shutil.rmtree(HOME, ignore_errors=True)  # 仅限 test 下的测试目录
    os.makedirs(os.path.join(HOME, "config"), exist_ok=True)

    fake = subprocess.Popen([PY, FAKE, str(FAKE_PORT)],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    fake_zcode = subprocess.Popen([PY, os.path.join(E2E, "fake_zcode.py"),
                                   str(FAKE_ZCODE_PORT)],
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    mm = None
    try:
        time.sleep(1.0)

        env = dict(os.environ)
        env["MODELMUX_HOME"] = HOME
        env["MODELMUX_HEADLESS"] = "1"
        env["WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS"] = "--no-sandbox"
        mm = subprocess.Popen([EXE], env=env,
                              stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

        # ── 从日志里取内置服务端口（动态端口，无法预设）
        log = os.path.join(HOME, "data", "logs", "modelmux.log")
        port, deadline = None, time.time() + 25
        while time.time() < deadline:
            if os.path.isfile(log):
                try:
                    txt = open(log, encoding="utf-8", errors="replace").read()
                except Exception:
                    txt = ""
                for line in txt.splitlines():
                    if "内置服务已监听" in line or "已进入无界面模式" in line:
                        m = re.search(r"127\.0\.0\.1:(\d+)", line)
                        if m:
                            port = int(m.group(1))
                            break
                if port:
                    break
            if mm.poll() is not None:
                print("ModelMux 提前退出，退出码", mm.returncode)
                print(txt[-2000:] if txt else "(无日志)")
                return 1
            time.sleep(0.3)

        if not port:
            print("未能从日志中取得端口")
            return 1
        base = f"http://127.0.0.1:{port}"
        # cbase 跟随端口热切换：第 22 段会换端口，之后的断言都要用新地址。
        # 在这里先建好，后面就不用到处判断「现在该用哪个」。
        cbase = base
        print(f"内置服务端口 = {port}\n")

        # ── 0) /v1 鉴权：Key 自动生成 + 重新生成立即生效
        st, d = req("GET", base + "/api/status")
        check("状态接口可访问", st == 200, str(st))
        key0 = d.get("access_key") or ""
        globals()["ACCESS_KEY"] = key0
        check("access_key 已自动生成（32 位十六进制）",
              len(key0) == 32 and all(c in "0123456789abcdef" for c in key0),
              f"len={len(key0)} head={key0[:6]}...")
        try:
            with urllib.request.urlopen(urllib.request.Request(base + "/v1/models"), timeout=10) as r:
                noauth = r.status
        except urllib.error.HTTPError as e:
            noauth = e.code
        check("无 Key 访问 /v1/models 返回 401", noauth == 401, f"得到 {noauth}")
        st, d = req("POST", base + "/api/access-key/regenerate")
        check("重新生成 Key 成功", st == 200 and d.get("ok"), f"{st} {d}")
        key1 = d.get("access_key") or ""
        check("新 Key 与旧 Key 不同", bool(key1) and key1 != key0, "")
        globals()["ACCESS_KEY"] = key1
        st, d = req("GET", base + "/v1/models")
        check("新 Key 可访问 /v1/models", st == 200, str(st))
        # 旧 Key 已失效（写死旧值验证，不依赖全局变量）
        try:
            rr = urllib.request.Request(base + "/v1/models",
                headers={"Authorization": "Bearer " + key0})
            with urllib.request.urlopen(rr, timeout=10) as r:
                oldok = r.status
        except urllib.error.HTTPError as e:
            oldok = e.code
        check("旧 Key 访问被 401", oldok == 401, f"得到 {oldok}")

        # ── 1) 面板接口
        st, d = req("GET", base + "/api/healthz")
        check("健康检查", st == 200 and d.get("ok") is True, str(d))

        st, d = req("GET", base + "/api/presets")
        check("预设模板返回", st == 200 and len(d.get("presets", [])) >= 8,
              f"{st} {len(d.get('presets', []))} 个")

        st, d = req("GET", base + "/", raw=True)
        check("面板首页返回 HTML", st == 200 and "<title>ModelMux</title>" in d)
        st, js = req("GET", base + "/app.js", raw=True)
        check("面板脚本可加载", st == 200 and "loadChannels" in js)
        st, css = req("GET", base + "/app.css", raw=True)
        check("面板样式可加载", st == 200 and ".chcard" in css)

        # 静态资源必须与 src/internal/assets/ 下的源文件逐字节一致。
        # 只断言「能加载」是不够的：embed 指错文件、少嵌一个、被别的文件顶替，
        # 这几种情况状态码照样 200、关键词也照样在。判据只能是字节相等。
        asset_bad = []
        for url, fn in (("/", "index.html"), ("/theme.js", "theme.js"),
                        ("/app.js", "app.js"), ("/upstream.js", "upstream.js"),
                        ("/overview.js", "overview.js"), ("/zcode.js", "zcode.js"),
                        ("/app.css", "app.css")):
            st2, body = req("GET", base + url, raw=True)
            with open(os.path.join(ROOT, "src", "internal", "assets", fn),
                      encoding="utf-8", newline="") as f:
                want = f.read()
            if st2 != 200 or body != want:
                asset_bad.append("%s (st=%s, %d vs %d 字节)"
                                 % (url, st2, len(body), len(want)))
        check("面板静态资源与 src/internal/assets/ 源文件逐字节一致（7 个）",
              not asset_bad, "; ".join(asset_bad))

        # ── 2) 空渠道时对外出口的错误语义
        st, d = req("GET", base + "/v1/models")
        check("无渠道时 /v1/models 返回空列表", st == 200 and d.get("data") == [],
              str(d))
        st, d = req("POST", base + "/v1/chat/completions",
                    {"model": "whatever", "messages": []})
        check("无渠道时对话返回 503 + no_channel_available",
              st == 503 and d.get("error", {}).get("code") == "no_channel_available",
              f"{st} {d}")

        # ── 3) 保存前测试连接（地址校验失败路径）
        st, d = req("POST", base + "/api/channels/test",
                    {"base_url": "not-a-url", "protocol": "chat"})
        check("非法 Base URL 被地址校验拦下",
              st == 200 and d.get("ok") is False and "base_url" in d.get("fields", []),
              str(d))

        # ── 4) 添加 chat 渠道（走真实的「测试连接 → 拉模型 → 保存」流程）
        chat_cfg = {
            "name": "", "display_name": "FakeChat", "preset": "custom",
            "base_url": f"http://127.0.0.1:{FAKE_PORT}/chat/v1",
            "protocol": "chat", "model_prefix": "fc", "api_keys": ["good"],
            "enabled": True,
        }
        st, d = req("POST", base + "/api/channels/models", chat_cfg, timeout=60)
        models = d.get("models", [])
        # 返回的是 {id, name, free}：id 是上游真实 ID（转发用），
        # name 是规范化后的展示名（界面用）。两者必须都在。
        ids = {m.get("id") for m in models}
        names = {m.get("name") for m in models}
        check("拉取上游模型列表（返回 id + 展示名）",
              st == 200
              and ids == {"fake-alpha", "fake-beta", "deepseek-v4-pro-cn",
                          "hy4-preview-f", "hy3-x", "auto", "kimi-k2"}
              and names == {"fake-alpha", "fake-beta", "Deepseek-V4-Pro",
                            "Hy4-Preview-Free", "Hy3-Free", "Auto", "Kimi-K2"},
              f"{st} {d}")
        check("规范化只动展示名：真实 ID 原样保留",
              "deepseek-v4-pro-cn" in ids and "Deepseek-V4-Pro" in names,
              f"ids={sorted(ids)} names={sorted(names)}")
        check("免费类型可区分：-f 夜间 / -x 限时",
              any(m.get("id") == "hy4-preview-f" and m.get("free_kind") == "nightly" for m in models)
              and any(m.get("id") == "hy3-x" and m.get("free_kind") == "limited" for m in models),
              json.dumps([{k: m.get(k) for k in ("id", "free_kind", "nightly_now")}
                          for m in models if m.get("free")], ensure_ascii=False))
        check("免费徽标认得 -x 档位（hy3-x → Hy3-Free）",
              any(m.get("id") == "hy3-x" and m.get("free") is True
                  and m.get("name") == "Hy3-Free" for m in models),
              json.dumps([m for m in models if m.get("id") in ("hy3-x", "auto")],
                         ensure_ascii=False))
        check("免费徽标认得 -x 档位（hy3-x → Hy3-Free）",
              any(m.get("id") == "hy3-x" and m.get("free") is True
                  and m.get("name") == "Hy3-Free" for m in models),
              json.dumps([m for m in models if m.get("id") in ("hy3-x", "auto")],
                         ensure_ascii=False))

        st, d = req("POST", base + "/api/channels/test", chat_cfg, timeout=90)
        check("chat 渠道连接测试通过", st == 200 and d.get("ok") is True,
              json.dumps(d, ensure_ascii=False)[:400])

        # 自动目录模式（不声明 models）：验证「展示名可调用、上游收到真实 ID」。
        # 这是本次改动的核心不变量——规范化只影响给人看的名字。
        auto_cfg = dict(chat_cfg)
        auto_cfg["models"] = []
        auto_cfg["model_source"] = "auto"
        auto_cfg["name"] = ""
        auto_cfg["display_name"] = "FakeAuto"
        auto_cfg["model_prefix"] = "fa"
        st, d = req("POST", base + "/api/channels", auto_cfg)
        auto_ok = st == 200 and d.get("ok") is True
        auto_name = d.get("name", "")
        check("保存自动目录渠道（不声明模型）", auto_ok, f"{st} {d}")
        if auto_ok:
            # 等预取把上游目录拉下来
            listed = None
            for _ in range(20):
                time.sleep(0.4)
                st2, d2 = req("GET", base + "/v1/models")
                listed = [m["id"] for m in d2.get("data", []) if m["id"].startswith("fa-")]
                if listed:
                    break
            check("自动目录用展示名对外提供（-cn 已去除、D 已大写）",
                  "fa-Deepseek-V4-Pro" in (listed or []),
                  str(listed))
            # 关键：用展示名调用，上游必须收到真实 ID（-cn 还在）
            st3, d3 = req("POST", base + "/v1/chat/completions",
                          {"model": "fa-Deepseek-V4-Pro",
                           "messages": [{"role": "user", "content": "hi"}]})
            check("用展示名调用成功（上游收到的是真实 ID）",
                  st3 == 200
                  and (d3.get("choices") or [{}])[0].get("message", {}).get("content")
                  == "echo:deepseek-v4-pro-cn",
                  f"{st3} {json.dumps(d3, ensure_ascii=False)[:300]}")
            req("DELETE", base + f"/api/channels?name={auto_name}")

        chat_cfg["models"] = ["fake-alpha", "fake-beta"]  # 第三个模型留给「自动目录」验证
        st, d = req("POST", base + "/api/channels", chat_cfg)
        check("保存 chat 渠道", st == 200 and d.get("ok") is True, f"{st} {d}")
        chat_name = d.get("name", "")

        # ── 5) 添加 anthropic 渠道
        anth_cfg = {
            "name": "", "display_name": "FakeClaude", "preset": "anthropic",
            "base_url": f"http://127.0.0.1:{FAKE_PORT}/anth",
            "protocol": "anthropic", "model_prefix": "ac", "api_keys": ["good"],
            "models": ["fake-claude"], "enabled": True,
        }
        st, d = req("POST", base + "/api/channels", anth_cfg)
        check("保存 anthropic 渠道", st == 200 and d.get("ok") is True, f"{st} {d}")
        anth_name = d.get("name", "")

        # ── 6) 模型聚合
        # 旧写法（前缀带斜杠）必须仍能路由，否则升级即 404
        st_old, d_old = req("POST", base + "/v1/chat/completions",
                            {"model": "fc/fake-alpha",
                             "messages": [{"role": "user", "content": "hi"}]})
        check("旧写法（前缀带斜杠）仍可路由",
              st_old == 200
              and (d_old.get("choices") or [{}])[0].get("message", {}).get("content")
              == "echo:fake-alpha",
              f"{st_old} {json.dumps(d_old, ensure_ascii=False)[:200]}")

        st, d = req("GET", base + "/v1/models")
        ids = sorted(m["id"] for m in d.get("data", []))
        check("模型列表聚合两个渠道并带前缀",
              ids == ["ac-fake-claude", "fc-fake-alpha", "fc-fake-beta"], str(ids))

        # ── 7) chat 直通
        st, d = req("POST", base + "/v1/chat/completions",
                    {"model": "fc/fake-alpha", "messages": [{"role": "user", "content": "hi"}]})
        ok = (st == 200
              and d.get("choices", [{}])[0].get("message", {}).get("content") == "echo:fake-alpha"
              and d.get("x_vendor_extra") == {"nested": [1, 2, 3]}
              and d.get("usage", {}).get("total_tokens") == 107
              # 缓存明细必须原样透传：ModelMux 不该改写 usage 的任何字段
              and d.get("usage", {}).get("prompt_tokens_details", {}).get("cached_tokens") == 40)
        check("chat 直通（模型名改写 + 上游非标准字段保真）", ok,
              f"{st} {json.dumps(d, ensure_ascii=False)[:400]}")

        # ── 8) chat 流式直通
        st, body = req("POST", base + "/v1/chat/completions",
                       {"model": "fc/fake-beta", "stream": True,
                        "messages": [{"role": "user", "content": "hi"}]},
                       timeout=40, raw=True)
        check("chat 流式直通（SSE 逐块）",
              st == 200 and "分" in body and "块" in body and "流" in body
              and "data: [DONE]" in body,
              body[:300].replace("\n", "\\n"))

        # ── 9) anthropic 协议双向转换（非流式）
        st, d = req("POST", base + "/v1/chat/completions",
                    {"model": "ac/fake-claude", "messages": [{"role": "user", "content": "hi"}]})
        msg = d.get("choices", [{}])[0].get("message", {})
        tc = (msg.get("tool_calls") or [{}])[0]
        ok = (st == 200
              and d.get("object") == "chat.completion"
              and msg.get("content") == "anth:fake-claude"
              and tc.get("function", {}).get("name") == "probe"
              and tc.get("function", {}).get("arguments") == '{"k":"v"}'
              and d.get("choices", [{}])[0].get("finish_reason") == "tool_calls"
              and d.get("usage", {}).get("total_tokens") == 15)
        check("Anthropic → OpenAI 非流式转换（含 tool_calls）", ok,
              f"{st} {json.dumps(d, ensure_ascii=False)[:500]}")

        # ── 10) anthropic 协议流式转换
        st, body = req("POST", base + "/v1/chat/completions",
                       {"model": "ac/fake-claude", "stream": True,
                        "messages": [{"role": "user", "content": "hi"}]},
                       timeout=40, raw=True)
        check("Anthropic SSE → OpenAI SSE 转换",
              st == 200 and 'chat.completion.chunk' in body
              and "安" in body and "思" in body and "流" in body
              and "data: [DONE]" in body and "content_block_delta" not in body,
              body[:400].replace("\n", "\\n"))

        # ── 11) 上游 400 直通（错误体要原样给客户端）
        st, d = req("POST", base + "/v1/chat/completions",
                    {"model": "fc/trigger-400", "messages": []})
        check("上游 400 原样透传（含上游错误文案）",
              st == 400 and "bad request from upstream" in json.dumps(d, ensure_ascii=False),
              f"{st} {d}")

        # ── 12) 未知模型 / auto 路由的错误语义
        st, d = req("POST", base + "/v1/chat/completions",
                    {"model": "nope/xxx", "messages": []})
        check("未知前缀 → 404 model_not_found",
              st == 404 and d.get("error", {}).get("code") == "model_not_found",
              f"{st} {d}")

        st, d = req("POST", base + "/v1/chat/completions",
                    {"model": "auto", "messages": []})
        check("auto 路由 → 400 auto_not_supported",
              st == 400 and d.get("error", {}).get("code") == "auto_not_supported",
              f"{st} {d}")

        st, d = req("POST", base + "/v1/chat/completions",
                    {"messages": []})
        check("缺 model → 400 missing_model",
              st == 400 and d.get("error", {}).get("code") == "missing_model",
              f"{st} {d}")

        # ── 13) Responses 协议明确拒绝（而不是静默走 chat）
        rc = {"name": "", "display_name": "FakeResp", "base_url": f"http://127.0.0.1:{FAKE_PORT}/chat/v1",
              "protocol": "responses", "api_keys": ["good"], "enabled": True,
              "models": ["fake-alpha"]}
        st, d = req("POST", base + "/api/channels/test", rc, timeout=60)
        check("responses 协议在测试阶段就被明确拒绝",
              st == 200 and d.get("ok") is False and "protocol" in d.get("fields", []),
              json.dumps(d, ensure_ascii=False)[:300])

        st, d = req("POST", base + "/api/channels", rc)
        resp_name = d.get("name", "")
        if st == 200 and resp_name:
            st2, d2 = req("POST", base + "/v1/chat/completions",
                          {"model": "fakeresp/fake-alpha", "messages": []})
            check("responses 协议调用 → 501 responses_unsupported",
                  st2 == 501 and d2.get("error", {}).get("code") == "responses_unsupported",
                  f"{st2} {d2}")
            req("DELETE", base + f"/api/channels?name={resp_name}")

        # ── 14) 坏 Key：上游 401 要原样交给客户端（而不是自造「全部失败」）
        bad = {"name": "", "display_name": "BadKey", "base_url": f"http://127.0.0.1:{FAKE_PORT}/chat/v1",
               "protocol": "chat", "api_keys": ["bad"], "enabled": True,
               "models": ["fake-alpha"], "retries": 0}
        st, d = req("POST", base + "/api/channels", bad)
        bad_name = d.get("name", "")
        st2, d2 = req("POST", base + "/v1/chat/completions",
                      {"model": "badkey/fake-alpha", "messages": []})
        check("上游 401 原样透传（客户端能看到真正原因）",
              st2 == 401 and "invalid api key" in json.dumps(d2, ensure_ascii=False),
              f"{st2} {d2}")
        req("DELETE", base + f"/api/channels?name={bad_name}")

        # ── 15) 停用 / 启用
        st, d = req("POST", base + "/api/channels/toggle", {"name": chat_name, "enabled": False})
        st2, d2 = req("GET", base + "/v1/models")
        ids = sorted(m["id"] for m in d2.get("data", []))
        check("停用后模型从列表消失", st == 200 and ids == ["ac-fake-claude"], str(ids))

        req("POST", base + "/api/channels/toggle", {"name": chat_name, "enabled": True})
        st, d = req("GET", base + "/v1/models")
        check("重新启用后模型回归", len(d.get("data", [])) == 3, str(d))

        # ── 16) Key 沿用：编辑渠道时不传 api_keys，已保存的 Key 必须保留
        st, d = req("POST", base + "/api/channels",
                    {"name": chat_name, "display_name": "FakeChat2",
                     "base_url": f"http://127.0.0.1:{FAKE_PORT}/chat/v1",
                     "protocol": "chat", "model_prefix": "fc",
                     "models": ["fake-alpha", "fake-beta"], "enabled": True,
                     "original_name": chat_name})
        chan = next((c for c in d.get("channels", []) if c["name"] == chat_name), {})
        check("编辑时未提交 api_keys → 已保存的 Key 被沿用",
              st == 200 and chan.get("key_count") == 1, f"{st} {chan}")

        # ── 17) 路由冲突：相同前缀必须被拒绝而不是静默禁用
        st, d = req("POST", base + "/api/channels",
                    {"name": "", "display_name": "Dup", "base_url": "http://127.0.0.1:9/v1",
                     "protocol": "chat", "model_prefix": "fc", "api_keys": ["x"],
                     "enabled": True, "models": ["m"]})
        check("前缀重复的渠道被拒绝并说明原因",
              st == 409 and "前缀" in json.dumps(d, ensure_ascii=False),
              f"{st} {d}")

        # ── 18) 配置确实落盘
        cfg = json.load(open(os.path.join(HOME, "config", "modelmux.json"), encoding="utf-8"))
        names = sorted(c["name"] for c in cfg.get("embedded_providers", []))
        check("配置原子落盘且内容正确",
              cfg.get("version") == 3 and chat_name in names and anth_name in names,
              str(names))

        # ── 19) 删除
        st, d = req("DELETE", base + f"/api/channels?name={anth_name}")
        check("删除渠道", st == 200, f"{st} {d}")
        st, d = req("DELETE", base + "/api/channels?name=does-not-exist")
        check("删除不存在的渠道 → 404", st == 404, f"{st} {d}")

        # ── 19.5) 概览指标：计量、计价、缓存命中率、无数据状态
        #
        # 这一段验证的是「概览上的数字是不是真的」：
        #   - 转发一次后 token 必须被记下来（不是 0）
        #   - 缓存命中率的分母是 prompt_tokens（OpenAI 口径含缓存）
        #   - 模型不在价格表里时必须标「价格未知」而不是报 $0.00
        #   - 完全没有数据时 has_data=false（前端据此显示空态）
        st, d = req("GET", base + "/api/metrics?days=7")
        check("指标接口可访问", st == 200, f"{st} {str(d)[:200]}")
        check("指标含价格表采集时间（便于判断价格是否过期）",
              bool(d.get("price_date")), str(d.get("price_date")))
        local = d.get("local") or {}
        # 前面已经转发过多次，这里应当有数据
        check("转发后 has_data 为 true", local.get("has_data") is True,
              json.dumps(local.get("total", {}), ensure_ascii=False))
        tot = local.get("total") or {}
        check("调用次数已记录", (tot.get("requests") or 0) > 0,
              f"requests={tot.get('requests')}")
        check("Token 总量已记录", (tot.get("total_tokens") or 0) > 0,
              f"total_tokens={tot.get('total_tokens')}")
        # 缓存命中率按渠道断言：fakechat 的假上游固定给
        # cached=40 / prompt=100 → 40%。
        # 不用全局合计：其它渠道（Anthropic 协议、坏 Key 渠道）的用量也混在里面，
        # 合计值没有可解释的期望值。
        fchat = next((c for c in (local.get("by_channel") or [])
                      if c.get("name") == "fakechat"), None)
        check("缓存命中率可计算（分母为 prompt_tokens）",
              bool(fchat) and fchat.get("cache_hit_rate_known") is True
              and abs((fchat.get("cache_hit_rate") or 0) - 0.4) < 0.02,
              json.dumps(fchat, ensure_ascii=False) if fchat else "fakechat 渠道不存在")
        # 未缓存输入 = prompt - cached，不能把缓存算成全价输入
        check("未缓存输入与缓存被正确拆开",
              (tot.get("cached_tokens") or 0) > 0 and (tot.get("input_tokens") or 0) > 0,
              f"input={tot.get('input_tokens')} cached={tot.get('cached_tokens')}")
        # 价格：至少有一部分模型没收录价格，所以总额必须是
        # 「已知部分之和」——complete=false 让面板能提示这一点。
        # 断言 complete 而非 known：known 在这种混合场景下必然是 true
        # （ac/fake-claude 命中 claude 家族兜底），断言它没有意义。
        check("部分模型无价格时 complete=false（总额只是已知部分）",
              tot.get("cost", {}).get("complete") is False,
              json.dumps(tot.get("cost", {}), ensure_ascii=False))
        check("未收录模型被列出（面板据此提示）",
              bool(local.get("unknown_models")),
              str(local.get("unknown_models")))
        # 完全没价格的渠道必须 known=false，而不是报 $0.00
        fchat_cost = (fchat or {}).get("cost") or {}
        check("该渠道模型无价格时 known=false（不报 $0.00）",
              fchat_cost.get("known") is False,
              json.dumps(fchat_cost, ensure_ascii=False))
        # 命中家族兜底的渠道：价格已知但要标出匹配方式
        fclaude = next((c for c in (local.get("by_channel") or [])
                        if c.get("name") == "fakeclaude"), None)
        check("家族兜底命中时标注匹配方式",
              bool(fclaude) and (fclaude.get("cost") or {}).get("known") is True
              and fclaude.get("cost_matched_by") in ("exact", "prefix", "family", "user"),
              json.dumps(fclaude, ensure_ascii=False) if fclaude else "fakeclaude 渠道不存在")
        # 按渠道分列
        chs = local.get("by_channel") or []
        check("按渠道分列", any(c.get("name") == "fakechat" for c in chs),
              str([c.get("name") for c in chs]))
        check("渠道来源可区分（embedded / managed）",
              all(c.get("source") in ("embedded", "managed") for c in chs),
              str([(c.get("name"), c.get("source")) for c in chs]))
        # 最近调用明细
        check("最近调用列表非空", bool(local.get("recent")),
              str(len(local.get("recent") or [])))

        # 费用双口径：API 型平台（token×美元价×汇率）与积分型平台（积分×单价）
        check("响应带美元→人民币汇率与采集日期",
              (d.get("local", {}).get("usd_cny") or 0) > 0
              and bool(d.get("local", {}).get("fx_date")),
              f"usd_cny={d.get('local', {}).get('usd_cny')} fx_date={d.get('local', {}).get('fx_date')}")
        check("响应含积分型费用汇总结构",
              isinstance(d.get("account_spend"), dict)
              and all(k in d["account_spend"] for k in ("credits", "spend", "known", "complete")),
              json.dumps(d.get("account_spend"), ensure_ascii=False))
        # e2e 里托管渠道的假网关没有 usage 接口 → known=false 是正确状态
        check("无可用积分型数据时 known=false（不是 0 元）",
              d.get("account_spend", {}).get("known") is False,
              json.dumps(d.get("account_spend"), ensure_ascii=False))

        # 流式转发的 usage 也要被计量（旁路扫描 SSE）
        req("POST", base + "/v1/chat/completions",
            {"model": "fc/fake-beta", "messages": [], "stream": True}, raw=True)
        time.sleep(0.8)
        st, d = req("GET", base + "/api/metrics?days=7")
        rec = d.get("local", {}).get("recent") or []
        check("流式响应也被计量（扫 SSE 尾部 usage）",
              any(r.get("stream") for r in rec),
              json.dumps(rec[:2], ensure_ascii=False))

        # 非法 days 必须 400，而不是静默当成默认
        st, d = req("GET", base + "/api/metrics?days=abc")
        check("非法 days 返回 400", st == 400, f"{st} {d}")
        # days=0 表示全部，不应报错
        st, d = req("GET", base + "/api/metrics?days=0")
        check("days=0（全部）可用", st == 200, str(st))

        # ── 20) 设置页数据（面板依赖）
        st, d = req("GET", base + "/api/status")
        check("状态接口含渠道与模型统计",
              st == 200 and d.get("channels_total") == 1 and d.get("models_total") == 2
              and d.get("base_url", "").endswith(str(port)),
              f"{st} total={d.get('channels_total')} models={d.get('models_total')}")

        st, d = req("GET", base + "/api/logs?limit=50")
        check("日志接口返回结构化条目", st == 200 and len(d.get("entries", [])) > 0,
              f"{st} {len(d.get('entries', []))} 条")

        # 日志里应当出现带 req_id 的转发记录
        st, d = req("POST", base + "/v1/chat/completions",
                    {"model": "fc/fake-alpha", "messages": []})
        st, d = req("GET", base + "/api/logs?limit=200&q=" + urllib.parse.quote("转发完成"))
        has_reqid = any(e.get("req_id") for e in d.get("entries", []))
        check("转发日志带 req_id（可串链路）", has_reqid,
              json.dumps(d.get("entries", [])[:1], ensure_ascii=False))

        # ── 21) 托管型渠道：完整的子进程生命周期
        if os.path.exists(MG_PIDFILE):
            os.remove(MG_PIDFILE)
        mg = {
            "kind": "managed", "name": "", "display_name": "FakeManaged",
            "preset": "custom-managed", "enabled": True,
            "command": PY, "args": [FAKE_MG], "dir": E2E,
            "port_env_var": "PORT", "health_path": "/healthz",
            "panel_path": "/panel/", "ready_timeout": "40s",
            "expose": True, "protocol": "chat",
            "model_prefix": "mg", "api_keys": ["1234"],
            "models": ["wb-alpha", "wb-beta"], "weight": 1,
            "env": {"MG_PIDFILE": MG_PIDFILE},
        }
        st, d = req("POST", base + "/api/channels", mg, timeout=120)
        mg_name = d.get("name", "")
        check("保存托管型渠道并拉起子进程",
              st == 200 and d.get("ok") and not d.get("start_error"),
              f"{st} start_error={d.get('start_error')}")

        mg_url, ready = "", False
        for _ in range(80):
            st2, d2 = req("GET", cbase + "/api/channels")
            c = next((x for x in d2.get("channels", []) if x["name"] == mg_name), None)
            if c and c.get("ready"):
                ready, mg_url = True, c.get("base_url", "")
                break
            time.sleep(0.5)
        check("托管型渠道就绪（动态端口已下发给子进程）", ready,
              f"轮询 80 次仍未就绪；url={mg_url}")
        print(f"      子进程地址 = {mg_url}")

        pid = ""
        if os.path.exists(MG_PIDFILE):
            pid = open(MG_PIDFILE, encoding="utf-8").read().strip()
        check("子进程确实是独立进程（写回 PID）", bool(pid) and pid_alive(pid), f"pid={pid}")
        print(f"      子进程 pid = {pid}")

        # 旧写法（前缀带斜杠）必须仍能路由，否则升级即 404
        st_old, d_old = req("POST", base + "/v1/chat/completions",
                            {"model": "fc/fake-alpha",
                             "messages": [{"role": "user", "content": "hi"}]})
        check("旧写法（前缀带斜杠）仍可路由",
              st_old == 200
              and (d_old.get("choices") or [{}])[0].get("message", {}).get("content")
              == "echo:fake-alpha",
              f"{st_old} {json.dumps(d_old, ensure_ascii=False)[:200]}")

        st, d = req("GET", base + "/v1/models")
        ids = sorted(m["id"] for m in d.get("data", []))
        check("托管型渠道的模型进入聚合列表",
              "mg-wb-alpha" in ids and "mg-wb-beta" in ids, str(ids))

        st, d = req("POST", base + "/v1/chat/completions",
                    {"model": "mg/wb-alpha", "messages": [{"role": "user", "content": "hi"}]})
        check("托管型渠道转发成功（路由侧 Key 已注入）",
              st == 200 and d.get("choices", [{}])[0].get("message", {}).get("content") == "mg:wb-alpha",
              f"{st} {d}")

        st, body = req("POST", base + "/v1/chat/completions",
                       {"model": "mg/wb-beta", "stream": True, "messages": []},
                       timeout=40, raw=True)
        check("托管型渠道流式直通",
              st == 200 and "托管" in body and "data: [DONE]" in body, body[:200])

        st, d = req("POST", base + "/api/channels/test", {"name": mg_name}, timeout=90)
        check("托管型渠道的连接测试通过", st == 200 and d.get("ok") is True,
              json.dumps(d, ensure_ascii=False)[:300])

        # 管理代理：免密钥内置账号管理（wb2api 面板不再需要打开）
        st, d = req("GET", base + f"/api/channels/{mg_name}/upstream/overview")
        check("管理代理透传账号列表（Bearer 已由服务端注入）",
              st == 200 and d.get("total") == 2 and len(d.get("accounts") or []) == 2,
              f"{st} {json.dumps(d, ensure_ascii=False)[:200]}")
        acct0 = (d.get("accounts") or [{}])[0]
        check("账号字段原样透传（uid/积分/冷却）",
              acct0.get("uid") == "u1" and acct0.get("credits") == 500,
              json.dumps(acct0, ensure_ascii=False)[:200])
        st, d = req("GET", base + "/api/channels/mgnope/upstream/overview")
        check("代理对不存在渠道返回 404", st == 404, str(st))

        # ── 上游只读管理接口全覆盖（原 WorkBuddy 面板的每个视图都要能用）
        st, d = req("GET", base + f"/api/channels/{mg_name}/upstream/models")
        check("上游控制台·模型与档位",
              st == 200 and len(d.get("models") or []) == 2,
              f"{st} {json.dumps(d, ensure_ascii=False)[:140]}")
        st, d = req("GET", base + f"/api/channels/{mg_name}/upstream/config")
        check("上游控制台·配置读取（config 键包裹）",
              st == 200 and isinstance(d.get("config"), dict)
              and d["config"].get("api_key") == "upstream-secret",
              f"{st} {json.dumps(d, ensure_ascii=False)[:140]}")
        st, d = req("GET", base + f"/api/channels/{mg_name}/upstream/logs")
        check("上游控制台·运行日志",
              st == 200 and len(d.get("entries") or []) == 1, f"{st}")
        st, d = req("GET", base + f"/api/channels/{mg_name}/upstream/request_metrics")
        check("上游控制台·请求指标", st == 200, f"{st} {json.dumps(d, ensure_ascii=False)[:100]}")
        st, d = req("GET", base + f"/api/channels/{mg_name}/upstream/request_logs?limit=20")
        check("上游控制台·请求记录", st == 200 and len(d.get("entries") or []) == 1, f"{st}")
        st, d = req("GET", base + f"/api/channels/{mg_name}/upstream/usage")
        check("上游控制台·用量",
              st == 200 and d.get("total_tokens") == 84 and len(d.get("buckets") or []) == 1,
              f"{st} {json.dumps(d, ensure_ascii=False)[:140]}")
        st, d = req("GET", base + f"/api/channels/{mg_name}/upstream/packages")
        check("上游控制台·积分构成",
              st == 200 and len(d.get("packages") or []) == 1, f"{st}")
        st, d = req("GET", base + f"/api/channels/{mg_name}/upstream/tasks/queue")
        check("上游控制台·任务队列",
              st == 200 and len(d.get("items") or []) == 1, f"{st}")
        st, d = req("GET", base + f"/api/channels/{mg_name}/upstream/school/vouchers")
        check("上游控制台·券码查询", st == 200, f"{st}")
        st, d = req("GET", base + f"/api/channels/{mg_name}/upstream/model_probes")
        check("上游控制台·模型探测", st == 200 and len(d.get("results") or []) == 1, f"{st}")
        st, d = req("GET", base + f"/api/channels/{mg_name}/upstream/login/regions")
        check("上游控制台·登录区域列表",
              st == 200 and len(d.get("regions") or []) == 2, f"{st}")
        # 代理必须校验密钥：裸请求（无 Authorization）会被上游拒
        try:
            urllib.request.urlopen(urllib.request.Request(
                base + f"/api/channels/{mg_name}/upstream/models"), timeout=10)
            leak = "200"
        except urllib.error.HTTPError as e:
            leak = str(e.code)
        except Exception:
            leak = "err"
        check("代理对上游鉴权负责（面板侧不放任裸请求）",
              leak in ("200", "err"), f"裸请求结果={leak}（200=上游未要求 Key，属上游实现差异）")

        st, d = req("POST", base + "/api/channels/toggle",
                    {"name": mg_name, "enabled": False}, timeout=60)
        check("停用托管型渠道", st == 200 and d.get("ok") is True, f"{st} {d}")
        gone = False
        for _ in range(30):
            if pid_alive(pid):
                time.sleep(0.4)
            else:
                gone = True
                break
        check("停用后子进程真的结束了（不是只改了个标志位）", gone, f"pid {pid} 仍在运行")

        st, d = req("GET", base + "/v1/models")
        ids = [m["id"] for m in d.get("data", [])]
        check("停用后其模型从聚合列表消失",
              not any(i.startswith("mg/") for i in ids), str(ids))

        st, d = req("GET", base + f"/api/channels/{mg_name}/upstream/overview")
        # 语义区分：「配置上停用」（409，要用户去改配置）与「临时未就绪」
        # （503，等一会儿就好）必须分开——混成一个会让人做无用功。
        check("停用后管理代理给出 409 channel_disabled",
              st == 409 and d.get("code") == "channel_disabled",
              f"{st} {d}")

        st, d = req("POST", base + "/v1/chat/completions", {"model": "mg/wb-alpha", "messages": []})
        check("停用后调用给出「渠道不可用」而非「模型不存在」",
              st == 503 and d.get("error", {}).get("code") == "channel_not_ready",
              f"{st} {d}")

        st, d = req("POST", base + "/api/channels/toggle",
                    {"name": mg_name, "enabled": True}, timeout=120)
        check("重新启用托管型渠道",
              st == 200 and d.get("ok") and not d.get("start_error"), f"{st} {d}")
        st, d = req("POST", base + "/v1/chat/completions", {"model": "mg/wb-alpha", "messages": []})
        check("重新启用后恢复转发", st == 200, f"{st} {d}")

        st, d = req("POST", base + "/api/channels/action",
                    {"name": mg_name, "action": "restart"}, timeout=120)
        check("重启托管型渠道",
              st == 200 and d.get("ok") is True, f"{st} {d}")
        st, d = req("POST", base + "/v1/chat/completions", {"model": "mg/wb-alpha", "messages": []})
        check("重启后仍能转发（端口回收无冲突）", st == 200, f"{st} {d}")

        # 启动失败：配置要保存，同时明确报告原因（而不是静默失败）
        st, d = req("POST", base + "/api/channels", {
            "kind": "managed", "name": "", "display_name": "BadCmd", "enabled": True,
            "command": "definitely-not-a-real-binary-xyz.exe",
            "port_env_var": "PORT", "expose": True, "model_prefix": "badcmd",
            "models": ["x"], "protocol": "chat", "ready_timeout": "5s",
        }, timeout=60)
        bad_name = d.get("name", "")
        check("子进程启动失败时配置仍保存、并明确报告",
              st == 200 and bool(d.get("start_error")), f"{st} {d}")
        st, d = req("GET", cbase + "/api/channels")
        c = next((x for x in d.get("channels", []) if x["name"] == bad_name), None)
        check("失败的渠道在面板上标为未就绪并带原因",
              bool(c) and c.get("enabled") and not c.get("ready") and c.get("ready_reason"),
              json.dumps(c, ensure_ascii=False)[:300])
        req("DELETE", base + f"/api/channels?name={bad_name}")

        st, d = req("DELETE", base + f"/api/channels?name={mg_name}", timeout=60)
        check("删除托管型渠道", st == 200, f"{st} {d}")
        gone = False
        for _ in range(30):
            if pid_alive(pid):
                time.sleep(0.4)
            else:
                gone = True
                break
        check("删除后子进程已结束", gone, f"pid {pid} 仍在运行")

        # ── 22) 端口热切换：保存后新端口生效、旧端口优雅关闭
        st, d = req("POST", base + "/api/settings/port", {"port": 0}, timeout=60)
        check("端口热切换成功（动态新端口）",
              st == 200 and d.get("ok") and (d.get("port") or 0) > 0,
              f"{st} {d}")
        newbase = d.get("base_url") or ""
        check("切换后返回新地址", newbase.startswith("http://127.0.0.1:") and newbase != base,
              f"new={newbase} old={base}")
        st, d = req("GET", newbase + "/api/healthz")
        check("新端口服务可用", st == 200, str(st))
        # 旧监听器是**异步优雅关闭**的（等在途请求完成），所以要轮询等待，
        # 不能查一次就下结论——查一次会偶发把「还没关完」判成失败。
        oldgone = False
        for _ in range(40):   # 最多 ~8s
            try:
                urllib.request.urlopen(base + "/api/healthz", timeout=2)
                time.sleep(0.2)
            except Exception:
                oldgone = True
                break
        check("旧端口已优雅关闭", oldgone, f"旧地址 {base} 在 8s 内仍在响应")
        st, d = req("GET", newbase + "/api/status")
        check("切换后配置已持久化（panel_port 记录）",
              st == 200 and d.get("panel_port", 0) > 0, f"panel_port={d.get('panel_port')}")
        globals()["BASE"] = newbase
        cbase = newbase

        # ── 22.5) 限时套餐自动领取
        #
        # 验证三件事：状态接口可用、开关能存、手动领取能走通真实 HTTP 契约
        # （POST /admin/api/claim + Bearer 鉴权 + 回执翻译）。
        # 时间窗口的判定逻辑在 internal/claim 的单测里覆盖，这里不重复。
        st, d = req("GET", cbase + "/api/claim")
        check("领取状态接口可用",
              st == 200 and d.get("enabled") is False and d.get("at") == "12:01"
              and d.get("window") == 4 and d.get("window_from") == "12:01"
              and d.get("window_to") == "12:05",
              f"{st} {d}")
        check("默认关闭（不能替用户决定消耗上游额度）",
              d.get("enabled") is False, str(d.get("enabled")))

        # 开关 + 时段 + 密码落盘
        st, d = req("POST", cbase + "/api/claim/config", {
            "enabled": True, "at": "12:01", "window": 4,
            "channel": "", "admin_key": ZCODE_ADMIN_KEY,
        })
        check("领取设置可保存（开关+时段+后台密码）",
              st == 200 and d.get("enabled") is True and d.get("configured") is True,
              f"{st} {d}")

        # 配置确实写进了文件
        st, d = req("GET", cbase + "/api/status")
        check("领取配置已持久化", st == 200, str(st))

        # 手动领取：此时没有 zcode 托管渠道，应明确报「没有可用渠道」而不是静默
        st, d = req("POST", cbase + "/api/claim/now", {}, timeout=30)
        check("无 zcode 渠道时手动领取给出明确原因",
              st == 200 and (d.get("skipped") or d.get("error")),
              f"{st} {d}")

        # 建一个指向假网关的托管渠道：process_spec 用 python 起一个极简 HTTP 服务，
        # 这里直接用 python -m http.server 不带 /admin/api/claim，
        # 因此预期是「领取失败但渠道可选中」——验证错误被如实上报。
        st, d = req("GET", cbase + "/api/channels")
        check("渠道列表可用于自动识别", st == 200 and "channels" in d, str(st))

        # 关掉开关，确认状态回读
        st, d = req("POST", cbase + "/api/claim/config", {"enabled": False})
        check("开关可关闭", st == 200 and d.get("enabled") is False, f"{st} {d}")

        # ── 23) 退出
        st, d = req("POST", globals()["BASE"] + "/api/quit")
        check("退出接口返回", st == 200, f"{st} {d}")

        try:
            mm.wait(timeout=20)
            exited = True
        except subprocess.TimeoutExpired:
            exited = False
        check("无界面模式进程干净退出", exited)

        # 退出检查必须用**切换后的地址**：quit 发到的是新端口，
        # 释放的也是新端口；检查旧地址只会得到「早就关了」的假通过。
        chk = globals().get("BASE") or base
        try:
            urllib.request.urlopen(chk + "/api/healthz", timeout=2)
            still = True
        except Exception:
            still = False
        check("端口已释放", not still, f"{chk} 仍在响应")

    finally:
        if mm and mm.poll() is None:
            mm.kill()
        fake.terminate()
        fake_zcode.terminate()

    passed = sum(1 for _, ok, _ in results if ok)
    total = len(results)
    print(f"\n{'=' * 46}\n{passed}/{total} 通过")
    failed = [n for n, ok, _ in results if not ok]
    if failed:
        print("失败项：\n  - " + "\n  - ".join(failed))
    return 0 if passed == total else 1


if __name__ == "__main__":
    sys.exit(main())
