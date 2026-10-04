// api_channels.go 渠道增删改查 + 保存前测试连接。
//
// 「渠道」在这里统一指两类可路由上游：
//   - 内嵌型（embedded）：Mergence 直接向一个 OpenAI 兼容端点发请求
//   - 托管型（managed）：上游是 Mergence 拉起的独立子进程
//
// 两类共用一套路由与协议适配（见 provider.Upstream），所以管理接口也共用一套：
// 只有「怎么把上游拉起来」这一段不同，其余字段与行为完全一致。
//
// 所有写操作都走同一条路径：改内存配置 → 序列化 → config.Parse（校验 + 归一化）
// → 原子落盘 → 重载注册表。这样「磁盘上的配置」与「运行时行为」永远一致，
// 不会出现「面板显示改了但实际没生效」。
package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"mergence/internal/config"
	"mergence/internal/orchestrator"
	"mergence/internal/provider"
)

// accountCountTimeout 问上游账号数的超时。给得比面板代理短：
// 它跑在渠道列表的渲染路径上，慢一点就拖住整个列表。
const accountCountTimeout = 3 * time.Second

// 渠道来源。
const (
	kindEmbedded = "embedded"
	kindManaged  = "managed"
)

// 网关种类（配置里落盘的识别结果）。newapi 特判：它天生支持多实例。
const gatewayKindNewAPI = "newapi"

// handlePresets 返回内置模板（内嵌型 + 托管型）。
func (s *Server) handlePresets(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"presets":         provider.Presets,
		"managed_presets": provider.ManagedPresets,
	})
}

// handleChannels 渠道列表（GET）与新增/修改（POST）。
func (s *Server) handleChannels(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.syncRegistry()
		writeJSON(w, map[string]any{
			"channels": s.channelStatuses(),
			"total":    len(s.reg.Channels()),
			"enabled":  len(s.reg.Enabled()),
			"ready":    s.readyCount(),
		})
	case http.MethodPost:
		s.upsertChannel(w, r)
	case http.MethodDelete:
		s.deleteChannel(w, r)
	default:
		writeJSONStatus(w, http.StatusMethodNotAllowed, map[string]any{
			"error": "仅支持 GET / POST / DELETE",
		})
	}
}

func (s *Server) readyCount() int {
	n := 0
	for _, c := range s.reg.Enabled() {
		if c.Ready() {
			n++
		}
	}
	return n
}

// channelInput 面板提交的渠道数据。
//
// 刻意用**一个扁平结构**承载两类渠道，而不是嵌两个结构体：内嵌型与托管型有
// 大量同名字段（name / enabled / models / prefix…），嵌两个会让字段选择器产生歧义，
// 反而要在各处写重复的前缀。用 kind 决定「哪些字段生效」更直白。
type channelInput struct {
	Kind         string `json:"kind"`
	OriginalName string `json:"original_name,omitempty"`

	// ── 两类共用
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Enabled     bool   `json:"enabled"`
	Preset      string `json:"preset,omitempty"`
	// GatewayKind 识别出来的网关种类（zcode / workbuddy / newapi）。
	// 与上面的 Kind（内嵌/托管）是两回事，别混。
	GatewayKind string            `json:"gateway_kind,omitempty"`
	Protocol    string            `json:"protocol"`
	ModelPrefix string            `json:"model_prefix"`
	ModelSource string            `json:"model_source,omitempty"`
	Models      config.ModelList  `json:"models"`
	Headers     map[string]string `json:"headers"`
	Weight      int               `json:"weight"`
	Priority    int               `json:"priority"`
	Timeout     string            `json:"timeout"`
	Retries     int               `json:"retries"`
	HealthPath  string            `json:"health_path"`
	ModelsPath  string            `json:"models_path"`
	APIKeys     []string          `json:"api_keys"`

	// ── 内嵌型专有
	BaseURL string `json:"base_url"`
	Proxy   string `json:"proxy"`
	// Price 自定义单价（美元/百万 token）。留空则用内置价格表。
	Price config.ChannelPrice `json:"price"`

	// ── 托管型专有
	// Mode 运行方式：空 / "process" = 独立子进程；"native" = 进程内原生。
	// 老配置没有这个字段 ⇒ 空值必须解释成子进程，见 config.ManagedProvider.Mode。
	Mode          string   `json:"mode,omitempty"`
	Command       string   `json:"command"`
	Args          []string `json:"args"`
	Dir           string   `json:"dir"`
	DataDir       string   `json:"data_dir"`
	PortEnvVar    string   `json:"port_env_var"`
	FixedPort     int      `json:"fixed_port"`
	ReadyTimeout  string   `json:"ready_timeout"`
	ShutdownGrace string   `json:"shutdown_grace"`
	PanelPath     string   `json:"panel_path"`
	// PathPrefix 上游 API 的路径前缀，缺省 /v1；显式 "" 表示没有前缀。
	PathPrefix *string           `json:"path_prefix"`
	Env        map[string]string `json:"env"`
	// Expose 是否把该子进程注册为可路由渠道。false = 只托管、不出现在模型列表里。
	Expose bool `json:"expose"`

	// CreditValue 每积分价值（元），积分型平台的实际花费折算用。0 = 未配置。
	CreditValue float64 `json:"credit_value"`

	// PresetInferred 仅用于**出参**（handleChannelRaw）：为 true 表示返回的
	// Preset 是按当前 command/args/health_path 推断出来的，而不是配置里记的值
	// （老配置没有 preset 字段）。前端据此可区分「模板回显」与「推断回显」。
	// 入参里带上它也无副作用——保存时只取 Preset，不会写这个标志。
	PresetInferred bool `json:"preset_inferred,omitempty"`
}

func (in *channelInput) kind() string {
	if in.Kind == kindManaged {
		return kindManaged
	}
	return kindEmbedded
}

