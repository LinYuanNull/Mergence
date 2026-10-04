// v1.go 对外的 OpenAI 兼容出口。
//
// 契约只有一份：OpenAI Chat Completions。上游说别的协议时，差异在 provider 层
// 被吸收掉，这里看到的一律是 OpenAI 形状。
//
// 这个文件刻意不做任何「业务判断」——鉴权、限流、计费都不在这里。
// 它的全部职责是：解析出 model → 交给路由 → 把结果原样送回客户端。
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"mergence/internal/logging"
	"mergence/internal/metrics"
	"mergence/internal/provider"
)

// maxChatBody 请求体上限。长上下文对话可能很大，放宽到 32MB。
const maxChatBody = 32 << 20

// handleV1Models 返回聚合后的模型列表。
//
// 只有「启用且已配置模型」的渠道会出现在这里——模型列表是客户端选模型用的，
// 列出一个调用必然失败的模型只会让人困惑。
func (s *Server) handleV1Models(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "该接口仅支持 GET",
			"invalid_request_error", "method_not_allowed")
		return
	}
	entries := s.reg.Models()
	created := s.start.Unix()
	data := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		data = append(data, map[string]any{
			"id":       e.ID,
			"object":   "model",
			"created":  created,
			"owned_by": e.Vendor,
			// 多给两个非标准字段：客户端解析时会忽略，但我们自己的面板与
			// dsh 插件直接读它们做展示，省掉一次额外请求。
			"channel": e.Channel,
			"context": e.Context,
		})
	}
	writeJSON(w, map[string]any{"object": "list", "data": data})
}

// handleV1Chat OpenAI 兼容的 Chat Completions 出口。
func (s *Server) handleV1Chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "该接口仅支持 POST",
			"invalid_request_error", "method_not_allowed")
		return
	}

	reqID := strings.TrimSpace(r.Header.Get("X-Request-Id"))
	if reqID == "" {
		reqID = logging.NewReqID()
	}
	// 回显链路 ID：客户端拿这个值就能在自己的日志里对上 Mergence 的请求链路
	w.Header().Set("X-Request-Id", reqID)

	body, err := io.ReadAll(io.LimitReader(r.Body, maxChatBody))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "读取请求体失败："+err.Error(),
			"invalid_request_error", "body_read_failed")
		return
	}

	var head struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "请求体不是合法 JSON："+err.Error(),
			"invalid_request_error", "invalid_json")
		return
	}

	ch, upstream, err := s.reg.Route(head.Model)
	if err != nil {
		status, typ, code := routeError(err)
		s.lg.Warn("路由失败", "req_id", reqID, "model", head.Model, "err", err.Error())
		writeOpenAIError(w, status, err.Error(), typ, code)
		return
	}

	lg := s.lg.WithProvider(ch.Name())
	lg.Info("转发请求",
		"req_id", reqID, "model", head.Model, "upstream_model", upstream,
		"stream", head.Stream, "body_bytes", len(body))

	call := &provider.ChatCall{
		Body:          body,
		Stream:        head.Stream,
		UpstreamModel: upstream,
		ReqID:         reqID,
	}

	ctx := r.Context()
	if !head.Stream {
		// 非流式才设整体超时；流式由传输层的「首字节超时」保护，
		// 否则长回答会被整体超时拦腰截断。
		tctx, cancel := context.WithTimeout(ctx, ch.Upstream().TimeoutDuration())
		defer cancel()
		ctx = tctx
	}

	start := time.Now()
	res, err := ch.Chat(ctx, call)
	if err != nil {
		status, typ, code := forwardError(err)
		lg.Error("转发失败",
			"req_id", reqID, "model", head.Model,
			"dur_ms", time.Since(start).Milliseconds(), "err", err.Error())
		// 失败也要记计量：上游可能已经扣了费（连接在响应途中断开、
		// 上游算完账才返回 5xx），漏记会让概览上的费用偏低于真实账单。
		s.metrics().Record(metrics.Record{
			Channel: ch.Name(), Source: string(ch.Source()), Model: head.Model,
			UpstreamModel: upstream, OK: false, Stream: head.Stream,
			DurationMS: time.Since(start).Milliseconds(), Error: err.Error(),
		})
		writeOpenAIError(w, status, err.Error(), typ, code)
		return
	}

	if res.Stream != nil {
		s.streamResponse(w, res, lg, ch, reqID, head.Model, upstream, start)
		return
	}

	ct := res.ContentType
	if ct == "" {
		ct = "application/json; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(res.Status)
	_, _ = w.Write(res.Body)

	dur := time.Since(start).Milliseconds()
	ok := res.Status < 400
	s.metrics().Record(metrics.Record{
		Channel: ch.Name(), Source: string(ch.Source()), Model: head.Model,
		UpstreamModel: upstream, Usage: usageOf(res.Usage), OK: ok,
		Status: res.Status, DurationMS: dur,
	})

	level := lg.Info
	if res.Status >= 400 {
		level = lg.Warn
	}
	level("转发完成",
		"req_id", reqID, "model", head.Model, "upstream_model", upstream,
		"status", res.Status, "dur_ms", dur,
		"tokens", res.TotalTokens, "key", res.KeyHint, "attempts", res.Attempts,
		"resp_bytes", len(res.Body))
}

