// modelname.go 模型展示名规范化。
//
// 为什么需要：上游给的模型 ID 是给机器用的（`glm-5.3-flash-cn`、
// `hy4-preview-f`、`deepseek/deepseek-chat:free`），直接摆到界面上既难读
// 也不一致——同一个模型在不同渠道可能一个带 `-cn` 一个不带、大小写混杂。
//
// 铁律：**只改展示名，绝不改真实 ID**。
//
//	真实 ID   上游只认它。转发时必须原样送上去，改一个字符就 404。
//	展示名    给人看的：/v1/models 的对外 id、面板里的模型列表。
//
// 二者的桥梁是 config.ChannelModel：ID 存真实值，Alias 存展示名。
// 路由反查（upstreamFor / upstreamByDeclared）走 effectiveModels，
// 因此「客户端拿展示名调用 → 仍然用真实 ID 请求上游」是成立的。
//
// 所有规则都必须**幂等**：对已规范的名称再跑一遍不能有变化。
// 面板会反复用同一份数据渲染，重复追加 "-Free" 或重复大写都会暴露出来。
package provider

import (
	"regexp"
	"strings"
	"time"
)

var (
	// 开头的区域前缀：cn:glm-5.3-flash → glm-5.3-flash
	//
	// 用 `(?:cn:)+` 而不是 `cn:`：前者一次吃掉连续重复的前缀
	// （`cn:cn:x` → `x`），后者要跑两次才干净，幂等性就破了。
	// 只锚定开头 —— `deepseek/chat:free` 这种中间的冒号不能动。
	reCNPrefix = regexp.MustCompile(`(?i)^(?:cn:)+`)
	// 结尾的 -cn（区域标记）：glm-5.3-flash-cn → glm-5.3-flash
	reSuffixCN = regexp.MustCompile(`(?i)-cn$`)
	// 结尾的免费标记，四种写法都认：
	//   deepseek-chat:free   OpenRouter 风格
	//   gpt-4o-free          通用后缀
	//   hy4-preview-f        上游用单字母 f 表示 free 档
	//   hy3-x                同体系里 x 档位同样是免费档
	// 写成一条正则吃掉四种，剥离后统一在末尾挂 `-Free`，
	// 不会出现 `-free-Free` 这种叠加。
	reFreeSuffix = regexp.MustCompile(`(?i)(?:[:\-_]f(?:ree)?|[-]x)$`)
	// preview → Preview（统一写法与大小写）
	rePreview = regexp.MustCompile(`(?i)preview`)

	// 强制大写词。
	//
	// 注意：全部用 RE2 支持的写法（捕获组 + \b），不用 (?=) 这类前瞻断言——
	// Go 的 regexp 会直接 panic。
	reGLM      = regexp.MustCompile(`(?i)glm`)                // GLM 全部大写
	reFlash    = regexp.MustCompile(`(?i)flash`)              // Flash 首字母大写
	reHy       = regexp.MustCompile(`(?i)\bhy([-0-9])`)       // Hy 的 H 大写（限 hy3 / hy- 这类）
	reVersionV = regexp.MustCompile(`(?i)(^|[^a-z0-9])v(\d)`) // 版本号里的 v → V
	reKimi     = regexp.MustCompile(`(?i)\bkimi`)             // Kimi 的 K 大写
	reKNum     = regexp.MustCompile(`(?i)\bk(\d)\b`)          // K1/K2/K3… 的 K 大写（不止 K3）
	reMiniMax  = regexp.MustCompile(`(?i)\bminimax\b`)        // MiniMax 的 M 大写
	reMNum     = regexp.MustCompile(`(?i)\bm(\d)\b`)          // M1/M2/M3… 的 M 大写（MiniMax 的版本号）
	reSpace    = regexp.MustCompile(`(?i)\bspace`)            // Space 的 S 大写
	reBunny    = regexp.MustCompile(`(?i)\bbunny`)            // Bunny 的 B 大写
	reDeepseek = regexp.MustCompile(`(?i)\bdeepseek`)         // Deepseek 的 D 大写
	rePro      = regexp.MustCompile(`(?i)\bpro\b`)            // Pro 的 P 大写（\b 防止误伤 prompt/proxy）
	reAuto     = regexp.MustCompile(`(?i)\bauto\b`)           // Auto 的 A 大写（自动路由档位）
	// 免费档位标记：结尾的 -x（hy3-x 这类）= 限时免费。
	// 规范化时由 reFreeSuffix 连分隔符一起剥。
	reFreeTier = regexp.MustCompile(`(?i)[-]x$`)
	// 夜间免费标记：结尾的 -f / -free（hy4-preview-f）。
	// 与 -x 分开是因为两者时段不同：夜间仅 23:00–次日 08:00。
	reNightly = regexp.MustCompile(`(?i)(?:[-]f|[-]free)$`)
	// 词中间的 free（xxx-free-yyy），用分隔符界定避免误伤 "freedom"
	reFreeWord = regexp.MustCompile(`(?i)(^|[-_:])free([-_:]|$)`)
)

// freeSuffix 追加到免费模型名末尾的标识。
const freeSuffix = "-Free"

