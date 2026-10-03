// proxy.go 把托管型渠道自带的管理 API 代理到本面板之下。
//
// 解决两个真实问题：
//
//  1. 「面板里再开面板」：以前管理 WorkBuddy 账号要跳转到 wb2api 自己的面板——
//     两套 UI、两个地址。现在管理请求统一走本代理，账号列表/启停/签到直接
//     呈现在 ModelMux 主窗口里。
//  2. 「打开账号池要输密钥」：渠道的 api_key 由 ModelMux 服务端持有，
//     代理转发时注入 Authorization。浏览器从头到尾不接触上游密钥。
//
// 边界：只代理「托管型且已就绪」的渠道；这是管理通道不是通用代理——
// 上游地址由服务端配置决定，客户端无法用它访问任意主机。
//
// 方法覆盖：GET/POST/PUT/DELETE 一律透传。写操作曾经被禁（zcode 只放 GET），
// 那是「账号面板留在网关自己的 UI 里」时代的口径；面板整块搬进 ModelMux 后
// 已撤掉，理由见 handleChannelUpstream 里的注释。
package web

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"modelmux/internal/provider"
)

// upstreamProxyTimeout 只读管理请求的上限。
//
// 账号列表、启停这类都是轻请求；个别操作（如余额刷新）上游可能要逐账号
// 回源查询，给 30s 但不无限等——面板 UI 还等着渲染。
const upstreamProxyTimeout = 30 * time.Second

// upstreamProxyWriteTimeout 一般写操作的上限。
//
// 增删改账号、导入导出、发起设备码登录都是「上游一次往返 + 落盘」，
// 比只读查询慢，但远不到分钟级。给 120s。
const upstreamProxyWriteTimeout = 120 * time.Second

// upstreamProxyClaimTimeout 领取套餐的上限，比一般写操作宽得多。
//
// 领取前上游要解一次人机验证（自带 Node 求解器要拉浏览器、加载页面），
// 实测可长达数十秒；再叠加激活上报与领取后的额度刷新。给 300s——
// 这是唯一一个「等一分钟是正常的」接口，用 120s 会把它误杀成超时。
const upstreamProxyClaimTimeout = 300 * time.Second

// proxyTimeoutFor 按方法与路径选择上游等待上限。
//
// 领取单独放宽的原因见 upstreamProxyClaimTimeout；其余 GET/HEAD 走只读上限，
// 非只读走写上限。
func proxyTimeoutFor(method, rest string) time.Duration {
	if strings.HasPrefix(strings.TrimLeft(rest, "/"), "claim") {
		return upstreamProxyClaimTimeout
	}
	if method == http.MethodGet || method == http.MethodHead {
		return upstreamProxyTimeout
	}
	return upstreamProxyWriteTimeout
}