// usageOf 把 provider 的 Usage 转成计量层的形状。
func usageOf(u provider.Usage) metrics.Usage {
	return metrics.Usage{
		PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens,
		TotalTokens: u.TotalTokens, CachedTokens: u.CachedTokens,
		CacheWriteTokens: u.CacheWriteTokens, HaveHitMiss: u.HaveHitMiss,
		PromptExcludesCache: u.PromptExcludesCache,
	}
}

// streamResponse 把流式响应写回客户端。
//
// 关键是**每读到一块就立刻 Flush**：否则 Go 的 http 缓冲会把小块攒起来，
// 客户端看到的效果是「一次性吐出全部内容」——流式就白做了。
func (s *Server) streamResponse(w http.ResponseWriter, res *provider.ChatResult,
	lg *logging.Logger, ch *provider.Channel, reqID, model, upstream string, start time.Time) {

	defer res.Stream.Close()

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(res.Status)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	var sent int64
	clientGone := false
	// 旁路扫 usage：只读不改，转发的字节一个都不动。
	// 流式的 usage 在末尾几个 chunk 里，扫不到就记 0（面板显示为无数据，
	// 而不是编一个数）。
	scanner := &provider.StreamUsageScanner{}

	for {
		n, rerr := res.Stream.Read(buf)
		if n > 0 {
			scanner.Feed(buf[:n])
			if _, werr := w.Write(buf[:n]); werr != nil {
				// 客户端断开：正常现象（用户按了停止），不当错误处理
				clientGone = true
				break
			}
			sent += int64(n)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				lg.Warn("流式传输中断",
					"req_id", reqID, "model", model,
					"sent_bytes", sent, "err", rerr.Error())
			}
			break
		}
	}

	dur := time.Since(start).Milliseconds()
	// 客户端提前断开时这次调用已经产生了消耗，照实计。
	s.metrics().Record(metrics.Record{
		Channel: ch.Name(), Source: string(ch.Source()), Model: model,
		UpstreamModel: upstream, Usage: usageOf(scanner.Usage()),
		OK: !clientGone, Status: res.Status, DurationMS: dur, Stream: true,
	})

	args := []any{
		"req_id", reqID, "model", model, "status", res.Status,
		"dur_ms", dur, "tokens", scanner.Usage().TotalTokens,
		"key", res.KeyHint, "attempts", res.Attempts,
		"stream_bytes", sent,
	}
	if clientGone {
		lg.Info("客户端提前断开流式连接", args...)
		return
	}
	lg.Info("流式转发完成", args...)
}

// routeError 把路由错误映射成 HTTP 状态码与 OpenAI 错误类型。
func routeError(err error) (int, string, string) {
	// 命中了渠道但它当前不可用（多半是托管型子进程没起来）。
	// 用 503 而不是 404：模型名是对的，只是暂时服务不了 —— 客户端据此可以重试，
	// 而 404 会让它以为模型名写错了去改配置。
	var nre *provider.NotReadyError
	if errors.As(err, &nre) {
		return http.StatusServiceUnavailable, "upstream_error", "channel_not_ready"
	}
	switch {
	case errors.Is(err, provider.ErrModelRequired):
		return http.StatusBadRequest, "invalid_request_error", "missing_model"
	case errors.Is(err, provider.ErrModelNotFound):
		return http.StatusNotFound, "invalid_request_error", "model_not_found"
	case errors.Is(err, provider.ErrAutoRouting):
		return http.StatusBadRequest, "invalid_request_error", "auto_not_supported"
	case errors.Is(err, provider.ErrNoChannel):
		return http.StatusServiceUnavailable, "server_error", "no_channel_available"
	default:
		return http.StatusInternalServerError, "server_error", "internal_error"
	}
}

// forwardError 把转发阶段的错误映射成 HTTP 状态码。
//
// 上游失败一律用 502：客户端据此能区分「我的请求有问题」（4xx）与
// 「网关拿到请求了但上游不行」（502），重试策略才有依据。
func forwardError(err error) (int, string, string) {
	switch {
	case errors.Is(err, provider.ErrResponsesUnsupported):
		return http.StatusNotImplemented, "invalid_request_error", "responses_unsupported"
	default:
		return http.StatusBadGateway, "upstream_error", "upstream_failed"
	}
}

// writeOpenAIError 用 OpenAI 的错误形状回复，保证任何 OpenAI SDK 都能解析出原因。
func writeOpenAIError(w http.ResponseWriter, status int, msg, typ, code string) {
	writeJSONStatus(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    typ,
			"code":    code,
			"param":   nil,
		},
	})
}

var _ = fmt.Sprintf
