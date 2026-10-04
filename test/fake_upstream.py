#!/usr/bin/env python3
"""假上游：模拟三种协议形态，供 Mergence 端到端验证。

- /chat/v1/chat/completions : OpenAI Chat，支持非流式与 SSE 流式
- /anth/v1/models           : Anthropic 风格模型列表
- /anth/v1/messages         : Anthropic Messages，支持非流式与 SSE 流式
鉴权：Authorization 为 "Bearer bad" 或 x-api-key 为 "bad" 时返回 401。
"""
import json
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 18080

# 第三个模型故意用需要规范化的名字：区域后缀 + 小写厂商 + 预览版。
# 用来验证「客户端拿展示名调用 → 上游仍收到真实 ID」这条链路。
# 后三个是规范化规则的样本：区域后缀+小写厂商 / 免费 x 档 / 自动路由 / K+数字
CHAT_MODELS = ["fake-alpha", "fake-beta", "deepseek-v4-pro-cn",
               "hy4-preview-f", "hy3-x", "auto", "kimi-k2"]
ANTH_MODELS = ["fake-claude"]


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass  # 静默，避免污染测试输出

    def _json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _auth_bad(self):
        if self.headers.get("Authorization") == "Bearer bad":
            return True
        if self.headers.get("x-api-key") == "bad":
            return True
        return False

    def do_GET(self):
        if self.path.endswith("/models"):
            if self._auth_bad():
                self._json(401, {"error": {"message": "invalid api key"}})
                return
            models = ANTH_MODELS if self.path.startswith("/anth/") else CHAT_MODELS
            self._json(200, {"object": "list", "data": [{"id": m} for m in models]})
            return
        self._json(404, {"error": {"message": "not found: " + self.path}})

    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b"{}"
        try:
            req = json.loads(raw)
        except Exception:
            req = {}

        if self.path.endswith("/chat/completions"):
            self._chat(req)
        elif self.path.endswith("/v1/messages"):
            self._messages(req)
        else:
            self._json(404, {"error": {"message": "not found: " + self.path}})

    # ── OpenAI Chat

    def _chat(self, req):
        if self._auth_bad():
            self._json(401, {"error": {"message": "invalid api key"}})
            return
        model = req.get("model", "?")
        if model == "trigger-400":
            self._json(400, {"error": {"message": "bad request from upstream"}})
            return
        if req.get("stream"):
            self._sse_chat(model)
            return
        self._json(200, {
            "id": "chatcmpl-fake", "object": "chat.completion",
            "created": int(time.time()), "model": model,
            "choices": [{"index": 0, "message": {"role": "assistant", "content": f"echo:{model}"},
                         "finish_reason": "stop"}],
            # 故意带一个非标准字段，验证 Mergence 直通时不会把它丢掉
            "x_vendor_extra": {"nested": [1, 2, 3]},
            # 带缓存明细：验证 Mergence 能算出缓存命中率与分档费用
            # （prompt_tokens 含 cached，OpenAI 口径）
            "usage": {"prompt_tokens": 100, "completion_tokens": 7, "total_tokens": 107,
                      "prompt_tokens_details": {"cached_tokens": 40}},
        })

    def _sse_chat(self, model):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()
        for piece in ["分", "块", "流"]:
            chunk = {"id": "chatcmpl-fake", "object": "chat.completion.chunk",
                     "model": model,
                     "choices": [{"index": 0, "delta": {"content": piece}, "finish_reason": None}]}
            self.wfile.write(f"data: {json.dumps(chunk, ensure_ascii=False)}\n\n".encode())
            self.wfile.flush()
            time.sleep(0.02)
        done = {"id": "chatcmpl-fake", "object": "chat.completion.chunk", "model": model,
                "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 50, "completion_tokens": 3, "total_tokens": 53,
                          "prompt_tokens_details": {"cached_tokens": 20}}}
        self.wfile.write(f"data: {json.dumps(done)}\n\n".encode())
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()

    # ── Anthropic Messages

    def _messages(self, req):
        if self._auth_bad():
            self._json(401, {"type": "error",
                             "error": {"type": "authentication_error", "message": "invalid x-api-key"}})
            return
        model = req.get("model", "?")
        if req.get("stream"):
            self._sse_messages(model)
            return
        self._json(200, {
            "id": "msg_fake", "type": "message", "role": "assistant", "model": model,
            "content": [{"type": "text", "text": f"anth:{model}"},
                        {"type": "tool_use", "id": "toolu_1", "name": "probe", "input": {"k": "v"}}],
            "stop_reason": "tool_use",
            "usage": {"input_tokens": 11, "output_tokens": 4},
        })

    def _sse_messages(self, model):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Connection", "close")
        self.end_headers()

        def ev(name, obj):
            self.wfile.write(f"event: {name}\ndata: {json.dumps(obj, ensure_ascii=False)}\n\n".encode())
            self.wfile.flush()

        ev("message_start", {"type": "message_start",
                             "message": {"id": "msg_fake", "model": model,
                                         "usage": {"input_tokens": 9}}})
        ev("content_block_start", {"type": "content_block_start", "index": 0,
                                   "content_block": {"type": "text", "text": ""}})
        for piece in ["安", "思", "流"]:
            ev("content_block_delta", {"type": "content_block_delta", "index": 0,
                                       "delta": {"type": "text_delta", "text": piece}})
            time.sleep(0.02)
        ev("content_block_stop", {"type": "content_block_stop", "index": 0})
        ev("message_delta", {"type": "message_delta", "delta": {"stop_reason": "end_turn"},
                             "usage": {"output_tokens": 3}})
        ev("message_stop", {"type": "message_stop"})


if __name__ == "__main__":
    srv = ThreadingHTTPServer(("127.0.0.1", PORT), H)
    print(f"fake upstream on {PORT}", flush=True)
    srv.serve_forever()
