// upstream.go 可路由上游的统一形状。
//
// 三种来源在这里归一成同一个形状：
//
//	内嵌型 embedded —— ModelMux 进程内直连一个 OpenAI 兼容端点（纯转发）
//	托管型 managed  —— ModelMux 拉起的独立子进程
//	原生型 native   —— ModelMux 在本进程内装配上游实现并服务（见 internal/native）
//
// 后两者都跑在本机回环上、都自带管理面，只差「谁提供服务」——那是编排层的事，
// 到了这一层已经看不出区别（见 Source.Hosted）。于是路由、多 Key 池、协议适配、
// 模型聚合、连接测试、额度查询全都只有一份实现。
//
// 这是「不让 WorkBuddy / new-api 的源码被改写成库」的前提：它们本来就是独立可用的
// OpenAI 兼容服务，ModelMux 只需要知道「叫什么、前缀是什么、怎么鉴权」。
package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"modelmux/internal/config"
)

// defaultModelsPath 模型列表路径（相对 API 前缀）。
const defaultModelsPath = "/models"

// defaultAPIPrefix 托管型上游的默认 API 路径前缀。
const defaultAPIPrefix = "/v1"

// Source 渠道来源。
type Source string

const (
	// SourceEmbedded 内嵌型：ModelMux 直接向上游发请求。
	SourceEmbedded Source = "embedded"
	// SourceManaged 托管型：上游是 ModelMux 拉起的独立子进程。
	SourceManaged Source = "managed"
	// SourceNative 原生型：上游由 ModelMux **在本进程内**装配并服务。
	//
	// 对外它仍然是一个回环 HTTP 端点（见 internal/native），所以与托管型相比
	// 只差「谁提供服务」这一件事，而这件事完全由编排层吸收：两者都给得出
	// RootURL、管理 API 前缀与就绪状态。因此管理代理、领取执行器、用量采集、
	// 控制台探测对它们一视同仁——判据是 Hosted()，不是 == SourceManaged。
	SourceNative Source = "native"
)

// Hosted 该来源是否由 ModelMux 提供、跑在本机回环上、且自带管理面。
//
// 托管型（子进程）与原生型（进程内）都满足；内嵌型只是通用 HTTP 转发，
// 没有独立的管理 API，也没法在服务端注入凭据。
//
// 四条接缝一律用这个判据而不是 `== SourceManaged`：否则每加一种形态都要
// 去四个文件里追一遍，而漏掉任何一处都是静默的功能缺失（面板入口空、
// 领取不执行、用量采不到），不是报错。
func (s Source) Hosted() bool { return s == SourceManaged || s == SourceNative }

// Family 把来源归一成**对外的**两类：内嵌型 / 托管型。
//
// 为什么对外只认两类：面板与客户端真正要判的是「这是 ModelMux 托管的一个平台
// （积分型、账号在控制台里管），还是用户自己填的一个 API 端点」，而**不是**
// 「上游跑在子进程里还是本进程里」——后者是实现细节，连渠道配置都能在两种形态
// 之间切换而其余字段一个不动。
//
// 另一半原因是硬约束而非审美：上游控制台那套前端（web/upstream.js）按两值
// 写死了 `source === 'managed'`。多暴露一个 native 会让它把原生渠道判成 API 型，
// 一次性少掉账号池 / 模型档位 / 用量 / 配置 / 日志五个视图 —— 而 W2 的验收
// 标准正是「upstream.js 字节级零改动」。要区分「进程内还是子进程」时，
// 用 Upstream.Source 本身（Go 侧），别扩这个对外值域。
func (s Source) Family() Source {
	if s == SourceNative {
		return SourceManaged
	}
	return s
}