// toEmbedded 组装内嵌型配置。
func (in *channelInput) toEmbedded() config.EmbeddedProvider {
	return config.EmbeddedProvider{
		Name: in.Name, DisplayName: in.DisplayName, Enabled: in.Enabled,
		Preset: in.Preset, BaseURL: in.BaseURL, APIKeys: in.APIKeys,
		Protocol: in.Protocol, ModelPrefix: in.ModelPrefix, ModelSource: in.ModelSource,
		Models: in.Models, Headers: in.Headers, Weight: in.Weight, Priority: in.Priority,
		Timeout: in.Timeout, Retries: in.Retries, HealthPath: in.HealthPath,
		ModelsPath: in.ModelsPath, Proxy: in.Proxy, Price: in.Price,
	}
}

// toManaged 组装托管型配置。expose=false 时 Route 为 nil（只托管、不路由）。
func (in *channelInput) toManaged() config.ManagedProvider {
	m := config.ManagedProvider{
		Name: in.Name, DisplayName: in.DisplayName, Enabled: in.Enabled,
		// Preset 只是「由哪个模板创建」的回显标记，不参与运行时；这里必须原样
		// 存下来，否则老配置之外的**新保存**渠道也会丢 preset，编辑时下拉框只能
		// 靠推断。与 toEmbedded 的处理保持一致。
		Preset: in.Preset,
		// GatewayKind 落盘识别结果，供后续判重与面板代理使用。
		Kind: in.GatewayKind,
		// Mode 决定它是子进程还是进程内原生；空值即子进程（见 config 的同名字段）。
		Mode:    in.Mode,
		Command: in.Command, Args: in.Args, Dir: in.Dir, DataDir: in.DataDir,
		PortEnvVar: in.PortEnvVar, FixedPort: in.FixedPort,
		HealthPath: in.HealthPath, ReadyTimeout: in.ReadyTimeout,
		ShutdownGrace: in.ShutdownGrace, PanelPath: in.PanelPath, Env: in.Env,
		CreditValue: in.CreditValue,
	}
	if in.Expose {
		r := &config.RouteSpec{
			ModelPrefix: in.ModelPrefix, Models: in.Models,
			Protocol: in.Protocol, Headers: in.Headers,
			Weight: in.Weight, Priority: in.Priority,
			Timeout: in.Timeout, Retries: in.Retries,
			ModelsPath: in.ModelsPath, PathPrefix: in.PathPrefix,
		}
		if len(in.APIKeys) > 0 {
			r.APIKey = in.APIKeys[0]
		}
		m.Route = r
	}
	return m
}

func (s *Server) upsertChannel(w http.ResponseWriter, r *http.Request) {
	var in channelInput
	if err := readJSONBody(r, 1<<20, &in); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if in.kind() == kindManaged {
		s.upsertManaged(w, &in)
		return
	}
	s.upsertEmbedded(w, &in)
}

// upsertEmbedded 新增/修改内嵌型渠道。
func (s *Server) upsertEmbedded(w http.ResponseWriter, in *channelInput) {
	in.Name = strings.TrimSpace(in.Name)

	cur := s.currentConfig()

	// Key 沿用：编辑已有渠道时若请求体里**没有** api_keys 字段（Go 里是 nil slice），
	// 说明用户没动 Key 输入框，沿用已保存的值。显式传 [] 才表示清空。
	if in.APIKeys == nil {
		target := firstNonEmptyStr(strings.TrimSpace(in.OriginalName), in.Name)
		if old := findEmbedded(cur, target); old != nil {
			in.APIKeys = old.APIKeys
		}
	}

	if in.Name == "" {
		in.Name = s.uniqueChannelName(cur, in.DisplayName, in.Preset)
	} else if in.OriginalName != "" && in.OriginalName != in.Name && s.nameTaken(cur, in.Name) {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf("渠道名 %q 已被占用", in.Name),
		})
		return
	}

	want := in.toEmbedded()
	next := make([]config.EmbeddedProvider, 0, len(cur.Embedded)+1)
	replaced := false
	for _, c := range cur.Embedded {
		switch {
		case in.OriginalName != "" && c.Name == in.OriginalName:
			next = append(next, want)
			replaced = true
		case c.Name == want.Name:
			next = append(next, want)
			replaced = true
		default:
			next = append(next, c)
		}
	}
	if !replaced {
		next = append(next, want)
	}

	saved, warns, err := s.saveEmbedded(next)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// 用户明确要求启用、但归一化把它禁用了 —— 说明配置有硬冲突（前缀重复、
	// 没填 base_url）。这种情况必须回退并说明原因，绝不能「存下来但偷偷变成禁用」。
	if want.Enabled {
		if ch := findEmbedded(saved, want.Name); ch != nil && !ch.Enabled {
			_, _, _ = s.saveEmbedded(cur.Embedded) // 回滚
			writeJSONStatus(w, http.StatusConflict, map[string]any{
				"error": "渠道未能启用，请先解决以下问题", "warns": warns,
			})
			return
		}
	}

	s.syncRegistry()
	writeJSON(w, map[string]any{
		"ok": true, "warns": warns, "name": want.Name,
		"channels": s.channelStatuses(),
	})
}

