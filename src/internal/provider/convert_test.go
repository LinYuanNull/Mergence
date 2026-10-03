package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("测试数据不是合法 JSON：%v", err)
	}
	return m
}

func TestRewriteModelKeepsOtherFields(t *testing.T) {
	in := `{"model":"or/foo","temperature":0.7,"n":9007199254740993,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"a":1}}}]}`
	out, err := rewriteModel([]byte(in), "up/foo")
	if err != nil {
		t.Fatalf("rewriteModel 失败：%v", err)
	}
	got := mustJSON(t, string(out))

	if got["model"] != "up/foo" {
		t.Errorf("model 未替换：%v", got["model"])
	}
	if got["temperature"] != 0.7 {
		t.Errorf("temperature 丢失：%v", got["temperature"])
	}
	// 大整数必须原样保留 —— 这正是用 json.RawMessage 而不是 map[string]any 的原因：
	// float64 往返会把 9007199254740993 变成 9007199254740992。
	if !strings.Contains(string(out), "9007199254740993") {
		t.Errorf("大整数被破坏：%s", out)
	}
	if got["tools"] == nil {
		t.Error("tools 丢失")
	}
}

func TestRewriteModelRejectsBadJSON(t *testing.T) {
	if _, err := rewriteModel([]byte("not json"), "m"); err == nil {
		t.Fatal("非法 JSON 应当报错")
	}
}

func TestToAnthropicRequestBasics(t *testing.T) {
	in := `{
	  "model":"claude/whatever","temperature":0.3,"stop":["END"],"max_tokens":123,
	  "messages":[
	    {"role":"system","content":"你是助手"},
	    {"role":"user","content":"你好"},
	    {"role":"user","content":[{"type":"text","text":"再来一句"}]}
	  ]
	}`
	out, err := toAnthropicRequest([]byte(in), "claude-3-5-sonnet")
	if err != nil {
		t.Fatalf("转换失败：%v", err)
	}
	m := mustJSON(t, string(out))

	if m["model"] != "claude-3-5-sonnet" {
		t.Errorf("model = %v", m["model"])
	}
	if m["max_tokens"] != float64(123) {
		t.Errorf("max_tokens = %v", m["max_tokens"])
	}
	if m["system"] != "你是助手" {
		t.Errorf("system 未提取：%v", m["system"])
	}
	stops, _ := m["stop_sequences"].([]any)
	if len(stops) != 1 || stops[0] != "END" {
		t.Errorf("stop_sequences = %v", m["stop_sequences"])
	}
	// 两条连续 user 必须被合并，否则 Anthropic 会 400
	msgs, _ := m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("连续同角色消息未合并，got %d 条：%v", len(msgs), msgs)
	}
	blocks, _ := msgs[0].(map[string]any)["content"].([]any)
	if len(blocks) != 2 {
		t.Errorf("合并后内容块数 = %d，期望 2", len(blocks))
	}
}

