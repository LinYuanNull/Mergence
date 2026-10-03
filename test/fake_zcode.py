#!/usr/bin/env python3
"""假 zcode2api 网关：复刻 dengyie/zcode2api 的管理接口契约。

与原版（只读探活 + 领取）相比，这里补齐了「账号面板整块搬进 ModelMux」
所需要的那批写接口，并在内存里维护账号池——写操作必须能被随后的
GET /admin/api/accounts 读回，否则「点保存返回 200 但列表没变」这类 bug
在端到端里根本看不出来。

已实现（前缀 /admin/api，除 /meta 与 /v1/models 外都带 Bearer 鉴权）：

    GET    /meta                      探活（健康检查，不带鉴权）
    GET    /v1/models                 模型列表
    GET    /status                    额度池 / 提供方 / 网关密钥态
    GET    /accounts                  账号池（含 stats / providers）
    POST   /accounts                  新增（body: {provider, tokens, name?}）
    PUT    /accounts/{id}             改名 / 换 token（body: {name?, token?}）
    DELETE /accounts                  删除（body 是 **JSON 数组**，要删的 id）
    POST   /accounts/{id}/enabled     启停（body: {enabled}）
    POST   /accounts/{id}/refresh     单账号刷新额度
    POST   /accounts/refresh          全量刷新（body: {all}）
    POST   /accounts/{id}/fingerprint/rotate  换发设备指纹
    GET    /export                    导出（JSON 响应，不是文件流）
    POST   /import                    导入（JSON body，不是 multipart）
    GET    /claim/preview             预览可领套餐
    GET    /claim/captcha-config      验证码配置（UI 目前不调，留着更真实）
    POST   /claim                     领取（支持 account_ids 过滤）
    POST   /login/start               发起设备码登录
    GET    /login/poll/{flow_id}      轮询登录态（未知 flow 返回 expired）

几个必须仿真的契约细节（照抄上游 accounts.html 会踩的坑）：
  * /import 与 /export 都是 JSON body/响应，不是 multipart；
  * DELETE /accounts 收 JSON 数组 body，不是 query——退化成无 body 会
    「安静地一个都不删」（deleted:0），而不是报错；
  * GET /login/poll 对未知 flow 返回 status=expired，不是 404；
  * 领取回执的字段名是 account_id/account_name/ok/plan_name/message/next_at，
    1005「名额用完」时带 next_at——ModelMux 要原样透传给用户看。

用法：python fake_zcode.py [port] [--fail-code 1005] [--no-auth]
若设置了环境变量 ZCODE_PORT 则以它为准（托管场景，ModelMux 注入的）。
"""
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

PORT = 18101
FAIL_CODE = 0          # >0 时模拟「名额用完」
NO_AUTH = False
CLAIM_HITS = []        # 记录领取请求，供测试断言调用次数
LOGIN_HITS = []        # 记录登录轮询，供测试断言轮询真的在跑

TOKEN = "fake-admin-key-123"


def _set_admin_key(key):
    """改后台密码。

    真实网关改完立刻就按新值鉴权，这里必须一致——否则面板改完密码之后，
    后面所有请求仍按旧值放行，「两处同步」这个不变式在测试里就是空的。
    """
    global TOKEN
    TOKEN = key


# ── 内存设置 / 请求监控 ──────────────────────────────────
# 设置也要有状态：面板里「改设置 → 读回新值」是一条完整链路。假上游若永远回
# 同一份硬编码值，PUT 被吞掉（或返回 404 被静默忽略）都测不出来。
SETTINGS = {
    "gateway_key": "sk-fake-gateway-9f2c",
    "quota_refresh_interval": 300,
    "account_concurrency": 4,
    "claim_round_interval": 86400,
}
SETTINGS_WRITES = []   # 收到的 PUT /settings body，供断言「请求真的发过来了」
ADMIN_KEY_WRITES = []  # 收到的后台密码变更，供断言同步确实发生

# 请求监控：内存环形日志，POST /monitoring/clear 会清空它。
MON_ENTRIES = [
    {"ts": time.time(), "account": "账号甲", "mode": "jwt",
     "model": "glm-4.6", "endpoint": "/v1/chat/completions", "stream": True,
     "ok": True, "status": 200, "error": "",
     "t_first": 0.42, "t_total": 1.85, "input_tokens": 1200,
     "output_tokens": 640, "preview": "你好，我是…"},
    {"ts": time.time() - 12, "account": "账号乙", "mode": "jwt",
     "model": "glm-5.3-flash", "endpoint": "/v1/chat/completions", "stream": False,
     "ok": False, "status": 429, "error": "rate limited",
     "t_first": None, "t_total": 0.31, "input_tokens": 300,
     "output_tokens": 0, "preview": ""},
]

