package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mergence/internal/config"
	"mergence/internal/logging"
)

func testLogger(t *testing.T) *logging.Logger {
	t.Helper()
	lg, err := logging.New("debug", t.TempDir(), 200, 8)
	if err != nil {
		t.Fatalf("创建日志器失败：%v", err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	return lg
}

func testChannel(t *testing.T, baseURL, protocol string, keys []string, models config.ModelList) *Channel {
	t.Helper()
	up := Upstream{
		Name: "test", DisplayName: "test", Source: SourceEmbedded, Enabled: true,
		BaseURL: baseURL, Protocol: protocol, ModelPrefix: "t/",
		APIKeys: keys, Models: models, Weight: 1, Retries: 1,
	}
	ch, err := NewTempChannel(up, testLogger(t))
	if err != nil {
		t.Fatalf("构造渠道失败：%v", err)
	}
	return ch
}

// ── chat 直通：必须保真，不能丢字段

func TestChatPassthroughPreservesBody(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"total_tokens":42}}`)
	}))
	defer srv.Close()

	ch := testChannel(t, srv.URL, "chat", []string{"sk-test"}, nil)
	call := &ChatCall{
		Body:          []byte(`{"model":"t/anything","temperature":0.9,"reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`),
		UpstreamModel: "up-model",
	}
	res, err := ch.Chat(context.Background(), call)
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}

	if gotPath != "/chat/completions" {
		t.Errorf("上游路径 = %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("鉴权头 = %q", gotAuth)
	}
	if gotBody["model"] != "up-model" {
		t.Errorf("model 未改写：%v", gotBody["model"])
	}
	// 关键：我们没预料到的字段必须原样送到上游
	if gotBody["reasoning_effort"] != "high" {
		t.Errorf("未知字段 reasoning_effort 丢失：%v", gotBody)
	}
	if gotBody["temperature"] != 0.9 {
		t.Errorf("temperature 丢失：%v", gotBody)
	}
	if res.Status != 200 || res.TotalTokens != 42 {
		t.Errorf("status=%d tokens=%d", res.Status, res.TotalTokens)
	}
	if !strings.Contains(string(res.Body), `"content":"hi"`) {
		t.Errorf("响应体被改动：%s", res.Body)
	}
}

func TestChatStreamPassthroughIsByteIdentical(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer srv.Close()

	ch := testChannel(t, srv.URL, "chat", []string{"k"}, nil)
	res, err := ch.Chat(context.Background(), &ChatCall{
		Body:          []byte(`{"model":"t/m","stream":true,"messages":[]}`),
		Stream:        true,
		UpstreamModel: "m",
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	if res.Stream == nil {
		t.Fatal("流式请求应返回 Stream")
	}
	got, _ := io.ReadAll(res.Stream)
	_ = res.Stream.Close()
	if string(got) != sse {
		t.Errorf("流式字节被改动：\n得到 %q\n期望 %q", got, sse)
	}
}

// ── 多 Key 轮询：401 必须换 Key 重试

func TestChatRetriesWithNextKeyOn401(t *testing.T) {
	var seenAuth []string
	var n int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))
		if n == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"invalid key"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	ch := testChannel(t, srv.URL, "chat", []string{"bad", "good"}, nil)
	res, err := ch.Chat(context.Background(), &ChatCall{
		Body: []byte(`{"model":"t/m","messages":[]}`), UpstreamModel: "m",
	})
	if err != nil {
		t.Fatalf("应当换 Key 重试成功，却失败：%v", err)
	}
	if res.Status != 200 {
		t.Errorf("最终状态 = %d", res.Status)
	}
	if res.Attempts != 2 {
		t.Errorf("尝试次数 = %d，期望 2", res.Attempts)
	}
	if len(seenAuth) != 2 || seenAuth[0] != "Bearer bad" || seenAuth[1] != "Bearer good" {
		t.Errorf("Key 轮询顺序错误：%v", seenAuth)
	}
	// 坏 Key 应进入冷却
	stats := ch.KeyStats()
	if !stats[0].Cooling {
		t.Errorf("401 后首个 Key 应进入冷却：%+v", stats[0])
	}
}

