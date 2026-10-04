// Package zcode 把 zcode2api 的原生实现装配成一个 Mergence 进程内服务。
//
// ── 这个包为什么存在 ────────────────────────────────────────
//
// Track 2 产出的 `zcode2api-go`（`LinYuanNull/zcode2api-go`，MIT）是一套**独立可用**
// 的 HTTP 服务：自带 `http.ServeMux` 路由表、管理面板宿主、CLI 子命令、appdir 解析。
// 到了 Mergence 这里，那些「服务外壳」全部由编排层承担 —— Mergence 已经起了回环
// HTTP 服务、已经有面板、已经有数据目录。于是 Track 3 做的不是「把那个仓库搬过来」，
// 而是**按 Mergence 的 provider 接口面重新组织它的业务层**：
//
//	去掉：internal/server（独立服务外壳）、cmd/（CLI）、internal/appdir（数据目录解析）
//	保留：store / models / settings / agent / identity / bodytransform / compat /
//	      scheduler / gateway / quota / claim / oauth / captcha / adminapi / …
//
// 装配出来的仍然是一个 `http.Handler`（见 native.Service）—— 因为四条接缝
// （前端代理 / 控制台代理 / 领取执行器 / 数据面）都是「打一个 HTTP 端点」的形态，
// 保持 handler 形态就能让它们**一行不改**地同时服务原生型与托管型。
//
// ── 与「照搬上游源码」型的区别（workbuddy / trae）────────────
//
// 那两个是 MIT 上游 + 逐字照搬，好处是能 `git merge` 上游 bug 修复。
// zcode 走的是**另一条路**：上游（`dengyie/zcode2api`）是 AGPL-3.0，**不能照搬**，
// 所以 Track 2 起就是**独立重写**（契约驱动、按实测样本实现），
// 这里只是把那份独立重写的业务层改挂到 Mergence 的接缝上。
// 结果是：本仓库里**没有任何上游代码**，因此 `src/THIRD-PARTY-LICENSES/` 里
// **没有 zcode 目录**（对照 workbuddy2api-panel / trae2api-web 两个）。
//
// ── 密码合并（Track 3 最大的简化收益）─────────────────────────
//
// 独立部署时密码有**两处**：zcode2api 自己的 `ZCODE_ADMIN_KEY`（首启写进
// accounts.db 的 meta 表，之后以库为准），以及 Mergence 侧 `config.Claim.AdminKey`
// —— 后者被 `web/proxy.go:panelAuthKey` 与 `claim/scheduler` 消费。
// 「只改一边 = 内置面板整块 401 + 定时领取失效」是独立部署最经典的故障。
//
// 合并后只剩一处：本包把 **Mergence 渠道的 route key**（`host.Route.APIKey`）
// 当作唯一的后台密码，装配时**同时**注入到管理面鉴权器与账号库的 meta 初值里。
// 于是 web 层的 `panelAuthKey` / `claimKeyFor` 都只需要取 route key，
// 「两处同步」这件事从根上消失。
//
// ── 数据落点 ──────────────────────────────────────────────
//
// 全部落在编排器给出的实例数据目录下，与其它实例、与 Mergence 自身数据互不干扰：
//
//	<data>/accounts.db   账号池与设置（SQLite，落盘契约见 store 包）
//	<data>/panel/        面板静态资源（可选，由 Mergence 侧指向上游 frontend/）
package zcode

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"mergence/internal/config"
	"mergence/internal/logging"
	"mergence/internal/native"

	"mergence/internal/provider/zcode/adminapi"
	"mergence/internal/provider/zcode/agent"
	"mergence/internal/provider/zcode/authadmin"
	"mergence/internal/provider/zcode/captcha"
	"mergence/internal/provider/zcode/claim"
	"mergence/internal/provider/zcode/constants"
	"mergence/internal/provider/zcode/gateway"
	"mergence/internal/provider/zcode/httpx"
	"mergence/internal/provider/zcode/oauth"
	"mergence/internal/provider/zcode/pages"
	"mergence/internal/provider/zcode/quota"
	"mergence/internal/provider/zcode/reqlog"
	"mergence/internal/provider/zcode/settings"
	"mergence/internal/provider/zcode/store"
)

// Kind 本原生实现在注册表里的键，也是配置里 `kind` 的取值。
//
// 与独立部署时的 `zcode` 同名：渠道配置在两种形态之间切换时，`kind` 一个字符
// 都不用改（这正是接缝架构想要的效果）。
const Kind = "zcode"