# ── 内存账号池 ────────────────────────────────────────────
# 种子与旧版保持一致（账号甲 / 账号乙），这样既有的 verify_claim.py 不受影响，
# 也顺带验证「两个 JWT 账号 → 领取回执 2 条」。真实网关的账号是有状态的，
# 假网关若每次返回同一份硬编码列表，就等于把「写操作有没有生效」这件事测掉了。
_LOCK = threading.Lock()
_NEXT_ID = [1]  # 种子账号占 1、2；新账号从 3 起，与旧版硬编码的 acc-1/acc-2 对齐
DAY = 86400.0


def _new_id():
    i = _NEXT_ID[0]
    _NEXT_ID[0] += 1
    return f"acc-{i}"


def _mask(tok):
    """复刻上游 _mask_secret：长度 > 8 给 `前4…后4`，否则整体打点。"""
    if not tok:
        return ""
    return f"{tok[:4]}…{tok[-4:]}" if len(tok) > 8 else "••••"


def _mode_of(tok):
    """JWT（三段点分）走 Coding Plan 通道；其余按 API Key 处理。"""
    return "jwt" if tok.count(".") == 2 else "key"


def _mk_account(name, provider, token, *, enabled=True, status=None, quota=None,
                plans=None, last_error=""):
    mode = _mode_of(token)
    return {
        "id": _new_id(),
        "name": name,
        "provider": provider,
        "mode": mode,
        "status": status or ("active" if enabled else "disabled"),
        "enabled": enabled,
        "token": token,                 # 假网关内部保存明文，供导出用
        "token_masked": _mask(token),
        "quota": quota or {},
        "plans": plans or [],
        "last_error": last_error,
    }


def _seed():
    now = time.time()
    return [
        _mk_account("账号甲", "zai", "eyJhbGciOiJI.eyJzdWIiOiJhIn0.sigaaa",
                    quota={"coding_plan": {"total": 1000000, "used": 250000,
                                           "remaining": 750000, "expires_at": now + 30 * DAY}},
                    plans=[{"name": "GLM Coding Plan", "ends_at": now + 30 * DAY}]),
        _mk_account("账号乙", "zai", "eyJhbGciOiJI.eyJzdWIiOiJiIn0.sigbbb",
                    quota={"coding_plan": {"total": 1000000, "used": 900000,
                                           "remaining": 100000, "expires_at": now + 5 * DAY}},
                    plans=[{"name": "GLM Coding Plan", "ends_at": now + 5 * DAY}]),
    ]


ACCOUNTS = _seed()

# 设备码登录：flow_id -> {"tries": n, "label": str}
LOGIN_FLOWS = {}
LOGIN_READY_AFTER = 2   # 轮询 2 次后变 ready（让 UI 真的轮询几轮才成功）


def _by_id(aid):
    return next((a for a in ACCOUNTS if a["id"] == aid), None)


def _public(a):
    """对外呈现的账号对象：不吐明文 token（与上游一致，只给掩码）。"""
    return {k: v for k, v in a.items() if k != "token"}


def _stats():
    st = {"total": len(ACCOUNTS), "active": 0, "exhausted": 0,
          "cooling": 0, "invalid": 0, "disabled": 0}
    for a in ACCOUNTS:
        key = a.get("status") or "active"
        if key not in st:
            key = "active"
        st[key] += 1
    return st


