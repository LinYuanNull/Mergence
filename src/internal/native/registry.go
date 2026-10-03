// registry.go 原生实现的注册表。
//
// 编排器只知道「这个渠道是原生型、kind 是 workbuddy」，不该知道 workbuddy 的
// 上游代码长什么样。反过来，各原生实现（native/workbuddy、将来的 native/zcode）
// 也不该反过来依赖编排器。两边靠这张表解耦：
//
//	原生实现包在 init() 里 Register(自己的 kind, Boot)
//	编排器在 Start 里 Lookup(kind) → 拿到 Boot 并装配
//
// 键是配置里的 `kind`，因此它必须**唯一且稳定**：换名字等于换配置值。
// 重复注册直接 panic 而不是覆盖——两个包抢同一个 kind 是编译期就该发现的错误，
// 而「跑起来只有后注册的那个生效」会让人对着源码想不通。
package native

import (
	"fmt"
	"sort"
	"sync"
)

var (
	regMu    sync.RWMutex
	builders = map[string]Boot{}
)

// Register 注册一个原生实现。kind 空或重复都会 panic（见包注释）。
//
// 由原生实现包的 init() 调用；显式在 main 里空白导入该包即完成装配，
// 不引入任何运行期反射或插件加载。
func Register(kind string, boot Boot) {
	if kind == "" {
		panic("native.Register：kind 不能为空")
	}
	if boot == nil {
		panic("native.Register：boot 不能为 nil（kind=" + kind + "）")
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := builders[kind]; dup {
		panic(fmt.Sprintf("native.Register：kind %q 被重复注册（两个原生实现抢同一个键）", kind))
	}
	builders[kind] = boot
}

// Lookup 按 kind 取原生实现。
func Lookup(kind string) (Boot, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	b, ok := builders[kind]
	return b, ok
}

// Kinds 已注册的 kind（排序，供日志与报错里列出可选值）。
//
// 「未注册的原生实现」这类报错必须能说出**有哪些可用**：用户手写
// `kind: "workbudy"`（拼错）时，只有把可选项列出来他才能一眼看出问题。
func Kinds() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(builders))
	for k := range builders {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
