// server.go Mergence 自带的 HTTP 服务。
//
// 两类接口，职责分明：
//
//	/v1/*         对外出口 —— OpenAI 兼容契约，供任意客户端（含只认 OpenAI 协议的工具）调用
//	/api/*        面板接口 —— 状态、日志、渠道增删改查；仅供本机面板使用
//
// 对外出口与面板接口分开的理由：面板接口会改动配置、能触发退出，属于「控制面」；
// 对外出口只转发请求，属于「数据面」。混在一起会让「给第三方客户端一个 Key」
// 这种需求无处下手——目前二者都只监听 127.0.0.1，靠回环地址本身隔离。
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"mergence/internal/assets"
	"mergence/internal/claim"
	"mergence/internal/config"
	"mergence/internal/logging"
	"mergence/internal/metrics"
	"mergence/internal/orchestrator"
	"mergence/internal/provider"
)

// 面板前端资源（index.html / *.js / app.css）以 internal/assets 包的内嵌副本兜底，
// 该包自己声明 go:embed —— embed 不允许 `..`，也不能跨包引用文件。
// 若 exe 同级有 web/ 目录则以磁盘上的为准（见 asset），对外的 URL 不变，
// 路由见 buildMux。
// Server 内置 HTTP 服务。
type Server struct {
	lg    *logging.Logger
	orch  *orchestrator.Orchestrator
	reg   *provider.Registry
	home  string
	start time.Time

	// webDir 磁盘上的外置前端目录（<root>/web）。为空表示没有外置前端，
	// 一律用内嵌副本 —— 见 asset。启动日志会记明实际用了哪一份。
	webDir string

	// srvMu 保护 baseURL / httpSrv / currentPort —— 端口热切换会替换它们，
	// 而托盘菜单、日志、状态接口随时可能在读。
	srvMu       sync.Mutex
	baseURL     string
	currentPort int
	httpSrv     *http.Server

	cfgMu   sync.Mutex
	cfg     *config.Config
	cfgPath string

	// store 调用计量（费用、调用次数、缓存命中率的本地数据源）。
	store *metrics.Store
	// pricer 计价器。价格表可能随版本更新，故与 store 分开。
	pricer *metrics.Pricer

	// claims 限时套餐的定时领取器（每天在窗口内代领一次）。
	claims *claim.Scheduler

	// stop 用于结束后台同步协程。
	stop chan struct{}

	// OnQuit 由 main 注入：面板/接口触发退出时调用（走托盘同一套有序退出流程）
	OnQuit func()
	// OnPortChange 由 main 注入：端口切换成功后通知壳（更新托盘提示、跳转页面）。
	// 参数是新 baseURL。可为 nil（headless 模式）。
	OnPortChange func(baseURL string)
	// OnCloseBehaviorChange 由 main 注入：面板改了「关窗行为」后立即生效。
	// 参数 = 是否最小化到托盘。可为 nil（headless 模式没有窗口）。
	OnCloseBehaviorChange func(minimizeToTray bool)
}

func New(lg *logging.Logger, orch *orchestrator.Orchestrator, reg *provider.Registry, home string) *Server {
	s := &Server{
		lg: lg, orch: orch, reg: reg, home: home,
		start: time.Now(), stop: make(chan struct{}),
		webDir: detectWebDir(home),
		store:  metrics.NewStore(config.DataPath(home, "usage")),
		// 计价失败只降级为「价格未知」，绝不影响转发，所以这里可以静默。
		pricer: metrics.NewPricer(),
	}
	// 领取器：设置每次判定时现读（支持面板热改开关），执行走 runClaim。
	s.claims = claim.New(s.claimSettings, s.runClaim,
		func(msg string, args ...any) { lg.Info(msg, args...) })
	s.store.SetWarnFunc(func(msg string, args ...any) { lg.Warn(msg, args...) })
	return s
}

// metrics 返回计量存储（不会为 nil：New 里已初始化）。
func (s *Server) metrics() *metrics.Store { return s.store }

