// convert.go 协议适配：OpenAI Chat Completions ⇄ Anthropic Messages。
//
// 对外只有一种契约（OpenAI Chat Completions），上游说别的协议时在这里吸收差异。
//
// 为什么 chat 协议不做转换、只做保真直通：
// OpenAI 兼容生态里的字段远比规范多（reasoning_content、tool_calls 的扩展、
// 各家自创的 usage 明细）。解析再重新序列化必然会丢掉我们没预料到的字段，
// 而转发类服务丢字段是最难排查的故障——客户端拿到的响应「看起来对但就是不对」。
// 所以只要上下游格式一致，就一个字节都别碰。
package provider

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ─────────────────────────────── 小工具

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// firstPresent 按顺序取第一个存在的键。
func firstPresent(m map[string]any, keys ...string) (any, bool) {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v, true
		}
	}
	return nil, false
}

// contentText 把 OpenAI 的 content（字符串或分块数组）拍平成纯文本。
func contentText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, part := range t {
			p := asMap(part)
			if p == nil {
				continue
			}
			if s := asString(p["text"]); s != "" {
				b.WriteString(s)
			}
		}
		return b.String()
	default:
		return ""
	}
}

// ─────────────────────────────── 请求：OpenAI → Anthropic

// toAnthropicRequest 把 OpenAI Chat Completions 请求体转成 Anthropic Messages 请求体。
func toAnthropicRequest(body []byte, upstreamModel string) ([]byte, error) {
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON：%w", err)
	}

	out := map[string]any{"model": upstreamModel}

	// max_tokens 在 Anthropic 是**必填**；OpenAI 里可省略。缺省给一个安全值，
	// 否则上游直接 400 —— 这种错用户很难自己定位。
	if v, ok := firstPresent(in, "max_completion_tokens", "max_tokens"); ok {
		out["max_tokens"] = v
	} else {
		out["max_tokens"] = float64(4096)
	}

	for _, k := range []string{"temperature", "top_p", "top_k", "stream"} {
		if v, ok := in[k]; ok {
			out[k] = v
		}
	}
	if v, ok := in["stop"]; ok {
		out["stop_sequences"] = toStopSequences(v)
	}

	// 工具定义：OpenAI 是 {type:"function", function:{name,...}}，Anthropic 是扁平的。
	if v, ok := in["tools"]; ok {
		if tools := toAnthropicTools(v); len(tools) > 0 {
			out["tools"] = tools
		}
	}
	if v, ok := in["tool_choice"]; ok {
		if tc := toAnthropicToolChoice(v); tc != nil {
			out["tool_choice"] = tc
		}
	}

	systemParts, msgs := splitMessages(asSlice(in["messages"]))
	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("messages 为空，无法转换为 Anthropic 格式")
	}
	out["messages"] = mergeConsecutiveRoles(msgs)

	return json.Marshal(out)
}

// splitMessages 把 OpenAI 的 messages 拆成 system 文本 + Anthropic 消息数组。
func splitMessages(msgs []any) ([]string, []any) {
	var system []string
	var out []any

	for _, raw := range msgs {
		m := asMap(raw)
		if m == nil {
			continue
		}
		switch asString(m["role"]) {
		case "system", "developer":
			if t := contentText(m["content"]); t != "" {
				system = append(system, t)
			}

		case "user":
			blocks := toAnthropicBlocks(m["content"])
			if len(blocks) > 0 {
				out = append(out, map[string]any{"role": "user", "content": blocks})
			}

		case "assistant":
			blocks := toAnthropicBlocks(m["content"])
			// OpenAI 把工具调用放在 message.tool_calls，Anthropic 放在 content 的 tool_use 块里
			for _, tcRaw := range asSlice(m["tool_calls"]) {
				tc := asMap(tcRaw)
				if tc == nil {
					continue
				}
				fn := asMap(tc["function"])
				if fn == nil {
					continue
				}
				var input any = map[string]any{}
				// arguments 是「JSON 字符串」，Anthropic 要的是对象
				if s := asString(fn["arguments"]); strings.TrimSpace(s) != "" {
					var parsed any
					if err := json.Unmarshal([]byte(s), &parsed); err == nil {
						input = parsed
					}
				}
				block := map[string]any{
					"type":  "tool_use",
					"id":    asString(tc["id"]),
					"name":  asString(fn["name"]),
					"input": input,
				}
				if block["id"] == "" {
					delete(block, "id")
				}
				blocks = append(blocks, block)
			}
			if len(blocks) > 0 {
				out = append(out, map[string]any{"role": "assistant", "content": blocks})
			}

		case "tool", "function":
			// Anthropic 没有 tool 角色：工具结果要以 user 消息里的 tool_result 块表达
			id := asString(m["tool_call_id"])
			if id == "" {
				continue
			}
			block := map[string]any{
				"type":        "tool_result",
				"tool_use_id": id,
				"content":     contentText(m["content"]),
			}
			out = append(out, map[string]any{"role": "user", "content": []any{block}})
		}
	}
	return system, out
}