// Upstream 一个可路由的上游端点。
type Upstream struct {
	Name        string
	DisplayName string
	Source      Source
	Enabled     bool

	BaseURL     string
	Protocol    string
	ModelPrefix string
	Models      config.ModelList
	APIKeys     []string
	Headers     map[string]string
	Weight      int
	Priority    int
	TimeoutStr  string
	Retries     int
	HealthPath  string
	// ModelsPath 枚举模型用的路径。与 HealthPath 分开：前者回答「有哪些模型」，
	// 后者回答「进程起来了没有」，混用会让 /healthz 那种响应被当成模型列表。
	ModelsPath string
	Proxy      string

	// Preset 来源模板 id（仅展示）。
	Preset string
	// Kind 识别出来的网关种类（zcode / workbuddy / newapi / ""）。
	// 由配置里的 Kind 落盘而来，不靠名称猜——见 config.ManagedProvider.Kind。
	Kind string
	// ModelSource 面板提示：auto / manual。
	ModelSource string

	// RootURL 子进程的根地址（不含 API 前缀），仅托管型有。
	// 管理代理用它拼「上游自带面板 API」的地址——那些 API 不在 /v1 之下。
	RootURL string
	// PanelAPIPrefix 上游管理 API 的路径前缀（缺省 /panel/api），仅托管型有。
	// 与 RootURL 配合：管理请求转发到 RootURL + PanelAPIPrefix + /...，
	// 密钥由本进程注入，浏览器从头到尾不接触上游密钥。
	PanelAPIPrefix string

	// Ready 运行时就绪判定；nil 视为恒就绪。
	// 托管型用它反映「子进程还没起来」——此时不该把请求路由过去。
	Ready func() bool
	// UnreadyReason 未就绪的原因（启动失败的报错等）。
	UnreadyReason func() string
	// PanelURL 该上游自带面板的地址（仅托管型有）。
	PanelURL string
}

// IsReady 是否可用于转发。
func (u Upstream) IsReady() bool { return u.Ready == nil || u.Ready() }

// Reason 未就绪原因。
func (u Upstream) Reason() string {
	if u.UnreadyReason == nil {
		return ""
	}
	return u.UnreadyReason()
}

// TimeoutDuration 单次请求超时。
func (u Upstream) TimeoutDuration() time.Duration {
	return durOr(u.TimeoutStr, 120*time.Second)
}

// RetryCount 归一化后的重试次数。
func (u Upstream) RetryCount() int {
	switch {
	case u.Retries < 0:
		return 0
	case u.Retries > 5:
		return 5
	default:
		return u.Retries
	}
}

// FromEmbedded 把内嵌型配置转成统一形状。
func FromEmbedded(c config.EmbeddedProvider) Upstream {
	return Upstream{
		Name: c.Name, DisplayName: c.DisplayName, Source: SourceEmbedded,
		Enabled: c.Enabled, BaseURL: c.BaseURL, Protocol: c.ProtocolKind(),
		ModelPrefix: c.ModelPrefix, Models: c.Models, APIKeys: c.APIKeys,
		Headers: c.Headers, Weight: c.Weight, Priority: c.Priority,
		TimeoutStr: c.Timeout, Retries: c.Retries,
		HealthPath: c.HealthPath, Proxy: c.Proxy,
		Preset: c.Preset, ModelSource: c.ModelSource,
		// 取值顺序：显式 models_path → 老的 health_path（老配置里它兼任模型列表路径）
		// → 通用默认 /v1/models。
		ModelsPath: firstNonEmpty(c.ModelsPath, c.HealthPath, defaultModelsPath),
	}
}