// upsertManaged 新增/修改托管型 provider。
func (s *Server) upsertManaged(w http.ResponseWriter, in *channelInput) {
	in.Name = strings.TrimSpace(in.Name)
	cur := s.currentConfig()

	// Key 沿用（语义同内嵌型）
	if in.APIKeys == nil {
		target := firstNonEmptyStr(strings.TrimSpace(in.OriginalName), in.Name)
		if old := findManaged(cur, target); old != nil && old.Route != nil && old.Route.APIKey != "" {
			in.APIKeys = []string{old.Route.APIKey}
		}
	}

	if in.Name == "" {
		in.Name = s.uniqueChannelName(cur, in.DisplayName, in.Preset)
	} else if in.OriginalName != "" && in.OriginalName != in.Name && s.nameTaken(cur, in.Name) {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf("渠道名 %q 已被占用", in.Name),
		})
		return
	}

	// 识别网关种类：前端只在「编辑既有渠道」时才回带 gateway_kind，新建时通常是空的。
	// 所以这里必须自己按名称/预设推断一次——否则新建路径完全不判重，
	// 用户不传就能绕过（实测：这正是漏掉的那一半）。
	if in.GatewayKind == "" {
		in.GatewayKind = gatewayKindOf(config.ManagedProvider{
			Name: in.Name, DisplayName: in.DisplayName, Preset: in.Preset,
			Command: in.Command, Args: in.Args,
		})
	}

	// 平台唯一性：同一个网关种类只允许一个实例。
	//
	// 放在这里而不是 normalize：normalize 只有「禁用重复项」的力度（且只查 name），
	// 而这里能给用户一句人话，明确告诉他「这个平台已经有了」。newapi 豁免。
	// skipName 回退到 in.Name：同名提交就是「编辑自己」，不是新建。
	// 不回退的话，直接调 API（不传 original_name）编辑一个渠道会被判成
	// 「又加了一个同种平台」而 409 —— 唯一性校验把自己拦住了。
	skipName := firstNonEmptyStr(in.OriginalName, in.Name)
	if in.GatewayKind != "" && gatewayKindTaken(cur, in.GatewayKind, skipName) {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf("已存在一个 %s 平台，不能重复添加（new-api 除外，它支持多个实例）",
				gatewayLabel(in.GatewayKind)),
		})
		return
	}

	want := in.toManaged()
	next := make([]config.ManagedProvider, 0, len(cur.Managed)+1)
	replaced := false
	for _, c := range cur.Managed {
		switch {
		case in.OriginalName != "" && c.Name == in.OriginalName:
			next = append(next, want)
			replaced = true
		case c.Name == want.Name:
			next = append(next, want)
			replaced = true
		default:
			next = append(next, c)
		}
	}
	if !replaced {
		next = append(next, want)
	}

	saved, warns, err := s.saveManaged(next)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	if want.Enabled && want.Route != nil {
		if m := findManaged(saved, want.Name); m != nil && (m.Route == nil || !m.Enabled) {
			_, _, _ = s.saveManaged(cur.Managed) // 回滚
			writeJSONStatus(w, http.StatusConflict, map[string]any{
				"error": "渠道未能启用，请先解决以下问题", "warns": warns,
			})
			return
		}
	}

	// 配置已变：重启子进程让改动生效（命令/参数/环境变量只有重启才吃得到）
	var startErr string
	if want.Enabled {
		if err := s.restartManaged(want.Name); err != nil {
			startErr = err.Error()
		}
	} else {
		_ = s.orch.Stop(want.Name)
	}

	s.syncRegistry()
	writeJSON(w, map[string]any{
		"ok": true, "warns": warns, "name": want.Name,
		"start_error": startErr,
		"channels":    s.channelStatuses(),
	})
}

func (s *Server) deleteChannel(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "缺少 name 参数"})
		return
	}
	cur := s.currentConfig()

	if findManaged(cur, name) != nil {
		_ = s.orch.Stop(name) // 先停进程，再摘配置
		next := make([]config.ManagedProvider, 0, len(cur.Managed))
		for _, c := range cur.Managed {
			if c.Name != name {
				next = append(next, c)
			}
		}
		if _, _, err := s.saveManaged(next); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		s.lg.Info("托管型渠道已删除", "provider", name)
		s.syncRegistry()
		writeJSON(w, map[string]any{"ok": true, "channels": s.channelStatuses()})
		return
	}

	next := make([]config.EmbeddedProvider, 0, len(cur.Embedded))
	found := false
	for _, c := range cur.Embedded {
		if c.Name == name {
			found = true
			continue
		}
		next = append(next, c)
	}
	if !found {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "渠道不存在：" + name})
		return
	}
	if _, _, err := s.saveEmbedded(next); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.lg.Info("内嵌型渠道已删除", "provider", name)
	s.syncRegistry()
	writeJSON(w, map[string]any{"ok": true, "channels": s.channelStatuses()})
}

// handleChannelToggle 启用 / 停用单个渠道。
//
// 对托管型来说「停用」不只是改个标志位 —— 得把子进程也停掉，否则它会继续
// 占着端口、继续消耗上游额度，而面板显示「已停用」，属于典型的言行不一。
func (s *Server) handleChannelToggle(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if err := readJSONBody(r, 64<<10, &in); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	cur := s.currentConfig()

	if m := findManaged(cur, in.Name); m != nil {
		s.toggleManaged(w, cur, *m, in.Enabled)
		return
	}

	next := make([]config.EmbeddedProvider, 0, len(cur.Embedded))
	found := false
	for _, c := range cur.Embedded {
		if c.Name == in.Name {
			c.Enabled = in.Enabled
			found = true
		}
		next = append(next, c)
	}
	if !found {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "渠道不存在：" + in.Name})
		return
	}

	saved, warns, err := s.saveEmbedded(next)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if in.Enabled {
		if ch := findEmbedded(saved, in.Name); ch != nil && !ch.Enabled {
			_, _, _ = s.saveEmbedded(cur.Embedded)
			writeJSONStatus(w, http.StatusConflict, map[string]any{
				"error": "无法启用该渠道，请先解决以下问题", "warns": warns,
			})
			return
		}
	}
	s.syncRegistry()
	writeJSON(w, map[string]any{"ok": true, "warns": warns, "channels": s.channelStatuses()})
}

