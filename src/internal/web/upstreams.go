// upstreams.go 把「配置」与「子进程实时状态」组装成可路由上游列表。
//
// 组装放在 web 层而不是 provider 包，是为了不让 provider 依赖 orchestrator：
// provider 只管「怎么把请求发出去」，它不需要知道上游是个内嵌端点还是一个子进程，
// 更不该知道子进程怎么拉起来。
package web

import (
	"modelmux/internal/orchestrator"
	"modelmux/internal/provider"
)

// buildUpstreams 组装当前全部可路由上游。
//
// 内嵌型直接用配置；托管型要叠加子进程的就绪状态 —— 子进程没起来时它的
// 端点是不存在的，路由必须跳过（否则请求会打到空气上，报出一个误导性的连接错误）。
func (s *Server) buildUpstreams() []provider.Upstream {
	cfg := s.currentConfig()

	out := make([]provider.Upstream, 0, len(cfg.Embedded)+len(cfg.Managed))
	for _, c := range cfg.Embedded {
		out = append(out, provider.FromEmbedded(c))
	}

	live := make(map[string]orchestrator.Status, 4)
	for _, st := range s.orch.List() {
		live[st.Name] = st
	}
	for _, m := range cfg.Managed {
		if m.Route == nil {
			continue // 只托管、不路由
		}
		st, ok := live[m.Name]
		ready := ok && st.State == orchestrator.StateRunning && st.Port > 0

		reason := ""
		switch {
		case !m.Enabled:
			reason = "已在配置中停用"
		case !ok:
			reason = "未启动（可能是配置里缺少 command）"
		case st.State == orchestrator.StateFailed:
			reason = firstNonEmptyStr(st.LastErr, "启动失败")
		case st.State == orchestrator.StateStarting:
			reason = "正在启动"
		default:
			reason = firstNonEmptyStr(st.LastErr, "子进程未运行")
		}
		out = append(out, provider.FromManaged(m, st.BaseURL, ready, reason))
	}
	return out
}

// syncRegistry 把上游列表同步到注册表；指纹未变时不重建。
func (s *Server) syncRegistry() {
	ups := s.buildUpstreams()
	if s.reg.SyncIfChanged(ups) {
		s.lg.Info("可路由上游已同步",
			"total", len(ups), "models", len(s.reg.Models()))
	}
	// 托管型子进程起来后，它的模型目录需要在后台补上（内部有时间窗，调用很便宜）。
	s.reg.PrefetchModels()
	// 顺带探一次「这个渠道的控制台能不能直接用」（同样带 TTL）。
	s.reg.ProbeConsoles()
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
