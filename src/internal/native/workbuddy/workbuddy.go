// Package workbuddy 把 workbuddy2api 装配成一个 ModelMux 进程内原生服务。
//
// ── 这个包为什么存在 ────────────────────────────────────────
//
// 上游（`linguo2625469/workbuddy2api-panel`，MIT）的装配逻辑写在它自己的
// `cmd/server/main.go` 里，而那是 `package main`——**不可 import**。所以要让
// 上游的 http.Handler 跑在 ModelMux 进程内，必须在 ModelMux 侧把同一张对象图
// 重新装配一遍：pool → upstream.Client → scheduler → usage / reqlog → panel →
// server.Handler。
//
// 本包就是那张对象图，且**刻意不复刻上游的 cmd/**：
//
//   - 不做 CLI flag / 配置文件自动生成 / 优雅停机信号（这些由编排器与 ModelMux
//     自己的退出时序负责）；
//   - 配置不走环境变量（`WB2A_*` 是「独立部署」场景的约定；进程内实例的配置
//     由 ModelMux 渠道条目给出，落盘在实例自己的数据目录里）。
//
// 落位的那份上游源码（`internal/provider/workbuddy/`）保持逐字不动，
// 好处是能直接 `git merge` 上游的 bug 修复——这也是当初选「fork + 照搬」
// 而不是「重写」的全部理由。
//
// ── 数据落点 ──────────────────────────────────────────────
//
// 全部落在编排器给出的实例数据目录下，与其它实例、与 ModelMux 自身数据互不干扰：
//
//	<data>/config.json        本实例的配置（含面板配置页要读写的字段）
//	<data>/state.json         账号池状态（积分、冷却、熔断、粘性）
//	<data>/usage.json         逐请求用量台账
//	<data>/model.json         模型目录本地缓存
//	<data>/request-logs/      脱敏请求归档（JSONL）
//	<data>/auths/             账号凭证（一账号一文件）
//	<data>/output_probes.json 模型输出上限实测（只读展示）
package workbuddy

import (
	"context"
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

	"modelmux/internal/config"
	"modelmux/internal/logging"
	"modelmux/internal/native"

	"modelmux/internal/provider/workbuddy/auth"
	"modelmux/internal/provider/workbuddy/livecfg"
	"modelmux/internal/provider/workbuddy/panel"
	"modelmux/internal/provider/workbuddy/pool"
	"modelmux/internal/provider/workbuddy/prompt"
	"modelmux/internal/provider/workbuddy/redisstore"
	"modelmux/internal/provider/workbuddy/reqlog"
	"modelmux/internal/provider/workbuddy/scheduler"
	"modelmux/internal/provider/workbuddy/server"
	"modelmux/internal/provider/workbuddy/upstream"
	"modelmux/internal/provider/workbuddy/usage"
)

// upstreamVersion 落位那份上游源码的版本号。
//
// 与上游 `cmd/server/main.go` 的 appVersion 对应；它只用于面板展示（「关于」与
// `/panel/api/overview`），不参与任何逻辑。上游升级时跟着改一次即可。
const upstreamVersion = "1.11.11-panel (native)"

// PanelAPIPrefix 上游管理 API 的前缀。
//
// 这是接缝①②的目标前缀：ModelMux 的代理把 `/api/channels/<name>/upstream/<rest>`
// 打到 `<root>/panel/api/<rest>`。集中导出是为了让它与渠道预设里的
// `PanelAPIPrefix` 有同一个可见出处，避免两边各写一份而漂移。
const PanelAPIPrefix = "/panel/api"

// HealthPath 上游的探活路径（编排器等就绪用）。
const HealthPath = "/healthz"

// PanelPath 上游自带面板的路径（供「打开上游面板」外链用）。
const PanelPath = "/panel/"