func (s *Server) toggleManaged(w http.ResponseWriter, cur *config.Config, m config.ManagedProvider, enabled bool) {
	next := make([]config.ManagedProvider, 0, len(cur.Managed))
	for _, c := range cur.Managed {
		if c.Name == m.Name {
			c.Enabled = enabled
		}
		next = append(next, c)
	}
	saved, warns, err := s.saveManaged(next)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if enabled {
		if mm := findManaged(saved, m.Name); mm != nil && !mm.Enabled {
			_, _, _ = s.saveManaged(cur.Managed)
			writeJSONStatus(w, http.StatusConflict, map[string]any{
				"error": "无法启用该渠道，请先解决以下问题", "warns": warns,
			})
			return
		}
	}

	// 配置先落盘（用户的意图必须被记住），进程启停失败单独汇报。
	// 反过来「因为起不来就不保存」会让用户以为改动丢了。
	var startErr string
	if enabled {
		if err := s.restartManaged(m.Name); err != nil {
			startErr = err.Error()
		}
	} else if err := s.orch.Stop(m.Name); err != nil {
		startErr = err.Error()
	}

	s.syncRegistry()
	writeJSON(w, map[string]any{
		"ok": true, "warns": warns, "start_error": startErr,
		"channels": s.channelStatuses(),
	})
}

// handleChannelAction 托管型渠道的启动 / 停止 / 重启。
func (s *Server) handleChannelAction(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name   string `json:"name"`
		Action string `json:"action"` // start | stop | restart
	}
	if err := readJSONBody(r, 64<<10, &in); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	cur := s.currentConfig()
	m := findManaged(cur, in.Name)
	if m == nil {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{
			"error": "不是托管型渠道：" + in.Name,
		})
		return
	}

	var err error
	switch in.Action {
	case "stop":
		err = s.orch.Stop(m.Name)
	case "start":
		err = s.orch.Start(context.Background(), *m)
	case "restart":
		err = s.restartManaged(m.Name)
	default:
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"error": "action 只能是 start / stop / restart",
		})
		return
	}

	s.syncRegistry()
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{
			"error": err.Error(), "channels": s.channelStatuses(),
		})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "channels": s.channelStatuses()})
}

// handleChannelAdminKey 改托管网关的后台密码，并把同一个值同步到 Mergence 侧。
//
// 为什么必须由本进程代写，而不让前端直接打面板代理的 PUT /settings：
// 这个密码在两侧各存一份，而且两侧都真的在用——
//
//	网关侧  store 里的 admin_key：/admin/api/* 的鉴权凭据
//	本机侧  config.Claim.AdminKey：proxy.go 的 panelAuthKey 给内置面板注入
//	         Bearer，外加 claim 调度器定时领取时自报凭据
//
// 只改一边的后果是确定的故障（内置面板整块 401、定时领取静默失效），所以这里
// 把「改两处」做成一次调用，顺序固定为「先网关、后本机」：网关会校验新值
// （空值被它挡掉），它先失败就什么都不必动；本机落盘走的是与其他设置一致的
// 「Parse 校验 → 原子落盘 → 重载」管道，几乎不会失败，万一失败也会把
// 「网关已改、本机还是旧值」明写进错误里，让人知道该补哪一边，
// 而不是留一个静默的半成品。
//
// current_key 是给「两侧已经分叉」准备的：网关的 PUT /settings 要拿**旧密码**
// 鉴权，若本机存的那个值已经不对（面板正报 401），再用本机的值去改必然还是
// 401——这时让用户把网关真正在用的密码传进来即可复位。留空则用本机存的值。
func (s *Server) handleChannelAdminKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name       string `json:"name"`
		AdminKey   string `json:"admin_key"`
		CurrentKey string `json:"current_key"`
	}
	if err := readJSONBody(r, 8<<10, &in); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	newKey := strings.TrimSpace(in.AdminKey)
	if newKey == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "新的后台密码不能为空"})
		return
	}
	name := strings.TrimSpace(in.Name)
	ch, ok := s.reg.ByName(name)
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "渠道不存在：" + name})
		return
	}
	up := ch.Upstream()
	if up.Source != "managed" {
		writeJSONStatus(w, http.StatusNotImplemented, map[string]any{
			"error": "内嵌型渠道没有独立的网关设置", "code": "not_managed"})
		return
	}
	if !up.Enabled || !ch.Ready() {
		reason := ch.Status().ReadyReason
		if reason == "" {
			reason = "子进程未运行"
		}
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{
			"error": "网关未就绪，改不了它的密码（" + reason + "）", "code": "channel_not_ready"})
		return
	}

	root := strings.TrimRight(up.RootURL, "/")
	prefix := strings.TrimRight(panelAPIPrefixFor(kindOfUpstream(up), up.PanelAPIPrefix), "/")
	if root == "" || prefix == "" {
		writeJSONStatus(w, http.StatusBadGateway,
			map[string]any{"error": "不知道网关地址，改不了它的密码"})
		return
	}
	oldKey := strings.TrimSpace(in.CurrentKey)
	if oldKey == "" {
		oldKey = s.panelAuthKey(up) // 本机存的旧值
	}

	body, err := json.Marshal(map[string]string{"admin_key": newKey})
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), proxyTimeoutFor(http.MethodPut, "settings"))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, root+prefix+"/settings", bytes.NewReader(body))
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway,
			map[string]any{"error": "构造上游请求失败：" + err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if oldKey != "" {
		req.Header.Set("Authorization", "Bearer "+oldKey)
	}

	resp, err := upstreamClient.Do(req)
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{
			"error": "改网关密码失败（请求未送达）：" + err.Error()})
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			msg = "网关不认这个旧密码，请在「当前后台密码」里填网关真正在用的那个。原始返回：" + msg
		}
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{
			"error": "改网关密码失败：" + msg, "upstream_status": resp.StatusCode})
		return
	}

	// ── 原生型：route key 是后台密码的**真源**，所以还要把新值写进渠道配置 ──
	//
	// 内嵌形态下「两处」变成了「一处存储 + 一处覆盖」：密码存在网关库（上面
	// 刚 PUT 成功），而 native/zcode 每次装配都会用 route key 覆盖它
	// （WithForcedSetting）。若这里不同步 route key，下次重启就又把密码
	// 压回旧值 —— 那正是「只改一边」故障换了个方向。
	//
	// 老式子进程形态不需要这一步：它的密码本就存在网关侧，Mergence 只负责
	// 转发时注入；本机唯一的那份副本（Claim.AdminKey）在下面统一写。
	if up.Source == provider.SourceNative {
		if _, _, err := s.saveConfig(func(c *config.Config) {
			for i := range c.Managed {
				if c.Managed[i].Name == up.Name && c.Managed[i].Route != nil {
					c.Managed[i].Route.APIKey = newKey
				}
			}
		}); err != nil {
			writeJSONStatus(w, http.StatusBadGateway, map[string]any{
				"error": "网关密码已改成新值，但写入渠道配置失败：" + err.Error() +
					"；请到该渠道的「路由密钥」里再填一次同一个密码",
				"upstream_changed": true, "code": "partial"})
			return
		}
		s.lg.Info("原生网关后台密码已修改（route key 即唯一真源）", "channel", up.Name)
		writeJSON(w, map[string]any{"ok": true, "synced": []string{"gateway", "route_key"}})
		return
	}

	// ── 老式子进程：网关已改，接着把同一个值写进本机配置 ──
	if _, _, err := s.saveConfig(func(c *config.Config) { c.Claim.AdminKey = newKey }); err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{
			"error": "网关密码已改成新值，但写入本机配置失败：" + err.Error() +
				"；请到「设置 → 限时套餐自动领取」再填一次同一个密码",
			"upstream_changed": true, "code": "partial"})
		return
	}
	// 日志只说改了哪条渠道：密码属于凭据，不落日志。
	s.lg.Info("网关后台密码已修改，两处同步一致", "channel", up.Name)
	writeJSON(w, map[string]any{"ok": true, "synced": []string{"gateway", "mergence"}})
}

