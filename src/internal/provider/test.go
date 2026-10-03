// test.go 渠道连接测试。
//
// 用途是「保存前把关」：一个 Key 填错、URL 多打个斜杠的渠道如果被存下来，
// 用户会在很久以后某次调用失败时才发现。所以这里跑一套真实请求，
// 并把失败**归因到具体表单字段**，让面板能直接把出错的输入框标红。
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"modelmux/internal/logging"
)

// TestStep 一步测试的结果。
type TestStep struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	// Blocking=false 表示这步失败不阻止保存（例如上游没实现 /models）。
	Blocking  bool   `json:"blocking"`
	Detail    string `json:"detail,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
}

// TestResult 整体测试结果。
type TestResult struct {
	OK     bool       `json:"ok"`
	Steps  []TestStep `json:"steps"`
	Models []string   `json:"models,omitempty"`
	// Fields 需要高亮的表单字段名：base_url / api_keys / protocol / model。
	Fields []string `json:"fields,omitempty"`
}

// TestChannel 对一份尚未保存的渠道配置跑连接测试。
func TestChannel(ctx context.Context, cfg Upstream, lg *logging.Logger) *TestResult {
	res := &TestResult{}

	// ── 步骤 1：基础校验（本地，无网络）
	if step, fields := validateBasic(cfg); !step.OK {
		res.Steps = append(res.Steps, step)
		res.Fields = append(res.Fields, fields...)
		return res
	} else {
		res.Steps = append(res.Steps, step)
	}

	switch cfg.Protocol {
	case "responses":
		res.Steps = append(res.Steps, TestStep{
			Name: "协议支持", OK: false, Blocking: true,
			Detail: ErrResponsesUnsupported.Error(),
		})
		res.Fields = append(res.Fields, "protocol")
		return res
	default:
		res.Steps = append(res.Steps, TestStep{
			Name: "协议支持", OK: true, Blocking: true,
			Detail: "上游协议 " + cfg.Protocol + " 由适配层接收",
		})
	}

	ch, err := newChannel(cfg, lg, nil)
	if err != nil {
		res.Steps = append(res.Steps, TestStep{Name: "构造客户端", OK: false, Blocking: true, Detail: err.Error()})
		res.Fields = append(res.Fields, "base_url")
		return res
	}

	// ── 步骤 2：拉模型列表（失败不阻断——有些上游确实没有 /models）
	{
		start := timeNow()
		ids, ferr := ch.FetchModels(ctx)
		lat := time.Since(start).Milliseconds()
		if ferr != nil {
			res.Steps = append(res.Steps, TestStep{
				Name: "拉取模型列表", OK: false, Blocking: false,
				Detail:    ferr.Error() + "（若该上游确实不提供模型列表，可改用「手动填写模型」后继续）",
				LatencyMS: lat,
			})
			res.Fields = appendUnique(res.Fields, "base_url")
		} else {
			res.Models = ids
			res.Steps = append(res.Steps, TestStep{
				Name: "拉取模型列表", OK: true, Blocking: false,
				Detail: fmt.Sprintf("识别到 %d 个模型", len(ids)), LatencyMS: lat,
			})
		}
	}

	// ── 步骤 3：跑一次真实对话（这一步决定能不能保存）
	probe := probeModel(cfg, res.Models)
	if probe == "" {
		res.Steps = append(res.Steps, TestStep{
			Name: "试跑对话", OK: false, Blocking: true,
			Detail: "没有可用于试跑的模型：上游未返回模型列表，且渠道里也没有手工填写模型",
		})
		res.Fields = appendUnique(res.Fields, "models")
		res.OK = false
		return res
	}

	{
		body, _ := json.Marshal(map[string]any{
			"model":      probe,
			"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
			"max_tokens": 1,
			"stream":     false,
		})
		start := timeNow()
		call := &ChatCall{Body: body, Stream: false, UpstreamModel: probe, ReqID: logging.NewReqID()}
		out, cerr := ch.Chat(ctx, call)
		lat := time.Since(start).Milliseconds()

		switch {
		case cerr != nil:
			res.Steps = append(res.Steps, TestStep{
				Name: "试跑对话", OK: false, Blocking: true,
				Detail: "调用失败：" + cerr.Error(), LatencyMS: lat,
			})
			res.Fields = appendUnique(res.Fields, "base_url")
			res.OK = false
		case out.Status >= 200 && out.Status < 300:
			res.Steps = append(res.Steps, TestStep{
				Name: "试跑对话", OK: true, Blocking: true,
				Detail: fmt.Sprintf("模型 %s 正常响应", probe), LatencyMS: lat,
			})
			res.OK = true
		default:
			detail := fmt.Sprintf("上游返回 HTTP %d", out.Status)
			if msg := extractUpstreamMessage(out.Body); msg != "" {
				detail += "：" + msg
			}
			res.Steps = append(res.Steps, TestStep{
				Name: "试跑对话", OK: false, Blocking: true, Detail: detail, LatencyMS: lat,
			})
			res.Fields = appendUnique(res.Fields, attributeFields(out.Status)...)
			res.OK = false
		}
	}
	return res
}

// validateBasic 纯本地校验，避免把明显写错的地址真发出去。
func validateBasic(cfg Upstream) (TestStep, []string) {
	raw := strings.TrimSpace(cfg.BaseURL)
	if raw == "" {
		return TestStep{Name: "地址校验", OK: false, Blocking: true, Detail: "Base URL 为空"},
			[]string{"base_url"}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return TestStep{Name: "地址校验", OK: false, Blocking: true,
			Detail: "Base URL 无法解析：" + err.Error()}, []string{"base_url"}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return TestStep{Name: "地址校验", OK: false, Blocking: true,
			Detail: "Base URL 必须以 http:// 或 https:// 开头"}, []string{"base_url"}
	}
	if u.Host == "" {
		return TestStep{Name: "地址校验", OK: false, Blocking: true,
			Detail: "Base URL 缺少主机名"}, []string{"base_url"}
	}
	host := u.Hostname()
	if host == "0.0.0.0" || host == "::" {
		return TestStep{Name: "地址校验", OK: false, Blocking: true,
			Detail: "0.0.0.0 是监听地址、不能作为请求目标，请改成 127.0.0.1"}, []string{"base_url"}
	}

	detail := "地址合法"
	if len(cfg.APIKeys) == 0 {
		if isLocalHost(host) {
			detail = "地址合法；本地端点未填 Key（可接受）"
		} else {
			detail = "地址合法；但未填 API Key，若上游要求鉴权会返回 401"
		}
	}
	return TestStep{Name: "地址校验", OK: true, Blocking: true, Detail: detail}, nil
}

// probeModel 选出用于试跑的模型：优先用户已声明的，其次上游刚返回的第一个。
func probeModel(cfg Upstream, fetched []string) string {
	if len(cfg.Models) > 0 {
		return cfg.Models[0].ID
	}
	if len(fetched) > 0 {
		return fetched[0]
	}
	return ""
}

// attributeFields 把 HTTP 状态码归因到表单字段，供面板高亮。
func attributeFields(status int) []string {
	switch {
	case status == 401 || status == 403:
		return []string{"api_keys"}
	case status == 404:
		return []string{"base_url", "models"}
	case status == 429:
		return []string{"api_keys"}
	case status >= 500:
		return []string{"base_url"}
	case status >= 400:
		return []string{"models", "protocol"}
	default:
		return nil
	}
}

// extractUpstreamMessage 从错误响应里挖出可读的原因（兼容 OpenAI 与 Anthropic 两种错误形状）。
func extractUpstreamMessage(body []byte) string {
	var w struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return truncate(string(body), 200)
	}
	switch {
	case w.Error.Message != "":
		return truncate(w.Error.Message, 300)
	case w.Message != "":
		return truncate(w.Message, 300)
	default:
		return truncate(string(body), 200)
	}
}

func isLocalHost(h string) bool {
	if h == "localhost" || h == "127.0.0.1" || h == "::1" {
		return true
	}
	return strings.HasPrefix(h, "192.168.") || strings.HasPrefix(h, "10.") ||
		strings.HasPrefix(h, "172.16.") || strings.HasSuffix(h, ".local")
}

func appendUnique(s []string, vals ...string) []string {
	for _, v := range vals {
		found := false
		for _, x := range s {
			if x == v {
				found = true
				break
			}
		}
		if !found {
			s = append(s, v)
		}
	}
	return s
}
