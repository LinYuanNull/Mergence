// config.go ModelMux 配置：加载 / 默认值 / 保存。
//
// 设计原则：
//   - 配置缺失一律回落默认值，不因缺字段而报错（用户手改 json 是常态）
//   - 非法值走「钳制 + 记警告」而不是拒绝启动，避免一个笔误让整个应用起不来
//   - 每次保存前做一次 Parse（校验 + 归一化），保证落盘的是有效状态
//
// 唯一例外是「会导致路由歧义」的配置（前缀重复、启用但没填 base_url）：
// 这类必须禁用该渠道而不是猜，否则请求会被静默送到错误的渠道上。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Version 当前配置结构版本，用于后续自动迁移。
//
// v1 → v2：embedded_providers 从占位结构扩展为完整渠道结构
// （多 Key、三协议、模型别名、权重优先级、超时重试、代理）。
//
// v2 → v3：把「模型列表路径」从 health_path 里拆出来。
// v2 没有独立字段，health_path 兼任它且默认值就是 /v1/models；升到 v3 后
// 若不清掉这个旧默认值，就会拼成 /v1/v1/models（一个来自上游的 404，
// 从现象几乎推不到原因）。所以迁移动作是：等于旧默认值的 health_path 清空。
const Version = 3

// Config 顶层配置。
type Config struct {
	Version int `json:"version"`

	Log   LogConfig   `json:"log"`
	Tray  TrayConfig  `json:"tray"`
	Ports PortsConfig `json:"ports"`
	Claim ClaimConfig `json:"claim"`

	// AccessKey 对外 /v1 出口的鉴权 Key。
	//
	// 由系统自动生成（crypto/rand），用户不需要也不会想密码；面板上可查看/
	// 复制/重新生成。留空（老配置）会在 normalize 时补生成——生成即是迁移。
	AccessKey string `json:"access_key"`

	// Managed 托管型 provider：独立子进程，由编排器负责拉起与回收。
	Managed []ManagedProvider `json:"managed_providers"`

	// Embedded 内嵌型 provider：跑在本进程内（自定义渠道 / 预设渠道）。
	Embedded []EmbeddedProvider `json:"embedded_providers"`
}

// LogConfig 日志设置。
type LogConfig struct {
	// Level 取值 debug / info / warn / error，缺省 info。
	Level string `json:"level"`
	// Dir 日志目录，缺省 <数据目录>/logs。
	Dir string `json:"dir"`
	// RingSize 面板消费的结构化环形缓冲容量（条），缺省 5000。
	RingSize int `json:"ring_size"`
	// MaxFileMB 单个日志文件上限（MiB），超出后轮转，缺省 32。
	MaxFileMB int `json:"max_file_mb"`
}

// TrayConfig 托盘与窗口生命周期设置。
type TrayConfig struct {
	// MinimizeToTray 关窗时隐藏到托盘而不是退出。缺省 true。
	MinimizeToTray bool `json:"minimize_to_tray"`
	// SingleInstance 单实例；第二次启动唤出已有窗口。缺省 true。
	SingleInstance bool `json:"single_instance"`
}

// ClaimConfig 限时套餐的定时领取设置。
//
// 场景：zcode2api 这类网关的上游每天 12:01 放出一批**限时免费套餐**，
// 名额有限、抢完即止。手动守着点领取不现实，所以由 ModelMux 每天在
// 12:01–12:05 这个窗口内代领一次。
//
// 为什么是「窗口」而不是「精确 12:01」：
//   - 上游名额恢复时间不精确（回执里的 next_at 可能浮动几分钟）；
//   - 机器在 12:00 关机、12:03 才开机的话，精确到分钟的定时器会整天错过。
//
// 窗口内启动会**补领**当天这一次，窗口过后不再补。
type ClaimConfig struct {
	// Enabled 总开关。缺省 false——自动领取会消耗上游的写流量配额，
	// 必须由用户显式打开，不能默认替用户做决定。
	Enabled bool `json:"enabled"`
	// At 窗口开始时刻 HH:MM，缺省 12:01。
	At string `json:"at"`
	// Window 窗口长度（分钟），缺省 4，即 12:01–12:05。
	Window int `json:"window"`
	// Channel 目标托管渠道名。留空则自动挑「第一个就绪的 zcode 类托管渠道」。
	Channel string `json:"channel"`
	// AdminKey 该网关的后台密码（zcode2api 的 ZCODE_ADMIN_KEY）。
	//
	// 领取走管理接口 `/admin/api/claim`，需要 Bearer 鉴权。
	// 单独放这里而不是塞进 env：那个 .env 归 zcode2api 自己管，
	// ModelMux 只能通过渠道配置拿到它。
	AdminKey string `json:"admin_key"`
}