// toAnthropicBlocks 把 OpenAI 的 content 转成 Anthropic 内容块数组。
func toAnthropicBlocks(content any) []any {
	switch t := content.(type) {
	case string:
		if strings.TrimSpace(t) == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": t}}

	case []any:
		out := make([]any, 0, len(t))
		for _, part := range t {
			p := asMap(part)
			if p == nil {
				continue
			}
			switch asString(p["type"]) {
			case "text", "input_text", "output_text":
				if s := asString(p["text"]); s != "" {
					out = append(out, map[string]any{"type": "text", "text": s})
				}
			case "image_url":
				if blk := toAnthropicImage(p); blk != nil {
					out = append(out, blk)
				}
			}
		}
		return out

	default:
		return nil
	}
}

// toAnthropicImage 处理 OpenAI 的 image_url 分块。
func toAnthropicImage(p map[string]any) map[string]any {
	u := ""
	switch v := p["image_url"].(type) {
	case string:
		u = v
	case map[string]any:
		u = asString(v["url"])
	}
	if u == "" {
		return nil
	}
	// data URL 走 base64 source；普通 URL 走 url source（新版 Anthropic 支持）
	if strings.HasPrefix(u, "data:") {
		rest := strings.TrimPrefix(u, "data:")
		idx := strings.Index(rest, ";base64,")
		if idx < 0 {
			return nil
		}
		return map[string]any{
			"type": "image",
			"source": map[string]any{
				"type": "base64", "media_type": rest[:idx], "data": rest[idx+len(";base64,"):],
			},
		}
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": u}}
}

// mergeConsecutiveRoles 合并连续同角色消息。
//
// Anthropic 要求 user/assistant 严格交替；而 OpenAI 的对话里连续两条 user 很常见
// （例如注入的上下文 + 用户输入）。不合并会被上游 400 掉。
func mergeConsecutiveRoles(msgs []any) []any {
	out := make([]any, 0, len(msgs))
	for _, raw := range msgs {
		m := asMap(raw)
		if m == nil {
			continue
		}
		if len(out) == 0 {
			out = append(out, m)
			continue
		}
		prev := asMap(out[len(out)-1])
		if asString(prev["role"]) != asString(m["role"]) {
			out = append(out, m)
			continue
		}
		prev["content"] = append(asSlice(prev["content"]), asSlice(m["content"])...)
	}
	return out
}

func toStopSequences(v any) []string {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, s := range t {
			if str := asString(s); str != "" {
				out = append(out, str)
			}
		}
		return out
	default:
		return nil
	}
}

func toAnthropicTools(v any) []any {
	var out []any
	for _, raw := range asSlice(v) {
		t := asMap(raw)
		if t == nil || asString(t["type"]) != "function" {
			continue
		}
		fn := asMap(t["function"])
		if fn == nil || asString(fn["name"]) == "" {
			continue
		}
		tool := map[string]any{"name": asString(fn["name"])}
		if d := asString(fn["description"]); d != "" {
			tool["description"] = d
		}
		if p, ok := fn["parameters"]; ok {
			tool["input_schema"] = p
		} else {
			tool["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, tool)
	}
	return out
}

func toAnthropicToolChoice(v any) map[string]any {
	switch t := v.(type) {
	case string:
		switch t {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required":
			return map[string]any{"type": "any"}
		case "none":
			// Anthropic 没有「禁止调用」；省略 tool_choice 且不传 tools 才是等价语义。
			// 这里返回 nil，由调用方决定是否清空 tools。
			return nil
		}
	case map[string]any:
		if fn := asMap(t["function"]); fn != nil {
			if n := asString(fn["name"]); n != "" {
				return map[string]any{"type": "tool", "name": n}
			}
		}
	}
	return nil
}

// ─────────────────────────────── 响应：Anthropic → OpenAI

// fromAnthropicResponse 把 Anthropic Messages 响应转成 OpenAI Chat Completion 响应。
func fromAnthropicResponse(body []byte, model string) ([]byte, error) {
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("上游响应不是合法 JSON：%w", err)
	}

	// 上游报错：转成 OpenAI 错误形状，让客户端能正常解析并显示原因
	if asString(in["type"]) == "error" {
		return anthropicErrorToOpenAI(in), nil
	}

	var text strings.Builder
	var toolCalls []any

	for _, raw := range asSlice(in["content"]) {
		b := asMap(raw)
		if b == nil {
			continue
		}
		switch asString(b["type"]) {
		case "text":
			text.WriteString(asString(b["text"]))
		case "tool_use":
			args, _ := json.Marshal(b["input"])
			if b["input"] == nil {
				args = []byte("{}")
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   asString(b["id"]),
				"type": "function",
				"function": map[string]any{
					"name":      asString(b["name"]),
					"arguments": string(args),
				},
			})
		case "thinking", "redacted_thinking":
			// Anthropic 的思考块。OpenAI 侧没有对应字段，丢弃但保留正文不受影响。
		}
	}

	msg := map[string]any{"role": "assistant", "content": text.String()}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		if text.Len() == 0 {
			msg["content"] = nil
		}
	}

	out := map[string]any{
		"id":      openAIChatID(asString(in["id"])),
		"object":  "chat.completion",
		"created": nowUnix(),
		"model":   firstNonEmpty(asString(in["model"]), model),
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": anthropicFinishReason(asString(in["stop_reason"])),
		}},
	}
	if u := asMap(in["usage"]); u != nil {
		out["usage"] = anthropicUsageToOpenAI(u)
	}
	return json.Marshal(out)
}