// Kind 本原生实现在注册表里的键，也是配置里 `kind` 的取值。
//
// 它与渠道识别用的「网关种类」是同一个值（workbuddy）——刻意的：配置里
// kind=workbuddy 的渠道，识别出来是 WorkBuddy 网关，装配的也就是本实现。
// 两套命名只会漂移，不会带来好处。
const Kind = "workbuddy"

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
	// 路由 Key 由 ModelMux 侧持有并在转发时注入。原生服务与它共用同一个值，
	// 这样「谁也别再抄一份密钥」——值为空即完全不鉴权（只绑回环，可接受）。
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
	lg.Info("原生 workbuddy：已加载账号", "count", len(auths), "auth_dir", authsDir)

	// ── 存储（未配置 Upstash 时自动降级为纯内存，不要求用户装 Redis）
	store := redisstore.New(nc.Upstash.URL, nc.Upstash.Token)
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}

	// ── 账号池
	p := pool.New(stateFile)
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用
	p.SyncToDir(auths)      // 与 auths 目录对齐
	p.SetBreaker(intOr(nc.Pool.BreakerThreshold, 3),
		nc.dur(nc.Pool.BreakerCooldown, 30*time.Minute),
		nc.dur(nc.Pool.BreakerCooldownMax, 6*time.Hour))
	p.SetDegrade(intOr(nc.Pool.DegradeThreshold, 5),
		nc.dur(nc.Pool.DegradeCooldown, 10*time.Minute),
		nc.dur(nc.Pool.DegradeCooldownMax, 2*time.Hour))
	p.SetSoftRateMax(nc.dur(nc.Cooldown.SoftRateMax, 2*time.Hour))
	p.SetCostExploreInterval(nc.dur(nc.Pool.CostExploreInterval, 30*time.Minute))
	p.SetCreditFloor(nc.Pool.CreditFloor)
	p.SetWeights(nc.Pool.IdleWeightPerHour, nc.Pool.IdleWeightMax)
	p.SetPreferExpiring(nc.Pool.PreferExpiring)
	p.SetMaxInFlight(nc.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(nc.Pool.MaxInFlightGlobal)

	// ── 上游客户端
	up := upstream.New()
	up.HTTP.Timeout = nc.durSeconds(nc.Upstream.TimeoutSeconds, 120*time.Second)
	up.HeaderTimeout = nc.durSeconds(nc.Upstream.HeaderTimeoutSeconds, up.HTTP.Timeout)
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	up.IdleTimeout = nc.durSeconds(nc.Upstream.IdleTimeoutSeconds, 300*time.Second)
	up.SanitizeFingerprints.Store(nc.Features.SanitizeBlacklistFingerprints)
	up.UserAgent = nc.Upstream.UserAgent
	up.ClientVersion = nc.Upstream.ClientVersion
	up.CliVersion = nc.Upstream.CliVersion
	up.ClientName = nc.Upstream.ClientName
	up.DeviceToken = nc.Upstream.DeviceToken
	up.DeviceTokenFile = nc.Upstream.DeviceTokenFile
	up.PassthroughIP = nc.Upstream.PassthroughIP
	up.GlobalEnabled = nc.Global.Enabled
	up.ChatBaseGlobal = nc.Global.ChatBase
	up.BillingBaseGlobal = nc.Global.BillingBase
	auth.SetGlobalEnabled(nc.Global.Enabled)
	// 模型目录缓存：与 state 文件同目录，缺失时回落上游内嵌种子。
	upstream.SetModelCatalogPath(filepath.Join(home, "model.json"))

	// 积分保底的「收费」兜底判据：接上游模型目录的积分倍率表。
	// 必须在 up 装配之后挂：闭包每次调用读实时快照。
	p.SetModelRateOf(func(realm, model string) string { return up.ModelRate(realm, model) })

	// ── 排程（签到 / 猫猫旅行 / 活跃上报 / token 保活 / 夜猫子 / 成长任务）
	sch := scheduler.New(scheduler.Config{
		Pool:               p,
		Upstream:           up,
		CheckinHours:       nc.Schedule.CheckinHours,
		TravelHours:        nc.Schedule.TravelHours,
		ActivityHours:      nc.Schedule.ActivityHours,
		KeepaliveHours:     nc.Schedule.KeepaliveHours,
		BlackcatHours:      nc.Schedule.BlackcatHours,
		GrowthHours:        nc.Schedule.GrowthHours,
		ExpiringSoonWindow: nc.dur(nc.Pool.ExpiringSoon, 168*time.Hour),
		CheckinDisabled:    !nc.Schedule.CheckinEnabled,
		TravelDisabled:     !nc.Schedule.TravelEnabled,
		ActivityDisabled:   !nc.Schedule.ActivityEnabled,
		KeepaliveDisabled:  !nc.Schedule.KeepaliveEnabled,
		BlackcatDisabled:   !nc.Schedule.BlackcatEnabled,
		GrowthDisabled:     !nc.Schedule.GrowthEnabled,
	})

	// ── 用量台账与请求归档
	rec := usage.New(filepath.Join(home, "usage.json"))
	rec.Start()
	requestLog := reqlog.New(reqlog.Config{
		Dir:           filepath.Join(home, "request-logs"),
		Enabled:       nc.Logging.RequestArchiveEnabled,
		RetentionDays: nc.Logging.RequestRetentionDays,
		MaxBytes:      int64(nc.Logging.RequestArchiveMaxMB) << 20,
	})

	// ── 运行期可变配置（面板在线改 api_key / soft_rate / 脱敏 / 来源记录）
	live := livecfg.New(livecfg.Snapshot{
		APIKey:               nc.APIKey,
		SoftCooldown:         nc.dur(nc.Cooldown.SoftRate, 600*time.Second),
		SanitizeFingerprints: nc.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     nc.Logging.RequestClientInfo,
	})

	// 提示词模式在装配期一次性解析（custom / append 才需要文本）。
	if nc.Prompt.Mode == "custom" || nc.Prompt.Mode == "append" {
		text, perr := prompt.Load(nc.Prompt.Mode, nc.Prompt.File)
		if perr != nil {
			cleanupRuntime(rec, requestLog, p, store)
			return nil, perr
		}
		nc.PromptText = text
	}

	// ── 面板
	pn := panel.New(panel.Config{
		Pool:        p,
		Upstream:    up,
		Scheduler:   sch,
		AuthDir:     authsDir,
		APIKey:      nc.APIKey,
		RedisMode:   redisMode,
		Version:     upstreamVersion,
		Live:        live,
		StickyCount: func() int { return 0 },
		Usage:       rec,
		RequestLog:  requestLog,
		ProbeFile:   filepath.Join(home, "output_probes.json"),
		ConfigPath:  cfgPath,
		LoadConfig:  func() (any, error) { return loadFileConfig(cfgPath) },
		SaveConfig: func(raw []byte) ([]string, error) {
			return saveFileConfig(cfgPath, nc, live, p, raw)
		},
	})
	// 成长任务队列每日自动执行（与面板「执行全部待办」同管线）。
	sch.SetGrowthHook(pn.RunGrowthQueueOnce)

	// ── 主 handler
	h := server.NewHandler(server.Config{
		Pool:             p,
		Upstream:         up,
		APIKey:           nc.APIKey,
		RedisMode:        redisMode,
		StickyCount:      func() int { return 0 },
		SoftCooldown:     nc.dur(nc.Cooldown.SoftRate, 600*time.Second),
		Panel:            pn,
		Live:             live,
		Usage:            rec,
		RequestLog:       requestLog,
		PromptMode:       nc.Prompt.Mode,
		PromptText:       nc.PromptText,
		RecordClientInfo: nc.Logging.RequestClientInfo,
		GlobalEnabled:    nc.Global.Enabled,
	})

	// 上游用标准 log 包输出运维日志（告警、签到结果、请求流水）。
	// 面板的「运行日志」视图读的是它自己的环形缓冲，而上游 main 通过
	// `log.SetOutput(MultiWriter(...))` 把日志镜像进去——不接这一步，
	// 面板日志视图就是空的。ModelMux 自身不使用标准 log 包（用的是
	// internal/logging），因此这里接管全局输出不会串味。
	restoreLog := mirrorLog(lg, host.Name, pn)

	// 排程与余额刷新常驻运行，直到 Close。
	ctx, cancel := context.WithCancel(context.Background())
	go sch.Run(ctx)
	sch.StartBalanceRefresh(ctx, nc.balanceRefreshInterval())
	go warmModelRates(ctx, up, p)

	var once sync.Once
	closeAll := func() error {
		once.Do(func() {
			cancel()
			restoreLog()
			cleanupRuntime(rec, requestLog, p, store)
		})
		return nil
	}
	return &native.Service{Handler: h, Close: closeAll}, nil
}