// syncEvery 后台同步周期。
//
// 为什么要有定时同步：托管型渠道的可用性随子进程变化，而**外部客户端不会去调面板接口**。
// 只在面板轮询时同步的话，一个用 curl 调 /v1 的用户会长期拿到过期的路由表
// （子进程已就绪却仍被判定不可用）。2 秒的成本只是一次指纹比对。
const syncEvery = 2 * time.Second

func (s *Server) syncLoop() {
	t := time.NewTicker(syncEvery)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.syncRegistry()
		}
	}
}

// SetConfig 注入当前配置与配置文件路径（渠道变更会写回这里）。
func (s *Server) SetConfig(cfg *config.Config, path string) {
	s.cfgMu.Lock()
	s.cfg = cfg
	s.cfgPath = path
	s.cfgMu.Unlock()
}

// Start 启动内置 HTTP 服务，返回实际端口。
//
// 端口来源：配置里 panel_port > 0 用固定端口（冲突则报错回落动态并记警告），
// 否则 :0 由 OS 分配。运行中可通过 /api/settings/port 热切换。
func (s *Server) Start() (int, error) {
	// 前端资源的来源必须留痕：外置生效时改文件立刻可见，内嵌生效时改文件无效，
	// 而这两种情况在浏览器里完全看不出差别（同一个 URL、同一份内容）。
	if s.webDir != "" {
		s.lg.Info("前端资源：使用外置目录", "dir", s.webDir)
	} else {
		s.lg.Info("前端资源：使用内嵌副本（未发现 web/ 目录）")
	}

	s.cfgMu.Lock()
	want := s.cfg.Ports.PanelPort
	s.cfgMu.Unlock()

	if want > 0 {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(want)))
		if err == nil {
			_, _ = s.serveOn(ln)
			return portOf(ln), nil
		}
		// 用户显式配置的端口起不来必须大声说：静默换个端口的话，
		// 书签和外部客户端里写死的地址全会悄悄失效。
		s.lg.Warn("配置的固定端口不可用，回落动态分配",
			"port", want, "err", err.Error())
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("监听失败：%w", err)
	}
	port, _ := s.serveOn(ln)
	return port, nil
}

// portOf 从 listener 取端口（serveOn 返回值拆分后的便捷函数）。
func portOf(ln net.Listener) int {
	if addr, ok := ln.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

// serveOn 在给定 listener 上开始服务，返回**被替换下来的旧 server**（可为 nil）。
//
// listener 由调用方持有成功后才传进来（固定端口先试听成功再交棒），
// 避免「探测说空闲、真正监听时被抢」的窗口期。
//
// 返回旧 server 而不是让调用方事后从 s.httpSrv 取：这里替换完字段后，
// s.httpSrv 已经是新 server 了——事后取到的是自己，shutdown 它等于
// 把刚起的服务关掉。
func (s *Server) serveOn(ln net.Listener) (int, *http.Server) {
	port := ln.Addr().(*net.TCPAddr).Port
	base := fmt.Sprintf("http://127.0.0.1:%d", port)

	s.srvMu.Lock()
	old := s.httpSrv
	s.baseURL = base
	s.currentPort = port
	srv := &http.Server{
		Handler:           s.buildMux(),
		ReadHeaderTimeout: 10 * time.Second,
		// 不设 WriteTimeout：流式响应可能持续很久，写超时会把长回答截断。
		// 真正的保护在 provider 侧的传输层超时（连接 15s / 首字节 60s）。
	}
	s.httpSrv = srv
	s.srvMu.Unlock()

	// 先组装一次再开始监听：否则第一个 /v1 请求可能撞上空的注册表，
	// 而那个请求可能来自任意客户端（不是我们的面板），它没机会重试。
	s.syncRegistry()
	s.ensureSyncLoop()

	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.lg.Error("内置服务异常退出", "err", err.Error())
		}
	}()
	s.lg.Info("内置服务已监听",
		"port", port, "panel", base+"/", "api", base+"/v1")
	return port, old
}

// ensureSyncLoop 保证后台同步协程只跑一份（端口切换不重启它）。
var syncOnce sync.Once

