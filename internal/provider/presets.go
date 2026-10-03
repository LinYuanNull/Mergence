// presets.go 渠道预设模板。
//
// 预设只是「预填了一组字段」的模板，选中后用户仍可改任何一项——它不是特殊类型，
// 保存后就是一个普通的 EmbeddedProvider。这样接入后与手填渠道完全同权，
// 不需要在路由/额度/日志里为「预设渠道」留任何分支。
package provider

import (
	"strings"

	"modelmux/internal/config"
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

// ApplyPreset 用模板填充一个渠道（仅填空字段，不覆盖已填内容）。
func ApplyPreset(p Preset, name string) config.EmbeddedProvider {
	ch := config.EmbeddedProvider{
		Name:        name,
		Enabled:     true,
		Preset:      p.ID,
		Protocol:    p.Protocol,
		BaseURL:     p.BaseURL,
		ModelPrefix: p.ModelPrefix,
		ModelSource: "auto",
		Weight:      1,
	}
	if ch.DisplayName == "" {
		ch.DisplayName = p.Label
	}
	return ch
}

// ManagedPreset 托管型 provider 的模板。
//
// 与内嵌型预设的区别：这里预填的是「怎么把子进程拉起来」，而路由字段
// （模型前缀、声明模型）由用户在面板里补。两种预设都是模板，保存后没有特殊分支。
type ManagedPreset struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Vendor  string   `json:"vendor"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	// PortEnvVar 该 provider 原生的端口环境变量名。填了才能用动态端口。
	PortEnvVar string `json:"port_env_var,omitempty"`
	HealthPath string `json:"health_path"`
	// PanelPath 它自带面板的路径。
	PanelPath string `json:"panel_path,omitempty"`
	// PanelAPIPrefix 它管理 API 的前缀。留空表示用通用默认（/panel/api）；
	// zcode2api 这类网关的管理 API 挂在 /admin/api，与默认值不同，在这里
	// 写死，新建渠道时直接带上正确值（老配置由 web 层的契约表兜住）。
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
		ID: "workbuddy", Label: "WorkBuddy 网关（wb2api）", Vendor: "WorkBuddy",
		Command: "wb2api.exe", PortEnvVar: "WB2A_LISTEN",
		HealthPath: "/healthz", PanelPath: "/panel/", ModelPrefix: "wb",
		CreditValue:  WorkBuddyCreditValue,
		RouteKeyHint: "填它 config.json 里的 api_key",
		Note: "把「工作目录」指到 wb2api 所在目录（含 auths 与 config.json）。" +
			"它会复用该目录的账号，但状态文件建议另指一份，避免与单独运行的实例互相覆盖。",
	},
	{
		ID: "new-api", Label: "new-api（多渠道聚合底座）", Vendor: "new-api",
		Command: "new-api.exe", PortEnvVar: "PORT",
		HealthPath: "/api/status", PanelPath: "/", ModelPrefix: "newapi",
		RouteKeyHint: "填 new-api 里创建的令牌（sk-…）",
		DocURL:       "https://github.com/QuantumNous/new-api",
		Note:         "AGPL-3.0。只以独立进程方式调用、不随包分发，以免影响本项目自身的许可。",
	},
	{
		ID: "zcode", Label: "ZCode 账号网关（zcode2api）", Vendor: "zcode2api",
		// 它是 Python 项目（AGPL-3.0），不是单个 exe：入口是 cli.py 的 serve 子命令。
		// 用托管型拉起时把「启动命令」填成 python、参数填 cli.py serve。
		Command: "python", Args: []string{"cli.py", "serve"},
		PortEnvVar: "ZCODE_PORT",
		// 探活用 /meta（它没有 /healthz）；后台在 /admin/。
		HealthPath: "/meta", PanelPath: "/admin/", ModelPrefix: "zcode-",
		// 管理 API 在 /admin/api（不是通用的 /panel/api）。
		PanelAPIPrefix: "/admin/api",
		RouteKeyHint:   "留空即可：网关默认不校验；也可在它的设置页配网关 Key 后填在这里",
		DocURL:         "https://github.com/dengyie/zcode2api",
		Note: "AGPL-3.0，只以独立进程方式调用、不随包分发。" +
			"它是 Python 项目：启动命令填它 venv 里的 python.exe，参数填 cli.py serve。" +
			"它自带 600 秒一轮的套餐领取；要按固定时段领，把它 .env 里的 " +
			"ZCODE_CLAIM_ROUND_INTERVAL 设为 0 关掉，再交给 ModelMux 的" +
			"「限时套餐自动领取」（后台密码填它的 ZCODE_ADMIN_KEY，渠道留空即自动识别）。",
	},
	{
		ID: "trae", Label: "Trae 本地网关", Vendor: "Trae",
		Command: "node", Args: []string{"server.js"}, PortEnvVar: "PORT",
		HealthPath: "/healthz", PanelPath: "/", ModelPrefix: "trae",
		RouteKeyHint: "若它要求本地 Key，按它文档填写",
		Note:         "Node 实现：命令填 node，参数填入口脚本（相对工作目录）。",
	},
	{
		ID: "custom-managed", Label: "自定义托管进程（空白）", Vendor: "自定义",
		HealthPath: "/healthz", ModelPrefix: "",
		Note: "任何暴露 OpenAI 兼容端点、且能用环境变量指定端口的可执行程序都能接。",
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

// InferManagedPreset 从一份托管型配置里推断它「由哪个模板创建」。
//
// 这个函数只服务于面板回显：老配置里没有存 preset（ManagedProvider.Preset 是
// 后加的字段），编辑时下拉框要是取不到值就会停在第一个选项上，把 zcode 之类
// 的渠道显示成 workbuddy。它不参与任何运行时逻辑，推断错了也只影响下拉框的
// 初始选中项，不影响子进程怎么拉起、请求怎么路由。
//
// 为什么用「打分取最高分」而不是「第一个命中就返回」：
//
//	这些模板在字段上互相重叠——workbuddy 与 trae 的 health_path 都是 /healthz，
//	custom-managed 也是。谁先排在前面谁就被选中，而 ManagedPresets 的顺序是
//	按「最容易先跑通」排的（workbuddy 第一），于是「第一个命中」会把一切都判成
//	workbuddy。改顺序又会动到前端依赖的顺序，所以顺序不能当判别依据。
//	改成打分后，每个候选按它命中的信号累加，命中越多越可信；再用阈值兜底。
//
// 为什么门槛是 health_path 必须相同：
//
//	command 会被用户改成任意路径、args 可能为空、port_env_var 常被留空，只有
//	health_path 是「探活必须写对、且各模板取值互不相同」的字段，是最稳定的锚点。
//	没有它做门槛时，单凭空的 command/args 很容易误命中 custom-managed。所以
//	health_path 不匹配的候选一律淘汰；都匹配不了就返回 "" 表示「判不出来」。
//
// 打分权重（均为命中即累加）：
//
//	health_path 相同且非空          +3（同时充当门槛）
//	port_env_var 相同且非空         +2
//	args 完全相同（nil 与空切片同） +2
//	command 归一化后相同            +2
//
// 最高分并列时返回 ""：两个模板一样像，说明信号不足以区分，宁可让面板停在
// 第一个选项，也不随机猜一个。
func InferManagedPreset(command string, args []string, healthPath, portEnvVar string) string {
	healthPath = strings.TrimSpace(healthPath)
	portEnvVar = strings.TrimSpace(portEnvVar)
	wantCmd := normalizeCommand(command)

	bestID := ""
	bestScore := -1
	tied := false
	for _, p := range ManagedPresets {
		// 门槛：health_path 必须非空且一致，否则这个候选直接淘汰。
		if healthPath == "" || p.HealthPath != healthPath {
			continue
		}
		score := 3

		if portEnvVar != "" && p.PortEnvVar == portEnvVar {
			score += 2
		}
		if sameArgs(p.Args, args) {
			score += 2
		}
		if normalizeCommand(p.Command) == wantCmd {
			score += 2
		}

		switch {
		case score > bestScore:
			bestScore = score
			bestID = p.ID
			tied = false
		case score == bestScore:
			tied = true
		}
	}

	if tied {
		return ""
	}
	return bestID
}

// normalizeCommand 把一条启动命令归一化成可比较的短名。
//
// 取 basename（兼容 / 与 \ 两种分隔符）、转小写、去掉 .exe 后缀，并把
// python3 视作 python——因为同一个解释器在不同项目里可能写成
// `D:/x/.venv/Scripts/python.exe`、`python3` 或 `python`，它们指向的是同一种东西。
func normalizeCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}
	cmd = strings.ReplaceAll(cmd, "\\", "/")
	if i := strings.LastIndex(cmd, "/"); i >= 0 {
		cmd = cmd[i+1:]
	}
	cmd = strings.ToLower(cmd)
	cmd = strings.TrimSuffix(cmd, ".exe")
	if cmd == "python3" {
		cmd = "python"
	}
	return cmd
}

// sameArgs 比较两组启动参数，nil 与空切片视为相同。
func sameArgs(a, b []string) bool {
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