def _providers():
    seen = []
    for a in ACCOUNTS:
        p = a.get("provider")
        if p and p not in seen:
            seen.append(p)
    return seen or ["zai"]


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    # ── 基础工具 ──────────────────────────────────────────
    def _json(self, code, obj):
        body = json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _authorized(self):
        if NO_AUTH:
            return True
        return self.headers.get("Authorization", "") == "Bearer " + TOKEN

    def _read_body(self):
        n = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(n) if n else b""

    def _read_json(self, default=None):
        raw = self._read_body()
        if not raw:
            return default if default is not None else {}
        try:
            return json.loads(raw)
        except Exception:
            return default if default is not None else {}

    def _seg(self):
        """把 /admin/api/... 之后的路径切成段（忽略 query）。"""
        path = urlsplit(self.path).path
        if not path.startswith("/admin/api/"):
            return None
        return [s for s in path[len("/admin/api/"):].strip("/").split("/") if s != ""]

    def _deny(self):
        self._json(401, {"error": "invalid admin key"})

    # ── GET ───────────────────────────────────────────────
    def do_GET(self):
        path = urlsplit(self.path).path
        if path == "/meta":
            return self._json(200, {"ok": True, "version": "2.6.0", "uptime_ms": 1234})
        if path == "/v1/models":
            if not self._authorized():
                return self._json(401, {"error": "invalid key"})
            return self._json(200, {"object": "list", "data": [
                {"id": "glm-4.6", "object": "model"},
                {"id": "glm-5.3-flash", "object": "model"},
            ]})

        seg = self._seg()
        if seg is None:
            return self._json(404, {"error": "not found"})
        if not self._authorized():
            return self._deny()

        if seg == ["status"]:
            return self._json(200, {
                "ok": True, "version": "2.6.0",
                "providers": _providers(),
                "quota_pool": {"zai": len(ACCOUNTS)},
                "gateway_key_set": True,
            })
        if seg == ["accounts"]:
            return self._json(200, {
                "accounts": [_public(a) for a in ACCOUNTS],
                "stats": _stats(),
                "providers": _providers(),
            })
        if seg == ["monitoring"]:
            # 回 MON_ENTRIES 本体：清空之后 GET 必须真的变空
            return self._json(200, {"keep": 200,
                                    "entries": [dict(e) for e in MON_ENTRIES]})
        if seg == ["settings"]:
            gw = SETTINGS["gateway_key"]
            return self._json(200, {
                "admin_key_set": bool(TOKEN), "admin_key_masked": _mask(TOKEN),
                "admin_key_is_default": False,
                "gateway_key_set": bool(gw), "gateway_key_masked": _mask(gw),
                "quota_refresh_interval": SETTINGS["quota_refresh_interval"],
                "account_concurrency": SETTINGS["account_concurrency"],
                "claim_round_interval": SETTINGS["claim_round_interval"],
            })
        if seg == ["export"]:
            # 导出是 JSON 响应（不是文件流）；按提供方分组，值是 token 列表
            prov = {}
            for a in ACCOUNTS:
                prov.setdefault(a["provider"], []).append(a["token"])
            return self._json(200, {"version": 1, "providers": prov,
                                    "exported_at": int(time.time())})
        if seg == ["claim", "preview"]:
            rows = []
            for a in ACCOUNTS:
                if a["mode"] != "jwt" or a["status"] != "active":
                    continue
                rows.append({
                    "account_id": a["id"], "account_name": a["name"],
                    "plans": [{"name": "限时免费套餐", "grants": 100}],
                    "activated": True,
                })
            return self._json(200, {"preview": rows})
        if seg == ["claim", "captcha-config"]:
            return self._json(200, {"enabled": False, "provider": "none"})
        if len(seg) == 3 and seg[0] == "login" and seg[1] == "poll":
            flow = LOGIN_FLOWS.get(seg[2])
            if flow is None:
                # 未知 flow 是 expired，不是 404（UI 靠这个收尾）
                return self._json(200, {"status": "expired"})
            # 轮询计数与「入池」都在**发响应之前**做：UI 拿到 ready 后马上会
            # 重新拉账号列表，若这时账号还没入池，断言会看到一个恰好慢半拍的
            # 空列表——这是测试自己造出来的假失败。
            flow["tries"] += 1
            LOGIN_HITS.append(seg[2])
            if flow["tries"] > LOGIN_READY_AFTER:
                label = flow["label"]
                with _LOCK:
                    if not any(a["name"] == label for a in ACCOUNTS):
                        ACCOUNTS.append(_mk_account(
                            label, "zai", "eyJhbGciOiJI.eyJzdWIiOiJvIn0.sigooo"))
                return self._json(200, {"status": "ready", "account": {"name": label}})
            return self._json(200, {"status": "waiting", "message": "等待授权"})
        return self._json(404, {"error": "not found"})

    # ── POST ──────────────────────────────────────────────
    def do_POST(self):
        seg = self._seg()
        if seg is None:
            return self._json(404, {"error": "not found"})
        body = self._read_json({})
        if not self._authorized():
            return self._deny()

        if seg == ["claim"]:
            CLAIM_HITS.append(time.time())
            wanted = set(body.get("account_ids") or [])
            outcomes = []
            for a in ACCOUNTS:
                if wanted and a["id"] not in wanted:
                    continue
                if FAIL_CODE == 1005:
                    # 名额用完：上游会带 next_at（名额恢复时间）
                    outcomes.append({
                        "account_id": a["id"], "account_name": a["name"], "ok": False,
                        "code": 1005, "message": "今日名额已用完",
                        "next_at": "2026-10-03 12:01:00"})
                else:
                    outcomes.append({
                        "account_id": a["id"], "account_name": a["name"], "ok": True,
                        "plan_name": "限时免费套餐", "grants": 100})
            ok = sum(1 for o in outcomes if o["ok"])
            return self._json(200, {"outcomes": outcomes,
                                    "summary": {"ok": ok, "fail": len(outcomes) - ok}})

        if seg == ["accounts"]:
            provider = (body.get("provider") or "").strip()
            tokens = [t for t in (body.get("tokens") or []) if str(t).strip()]
            name = (body.get("name") or "").strip()
            if not provider or not tokens:
                return self._json(400, {"error": "provider 与 tokens 必填"})
            now = time.time()
            with _LOCK:
                for i, t in enumerate(tokens):
                    if any(a["token"] == t for a in ACCOUNTS):
                        continue  # 重复 token 上游自动跳过
                    nm = name if (name and len(tokens) == 1) else f"{provider}-{_NEXT_ID[0]}"
                    ACCOUNTS.append(_mk_account(
                        nm, provider, t,
                        quota={"coding_plan": {"total": 1000000, "used": 0,
                                               "remaining": 1000000,
                                               "expires_at": now + 30 * DAY}},
                        plans=[{"name": "GLM Coding Plan", "ends_at": now + 30 * DAY}]))
                count = len(tokens)
            return self._json(200, {"ok": True, "count": count})

        if seg == ["accounts", "refresh"]:
            live = [a for a in ACCOUNTS if a["status"] == "active"]
            return self._json(200, {
                "ok": True, "count": len(live),
                "skipped_cooling": sum(1 for a in ACCOUNTS if a["status"] == "cooling"),
                "skipped_invalid": sum(1 for a in ACCOUNTS if a["status"] == "invalid"),
            })

        if len(seg) == 3 and seg[0] == "accounts" and seg[2] == "enabled":
            a = _by_id(seg[1])
            if not a:
                return self._json(404, {"error": "账号不存在"})
            on = bool(body.get("enabled"))
            a["enabled"] = on
            a["status"] = "active" if on else "disabled"
            return self._json(200, {"ok": True, "enabled": on})

        if len(seg) == 3 and seg[0] == "accounts" and seg[2] == "refresh":
            a = _by_id(seg[1])
            if not a:
                return self._json(404, {"error": "账号不存在"})
            if a["mode"] != "jwt":
                # 上游对非 JWT 账号返回 ok:false + 文案（不是 HTTP 错误）
                return self._json(200, {"ok": False,
                                        "message": "该账号是 API Key 模式，不支持额度刷新"})
            now = time.time()
            a["quota"] = {"coding_plan": {"total": 1000000, "used": a["quota"]
                                          .get("coding_plan", {}).get("used", 0),
                                          "remaining": 1000000 - a["quota"]
                                          .get("coding_plan", {}).get("used", 0),
                                          "expires_at": now + 30 * DAY}}
            return self._json(200, {"ok": True})

        if len(seg) == 4 and seg[0] == "accounts" and seg[2] == "fingerprint" \
                and seg[3] == "rotate":
            a = _by_id(seg[1])
            if not a:
                return self._json(404, {"error": "账号不存在"})
            return self._json(200, {"ok": True, "fingerprint": {
                "platform": "windows", "device_mid": "fp-%08x" % (int(time.time() * 1000) & 0xFFFFFFFF)}})

        if seg == ["import"]:
            prov = body.get("providers") or {}
            count = 0
            now = time.time()
            with _LOCK:
                for p, toks in prov.items():
                    for t in toks or []:
                        if any(a["token"] == t for a in ACCOUNTS):
                            continue
                        ACCOUNTS.append(_mk_account(
                            f"{p}-{_NEXT_ID[0]}", p, t,
                            quota={"coding_plan": {"total": 1000000, "used": 0,
                                                   "remaining": 1000000,
                                                   "expires_at": now + 30 * DAY}}))
                        count += 1
            return self._json(200, {"ok": True, "count": count})

        if seg == ["login", "start"]:
            label = (body.get("label") or "").strip() or "OAuth 账号"
            flow = "flow-%d" % int(time.time() * 1000)
            LOGIN_FLOWS[flow] = {"tries": 0, "label": label}
            return self._json(200, {
                "flow_id": flow,
                "authorize_url": "https://example.invalid/oauth/device?code=FAKE-CODE",
                "expires_in": 300,
            })

        if seg == ["monitoring", "clear"]:
            # 内存环形日志：清空后 GET /monitoring 必须真的回空列表
            with _LOCK:
                MON_ENTRIES.clear()
            return self._json(200, {"ok": True})

        return self._json(404, {"error": "not found"})

    # ── PUT ───────────────────────────────────────────────
    def do_PUT(self):
        seg = self._seg()
        if seg is None:
            return self._json(404, {"error": "not found"})
        body = self._read_json({})
        if not self._authorized():
            return self._deny()
        if len(seg) == 2 and seg[0] == "accounts":
            a = _by_id(seg[1])
            if not a:
                return self._json(404, {"error": "账号不存在"})
            if "name" in body and str(body["name"]).strip():
                a["name"] = str(body["name"]).strip()
            if "token" in body and str(body["token"]).strip():
                a["token"] = str(body["token"]).strip()
                a["token_masked"] = _mask(a["token"])
                a["mode"] = _mode_of(a["token"])
            return self._json(200, {"ok": True, "account": _public(a)})
        if seg == ["settings"]:
            SETTINGS_WRITES.append(dict(body))
            if "admin_key" in body:
                key = str(body.get("admin_key") or "").strip()
                if not key:
                    return self._json(400, {"error": "后台密钥不能为空"})
                if "…" not in key and key != "••••":
                    ADMIN_KEY_WRITES.append(key)
                    _set_admin_key(key)
            if "gateway_key" in body:
                k = str(body.get("gateway_key") or "").strip()
                if k and "…" not in k:
                    SETTINGS["gateway_key"] = k
            for f in ("quota_refresh_interval", "account_concurrency", "claim_round_interval"):
                if f in body:
                    try:
                        SETTINGS[f] = max(0, int(body[f]))
                    except (TypeError, ValueError):
                        return self._json(400, {"error": f + " 必须是非负整数"})
            return self._json(200, {"ok": True})
        return self._json(404, {"error": "not found"})

    # ── DELETE ────────────────────────────────────────────
    def do_DELETE(self):
        seg = self._seg()
        if seg is None:
            return self._json(404, {"error": "not found"})
        body = self._read_json([])
        if not self._authorized():
            return self._deny()
        if seg == ["accounts"]:
            # body 是 JSON 数组（要删的 id 列表），不是 query。
            # 无 body 时按空列表处理 → deleted:0，而不是报错。
            ids = body if isinstance(body, list) else []
            n = 0
            with _LOCK:
                for aid in ids:
                    a = _by_id(aid)
                    if a:
                        ACCOUNTS.remove(a)
                        n += 1
            return self._json(200, {"ok": True, "deleted": n})
        return self._json(404, {"error": "not found"})


def main():
    global PORT, FAIL_CODE, NO_AUTH
    # 端口优先取环境变量 ZCODE_PORT——这正是 zcode2api 自己的约定，
    # 也是 ModelMux 托管时注入的那个变量。命令行参数只用于手动起（自测）。
    env_port = os.environ.get("ZCODE_PORT", "").strip()
    if env_port:
        PORT = int(env_port)
    args = sys.argv[1:]
    if args and not env_port and not args[0].startswith("-"):
        PORT = int(args[0])
    if "--fail-code" in args:
        FAIL_CODE = int(args[args.index("--fail-code") + 1])
    if "--no-auth" in args:
        NO_AUTH = True
    srv = ThreadingHTTPServer(("127.0.0.1", PORT), H)
    print(f"fake zcode2api on {PORT} fail_code={FAIL_CODE} no_auth={NO_AUTH}", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    main()