func TestChatDoesNotRetryOn400(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad request"}}`)
	}))
	defer srv.Close()

	ch := testChannel(t, srv.URL, "chat", []string{"k1", "k2", "k3"}, nil)
	res, err := ch.Chat(context.Background(), &ChatCall{
		Body: []byte(`{"model":"t/m","messages":[]}`), UpstreamModel: "m",
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	if res.Status != 400 {
		t.Errorf("状态 = %d", res.Status)
	}
	// 400 是请求本身的问题，换 Key 没用，不该重试
	if n != 1 {
		t.Errorf("400 不应重试，实际请求了 %d 次", n)
	}
	if res.Attempts != 1 {
		t.Errorf("尝试次数 = %d", res.Attempts)
	}
}

func TestChatResponsesProtocolRejected(t *testing.T) {
	ch := testChannel(t, "http://127.0.0.1:1", "responses", nil, nil)
	_, err := ch.Chat(context.Background(), &ChatCall{
		Body: []byte(`{"model":"t/m","messages":[]}`), UpstreamModel: "m",
	})
	if err == nil || !strings.Contains(err.Error(), "Responses") {
		t.Fatalf("应当明确拒绝 Responses 协议，实际：%v", err)
	}
}

// ── Anthropic 协议：路径、鉴权头、双向转换

func TestChatAnthropicProtocol(t *testing.T) {
	var gotPath, gotKey, gotVersion string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","model":"claude-x","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
	}))
	defer srv.Close()

	ch := testChannel(t, srv.URL, "anthropic", []string{"sk-ant-1"}, nil)
	res, err := ch.Chat(context.Background(), &ChatCall{
		Body:          []byte(`{"model":"t/claude","messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}],"max_tokens":64}`),
		UpstreamModel: "claude-x",
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}

	if gotPath != "/v1/messages" {
		t.Errorf("Anthropic 路径 = %q，期望 /v1/messages", gotPath)
	}
	if gotKey != "sk-ant-1" {
		t.Errorf("x-api-key = %q", gotKey)
	}
	if gotVersion == "" {
		t.Error("缺少 anthropic-version 头")
	}
	if gotBody["system"] != "sys" {
		t.Errorf("system 未提取：%v", gotBody)
	}
	if gotBody["model"] != "claude-x" {
		t.Errorf("model = %v", gotBody["model"])
	}
	// 回到客户端的必须是 OpenAI 形状
	out := mustJSON(t, string(res.Body))
	if out["object"] != "chat.completion" {
		t.Errorf("对外响应不是 OpenAI 形状：%s", res.Body)
	}
	if res.TotalTokens != 4 {
		t.Errorf("token 合计 = %d，期望 4", res.TotalTokens)
	}
}

func TestChatAnthropicStreamConverted(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":2}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"嗨\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, sse)
	}))
	defer srv.Close()

	ch := testChannel(t, srv.URL, "anthropic", []string{"k"}, nil)
	res, err := ch.Chat(context.Background(), &ChatCall{
		Body:          []byte(`{"model":"t/c","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		Stream:        true,
		UpstreamModel: "c",
	})
	if err != nil {
		t.Fatalf("Chat 失败：%v", err)
	}
	got, err := io.ReadAll(res.Stream)
	_ = res.Stream.Close()
	if err != nil {
		t.Fatalf("读取流失败：%v", err)
	}
	out := string(got)
	if !strings.Contains(out, `"嗨"`) {
		t.Errorf("文本增量未转换：%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("缺少 [DONE]：%s", out)
	}
	if strings.Contains(out, "content_block_delta") {
		t.Errorf("Anthropic 原始事件漏出去了：%s", out)
	}
}

// ── 模型列表解析：三种上游形态都要认

func TestParseModelIDs(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"OpenAI 风格", `{"object":"list","data":[{"id":"a"},{"id":"b"}]}`, []string{"a", "b"}},
		{"重复去重", `{"data":[{"id":"a"},{"id":"a"}]}`, []string{"a"}},
		{"Ollama 原生", `{"models":[{"name":"llama3"}]}`, []string{"llama3"}},
		{"裸数组", `["x","y"]`, []string{"x", "y"}},
		{"空响应", `{}`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseModelIDs([]byte(c.body))
			if len(got) != len(c.want) {
				t.Fatalf("得到 %v，期望 %v", got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("得到 %v，期望 %v", got, c.want)
				}
			}
		})
	}
}

// ── 连接测试：失败要能归因到具体字段

func TestTestChannelAttributesFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"data":[{"id":"m1"}]}`)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"bad api key"}}`)
	}))
	defer srv.Close()

	up := Upstream{
		Name: "t", Source: SourceEmbedded, Enabled: true,
		BaseURL: srv.URL, Protocol: "chat", APIKeys: []string{"bad"},
		Models: config.ChannelModelOf("m1"), Weight: 1,
	}
	res := TestChannel(context.Background(), up, testLogger(t))

	if res.OK {
		t.Fatal("401 场景不应判定为通过")
	}
	found := false
	for _, f := range res.Fields {
		if f == "api_keys" {
			found = true
		}
	}
	if !found {
		t.Errorf("401 应归因到 api_keys，实际 %v", res.Fields)
	}
	if len(res.Models) != 1 {
		t.Errorf("模型列表应被识别：%v", res.Models)
	}
}

func TestTestChannelRejectsBadURL(t *testing.T) {
	up := Upstream{Name: "t", Source: SourceEmbedded, Enabled: true,
		BaseURL: "ftp://x.com", Protocol: "chat"}
	res := TestChannel(context.Background(), up, testLogger(t))
	if res.OK {
		t.Fatal("非 http(s) 地址应当校验失败")
	}
	if len(res.Steps) == 0 || res.Steps[0].OK {
		t.Fatalf("首步应为地址校验失败：%+v", res.Steps)
	}
}

func TestTestChannelModelsFailureIsNonBlocking(t *testing.T) {
	// 上游没有 /models，但对话可用 —— 不该阻止保存
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"pong"}}]}`)
	}))
	defer srv.Close()

	up := Upstream{
		Name: "t", Source: SourceEmbedded, Enabled: true,
		BaseURL: srv.URL, Protocol: "chat",
		Models: config.ChannelModelOf("m1"), Weight: 1,
	}
	res := TestChannel(context.Background(), up, testLogger(t))
	if !res.OK {
		t.Fatalf("对话可用时不应阻止保存：%+v", res.Steps)
	}
}