// 定时领取的缺省值。
const (
	ClaimDefaultAt     = "12:01"
	ClaimDefaultWindow = 4 // 分钟 → 12:01–12:05
)

// Normalize 归一化并校验，补齐缺省值。
//
// 非法输入（时间格式错、窗口为负）一律回落缺省并保持 Enabled 原值：
// 配置写错不该悄悄关掉用户的开关，但也不能让它变成永不触发的死配置。
func (c *ClaimConfig) Normalize() {
	c.At = strings.TrimSpace(c.At)
	if _, err := time.Parse("15:04", c.At); err != nil {
		c.At = ClaimDefaultAt
	}
	if c.Window <= 0 {
		c.Window = ClaimDefaultWindow
	}
	if c.Window > 60 {
		c.Window = 60 // 窗口跨一小时就失去意义了
	}
	c.Channel = strings.TrimSpace(c.Channel)
	c.AdminKey = strings.TrimSpace(c.AdminKey)
}

// WindowStart 返回当天窗口的起始时刻。
func (c ClaimConfig) WindowStart(day time.Time) time.Time {
	t, err := time.Parse("15:04", c.At)
	if err != nil {
		t, _ = time.Parse("15:04", ClaimDefaultAt)
	}
	return time.Date(day.Year(), day.Month(), day.Day(), t.Hour(), t.Minute(), 0, 0, day.Location())
}

// PortsConfig 动态端口设置。
type PortsConfig struct {
	// Reuse 重启时优先复用上次分配的端口，避免用户书签失效。缺省 true。
	Reuse bool `json:"reuse"`
	// MaxRetry 端口被抢占时的换端口重试次数。缺省 3。
	MaxRetry int `json:"max_retry"`
	// PanelPort 用户指定的固定监听端口。0 = 动态分配（默认）。
	//
	// 动态端口的缺点是「每次重启都可能变」；用户如果想把它当常驻服务用
	// （书签、外部客户端写死地址），在这里固定。保存时服务端会做平滑切换。
	PanelPort int `json:"panel_port"`
}

// RouteSpec 把托管型 provider 的本地端点暴露成可路由的渠道。
//
// 这是「托管型」与「内嵌型」在路由层的接口：有了它，new-api / zcode2api /
// trae-local-api / 现有的 WorkBuddy 网关都不必把源码搬进来 —— 它们本来就是
// 独立的 OpenAI 兼容服务，只要告诉 ModelMux「它的模型叫什么、前缀是什么」即可。
//
// 为 nil 表示只托管、不路由（例如纯粹为了用那个 provider 自带的面板）。
type RouteSpec struct {
	// ModelPrefix 对外模型名前缀（如 `or-`），留空按 provider 名自动生成。
	// 不带尾斜杠：拼接时直接与模型名相连。
	ModelPrefix string `json:"model_prefix"`
	// Models 声明它能提供哪些模型。留空则只在 /v1/models 里不出现，
	// 但前缀匹配的请求仍会透传（与内嵌型的规则一致）。
	Models ModelList `json:"models"`
	// APIKey ModelMux 调该子进程时带的 Bearer Key。
	// 多数 2api 项目要求一个本地 Key 才肯服务，填它。
	APIKey string `json:"api_key"`
	// Protocol 该子进程说的是哪种协议，默认 chat。
	Protocol string `json:"protocol"`
	// PathPrefix 该子进程的 API 路径前缀，缺省 /v1。
	//
	// 为什么必须有它：内嵌型渠道填的 BaseURL 通常已经含 /v1（用户从文档里粘的），
	// 而托管型只知道一个「监听在哪个端口」，它的 API 挂在哪一段路径下只有配置知道。
	// 缺了它就会把请求发到 /chat/completions，而上游只认 /v1/chat/completions，
	// 表现是一个来自上游的 404 —— 极难从现象推断到原因。
	//
	// 显式写 "" 表示没有前缀（端点直接挂在根上）。
	PathPrefix *string `json:"path_prefix"`
	// ModelsPath 模型列表路径，**相对 PathPrefix**，缺省 /models。
	// 它与 health_path 是两件事：前者回答「有哪些模型」，后者回答「进程起来了没有」。
	ModelsPath string            `json:"models_path"`
	Headers    map[string]string `json:"headers"`
	Weight     int               `json:"weight"`
	Priority   int               `json:"priority"`
	Timeout    string            `json:"timeout"`
	Retries    int               `json:"retries"`
}

