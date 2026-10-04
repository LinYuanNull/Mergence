// presets.go 渠道预设模板。
//
// 预设只是「预填了一组字段」的模板，选中后用户仍可改任何一项——它不是特殊类型，
// 保存后就是一个普通的 EmbeddedProvider。这样接入后与手填渠道完全同权，
// 不需要在路由/额度/日志里为「预设渠道」留任何分支。
package provider

import (
	"strings"
)

// Preset 一个渠道模板。
type Preset struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Vendor   string `json:"vendor"`
	BaseURL  string `json:"base_url"`
	Protocol string `json:"protocol"`
	// ModelPrefix 建议前缀；面板会预填，用户可改。
	ModelPrefix string `json:"model_prefix"`
	// KeyHint 帮助用户辨认「填错了东西」，例如把 URL 填进 Key 框。
	KeyHint string `json:"key_hint"`
	// DocURL 用户获取 Key 的地方。
	DocURL string `json:"doc_url"`
	// NeedsKey=false 表示本地端点通常不需要 Key（Ollama / vLLM）。
	NeedsKey bool   `json:"needs_key"`
	Note     string `json:"note,omitempty"`
}

// Presets 内置模板，顺序即面板展示顺序：
// 通用端点排第一（最常见），其余按厂商排列。
var Presets = []Preset{
	{
		ID: "custom", Label: "自定义 OpenAI 兼容端点（空白）", Vendor: "自定义",
		BaseURL: "", Protocol: "chat", ModelPrefix: "",
		KeyHint: "按上游要求填写，本地端点可留空", NeedsKey: false,
		Note: "从零填写。任何实现 /v1/chat/completions 的服务都能接。",
	},
	{
		ID: "openrouter", Label: "OpenRouter", Vendor: "OpenRouter",
		BaseURL: "https://openrouter.ai/api/v1", Protocol: "chat", ModelPrefix: "or",
		KeyHint: "以 sk-or-v1- 开头", DocURL: "https://openrouter.ai/keys",
		NeedsKey: true,
		Note:     "聚合了多家模型；模型名自带厂商斜杠（如 openai/gpt-4o），前缀与它不冲突。",
	},
	{
		ID: "zhipu", Label: "智谱 BigModel", Vendor: "智谱",
		BaseURL: "https://open.bigmodel.cn/api/paas/v4", Protocol: "chat", ModelPrefix: "zhipu",
		KeyHint: "形如 xxxxxxxx.yyyyyyyy", DocURL: "https://open.bigmodel.cn/usercenter/apikeys",
		NeedsKey: true,
		Note:     "GLM 系列官方接口。若走 Coding Plan，请用对应的反向网关地址。",
	},
	{
		ID: "deepseek", Label: "DeepSeek 官方", Vendor: "DeepSeek",
		BaseURL: "https://api.deepseek.com/v1", Protocol: "chat", ModelPrefix: "ds",
		KeyHint: "以 sk- 开头", DocURL: "https://platform.deepseek.com/api_keys",
		NeedsKey: true,
	},
	{
		ID: "siliconflow", Label: "SiliconFlow 硅基流动", Vendor: "SiliconFlow",
		BaseURL: "https://api.siliconflow.cn/v1", Protocol: "chat", ModelPrefix: "sf",
		KeyHint: "以 sk- 开头", DocURL: "https://cloud.siliconflow.cn/account/ak",
		NeedsKey: true,
	},
	{
		ID: "moonshot", Label: "Moonshot / Kimi", Vendor: "Moonshot",
		BaseURL: "https://api.moonshot.cn/v1", Protocol: "chat", ModelPrefix: "kimi",
		KeyHint: "以 sk- 开头", DocURL: "https://platform.moonshot.cn/console/api-keys",
		NeedsKey: true,
	},
	{
		ID: "anthropic", Label: "Anthropic 官方（Claude）", Vendor: "Anthropic",
		BaseURL: "https://api.anthropic.com", Protocol: "anthropic", ModelPrefix: "claude",
		KeyHint: "以 sk-ant- 开头", DocURL: "https://console.anthropic.com/settings/keys",
		NeedsKey: true,
		Note:     "上游说 Anthropic Messages 协议，由适配层转成 OpenAI 格式对外。",
	},
	{
		ID: "openai", Label: "OpenAI 官方", Vendor: "OpenAI",
		BaseURL: "https://api.openai.com/v1", Protocol: "chat", ModelPrefix: "oai",
		KeyHint: "以 sk-proj- 或 sk- 开头", DocURL: "https://platform.openai.com/api-keys",
		NeedsKey: true,
		Note:     "部分新模型只在新版 Responses 协议下可用；如需可在协议里改选「Responses」。",
	},
	{
		ID: "ollama", Label: "本地 Ollama", Vendor: "本地",
		BaseURL: "http://127.0.0.1:11434/v1", Protocol: "chat", ModelPrefix: "ollama",
		KeyHint: "本地端点通常不需要 Key，留空即可", NeedsKey: false,
		Note: "Ollama 的 OpenAI 兼容层。模型名用 `ollama list` 里的名字。",
	},
	{
		ID: "vllm", Label: "本地 vLLM / LM Studio", Vendor: "本地",
		BaseURL: "http://127.0.0.1:8000/v1", Protocol: "chat", ModelPrefix: "local",
		KeyHint: "本地端点通常不需要 Key，留空即可", NeedsKey: false,
		Note: "任何暴露 /v1/chat/completions 的本地推理服务都可以用这个。",
	},
}