func TestToAnthropicRequestDefaultsMaxTokens(t *testing.T) {
	// Anthropic 的 max_tokens 是必填；OpenAI 里可省略。缺省必须补上，否则上游 400。
	out, err := toAnthropicRequest([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "m")
	if err != nil {
		t.Fatalf("转换失败：%v", err)
	}
	m := mustJSON(t, string(out))
	if m["max_tokens"] == nil || m["max_tokens"] == float64(0) {
		t.Fatalf("max_tokens 未补默认值：%v", m["max_tokens"])
	}
}

func TestToAnthropicRequestDoubleSystemMessageRejected(t *testing.T) {
	// 只有 system、没有 user/assistant 时应当明确报错，而不是让上游返回难懂的错误
	if _, err := toAnthropicRequest([]byte(`{"messages":[{"role":"system","content":"x"}]}`), "m"); err == nil {
		t.Fatal("无对话消息时应当报错")
	}
}

func TestToAnthropicRequestToolsAndToolCalls(t *testing.T) {
	in := `{
	  "messages":[
	    {"role":"user","content":"天气"},
	    {"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"上海\"}"}}]},
	    {"role":"tool","tool_call_id":"call_1","content":"晴 25℃"}
	  ],
	  "tools":[{"type":"function","function":{"name":"get_weather","description":"查天气","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}],
	  "tool_choice":{"type":"function","function":{"name":"get_weather"}}
	}`
	out, err := toAnthropicRequest([]byte(in), "claude")
	if err != nil {
		t.Fatalf("转换失败：%v", err)
	}
	m := mustJSON(t, string(out))

	tools, _ := m["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 数 = %d", len(tools))
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "get_weather" {
		t.Errorf("工具名 = %v", tool["name"])
	}
	if tool["input_schema"] == nil {
		t.Error("input_schema 未生成（Anthropic 用这个名字，不是 parameters）")
	}
	if _, bad := tool["function"]; bad {
		t.Error("不应保留 OpenAI 的 function 包装")
	}

	tc, _ := m["tool_choice"].(map[string]any)
	if tc == nil || tc["type"] != "tool" || tc["name"] != "get_weather" {
		t.Errorf("tool_choice = %v", m["tool_choice"])
	}

	msgs, _ := m["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("消息数 = %d，期望 3", len(msgs))
	}
	// assistant 的 tool_calls 应转成 content 里的 tool_use 块，且 arguments 由字符串变成对象
	asst := msgs[1].(map[string]any)
	blocks, _ := asst["content"].([]any)
	if len(blocks) != 1 {
		t.Fatalf("assistant 内容块数 = %d", len(blocks))
	}
	use, _ := blocks[0].(map[string]any)
	if use["type"] != "tool_use" || use["name"] != "get_weather" {
		t.Fatalf("tool_use 块 = %v", use)
	}
	input, _ := use["input"].(map[string]any)
	if input["city"] != "上海" {
		t.Errorf("input 未从 arguments 字符串解析成对象：%v", use["input"])
	}

	// tool 角色要变成 user 消息里的 tool_result 块
	last := msgs[2].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("tool 消息未转成 user 角色：%v", last["role"])
	}
	tres, _ := last["content"].([]any)[0].(map[string]any)
	if tres["type"] != "tool_result" || tres["tool_use_id"] != "call_1" {
		t.Errorf("tool_result 块 = %v", tres)
	}
}

func TestFromAnthropicResponseText(t *testing.T) {
	in := `{"id":"msg_01","type":"message","model":"claude-x","role":"assistant",
	  "content":[{"type":"text","text":"你好"},{"type":"text","text":"世界"}],
	  "stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":3}}`
	out, err := fromAnthropicResponse([]byte(in), "fallback")
	if err != nil {
		t.Fatalf("转换失败：%v", err)
	}
	m := mustJSON(t, string(out))

	if m["object"] != "chat.completion" {
		t.Errorf("object = %v", m["object"])
	}
	if m["model"] != "claude-x" {
		t.Errorf("model = %v", m["model"])
	}
	choices, _ := m["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices 数 = %d", len(choices))
	}
	c0 := choices[0].(map[string]any)
	msg := c0["message"].(map[string]any)
	if msg["content"] != "你好世界" {
		t.Errorf("文本未拼接：%v", msg["content"])
	}
	if msg["role"] != "assistant" {
		t.Errorf("role = %v", msg["role"])
	}
	if c0["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v（end_turn 应映射为 stop）", c0["finish_reason"])
	}
	u := m["usage"].(map[string]any)
	if u["prompt_tokens"] != float64(10) || u["completion_tokens"] != float64(3) || u["total_tokens"] != float64(13) {
		t.Errorf("usage 映射错误：%v", u)
	}
}

func TestFromAnthropicResponseToolUse(t *testing.T) {
	in := `{"id":"msg_02","model":"claude-x","content":[
	    {"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"北京"}}],
	  "stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":2}}`
	out, err := fromAnthropicResponse([]byte(in), "m")
	if err != nil {
		t.Fatalf("转换失败：%v", err)
	}
	m := mustJSON(t, string(out))
	c0 := m["choices"].([]any)[0].(map[string]any)
	if c0["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v（tool_use 应映射为 tool_calls）", c0["finish_reason"])
	}
	msg := c0["message"].(map[string]any)
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls 数 = %d", len(tcs))
	}
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("工具名 = %v", fn["name"])
	}
	// arguments 必须是「JSON 字符串」，这是 OpenAI 的约定
	args, _ := fn["arguments"].(string)
	if args == "" {
		t.Fatal("arguments 应为字符串")
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("arguments 不是合法 JSON 字符串：%v", err)
	}
	if parsed["city"] != "北京" {
		t.Errorf("arguments 内容 = %v", parsed)
	}
}

func TestFromAnthropicErrorToOpenAI(t *testing.T) {
	in := `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens 必须大于 0"}}`
	out, err := fromAnthropicResponse([]byte(in), "m")
	if err != nil {
		t.Fatalf("转换失败：%v", err)
	}
	m := mustJSON(t, string(out))
	e, _ := m["error"].(map[string]any)
	if e == nil {
		t.Fatalf("未转成 OpenAI 错误形状：%s", out)
	}
	if e["message"] != "max_tokens 必须大于 0" {
		t.Errorf("错误消息丢失：%v", e["message"])
	}
}

