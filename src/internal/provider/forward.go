// forward.go 请求转发主流程。
//
// 三种上游协议在这里分流：
//
//	chat      直通（只改 model 字段，其余字节不动）  ← 主路径，覆盖绝大多数上游
//	anthropic 双向转换（OpenAI Chat ⇄ Anthropic Messages）
//	responses 尚未实现（明确报错，而不是静默走 chat 给出误导性的失败）
package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ErrResponsesUnsupported 上游协议为 Responses 时的明确拒绝。
//
// 刻意不「回落成 chat 再试」：一个只支持 Responses 的上游收到 chat 请求会返回
// 让人摸不着头脑的 400，用户会以为是自己的请求写错了。直接说清「暂不支持」
// 比让他去猜要好。
var ErrResponsesUnsupported = errors.New(
	"上游协议为 OpenAI Responses，Mergence 的该协议适配尚未完成；" +
		"请在渠道设置里改选「Chat Completions」，或改用支持 Chat Completions 的上游")

// maxUpstreamBody 单次上游响应体的读取上限（非流式）。
// 设得比常规响应大得多，避免把长上下文回复截断；同时防止上游异常时吃光内存。
const maxUpstreamBody = 32 << 20

// ChatCall 一次转发调用。
type ChatCall struct {
	// Body 客户端原始请求体，OpenAI Chat Completions 格式，字节级保真。
	Body []byte
	// Stream 客户端是否要求流式。
	Stream bool
	// UpstreamModel 上游真实模型名（已剥离前缀、已解析别名）。
	UpstreamModel string
	// ReqID 链路 ID，贯穿日志。
	ReqID string
}

// ChatResult 转发结果。
type ChatResult struct {
	Status int
	// Body 非流式时的完整响应体（已是 OpenAI Chat Completions 格式）。
	Body []byte
	// Stream 流式时的增量读取器（已是 OpenAI SSE 格式）。
	Stream io.ReadCloser
	// ContentType 上游响应的内容类型。
	ContentType string

	KeyHint   string
	Attempts  int
	LatencyMS int64
	// TotalTokens 从响应里尽力提取的 token 数，仅用于日志展示；取不到为 0。
	TotalTokens int
	// Usage 完整用量（输入/输出/缓存）。取不到时各字段为 0。
	//
	// 与 TotalTokens 并存是因为用途不同：TotalTokens 只为日志里显示一个数，
	// 而 Usage 要用来算费用与缓存命中率——后者必须区分「未命中的输入」
	// 与「命中的输入」，两者单价差一个数量级。
	Usage Usage
}

// Usage 一次调用的 token 用量。
//
// 字段命名对齐 OpenAI 口径，但取值时兼容各家的不同叫法
// （见 parseUsage 的注释）。所有字段都可能为 0：上游没给就是 0，
// 不猜、不用别的字段倒推。
type Usage struct {
	// PromptTokens 输入 token（OpenAI 口径：含缓存命中部分）。
	PromptTokens int64
	// CompletionTokens 输出 token。
	CompletionTokens int64
	// TotalTokens 总量。
	TotalTokens int64
	// CachedTokens 命中缓存的输入 token。
	CachedTokens int64
	// CacheWriteTokens 写入缓存的 token（Anthropic 独有）。
	CacheWriteTokens int64
	// HaveHitMiss 上游是否分别给了命中/未命中两个计数（DeepSeek 口径）。
	HaveHitMiss bool
	// PromptExcludesCache 为 true 表示 PromptTokens **不含**缓存命中部分。
	//
	// 这个区分是必须的，因为两家的 prompt_tokens 口径相反：
	// OpenAI 的 prompt_tokens 含缓存命中，Anthropic 的 input_tokens 不含。
	// 靠「prompt - cached」去拆，只在 OpenAI 口径下成立；
	// 套到 Anthropic 上会算出负数，被夹成 0 后总量凭空少一大截
	// （一个 8k 缓存 + 500 新输入的请求会显示成 500 token）。
	PromptExcludesCache bool
}

