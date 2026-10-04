// Package trae 把 trae2api-web 装配成一个 Mergence 进程内原生服务。
//
// ── 这个包为什么存在 ────────────────────────────────────────
//
// 上游（`connectedGraph/trae2api-web`，MIT）的装配逻辑写在它自己的
// `cmd/server/main.go` 里，而那是 `package main`——**不可 import**。所以要让
// 上游的 http.Handler 跑在 Mergence 进程内，必须在 Mergence 侧把同一张对象图
// 重新装配一遍：auth → pool → upstream.Client → scheduler → server.Handler。
//
// 本包就是那张对象图，且**刻意不复刻上游的 cmd/**：
//
//   - 不做 CLI flag / 配置文件自动生成 / 优雅停机信号（这些由编排器与 Mergence
//     自己的退出时序负责）；
//   - 配置不走环境变量（`TW2A_*` 是「独立部署」场景的约定；进程内实例的配置
//     由 Mergence 渠道条目给出，落盘在实例自己的数据目录里）。
//
// 落位的那份上游源码（`internal/provider/trae/`）保持逐字不动，
// 好处是能直接 `git merge` 上游的 bug 修复——这也是当初选「fork + 照搬」
// 而不是「重写」的全部理由。
//
// ── 与 workbuddy 装配层的两处不同 ───────────────────────────
//
//  1. **登录回调需要一个固定端口的第二监听**。上游 main 起了两个 http.Server：
//     主服务（配置里的 listen）与回调服务（`127.0.0.1:<callback_port>`，只处理
//     `/authorize`）。后者不能省——TRAE 的 OAuth redirect_uri 是**预注册**的，
//     端口必须固定，不能像主服务那样由内核分配。所以本包照上游做法再起一个
//     监听；端口被占用时**降级为手动粘贴回调链接**（上游同款行为），不阻断启动。
//
//  2. **签到没有 HTTP 入口，需要本包补一个**。workbuddy 的面板自带
//     `/panel/api/*` 全套（含领取）；trae 的 scheduler 只在进程内按整点跑
//     `RunCheckinNow()`，上游**没有**把它暴露成接口。Mergence 的「限时套餐自动领取」
//     （internal/claim）却是从**进程外**通过 HTTP 打渠道的，所以本包在 handler
//     外面包一层 ServeMux，加一个 `POST /admin/api/checkin` 触发它。
//     这一层是**纯增量**：上游 mux 一个字节没改，只是被挂在 `/` 下面。
//
// ── 数据落点 ──────────────────────────────────────────────
//
// 全部落在编排器给出的实例数据目录下，与其它实例、与 Mergence 自身数据互不干扰：
//
//	<data>/config.json   本实例的配置（键名与上游 config.json 逐字对齐）
//	<data>/state.json    账号池状态（积分、冷却、启停）
//	<data>/auths/        账号凭证（一账号一文件 trae-*.json）
package trae

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mergence/internal/config"
	"mergence/internal/logging"
	"mergence/internal/native"

	"mergence/internal/provider/trae/auth"
	"mergence/internal/provider/trae/pool"
	"mergence/internal/provider/trae/scheduler"
	"mergence/internal/provider/trae/server"
	"mergence/internal/provider/trae/upstream"
)

// upstreamVersion 落位那份上游源码的版本号（仅日志/展示用，不参与逻辑）。
const upstreamVersion = "trae2api-web (native)"

// PanelAPIPrefix 上游管理 API 的前缀。
//
// 这是接缝①②的目标前缀：Mergence 的代理把 `/api/channels/<name>/upstream/<rest>`
// 打到 `<root>/admin/api/<rest>`。集中导出是为了让它与渠道预设里的
// `PanelAPIPrefix` 有同一个可见出处，避免两边各写一份而漂移。
const PanelAPIPrefix = "/admin/api"

// HealthPath 上游的探活路径（编排器等就绪用）。
const HealthPath = "/healthz"

// PanelPath 上游自带面板的路径（供「打开上游面板」外链用）。
const PanelPath = "/admin"

// CheckinPath Mergence 专用的签到触发路径（见包注释第 2 点）。
//
// 它落在 `/admin/api` 前缀下，因此走与其它管理请求**同一条代理路径**与**同一份
// 凭据**（渠道的 route key），claim 侧不需要为 trae 单开一条鉴权通道。
const CheckinPath = "/admin/api/checkin"

// Kind 本原生实现在注册表里的键，也是配置里 `kind` 的取值。
const Kind = "trae"

func init() { native.Register(Kind, Boot) }