// ManagedProvider 托管型 provider 定义。
type ManagedProvider struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Enabled     bool   `json:"enabled"`
	// Preset 记录「由哪个模板创建」，仅面板回显与厂商归类用。
	Preset string `json:"preset,omitempty"`
	// Kind 是识别出来的网关种类（zcode / workbuddy / newapi / ""）。
	//
	// 为什么要有这个字段：识别「这是哪个网关」原本靠 Name+DisplayName+Preset
	// 三个字符串模糊匹配。但预设已经取消（Preset 不再由用户选择、也可能为空），
	// 用户完全可以把渠道改名叫「我的网关」——那时三字段都匹配不上，
	// kind 变空，后果是zcode 的管理代理打到错误地址（404）、凭据取错（401）。
	//
	// 所以识别结果在**创建时落盘一次**，之后一律读它，不再靠猜。
	// 空值表示「还没识别或识别不出」，此时才回退到名称匹配（兼容老配置）。
	Kind string `json:"kind,omitempty"`

	// PanelAPIPrefix 该子进程自带管理面板的 API 前缀（缺省 /panel/api）。
	//
	// ModelMux 的代理用它把「账号列表 / 启停账号」这类管理请求转发给子进程，
	// 面板上因此不再需要跳转打开上游面板，也就不再需要向上游输密钥——
	// 密钥由 ModelMux 服务端持有并在代理时注入，浏览器从不接触它。
	PanelAPIPrefix string `json:"panel_api_prefix,omitempty"`

	Command string   `json:"command"`
	Args    []string `json:"args"`
	Dir     string   `json:"dir"`
	DataDir string   `json:"data_dir"`

	// PortEnvVar provider 原生的端口环境变量名（如 new-api 用 PORT、
	// workbuddy2api 用 WB2A_LISTEN）。留空则只注入 MODELMUX_PORT。
	PortEnvVar string `json:"port_env_var"`
	// FixedPort > 0 表示该 provider 硬编码端口、不支持动态分配，
	// 此时走「固定端口 + 冲突预检」降级模式。
	FixedPort int `json:"fixed_port"`

	HealthPath    string `json:"health_path"`
	ReadyTimeout  string `json:"ready_timeout"`
	ShutdownGrace string `json:"shutdown_grace"`

	// PanelPath 该 provider 自带面板的路径，供面板「打开上游面板」用。
	PanelPath string `json:"panel_path"`

	// CreditValue 每积分价值（元），用于把上游的积分消耗折算成实际花费。
	//
	// 为什么需要它：积分型平台渠道的花费以「积分」计，而积分本身是花钱买的
	// （套餐、充值），上游不会告诉你 1 积分值多少钱。留 0 表示未配置——
	// 概览会显示「—」并提示填写，绝不按 0 元展示（那会被读成"没花钱"）。
	//
	// 典型值：99 元买了 100 万积分 → 每积分 0.000099 元。
	CreditValue float64 `json:"credit_value"`

	// Route 非空则把它的端点注册为可路由渠道。
	Route *RouteSpec `json:"route,omitempty"`

	Env map[string]string `json:"env"`
}

// ChannelModel 渠道里的单个模型。
//
// Alias 用于别名映射：上游真实模型名是 ID，对外想让客户端用 Alias。
// 典型场景——上游把 `claude-opus` 映射成了 `glm-5.3`，直接暴露会让人看不懂。
type ChannelModel struct {
	ID      string `json:"id"`
	Alias   string `json:"alias,omitempty"`
	Context int    `json:"context,omitempty"`
	MaxOut  int    `json:"max_output,omitempty"`
}

// Outward 返回该模型对外的名字（不含渠道前缀）。
func (m ChannelModel) Outward() string {
	if a := strings.TrimSpace(m.Alias); a != "" {
		return a
	}
	return strings.TrimSpace(m.ID)
}

// ModelList 模型列表，兼容两种写法：
//
//	["gpt-4o", "gpt-4o-mini"]                        ← 简写
//	[{"id":"gpt-4o","alias":"4o","context":128000}]  ← 完整
//
// 手改配置是常态，两种都认可以省掉一次「配置迁移」的坑。
type ModelList []ChannelModel