// Chat 执行一次转发（含重试与 Key 轮询）。
func (c *Channel) Chat(ctx context.Context, call *ChatCall) (*ChatResult, error) {
	kind := c.Protocol()
	if kind == "responses" {
		return nil, ErrResponsesUnsupported
	}

	attempts := 1 + c.up.RetryCount()
	var lastErr error

	for i := 0; i < attempts; i++ {
		key, allCool, _ := c.pool.Pick()
		if allCool {
			c.lg.Debug("全部 Key 处于冷却，仍发起一次尝试", "attempt", i+1)
		}

		req, err := c.buildUpstreamRequest(ctx, call, key, kind)
		if err != nil {
			return nil, err // 本地构造失败（请求体非法/URL 非法），重试无意义
		}

		start := timeNow()
		resp, err := c.http.Do(req)
		if err != nil {
			c.pool.ReportFailure(key, 0)
			lastErr = err
			if i == attempts-1 || !isRetryableNetErr(err) {
				break
			}
			c.lg.Warn("上游连接失败，准备重试", "attempt", i+1, "err", err.Error())
			continue
		}

		if isRetryableStatus(resp.StatusCode) && i < attempts-1 {
			// 401/403 是 Key 自身的问题。池里没有别的 Key 可换时，重试同一个 Key
			// 只会白等一轮超时——此时把上游的响应**原样交出去**，客户端才能看到
			// 真正的原因（"invalid api key"），而不是我们编的"全部尝试均失败"。
			if isAuthStatus(resp.StatusCode) && c.pool.Len() <= 1 {
				c.pool.ReportFailure(key, resp.StatusCode)
				c.markErr(statusErr(resp.StatusCode).Error())
				return c.buildResult(resp, call, kind, key, i+1, time.Since(start))
			}
			drain(resp.Body)
			c.pool.ReportFailure(key, resp.StatusCode)
			lastErr = statusErr(resp.StatusCode)
			c.lg.Warn("上游返回可重试状态，准备换 Key 重试",
				"attempt", i+1, "status", resp.StatusCode)
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			c.pool.ReportSuccess(key)
			c.markOK(time.Since(start).Milliseconds())
		} else {
			c.pool.ReportFailure(key, resp.StatusCode)
			c.markErr(statusErr(resp.StatusCode).Error())
		}

		return c.buildResult(resp, call, kind, key, i+1, time.Since(start))
	}

	c.markErr(errText(lastErr))
	return nil, fmt.Errorf("全部 %d 次尝试均失败：%w", attempts, lastErr)
}

// buildUpstreamRequest 组装发往上游的请求。
func (c *Channel) buildUpstreamRequest(ctx context.Context, call *ChatCall, key, kind string) (*http.Request, error) {
	var (
		body []byte
		err  error
	)
	switch kind {
	case "anthropic":
		body, err = toAnthropicRequest(call.Body, call.UpstreamModel)
	default: // chat：只换 model，其余字节原样
		body, err = rewriteModel(call.Body, call.UpstreamModel)
	}
	if err != nil {
		return nil, err
	}

	url := c.endpoint(c.protocolPath(kind))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("构造上游请求失败：%w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if call.Stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	c.setAuth(req, key)
	return req, nil
}

// buildResult 依据是否为流式，产出最终结果。
func (c *Channel) buildResult(resp *http.Response, call *ChatCall, kind, key string,
	attempts int, dur time.Duration) (*ChatResult, error) {

	res := &ChatResult{
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		KeyHint:     MaskKey(key),
		Attempts:    attempts,
		LatencyMS:   dur.Milliseconds(),
	}

	// 非 2xx 一律不当流处理：错误体是普通 JSON，交给客户端按错误处理更清晰。
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	if call.Stream && ok {
		switch kind {
		case "anthropic":
			pr, pw := io.Pipe()
			go func() {
				defer pw.Close()
				defer resp.Body.Close()
				if err := anthropicStream(resp.Body, pw, nil, call.UpstreamModel); err != nil {
					c.lg.Warn("转换上游流式响应时中断", "err", err.Error())
				}
			}()
			res.Stream = pr
		default:
			// 直通：上游本来就是 OpenAI SSE，一个字节都不用碰
			res.Stream = resp.Body
		}
		return res, nil
	}

	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBody))
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败：%w", err)
	}

	if kind == "anthropic" {
		converted, cerr := fromAnthropicResponse(raw, call.UpstreamModel)
		if cerr != nil {
			// 转换失败时把上游原始字节吐出去 —— 让用户看到上游真正说了什么，
			// 比返回一个我们自己编的错误有用得多。
			c.lg.Warn("转换 Anthropic 响应失败，已回退为原始响应", "err", cerr.Error())
			res.Body = raw
		} else {
			res.Body = converted
			res.ContentType = "application/json"
		}
	} else {
		res.Body = raw
	}

	if ok {
		res.Usage = parseUsage(res.Body)
		res.TotalTokens = int(res.Usage.TotalTokens)
	}
	return res, nil
}