func (s *Server) ensureSyncLoop() {
	syncOnce.Do(func() {
		go s.syncLoop()
		// 限时套餐的定时领取。Start 开头会立刻判一次：进程若恰好在
		// 领取窗口内启动，这一次就把当天的补上，不必等到下一个 tick。
		// 停止统一走 s.stop，与同步协程共用一个生命周期。
		go s.claims.Start(context.Background())
	})
}

// buildMux 组装全部路由。端口切换会重建 mux，但 handler 都闭包引用 s 自身，
// 状态（注册表/配置/日志）天然共享。
func (s *Server) buildMux() *http.ServeMux {
	mux := http.NewServeMux()

	// ── 面板静态资源与接口
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/theme.js", s.handleThemeJS)
	mux.HandleFunc("/app.js", s.handleJS)
	mux.HandleFunc("/upstream.js", s.handleUpstreamJS)
	mux.HandleFunc("/overview.js", s.handleOverviewJS)
	mux.HandleFunc("/zcode.js", s.handleZcodeJS)
	mux.HandleFunc("/app.css", s.handleCSS)
	mux.HandleFunc("/api/healthz", s.handleHealthz)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/metrics", s.handleMetrics)
	mux.HandleFunc("/api/claim", s.handleClaimStatus)
	mux.HandleFunc("/api/claim/config", s.handleClaimConfig)
	mux.HandleFunc("/api/claim/now", s.handleClaimNow)
	mux.HandleFunc("/api/logs", s.handleLogs)
	mux.HandleFunc("/api/quit", s.handleQuit)
	mux.HandleFunc("/api/presets", s.handlePresets)
	mux.HandleFunc("/api/channels", s.handleChannels)
	mux.HandleFunc("/api/channels/toggle", s.handleChannelToggle)
	mux.HandleFunc("/api/channels/action", s.handleChannelAction)
	mux.HandleFunc("/api/channels/raw", s.handleChannelRaw)
	mux.HandleFunc("/api/channels/test", s.handleChannelTest)
	mux.HandleFunc("/api/channels/models", s.handleChannelModels)
	// 网关后台密码改一次要落两处（网关侧 + 本机 Claim.AdminKey），
	// 所以单独一个原生接口，不让前端绕过面板代理自己拼。
	mux.HandleFunc("POST /api/channels/admin-key", s.handleChannelAdminKey)
	// 托管型上游的管理 API 代理：账号列表/启停/签到等内置进主窗口的关键。
	// {path...} 是 Go 1.22 ServeMux 的通配语法，把剩余路径原样交给代理；
	// 单独注册子树模式会与它 panic 冲突（两者匹配同一前缀）。
	mux.HandleFunc("/api/channels/{name}/upstream/{path...}", s.handleChannelUpstream)

	// ── 设置：对外 Key 与监听端口
	mux.HandleFunc("GET /api/access-key", s.handleAccessKeyGet)
	mux.HandleFunc("POST /api/access-key/regenerate", s.handleAccessKeyRegen)
	mux.HandleFunc("POST /api/settings/port", s.handleSettingsPort)
	mux.HandleFunc("POST /api/settings/close", s.handleSettingsClose)
	mux.HandleFunc("POST /api/settings/console", s.handleSettingsConsole)

	// ── 对外 OpenAI 兼容出口
	// 中间件做 Bearer 校验；handler 保持纯净（契约只有转发）。
	mux.HandleFunc("/v1/models", s.withAccessKey(s.handleV1Models))
	mux.HandleFunc("/v1/chat/completions", s.withAccessKey(s.handleV1Chat))

	return mux
}

// BaseURL 对外地址（端口热切换后返回新值）。
func (s *Server) BaseURL() string {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	return s.baseURL
}

// Port 当前监听端口。
func (s *Server) Port() int {
	s.srvMu.Lock()
	defer s.srvMu.Unlock()
	return s.currentPort
}