func TestAnthropicStreamToOpenAI(t *testing.T) {
	sse := strings.Join([]string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-x\",\"usage\":{\"input_tokens\":7}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"你\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"好\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":2}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}, "")

	var sb strings.Builder
	if err := anthropicStream(strings.NewReader(sse), &sb, nil, "claude-x"); err != nil {
		t.Fatalf("流式转换失败：%v", err)
	}
	out := sb.String()

	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("缺少 [DONE] 结尾：%q", out[len(out)-40:])
	}

	var text strings.Builder
	var finish string
	var promptTok, compTok float64
	err := eachSSEEvent(strings.NewReader(out), func(_ string, data []byte) error {
		if string(data) == "[DONE]" {
			return nil
		}
		var c map[string]any
		if err := json.Unmarshal(data, &c); err != nil {
			return err
		}
		if c["object"] != "chat.completion.chunk" {
			t.Errorf("object = %v", c["object"])
		}
		if ch, ok := c["choices"].([]any); ok && len(ch) > 0 {
			c0 := ch[0].(map[string]any)
			if d, ok := c0["delta"].(map[string]any); ok {
				if s, ok := d["content"].(string); ok {
					text.WriteString(s)
				}
			}
			if fr, ok := c0["finish_reason"].(string); ok && fr != "" {
				finish = fr
			}
		}
		if u, ok := c["usage"].(map[string]any); ok {
			promptTok, _ = u["prompt_tokens"].(float64)
			compTok, _ = u["completion_tokens"].(float64)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("解析输出 SSE 失败：%v", err)
	}

	if text.String() != "你好" {
		t.Errorf("拼接文本 = %q，期望 你好", text.String())
	}
	if finish != "length" {
		t.Errorf("finish_reason = %q（max_tokens 应映射为 length）", finish)
	}
	if promptTok != 7 || compTok != 2 {
		t.Errorf("usage = %v/%v", promptTok, compTok)
	}
}

func TestAnthropicStreamToolUse(t *testing.T) {
	sse := strings.Join([]string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_9\",\"name\":\"lookup\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"\\\"x\\\"}\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":5}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}, "")

	var sb strings.Builder
	if err := anthropicStream(strings.NewReader(sse), &sb, nil, "m"); err != nil {
		t.Fatalf("流式转换失败：%v", err)
	}

	var args strings.Builder
	var name string
	var finish string
	_ = eachSSEEvent(strings.NewReader(sb.String()), func(_ string, data []byte) error {
		if string(data) == "[DONE]" {
			return nil
		}
		var c map[string]any
		if err := json.Unmarshal(data, &c); err != nil {
			return err
		}
		ch, _ := c["choices"].([]any)
		if len(ch) == 0 {
			return nil
		}
		c0 := ch[0].(map[string]any)
		if fr, ok := c0["finish_reason"].(string); ok && fr != "" {
			finish = fr
		}
		d, _ := c0["delta"].(map[string]any)
		tcs, _ := d["tool_calls"].([]any)
		for _, raw := range tcs {
			tc := raw.(map[string]any)
			fn, _ := tc["function"].(map[string]any)
			if fn == nil {
				continue
			}
			if n, ok := fn["name"].(string); ok && n != "" {
				name = n
			}
			if a, ok := fn["arguments"].(string); ok {
				args.WriteString(a)
			}
		}
		return nil
	})

	if name != "lookup" {
		t.Errorf("工具名 = %q", name)
	}
	if args.String() != `{"q":"x"}` {
		t.Errorf("arguments 拼接 = %q", args.String())
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q", finish)
	}
}

func TestImageDataURLConversion(t *testing.T) {
	in := `{"messages":[{"role":"user","content":[
	    {"type":"text","text":"看图"},
	    {"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
	out, err := toAnthropicRequest([]byte(in), "m")
	if err != nil {
		t.Fatalf("转换失败：%v", err)
	}
	m := mustJSON(t, string(out))
	blocks := m["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("块数 = %d", len(blocks))
	}
	img := blocks[1].(map[string]any)
	if img["type"] != "image" {
		t.Fatalf("图片块类型 = %v", img["type"])
	}
	src := img["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "AAAA" {
		t.Errorf("data URL 未正确拆分：%v", src)
	}
}
