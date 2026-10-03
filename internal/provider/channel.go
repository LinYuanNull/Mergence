// channel.go 一个可路由上游的运行时。
//
// Channel = Upstream（配置形状）+ 运行时状态（Key 池、HTTP 客户端、累计计数）。
// 配置变更走 Registry.Sync 整体替换，但**同一渠道的 Key 池会被沿用**——
// 否则用户每次在面板上点保存，刚积累的 Key 冷却状态就全丢了，
// 表现为「刚被限流的 Key 马上又被拿去撞一遍」。
package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"modelmux/internal/config"
	"modelmux/internal/logging"
)

// Channel 单个可路由上游的运行时。
type Channel struct {
	up   Upstream
	pool *KeyPool
	http *http.Client
	lg   *logging.Logger

	mu       sync.Mutex
	lastErr  string
	lastUsed time.Time
	reqOK    int64
	reqErr   int64
	lastMS   int64

	// modelCache 上游模型目录缓存（FetchModels 结果）。
	// 它不写回配置 —— 只有用户显式保存才改配置。但对「自动」模式的渠道，
	// /v1/models 会用它兜底，这样托管型子进程起来后模型列表能自己出现。
	modelCache   []string
	cacheAt      time.Time
	cacheTriedAt time.Time

	// 控制台形态探测结果（见 ProbeConsole）。缓存的是「这个渠道的管理 API
	// 能不能被集成面板直接用」，判错也只影响侧栏列出哪些入口，不碰转发。
	consoleKind     ConsoleKind
	consoleProbedAt time.Time
	consoleProbing  bool
}

// ConsoleKind 托管渠道的控制台形态。
type ConsoleKind string

const (
	// ConsoleUnknown 尚未探测出结果（子进程可能还在起）。
	ConsoleUnknown ConsoleKind = ""
	// ConsoleGateway 管理 API 与集成面板兼容（wb2api 那套 /panel/api/*），
	// 账号池 / 任务 / 模型 / 用量等视图可以直接嵌进 ModelMux。
	ConsoleGateway ConsoleKind = "gateway"
	// ConsoleZcode zcode2api 系列网关：管理 API 在 /admin/api，与集成面板不兼容，
	// 但有**专属适配**（面板代理 + 原生视图），因此不再退化成外链。
	//
	// 与 ConsoleGateway 的区别：ConsoleGateway 的上游接口签名和集成面板一致，
	// 同一套视图能直接复用；ConsoleZcode 的结构完全不同，得走单独的原生视图。
	// 与 ConsoleWeb 的区别：ConsoleWeb 是「接不进来」，只给一个打开自带面板的
	// 外链；ConsoleZcode 有 ModelMux 内的专属视图与代理，不是外链。
	ConsoleZcode ConsoleKind = "zcode"
	// ConsoleWeb 只有自带网页面板，集成面板只给一个外链入口。
	ConsoleWeb ConsoleKind = "web"
	// ConsoleNone 没有可接入的控制台。
	ConsoleNone ConsoleKind = "none"
)

// 控制台形态的探测节奏。渠道可能在运行中被改成指向别的实现，
// 所以不能只探一次；但也没必要每次都问，5 分钟足够。
const (
	consoleProbeTTL     = 5 * time.Minute
	consoleProbeTimeout = 3 * time.Second
)

// NewTempChannel 用一份尚未保存的配置构造临时渠道（不注册进 Registry）。
//
// 用途是「保存前先试」：面板可以在用户还没点保存时，就用当前表单值去拉模型列表
// 或跑一次真实对话。临时实例的 Key 池是独立的，不会污染已保存渠道的冷却状态。
func NewTempChannel(up Upstream, lg *logging.Logger) (*Channel, error) {
	return newChannel(up, lg, nil)
}