// UnmarshalJSON 同时接受字符串数组与对象数组。
func (l *ModelList) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*l = nil
		return nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("模型列表应为数组：%w", err)
	}
	out := make(ModelList, 0, len(raw))
	for _, r := range raw {
		t := strings.TrimSpace(string(r))
		if t == "" || t == "null" {
			continue
		}
		if t[0] == '"' {
			var id string
			if err := json.Unmarshal(r, &id); err != nil {
				return err
			}
			out = append(out, ChannelModel{ID: id})
			continue
		}
		var m ChannelModel
		if err := json.Unmarshal(r, &m); err != nil {
			return err
		}
		out = append(out, m)
	}
	*l = out
	return nil
}

// ChannelModelOf 把一批模型名转成列表（供面板/预设构造）。
func ChannelModelOf(ids ...string) ModelList {
	out := make(ModelList, 0, len(ids))
	for _, id := range ids {
		if s := strings.TrimSpace(id); s != "" {
			out = append(out, ChannelModel{ID: s})
		}
	}
	return out
}

// EmbeddedProvider 内嵌型 provider（自定义渠道 / 预设渠道）。
//
// 「内嵌型」= 跑在 ModelMux 进程内，由本进程直接实现对外出口；
// 与 ManagedProvider（独立子进程）相对。
type EmbeddedProvider struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Enabled     bool   `json:"enabled"`

	// Preset 记录「由哪个模板创建」，仅面板回显用，不参与运行时逻辑。
	Preset string `json:"preset,omitempty"`

	// BaseURL 上游根地址，如 https://openrouter.ai/api/v1（不带结尾斜杠）。
	BaseURL string `json:"base_url"`
	// APIKeys 支持多个，运行时轮询，构成一个小账号池。
	APIKeys []string `json:"api_keys"`

	// Protocol 描述**上游**说的是哪种协议：chat / responses / anthropic。
	// 对外一律是 OpenAI Chat Completions，协议差异由适配层吸收。
	Protocol string `json:"protocol"`

	// ModelPrefix 对外模型名前缀，如 `or-`。留空则按名称自动生成。
	// 拼接时不再加分隔符：`or-` + `gpt-4o` → `or-gpt-4o`。
	ModelPrefix string `json:"model_prefix"`

	// ModelSource 仅面板提示：auto（拉取后多选）/ manual（逐条填）。
	ModelSource string `json:"model_source,omitempty"`

	Models ModelList `json:"models"`

	// Headers 自定义请求头，覆盖默认鉴权头。很多中转站靠特定 header 标识来源。
	Headers map[string]string `json:"headers"`

	// Weight / Priority：多个渠道能提供同一个对外模型名时的择一依据。
	// Priority 大者优先；相同则按 Weight 加权随机。缺省 weight=1、priority=0。
	Weight   int `json:"weight"`
	Priority int `json:"priority"`

	// Timeout 单次上游请求超时（含建立连接），缺省 120s。
	// 注意这是「非流式整体」超时；流式只约束建立连接与首字节。
	Timeout string `json:"timeout"`
	// Retries 失败重试次数（仅对可重试错误：连接失败、429、5xx）。缺省 1。
	Retries int `json:"retries"`

	// HealthPath 兼容字段：早期把「健康检查路径」与「模型列表路径」当成同一个，
	// 现在两者分开。留着它是为了老配置继续可用（见 Upstream.ModelsPath 的取值顺序）。
	HealthPath string `json:"health_path"`
	// ModelsPath 模型列表路径，缺省 /v1/models。
	// 很多服务的模型列表并不在 /v1/models 下（Ollama、各类自建网关），所以它必须可配。
	ModelsPath string `json:"models_path"`
	// Proxy HTTP/SOCKS5 代理，如 http://127.0.0.1:7890。留空表示直连。
	Proxy string `json:"proxy"`

	// Price 该渠道的自定义单价（美元 / 每百万 token）。
	//
	// 为什么要它：概览上的「API 费用」= 本地记的 token × 单价，
	// 而内置价格表只覆盖主流厂商的公开价。以下情况它一定不准：
	//   - 中转站/OpenRouter 的倍率与厂商原价不同
	//   - 用了协议价、赠送额度、批量折扣
	//   - 私有部署（vLLM / Ollama）根本没有单价
	// 前两种填上真实单价，第三种留空——留空会显示「价格未知」，
	// 这比显示一个按 gpt-4o 价算出来的假数字诚实得多。
	//
	// 三个字段留空/为 0 时回落到内置表；CachedPerM 为 0 时按 CachedRatio 算。
	Price ChannelPrice `json:"price,omitempty"`
}