// channelStatuses 渠道状态，外加面板要用的补充字段。
//
// 补的是「每积分价值」：渠道级常量，用量视图的「实际花费估算」卡与
// 积分扣除历史的花费列都要它。跟着渠道列表返回，前端从已缓存的渠道
// 数据里取即可，省掉为常量单独发请求。
//
// 注意：托管渠道的「属于哪个平台」是**探测**出来的（老配置里没存 preset），
// 所以这里必须拿 Registry 里的 Upstream 去判，不能只看配置字段。
func (s *Server) channelStatuses() []provider.ChannelStatus {
	list := s.reg.Statuses()
	ups := make(map[string]provider.Upstream, len(list))
	for _, ch := range s.reg.Channels() {
		ups[ch.Name()] = ch.Upstream()
	}
	for i := range list {
		up, ok := ups[list[i].Name]
		// Hosted（托管型子进程 or 进程内原生）：两者都有管理面与积分口径，
		// 都要补「每积分价值」并按 kind 覆盖控制台形态。
		if !ok || !up.Source.Hosted() {
			continue
		}
		kind := kindOfUpstream(up)
		cv, isDefault := s.effectiveCreditValue(up.Name, kind)
		list[i].CreditValue, list[i].CreditValueDefault = cv, isDefault

		// zcode 系列覆盖成专属形态，前端据此给原生视图入口而不是外链。
		//
		// 判定走 kindOfUpstream（按名称/预设识别「这是哪个网关」），而**不是**
		// 控制台探测结果：探测回答的是另一个问题——「<PanelAPIPrefix>/models
		// 返回 200 吗」，而 zcode 根本没有这个接口，探出来永远是 web。它属于
		// 哪个网关与它的接口长什么样是两回事，前者靠识别、后者靠探测，在这里
		// 必须用前者。覆盖发生在探测结果之后：channelStatuses 本就是后加工入口。
		if kind == "zcode" {
			list[i].ConsoleKind = provider.ConsoleZcode
		}

		// 账号数：界面按「有没有账号」决定这个平台是否可见。
		list[i].AccountCount = s.fetchAccountCount(up, kind)
	}
	return list
}