// cleanupRuntime 按序收尾：停记账/归档 → 账号池落盘 → 关后端存储。
//
// 顺序不能换：账号池 Flush 时后端存储还必须在（Redis 镜像要在关连接前写完），
// 而归档写入器必须在进程退出前停止，否则最后一笔记录会丢。
func cleanupRuntime(rec *usage.Recorder, rl *reqlog.Recorder, p *pool.Pool, store redisstore.Store) {
	rec.Stop()
	rl.Close()
	p.Flush()
	p.Close()
	_ = store.Close()
}

// mirrorLog 把标准 log 的输出同时镜像到 ModelMux 的结构化日志与面板环形缓冲，
// 返回恢复原输出的函数。
//
// 局限（已知且可接受）：标准 log 是进程级全局状态，同一进程内跑两个原生
// workbuddy 实例时，两条日志会同时进两个面板的环形缓冲。原生实例在
// ModelMux 里是单例（一个渠道），且日志本身按 provider 名打了标签，
// 所以只是观感问题，不影响功能。
func mirrorLog(lg *logging.Logger, name string, pn *panel.Panel) func() {
	prev := log.Writer()
	log.SetOutput(io.MultiWriter(prev, lg.ProviderWriter(name), pn.Logs()))
	return func() { log.SetOutput(prev) }
}

