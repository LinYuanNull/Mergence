// settings.go 运行中可改的设置：对外监听端口、关窗行为、面板显示偏好。
//
// 端口切换的顺序是正确性的关键：
//
//	探测并持有新 listener → 新 server 起来 → 响应本次请求 → 旧 server 优雅关闭
//
// 先起新的再关旧的，中间没有「服务不可用」的窗口；旧 server 的 Shutdown
// 会等在途请求处理完（连接不异常中断），但**必须放在 goroutine 里**——
// 当前请求本身就跑在旧 server 上，同步 Shutdown 会等自己返回，死锁。
//
// 两类设置的共同形状：**落盘 + 通知壳**。只落盘的话，用户要点一次窗口
// 才知道设置生效了；只通知壳的话，重启后又回到旧行为。
package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"mergence/internal/config"
)

// handleSettingsPort POST /api/settings/port {"port": 8642}（0 = 恢复动态分配）。
func (s *Server) handleSettingsPort(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Port int `json:"port"`
	}
	if err := readJSONBody(r, 4<<10, &in); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if in.Port < 0 || in.Port > 65535 {
		writeJSONStatus(w, http.StatusBadRequest,
			map[string]any{"error": fmt.Sprintf("端口 %d 越界（1–65535，0 表示恢复动态分配）", in.Port)})
		return
	}

	s.srvMu.Lock()
	cur := s.currentPort
	s.srvMu.Unlock()
	if in.Port == cur {
		// 没变化：只落盘（用户可能改了别的又改回来），不动监听器。
		if _, _, err := s.saveConfig(func(c *config.Config) { c.Ports.PanelPort = in.Port }); err != nil {
			writeJSONStatus(w, http.StatusInternalServerError,
				map[string]any{"error": "保存配置失败：" + err.Error()})
			return
		}
		writeJSON(w, map[string]any{"ok": true, "base_url": s.BaseURL(),
			"note": "端口未变化，仅保存设置"})
		return
	}

	// 1) 先把新 listener 真正握在手里——探测成功后再交棒，杜绝
	//    「探测时空闲、真正监听时被抢」的窗口。失败必须明确报错不切换。
	ln, err := listenPanel(in.Port)
	if err != nil {
		writeJSONStatus(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf("端口 %d 不可用：%v；未做任何变更", in.Port, err),
			"code":  "port_unavailable"})
		return
	}

	// 2) 新 server 立即开始服务；serveOn 返回被替换下来的旧 server
	//（不能事后从 s.httpSrv 取——那时它已经是新的了）。
	port, old := s.serveOn(ln)

	// 3) 落盘配置（重启后仍用这个端口）
	if _, _, err := s.saveConfig(func(c *config.Config) { c.Ports.PanelPort = in.Port }); err != nil {
		// 端口已切成功但没存下来：重启会丢。如实告知，用户可重试保存。
		s.lg.Error("端口已切换但配置保存失败，重启后将回落", "port", port, "err", err.Error())
	}

	// 4) 优雅关闭旧 server：在 goroutine 里做（见文件头注释——不能同步等自己）。
	//    小延迟确保本次响应先完整送出。
	if old != nil {
		go func() {
			time.Sleep(200 * time.Millisecond)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if err := old.Shutdown(ctx); err != nil {
				s.lg.Warn("旧监听器关闭超时（在途连接被强制断开）", "err", err.Error())
			} else {
				s.lg.Info("旧监听器已优雅关闭（在途请求全部完成）")
			}
		}()
	}

	s.lg.Info("监听端口已切换", "from", cur, "to", port)
	if s.OnPortChange != nil {
		go s.OnPortChange(s.BaseURL())
	}
	writeJSON(w, map[string]any{
		"ok": true, "base_url": s.BaseURL(), "port": port,
		"note": "已切换到新端口；原端口的服务正在完成剩余请求后关闭",
	})
}

