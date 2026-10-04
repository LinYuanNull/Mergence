// external.go 「外部接管」：把一个**已经在运行**的外部网关纳入 Mergence 管理。
//
// ── 为什么需要它 ─────────────────────────────────────────────
//
// 内置原生（workbuddy / zcode / trae）是「Mergence 自己装配上游、自己起服务」。
// 但用户可能已经自己部署了 zcode2api / wb2api（甚至跑在另一台机器上），这时
// Mergence 要的只是「把它的地址接进来」：代理、控制台、领取执行器全都只依赖
// 一个 RootURL + 一个就绪状态，指向外部与指向内置在接缝看来**没有区别**。
//
// 它也是 e2e 测试的唯一可行通路：独立子进程模式移除后，测试不能再让 Mergence
// 去拉起 fake 上游；改为测试自己把 fake 起成独立服务，再用本通路接进来。
//
// ── 为什么用 Env 键，而不是新增一个 kind ─────────────────────
//
// kind 还兼任「网关种类」：控制台形态、管理 API 前缀（/panel/api 对 /admin/api）
// 都按它判定。接管一个 zcode 网关时 kind **必须仍是 zcode**，否则控制台会按错的
// 前缀去探、整块 401/404。所以「接管」是一种**传输方式**，不是一种网关：
// kind 说它是什么，这个键说它在哪。
package native

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"mergence/internal/config"
	"mergence/internal/logging"
)

// ExternalURLKey 渠道 Env 里的键：外部接管的目标地址（形如 http://127.0.0.1:7864）。
//
// 用环境变量而不是新增配置字段，是为了让「接管」保持为一个可选开关：
// 不填 = 用内置实现，填了 = 接管外部。
const ExternalURLKey = "MERGENCE_EXTERNAL_URL"

// Resolve 选出该渠道要装配的原生实现。
//
// 给了 ExternalURLKey 就接管外部网关；否则按 kind 找内置实现。
// 编排器只用这一个入口，不必自己判断是内置还是接管。
func Resolve(cfg config.ManagedProvider) (Boot, bool) {
	if strings.TrimSpace(cfg.Env[ExternalURLKey]) != "" {
		return externalBoot, true
	}
	return Lookup(cfg.Kind)
}

// externalBoot 装配一个反代到外部网关的服务。
//
// dataDir 刻意不用：外部网关的数据在它自己那里，Mergence 不碰。
func externalBoot(cfg config.ManagedProvider, _ string, lg *logging.Logger) (*Service, error) {
	target := strings.TrimSpace(cfg.Env[ExternalURLKey])
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%s=%q 不是合法的 http(s) 地址", ExternalURLKey, target)
	}

	tr := &http.Transport{
		// 保守上限：只服务本进程与面板代理，没有慢速公网客户端。
		MaxIdleConns:        32,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.Transport = tr
	// 默认 Director 保留原始 Host（Mergence 自己的回环地址）。对外部网关应发它
	// 自己的 Host —— 有些网关按 Host 做虚拟主机或校验。
	inner := rp.Director
	rp.Director = func(r *http.Request) {
		inner(r)
		r.Host = u.Host
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		lg.Warn("外部接管转发失败", "provider", cfg.Name,
			"path", r.URL.Path, "err", err.Error())
		w.WriteHeader(http.StatusBadGateway)
	}

	lg.Info("已接管外部网关", "provider", cfg.Name, "target", u.String())
	return &Service{
		Handler: rp,
		// 上游不是我们拉起的，也就没有「停它」这回事；只收回我们自己的连接池。
		Close: func() error { tr.CloseIdleConnections(); return nil },
	}, nil
}