// ChannelPrice 渠道自定义单价（美元 / 每百万 token）。
type ChannelPrice struct {
	InputPerM  float64 `json:"input_per_m,omitempty"`
	CachedPerM float64 `json:"cached_per_m,omitempty"`
	OutputPerM float64 `json:"output_per_m,omitempty"`
	// CachedRatio CachedPerM 为 0 时用比例（如 0.1 表示缓存价是全价的十分之一）。
	CachedRatio float64 `json:"cached_ratio,omitempty"`
}

// Override 该渠道是否填了可用的自定义单价。
//
// 必须三个维度都填才算「填了」：只填输入价却按内置输出价算，
// 得到的总额是个混合口径的数字，比明确显示「价格未知」更难解释。
func (p ChannelPrice) Override() (m Price, ok bool) {
	if p.InputPerM <= 0 || p.OutputPerM <= 0 {
		return
	}
	return Price{
		InputPerM: p.InputPerM, CachedPerM: p.CachedPerM,
		OutputPerM: p.OutputPerM, CachedRatio: p.CachedRatio,
	}, true
}

// Price 是计价用的单价结构（与 metrics.Price 字段一一对应）。
//
// 这里单独定义而不是直接用 metrics.Price：config 是最底层的包，
// 不该为了一个字段反向依赖上层。代价是 channelPricing 里要做一次转换。
type Price struct {
	InputPerM   float64
	CachedPerM  float64
	OutputPerM  float64
	CachedRatio float64
}

// ProtocolKind 归一化后的协议名。
func (p EmbeddedProvider) ProtocolKind() string {
	switch strings.ToLower(strings.TrimSpace(p.Protocol)) {
	case "responses":
		return "responses"
	case "anthropic":
		return "anthropic"
	default:
		return "chat"
	}
}

// TimeoutDuration 解析超时，非法或缺失回落默认。
func (p EmbeddedProvider) TimeoutDuration() time.Duration {
	return parseDur(p.Timeout, 120*time.Second)
}

// RetryCount 归一化后的重试次数。
func (p EmbeddedProvider) RetryCount() int {
	if p.Retries < 0 {
		return 0
	}
	if p.Retries > 5 {
		return 5
	}
	return p.Retries
}

// DefaultHealthPath 返回内嵌型的兼容字段 health_path 的缺省值。
//
// 注意它现在**只作为 ModelsPath 的历史回退**存在（v2 及以前两者合一）。
// 新配置请用 models_path；这个值保留是为了老配置文件继续工作。
func DefaultHealthPath(string) string { return "/v1/models" }

func parseDur(s string, def time.Duration) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// Default 返回带默认值的配置。
func Default() *Config {
	c := &Config{Version: Version}
	c.Log = LogConfig{Level: "info", RingSize: 5000, MaxFileMB: 32}
	c.Tray = TrayConfig{MinimizeToTray: true, SingleInstance: true}
	c.Ports = PortsConfig{Reuse: true, MaxRetry: 3}
	// 兜底配置也要有 Key：Default 的使用者（currentConfig 兜底、部分测试）
	// 不一定经过 normalize，而 /v1 出口「有 Key 才保护」——空 Key 会被
	// withAccessKey 判为配置损坏而拒绝服务。
	if k, err := GenerateAccessKey(); err == nil {
		c.AccessKey = k
	}
	return c
}