// Boot 装配并返回进程内服务。
func Boot(host config.ManagedProvider, dataDir string, lg *logging.Logger) (*native.Service, error) {
	home := dataDir
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, fmt.Errorf("创建原生实例数据目录失败：%w", err)
	}

	cfgPath := filepath.Join(home, "config.json")
	nc, err := loadFileConfig(cfgPath)
	if err != nil {
		return nil, err
	}
	// 路由 Key 由 Mergence 侧持有并在转发时注入。原生服务与它共用同一个值：
	// 上游的 withAuth（转发通道）与 withAdminAuth（管理通道）读的都是
	// cfg.APIKey，所以一个值同时管住两条通道，谁也别再抄一份密钥。
	// 值为空即完全不鉴权（只绑回环，可接受）。
	if host.Route != nil && strings.TrimSpace(host.Route.APIKey) != "" {
		nc.APIKey = strings.TrimSpace(host.Route.APIKey)
	}

	authsDir := filepath.Join(home, "auths")
	if err := os.MkdirAll(authsDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建账号目录失败：%w", err)
	}
	stateFile := filepath.Join(home, "state.json")

	auths, err := auth.LoadDir(authsDir)
	if err != nil {
		return nil, fmt.Errorf("读取账号目录失败：%w", err)
	}
	lg.Info("原生 trae：已加载账号", "count", len(auths), "auth_dir", authsDir)

	// ── 账号池
	p := pool.New(stateFile)
	p.SyncToDir(auths) // 对齐：剔除 state.json 中已删除 auth 文件的幽灵账号

	// ── 上游客户端
	up := upstream.New()
	up.HTTP.Timeout = nc.durSeconds(nc.Upstream.TimeoutSeconds, 120*time.Second)
	// 流式客户端无总超时，仅用首字节兜底（时长由 SSE 流本身决定）。
	if tr, ok := up.StreamHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HTTP.Timeout
	}

	// ── 排程（每日签到 + token 预刷新）
	sch := scheduler.New(scheduler.Config{
		Pool:         p,
		Upstream:     up,
		CheckinHour:  nc.Schedule.CheckinHour,
		RefreshHours: nc.Schedule.RefreshHours,
		RefreshSkew:  24 * time.Hour,
	})

	// ── 主 handler
	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       nc.APIKey,
		AuthDir:      authsDir,
		PlanCooldown: nc.dur(nc.Cooldown.PlanCredit, 12*time.Hour),
		SoftCooldown: nc.dur(nc.Cooldown.SoftRate, 60*time.Second),
		ErrThreshold: nc.Cooldown.ErrThresh,
		ErrCooldown:  nc.dur(nc.Cooldown.ErrCooldown, 10*time.Minute),
		RefreshSkew:  24 * time.Hour,
		DefaultModel: nc.DefaultModel,
	})

	// ── 外包一层：补上游没有的签到 HTTP 入口（见包注释第 2 点）。
	// 上游 mux 挂在 `/` 下，一个字节没改；只有 `POST /admin/api/checkin`
	// 被本层截住（上游没有这个路由，不存在覆盖）。
	outer := http.NewServeMux()
	outer.HandleFunc("POST "+CheckinPath, requireAPIKey(nc.APIKey, checkinHandler(p, sch)))
	outer.Handle("/", h)

	// 上游用标准 log 包输出运维日志（签到结果、刷新失败）。
	// 接到 Mergence 的结构化日志上，否则这些行会直接进 stderr 而面板看不到。
	restoreLog := mirrorLog(lg, host.Name)

	// 排程常驻运行，直到 Close。
	ctx, cancel := context.WithCancel(context.Background())
	go sch.Run(ctx)

	// ── 回调服务：第二监听，固定端口（见包注释第 1 点）。
	cb := startCallbackServer(nc.CallbackPort, outer, lg, host.Name)

	var once sync.Once
	closeAll := func() error {
		once.Do(func() {
			cancel()
			cb.close()
			restoreLog()
			// 账号池状态无需在此落盘：上游 pool 的每个写操作（SetCredits /
			// Cooldown / Disable / ReenableIfCredits…）末尾都会 saveLocked()，
			// 状态文件始终是最新的。这里若再调一次私有方法只会是多此一举。
		})
		return nil
	}
	lg.Info("原生 trae 已装配", "version", upstreamVersion,
		"callback_port", nc.CallbackPort, "api_key_set", nc.APIKey != "")
	return &native.Service{Handler: outer, Close: closeAll}, nil
}