// anthropicErrorToOpenAI 把 Anthropic 错误体转成 OpenAI 错误体。
func anthropicErrorToOpenAI(in map[string]any) []byte {
	e := asMap(in["error"])
	msg, typ := "上游返回错误", "upstream_error"
	if e != nil {
		msg = firstNonEmpty(asString(e["message"]), msg)
		typ = firstNonEmpty(asString(e["type"]), typ)
	}
	out, err := json.Marshal(map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": typ},
	})
	if err != nil {
		return []byte(`{"error":{"message":"上游返回错误","type":"upstream_error"}}`)
	}
	return out
}

// anthropicUsageToOpenAI 把 Anthropic 的 usage 转成 OpenAI 形状。
//
// 关键：额外带出缓存字段。Anthropic 的 prompt caching 差价很大
// （缓存读通常是全价的 1/10），把 cache_read_input_tokens 丢掉的话
// 概览上的缓存命中率永远是 0、费用也会明显偏高。
//
// 转换后 parseUsage 能从这些字段里读出缓存用量，所以两边口径一致。
func anthropicUsageToOpenAI(u map[string]any) map[string]any {
	in := intOf(u["input_tokens"])
	outT := intOf(u["output_tokens"])
	cacheRead := intOf(u["cache_read_input_tokens"])
	m := map[string]any{
		"prompt_tokens":     in,
		"completion_tokens": outT,
		// total 含缓存读：Anthropic 账单里的 token 总量是三者之和，
		// 与 OpenAI 的 prompt(含缓存)+completion 口径对齐，
		// 这样客户端看到的 total 与 Mergence 记的计量一致。
		"total_tokens": in + outT + cacheRead,
	}
	// 缓存读：Anthropic 单独给，且不计入 input_tokens。
	//
	// prompt_excludes_cache 是必需的标记：OpenAI 的 prompt_tokens 含缓存、
	// Anthropic 的 input_tokens 不含，两者算法相反。不带这个标记的话，
	// 下游按 OpenAI 口径做减法会得到负数，夹成 0 后总量凭空少一大截。
	if cacheRead > 0 {
		m["prompt_tokens_details"] = map[string]any{"cached_tokens": cacheRead}
		m["prompt_excludes_cache"] = true
	}
	// 缓存写：OpenAI 没有对应字段，但保留它才能算出「为缓存付了多少钱」。
	if n := intOf(u["cache_creation_input_tokens"]); n > 0 {
		m["cache_creation_input_tokens"] = n
	}
	return m
}

func anthropicFinishReason(r string) string {
	switch r {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "stop_sequence", "end_turn", "":
		return "stop"
	default:
		return "stop"
	}
}

// ─────────────────────────────── 流式：Anthropic SSE → OpenAI SSE