// handleChannelUpstream 转发 /api/channels/{name}/upstream/{path...}
// 到 <子进程根地址><panel_api_prefix>/<path>。
func (s *Server) handleChannelUpstream(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rest := r.PathValue("path")
	if strings.TrimSpace(name) == "" {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "缺少渠道名"})
		return
	}

	ch, ok := s.reg.ByName(name)
	if !ok {
		writeJSONStatus(w, http.StatusNotFound, map[string]any{"error": "渠道不存在：" + name})
		return
	}
	up := ch.Upstream()
	// 判据是 Hosted（托管型子进程 or 进程内原生），不是「是不是子进程」：
	// 原生型跑的虽然在本进程内，但它同样自带 /panel/api/* 管理 API，
	// 而且是它**唯一**的管理入口 —— 挡掉它就等于原生渠道没有账号面板。
	if !up.Source.Hosted() {
		writeJSONStatus(w, http.StatusNotImplemented, map[string]any{
			"error": "内嵌型渠道只是通用 HTTP 转发，没有独立的管理 API",
			"code":  "not_managed",
		})
		return
	}
	if !up.Enabled {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error": "渠道已停用，请先在渠道列表里启用它", "code": "channel_disabled"})
		return
	}
	if !ch.Ready() {
		// ReadyReason 由 Status() 统一给出（区分「配置停用」与「子进程未就绪」）
		st := ch.Status()
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]any{
			"error": "子进程未就绪：" + st.ReadyReason, "code": "channel_not_ready"})
		return
	}

	kind := kindOfUpstream(up)

	// zcode 的管理代理 GET/POST/PUT/DELETE 全开放。
	//
	// 历史上这里只放 GET，理由是「写操作留在网关自己的面板里」。但账号面板
	// 整块搬进 ModelMux 之后，那个口径就成了半成品：面板上有按钮、点下去 405。
	// 所以按需求撤掉这道限制。
	//
	// 安全性没有靠「只读」来兜底，靠的是另外两条一直没变的前提：
	//   ① 上游地址取自服务端配置，客户端无法借这个代理访问任意主机；
	//   ② 后台密码由本进程注入（见 panelAuthKey），浏览器全程不接触明文。
	// 仍然保留的边界：内嵌型渠道不走这里、停用/未就绪渠道在更上面就被挡掉。
	// 注意：「限时套餐自动领取」仍走 claim_api.go 自己的客户端，不经过这里。

	// 管理通道的凭据来源与转发通道不同：zcode 用后台密码，其它网关用渠道
	// route 的 Key。来源错了会拿到 401（而不是「功能没实现」），容易误判成
	// 接口不存在，所以单独抽出来见 panelAuthKey。
	key := s.panelAuthKey(up)
	if kind == "zcode" && key == "" {
		// 没配密码就别发请求：发出去只会得到一个没头没尾的 401，
		// 不如直接说清楚该去设置页哪里填。
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{
			"error": "未配置后台密码：请在「设置 → 限时套餐自动领取」里填写该网关的后台密码"})
		return
	}

	root := strings.TrimRight(up.RootURL, "/")
	// 前缀走契约表，不读配置里的 panel_api_prefix：老配置里 zcode 渠道存的是
	// /panel/api（错的），而这是网关自己的固定契约。见 panelAPIPrefixFor。
	prefix := strings.TrimRight(panelAPIPrefixFor(kind, up.PanelAPIPrefix), "/")
	target := root + prefix + "/" + strings.TrimLeft(rest, "/")

	ctx := r.Context()
	limit := proxyTimeoutFor(r.Method, rest)
	if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > limit {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}

	// 写请求的 body 先整块读进内存再转发。
	//
	// 把 r.Body 直接交给 http.Client 时会以 chunked 发送（ContentLength 未知），
	// 而这里要 POST/PUT 的 body 都是小 JSON（导入的账号文件也在 KB 级），
	// 定长发送对上游更友好，也让 8MB 上限有个明确落点。只读请求没有 body。
	var payload io.Reader
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		buf, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			writeJSONStatus(w, http.StatusBadRequest,
				map[string]any{"error": "读取请求体失败：" + err.Error()})
			return
		}
		payload = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, r.Method, target, payload)
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway,
			map[string]any{"error": "构造上游请求失败：" + err.Error()})
		return
	}
	// 只透传无碍的头；Hop-by-hop 与鉴权类一律重写（密钥由本进程注入）。
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "cookie" || lk == "host" ||
			lk == "connection" || lk == "accept-encoding" {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := upstreamClient.Do(req)
	if err != nil {
		// 上游在转发途中失联（用户可能刚点了重启/停止）：把原因讲清楚，
		// 面板才能提示「子进程可能正在重启，稍后重试」而不是显示一堆堆栈。
		writeJSONStatus(w, http.StatusBadGateway,
			map[string]any{"error": "上游请求失败：" + err.Error(), "code": "upstream_failed"})
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// 401/403 不原样透传：前端只会看到一个没头没尾的 401，无从判断是密码错
	// 还是接口问题。翻译成一句能行动的话，并统一成 502（网关侧的失败）。
	if kind == "zcode" &&
		(resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
		writeJSONStatus(w, http.StatusBadGateway, map[string]any{
			"error": fmt.Sprintf(
				"后台密码被网关拒绝（HTTP %d）：请核对设置页填的后台密码", resp.StatusCode)})
		return
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		writeJSONStatus(w, http.StatusBadGateway,
			map[string]any{"error": "读取上游响应失败：" + err.Error()})
		return
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

// upstreamClient 管理代理专用的 HTTP 客户端。
//
// 与转发链路的客户端分开：管理请求的目标是本机子进程，不需要代理、
// 不需要传输层超时调优；超时在请求 ctx 上控制（30s）。
var upstreamClient = &http.Client{}

// firstKey 取渠道的第一个 Key（管理代理语义：单 Key 足够，不轮询）。
func firstKey(up provider.Upstream) string {
	if len(up.APIKeys) == 0 {
		return ""
	}
	return up.APIKeys[0]
}

// panelAPIPrefixFor 网页面板类网关的管理 API 前缀。
//
// 不读配置里的 panel_api_prefix：老配置里 zcode 渠道存的是 /panel/api（错的，
// 当年照抄 workbuddy 预设留下），而这是网关自己的固定契约——写在这里比让
// 每个用户手改配置可靠。
func panelAPIPrefixFor(kind, configured string) string {
	if kind == "zcode" {
		return "/admin/api"
	}
	return configured
}

// panelAuthKey 取管理通道的凭据。
//
// 管理通道的凭据来源与转发通道不同：zcode2api 的管理 API 认的是它自己的
// ZCODE_ADMIN_KEY（ModelMux 侧存在 config.ClaimConfig.AdminKey），而不是
// 渠道 route 的 APIKey —— 后者是转发通道的凭据。用错来源会得到 401，
// 而不是「功能没实现」，排查时容易误判成接口不存在。
func (s *Server) panelAuthKey(up provider.Upstream) string {
	if kindOfUpstream(up) == "zcode" {
		return s.claimSettings().AdminKey
	}
	return firstKey(up)
}
