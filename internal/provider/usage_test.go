// usage_test.go usage 提取的三种上游口径。
//
// 这是最容易悄悄算错的地方：三个上游给缓存信息的方式完全不同，
// 而缓存 token 的单价通常只有全价的 1/10 —— 口径搞错，费用就差一个数量级。
package provider

import "testing"

func TestParseUsageOpenAI(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":200,
		"total_tokens":1200,"prompt_tokens_details":{"cached_tokens":400}}}`)
	u := parseUsage(body)
	if u.PromptTokens != 1000 || u.CompletionTokens != 200 || u.TotalTokens != 1200 {
		t.Fatalf("基础字段解析错误：%+v", u)
	}
	if u.CachedTokens != 400 {
		t.Errorf("缓存命中 = %d，期望 400", u.CachedTokens)
	}
	if u.PromptExcludesCache {
		t.Error("OpenAI 口径下 prompt_tokens 含缓存，PromptExcludesCache 应为 false")
	}
}

func TestParseUsageAnthropic(t *testing.T) {
	// fromAnthropicResponse 转换后的形状：
	// prompt_tokens = input_tokens（不含 cache_read），
	// prompt_excludes_cache 是转换时写下的口径标记。
	body := []byte(`{"usage":{"prompt_tokens":500,"completion_tokens":100,
		"total_tokens":8600,"prompt_tokens_details":{"cached_tokens":8000},
		"prompt_excludes_cache":true,"cache_creation_input_tokens":2000}}`)
	u := parseUsage(body)
	if u.CachedTokens != 8000 {
		t.Errorf("缓存读 = %d，期望 8000", u.CachedTokens)
	}
	if u.CacheWriteTokens != 2000 {
		t.Errorf("缓存写 = %d，期望 2000", u.CacheWriteTokens)
	}
	if !u.PromptExcludesCache {
		t.Error("Anthropic 口径下 prompt_tokens 不含缓存，PromptExcludesCache 应为 true")
	}
	// prompt(500) 是未缓存部分，总量应含缓存 8000
	if u.TotalTokens != 8600 {
		t.Errorf("总量 = %d，期望 8600（500+100+8000）", u.TotalTokens)
	}
}

func TestParseUsageAnthropicRaw(t *testing.T) {
	// 未转换的 Anthropic 原始形状：input_tokens 不含 cache_read
	body := []byte(`{"usage":{"input_tokens":500,"output_tokens":100,
		"cache_read_input_tokens":8000}}`)
	u := parseUsage(body)
	if u.PromptTokens != 500 || u.CompletionTokens != 100 {
		t.Errorf("Anthropic 原始字段解析错误：%+v", u)
	}
	if !u.PromptExcludesCache {
		t.Error("Anthropic 原始口径应标记 PromptExcludesCache")
	}
	if u.TotalTokens != 8600 {
		t.Errorf("总量 = %d，期望 8600", u.TotalTokens)
	}
}

func TestParseUsageDeepSeek(t *testing.T) {
	body := []byte(`{"usage":{"prompt_tokens":9000,"completion_tokens":500,
		"prompt_cache_hit_tokens":8000,"prompt_cache_miss_tokens":1000}}`)
	u := parseUsage(body)
	if u.CachedTokens != 8000 {
		t.Errorf("缓存命中 = %d，期望 8000", u.CachedTokens)
	}
	if u.PromptTokens != 9000 {
		t.Errorf("输入总量 = %d，期望 9000（hit+miss）", u.PromptTokens)
	}
	if !u.HaveHitMiss {
		t.Error("DeepSeek 口径应标记 HaveHitMiss")
	}
	if u.PromptExcludesCache {
		t.Error("DeepSeek 的 prompt_tokens 含缓存，不应标记 PromptExcludesCache")
	}
}

func TestParseUsageNoUsage(t *testing.T) {
	if u := parseUsage([]byte(`{"id":"x"}`)); u != (Usage{}) {
		t.Errorf("无 usage 字段时应返回零值，实际 %+v", u)
	}
	if u := parseUsage([]byte(`not json`)); u != (Usage{}) {
		t.Errorf("非法 JSON 应返回零值，实际 %+v", u)
	}
}

func TestParseUsageTotalDerived(t *testing.T) {
	// 只给 prompt/completion，没给 total
	u := parseUsage([]byte(`{"usage":{"prompt_tokens":300,"completion_tokens":100}}`))
	if u.TotalTokens != 400 {
		t.Errorf("总量应由 prompt+completion 推出，实际 %d", u.TotalTokens)
	}
}

func TestStreamUsageScanner(t *testing.T) {
	var s StreamUsageScanner
	// 前面是只有 delta 的 chunk
	s.Feed([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	if s.Usage().TotalTokens != 0 {
		t.Error("还没有 usage 时不应有数据")
	}
	// usage 在末尾（OpenAI 流式的常见形态）
	s.Feed([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"total_tokens\":120}}\n\n"))
	s.Feed([]byte("data: [DONE]\n\n"))
	u := s.Usage()
	if u.TotalTokens != 120 {
		t.Errorf("流式 usage = %d，期望 120", u.TotalTokens)
	}
	if u.PromptTokens != 100 || u.CompletionTokens != 20 {
		t.Errorf("流式 usage 基础字段错误：%+v", u)
	}
}

func TestStreamUsageScannerChunked(t *testing.T) {
	// 事件被网络切碎：逐字节喂入也应能解析出来
	var s StreamUsageScanner
	raw := "data: {\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":5,\"total_tokens\":55}}\n\n"
	for i := 0; i < len(raw); i++ {
		s.Feed([]byte{raw[i]})
	}
	if s.Usage().TotalTokens != 55 {
		t.Errorf("切碎后仍应解析出 usage，实际 %+v", s.Usage())
	}
}

func TestStreamUsageScannerWindowBounded(t *testing.T) {
	// 大量无 usage 的内容不应让窗口无限增长
	var s StreamUsageScanner
	big := make([]byte, 4096)
	for i := range big {
		big[i] = 'x'
	}
	for i := 0; i < 100; i++ {
		s.Feed(big)
	}
	if len(s.window) > streamScanWindow {
		t.Errorf("窗口应被限制在 %d 字节内，实际 %d", streamScanWindow, len(s.window))
	}
}

func TestAnthropicConvertRoundTrip(t *testing.T) {
	// 端到端：Anthropic 响应 → OpenAI 形状 → parseUsage 读回，
	// 缓存口径必须一路保持（这是最容易在转换层丢信息的地方）。
	raw := []byte(`{"id":"msg_1","model":"claude-sonnet-5","stop_reason":"end_turn",
		"content":[{"type":"text","text":"ok"}],
		"usage":{"input_tokens":500,"output_tokens":100,
			"cache_read_input_tokens":8000,"cache_creation_input_tokens":2000}}`)
	converted, err := fromAnthropicResponse(raw, "claude-sonnet-5")
	if err != nil {
		t.Fatalf("转换失败：%v", err)
	}
	u := parseUsage(converted)
	if u.CachedTokens != 8000 {
		t.Errorf("往返后缓存读 = %d，期望 8000", u.CachedTokens)
	}
	if u.CacheWriteTokens != 2000 {
		t.Errorf("往返后缓存写 = %d，期望 2000", u.CacheWriteTokens)
	}
	if !u.PromptExcludesCache {
		t.Error("往返后应保持 Anthropic 口径标记")
	}
	if u.TotalTokens != 8600 {
		t.Errorf("往返后总量 = %d，期望 8600", u.TotalTokens)
	}
}
