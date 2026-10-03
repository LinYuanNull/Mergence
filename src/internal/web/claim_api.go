// claim_api.go 限时套餐领取：执行器 + 面板接口。
//
// 分工：
//
//	internal/claim  只管「什么时候领」（窗口、去重、开关），不碰 HTTP；
//	本文件          只管「怎么领」（找渠道、发请求、翻译回执），不碰时间逻辑。
//
// 这么切是因为两边各自的失败模式完全不同：时间逻辑错会重复领取，
// HTTP 逻辑错会领错渠道或拿不到回执。
package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"modelmux/internal/claim"
	"modelmux/internal/config"
	"modelmux/internal/provider"
)

// claimTimeout 单次领取的 HTTP 超时。
//
// 领取要走上游写流量（还可能顺带解一次验证码），比普通查询慢；
// 但也不能无限等——卡住会占住当天的去重名额。
const claimTimeout = 90 * time.Second

// 复用 proxy.go 里的 upstreamClient（管理代理专用）。
// 超时放在请求 ctx 上控制（claimTimeout），不设客户端级超时——
// 那个客户端还服务别的管理请求，不能被领取的 90s 污染。

// claimAdminPath zcode2api 的领取接口。
//
// 它不在 /panel/ 前缀下 —— 那是 wb2api 的约定。这里用配置里的
// PanelAPIPrefix 之外单独一份，因为两个网关的路由结构不同。
// 抽成常量并写清出处，避免下次有人「顺手改成 /panel/」而静默 404。
const claimAdminPath = "/admin/api/claim"

// runClaim 执行一次领取：定位渠道 → 发请求 → 翻译回执。
func (s *Server) runClaim(ctx context.Context, cfg config.ClaimConfig, trigger string) claim.Result {
	res := claim.Result{At: time.Now(), Trigger: trigger}

	ch, err := s.pickClaimChannel(cfg)
	if err != nil {
		res.Skipped = err.Error()
		return res
	}
	st := ch.Status()
	res.Channel = st.DisplayName
	if res.Channel == "" {
		res.Channel = st.Name
	}
	if !st.Ready {
		res.Skipped = "渠道「" + st.Name + "」未就绪：" + st.ReadyReason
		return res
	}
	up := ch.Upstream()
	if cfg.AdminKey == "" {
		res.Skipped = "未填写该网关的后台密码（设置页的「后台密码」）"
		return res
	}

	body, err := s.postClaim(ctx, up.RootURL, cfg.AdminKey)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.OK, res.Fail, res.Outcomes = body.Translated()
	return res
}

// pickClaimChannel 定位要领取的渠道。
//
// 优先用配置里指定的；没指定就挑「就绪的、预设像 zcode 的托管渠道」。
// 挑不出来时明确报「没有可用渠道」，不猜——猜错会把领取打到别的网关上。
func (s *Server) pickClaimChannel(cfg config.ClaimConfig) (*provider.Channel, error) {
	chans := s.reg.Channels()
	if cfg.Channel != "" {
		for _, c := range chans {
			if c.Name() == cfg.Channel {
				return c, nil
			}
		}
		return nil, errNoSuchChannel(cfg.Channel)
	}
	var cands []*provider.Channel
	for _, c := range chans {
		if !c.Source().Hosted() || !c.Ready() {
			continue
		}
		if kindOfUpstream(c.Upstream()) == "zcode" {
			return c, nil
		}
		cands = append(cands, c)
	}
	if len(cands) == 1 {
		return cands[0], nil
	}
	if len(cands) > 1 {
		// 多个候选时不能替用户选：领错渠道的后果是白跑一趟 + 消耗别人的额度
		names := make([]string, 0, len(cands))
		for _, c := range cands {
			names = append(names, c.Name())
		}
		sort.Strings(names)
		return nil, errAmbiguousChannel(names)
	}
	return nil, errNoManagedChannel
}