// PanelAPIPrefix 管理 API 的前缀。
//
// 集中导出是为了让它与渠道预设里的 `PanelAPIPrefix`、以及 web 层契约表
// `panelAPIPrefixFor` 有同一个可见出处 —— 三处必须一致，否则管理代理会
// 拿着 `/panel/api` 去打一个只认 `/admin/api` 的服务，表现为静默 404。
const PanelAPIPrefix = "/admin/api"

// HealthPath 探活路径（编排器等就绪用）。
//
// `/meta` 是上游就有的端点（只有 version 一个键），托管型预设里也配的它，
// 保持同名能让「托管 → 原生」的迁移对用户完全透明。
const HealthPath = "/meta"

// PanelPath 上游自带面板的路径（供「打开上游面板」外链用）。
const PanelPath = "/admin/"

// versionLabel `/meta` 对外报告的版本号。
//
// 它不参与任何逻辑，只用于面板展示与排查；取「原生内嵌」这种明确措辞，
// 免得有人把它当成 zcode2api 的上游版本号去对照它的 release。
const versionLabel = "zcode-native (embedded)"

func init() { native.Register(Kind, Boot) }

// Boot 装配并返回进程内服务。
//
// dataDir 是编排器给的实例数据目录；账号库落在它下面的 accounts.db，
// 因此把老部署的 data/ 目录指过来即可沿用已登录的账号（与 workbuddy 同款语义）。
func Boot(host config.ManagedProvider, dataDir string, lg *logging.Logger) (*native.Service, error) {
	home := dataDir
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, fmt.Errorf("创建原生实例数据目录失败：%w", err)
	}

	// ── 唯一的后台密码（见包注释「密码合并」）──────────────
	//
	// 取渠道 route key。空时**不改动**库里已有的密码（用户可能刚把老部署的
	// data/ 指过来，里面有他设好的密码）；库里没有时由 NewConfigured 的
	// NormalizeAdminKey 回落到契约默认值 `zcode`
	// （实测：不带 ZCODE_ADMIN_KEY 起靶机、用 `Bearer zcode` 得 200）。
	//
	// 非空时**覆盖**（不是仅首启 seed）：内嵌形态下 route key 是后台密码的
	// 真源 —— 否则用户改了渠道的 route key 却不生效，就是「只改一边就整块 401」
	// 换了个方向重现。上游「首启写库、之后以库为准」对**独立部署**是对的，
	// 内嵌形态下由这一处覆盖显式取代。
	adminKey := routeKey(host)

	configured := settings.NewConfigured(
		adminKey, // 空 ⇒ NewConfigured 内部 NormalizeAdminKey 回落 zcode
		adminKey, // gateway_key：合并后与后台密码同源（一把钥匙管两条通道）
		constants.DefaultQuotaRefreshInterval,
		constants.DefaultAccountConcurrency,
		constants.DefaultClaimRoundInterval,
	)

	st, err := store.Open(filepath.Join(home, "accounts.db"),
		store.WithInitialSettings(configured),
		store.WithForcedSetting(constants.MetaAdminKey, settings.NormalizeAdminKey(adminKey)),
		store.WithForcedSetting(constants.MetaGatewayKey, adminKey),
	)
	if err != nil {
		return nil, fmt.Errorf("打开账号库失败：%w", err)
	}
	lg.Info("原生 zcode：账号库已就绪",
		"path", st.Path(), "accounts", len(st.List()), "admin_key_set", adminKey != "")

	// ── 密钥实时读取（不是快照）──────────────────────────
	//
	// `PUT /admin/api/settings` 改完后台密码后，下一次请求必须按新值鉴权 ——
	// 这正是独立部署里「改密码要两处同步」那个坑的根源。这里两个闭包都从
	// 设置快照**实时**读，改一处即全生效。
	cache := settings.NewCache(st, configured)
	adminKeyFn := func() string {
		s, err := cache.Get()
		if err != nil {
			return ""
		}
		return s.AdminKey
	}
	gatewayKeyFn := func() string {
		s, err := cache.Get()
		if err != nil {
			return ""
		}
		return s.GatewayKey
	}

	guard := authadmin.New(adminKeyFn)
	ring := reqlog.New(constants.MonitoringKeep)
	out := agent.New()

	sessions := oauth.NewService(oauth.NewRegistry(), out)

	var quotaSvc *quota.Service
	var interval int64
	if s, err := cache.Get(); err == nil {
		interval = s.QuotaRefreshInterval
	}
	quotaSvc = quota.NewService(out, st, interval)

	claimer := claim.NewService(st)

	captchaProvider := captcha.NewManager(captcha.Options{
		Source: out,
		Solver: captcha.NewCDPSolver(captcha.SolveOptions{Logf: logf(lg, host.Name)}),
		Logf:   logf(lg, host.Name),
	})

	api := adminapi.New(adminapi.Deps{
		Store:      st,
		Guard:      guard,
		Ring:       ring,
		Sessions:   sessions,
		Quota:      quotaSvc,
		Claimer:    claimer,
		Captcha:    captchaProvider,
		Settings:   cache,
		Configured: configured,
	})

	gw := gateway.New(gateway.Options{
		Store:      st,
		GatewayKey: gatewayKeyFn,
	})

	// 面板静态资源：Mergence 侧若指了目录就挂上，否则走明确说明的占位页
	// （与独立部署 `--panel-dir` 未配时的行为一致）。
	panel := &pages.Handler{Dir: panelDir(host, home), Version: versionLabel}

	mux := http.NewServeMux()
	mux.HandleFunc(HealthPath, metaHandler())
	mux.Handle(PanelAPIPrefix+"/", api)
	mux.Handle("/v1/", gw)
	mux.Handle("/", panel)

	// 领取调度：独立部署时由 cli serve 起一个常驻循环；合并后**关掉它** ——
	// Mergence 的「限时套餐自动领取」（internal/claim）统一接管定时，
	// 两套调度同时跑会重复领取。这里只提供 `POST /admin/api/claim` 让
	// Mergence 从进程外触发（与 trae 的签到入口同款设计）。
	//
	// 启动自刷保留：它是「启动触发一次额度查询」的实测行为（observations.md 4.4），
	// 与定时无关。放 goroutine 里，不阻塞 Boot。
	go quotaSvc.RefreshActives()

	var once sync.Once
	closeAll := func() error {
		once.Do(func() {
			_ = st.Close()
		})
		return nil
	}
	lg.Info("原生 zcode 已装配",
		"version", versionLabel, "panel_dir", panel.Dir, "admin_key_set", adminKey != "")
	return &native.Service{Handler: mux, Close: closeAll}, nil
}