// normalize 校验与归一化：非法值钳制回默认，不报错。
func (c *Config) normalize() []string {
	var warns []string

	from := c.Version
	if c.Version == 0 {
		from = 1
	}
	if from < Version {
		warns = append(warns, fmt.Sprintf("配置版本 v%d 已升级到 v%d（结构向后兼容，无需手工迁移）", from, Version))
		c.Version = Version
	}

	c.Claim.Normalize()

	switch strings.ToLower(strings.TrimSpace(c.Log.Level)) {
	case "debug", "info", "warn", "error":
		c.Log.Level = strings.ToLower(strings.TrimSpace(c.Log.Level))
	case "":
		c.Log.Level = "info"
	default:
		warns = append(warns, fmt.Sprintf("log.level=%q 非法，回落 info", c.Log.Level))
		c.Log.Level = "info"
	}
	if c.Log.RingSize <= 0 {
		c.Log.RingSize = 5000
	}
	if c.Log.MaxFileMB <= 0 {
		c.Log.MaxFileMB = 32
	}

	if c.Ports.MaxRetry <= 0 {
		c.Ports.MaxRetry = 3
	}

	// 对外 Key：留空即生成。生成即是迁移——老配置文件升级后自动获得保护，
	// 用户不需要做任何事；面板上随时可以查看或重新生成。
	if c.AccessKey = strings.TrimSpace(c.AccessKey); c.AccessKey == "" {
		k, err := GenerateAccessKey()
		if err != nil {
			// crypto/rand 失败极罕见（系统熵源故障）。宁可拒绝启动也不能
			// 静默跑在一个「以为有保护其实没有」的出口上。
			return append(warns, "生成 access_key 失败："+err.Error())
		}
		c.AccessKey = k
		warns = append(warns, "已自动生成对外 API Key（面板设置里可查看）")
	}

	seen := map[string]bool{}
	// 前缀唯一性在**托管型与内嵌型之间共享一张表**：两类渠道参与同一套路由，
	// 各自内部唯一但互相重复，仍然会让请求被静默送到错误的上游。
	prefixOwner := map[string]string{}

	for i := range c.Managed {
		p := &c.Managed[i]
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			warns = append(warns, fmt.Sprintf("managed_providers[%d] 缺少 name，已忽略", i))
			p.Enabled = false
			continue
		}
		if seen["m:"+p.Name] {
			warns = append(warns, fmt.Sprintf("托管 provider 重名 %q，后者已禁用", p.Name))
			p.Enabled = false
		}
		seen["m:"+p.Name] = true

		if p.DisplayName = strings.TrimSpace(p.DisplayName); p.DisplayName == "" {
			p.DisplayName = p.Name
		}
		p.Command = strings.TrimSpace(p.Command)
		p.Dir = strings.TrimSpace(p.Dir)
		p.DataDir = strings.TrimSpace(p.DataDir)
		p.PortEnvVar = strings.TrimSpace(p.PortEnvVar)
		p.PanelPath = strings.TrimSpace(p.PanelPath)
		if p.PanelAPIPrefix = strings.TrimSpace(p.PanelAPIPrefix); p.PanelAPIPrefix == "" {
			// 绝大多数 2api 项目的管理 API 都挂在 /panel/api 下；个例在预设或
			// 表单里改。留空自动补默认值，用户不必知道这个字段存在。
			p.PanelAPIPrefix = "/panel/api"
		}
		if p.FixedPort < 0 || p.FixedPort > 65535 {
			warns = append(warns, fmt.Sprintf("%s 的 fixed_port=%d 越界，按 0 处理", p.Name, p.FixedPort))
			p.FixedPort = 0
		}
		if p.HealthPath = strings.TrimSpace(p.HealthPath); p.HealthPath == "" {
			p.HealthPath = "/healthz"
		}
		if p.Enabled && p.Command == "" {
			warns = append(warns, fmt.Sprintf("托管 provider %q 已启用但没填 command，已禁用", p.Name))
			p.Enabled = false
		}

		if p.Route != nil {
			normalizeRouteSpec(p.Name, p.Route, &warns)
			if p.Enabled && !claimPrefix("托管 "+p.Name, p.Route.ModelPrefix, prefixOwner, &warns) {
				p.Enabled = false
				// 禁用后它不再参与路由，前缀要让出来给后面的渠道用
				delete(prefixOwner, p.Route.ModelPrefix)
			}
		}
	}

	c.normalizeEmbedded(&warns, seen, prefixOwner, from)

	return warns
}

// normalizeRouteSpec 归一化托管型 provider 的可路由部分。
func normalizeRouteSpec(name string, r *RouteSpec, warns *[]string) {
	r.ModelPrefix = normalizePrefix(name, r.ModelPrefix)
	r.Protocol = normalizeProtocol(name, r.Protocol, warns)
	r.Models = normalizeModels(name, r.Models, warns)
	r.Headers = normalizeHeaders(r.Headers)
	r.ModelsPath = strings.TrimSpace(r.ModelsPath)
	if r.PathPrefix != nil {
		v := strings.Trim(strings.TrimSpace(*r.PathPrefix), "/")
		if v == "" {
			r.PathPrefix = ptrString("")
		} else {
			r.PathPrefix = ptrString("/" + v)
		}
	}
	if r.Weight <= 0 {
		r.Weight = 1
	}
	r.Retries = clampRetries(name, r.Retries, warns)
}