// newChannel 依据上游定义构造渠道。prev 非空且 Key 集合未变时沿用其 Key 池。
func newChannel(up Upstream, lg *logging.Logger, prev *Channel) (*Channel, error) {
	c := &Channel{up: up, lg: lg.WithProvider(up.Name)}

	if prev != nil && prev.pool != nil && sameKeys(prev.up.APIKeys, up.APIKeys) {
		c.pool = prev.pool
		c.modelCache, c.cacheAt = prev.modelCache, prev.cacheAt
	} else {
		c.pool = NewKeyPool(up.APIKeys)
	}

	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second, // 约束到首字节；对长流式安全
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: 2 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	if p := strings.TrimSpace(up.Proxy); p != "" {
		pu, err := url.Parse(p)
		if err != nil {
			return nil, fmt.Errorf("代理地址 %q 无法解析：%w", p, err)
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	// 不设 Client.Timeout：它会把长流式一起掐断。非流式改用 context 超时控制。
	c.http = &http.Client{Transport: tr}

	return c, nil
}

// Upstream 返回上游定义快照。
func (c *Channel) Upstream() Upstream { return c.up }

// Name 渠道标识。
func (c *Channel) Name() string { return c.up.Name }

// Prefix 对外模型名前缀。
func (c *Channel) Prefix() string { return c.up.ModelPrefix }

// Protocol 上游协议。
func (c *Channel) Protocol() string { return c.up.Protocol }

// Source 来源（内嵌 / 托管）。
func (c *Channel) Source() Source { return c.up.Source }

// Ready 当前是否可用于转发。
func (c *Channel) Ready() bool { return c.up.IsReady() }

// unreadyReason 给出「为什么不能路由到它」的可读原因。
//
// 区分「配置里停用」与「临时未就绪」很重要：前者要用户去改配置，
// 后者等一会儿就好，把两者混成一句话会让人做无用功。
func (c *Channel) unreadyReason() string {
	if !c.up.Enabled {
		return "已在配置中停用"
	}
	if r := c.up.Reason(); r != "" {
		return r
	}
	return "当前不可用"
}

// KeyStats Key 池状态。
func (c *Channel) KeyStats() []KeyStat { return c.pool.Stats() }

// upstreamFor 把「对外模型短名」反查成上游真实模型名。
//
// 先用别名表反查（用户显式映射过，或展示名经过规范化），再退化为原样透传 ——
// 透传是刻意的：上游随时可能上新模型，不该逼用户先改配置才能用。
//
// 查的是 effectiveModels 而不是 c.up.Models：自动模式下模型只存在于
// modelCache 里，而它们的对外名是规范化过的展示名。反查必须能命中它，
// 否则用户拿 /v1/models 里看到的名字调用会直接 404。
// 展示名与真实 ID 都比一遍：客户端从别处抄来的真实 ID 也能命中。
func (c *Channel) upstreamFor(short string) (string, bool) {
	for _, m := range c.effectiveModels() {
		if m.Outward() == short || m.ID == short {
			return m.ID, true
		}
	}
	return short, false
}

// upstreamByDeclared 只在声明列表里找（用于「不带前缀」的精确匹配）。
//
// 同样走 effectiveModels：自动模式下模型在 modelCache 里，
// 客户端从 /v1/models 拿到的展示名（规范化过）也要能反查到真实 ID。
// 展示名与真实 ID 都比一遍，两种写法都认。
func (c *Channel) upstreamByDeclared(model string) (string, bool) {
	for _, m := range c.effectiveModels() {
		if m.Outward() == model || m.ID == model {
			return m.ID, true
		}
	}
	return "", false
}

// ModelsPath 模型列表路径。构造 Upstream 时已保证非空。
func (c *Channel) ModelsPath() string {
	if c.up.ModelsPath != "" {
		return c.up.ModelsPath
	}
	return defaultModelsPath
}

// endpoint 拼接上游完整 URL。
func (c *Channel) endpoint(path string) string {
	return c.up.BaseURL + path
}

// protocolPath 返回某协议在 Chat Completions 语义下对应的上游路径。
func (c *Channel) protocolPath(kind string) string {
	switch kind {
	case "anthropic":
		return "/v1/messages"
	case "responses":
		return "/responses"
	default:
		return "/chat/completions"
	}
}

// setAuth 按协议写鉴权头，最后应用用户自定义头（允许覆盖默认鉴权方式）。
func (c *Channel) setAuth(req *http.Request, key string) {
	switch c.Protocol() {
	case "anthropic":
		if key != "" {
			req.Header.Set("x-api-key", key)
		}
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	default:
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	for k, v := range c.up.Headers {
		req.Header.Set(k, v)
	}
}

// markOK / markErr 记录最近一次调用结果，用于面板与状态展示。
func (c *Channel) markOK(ms int64) {
	c.mu.Lock()
	c.reqOK++
	c.lastMS = ms
	c.lastErr = ""
	c.lastUsed = time.Now()
	c.mu.Unlock()
}

func (c *Channel) markErr(msg string) {
	c.mu.Lock()
	c.reqErr++
	c.lastErr = msg
	c.lastUsed = time.Now()
	c.mu.Unlock()
}

// ChannelStatus 面板用的渠道状态。
type ChannelStatus struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Source      Source `json:"source"`
	Enabled     bool   `json:"enabled"`
	// Ready 当前是否可用（托管型子进程没起来时为 false）。
	Ready       bool   `json:"ready"`
	ReadyReason string `json:"ready_reason,omitempty"`
	Preset      string `json:"preset,omitempty"`
	BaseURL     string `json:"base_url"`
	PanelURL    string `json:"panel_url,omitempty"`
	// ConsoleKind 控制台形态：gateway（可嵌入整套视图）/ web（只有外链）/
	// none / 空（还在探测）。前端据此决定侧栏给这个渠道列哪些入口。
	ConsoleKind ConsoleKind `json:"console_kind,omitempty"`
	// CreditValue 每积分价值（元），积分型平台的花费估算用。
	// 已含平台预设的默认价（用户没在渠道设置里填时用它兜底）。
	//
	// 由 web 层填充而非 provider 自算：这个值是「配置值 || 平台默认价」，
	// 而判断属于哪个平台要靠名称/命令探测（老配置里没存 preset），
	// 探测逻辑在 web 层。放在这个结构里是因为它本就是面板 DTO。
	CreditValue float64 `json:"credit_value,omitempty"`
	// CreditValueDefault 上面的值来自平台默认价而非用户配置——
	// 面板据此把来源写清楚，免得用户以为是自己填的。
	CreditValueDefault bool   `json:"credit_value_default,omitempty"`
	Protocol           string `json:"protocol"`
	ModelPrefix        string `json:"model_prefix"`
	ModelSource        string `json:"model_source"`

	// AccountCount 托管渠道在上游账号池里的账号数。
	//
	// 为什么要它：账号是**上游子进程持有的运行时数据**（ModelMux 侧不落库），
	// 而界面要按「有没有账号」决定平台是否可见。这个值探测不到时是 -1，
	// 含义是「还不知道」——前端据此留空而不是当成 0 把平台藏掉。
	AccountCount int `json:"account_count"`

	ModelCount int                   `json:"model_count"`
	Models     []config.ChannelModel `json:"models"`
	KeyCount   int                   `json:"key_count"`
	Keys       []KeyStat             `json:"keys"`

	Weight     int               `json:"weight"`
	Priority   int               `json:"priority"`
	Timeout    string            `json:"timeout"`
	Retries    int               `json:"retries"`
	Headers    map[string]string `json:"headers,omitempty"`
	Proxy      string            `json:"proxy,omitempty"`
	HealthPath string            `json:"health_path"`
	ModelsPath string            `json:"models_path"`

	ReqOK   int64     `json:"req_ok"`
	ReqErr  int64     `json:"req_err"`
	LastMS  int64     `json:"last_ms"`
	LastErr string    `json:"last_err,omitempty"`
	UsedAt  time.Time `json:"used_at,omitempty"`

	// Warning 非阻断性提示。
	Warning string `json:"warning,omitempty"`
}

// Status 汇总渠道状态。
func (c *Channel) Status() ChannelStatus {
	c.mu.Lock()
	st := ChannelStatus{
		ReqOK: c.reqOK, ReqErr: c.reqErr, LastMS: c.lastMS,
		LastErr: c.lastErr, UsedAt: c.lastUsed,
	}
	c.mu.Unlock()

	st.Name = c.up.Name
	st.DisplayName = c.up.DisplayName
	st.Source = c.up.Source
	st.Enabled = c.up.Enabled
	st.Ready = c.up.IsReady()
	if !st.Ready {
		st.ReadyReason = c.unreadyReason()
	}
	st.Preset = c.up.Preset
	st.BaseURL = c.up.BaseURL
	st.PanelURL = c.up.PanelURL
	c.mu.Lock()
	st.ConsoleKind = c.consoleKind
	c.mu.Unlock()
	st.Protocol = c.Protocol()
	st.ModelPrefix = c.up.ModelPrefix
	st.ModelSource = c.up.ModelSource
	// 用 effectiveModels 而不是 c.up.Models：托管型渠道通常不声明模型，
	// 靠子进程起来后自动拉取。只读声明值会让面板显示「0 个模型」并误报
	// 「尚未配置模型」，而实际上 /v1/models 里明明列着它们。
	eff := c.effectiveModels()
	st.Models = eff
	st.ModelCount = len(eff)
	st.KeyCount = c.pool.Len()
	st.Keys = c.pool.Stats()
	st.Weight = c.up.Weight
	st.Priority = c.up.Priority
	st.Timeout = c.up.TimeoutStr
	st.Retries = c.up.Retries
	st.Headers = c.up.Headers
	st.Proxy = c.up.Proxy
	st.HealthPath = c.up.HealthPath
	st.ModelsPath = c.up.ModelsPath

	switch {
	case !c.up.Enabled:
		st.Warning = "已停用"
	case !st.Ready:
		// 原因已在 ReadyReason 里，不重复提示
	case st.ModelCount == 0:
		st.Warning = "尚未配置模型，/v1/models 不会列出该渠道"
	case st.KeyCount == 0 && c.up.Source == SourceEmbedded:
		st.Warning = "未填 API Key（本地端点可忽略）"
	}
	return st
}

// FetchModels 拉取上游模型列表。
func (c *Channel) FetchModels(ctx context.Context) ([]string, error) {
	key, allCool, _ := c.pool.Pick()
	_ = allCool

	path := c.ModelsPath()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint(path), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	c.setAuth(req, key)

	client := *c.http
	client.Timeout = 30 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 %s 失败：%w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败：%w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.pool.ReportFailure(key, resp.StatusCode)
		return nil, fmt.Errorf("上游返回 HTTP %d：%s", resp.StatusCode, truncate(string(body), 300))
	}
	c.pool.ReportSuccess(key)

	ids := parseModelIDs(body)
	if len(ids) == 0 {
		return nil, fmt.Errorf("上游返回成功但未解析出任何模型（响应片段：%s）", truncate(string(body), 200))
	}

	c.mu.Lock()
	c.modelCache, c.cacheAt = ids, time.Now()
	c.mu.Unlock()
	return ids, nil
}

// effectiveModels 返回该渠道对外提供的模型。
//
// 声明了模型就用声明的；没声明但拉到过上游目录时用目录（仅内存，不写配置）。
// 这样「自动」模式的渠道不必先让用户点一次保存才能出现在 /v1/models 里。
//
// 拉取到的目录会过一次 NormalizeModelName：**ID 保持上游原样**（转发要用），
// **Alias 存规范化后的展示名**。这样 /v1/models 列出的是干净名字，
// 而 upstreamFor 能把展示名反查回真实 ID，转发路径不受影响。
func (c *Channel) effectiveModels() []config.ChannelModel {
	if len(c.up.Models) > 0 {
		// 显式声明的模型也要重算展示名。
		//
		// 为什么不在拉取时算一次就存下来：规则本身会变——`hy3-x` 后来被
		// 认定为免费档、`minimax-m3` 的 M 改成全大写。存下来的 alias 会随
		// 规则一起过期，于是同一个模型在不同渠道显示成不同名字，
		// 那正是要消除的问题。
		out := make([]config.ChannelModel, 0, len(c.up.Models))
		for _, m := range c.up.Models {
			out = append(out, displayModel(m))
		}
		return out
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.modelCache) == 0 {
		return nil
	}
	out := make([]config.ChannelModel, 0, len(c.modelCache))
	for _, id := range c.modelCache {
		m := config.ChannelModel{ID: id}
		if display := NormalizeModelName(id); display != id {
			m.Alias = display
		}
		out = append(out, m)
	}
	return out
}

// displayModel 按当前命名规则重算展示名。
//
// 判据是「alias 有没有承载额外信息」：
//   - alias 只是真实 ID 的另一种写法（大小写 / 分隔符 / 区域前缀不同，
//     如 `cn:minimax-m3` 配 alias `minimax-m3`）→ 那是按旧规则自动写入的，
//     没有额外信息，用当前规则重算。
//   - alias 与 ID 有实质差异（如 `glm-4.6` 配 alias `glm`）→ 用户刻意指定的
//     对外短名，保留不动。
//
// 两头都不能省：无条件用规则会抹掉用户的短名；无条件用 alias 则规则一改
// 界面不跟着变——这正是 `minimax-m3` 停在旧名字上的原因。
func displayModel(m config.ChannelModel) config.ChannelModel {
	d := NormalizeModelName(m.ID)
	if d == m.ID {
		return m // 规则认不出这个 ID，alias 是唯一线索
	}
	if a := strings.TrimSpace(m.Alias); a != "" && !sameNameVariant(a, m.ID) {
		return m // 自定义对外名
	}
	m.Alias = d
	return m
}

// ProbeConsole 在后台探测一次控制台形态（带 TTL，重复调用很便宜）。
//
// 为什么不按渠道名或预设判断：各 2api 实现的管理 API 路径与结构都不一样
// （wb2api 在 /panel/api 下有一整套与集成面板兼容的接口，zcode2api 只有
// /admin/api 且签名完全不同），而渠道配置里连创建时用的模板都可能没记。
// 与其猜名字，不如直接问「集成面板要用的那个接口在不在」。
func (c *Channel) ProbeConsole() {
	if c.up.Source != SourceManaged {
		return
	}
	c.mu.Lock()
	if c.consoleProbing || time.Since(c.consoleProbedAt) < consoleProbeTTL {
		c.mu.Unlock()
		return
	}
	c.consoleProbing = true
	c.mu.Unlock()

	go func() {
		kind := c.probeConsole()
		c.mu.Lock()
		// 只有「真的探到了结论」才固化时间戳。
		// 探不通（ConsoleUnknown）时必须让 TTL 立即过期，否则启动瞬间那次失败
		// 会被当成结论缓存 5 分钟，子进程起来后入口仍然是空的。
		if kind != ConsoleUnknown {
			c.consoleKind = kind
			c.consoleProbedAt = time.Now()
		} else {
			// 把时间戳清零：下次ProbeConsole 立刻可以再探。
			c.consoleProbedAt = time.Time{}
		}
		c.consoleProbing = false
		c.mu.Unlock()
	}()
}

func (c *Channel) probeConsole() ConsoleKind {
	// 子进程没起来时接口必然不通，但这是「暂时」而不是「不支持」。
	//
	// 这里**刻意不返回缓存的上一次结论**，而是回ConsoleUnknown 并让调用方
	// 记下「尚未探测」。原因：ConsoleNone/ConsoleWeb 都只在**真的发出请求并
	// 拿到响应**时才可能得出，而启动瞬间探测必然失败（子进程还在拉起）。
	// 若把那次失败当结论缓存住，5 分钟内子进程起来了也不会重探，
	// 表现为「WorkBuddy 控制台入口空了 5 分钟」——启动时序越靠前越容易踩到。
	//
	// 判据统一交给响应本身：探不通就是探不通，不去猜、不固化否定结论。
	if !c.up.IsReady() {
		return ConsoleUnknown
	}

	root := strings.TrimRight(c.up.RootURL, "/")
	prefix := strings.TrimRight(c.up.PanelAPIPrefix, "/")
	if root != "" && prefix != "" {
		ctx, cancel := context.WithTimeout(context.Background(), consoleProbeTimeout)
		defer cancel()
		// 探的是集成面板真正会调的第一个接口：模型列表。
		// wb2api 有、zcode2api 没有（它只有 /admin/api），一个请求就能分开。
		if req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			root+prefix+"/models", nil); err == nil {
			if len(c.up.APIKeys) > 0 && c.up.APIKeys[0] != "" {
				req.Header.Set("Authorization", "Bearer "+c.up.APIKeys[0])
			}
			if resp, err := c.http.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return ConsoleGateway
				}
			}
		}
	}
	if c.up.PanelURL != "" {
		return ConsoleWeb
	}
	return ConsoleNone
}