// restartManaged 重启并等待就绪。
//
// 用 70s 超时：ReadyTimeout 默认 40s，加上停止回收的等待，给足余量。
// 首次启动一个 Node/Python 实现的上游可能要几十秒（装依赖、拉模型列表）。
func (s *Server) restartManaged(name string) error {
	cur := s.currentConfig()
	m := findManaged(cur, name)
	if m == nil {
		return fmt.Errorf("渠道不存在：%s", name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	return s.orch.Restart(ctx, *m)
}

// handleChannelRaw 返回渠道的可编辑完整配置（面板编辑用）。
//
// 列表接口返回的是「状态」，缺 command / args 这类只有编辑才需要的字段；
// 与其把它们塞进状态里增加噪音，不如单开一个按需调用的接口。
func (s *Server) handleChannelRaw(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "缺少 name 参数"})
		return
	}
	cur := s.currentConfig()

	if m := findManaged(cur, name); m != nil {
		out := channelInput{
			Kind: kindManaged, Name: m.Name, DisplayName: m.DisplayName,
			Enabled: m.Enabled, Preset: m.Preset,
			// Mode 必须回显：前端保存时原样带回来。不回显的话，任何一次编辑都会
			// 把 mode 清成空值 —— 而空值等于子进程，于是一个原生渠道会被
			// 「编辑一下」就变成去拉起一个不存在的可执行文件。
			Mode: m.Mode,
			// 回显已识别的网关种类：前端保存时会把它带回来。不回显的话，
			// 任何一次编辑都会把 kind 清空，唯一性校验随即失效。
			GatewayKind: gatewayKindOf(*m),
			Command:     m.Command, Args: m.Args, Dir: m.Dir, DataDir: m.DataDir,
			PortEnvVar: m.PortEnvVar, FixedPort: m.FixedPort,
			HealthPath: m.HealthPath, ReadyTimeout: m.ReadyTimeout,
			ShutdownGrace: m.ShutdownGrace, PanelPath: m.PanelPath, Env: m.Env,
			CreditValue: m.CreditValue,
			ModelSource: "auto", Protocol: "chat", Weight: 1,
		}
		// 老配置里没存 preset（该字段是后加的），直接回空会让前端下拉框停在
		// 第一个选项——于是 zcode 渠道被显示成 workbuddy。这里按当前的
		// command/args/health_path 推断一次兜底，并标记这是推断值而非记录值。
		if out.Preset == "" {
			// 原生型没有 command 可当锚点，InferManagedPreset 只会落到
			// 「自定义托管进程（空白）」上。它的 kind 就是内置实现名，也正是
			// 预设 id（workbuddy / zcode / trae），直接照着找，别去猜。
			if m.Native() {
				if p, ok := provider.ManagedPresetByKind(m.Kind); ok {
					out.Preset, out.PresetInferred = p.ID, true
				}
			} else if inferred := provider.InferManagedPreset(
				m.Command, m.Args, m.HealthPath, m.PortEnvVar); inferred != "" {
				out.Preset = inferred
				out.PresetInferred = true
			}
		}
		if m.Route != nil {
			out.Expose = true
			out.ModelPrefix = m.Route.ModelPrefix
			out.Models = m.Route.Models
			out.Protocol = m.Route.Protocol
			out.ModelsPath = m.Route.ModelsPath
			out.PathPrefix = m.Route.PathPrefix
			out.Headers = m.Route.Headers
			out.Weight = m.Route.Weight
			out.Priority = m.Route.Priority
			out.Timeout = m.Route.Timeout
			out.Retries = m.Route.Retries
			if m.Route.APIKey != "" {
				out.APIKeys = []string{m.Route.APIKey}
			}
		}
		writeJSON(w, out)
		return
	}

	if e := findEmbedded(cur, name); e != nil {
		writeJSON(w, channelInput{
			Kind: kindEmbedded, Name: e.Name, DisplayName: e.DisplayName,
			Enabled: e.Enabled, Preset: e.Preset, BaseURL: e.BaseURL,
			APIKeys: e.APIKeys, Protocol: e.Protocol, ModelPrefix: e.ModelPrefix,
			ModelSource: e.ModelSource, Models: e.Models, Headers: e.Headers,
			Weight: e.Weight, Priority: e.Priority, Timeout: e.Timeout,
			Retries: e.Retries, HealthPath: e.HealthPath, ModelsPath: e.ModelsPath,
			Proxy: e.Proxy, Price: e.Price,
		})
		return
	}
	writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "渠道不存在：" + name})
}

// handleChannelTest 保存前测试连接。
//
// 接受两种入参：
//   - 完整渠道对象（面板表单当前值，含未保存的改动）
//   - {"name":"x", ...}：测试已保存的渠道
func (s *Server) handleChannelTest(w http.ResponseWriter, r *http.Request) {
	var in channelInput
	if err := readJSONBody(r, 1<<20, &in); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.syncRegistry()

	up, err := s.resolveUpstream(&in)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	res := provider.TestChannel(ctx, *up, s.lg)
	s.lg.Info("渠道连接测试完成",
		"provider", up.Name, "source", up.Source, "ok", res.OK, "models", len(res.Models))
	writeJSON(w, res)
}

// handleChannelModels 拉取上游模型列表（不落盘，供面板多选）。
func (s *Server) handleChannelModels(w http.ResponseWriter, r *http.Request) {
	var in channelInput
	if err := readJSONBody(r, 1<<20, &in); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.syncRegistry()

	up, err := s.resolveUpstream(&in)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if up.BaseURL == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": "该渠道没有可用的上游地址"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	ch, err := provider.NewTempChannel(*up, s.lg)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ids, err := ch.FetchModels(ctx)
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	s.lg.Info("已拉取上游模型列表", "provider", up.Name, "count", len(ids))
	// 返回「真实 ID + 展示名」成对，而不是只回展示名：
	// 面板上要显示规范化后的名字，但用户勾选保存后配置里必须留真实 ID
	// （展示名写进 alias）。只回展示名会让保存的 ID 是规范化过的名字，
	// 上游收到它会直接 404。
	type model struct {
		ID   string `json:"id"`   // 上游真实 ID，转发时原样使用
		Name string `json:"name"` // 规范化后的展示名
		Free bool   `json:"free"` // 是否免费（不区分类型，便于旧客户端）
		// FreeKind 免费类型：nightly（夜间 23:00–次日 08:00）/ limited（限时）/ open。
		// 两者时段不同，混成一个 Free 标签会让人以为白天也免费。
		FreeKind string `json:"free_kind,omitempty"`
		// NightlyNow 当前是否处于夜间免费时段（仅 free_kind=nightly 时有意义）。
		NightlyNow bool `json:"nightly_now,omitempty"`
	}
	nightlyNow := provider.IsNightlyNow(time.Now())
	display := make([]model, 0, len(ids))
	for _, id := range ids {
		kind := provider.FreeKindOf(id)
		display = append(display, model{
			ID: id, Name: provider.NormalizeModelName(id),
			Free: kind != provider.FreeNone, FreeKind: string(kind),
			NightlyNow: kind == provider.FreeNightly && nightlyNow,
		})
	}
	writeJSON(w, map[string]any{"models": display, "count": len(display)})
}

// ───────────────────────────── 内部工具