// postClaim 调用网关的领取接口。
//
// 契约（dengyie/zcode2api，AGPL-3.0，此处仅作进程级 HTTP 调用）：
//
//	POST /admin/api/claim
//	Authorization: Bearer <ZCODE_ADMIN_KEY>
//	→ {"outcomes":[{"account_id","account_name","ok","plan_name?","message?","next_at?"}],
//	   "summary":{"ok":N,"fail":M}}
//
// 1005「名额用完」时上游会带 next_at（名额恢复时间），原样带回面板——
// 用户据此知道该几点再试，而不是反复空转领取。
func (s *Server) postClaim(ctx context.Context, root, adminKey string) (claimResult, error) {
	var out claimResult
	root = strings.TrimRight(root, "/")
	if root == "" {
		return out, errNoRootURL
	}
	rctx, cancel := context.WithTimeout(ctx, claimTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(rctx, http.MethodPost,
		root+claimAdminPath, bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+adminKey)

	resp, err := upstreamClient.Do(req)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return out, err
	}
	if resp.StatusCode == http.StatusUnauthorized ||
		resp.StatusCode == http.StatusForbidden {
		// 后台密码错是最常见的原因，单独说清楚，别让用户去猜
		return out, errAdminKeyRejected(resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, errClaimStatus(resp.StatusCode, raw)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, errBadClaimBody(string(raw))
	}
	return out, nil
}

// claimResult 网关领取接口的回执。
type claimResult struct {
	Outcomes []struct {
		AccountID   string `json:"account_id"`
		AccountName string `json:"account_name"`
		OK          bool   `json:"ok"`
		PlanName    string `json:"plan_name"`
		Message     string `json:"message"`
		Code        any    `json:"code"`
		NextAt      string `json:"next_at"`
	} `json:"outcomes"`
	Summary struct {
		OK   int `json:"ok"`
		Fail int `json:"fail"`
	} `json:"summary"`
}

// Translated 把回执转成面板/日志用的形状（截断明细，避免刷屏）。
func (c claimResult) Translated() (ok, fail int, out []claim.Outcome) {
	out = make([]claim.Outcome, 0, len(c.Outcomes))
	for _, o := range c.Outcomes {
		name := o.AccountName
		if name == "" {
			name = o.AccountID
		}
		out = append(out, claim.Outcome{
			Account: name, OK: o.OK, Plan: o.PlanName,
			Message: o.Message, NextAt: o.NextAt,
		})
	}
	// 明细最多回 20 条：账号池可能有几十个，全列出来面板没法看
	if len(out) > 20 {
		out = out[:20]
	}
	return c.Summary.OK, c.Summary.Fail, out
}

// ── 面板接口 ───────────────────────────────────────────

// handleClaimStatus 查询领取状态（面板首屏用）。
func (s *Server) handleClaimStatus(w http.ResponseWriter, _ *http.Request) {
	cfg := s.claimSettings()
	writeJSON(w, s.claims.Status(cfg))
}

// handleClaimConfig 保存领取设置（开关 / 时刻 / 渠道 / 后台密码）。
func (s *Server) handleClaimConfig(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled  *bool   `json:"enabled"`
		At       *string `json:"at"`
		Window   *int    `json:"window"`
		Channel  *string `json:"channel"`
		AdminKey *string `json:"admin_key"`
	}
	if err := readJSONBody(r, 8<<10, &in); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// 走与其他设置一致的管道：config.Parse 校验 → 原子落盘 → 重载注册表。
	// 直接改内存会跳过校验，写坏了要等到下次启动才暴露。
	_, _, err := s.saveConfig(func(c *config.Config) {
		if in.Enabled != nil {
			c.Claim.Enabled = *in.Enabled
		}
		if in.At != nil {
			c.Claim.At = strings.TrimSpace(*in.At)
		}
		if in.Window != nil {
			c.Claim.Window = *in.Window
		}
		if in.Channel != nil {
			c.Claim.Channel = strings.TrimSpace(*in.Channel)
		}
		if in.AdminKey != nil {
			c.Claim.AdminKey = strings.TrimSpace(*in.AdminKey)
		}
	})
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	s.lg.Info("限时套餐领取设置已更新",
		"enabled", s.currentConfig().Claim.Enabled, "at", s.currentConfig().Claim.At)
	writeJSON(w, s.claims.Status(s.claimSettings()))
}