// EffectiveAdminKey 返回合并后**实际生效**的后台密码：渠道 route key；
// 为空时回落契约默认值 `zcode`。
//
// 为什么导出：web 层的 `panelAuthKey` 与 `claimKeyFor` 必须发**同一个值**，
// 否则「面板用空、后端用 zcode」就是那个经典的整块 401。两处都调本函数
// 就不会漂移 —— 独立部署时这件事靠人工「两处同步」，合并后变成一次函数调用。
func EffectiveAdminKey(host config.ManagedProvider) string {
	return settings.NormalizeAdminKey(routeKey(host))
}

// routeKey 取渠道的 route key（合并后的唯一后台密码来源）。
func routeKey(host config.ManagedProvider) string {
	if host.Route == nil {
		return ""
	}
	return strings.TrimSpace(host.Route.APIKey)
}

// panelDir 决定面板静态资源目录。
//
// 优先级：渠道 Env 里的 `ZCODE_PANEL_DIR`（用户显式覆盖）→ 实例数据目录下
// 约定位置 `<data>/panel/`。都没有时返回空串，pages.Handler 会给占位页。
//
// 为什么不默认去指上游 clone 的 frontend/：Mergence 不随包分发上游前端
// （许可纪律），用户的 frontend/ 在哪只有用户知道，猜一个绝对路径必然错。
func panelDir(host config.ManagedProvider, home string) string {
	if v := strings.TrimSpace(host.Env["ZCODE_PANEL_DIR"]); v != "" {
		return v
	}
	cand := filepath.Join(home, "panel")
	if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
		return cand
	}
	return ""
}

// logf 把验证码链路的诊断接到 Mergence 的结构化日志上。
func logf(lg *logging.Logger, name string) func(string, ...any) {
	return func(format string, args ...any) {
		lg.Info(fmt.Sprintf("zcode captcha: "+format, args...), "provider", name)
	}
}

// metaResponse 是 `GET /meta` 的响应体。**只有一个键**（实测靶机如此）。
type metaResponse struct {
	Version string `json:"version"`
}

// metaHandler 是健康/版本端点。Mergence 的预设把 `health_path` 配成它。
func metaHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			httpx.WriteDetail(w, http.StatusMethodNotAllowed, "Method Not Allowed")
			return
		}
		httpx.WriteJSON(w, http.StatusOK, metaResponse{Version: versionLabel})
	}
}

// 编译期断言：装配结果必须满足 native 的 Boot 形状。
var _ native.Boot = Boot

// errNoDataDir 保留一个明确错误，供将来 dataDir 校验用（当前由编排器保证非空）。
var errNoDataDir = errors.New("原生 zcode：数据目录为空")