// resolveUpstream 把面板入参解析成一个可用于测试 / 拉模型的上游。
//
// 托管型的地址不来自配置，而来自子进程当前监听的端口 —— 所以对已保存的托管型
// 渠道，这里要从编排器取实时地址；没跑起来就直接说明原因，而不是拿一个空地址
// 去发请求（那会给出一个看不懂的连接错误）。
func (s *Server) resolveUpstream(in *channelInput) (*provider.Upstream, error) {
	cur := s.currentConfig()

	// 编辑态下前端不提交 api_keys = 沿用已保存的 Key
	if in.APIKeys == nil && strings.TrimSpace(in.Name) != "" {
		if e := findEmbedded(cur, in.Name); e != nil {
			in.APIKeys = e.APIKeys
		} else if m := findManaged(cur, in.Name); m != nil && m.Route != nil && m.Route.APIKey != "" {
			in.APIKeys = []string{m.Route.APIKey}
		}
	}

	// 只给了 name（或只有前端必然带的空壳字段）：按已保存的渠道测
	if strings.TrimSpace(in.Name) != "" && strings.TrimSpace(in.BaseURL) == "" &&
		strings.TrimSpace(in.Command) == "" && !in.Expose {
		if m := findManaged(cur, in.Name); m != nil {
			if st, ok := s.liveManaged(m.Name); ok {
				up := provider.FromManaged(*m, st.BaseURL, true, "")
				return &up, nil
			}
			return nil, fmt.Errorf("渠道 %q 的子进程未在运行，请先启动它再测试", in.Name)
		}
		if e := findEmbedded(cur, in.Name); e != nil {
			up := provider.FromEmbedded(*e)
			return &up, nil
		}
		return nil, fmt.Errorf("渠道不存在：%s", in.Name)
	}

	if in.kind() == kindManaged {
		// 进程相关字段改了但还没保存 —— 此时「测试」测的是**旧进程**，
		// 结果会与实际保存后的行为不符。直接说清楚，别给一个看似通过的假信号。
		if m := findManaged(cur, in.Name); m != nil && processSpecChanged(*m, in) {
			return nil, fmt.Errorf(
				"进程相关配置已改动，需要先保存才能测试（当前能测到的是正在运行的旧进程）")
		}
		// 托管型未保存时地址只能从「已经在跑的实例」取。若名字对不上就说明原因，
		// 而不是编一个地址出来。
		if st, ok := s.liveManaged(in.Name); ok {
			up := provider.FromManaged(in.toManaged(), st.BaseURL, true, "")
			return &up, nil
		}
		return nil, fmt.Errorf(
			"托管型渠道的地址来自正在运行的子进程；请先保存并启动 %q 再测试", in.Name)
	}

	// 内嵌型：面板传来的可能是部分字段，走一次 Parse 补齐默认值
	tmp := config.Default()
	tmp.Embedded = []config.EmbeddedProvider{in.toEmbedded()}
	raw, err := json.Marshal(tmp)
	if err != nil {
		return nil, err
	}
	parsed, _, err := config.Parse(raw)
	if err != nil {
		return nil, err
	}
	up := provider.FromEmbedded(parsed.Embedded[0])
	return &up, nil
}

// processSpecChanged 判断「影响子进程本身」的字段是否被改动过。
func processSpecChanged(m config.ManagedProvider, in *channelInput) bool {
	return m.Command != strings.TrimSpace(in.Command) ||
		m.Dir != strings.TrimSpace(in.Dir) ||
		m.PortEnvVar != strings.TrimSpace(in.PortEnvVar) ||
		m.FixedPort != in.FixedPort ||
		!sameStrings(m.Args, in.Args) ||
		// 前缀改了等于换了个端点，测试时拿旧地址去试会给出误导性的「通过」
		!samePathPrefix(m.Route, in.PathPrefix)
}

func samePathPrefix(r *config.RouteSpec, want *string) bool {
	cur := ""
	if r != nil && r.PathPrefix != nil {
		cur = *r.PathPrefix
	} else {
		cur = "/v1" // 缺省值
	}
	if want == nil {
		return true // 前端没提交该字段，视为不改
	}
	v := strings.Trim(strings.TrimSpace(*want), "/")
	if v != "" {
		v = "/" + v
	}
	return cur == v
}

func sameStrings(a, b []string) bool {
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

// liveManaged 取某个托管实例的实时状态。
func (s *Server) liveManaged(name string) (orchestrator.Status, bool) {
	for _, st := range s.orch.List() {
		if st.Name == name && st.Port > 0 {
			return st, true
		}
	}
	return orchestrator.Status{}, false
}

// saveEmbedded / saveManaged 落盘并重载注册表。
func (s *Server) saveEmbedded(next []config.EmbeddedProvider) (*config.Config, []string, error) {
	return s.saveConfig(func(c *config.Config) { c.Embedded = next })
}

func (s *Server) saveManaged(next []config.ManagedProvider) (*config.Config, []string, error) {
	return s.saveConfig(func(c *config.Config) { c.Managed = next })
}

func (s *Server) saveConfig(mutate func(*config.Config)) (*config.Config, []string, error) {
	cur := s.currentConfig()
	clone := *cur
	mutate(&clone)

	raw, err := json.Marshal(&clone)
	if err != nil {
		return nil, nil, fmt.Errorf("序列化配置失败：%w", err)
	}
	parsed, warns, err := config.Parse(raw)
	if err != nil {
		return nil, nil, err
	}
	if err := config.Save(s.cfgPath, parsed); err != nil {
		return nil, nil, fmt.Errorf("写入配置失败：%w", err)
	}

	s.cfgMu.Lock()
	s.cfg = parsed
	s.cfgMu.Unlock()
	s.reg.Reload(s.buildUpstreams())
	return parsed, warns, nil
}

func (s *Server) currentConfig() *config.Config {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if s.cfg == nil {
		return config.Default()
	}
	return s.cfg
}

// uniqueChannelName 生成一个未被占用的渠道名。
//
// 没让用户起名是刻意的：名称是路由与日志的内部标识，让用户填只会出现
// 「名字重复」「名字里有空格」这类与目标无关的摩擦。用显示名派生即可。
func (s *Server) uniqueChannelName(cur *config.Config, display, preset string) string {
	base := strings.TrimSpace(display)
	if base == "" {
		if p, ok := provider.PresetByID(preset); ok {
			base = p.Label
		} else if p, ok := provider.ManagedPresetByID(preset); ok {
			base = p.Label
		}
	}
	if base == "" {
		base = "channel"
	}
	base = config.AutoPrefix(base)
	if base == "" {
		base = "channel"
	}
	if !s.nameTaken(cur, base) {
		return base
	}
	for i := 2; i < 1000; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if !s.nameTaken(cur, cand) {
			return cand
		}
	}
	return base
}