// warmModelRates 启动预热各域模型积分倍率表（供积分保底的目录兜底判定）。
//
// 倍率表只在 FetchModels / FetchGlobalModelInfos 成功时填充，两者都是懒触发；
// 重启后到首次触发之间的空窗期里 ModelRate 恒返回空串，保底的目录兜底判不出
// 收费，触底号会被当成「收费未知」放行并打穿。异步执行、失败只记日志。
func warmModelRates(ctx context.Context, up *upstream.Client, p *pool.Pool) {
	if ctx.Err() != nil {
		return
	}
	if uids := p.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if a := p.AuthByUID(uids[0]); a != nil {
			if _, err := up.FetchModels(a); err != nil {
				log.Printf("WARN: [upstream] warm model rates (cn): %v", err)
			}
		}
	}
	if !up.GlobalEnabled || ctx.Err() != nil {
		return
	}
	if uids := p.AvailableUIDsForRealm("global"); len(uids) > 0 {
		if a := p.AuthByUID(uids[0]); a != nil {
			_ = up.FetchGlobalModelInfos(a)
		}
	}
}

// ── 配置 ────────────────────────────────────────────────────

// fileConfig 本实例的配置，JSON 键与上游 config.json 一一对应。
//
// 为什么逐字对齐上游的键名：面板的「配置」视图是 schema 驱动的，读的就是这份
// 对象；键名一旦偏离，面板上就会少字段或多出无意义的空项。同时它也是
// ModelMux 侧唯一能改上游行为的地方（排程时点、池参数、脱敏开关）。
type fileConfig struct {
	Listen    string `json:"listen"`
	APIKey    string `json:"api_key"`
	AuthDir   string `json:"auth_dir"`
	StateFile string `json:"state_file"`

	Panel struct {
		PackageDetailLimit int `json:"package_detail_limit"`
	} `json:"panel"`

	Logging struct {
		RequestArchiveEnabled bool `json:"request_archive_enabled"`
		RequestRetentionDays  int  `json:"request_retention_days"`
		RequestArchiveMaxMB   int  `json:"request_archive_max_mb"`
		RequestClientInfo     bool `json:"request_client_info"`
	} `json:"logging"`

	Cooldown struct {
		SoftRate    string `json:"soft_rate"`
		SoftRateMax string `json:"soft_rate_max"`
	} `json:"cooldown"`

	Schedule struct {
		CheckinHours          []int `json:"checkin_hours"`
		TravelHours           []int `json:"travel_hours"`
		ActivityHours         []int `json:"activity_hours"`
		KeepaliveHours        []int `json:"keepalive_hours"`
		BlackcatHours         []int `json:"blackcat_hours"`
		GrowthHours           []int `json:"growth_hours"`
		CheckinEnabled        bool  `json:"checkin_enabled"`
		TravelEnabled         bool  `json:"travel_enabled"`
		ActivityEnabled       bool  `json:"activity_enabled"`
		KeepaliveEnabled      bool  `json:"keepalive_enabled"`
		BlackcatEnabled       bool  `json:"blackcat_enabled"`
		GrowthEnabled         bool  `json:"growth_enabled"`
		BalanceRefreshEnabled bool  `json:"balance_refresh_enabled"`
		BalanceRefreshMinutes int   `json:"balance_refresh_minutes"`
	} `json:"schedule"`

	Global struct {
		Enabled     bool   `json:"enabled"`
		ChatBase    string `json:"chat_base"`
		BillingBase string `json:"billing_base"`
	} `json:"global"`

	Upstream struct {
		TimeoutSeconds       int    `json:"timeout_seconds"`
		HeaderTimeoutSeconds int    `json:"header_timeout_seconds"`
		IdleTimeoutSeconds   int    `json:"idle_timeout_seconds"`
		UserAgent            string `json:"user_agent"`
		ClientVersion        string `json:"client_version"`
		CliVersion           string `json:"cli_version"`
		ClientName           string `json:"client_name"`
		DeviceToken          string `json:"device_token"`
		DeviceTokenFile      string `json:"device_token_file"`
		PassthroughIP        bool   `json:"passthrough_ip"`
	} `json:"upstream"`

	Features struct {
		SanitizeBlacklistFingerprints bool `json:"sanitize_blacklist_fingerprints"`
	} `json:"features"`

	Prompt struct {
		Mode string `json:"mode"`
		File string `json:"file"`
	} `json:"prompt"`

	Upstash struct {
		URL   string `json:"url"`
		Token string `json:"token"`
	} `json:"upstash"`

	Pool struct {
		MaxInFlight         int     `json:"max_in_flight"`
		MaxInFlightGlobal   int     `json:"max_in_flight_global"`
		BreakerThreshold    int     `json:"breaker_threshold"`
		BreakerCooldown     string  `json:"breaker_cooldown"`
		BreakerCooldownMax  string  `json:"breaker_cooldown_max"`
		DegradeThreshold    int     `json:"degrade_threshold"`
		DegradeCooldown     string  `json:"degrade_cooldown"`
		DegradeCooldownMax  string  `json:"degrade_cooldown_max"`
		IdleWeightPerHour   float64 `json:"idle_weight_per_hour"`
		IdleWeightMax       float64 `json:"idle_weight_max"`
		PreferExpiring      bool    `json:"prefer_expiring"`
		ExpiringSoon        string  `json:"expiring_soon"`
		CostExploreInterval string  `json:"cost_explore_interval"`
		CreditFloor         int64   `json:"credit_floor"`
	} `json:"pool"`

	SessionSticky struct {
		Enabled    bool   `json:"enabled"`
		TTL        string `json:"ttl"`
		GCInterval string `json:"gc_interval"`
	} `json:"session_sticky"`

	// PromptText 解析后的提示词文本（custom/append 模式使用），不落盘。
	PromptText string `json:"-"`
}