// anthropicStream 把 Anthropic 的 SSE 流转成 OpenAI 的 SSE 流写出去。
//
// 返回首个错误的描述（用于日志）；调用方已把响应头发出，无法再改状态码。
func anthropicStream(r io.Reader, w io.Writer, flush func(), model string) error {
	st := &streamState{
		model:   model,
		id:      openAIChatID(""),
		created: nowUnix(),
		toolIdx: map[int]int{},
	}
	if err := st.emitRole(w, flush); err != nil {
		return err
	}

	err := eachSSEEvent(r, func(_ string, data []byte) error {
		var ev map[string]any
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil // 无法解析的事件直接跳过，不打断整条流
		}
		switch asString(ev["type"]) {
		case "message_start":
			if m := asMap(ev["message"]); m != nil {
				if id := asString(m["id"]); id != "" {
					st.id = openAIChatID(id)
				}
				if u := asMap(m["usage"]); u != nil {
					st.promptTokens = intOf(u["input_tokens"])
				}
			}

		case "content_block_start":
			b := asMap(ev["content_block"])
			if b == nil {
				return nil
			}
			if asString(b["type"]) == "tool_use" {
				idx := intOf(ev["index"])
				oi := len(st.toolIdx)
				st.toolIdx[idx] = oi
				return st.emit(w, flush, map[string]any{
					"tool_calls": []any{map[string]any{
						"index": oi, "id": asString(b["id"]), "type": "function",
						"function": map[string]any{"name": asString(b["name"]), "arguments": ""},
					}},
				})
			}

		case "content_block_delta":
			d := asMap(ev["delta"])
			if d == nil {
				return nil
			}
			switch asString(d["type"]) {
			case "text_delta":
				if s := asString(d["text"]); s != "" {
					return st.emit(w, flush, map[string]any{"content": s})
				}
			case "input_json_delta":
				oi := st.toolIdx[intOf(ev["index"])]
				if s := asString(d["partial_json"]); s != "" {
					return st.emit(w, flush, map[string]any{
						"tool_calls": []any{map[string]any{
							"index":    oi,
							"function": map[string]any{"arguments": s},
						}},
					})
				}
			}

		case "message_delta":
			if d := asMap(ev["delta"]); d != nil {
				st.finish = anthropicFinishReason(asString(d["stop_reason"]))
			}
			if u := asMap(ev["usage"]); u != nil {
				st.outputTokens = intOf(u["output_tokens"])
			}

		case "message_stop":
			return errStreamDone
		}
		return nil
	})

	if err != nil && err != errStreamDone {
		return err
	}
	return st.emitFinal(w, flush)
}

// errStreamDone 用作「正常结束」的信号，避免用 error 表达控制流时被误报。
var errStreamDone = fmt.Errorf("stream done")

type streamState struct {
	model        string
	id           string
	created      int64
	toolIdx      map[int]int
	finish       string
	promptTokens int
	outputTokens int
	roleSent     bool
}

func (s *streamState) emitRole(w io.Writer, flush func()) error {
	s.roleSent = true
	return s.emit(w, flush, map[string]any{"role": "assistant", "content": ""})
}

func (s *streamState) emit(w io.Writer, flush func(), delta map[string]any) error {
	chunk := map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
	}
	if err := writeSSE(w, chunk); err != nil {
		return err
	}
	if flush != nil {
		flush()
	}
	return nil
}

func (s *streamState) emitFinal(w io.Writer, flush func()) error {
	if s.finish == "" {
		s.finish = "stop"
	}
	chunk := map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": s.created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": s.finish}},
		"usage": map[string]any{
			"prompt_tokens": s.promptTokens, "completion_tokens": s.outputTokens,
			"total_tokens": s.promptTokens + s.outputTokens,
		},
	}
	if err := writeSSE(w, chunk); err != nil {
		return err
	}
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	if flush != nil {
		flush()
	}
	return err
}

func writeSSE(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("data: ")
	buf.Write(b)
	buf.WriteString("\n\n")
	_, err = w.Write(buf.Bytes())
	return err
}

// eachSSEEvent 逐条解析 SSE 事件。fn 返回 error 时立即停止。
func eachSSEEvent(r io.Reader, fn func(event string, data []byte) error) error {
	br := bufio.NewReaderSize(r, 64*1024)
	var event string
	var data bytes.Buffer

	dispatch := func() error {
		if data.Len() == 0 {
			event = ""
			return nil
		}
		payload := bytes.TrimRight(data.Bytes(), "\n")
		data.Reset()
		ev := event
		event = ""
		return fn(ev, payload)
	}

	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "":
				if derr := dispatch(); derr != nil {
					return derr
				}
			case strings.HasPrefix(trimmed, ":"):
				// 注释行（含 Anthropic 的 ping），忽略
			case strings.HasPrefix(trimmed, "event:"):
				event = strings.TrimSpace(trimmed[len("event:"):])
			case strings.HasPrefix(trimmed, "data:"):
				data.WriteString(strings.TrimSpace(trimmed[len("data:"):]))
				data.WriteByte('\n')
			}
		}
		if err != nil {
			if derr := dispatch(); derr != nil {
				return derr
			}
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// ─────────────────────────────── 杂项

func nowUnix() int64 { return timeNow().Unix() }

func openAIChatID(upstreamID string) string {
	if strings.HasPrefix(upstreamID, "chatcmpl-") {
		return upstreamID
	}
	if upstreamID == "" {
		return "chatcmpl-" + randomID(24)
	}
	return "chatcmpl-" + upstreamID
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func intOf(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	default:
		return 0
	}
}