// handleClaimNow 立即领取一次（手动按钮）。
//
// 同步执行：领取通常几秒内返回，用户点完就该看到结果。
// 超时被上面 claimTimeout 兜住，不会无限挂着。
func (s *Server) handleClaimNow(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), claimTimeout+5*time.Second)
	defer cancel()
	res := s.claims.Manual(ctx)
	writeJSON(w, res)
}

// claimSettings 取当前领取设置（副本，避免调用方拿到指针改配置）。
func (s *Server) claimSettings() config.ClaimConfig {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if s.cfg == nil {
		return config.Default().Claim
	}
	return s.cfg.Claim
}

// kindOfUpstream 判定托管渠道的网关类型。
//
// 复用 metrics_api 里的 kindOf（它认 workbuddy），这里补上 zcode——
// 领取接口的路径按网关而异，认错类型就会把请求打到不存在的路径上。
func kindOfUpstream(up provider.Upstream) string {
	// 优先用配置里落盘的识别结果：预设已取消，用户还可能把渠道改名成
	// 「我的网关」这类不含平台字样的名字，届时名称匹配必然落空。
	if k := strings.TrimSpace(up.Kind); k != "" {
		return k
	}
	if k := kindOf(up); k != "" {
		return k
	}
	hay := strings.ToLower(up.Name + " " + up.DisplayName + " " + up.Preset)
	switch {
	case strings.Contains(hay, "zcode"), strings.Contains(hay, "glm coding"):
		return "zcode"
	case strings.Contains(hay, "wb2api"), strings.Contains(hay, "workbuddy"):
		return "workbuddy"
	case strings.Contains(hay, "new-api"), strings.Contains(hay, "newapi"):
		// new-api 是多渠道聚合底座，一个系统里可以跑多个实例，
		// 平台唯一性校验据此豁免（见 api_channels.go 的 kindTaken）。
		return "newapi"
	default:
		return ""
	}
}

// 领取失败的原因。刻意写成人能直接判断的句子：领取是后台任务，
// 用户看到的是日志里的一行字，得能据此知道下一步做什么。
var (
	errNoRootURL    = errors.New("渠道缺少子进程地址（未就绪？）")
	errBadClaimBody = func(body string) error {
		s := strings.TrimSpace(body)
		if len(s) > 120 {
			s = s[:120] + "…"
		}
		return fmt.Errorf("网关返回的不是可识别的领取回执：%s", s)
	}
	errAdminKeyRejected = func(code int) error {
		return fmt.Errorf("后台密码被拒绝（HTTP %d）：请核对设置页填的是该网关的 ZCODE_ADMIN_KEY", code)
	}
	errClaimStatus = func(code int, body []byte) error {
		s := strings.TrimSpace(string(body))
		if len(s) > 120 {
			s = s[:120] + "…"
		}
		if s == "" {
			return fmt.Errorf("网关返回 HTTP %d", code)
		}
		return fmt.Errorf("网关返回 HTTP %d：%s", code, s)
	}
)

func errNoSuchChannel(name string) error {
	return fmt.Errorf("指定的渠道 %q 不存在", name)
}

func errAmbiguousChannel(names []string) error {
	return fmt.Errorf("有多个托管渠道（%s），请在设置里指定用哪个", strings.Join(names, "、"))
}

var errNoManagedChannel = errors.New("没有就绪的托管渠道，无法领取")