// defaultFileConfig 与上游 Default() 逐项对齐。
func defaultFileConfig() *fileConfig {
	c := &fileConfig{Listen: "127.0.0.1:0", AuthDir: "./auths", StateFile: "./state.json"}
	c.Panel.PackageDetailLimit = 5
	c.Logging.RequestArchiveEnabled = true
	c.Logging.RequestRetentionDays = 7
	c.Logging.RequestArchiveMaxMB = 100
	c.Logging.RequestClientInfo = true
	c.Cooldown.SoftRate = "600s"
	c.Cooldown.SoftRateMax = "2h"
	c.Schedule.CheckinHours = []int{9, 21}
	c.Schedule.TravelHours = []int{9, 21}
	c.Schedule.ActivityHours = []int{10}
	c.Schedule.KeepaliveHours = []int{22}
	c.Schedule.BlackcatHours = []int{23}
	c.Schedule.GrowthHours = []int{1}
	c.Schedule.CheckinEnabled = true
	c.Schedule.TravelEnabled = true
	c.Schedule.ActivityEnabled = true
	c.Schedule.KeepaliveEnabled = true
	c.Schedule.BlackcatEnabled = true
	c.Schedule.GrowthEnabled = true
	c.Schedule.BalanceRefreshEnabled = true
	c.Schedule.BalanceRefreshMinutes = 5
	c.Upstream.TimeoutSeconds = 120
	c.Global.Enabled = true
	c.Features.SanitizeBlacklistFingerprints = true
	c.Prompt.Mode = "passthrough"
	c.Pool.MaxInFlight = 3
	c.Pool.MaxInFlightGlobal = 2
	c.Pool.BreakerThreshold = 3
	c.Pool.BreakerCooldown = "30m"
	c.Pool.BreakerCooldownMax = "6h"
	c.Pool.DegradeThreshold = 5
	c.Pool.DegradeCooldown = "10m"
	c.Pool.DegradeCooldownMax = "2h"
	c.Pool.IdleWeightPerHour = 0.5
	c.Pool.IdleWeightMax = 5.0
	c.Pool.PreferExpiring = true
	c.Pool.ExpiringSoon = "168h"
	c.Pool.CostExploreInterval = "30m"
	c.SessionSticky.Enabled = true
	c.SessionSticky.TTL = "30m"
	c.SessionSticky.GCInterval = "5m"
	return c
}