func (s *Server) nameTaken(c *config.Config, name string) bool {
	return findEmbedded(c, name) != nil || findManaged(c, name) != nil
}

func findEmbedded(c *config.Config, name string) *config.EmbeddedProvider {
	for i := range c.Embedded {
		if c.Embedded[i].Name == name {
			return &c.Embedded[i]
		}
	}
	return nil
}

func findManaged(c *config.Config, name string) *config.ManagedProvider {
	for i := range c.Managed {
		if c.Managed[i].Name == name {
			return &c.Managed[i]
		}
	}
	return nil
}

// fetchAccountCount 问上游「账号池里有几个账号」。
//
// 为什么直接问上游而不缓存进配置：账号是子进程持有的运行时数据
// （用户随时在网关那边增删），Mergence 侧存一份只会变成过期数据。
//
// 复用的是面板代理那一套目标地址与凭据（panelAPIPrefixFor / panelAuthKey），
// 不另造一条通道——两条通道迟早会漂移。
//
// 返回 -1 表示「问不到」（子进程没起、没配密码、上游报错）：前端据此把
// 平台相关内容留空，而不是当成 0 把平台藏掉。
func (s *Server) fetchAccountCount(up provider.Upstream, kind string) int {
	root := strings.TrimRight(up.RootURL, "/")
	prefix := strings.TrimRight(panelAPIPrefixFor(kind, up.PanelAPIPrefix), "/")
	if root == "" || prefix == "" {
		return -1
	}
	key := s.panelAuthKey(up)
	if kind == "zcode" && key == "" {
		return -1 // 没配后台密码，问了也只会得到 401
	}

	ctx, cancel := context.WithTimeout(context.Background(), accountCountTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, root+prefix+"/accounts", nil)
	if err != nil {
		return -1
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := upstreamClient.Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return -1
	}
	var body struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return -1
	}
	return len(body.Accounts)
}

// gatewayLabel 给用户看的中文名。
func gatewayLabel(kind string) string {
	switch kind {
	case "zcode":
		return "ZCode"
	case "workbuddy":
		return "WorkBuddy"
	case "trae":
		return "Trae"
	case gatewayKindNewAPI:
		return "new-api"
	default:
		return kind
	}
}

// gatewayKindTaken 判断「同一种网关」是否已存在。
//
// 为什么按网关种类而不是渠道名判重：用户可以随便起名（叫「我的网关」也行），
// 名字拦不住重复；而 zcode2api / wb2api 这类进程在同一台机器上跑两个实例没有意义
// ——它们各自持有账号池与配置目录，第二个要么抢端口、要么数据互相覆盖。
//
// newapi 豁免：new-api 本身是「多渠道聚合底座」，一个系统里跑多个实例是它的常规用法。
//
// skipName 用于「编辑自己」的场景：改自己的字段不算撞车。
func gatewayKindTaken(c *config.Config, kind, skipName string) bool {
	kind = strings.TrimSpace(kind)
	if kind == "" || kind == gatewayKindNewAPI {
		return false
	}
	for i := range c.Managed {
		if c.Managed[i].Name == skipName {
			continue
		}
		if gatewayKindOf(c.Managed[i]) == kind {
			return true
		}
	}
	return false
}

// gatewayKindOf 读配置里的 kind；为空时回退到特征识别（兼容老配置）。
//
// 识别信号只认**客观事实**——启动命令（command/args）与预设 id，**不认
// name/display_name**。理由：显示名是用户随手起的，把渠道叫「ZCode 网关」
// 不代表它跑的是 zcode；拿名字判重会把正常渠道误判成重复（e2e 领取测试
// 正是被这个坑到的：它的测试渠道显示名就叫「ZCode 网关」）。
//
// preset 仍然算事实：它由「创建时选了哪个模板」落盘，不是用户能随手改的
// 显示文字。老配置里可能为空，那时靠 command/args 兜住。
//
// 存了 kind 的老配置也不受影响——kind 优先，压根不看别的。
func gatewayKindOf(m config.ManagedProvider) string {
	if k := strings.TrimSpace(m.Kind); k != "" {
		return k
	}
	facts := strings.ToLower(m.Command + " " + strings.Join(m.Args, " ") + " " + m.Preset)
	switch {
	case strings.Contains(facts, "zcode"), strings.Contains(facts, "cli.py"):
		return "zcode"
	case strings.Contains(facts, "wb2api"), strings.Contains(facts, "wb2a"),
		strings.Contains(facts, "workbuddy"):
		return "workbuddy"
	case strings.Contains(facts, "trae"):
		// trae 只有 preset 这一个可靠信号（老配置的 command 是解释器名 `node`，
		// 不具区分度，不能作锚点）。所以只认 preset 里出现 trae 的情况。
		return "trae"
	case strings.Contains(facts, "new-api"), strings.Contains(facts, "newapi"):
		return gatewayKindNewAPI
	}
	return ""
}