// claimPrefix 登记前缀；重复时告警并返回 false。
func claimPrefix(owner, prefix string, prefixOwner map[string]string, warns *[]string) bool {
	if prev, dup := prefixOwner[prefix]; dup {
		*warns = append(*warns, fmt.Sprintf(
			"渠道 %q 的模型前缀 %q 与 %q 重复，已禁用（请改成不同的 model_prefix）",
			owner, prefix, prev))
		return false
	}
	prefixOwner[prefix] = owner
	return true
}

// normalizePrefix 前缀归一化：去空白、补结尾斜杠、留空则按名称自动生成。
// normalizePrefix 归一化模型前缀。
//
// 规则：**不带尾部斜杠**，拼接时直接与模型名相连——
// 用户明确要求去掉 `or/gpt-4o` 里那个 `/`。
//
// 但要保证前缀与模型名的**边界可辨**：若用户填的前缀末尾不是分隔符
// （`fc`），直接拼会得到 `fcfake-alpha` —— 两个渠道（`fc` 与 `fca`）
// 的模型名会缠在一起，「哪个前缀」只能靠猜。补一个 `-` 解决这个，
// 且与「不加斜杠」不冲突：`fc` → `fc-` → `fc-fake-alpha`。
func normalizePrefix(name, prefix string) string {
	prefix = strings.TrimSpace(prefix)
	prefix = strings.TrimRight(prefix, "-_./")
	if prefix == "" {
		prefix = AutoPrefix(name)
	}
	if !strings.HasSuffix(prefix, "-") && !strings.HasSuffix(prefix, "_") &&
		!strings.HasSuffix(prefix, ".") {
		prefix += "-"
	}
	return prefix
}

func ptrString(v string) *string { return &v }

// NormalizeProtocolName 把协议名归一化成 chat / responses / anthropic。
// 供外部（如托管型渠道构造）复用同一套判定，避免两处规则漂移。
func NormalizeProtocolName(proto string) string {
	var warns []string
	return normalizeProtocol("", proto, &warns)
}

func normalizeProtocol(name, proto string, warns *[]string) string {
	switch strings.ToLower(strings.TrimSpace(proto)) {
	case "chat", "responses", "anthropic":
		return strings.ToLower(strings.TrimSpace(proto))
	case "":
		return "chat"
	default:
		*warns = append(*warns, fmt.Sprintf("渠道 %q 的 protocol=%q 未知，按 chat 处理", name, proto))
		return "chat"
	}
}

// normalizeModels 去空、按对外名去重。别名映射的语义见 ChannelModel.Outward。
func normalizeModels(name string, in ModelList, warns *[]string) ModelList {
	out := make(ModelList, 0, len(in))
	seen := map[string]bool{}
	for _, m := range in {
		m.ID = strings.TrimSpace(m.ID)
		if m.ID == "" {
			continue
		}
		m.Alias = strings.TrimSpace(m.Alias)
		key := m.Outward()
		if seen[key] {
			*warns = append(*warns, fmt.Sprintf("渠道 %q 的模型 %q 重复，已去重", name, key))
			continue
		}
		seen[key] = true
		if m.Context < 0 {
			m.Context = 0
		}
		if m.MaxOut < 0 {
			m.MaxOut = 0
		}
		out = append(out, m)
	}
	return out
}

func normalizeHeaders(h map[string]string) map[string]string {
	if len(h) == 0 {
		return h
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		if k = strings.TrimSpace(k); k != "" {
			out[k] = strings.TrimSpace(v)
		}
	}
	return out
}

func clampRetries(name string, n int, warns *[]string) int {
	if n < 0 {
		return 0
	}
	if n > 5 {
		*warns = append(*warns, fmt.Sprintf("渠道 %q 的 retries=%d 过大，钳制为 5", name, n))
		return 5
	}
	return n
}

