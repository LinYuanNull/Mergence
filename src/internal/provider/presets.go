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
//
// 「托管」现在含两种运行方式（Mode），模板负责预填是哪一种：独立子进程要
// command/args/port_env_var，进程内原生不要这些、要的是 Mode+Kind。
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
	// zcode2api / trae 这类网关的管理 API 挂在 /admin/api，与默认值不同。
	//
	// 这是**声明性**字段，不是运行时真源：面板表单里那个「管理 API 前缀」
	// 输入框的值后端并不接收（channelInput 没有对应字段），保存往返会把它丢掉。
	// 真正取用的地方一律是 web 层的契约表 panelAPIPrefixFor —— 它按 kind 硬编码、
	// 不读配置。这里写下来是为了让「模板声明的前缀」与契约表对得上：
	// 两处必须同步改，否则新网关会拿到 /panel/api 而静默 404。
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

	// Mode 该模板创建出来的渠道以什么方式运行：空 = 独立子进程；
	// config.ModeNative = 进程内原生（不需要 command，前端会把 command 留空，
	// 并把 mode/kind 一起提交，见 web/app.js 的 applyPreset/collectForm）。
	Mode string `json:"mode,omitempty"`
	// Kind 原生实现名（internal/native 的注册键），也是网关种类（workbuddy / zcode / trae）。
	// 只在 Mode= native 时有意义。
	Kind string `json:"kind,omitempty"`

	// LegacyCommands 仅用于 InferManagedPreset 的锚点匹配，不下发给前端（json:"-"）。
	//
	// 为什么需要它：原生型模板没有 command（进程内运行没有可执行文件），可**老配置里
	// 同名渠道是子进程形态**，command 正是这些值。留着它们，面板才能把老渠道
	// 正确回显成本模板；否则推断会退化成「自定义托管进程（空白）」，
	// 用户以为自己的渠道配置丢了。
	LegacyCommands []string `json:"-"`
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
		// 内置原生：跑在 ModelMux 进程内，不再需要 wb2api.exe。
		Mode:       config.ModeNative,
		Kind:       "workbuddy",
		HealthPath: "/healthz", PanelPath: "/panel/", ModelPrefix: "wb",
		CreditValue:    WorkBuddyCreditValue,
		RouteKeyHint:   "可留空：内置实现默认不校验本地 Key；填了就会在转发时装进 Authorization",
		LegacyCommands: []string{"wb2api", "workbuddy2api"},
		Note: "跑在 ModelMux 进程内，不再需要单独准备 wb2api.exe。" +
			"把「数据目录」指到你原来 wb2api 的目录（含 auths 与 config.json）即可沿用已登录的账号；" +
			"留空则用独立的新目录，需要在「控制台」里重新登录。" +
			"注意它与子进程形态的 workbuddy 渠道是**同一个平台**，不能同时启用，迁移时先删掉旧渠道。",
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
		ID: "zcode", Label: "ZCode 账号网关（内置原生）", Vendor: "ZCode",
		// 内置原生：跑在 ModelMux 进程内，不再需要独立部署 zcode2api（Python）。
		//
		// 与 workbuddy / trae 的**关键区别**：那两个是「MIT 上游 + 逐字照搬」，
		// zcode 是 Track 2 **独立重写**（上游 dengyie/zcode2api 是 AGPL-3.0，
		// 不能照搬）。所以本仓库里没有任何上游代码，也就不需要
		// `src/THIRD-PARTY-LICENSES/zcode/`。
		Mode: config.ModeNative,
		Kind: "zcode",
		// 探活用 /meta（上游就有的端点，只有 version 一个键）。
		HealthPath: "/meta", PanelPath: "/admin/", ModelPrefix: "zcode-",
		// 管理 API 在 /admin/api（不是通用的 /panel/api）。
		PanelAPIPrefix: "/admin/api",
		RouteKeyHint:   "留空即用默认密码 `zcode`；填了就是唯一的后台密码（面板与转发共用一处）",
		// 老模板的 command 是 `python`（解释器名）—— 不能当锚点（任何用 python
		// 启动的渠道都会被误判）。改用独有参数串 `cli.py serve` 作锚点。
		LegacyCommands: []string{"cli.py serve"},
		Note: "跑在 ModelMux 进程内，不再需要单独准备 zcode2api（Python 项目）。" +
			"把「数据目录」指到你原来 zcode2api 的 data/（含 accounts.db）即可沿用已登录的账号；" +
			"留空则用独立的新目录，需要在「控制台」里重新登录。" +
			"**密码只剩一处**：渠道的「路由密钥」就是后台密码（留空用默认 `zcode`），" +
			"不再需要像独立部署那样两处同步。" +
			"注意它与子进程形态的 zcode 渠道是**同一个平台**，不能同时启用，迁移时先删掉旧渠道。",
	},
	{
		ID: "trae", Label: "Trae 网关（内置原生）", Vendor: "Trae",
		// 内置原生：跑在 ModelMux 进程内（上游 connectedGraph/trae2api-web，MIT 照搬）。
		// 它自带账号池调度 + 冷却状态机 + 每日签到 + 登录闭环。
		Mode:       config.ModeNative,
		Kind:       "trae",
		HealthPath: "/healthz", PanelPath: "/admin", ModelPrefix: "trae",
		// 管理 API 在 /admin/api（与 zcode 同前缀，不是通用的 /panel/api）。
		PanelAPIPrefix: "/admin/api",
		RouteKeyHint:   "可留空：内置实现默认不校验本地 Key；填了就会在转发与签到时装进 Authorization",
		DocURL:         "https://github.com/connectedGraph/trae2api-web",
		// 刻意**不**设 LegacyCommands：老 trae 模板的 command 是 `node`（解释器名），
		// 拿它当锚点会把任何用 node 启动的渠道都误判成 trae。workbuddy 能用
		// `wb2api`/`workbuddy2api` 作锚点是因为那是它独有的可执行文件名，trae 没有
		// 对应的独有名字，所以宁可让老渠道回显为空，也不引入误判。
		Note: "跑在 ModelMux 进程内，不再需要单独准备 Node 网关。" +
			"账号池、每日签到与登录闭环都由内置实现承担：" +
			"在「控制台」里用设备码/回调链接登录即可添加账号，" +
			"「限时套餐自动领取」会定时触发它内部的签到。" +
			"登录回调需要一个固定端口（默认 18080，见数据目录下的 config.json）。" +
			"注意它与子进程形态的 trae 渠道是**同一个平台**，不能同时启用，迁移时先删掉旧渠道。",
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

// ManagedPresetByKind 按网关种类（= 原生实现名）查托管型模板。
//
// 用途是原生型渠道的模板回显：它没有 command/args 可作推断锚点，而它的 kind
// 本就是内置实现名、与模板 id 一一对应（workbuddy / zcode / trae），
// 照着查即可，不必去猜。
func ManagedPresetByKind(kind string) (ManagedPreset, bool) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return ManagedPreset{}, false
	}
	for _, p := range ManagedPresets {
		if p.ID == kind || (p.Mode == config.ModeNative && p.Kind == kind) {
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
		if presetCommandMatch(p, wantCmd) {
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

// presetCommandMatch 该模板是否「认」这个启动命令。
//
// 除了模板自己的 command，还要看 LegacyCommands：原生型模板没有 command
// （进程内运行没有可执行文件），但它的**前身**正是老配置里的子进程形态，
// 那些渠道的 command 是 wb2api.exe 这类值。少了这一步，老渠道的模板回显会
// 退化成「自定义托管进程（空白）」，用户会以为自己填的东西丢了。
//
// 空 command 单独处理，且**只**认「子进程型、且从不需要 command」的模板
// （即自定义托管进程）。两个条件都不能少：
//
//   - 带 LegacyCommands 的模板（workbuddy）对应「曾经有 command」的形态，
//     让空值命中它会把一个空白渠道显示成某个具体网关——那正是要修的 bug 的镜像。
//   - **原生型模板（Mode=native）同样要排除**。它也不需要 command，但它不是
//     「空白自定义」，而是一个有明确身份的内置实现；它的回显走 kind
//     （ManagedPresetByKind），不走这条推断。不排除的话，空 command 渠道会
//     同时命中「内置原生 trae」与「自定义托管进程」而并列，把本来能判出来的
//     custom-managed 也拖成「判不出」。
func presetCommandMatch(p ManagedPreset, wantCmd string) bool {
	if wantCmd == "" {
		return normalizeCommand(p.Command) == "" && len(p.LegacyCommands) == 0 &&
			p.Mode != config.ModeNative
	}
	if normalizeCommand(p.Command) == wantCmd {
		return true
	}
	for _, c := range p.LegacyCommands {
		if normalizeCommand(c) == wantCmd {
			return true
		}
	}
	return false
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