// checkinHandler 触发一次全量签到，并回一份与 zcode 领取同形的回执。
//
// 为什么回执要对齐 zcode 的形状（`{outcomes:[{account_id,account_name,ok,message}],
// summary:{ok,fail}}`）：internal/claim 侧的翻译逻辑（claimResult.Translated）
// 是按这个形状写的，形状一致就能复用同一套面板渲染与日志格式，
// 不需要在 claim 侧为 trae 分叉出一条平行的翻译路径。
//
// 上游 RunCheckinNow 是**同步**的（遍历完所有账号才返回），且它不返回结果，
// 只往标准 log 写。所以这里「触发 → 读池状态 → 组回执」：
// ok 的判据是「该账号签到后不再处于冷却或硬禁用」——这正是签到要达成的效果
// （签到解开 1005 冷却）。这与上游「签到只是为了解冻」的设计意图一致。
func checkinHandler(p *pool.Pool, sch *scheduler.Scheduler) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		sch.RunCheckinNow()

		type outcome struct {
			AccountID   string `json:"account_id"`
			AccountName string `json:"account_name"`
			OK          bool   `json:"ok"`
			Message     string `json:"message,omitempty"`
		}
		type summary struct {
			OK   int `json:"ok"`
			Fail int `json:"fail"`
		}
		resp := struct {
			Outcomes []outcome `json:"outcomes"`
			Summary  summary   `json:"summary"`
		}{Outcomes: []outcome{}}

		for _, st := range p.List() {
			name := st.Nickname
			if name == "" {
				name = st.UID
			}
			o := outcome{
				AccountID:   st.UID,
				AccountName: name,
				OK:          !st.Disabled && !st.Cooling,
			}
			if o.OK {
				resp.Summary.OK++
			} else {
				resp.Summary.Fail++
				o.Message = st.Reason
				if o.Message == "" {
					if st.Disabled {
						o.Message = "账号已禁用（登录态失效）"
					} else {
						o.Message = "仍在冷却中"
					}
				}
			}
			resp.Outcomes = append(resp.Outcomes, o)
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// requireAPIKey 给本包补出来的写入口加一道与上游同口径的 Bearer 校验。
//
// 为什么需要它：上游的 withAdminAuth 是 `server.Handler` 的**私有方法**，本包
// 无法复用（也不该为了复用它去动落位的上游源码）。而签到入口是本包新加的，
// 若不自己补上这道校验，就会成为一个「配了 Key 却不校验」的写入口 ——
// 密钥被静默忽略，用户拿错 Key 也照样成功，与上游其它写接口的语义不一致。
//
// 口径与上游逐条对齐：APIKey 为空即完全不鉴权（只绑回环，可接受）、
// Bearer 前缀大小写不敏感、密钥常量时间比较、错误体与上游同形
// （`{"error":{"message":...,"type":"api_error","code":"invalid_api_key"}}`），
// 这样调用方（面板代理 / internal/claim）不必分辨 401 来自哪一层。
func requireAPIKey(apiKey string, next http.HandlerFunc) http.HandlerFunc {
	if apiKey == "" {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		authz := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if len(authz) < len(prefix) || !strings.EqualFold(authz[:len(prefix)], prefix) ||
			subtle.ConstantTimeCompare([]byte(authz[len(prefix):]), []byte(apiKey)) != 1 {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"missing or invalid API key",` +
				`"type":"api_error","code":"invalid_api_key"}}`))
			return
		}
		next(w, r)
	}
}

// callbackServer 登录回调的第二监听。
type callbackServer struct {
	srv *http.Server
}

// close 优雅停机；未启动时是空操作。
func (c *callbackServer) close() {
	if c == nil || c.srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = c.srv.Shutdown(ctx)
}

// startCallbackServer 起登录回调监听；端口为空或 "0" 时不起（纯手动粘贴模式）。
//
// 端口被占用不致命：降级为手动粘贴回调链接（上游 main 同款处理）。
// 所以这里只记一条 WARN，不把错误往上传——一个占不到端口的登录便利功能
// 不该让整个渠道起不来。
func startCallbackServer(port string, h http.Handler, lg *logging.Logger, name string) *callbackServer {
	port = strings.TrimSpace(port)
	if port == "" || port == "0" {
		lg.Info("原生 trae：未启用登录回调监听（手动粘贴回调链接模式）", "provider", name)
		return nil
	}
	srv := &http.Server{
		Addr:              "127.0.0.1:" + port,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	cs := &callbackServer{srv: srv}
	go func() {
		lg.Info("原生 trae：登录回调监听已就绪", "provider", name, "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			lg.Warn("原生 trae：登录回调监听启动失败，降级为手动粘贴回调链接",
				"provider", name, "addr", srv.Addr, "err", err.Error())
		}
	}()
	return cs
}

// mirrorLog 把标准 log 的输出镜像到 Mergence 的结构化日志，返回恢复函数。
//
// 局限（与 workbuddy 装配层同款，已知且可接受）：标准 log 是进程级全局状态，
// 同一进程内跑两个原生实例时，两条日志会同时进两边。原生实例在 Mergence 里
// 是单例（一个渠道），且日志本身按 provider 名打了标签，所以只是观感问题。
func mirrorLog(lg *logging.Logger, name string) func() {
	prev := log.Writer()
	log.SetOutput(io.MultiWriter(prev, lg.ProviderWriter(name)))
	return func() { log.SetOutput(prev) }
}

// ── 配置 ────────────────────────────────────────────────────

// fileConfig 本实例的配置，JSON 键与上游 config.json 一一对应。
//
// 为什么逐字对齐上游的键名：这份文件用户可以直接手改，也可以对着上游 README
// 找字段；键名一旦偏离，用户按上游文档改就会静默失效。
type fileConfig struct {
	Listen       string `json:"listen"`
	CallbackPort string `json:"callback_port"`
	APIKey       string `json:"api_key"`
	AuthDir      string `json:"auth_dir"`
	StateFile    string `json:"state_file"`
	DefaultModel string `json:"default_model"`

	Cooldown struct {
		PlanCredit  string `json:"plan_credit"`
		SoftRate    string `json:"soft_rate"`
		ErrThresh   int    `json:"err_threshold"`
		ErrCooldown string `json:"err_cooldown"`
	} `json:"cooldown"`

	Schedule struct {
		CheckinHour  int   `json:"checkin_hour"`
		RefreshHours []int `json:"refresh_hours"`
	} `json:"schedule"`

	Upstream struct {
		TimeoutSeconds int `json:"timeout_seconds"`
	} `json:"upstream"`
}

// defaultFileConfig 与上游 Default() 逐项对齐。
func defaultFileConfig() *fileConfig {
	c := &fileConfig{
		Listen:       "127.0.0.1:0", // 进程内实例由内核分配端口，此值仅作说明
		CallbackPort: "18080",
		AuthDir:      "./auths",
		StateFile:    "./state.json",
		DefaultModel: "glm-5.2",
	}
	c.Cooldown.PlanCredit = "12h"
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.ErrThresh = 3
	c.Cooldown.ErrCooldown = "10m"
	c.Schedule.CheckinHour = 9
	c.Schedule.RefreshHours = []int{3}
	c.Upstream.TimeoutSeconds = 120
	return c
}

// loadFileConfig 读配置；文件不存在时落一份默认配置再返回。
//
// 「先取默认再 JSON 覆盖」与上游 Load 同一手法：缺键保留默认，只有显式值才覆盖。
// 这样用户手改配置删掉一行，不会静默翻转整块行为。
func loadFileConfig(path string) (*fileConfig, error) {
	c := defaultFileConfig()
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if uerr := json.Unmarshal(raw, c); uerr != nil {
			return nil, fmt.Errorf("解析原生配置 %s 失败：%w", path, uerr)
		}
	case errors.Is(err, os.ErrNotExist):
		if werr := writeFileConfig(path, c); werr != nil {
			return nil, werr
		}
	default:
		return nil, fmt.Errorf("读取原生配置 %s 失败：%w", path, err)
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

func writeFileConfig(path string, c *fileConfig) error {
	buf, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		return fmt.Errorf("写入原生配置 %s 失败：%w", path, err)
	}
	return nil
}

// normalize 校验并归一（与上游 normalize 同口径）。
func (c *fileConfig) normalize() error {
	if c.Cooldown.ErrThresh <= 0 {
		c.Cooldown.ErrThresh = 3
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.DefaultModel == "" {
		c.DefaultModel = "glm-5.2"
	}
	if c.Listen == "" {
		c.Listen = "127.0.0.1:0"
	}
	if c.CallbackPort == "" {
		c.CallbackPort = "18080"
	}
	if c.Schedule.CheckinHour < 0 || c.Schedule.CheckinHour > 23 {
		return fmt.Errorf("schedule.checkin_hour: %d 不是合法的小时（0-23）", c.Schedule.CheckinHour)
	}
	for _, h := range c.Schedule.RefreshHours {
		if h < 0 || h > 23 {
			return fmt.Errorf("schedule.refresh_hours: %d 不是合法的小时（0-23）", h)
		}
	}
	for _, d := range []struct{ name, val string }{
		{"cooldown.plan_credit", c.Cooldown.PlanCredit},
		{"cooldown.soft_rate", c.Cooldown.SoftRate},
		{"cooldown.err_cooldown", c.Cooldown.ErrCooldown},
	} {
		if strings.TrimSpace(d.val) == "" {
			continue
		}
		if _, err := time.ParseDuration(d.val); err != nil {
			return fmt.Errorf("%s: %q 不是合法的时长（如 60s / 12h / 10m）", d.name, d.val)
		}
	}
	return nil
}

// dur 解析时长串，空或非法回落 def。
func (c *fileConfig) dur(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(s)); err == nil && d > 0 {
		return d
	}
	return def
}

// durSeconds 秒数 → 时长，<=0 回落 def。
func (c *fileConfig) durSeconds(n int, def time.Duration) time.Duration {
	if n > 0 {
		return time.Duration(n) * time.Second
	}
	return def
}