// FromManaged 把托管型配置 + 运行状态转成统一形状。
//
// status 为 nil 表示当前没有可用实例（未启用或启动失败）——此时上游仍然
// 会被构造出来（面板要显示它），但 IsReady() 为 false，路由会跳过它并给出明确原因。
//
// 配置里的 Mode 决定来源标成 managed（子进程）还是 native（进程内）。两者在
// 本函数之后就没有分叉点了：都带 RootURL / PanelAPIPrefix / 就绪闭包。
func FromManaged(c config.ManagedProvider, baseURL string, ready bool, lastErr string) Upstream {
	r := c.Route
	if r == nil {
		// 调用方不该在没有 Route 时调用本函数；给个安全的空壳总比 panic 好
		r = &config.RouteSpec{}
	}
	// 上游的 API 挂在前缀之下（默认 /v1）。把前缀拼进 BaseURL 之后，
	// 下游的路径拼接逻辑与内嵌型完全一致，不需要任何特殊分支。
	apiPrefix := defaultAPIPrefix
	if r.PathPrefix != nil {
		apiPrefix = *r.PathPrefix
	}
	root := strings.TrimRight(strings.TrimSpace(baseURL), "/")

	u := Upstream{
		Name: c.Name, DisplayName: c.DisplayName, Source: sourceOfManaged(c),
		Enabled: c.Enabled, BaseURL: root + apiPrefix,
		Protocol:    config.NormalizeProtocolName(r.Protocol),
		ModelPrefix: r.ModelPrefix, Models: r.Models,
		Headers: r.Headers, Weight: r.Weight, Priority: r.Priority,
		TimeoutStr: r.Timeout, Retries: r.Retries,
		HealthPath: c.HealthPath,
		ModelsPath: firstNonEmpty(r.ModelsPath, defaultModelsPath),
		Preset:     c.Preset,
		Kind:       c.Kind,
		RootURL:    root,
		// normalize 已把空值补成默认 /panel/api；这里直接取，代理不用再猜。
		PanelAPIPrefix: c.PanelAPIPrefix,
		// 托管型也走「自动」模型目录：子进程起来后自动拉一次，
		// 用户不必先手工点一次保存才能看到它的模型。
		ModelSource: "auto",
	}
	if r.APIKey != "" {
		u.APIKeys = []string{r.APIKey}
	}
	if c.PanelPath != "" && root != "" {
		// 面板不在 API 前缀之下（/panel/ 而非 /v1/panel/），所以用不带前缀的根地址
		u.PanelURL = root + c.PanelPath
	}
	if !ready {
		u.Ready = func() bool { return false }
		u.UnreadyReason = func() string {
			if lastErr != "" {
				return lastErr
			}
			// 「本地服务」而不是「子进程」：原生型跑在本进程内，说成子进程会
			// 让人去找一个根本不存在的进程。
			return "本地服务未就绪"
		}
	}
	return u
}

// sourceOfManaged 把配置里的运行方式映射成渠道来源。
//
// 只认显式的 Mode：`kind` / `preset` 都不参与判断。它们标记的是「这是哪个网关」，
// 与「这个网关以什么方式跑」是两件事——老配置里的 workbuddy 渠道 kind 同样是
// workbuddy，但它是子进程形态，按 kind 判来源会把它当场改成进程内，
// 直接丢掉用户已登录的账号。
func sourceOfManaged(c config.ManagedProvider) Source {
	if c.Native() {
		return SourceNative
	}
	return SourceManaged
}

// signature 计算「影响路由的字段」的指纹。
//
// 用途是避免面板每 3 秒轮询状态时都重建一遍渠道对象（会丢掉 Key 冷却状态）。
// 只把真正影响转发行为的字段算进去——纯展示字段（如 DisplayName）改了不该触发重建。
func (u Upstream) signature() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s|%s|%t|%s|%s|%d|%d|%s|%d|%s|%s|%s|",
		u.Name, u.Source, u.Enabled, u.BaseURL, u.ModelPrefix,
		u.Weight, u.Priority, u.TimeoutStr, u.Retries, u.HealthPath, u.Protocol, u.Proxy)
	fmt.Fprintf(&b, "keys=%s;", strings.Join(u.APIKeys, "\x1f"))

	hk := make([]string, 0, len(u.Headers))
	for k := range u.Headers {
		hk = append(hk, k)
	}
	sort.Strings(hk)
	for _, k := range hk {
		fmt.Fprintf(&b, "h:%s=%s;", k, u.Headers[k])
	}
	for _, m := range u.Models {
		fmt.Fprintf(&b, "m:%s=%s;", m.ID, m.Outward())
	}
	if u.Ready != nil && !u.IsReady() {
		b.WriteString("unready;")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

// Signature 导出给同步逻辑用。
func (u Upstream) Signature() string { return u.signature() }

// Signatures 计算整组上游的指纹（顺序无关）。
func Signatures(ups []Upstream) string {
	parts := make([]string, 0, len(ups))
	for _, u := range ups {
		parts = append(parts, u.Signature())
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:8])
}

func durOr(s string, def time.Duration) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
}