// shouldPrefetch 判断是否该为它拉一次模型目录。会顺带记录尝试时间，
// 避免上游不可达时每 2 秒重试一次（那会把日志刷满）。
func (c *Channel) shouldPrefetch(now time.Time) bool {
	if c.up.ModelSource != "auto" || len(c.up.Models) > 0 || !c.Ready() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Sub(c.cacheTriedAt) < 5*time.Minute {
		return false
	}
	c.cacheTriedAt = now
	return true
}

// CachedModels 返回上次拉取的模型目录（可能为空）。
func (c *Channel) CachedModels() ([]string, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.modelCache, c.cacheAt
}

// parseModelIDs 兼容多种模型列表响应形态：
//
//	{"data":[{"id":"x"}]}       OpenAI / OpenRouter / Anthropic
//	{"models":[{"name":"x"}]}   Ollama 原生
//	["x","y"]                   裸数组
func parseModelIDs(body []byte) []string {
	var wrap struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
		Models []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"models"`
	}
	var out []string
	if err := json.Unmarshal(body, &wrap); err == nil {
		for _, m := range wrap.Data {
			if s := pickModelName(m.ID, m.Name); s != "" {
				out = append(out, s)
			}
		}
		for _, m := range wrap.Models {
			if s := pickModelName(m.ID, m.Name); s != "" {
				out = append(out, s)
			}
		}
	}
	if len(out) > 0 {
		return dedupeStrings(out)
	}

	var bare []string
	if err := json.Unmarshal(body, &bare); err == nil {
		return dedupeStrings(bare)
	}
	var bareObj []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &bareObj); err == nil {
		for _, m := range bareObj {
			if s := pickModelName(m.ID, m.Name); s != "" {
				out = append(out, s)
			}
		}
	}
	return dedupeStrings(out)
}

func pickModelName(id, name string) string {
	if s := strings.TrimSpace(id); s != "" {
		return s
	}
	return strings.TrimSpace(name)
}

func dedupeStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func sameKeys(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "…"
}