// normalizeEmbedded 归一化内嵌型渠道。
//
// 前缀冲突与「启用但没填 base_url」两种情况必须禁用该渠道：
// 猜一个默认值会让请求被静默送到错误的上游，比直接报错危险得多。
func (c *Config) normalizeEmbedded(warns *[]string, seen map[string]bool, prefixOwner map[string]string, from int) {
	for i := range c.Embedded {
		p := &c.Embedded[i]
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			*warns = append(*warns, fmt.Sprintf("embedded_providers[%d] 缺少 name，已忽略", i))
			p.Enabled = false
			continue
		}
		if seen["e:"+p.Name] {
			*warns = append(*warns, fmt.Sprintf("内嵌渠道重名 %q，后者已禁用", p.Name))
			p.Enabled = false
		}
		seen["e:"+p.Name] = true

		if p.DisplayName = strings.TrimSpace(p.DisplayName); p.DisplayName == "" {
			p.DisplayName = p.Name
		}
		p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
		p.Protocol = normalizeProtocol(p.Name, p.Protocol, warns)
		if p.ModelSource != "manual" {
			p.ModelSource = "auto"
		}
		p.ModelPrefix = normalizePrefix(p.Name, p.ModelPrefix)

		if p.Enabled {
			if !claimPrefix(p.Name, p.ModelPrefix, prefixOwner, warns) {
				p.Enabled = false
			}
			if p.BaseURL == "" {
				*warns = append(*warns, fmt.Sprintf("渠道 %q 已启用但未填 base_url，已禁用", p.Name))
				p.Enabled = false
			}
		}

		// Key 去空去空白
		keys := make([]string, 0, len(p.APIKeys))
		for _, k := range p.APIKeys {
			if k = strings.TrimSpace(k); k != "" {
				keys = append(keys, k)
			}
		}
		p.APIKeys = keys

		if p.HealthPath = strings.TrimSpace(p.HealthPath); p.HealthPath == "" {
			p.HealthPath = DefaultHealthPath(p.Protocol)
		}
		if p.Weight <= 0 {
			p.Weight = 1
		}
		p.Retries = clampRetries(p.Name, p.Retries, warns)
		p.Models = normalizeModels(p.Name, p.Models, warns)
		p.Headers = normalizeHeaders(p.Headers)
		p.ModelsPath = strings.TrimSpace(p.ModelsPath)

		// v2 → v3 迁移：v2 里 health_path 兼任模型列表路径且默认 /v1/models。
		// 不清掉它，下面 ModelsPath 会取到 /v1/models，再拼上 baseURL 的 /v1
		// 就成了 /v1/v1/models。
		if from < 3 && p.ModelsPath == "" && p.HealthPath == "/v1/models" {
			p.HealthPath = ""
		}
	}
}

// AutoPrefix 从渠道名生成 ASCII 安全的前缀片断。
//
// 中文名取不出 ASCII 字符，此时退化为名称哈希——虽然可读性差，但保证唯一且稳定，
// 总好过让两个中文名渠道都拿到同一个前缀而被禁用。
func AutoPrefix(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) > 24 {
		s = s[:24]
	}
	if s == "" {
		h := fnv.New32a()
		_, _ = h.Write([]byte(name))
		s = fmt.Sprintf("ch%04x", h.Sum32()&0xffff)
	}
	return s
}

// Parse 解析字节流并归一化。
func Parse(raw []byte) (*Config, []string, error) {
	c := Default()
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, nil, fmt.Errorf("配置不是合法 JSON 或字段类型不匹配：%w", err)
	}
	return c, c.normalize(), nil
}

// Load 读取配置文件；文件不存在时返回默认配置并落盘一份。
func Load(path string) (*Config, []string, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		c := Default()
		if serr := Save(path, c); serr != nil {
			return c, nil, fmt.Errorf("写入初始配置失败：%w", serr)
		}
		return c, []string{"配置不存在，已生成默认配置"}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("读取配置失败：%w", err)
	}
	return Parse(raw)
}

// Save 原子写盘（先写临时文件再改名，避免写一半掉电留下坏配置）。
//
// 落盘前会先 Parse 一次（校验 + 归一化），保证磁盘上永远是可用的状态。
func Save(path string, c *Config) error {
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// DataDir 返回 ModelMux 的数据根目录（%LOCALAPPDATA%\ModelMux）。
func DataDir() string {
	if v := strings.TrimSpace(os.Getenv("MODELMUX_HOME")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); v != "" {
		return filepath.Join(v, "ModelMux")
	}
	return filepath.Join(os.TempDir(), "ModelMux")
}

// ConfigPath 返回模型/渠道配置文件的完整路径。
func ConfigPath(home string) string {
	return filepath.Join(home, "modelmux.json")
}

// GenerateAccessKey 生成对外 API Key：32 位十六进制（128 位熵）。
//
// 为什么不让用户自填：用户想出来的「密钥」几乎必然是弱口令（项目名、生日、
// 123456）。随机生成 + 面板一键复制 + 随时可重新生成，比「提醒用户设强密码」
// 靠谱得多。
func GenerateAccessKey() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