// loadFileConfig 读配置；文件不存在时落一份默认配置再返回。
//
// 「先取默认再 JSON 覆盖」与上游 Load 同一手法：缺键保留默认，只有显式 false
// 才关掉开关。这样用户手改配置删掉一行，不会静默翻转整块行为。
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

// normalize 校验并归一（与上游 normalize 同口径的轻量版）。
func (c *fileConfig) normalize() error {
	switch strings.ToLower(strings.TrimSpace(c.Prompt.Mode)) {
	case "", "passthrough":
		c.Prompt.Mode = "passthrough"
	case "custom":
		c.Prompt.Mode = "custom"
	case "append":
		c.Prompt.Mode = "append"
	default:
		return fmt.Errorf("prompt.mode: %q 不是合法值（passthrough / custom / append）", c.Prompt.Mode)
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.Upstream.HeaderTimeoutSeconds <= 0 {
		c.Upstream.HeaderTimeoutSeconds = c.Upstream.TimeoutSeconds
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if c.Pool.MaxInFlightGlobal <= 0 {
		c.Pool.MaxInFlightGlobal = 2
	}
	if c.Pool.BreakerThreshold <= 0 {
		c.Pool.BreakerThreshold = 3
	}
	if c.Pool.DegradeThreshold <= 0 {
		c.Pool.DegradeThreshold = 5
	}
	if c.Schedule.BalanceRefreshMinutes <= 0 {
		c.Schedule.BalanceRefreshMinutes = 5
	}
	if c.Panel.PackageDetailLimit <= 0 {
		c.Panel.PackageDetailLimit = 5
	}
	if c.Logging.RequestRetentionDays <= 0 {
		c.Logging.RequestRetentionDays = 7
	}
	if c.Logging.RequestArchiveMaxMB <= 0 {
		c.Logging.RequestArchiveMaxMB = 100
	}
	for _, d := range []struct{ name, val string }{
		{"cooldown.soft_rate", c.Cooldown.SoftRate},
		{"cooldown.soft_rate_max", c.Cooldown.SoftRateMax},
		{"pool.breaker_cooldown", c.Pool.BreakerCooldown},
		{"pool.breaker_cooldown_max", c.Pool.BreakerCooldownMax},
		{"pool.degrade_cooldown", c.Pool.DegradeCooldown},
		{"pool.degrade_cooldown_max", c.Pool.DegradeCooldownMax},
		{"pool.expiring_soon", c.Pool.ExpiringSoon},
		{"pool.cost_explore_interval", c.Pool.CostExploreInterval},
	} {
		if strings.TrimSpace(d.val) == "" {
			continue
		}
		if _, err := time.ParseDuration(d.val); err != nil {
			return fmt.Errorf("%s: %q 不是合法的时长（如 600s / 2h / 30m）", d.name, d.val)
		}
	}
	return nil
}

// saveFileConfig 校验 → 落盘 → 热应用 → 返回需重启字段。
//
// 「热生效」字段与上游一致：api_key / soft_rate / 脱敏开关 / 来源记录 / 积分保底。
// 其余字段（排程、上游超时、会话粘性…）在构造期一次性读入，改了要重启渠道才生效——
// 如实把它们列进 restart_required，而不是假装已经生效。
func saveFileConfig(path string, cur *fileConfig, live *livecfg.Holder, p *pool.Pool,
	raw []byte) ([]string, error) {

	next := *cur // 用当前值做底：面板提交的是完整对象，但缺键时保留当前值更安全
	if err := json.Unmarshal(raw, &next); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := next.normalize(); err != nil {
		return nil, err
	}
	if err := writeFileConfig(path, &next); err != nil {
		return nil, err
	}

	// 热生效
	live.Store(livecfg.Snapshot{
		APIKey:               next.APIKey,
		SoftCooldown:         next.dur(next.Cooldown.SoftRate, 600*time.Second),
		SanitizeFingerprints: next.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     next.Logging.RequestClientInfo,
	})
	if next.Pool.CreditFloor != cur.Pool.CreditFloor {
		p.SetCreditFloor(next.Pool.CreditFloor)
	}

	restart := []string{}
	add := func(changed bool, field string) {
		if changed {
			restart = append(restart, field)
		}
	}
	// 注意：Schedule 含切片，整体不可比较，逐字段比。
	s := next.Schedule
	o := cur.Schedule
	add(!sameInts(s.CheckinHours, o.CheckinHours), "schedule.checkin_hours")
	add(!sameInts(s.TravelHours, o.TravelHours), "schedule.travel_hours")
	add(!sameInts(s.ActivityHours, o.ActivityHours), "schedule.activity_hours")
	add(!sameInts(s.KeepaliveHours, o.KeepaliveHours), "schedule.keepalive_hours")
	add(!sameInts(s.BlackcatHours, o.BlackcatHours), "schedule.blackcat_hours")
	add(!sameInts(s.GrowthHours, o.GrowthHours), "schedule.growth_hours")
	add(next.Prompt != cur.Prompt, "prompt")
	add(next.Upstream != cur.Upstream, "upstream")
	add(next.Pool != cur.Pool, "pool")
	add(next.Upstash != cur.Upstash, "upstash")
	add(next.Global != cur.Global, "global")
	add(next.SessionSticky != cur.SessionSticky, "session_sticky")

	// 面板保存后当前配置对象要跟上，否则下一次保存会把「已生效的值」当成旧值比对。
	*cur = next
	return restart, nil
}

func sameInts(a, b []int) bool {
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

// ── 取值辅助 ────────────────────────────────────────────────

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

// balanceRefreshInterval 余额后台刷新间隔；开关关闭时返回 0（不启动）。
func (c *fileConfig) balanceRefreshInterval() time.Duration {
	if !c.Schedule.BalanceRefreshEnabled {
		return 0
	}
	return time.Duration(c.Schedule.BalanceRefreshMinutes) * time.Minute
}

func intOr(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}
