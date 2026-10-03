#!/usr/bin/env python3
"""假 zcode2api 网关：复刻 dengyie/zcode2api 的领取接口契约。

只实现本项目要验证的那几个端点，形状严格对齐上游：

    GET  /meta                     探活
    GET  /v1/models                模型列表
    POST /admin/api/claim          领取（Bearer 鉴权）
    GET  /admin/api/accounts       账号池（面板要显示候选渠道时用不到，留着更真实）

领取回执的字段名与上游一致（account_id/account_name/ok/plan_name/message/next_at，
summary.ok/summary.fail），1005「名额用完」时带 next_at —— ModelMux 要原样
透传给用户看，所以这个字段必须仿真。

用法：python fake_zcode.py [port] [--fail-code 1005] [--no-auth]
若设置了环境变量 ZCODE_PORT 则以它为准（托管场景）。
"""
import json
import os
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = 18101
FAIL_CODE = 0          # >0 时模拟「名额用完」
NO_AUTH = False
CLAIM_HITS = []        # 记录领取请求，供测试断言调用次数

TOKEN = "fake-admin-key-123"


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

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

    def do_GET(self):
        if self.path == "/meta":
            return self._json(200, {"ok": True, "version": "2.6.0", "uptime_ms": 1234})
        if self.path == "/v1/models":
            if not self._authorized():
                return self._json(401, {"error": "invalid key"})
            return self._json(200, {"object": "list", "data": [
                {"id": "glm-4.6", "object": "model"},
                {"id": "glm-5.3-flash", "object": "model"},
            ]})
        if self.path == "/admin/api/accounts":
            if not self._authorized():
                return self._json(401, {"error": "invalid admin key"})
            return self._json(200, {"accounts": [
                {"id": "acc-1", "name": "账号甲", "status": "ACTIVE"},
                {"id": "acc-2", "name": "账号乙", "status": "ACTIVE"},
            ]})
        return self._json(404, {"error": "not found"})

    def do_POST(self):
        if self.path != "/admin/api/claim":
            return self._json(404, {"error": "not found"})
        n = int(self.headers.get("Content-Length") or 0)
        if n:
            self.rfile.read(n)
        # 鉴权失败要给出可区分的状态码：ModelMux 靠它提示「后台密码不对」
        if not self._authorized():
            return self._json(401, {"error": "invalid admin key"})
        CLAIM_HITS.append(time.time())
        if FAIL_CODE == 1005:
            # 名额用完：上游会带 next_at（名额恢复时间）
            return self._json(200, {
                "outcomes": [
                    {"account_id": "acc-1", "account_name": "账号甲", "ok": False,
                     "code": 1005, "message": "今日名额已用完",
                     "next_at": "2026-10-03 12:01:00"},
                    {"account_id": "acc-2", "account_name": "账号乙", "ok": False,
                     "code": 1005, "message": "今日名额已用完",
                     "next_at": "2026-10-03 12:01:00"},
                ],
                "summary": {"ok": 0, "fail": 2},
            })
        return self._json(200, {
            "outcomes": [
                {"account_id": "acc-1", "account_name": "账号甲", "ok": True,
                 "plan_name": "限时免费套餐", "grants": [{"quota": 100}]},
                {"account_id": "acc-2", "account_name": "账号乙", "ok": True,
                 "plan_name": "限时免费套餐"},
            ],
            "summary": {"ok": 2, "fail": 0},
        })


def main():
    global PORT, FAIL_CODE, NO_AUTH
    # 端口优先取环境变量 ZCODE_PORT —— 这正是 zcode2api 自己的约定，
    # 也是 ModelMux 托管时注入的那个变量。命令行参数只用于手动起（自测）。
    env_port = os.environ.get("ZCODE_PORT", "").strip()
    if env_port:
        PORT = int(env_port)
    args = sys.argv[1:]
    if args and not env_port:
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