// listenPanel 监听面板端口：>0 用指定端口（失败原样返回错误），0 交给 OS。
func listenPanel(port int) (net.Listener, error) {
	if port > 0 {
		return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// handleSettingsClose POST /api/settings/close {"minimize_to_tray": true|false}。
//
// true = 关窗隐藏到托盘、进程继续；false = 关窗直接退出应用。
//
// 落盘与生效是**两件事**，都要做：
//   - 落盘走 saveConfig，重启后仍是用户选的那一种；
//   - 生效走 OnCloseBehaviorChange，让本次运行立刻改变，否则用户要点一次
//     窗口才知道设置起作用了。
//
// 刻意**不碰托盘本身**：托盘要不要存在是启动期的决定（main 里只在
// minimize_to_tray 为真时创建），运行中起停它要重构 Tray 的线程生命周期
// （ready/done 是一次性 channel），收益远小于风险。现在的行为是：
// 运行中切到「直接退出」托盘图标仍在（仍可显示窗口/复制地址），
// 下次启动则不再有托盘——这个差异写在这里，别当成 bug。
func (s *Server) handleSettingsClose(w http.ResponseWriter, r *http.Request) {
	var in struct {
		MinimizeToTray *bool `json:"minimize_to_tray"`
	}
	if err := readJSONBody(r, 4<<10, &in); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// 指针是故意的：JSON 里缺这个字段时解码成 nil，而 bool 的零值 false
	// 是一个合法的取值。少了这个判断，「字段漏传」会被当成「用户选了退出」。
	if in.MinimizeToTray == nil {
		writeJSONStatus(w, http.StatusBadRequest,
			map[string]any{"error": "缺少 minimize_to_tray（true = 最小化到托盘，false = 直接退出）"})
		return
	}
	want := *in.MinimizeToTray

	cur := s.currentConfig().Tray.MinimizeToTray
	if _, _, err := s.saveConfig(func(c *config.Config) { c.Tray.MinimizeToTray = want }); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError,
			map[string]any{"error": "保存配置失败：" + err.Error()})
		return
	}
	s.lg.Info("关窗行为已更新", "minimize_to_tray", want, "changed", want != cur)

	if want != cur && s.OnCloseBehaviorChange != nil {
		s.OnCloseBehaviorChange(want)
	}
	note := "关窗行为未变化，仅保存设置"
	if want {
		note = "关窗将隐藏到托盘，进程继续运行"
	} else {
		note = "关窗将直接退出应用"
	}
	writeJSON(w, map[string]any{"ok": true, "minimize_to_tray": want, "note": note})
}

// handleSettingsConsole POST /api/settings/console {"acct_console": true|false}。
//
// true = 在「积分型平台」页顶部显示「控制台」入口区块（缺省）；false = 收起它。
//
// 与 handleSettingsClose 的区别：这里**没有运行期副作用**，所以只需要落盘。
// 那块界面的显隐完全由前端按 /api/status 里的 acct_console 决定，保存后前端
// 刷新一次状态即生效，服务端没有需要跟着切换的东西（关窗行为则不同，它是
// 进程级的，必须额外通知窗口层）。
func (s *Server) handleSettingsConsole(w http.ResponseWriter, r *http.Request) {
	var in struct {
		AcctConsole *bool `json:"acct_console"`
	}
	if err := readJSONBody(r, 4<<10, &in); err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// 指针同样是必须的：false 是合法取值，用零值判断「没传」会把
	// 「收起入口」误判成「字段漏传」。
	if in.AcctConsole == nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]any{
			"error": "缺少 acct_console（true = 在「积分型平台」页显示控制台入口，false = 收起）"})
		return
	}
	want := *in.AcctConsole
	cur := s.currentConfig().UI.AcctConsole
	if _, _, err := s.saveConfig(func(c *config.Config) { c.UI.AcctConsole = want }); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError,
			map[string]any{"error": "保存配置失败：" + err.Error()})
		return
	}
	s.lg.Info("控制台入口显示偏好已更新", "acct_console", want, "changed", want != cur)

	note := "控制台入口在「积分型平台」页顶部显示"
	if !want {
		note = "已从「积分型平台」页收起；侧栏「控制台」分组不受影响，可在设置页随时恢复"
	}
	writeJSON(w, map[string]any{"ok": true, "acct_console": want, "note": note})
}
