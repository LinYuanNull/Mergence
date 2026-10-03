// keypool.go 多 Key 轮询池。
//
// 一个渠道可以填多个 API Key（很多中转站按 Key 计限额）。这里做的是：
//   - 轮询取用，让负载均摊
//   - 失败按「是否 Key 的问题」分档冷却 —— 401/403 冷却最久（Key 可能已失效），
//     429 中等（限流），网络/5xx 最短（大概率不是 Key 的锅）
//   - 某一档冷却中的 Key 不再被取用，但只要冷却到期就自动回归
//
// 刻意**不做**「永久拉黑」：Key 失效往往是暂时的（改套餐、临时封禁、重置额度），
// 永久拉黑会让用户在面板上看到「明明填了 3 个 Key 却只用 1 个」而找不到原因。
package provider

import (
	"sync"
	"time"
)

// 冷却时长分档。
const (
	coolAuth    = 10 * time.Minute // 401/403：Key 本身可能有问题
	coolRate    = 60 * time.Second // 429：限流，稍后即可
	coolTransit = 5 * time.Second  // 网络错误 / 5xx：上游抖动
)

type keyState struct {
	key      string
	failures int
	used     int64
	lastErr  string
	coolTill time.Time
}

// KeyStat 对外暴露的 Key 状态（Key 已脱敏）。
type KeyStat struct {
	Masked    string    `json:"masked"`
	Failures  int       `json:"failures"`
	Used      int64     `json:"used"`
	LastErr   string    `json:"last_err,omitempty"`
	Cooling   bool      `json:"cooling"`
	CoolUntil time.Time `json:"cool_until,omitempty"`
}

// KeyPool 轮询池。零 Key 时 Pick 返回空串，调用方据此跳过鉴权头。
type KeyPool struct {
	mu   sync.Mutex
	keys []*keyState
	next int
}

// NewKeyPool 创建池。
func NewKeyPool(keys []string) *KeyPool {
	p := &KeyPool{}
	for _, k := range keys {
		p.keys = append(p.keys, &keyState{key: k})
	}
	return p
}

// Len 返回 Key 数量。
func (p *KeyPool) Len() int { return len(p.keys) }

// Empty 是否没有配置 Key。
func (p *KeyPool) Empty() bool { return len(p.keys) == 0 }

// Pick 取一个 Key。
//
// 返回的 allCooling 表示「全部 Key 都在冷却中」——此时仍然返回冷却最早结束的那个，
// 因为「用可能已被限流的 Key 试一次」比「直接失败」更可能成功。
//
// 注意 Pick 不会把 Key 标记为占用：流式请求要跑很久，把 Key 锁住会导致并发请求
// 被无谓地挤到次优 Key 上。轮询 + 失败冷却已经足够。
func (p *KeyPool) Pick() (key string, allCooling bool, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.keys) == 0 {
		return "", false, true
	}

	now := time.Now()
	n := len(p.keys)
	for i := 0; i < n; i++ {
		idx := (p.next + i) % n
		if !p.keys[idx].coolTill.After(now) {
			p.next = (idx + 1) % n
			return p.keys[idx].key, false, true
		}
	}

	// 全在冷却：挑最早解冻的
	best, bestTill := 0, p.keys[0].coolTill
	for i, k := range p.keys {
		if k.coolTill.Before(bestTill) {
			best, bestTill = i, k.coolTill
		}
	}
	p.next = (best + 1) % n
	return p.keys[best].key, true, true
}

// ReportSuccess 上报成功：清零失败计数并解除冷却。
func (p *KeyPool) ReportSuccess(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ks := p.find(key); ks != nil {
		ks.failures = 0
		ks.lastErr = ""
		ks.coolTill = time.Time{}
		ks.used++
	}
}

// ReportFailure 上报失败。status<=0 表示传输层错误（连接失败、超时）。
//
// 只对「可能是 Key 的问题」的状态码惩罚 Key；400/404/422 这类是请求本身的问题，
// 归咎于 Key 会让用户换 Key 也解决不了，反而掩盖真实原因。
func (p *KeyPool) ReportFailure(key string, status int) {
	cool, penalize := classifyKeyFailure(status)
	if !penalize {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if ks := p.find(key); ks != nil {
		ks.failures++
		ks.lastErr = statusText(status)
		// 失败次数越多冷却越久（上限 8 倍），避免反复撞同一个坏 Key
		mult := ks.failures
		if mult > 8 {
			mult = 8
		}
		ks.coolTill = time.Now().Add(cool * time.Duration(mult))
	}
}

// classifyKeyFailure 判断某状态码是否归咎于 Key，以及冷却多久。
func classifyKeyFailure(status int) (time.Duration, bool) {
	switch {
	case status == 401 || status == 403:
		return coolAuth, true
	case status == 429:
		return coolRate, true
	case status == 408 || status == 409:
		return coolTransit, true
	case status >= 500:
		return coolTransit, true
	case status <= 0:
		return coolTransit, true
	default:
		// 400/404/413/422 等：请求内容的问题，Key 不背锅
		return 0, false
	}
}

func statusText(status int) string {
	if status <= 0 {
		return "传输层错误"
	}
	return "HTTP " + itoa(status)
}

func (p *KeyPool) find(key string) *keyState {
	for _, k := range p.keys {
		if k.key == key {
			return k
		}
	}
	return nil
}

// Stats 返回各 Key 状态（脱敏）。仅面板展示用。
func (p *KeyPool) Stats() []KeyStat {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	out := make([]KeyStat, 0, len(p.keys))
	for _, k := range p.keys {
		out = append(out, KeyStat{
			Masked:    MaskKey(k.key),
			Failures:  k.failures,
			Used:      k.used,
			LastErr:   k.lastErr,
			Cooling:   k.coolTill.After(now),
			CoolUntil: k.coolTill,
		})
	}
	return out
}

// MaskKey 脱敏：保留前 6 后 4，中间用 ● 代替。太短的 Key 全部遮掉。
func MaskKey(k string) string {
	r := []rune(k)
	if len(r) == 0 {
		return ""
	}
	if len(r) <= 12 {
		return "●●●●"
	}
	return string(r[:6]) + "●●●●" + string(r[len(r)-4:])
}

// itoa 避免为一个小转换引入 strconv。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