// PresetByID 按 id 查模板。
func PresetByID(id string) (Preset, bool) {
	for _, p := range Presets {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// ManagedPreset 托管型 provider 的模板。
//
// 与内嵌型预设的区别：这里预填的是「装配哪个内置原生实现」以及它的探活 / 面板契约，
// 路由字段（模型前缀、声明模型）由用户在面板里补。两种预设都是模板，保存后没有特殊分支。
//
// 托管型上游**只有进程内原生一种运行方式**（独立子进程模式已整块移除），所以模板
// 不再有 command / args / port_env_var 这些「怎么把子进程拉起来」的字段——需要拉起的
// 只有 Mergence 自己，而上游是以源码内嵌、进程内装配的方式跑的。
type ManagedPreset struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Vendor string `json:"vendor"`
	// HealthPath 就绪探测的 HTTP 路径。原生服务同样走真实 HTTP，所以它对原生依旧有意义。
	HealthPath string `json:"health_path"`
	// PanelPath 它自带面板的路径，供「打开上游面板」用。
	PanelPath string `json:"panel_path,omitempty"`
	// PanelAPIPrefix 它管理 API 的前缀。留空表示用通用默认（/panel/api）；
	// zcode2api / trae 这类网关的管理 API 挂在 /admin/api，与默认值不同。
	//
	// 这是**声明性**字段，不是运行时真源：真正取用的地方一律是 web 层的契约表
	// panelAPIPrefixFor —— 它按 kind 硬编码、不读配置。这里写下来是为了让
	// 「模板声明的前缀」与契约表对得上：两处必须同步改，否则新网关会拿到
	// /panel/api 而静默 404。
	PanelAPIPrefix string `json:"panel_api_prefix,omitempty"`
	ModelPrefix    string `json:"model_prefix"`
	// RouteKeyHint 提示它的路由鉴权 Key 从哪来。
	RouteKeyHint string `json:"route_key_hint,omitempty"`
	DocURL       string `json:"doc_url,omitempty"`
	Note         string `json:"note,omitempty"`
	// CreditValue 该平台每积分价值（元），概览的「积分型费用」估算用。
	// 来自官方公开定价；用户可在渠道设置里覆盖（自购套餐折算价更准）。
	// 0 = 该平台不以积分计费，无默认值。
	CreditValue float64 `json:"credit_value,omitempty"`

	// Kind 内置原生实现名（internal/native 的注册键），也是网关种类
	// （workbuddy / zcode / trae）。**必填**：缺了它无从知道装配谁，配置会被禁用。
	// 它同时决定控制台形态与管理 API 前缀这些契约判定（见 web 层的 kindOfUpstream）。
	Kind string `json:"kind"`
}

// WorkBuddyCreditValue WorkBuddy（腾讯）每积分价值：0.05 元。
//
// 依据（2026-10 官方定价页 codebuddy.cn/docs/workbuddy/Pricing）：
// 企业加量包 2,000 积分 = 100 元 → 0.05 元/积分。
// 这是「单独买积分」的边际价，不随订阅档位浮动，作为默认折算价最稳。
// 订阅档位折算其实更低（标准版 99 元 / 每月实得 4,000 积分 ≈ 0.025），
// 但那部分积分是订阅附带的，把订阅费摊到积分上会低估加量部分的价值，
// 所以默认取加量包口径。用户可按自己实际订阅档位在渠道设置里改。
const WorkBuddyCreditValue = 0.05

// ManagedPresets 托管型模板，按「最容易先跑通」的顺序排列。
var ManagedPresets = []ManagedPreset{
	{
		ID: "workbuddy", Label: "WorkBuddy 网关（内置原生）", Vendor: "WorkBuddy",
		Kind:       "workbuddy",
		HealthPath: "/healthz", PanelPath: "/panel/", ModelPrefix: "wb",
		CreditValue:  WorkBuddyCreditValue,
		RouteKeyHint: "可留空：内置实现默认不校验本地 Key；填了就会在转发时装进 Authorization",
		Note: "跑在 Mergence 进程内，不需要单独准备 wb2api.exe。" +
			"账号数据落在该渠道的数据目录（留空即 运行根/data/instances/<渠道名>/data），" +
			"首次使用需在「控制台」里登录。" +
			"要接管一个**已经在运行**的 wb2api，在环境变量里填 " +
			"MERGENCE_EXTERNAL_URL=http://127.0.0.1:<它的端口> 即可（Mergence 不拉起它）。",
	},
	{
		ID: "zcode", Label: "ZCode 账号网关（内置原生）", Vendor: "ZCode",
		// 与 workbuddy / trae 的**关键区别**：那两个是「MIT 上游 + 逐字照搬」，
		// zcode 是 Track 2 **独立重写**（上游 dengyie/zcode2api 是 AGPL-3.0，
		// 不能照搬）。所以本仓库里没有任何上游代码，也就不需要
		// `src/THIRD-PARTY-LICENSES/zcode/`。
		Kind: "zcode",
		// 探活用 /meta（上游就有的端点，只有 version 一个键）。
		HealthPath: "/meta", PanelPath: "/admin/", ModelPrefix: "zcode-",
		// 管理 API 在 /admin/api（不是通用的 /panel/api）。
		PanelAPIPrefix: "/admin/api",
		RouteKeyHint:   "留空即用默认密码 `zcode`；填了就是唯一的后台密码（面板与转发共用一处）",
		Note: "跑在 Mergence 进程内，不需要单独准备 zcode2api（Python 项目）。" +
			"账号数据落在该渠道的数据目录（留空即 运行根/data/instances/<渠道名>/data），" +
			"首次使用需在「控制台」里登录。" +
			"要接管一个**已经在运行**的 zcode2api，在环境变量里填 " +
			"MERGENCE_EXTERNAL_URL=http://127.0.0.1:<它的端口> 即可（Mergence 不拉起它）。" +
			"**密码只剩一处**：渠道的「路由密钥」就是后台密码（留空用默认 `zcode`），" +
			"不再需要像独立部署那样两处同步。",
	},
	{
		ID: "trae", Label: "Trae 网关（内置原生）", Vendor: "Trae",
		// 内置原生：跑在 Mergence 进程内（上游 connectedGraph/trae2api-web，MIT 照搬）。
		// 它自带账号池调度 + 冷却状态机 + 每日签到 + 登录闭环。
		Kind:       "trae",
		HealthPath: "/healthz", PanelPath: "/admin", ModelPrefix: "trae",
		// 管理 API 在 /admin/api（与 zcode 同前缀，不是通用的 /panel/api）。
		PanelAPIPrefix: "/admin/api",
		RouteKeyHint:   "可留空：内置实现默认不校验本地 Key；填了就会在转发与签到时装进 Authorization",
		DocURL:         "https://github.com/connectedGraph/trae2api-web",
		Note: "跑在 Mergence 进程内，不需要单独准备 Node 网关。" +
			"账号池、每日签到与登录闭环都由内置实现承担：" +
			"在「控制台」里用设备码/回调链接登录即可添加账号，" +
			"「限时套餐自动领取」会定时触发它内部的签到。" +
			"登录回调需要一个固定端口（默认 18080，见数据目录下的 config.json）。",
	},
}

// ManagedPresetByID 按 id 查托管型模板。
func ManagedPresetByID(id string) (ManagedPreset, bool) {
	for _, p := range ManagedPresets {
		if p.ID == id {
			return p, true
		}
	}
	return ManagedPreset{}, false
}

// ManagedPresetByKind 按网关种类（= 原生实现名）查托管型模板。
//
// 用途是渠道的模板回显：kind 本就是内置实现名、与模板 id 一一对应
// （workbuddy / zcode / trae），照着查即可，不必去猜。
func ManagedPresetByKind(kind string) (ManagedPreset, bool) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return ManagedPreset{}, false
	}
	for _, p := range ManagedPresets {
		if p.ID == kind || p.Kind == kind {
			return p, true
		}
	}
	return ManagedPreset{}, false
}