// NormalizeModelName 把上游模型 ID 规范化成展示名。
//
// 规则顺序（后一条依赖前一条的结果）：
//  1. 去掉开头的区域前缀 cn:
//  2. 识别并剥离原有的 free 标记（:free / -free）
//  3. 去掉结尾的 -cn
//  4. preview → Preview
//  5. 指定词强制大写（GLM/Flash/Hy/版本v/Kimi/K+数字/MiniMax/M+数字/Space/
//     Bunny/Deepseek/Pro/Auto）
//  6. 免费模型统一追加 -Free
//
// 顺序要点：
//   - free 必须**先**剥离。否则 `glm-4.6-air-cn:free` 的结尾是
//     `:free` 而不是 `-cn`，第 3 步就匹配不到 `-cn`，区域后缀会留在名字里。
//   - cn: 前缀放在最前：它不影响其余规则，先去前缀能让后面的
//     大写规则看到更干净的短名，行为更可预期。
//
// 幂等：对已规范化的名称再调用，结果不变。
func NormalizeModelName(id string) string {
	s := strings.TrimSpace(id)
	if s == "" {
		return s
	}

	// 1) 开头的区域前缀。剥完若空（模型名就是 "cn:"），保留原样——
	//    返回空串会让 /v1/models 少一个条目，比显示一个怪名字更糟。
	if v := reCNPrefix.ReplaceAllString(s, ""); v != "" {
		s = v
	}

	// 2) 免费标记：先记下是否免费，再把原标记剥掉——
	//    留着 ":free" 又追加 "-Free" 会变成 "chat:free-Free"。
	isFree := IsFreeModel(s)
	s = reFreeSuffix.ReplaceAllString(s, "")

	// 3) 区域后缀
	s = reSuffixCN.ReplaceAllString(s, "")

	// 4) preview → Preview（统一写法；幂等：已是 Preview 再替换仍是 Preview）
	s = rePreview.ReplaceAllString(s, "Preview")

	// 5) 强制大写。reVersionV 需保留前导分隔符，故单独 ReplaceAllString。
	s = reGLM.ReplaceAllString(s, "GLM")
	s = reFlash.ReplaceAllString(s, "Flash")
	s = reHy.ReplaceAllString(s, "Hy${1}")
	s = reVersionV.ReplaceAllString(s, "${1}V${2}")
	s = reKimi.ReplaceAllString(s, "Kimi")
	s = reKNum.ReplaceAllString(s, "K${1}")
	// MiniMax 的 M 分两处：品牌名里的 M，以及版本号 `-m3` 的 M。
	// `minimax-m3` 三个 M 都要大写 → `MiniMax-M3`。
	s = reMiniMax.ReplaceAllString(s, "MiniMax")
	s = reMNum.ReplaceAllString(s, "M${1}")
	s = reSpace.ReplaceAllString(s, "Space")
	s = reBunny.ReplaceAllString(s, "Bunny")
	s = reDeepseek.ReplaceAllString(s, "Deepseek")
	s = rePro.ReplaceAllString(s, "Pro")
	s = reAuto.ReplaceAllString(s, "Auto")

	// 6) 免费标识。已经带了就绝不再追加（幂等）。
	if isFree && !strings.HasSuffix(s, freeSuffix) {
		s += freeSuffix
	}
	return s
}

// FreeKind 免费模型的类型。
//
// 同是「免费」，含义完全不同：`-f` 只在夜间时段免费（上游限定
// 每晚 23:00 到次日 8:00），`-x` 是限时活动。混成一个 Free 标签，
// 用户会以为白天也一样免费，踩坑后才发现。
type FreeKind string

const (
	FreeNone    FreeKind = ""        // 不免费
	FreeNightly FreeKind = "nightly" // -f：夜间免费（23:00–次日 08:00）
	FreeLimited FreeKind = "limited" // -x：限时免费
	FreeOpen    FreeKind = "open"    // :free / -free：常规免费
)

// 夜间免费时段（本地时间）。上游随时可能改，这里只做展示提示。
const (
	NightlyStartHour = 23
	NightlyEndHour   = 8
)

// FreeKindOf 判断模型 ID 的免费类型。
func FreeKindOf(id string) FreeKind {
	s := strings.TrimSpace(id)
	switch {
	case reFreeTier.MatchString(s): // 结尾 -x
		return FreeLimited
	case reNightly.MatchString(s): // 结尾 -f / -free
		return FreeNightly
	case reFreeSuffix.MatchString(s), reFreeWord.MatchString(s):
		return FreeOpen
	}
	return FreeNone
}

// IsNightlyNow 判断当前是否在夜间免费时段内（23:00–次日 08:00）。
//
// 跨零点的区间要小心：23:00–24:00 与 00:00–08:00 是两段，
// 写成 `h >= 23 && h < 8` 永远为假。
func IsNightlyNow(now time.Time) bool {
	h := now.Hour()
	return h >= NightlyStartHour || h < NightlyEndHour
}

// IsFreeModel 判断模型 ID 是否是免费模型（不区分类型）。
func IsFreeModel(id string) bool { return FreeKindOf(id) != FreeNone }

// nameSeparators 折叠名字时要去掉的分隔符。
var nameSeparators = strings.NewReplacer("-", "", "_", "", ".", "", ":", "", "/", "")

// sameNameVariant 判断两个名字是否只是「同一个名字的不同写法」。
//
// 忽略的差异：大小写、分隔符（- _ . : /）、开头的区域前缀（cn:）。
//
// 用途是区分 alias 的来源：
//
//	`minimax-m3` ↔ `cn:minimax-m3`   同一名字 → 系统按旧规则自动写入的
//	`glm`        ↔ `glm-4.6`         不 同 → 用户自定义的对外短名
//
// 前者该随当前规则更新，后者必须原样保留。判据只看名字本身，
// 不需要在配置里存「这条 alias 是谁写的」——多一个字段就多一处会失配的状态。
func sameNameVariant(a, b string) bool {
	return foldName(a) == foldName(b)
}

// foldName 把名字折叠成只含字母数字的形态，用于比较。
func foldName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = reCNPrefix.ReplaceAllString(s, "")
	return nameSeparators.Replace(s)
}
