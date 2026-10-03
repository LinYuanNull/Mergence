#!/usr/bin/env python3
"""假托管上游：模拟一个「独立进程型的 2api 网关」。

用来验证 ModelMux 的托管型渠道链路，不依赖任何真实上游：
  - 端口从环境变量 PORT 取（验证 PortEnvVar 下发能否生效）
  - /healthz 供就绪探测
  - /models 与 /chat/completions 需要 Bearer 1234（验证路由侧 Key 注入）
  - 启动时把自己的 PID 写进 MG_PIDFILE（用于验证退出时进程真的没了）
"""
import json
import os
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("PORT") or os.environ.get("MODELMUX_PORT") or 18095)
TOKEN = "1234"
MODELS = ["wb-alpha", "wb-beta"]

pidfile = os.environ.get("MG_PIDFILE")
if pidfile:
    with open(pidfile, "w", encoding="utf-8") as f:
        f.write(str(os.getpid()))


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def _json(self, code, obj):
        body = json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _authed(self):
        return self.headers.get("Authorization") == "Bearer " + TOKEN

    # ── 只读管理接口（供「接口全覆盖」断言用；无副作用）─────────────────
    READONLY = {
        "/panel/api/models": {"ok": True, "models": [
            {"id": "mg-model-a", "name": "MG A", "credits": "x0.5",
             "default_effort": "high", "supported_efforts": ["low", "high"],
             "context_length": 128000, "max_output_tokens": 8192},
            {"id": "mg-model-b", "name": "MG B", "credits": "x0",
             "default_effort": "medium", "supported_efforts": [],
             "context_length": 64000, "max_output_tokens": 4096},
        ]},
        "/panel/api/config": {"ok": True, "path": "config.json", "config": {
            "listen": ":0", "api_key": "upstream-secret",
            "panel": {"package_detail_limit": 5},
            "upstream": {"timeout_seconds": 120},
        }},
        "/panel/api/logs": {"ok": True, "entries": [
            {"time": "2026-01-01T00:00:00Z", "level": "info",
             "channel": "sys", "msg": "mg fake log line"},
        ]},
        "/panel/api/request_metrics": {"ok": True, "total": 3, "recent": []},
        "/panel/api/request_logs": {"ok": True, "entries": [
            {"time": "2026-01-01T00:00:01Z", "ok": True, "model": "mg-model-a",
             "account": "u1", "dur_ms": 120, "tokens": 42, "credits": 1,
             "req_id": "mg-req-1"},
        ]},
        "/panel/api/usage": {"ok": True, "total_requests": 2, "total_tokens": 84,
                             "total_credits": 2, "buckets": [
                                 {"account": "u1", "requests": 2, "tokens": 84, "credits": 2},
                             ]},
        "/panel/api/packages": {"ok": True, "packages": [
            {"id": "p1", "name": "MG Package", "credits": 100, "remaining": 40,
             "expiry": "2026-12-31T00:00:00Z"},
        ], "total_credits": 100},
        "/panel/api/tasks/queue": {"ok": True, "items": [
            {"id": "t1", "name": "MG Task", "total": 3, "done": 1, "reward": 50},
        ], "running": False},
        "/panel/api/school/vouchers": {"ok": True, "vouchers": [
            {"code": "MG-VOUCHER-1", "state": "unused"},
        ]},
        "/panel/api/model_probes": {"ok": True, "results": [
            {"model": "mg-model-a", "ok": True, "latency_ms": 30},
        ]},
        "/panel/api/login/regions": {"ok": True, "regions": [
            {"id": "cn", "label": "cn"}, {"id": "global", "label": "global"},
        ]},
    }

    def do_GET(self):
        if not self._authed():
            self._json(401, {"error": {"message": "missing or bad token"}})
            return
        for path, payload in self.READONLY.items():
            if self.path.endswith(path):
                self._json(200, payload)
                return
        if self.path.endswith("/healthz"):
            self._json(200, {"ok": True})
            return
        # 管理面板 API（ModelMux 的代理断言用）：校验 Bearer 注入 + 原样透传
        if self.path.endswith("/panel/api/overview"):
            if not self._authed():
                self._json(401, {"error": {"message": "missing or bad token"}})
                return
            self._json(200, {
                "total": 2, "healthy": 2, "cooling": 0, "disabled": 0,
                "sticky_sessions": 1,
                "accounts": [
                    {"uid": "u1", "nickname": "账号甲", "credits": 500,
                     "credits_total": 1000, "cooling": False, "disabled": False,
                     "checkin_done": True, "realm": "cn"},
                    {"uid": "u2", "nickname": "账号乙", "credits": 30,
                     "cooling": True, "cool_remaining_sec": 120,
                     "disabled": False, "checkin_done": False, "realm": "cn"},
                ],
            })
            return
        if self.path.endswith("/models"):
            if not self._authed():
                self._json(401, {"error": {"message": "missing or bad token"}})
                return
            self._json(200, {"object": "list", "data": [{"id": m} for m in MODELS]})
            return
        self._json(404, {"error": {"message": "not found: " + self.path}})

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b"{}"
        try:
            req = json.loads(raw)
        except Exception:
            req = {}
        if not self.path.endswith("/chat/completions"):
            self._json(404, {"error": {"message": "not found: " + self.path}})
            return
        if not self._authed():
            self._json(401, {"error": {"message": "missing or bad token"}})
            return
        model = req.get("model", "?")
        if req.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Connection", "close")
            self.end_headers()
            for piece in ["托管", "流式"]:
                chunk = {"object": "chat.completion.chunk", "model": model,
                         "choices": [{"index": 0, "delta": {"content": piece}, "finish_reason": None}]}
                self.wfile.write(f"data: {json.dumps(chunk, ensure_ascii=False)}\n\n".encode())
                self.wfile.flush()
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
            return
        self._json(200, {
            "id": "chatcmpl-mg", "object": "chat.completion", "model": model,
            "choices": [{"index": 0, "message": {"role": "assistant", "content": f"mg:{model}"},
                         "finish_reason": "stop"}],
            "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
        })


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