// detectWebDir 探测 exe 同级是否存在外置前端目录（<root>/web）。
//
// 为什么允许外置：面板是纯静态资源，改一行文案不该逼用户重新编译整个 exe。
// 但**内嵌副本必须留着** —— 单文件拷走仍要能跑，否则「绿色便携」就没了。
// 两者冲突时以磁盘为准（见 asset），启动日志会记明用的是哪一份。
func detectWebDir(root string) string {
	p := filepath.Join(root, "web")
	if fi, err := os.Stat(p); err == nil && fi.IsDir() {
		return p
	}
	return ""
}

// asset 取前端资源：优先磁盘上的 web/ 覆盖文件，缺失时回落到内嵌副本。
//
// 每次请求都读盘而不是启动时缓存：面板资源合计约 260KB，读一遍是微秒级；
// 而「改了立刻生效」在这个项目里更重要——一旦缓存，用户就会遇到
// 「明明改了却没生效」这类最难查的问题。
func (s *Server) asset(name string, embedded []byte) []byte {
	if s.webDir == "" {
		return embedded
	}
	b, err := os.ReadFile(filepath.Join(s.webDir, name))
	if err != nil {
		return embedded
	}
	return b
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:")
	_, _ = w.Write(s.asset("index.html", assets.IndexHTML))
}

// handleThemeJS 首帧主题脚本。
//
// 单独一个文件是为了绕开 CSP（script-src 'self' 会拦掉内联脚本），
// 同时必须由 index.html 在 <head> 里**同步**引用——它跑得越晚，
// 用户看到的主题闪烁越明显。
func (s *Server) handleThemeJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(s.asset("theme.js", assets.ThemeJS))
}

func (s *Server) handleJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(s.asset("app.js", assets.AppJS))
}

// handleUpstreamJS 上游控制台脚本（独立文件：账号池/任务/模型/用量/配置/日志
// 六类视图的逻辑放这里，app.js 只管框架与渠道管理）。
func (s *Server) handleUpstreamJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(s.asset("upstream.js", assets.UpstreamJS))
}

// handleZcodeJS zcode2api 系列网关的原生控制台视图。
//
// 单独一个文件而不是并进 upstream.js：那套视图按「管理 API 与集成面板兼容」
// 的渠道设计（wb2api 的 /panel/api 契约），zcode 的字段与路径都不同，
// 混在一起会让两套口径互相污染。
func (s *Server) handleZcodeJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(s.asset("zcode.js", assets.ZcodeJS))
}

// handleOverviewJS 概览指标脚本（独立文件：指标口径与状态分支较多，
// 混进 app.js 会让框架代码被细节淹没）。
func (s *Server) handleOverviewJS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write(s.asset("overview.js", assets.OverviewJS))
}

func (s *Server) handleCSS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = w.Write(s.asset("app.css", assets.AppCSS))
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "service": "mergence"})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	insts := s.orch.List()
	providers := make([]map[string]any, 0, len(insts))
	running := 0
	for _, st := range insts {
		if st.State == orchestrator.StateRunning {
			running++
		}
		providers = append(providers, map[string]any{
			"name": st.Name, "display_name": st.DisplayName, "state": st.State,
			"port": st.Port, "base_url": st.BaseURL,
			"last_err":   st.LastErr,
			"started_at": st.StartedAt,
		})
	}

	channels := s.channelStatuses()
	enabled, ready := 0, 0
	for _, c := range channels {
		if c.Enabled {
			enabled++
		}
		if c.Enabled && c.Ready {
			ready++
		}
	}
	models := len(s.reg.Models())
	// 关窗行为要回显到设置页的 radio 上，所以从**当前配置**取，
	// 而不是问窗口层：面板可能被浏览器以外的客户端打开，
	// 那时窗口层的值对不上界面显示的端口同理。
	minimizeToTray := s.currentConfig().Tray.MinimizeToTray
	// 同理由当前配置取：面板显示偏好也要能在刷新后回显到设置页。
	acctConsole := s.currentConfig().UI.AcctConsole

	writeJSON(w, map[string]any{
		"service":          "mergence",
		"uptime_sec":       int(time.Since(s.start).Seconds()),
		"home":             s.home,
		"base_url":         s.BaseURL(),
		"panel_port":       s.Port(),
		"minimize_to_tray": minimizeToTray,
		"acct_console":     acctConsole,
		"access_key":       s.currentAccessKey(),
		"running":          running,
		"total":            len(providers),
		"providers":        providers,
		"channels":         channels,
		"channels_total":   len(channels),
		"channels_enabled": enabled,
		"channels_ready":   ready,
		"models_total":     models,
		"api_ready":        ready > 0 && models > 0,
		"log_seq":          s.lg.Ring().LastSeq(),
	})
}