// rewriteModel 只替换请求体里的 model 字段，其余值保持原样。
//
// 用 map[string]json.RawMessage 而不是 map[string]any：前者把每个字段的原始字节
// 原封不动带过去，后者会经历一次 float64 往返（大整数、高精度小数会变形）。
func rewriteModel(body []byte, model string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON：%w", err)
	}
	mb, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	fields["model"] = mb
	return json.Marshal(fields)
}

// parseUsage 从一个 OpenAI 形状的 JSON 里读出 usage。
//
// 兼容三种上游命名，因为它们给的是同一件事的不同叫法：
//
//	OpenAI    usage.prompt_tokens / completion_tokens / total_tokens
//	          缓存命中在 usage.prompt_tokens_details.cached_tokens（嵌套）
//	Anthropic usage.input_tokens / output_tokens（本进程已转成 OpenAI 形状，
//	          但缓存字段仍需从 cache_read_input_tokens 取）
//	DeepSeek  缓存命中/未命中分开给：prompt_cache_hit_tokens /
//	          prompt_cache_miss_tokens
//
// 只在 OpenAI 形状上做解析：非流式响应到这一步已经是 OpenAI 格式
// （Anthropic 上游走 fromAnthropicResponse 转过），所以一个解析器够用。
func parseUsage(body []byte) Usage {
	var w struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
			InputTokens      int64 `json:"input_tokens"`
			OutputTokens     int64 `json:"output_tokens"`
			// Anthropic 缓存
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			// OpenAI 缓存（嵌套在 details 里）
			PromptTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			// DeepSeek 缓存（平铺，两个数分别给）
			PromptCacheHitTokens  int64 `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTokens int64 `json:"prompt_cache_miss_tokens"`
			// PromptExcludesCache 由 anthropicUsageToOpenAI 写入：
			// 标记这份 usage 的 prompt_tokens 不含缓存部分。
			PromptExcludesCache bool `json:"prompt_excludes_cache"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &w); err != nil || w.Usage == nil {
		return Usage{}
	}
	u := w.Usage

	out := Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
		// 显式声明优先于「有没有 details」——Anthropic 转换后的响应
		// 也有 prompt_tokens_details，但它是不含缓存的另一种口径。
		PromptExcludesCache: u.PromptExcludesCache,
	}
	switch {
	case u.PromptTokensDetails != nil:
		// OpenAI 口径：prompt_tokens 含缓存命中部分。
		// PromptExcludesCache 已在初始化时按响应里的显式声明设好，不在这里覆盖。
		out.CachedTokens = u.PromptTokensDetails.CachedTokens
	case u.PromptCacheHitTokens > 0 || u.PromptCacheMissTokens > 0:
		// DeepSeek 口径：命中与未命中分开给，两者之和才是输入总量。
		out.CachedTokens = u.PromptCacheHitTokens
		out.PromptTokens = u.PromptCacheHitTokens + u.PromptCacheMissTokens
		out.HaveHitMiss = true
	case u.CacheReadInputTokens > 0:
		// Anthropic 原始口径（未经转换，例如流式透传）：input_tokens 不含缓存读。
		out.CachedTokens = u.CacheReadInputTokens
		out.PromptExcludesCache = true
	}
	// Anthropic 上游转过来的响应里输入输出用的是 input/output_tokens。
	if out.PromptTokens == 0 {
		out.PromptTokens = u.InputTokens
	}
	if out.CompletionTokens == 0 {
		out.CompletionTokens = u.OutputTokens
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.PromptTokens + out.CompletionTokens
		if out.PromptExcludesCache {
			out.TotalTokens += out.CachedTokens
		}
	}
	return out
}

