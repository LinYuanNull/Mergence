// access.go 对外 API Key 的管理与 /v1 出口的鉴权。
//
// 设计取向：
//   - Key 由系统生成，不由用户想——用户想出来的密钥几乎必然是弱口令
//   - 面板（本机 UI）不需要它：控制面靠「只监听 127.0.0.1」隔离，
//     数据面（/v1，会被填进各种第三方客户端）才需要 Bearer 保护
//   - 重新生成立即生效；已有客户端会开始收到 401，面板上会提示先更新客户端
package web

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"modelmux/internal/config"
)

// currentAccessKey 当前生效的对外 Key（cfgMu 下读，端口/再生成并发安全）。
func (s *Server) currentAccessKey() string {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if s.cfg == nil {
		return ""
	}
	return s.cfg.AccessKey
}

// withAccessKey 包住 /v1 handler 做 Bearer 校验。
//
// 常量时间比较（subtle）：Key 虽然是 128 位随机，逐字节比较也不会泄漏多少，
// 但这是零成本的正确习惯，没有理由不做。
func (s *Server) withAccessKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := s.currentAccessKey()
		if key == "" {
			// normalize 保证 Key 非空；走到这里说明配置被绕过校验改坏。
			// 拒绝服务比裸奔好。
			writeOpenAIError(w, http.StatusInternalServerError,
				"服务端未配置 access_key，拒绝服务", "server_error", "no_access_key")
			return
		}
		got := strings.TrimSpace(r.Header.Get("Authorization"))
		ok := len(got) > 7 && strings.EqualFold(got[:7], "Bearer ") &&
			subtle.ConstantTimeCompare([]byte(got[7:]), []byte(key)) == 1
		if !ok {
			// 也接受 X-API-Key：部分客户端不方便自定义 Authorization
			if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(r.Header.Get("X-API-Key"))), []byte(key)) != 1 {
				writeOpenAIError(w, http.StatusUnauthorized,
					"API Key 无效或缺失；请在 ModelMux 面板设置里复制正确的 Key",
					"invalid_request_error", "invalid_api_key")
				return
			}
		}
		next(w, r)
	}
}

// handleAccessKeyGet 面板读取当前 Key（本机控制面，无需鉴权）。
func (s *Server) handleAccessKeyGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"access_key": s.currentAccessKey()})
}

// handleAccessKeyRegen 重新生成 Key：立即生效，落盘持久化。
func (s *Server) handleAccessKeyRegen(w http.ResponseWriter, _ *http.Request) {
	k, err := config.GenerateAccessKey()
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError,
			map[string]any{"ok": false, "error": "生成失败：" + err.Error()})
		return
	}
	// 走统一的 saveConfig（Parse 校验 + 注册表同步），不绕过配置管道私写文件。
	_, _, err = s.saveConfig(func(c *config.Config) { c.AccessKey = k })
	if err != nil {
		writeJSONStatus(w, http.StatusInternalServerError,
			map[string]any{"ok": false, "error": "保存失败：" + err.Error()})
		return
	}
	s.lg.Info("对外 API Key 已重新生成（旧 Key 立即失效）")
	writeJSON(w, map[string]any{"ok": true, "access_key": k,
		"note": "旧 Key 已立即失效，请更新所有外部客户端"})
}
