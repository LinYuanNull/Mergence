// ports.go 动态端口分配。
//
// 为什么不用纯 OS 自动分配：托管型 provider 基本不接受监听 :0，而且用户需要知道
// 端口才能把 base_url 填进客户端。所以策略是「编排器选端口 → 传给子进程 → 面板明示」。
//
// 关键设计是**持久化复用**：把上次成功使用的端口记进 ports.json，重启时优先复用，
// 否则用户的书签、外部客户端、dsh 插件里的 base_url 每次重启都会失效。
package orchestrator

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// ErrPortUnavailable 固定端口模式下端口被占用。
var ErrPortUnavailable = errors.New("端口已被占用")

type portState struct {
	Version int            `json:"version"`
	Last    map[string]int `json:"last"` // provider name -> 上次成功使用的端口
}

// PortAllocator 负责端口的选择、冲突检测与持久化。
type PortAllocator struct {
	path     string
	reuse    bool
	maxRetry int

	mu   sync.Mutex
	last map[string]int
}

// NewPortAllocator 加载 ports.json（不存在则为空）。
func NewPortAllocator(path string, reuse bool, maxRetry int) (*PortAllocator, error) {
	if maxRetry <= 0 {
		maxRetry = 3
	}
	a := &PortAllocator{path: path, reuse: reuse, maxRetry: maxRetry, last: map[string]int{}}
	raw, err := os.ReadFile(path)
	if err == nil {
		var st portState
		if json.Unmarshal(raw, &st) == nil && st.Last != nil {
			a.last = st.Last
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("读取 %s 失败：%w", path, err)
	}
	return a, nil
}

// Allocate 为某个 provider 选一个可用端口。
//
// fixed > 0 表示该 provider 硬编码端口、不支持动态分配（如 glm-zcode-2api 的 7864）：
// 此时走降级模式——只做冲突预检，冲突就明确报错，绝不静默失败。
func (a *PortAllocator) Allocate(name string, fixed int) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if fixed > 0 {
		if !IsFree(fixed) {
			return 0, fmt.Errorf("%w：%s 固定端口 %d 已被占用；请先释放该端口或改用其他 provider",
				ErrPortUnavailable, name, fixed)
		}
		a.last[name] = fixed
		return fixed, nil
	}

	var tried []int
	// 1) 优先复用上次的端口
	if a.reuse {
		if p, ok := a.last[name]; ok && p > 0 && IsFree(p) {
			return p, nil
		}
	}
	// 2) 向 OS 申请一个空闲端口，重试若干次（存在「探测后到真正监听前被抢」的窄窗口）
	for i := 0; i <= a.maxRetry; i++ {
		p, err := probeFreePort()
		if err != nil {
			return 0, err
		}
		if !contains(tried, p) && IsFree(p) {
			a.last[name] = p
			return p, nil
		}
		tried = append(tried, p)
	}
	return 0, fmt.Errorf("连续 %d 次未能取得可用端口，请检查本机端口资源", a.maxRetry+1)
}

// Save 持久化端口映射。只在进程正常退出或端口变化时调用。
func (a *PortAllocator) Save() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(a.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(portState{Version: 1, Last: a.last}, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

// Snapshot 返回当前端口映射的副本，供面板展示 base_url。
func (a *PortAllocator) Snapshot() map[string]int {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[string]int, len(a.last))
	for k, v := range a.last {
		out[k] = v
	}
	return out
}

func contains(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// probeFreePort 让 OS 分配一个空闲端口后立即释放。
func probeFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("探测空闲端口失败：%w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}

// IsFree 判断 127.0.0.1 上的某个端口当前是否可监听。
func IsFree(port int) bool {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}