// handleLogs 增量拉取结构化日志。
//
// 参数：
//
//	since  只要 seq >= since 的条目（默认 0 = 全量）
//	limit  最多返回条数（默认 500，上限 5000）
//	level  最低级别过滤：debug|info|warn|error
//	provider  仅某 provider（含 subprocess 标签）
//	q      关键字（在 msg / raw / 字段值里做不区分大小写匹配）
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, _ := strconv.ParseUint(q.Get("since"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 500
	}
	if limit > 5000 {
		limit = 5000
	}
	minLevel := levelRank(strings.ToLower(q.Get("level")))
	providerName := strings.ToLower(q.Get("provider"))
	kw := strings.ToLower(strings.TrimSpace(q.Get("q")))

	entries := s.lg.Ring().Snapshot(since, 0)
	out := make([]logging.Entry, 0, len(entries))
	for _, e := range entries {
		if levelRank(strings.ToLower(e.Level)) < minLevel {
			continue
		}
		if providerName != "" && !strings.Contains(strings.ToLower(e.Provider), providerName) {
			continue
		}
		if kw != "" && !entryMatches(e, kw) {
			continue
		}
		out = append(out, e)
	}
	// 只保留最后 limit 条（前端要的是「最新的一屏」）
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	writeJSON(w, map[string]any{
		"entries": out,
		"seq":     s.lg.Ring().LastSeq(),
	})
}

func entryMatches(e logging.Entry, kw string) bool {
	if strings.Contains(strings.ToLower(e.Msg), kw) ||
		strings.Contains(strings.ToLower(e.Raw), kw) ||
		strings.Contains(strings.ToLower(e.Provider), kw) ||
		strings.Contains(strings.ToLower(e.ReqID), kw) {
		return true
	}
	for k, v := range e.Fields {
		if strings.Contains(strings.ToLower(k), kw) ||
			strings.Contains(strings.ToLower(fmt.Sprint(v)), kw) {
			return true
		}
	}
	return false
}

func levelRank(l string) int {
	switch l {
	case "debug":
		return 0
	case "warn", "warning":
		return 2
	case "error", "err", "fatal":
		return 3
	default:
		return 1
	}
}

func (s *Server) handleQuit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.OnQuit == nil {
		http.Error(w, "quit handler not wired", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "message": "正在退出"})
	go s.OnQuit()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

// writeJSONStatus 带状态码的 JSON 响应。
func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

// readJSONBody 读取并限制请求体大小。
func readJSONBody(r *http.Request, limit int64, dst any) error {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		return fmt.Errorf("仅支持 POST")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit))
	if err != nil {
		return fmt.Errorf("读取请求体失败：%w", err)
	}
	if len(body) == 0 {
		return fmt.Errorf("请求体为空")
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("请求体不是合法 JSON：%w", err)
	}
	return nil
}

// Shutdown 优雅关闭内置服务。
func (s *Server) Shutdown(ctx context.Context) {
	s.srvMu.Lock()
	srv := s.httpSrv
	s.srvMu.Unlock()
	if srv != nil {
		// 在途请求处理完再返回（连接不异常中断）；流式响应会阻塞到客户端断开，
		// 所以调用方要给 ctx 一个合理上限。
		_ = srv.Shutdown(ctx)
	}
	// 停掉定时领取：它可能正等在 30s 的 tick 上，不停就是 goroutine 泄漏。
	s.claims.Stop()
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	// 关掉计量文件：它按天追加，不 close 的话最后一次写入可能还在缓冲里。
	if s.store != nil {
		_ = s.store.Close()
	}
}