// StreamUsageScanner 边转发边从 SSE 字节流里捞 usage。
//
// 为什么不整段缓冲后再解析：流式响应可能几十分钟、几十兆，
// 全缓冲等于把内存占满；而 usage 几乎总在最后几个 chunk 里，
// 所以只需要保留「最近若干字节」的滑动窗口。
//
// 只读不改：转发路径上一个字节都不动，扫描是纯粹的旁路观察。
type StreamUsageScanner struct {
	window []byte
	got    Usage
	found  bool
}

// streamScanWindow 保留的字节数。
//
// 一个 usage chunk 通常不到 1KB；留 64KB 足以覆盖被网络切碎的大 chunk，
// 同时把内存占用固定住。
const streamScanWindow = 64 << 10

// Feed 喂入一段转发出去的字节。
func (s *StreamUsageScanner) Feed(chunk []byte) {
	if s.found {
		return // 已拿到就不用再扫了
	}
	s.window = append(s.window, chunk...)
	if len(s.window) > streamScanWindow {
		s.window = append([]byte(nil), s.window[len(s.window)-streamScanWindow:]...)
	}
	// 试解析：命中就固化，之后不再扫描。
	if u := parseUsage(s.lastData()); u.TotalTokens > 0 {
		s.got, s.found = u, true
		s.window = nil
	}
}

// lastData 取出窗口里最后一条完整 SSE data 负载。
//
// 只取最后一条而不是全部：usage 出现在流末尾，早期的 chunk 只有 delta。
// 若最后一条不是完整事件（被窗口截断），逐条向前找。
func (s *StreamUsageScanner) lastData() []byte {
	data := s.window
	// 从后往前找 "data:" 起点。
	for i := len(data) - 1; i >= 0; i-- {
		if !bytes.HasPrefix(data[i:], dataPrefix) {
			continue
		}
		payload := data[i+len(dataPrefix):]
		if j := bytes.IndexByte(payload, '\n'); j >= 0 {
			payload = payload[:j]
		}
		payload = bytes.TrimSpace(payload)
		if len(payload) > 0 && !bytes.Equal(payload, sseDone) {
			return payload
		}
	}
	return nil
}

var (
	dataPrefix = []byte("data:")
	sseDone    = []byte("[DONE]")
)

// Usage 返回扫到的用量。
func (s *StreamUsageScanner) Usage() Usage { return s.got }

// isRetryableStatus 判断某状态码是否值得换 Key 重试。
//
// 401/403/429 必须重试：多 Key 的意义正在于此——一个 Key 被限流或失效时，
// 另一个 Key 往往还能用。400/404/413/422 是请求本身的问题，重试纯属浪费。
func isRetryableStatus(code int) bool {
	switch {
	case code == 401 || code == 403 || code == 429:
		return true
	case code == 408 || code == 409:
		return true
	case code >= 500:
		return true
	default:
		return false
	}
}

// isRetryableNetErr 判断网络错误是否值得重试。
//
// 上下文取消（用户主动断开）不重试——继续打上游只会浪费额度。
func isRetryableNetErr(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr)
}

func drain(rc io.ReadCloser) {
	if rc == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
	_ = rc.Close()
}

func errText(err error) string {
	if err == nil {
		return "未知错误"
	}
	return err.Error()
}

// isAuthStatus 判断是否「Key 自身有问题」的状态码。
//
// 与 isRetryableStatus 的区别：429/5xx 换 Key 有用，400/404 换了也没用，
// 而 401/403 只有在池里还有别的 Key 时才值得重试。
func isAuthStatus(code int) bool {
	return code == 401 || code == 403
}

func statusErr(code int) error {
	return fmt.Errorf("上游返回 HTTP %d", code)
}
