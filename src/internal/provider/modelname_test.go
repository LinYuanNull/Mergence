// modelname_test.go 模型展示名规范化。
//
// 三类必须锁死的行为：
//  1. 规则本身（去 cn: 前缀、大写、Preview、-Free）
//  2. **幂等**：已规范的名称再跑一遍不能变（面板会反复渲染同一份数据）
//  3. **只改展示不改 ID**：规范化后的名字必须能反查回上游真实 ID
package provider

import (
	"testing"
	"time"

	"mergence/internal/config"
)

func TestNormalizeRules(t *testing.T) {
	cases := []struct{ in, want string }{
		// 1) 去开头 cn: 前缀与结尾 -cn
		{"cn:glm-5.3-flash", "GLM-5.3-Flash"},
		{"CN:kimi-k3-1", "Kimi-K3-1"},
		{"cn:hy3-x", "Hy3-Free"}, // -x 是免费档位
		{"glm-5.3-flash-cn", "GLM-5.3-Flash"},
		{"GLM-4.6-CN", "GLM-4.6"},
		// 2) 强制大写
		{"glm-4.6", "GLM-4.6"},
		{"hy3-x", "Hy3-Free"},
		{"hy4-preview-f", "Hy4-Preview-Free"},
		{"deepseek-v4-pro", "Deepseek-V4-Pro"},
		{"kimi-k3-1", "Kimi-K3-1"},
		{"space-bunny", "Space-Bunny"},
		{"qwen-flash", "qwen-Flash"},
		// 3) preview → Preview
		{"hy4-preview-f", "Hy4-Preview-Free"},
		{"model-preview", "model-Preview"},
		{"MODEL-PREVIEW-2026", "MODEL-Preview-2026"},
		// 4) 免费标识（:free / -free / -f 三种写法统一成 -Free）
		{"deepseek/deepseek-chat:free", "Deepseek/Deepseek-chat-Free"},
		{"gpt-4o-free", "gpt-4o-Free"},
		{"hy4-preview-f", "Hy4-Preview-Free"},
		{"model-f", "model-Free"},
		{"kimi-k2:free", "Kimi-K2-Free"}, // K+数字的 K 全部大写
		// Pro 的 P 大写
		{"deepseek-v4-pro", "Deepseek-V4-Pro"},
		{"qwen3-max-pro", "qwen3-max-Pro"},
		// 全大写输入统一成 "Pro"（与 Flash/Hy/Deepseek 同一形态，不是 "PRO"）
		{"PRO", "Pro"},
		// 组合：去 -cn + 免费
		{"glm-4.6-air-cn:free", "GLM-4.6-air-Free"},
		// 无规则命中时原样返回
		{"fake-alpha", "fake-alpha"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeModelName(c.in); got != c.want {
			t.Errorf("NormalizeModelName(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeIdempotent(t *testing.T) {
	// 幂等是硬要求：面板每次渲染都会重新规范化一遍，
	// 不幂等就会出现 "GLM-GLM" 或 "xxx-Free-Free"。
	inputs := []string{
		"cn:glm-5.3-flash", "glm-5.3-flash-cn", "deepseek-v4-pro", "hy4-preview-f",
		"kimi-k3-1", "space-bunny", "gpt-4o-free", "glm-4.6-air-cn:free",
		"cn:deepseek-v4-pro-cn:free", "fake-alpha", "hy3-x", "auto", "kimi-k2:free",
		"minimax-m3", "cn:minimax-m3",
	}
	for _, in := range inputs {
		once := NormalizeModelName(in)
		twice := NormalizeModelName(once)
		if once != twice {
			t.Errorf("不幂等：%q → %q → %q", in, once, twice)
		}
		if again := NormalizeModelName(twice); again != twice {
			t.Errorf("第三次仍变化：%q → %q", twice, again)
		}
	}
}

func TestNormalizeNoDoubleFreeSuffix(t *testing.T) {
	// 已经带 -Free 的名字（可能来自用户配置或上一轮规范化）不能再追加
	if got := NormalizeModelName("GPT-4o-Free"); got != "GPT-4o-Free" {
		t.Errorf("已带 -Free 的名称被重复追加：%q", got)
	}
	// 小写 -free 结尾也算带过
	if got := NormalizeModelName("model-free"); got != "model-Free" {
		t.Errorf("小写 -free 应规范成 -Free 而不是追加：%q", got)
	}
}

func TestNormalizeNoDoubleUppercase(t *testing.T) {
	// 已大写的词不应被再处理成别的大小写
	if got := NormalizeModelName("GLM-5.3-Flash"); got != "GLM-5.3-Flash" {
		t.Errorf("已规范的大写形式被改动：%q", got)
	}
	if got := NormalizeModelName("Deepseek-V4-Pro"); got != "Deepseek-V4-Pro" {
		t.Errorf("已规范的 Deepseek 被改动：%q", got)
	}
}

func TestNormalizeKeepsVendorSlash(t *testing.T) {
	// 带厂商前缀的模型名（OpenRouter 风格）不能破坏斜杠结构
	got := NormalizeModelName("deepseek/deepseek-chat:free")
	if got != "Deepseek/Deepseek-chat-Free" {
		t.Fatalf("斜杠结构被破坏：%q", got)
	}
}

func TestNormalizeAutoAndFreeTier(t *testing.T) {
	// Auto 的 A 大写
	for in, want := range map[string]string{
		"auto":    "Auto",
		"AUTO":    "Auto",
		"cn:auto": "Auto",
		// -x 结尾是免费档位 → 统一 -Free
		"hy3-x": "Hy3-Free",
		"hy3-X": "Hy3-Free",
		// K+数字的 K 全部大写（不只 K3）
		"kimi-k1":      "Kimi-K1",
		"kimi-k2:free": "Kimi-K2-Free",
		"kimi-k3-1":    "Kimi-K3-1",
	} {
		if got := NormalizeModelName(in); got != want {
			t.Errorf("NormalizeModelName(%q) = %q，期望 %q", in, got, want)
		}
	}
	// free 徽标要认得 -x 档位
	if !IsFreeModel("hy3-x") {
		t.Error("hy3-x 应被判定为免费模型（面板要打 Free 徽标）")
	}
	// auto 不含 free 语义，不能被标成免费
	if IsFreeModel("auto") {
		t.Error("auto 不应被标为免费模型")
	}
	// 幂等
	for _, in := range []string{"hy3-x", "auto", "kimi-k2:free"} {
		once := NormalizeModelName(in)
		if twice := NormalizeModelName(once); twice != once {
			t.Errorf("不幂等：%q → %q → %q", in, once, twice)
		}
	}
}

func TestNormalizeMiniMaxCasing(t *testing.T) {
	// `minimax-m3` 的三个 M 都要大写：品牌名里的两个 M + 版本号 `-m3` 的 M。
	//
	// 拆成两条规则（reMiniMax 管品牌名、reMNum 管版本号）是因为二者位置不同：
	// 只写一条匹配 `minimax-m\d` 的话，`minimax-m3-preview` 这类
	// 版本号不紧跟品牌名的形态就漏了。
	for in, want := range map[string]string{
		"minimax-m3":      "MiniMax-M3",
		"MiniMax-M3":      "MiniMax-M3",
		"MINIMAX-M3":      "MiniMax-M3",
		"cn:minimax-m3":   "MiniMax-M3",
		"minimax-m2":      "MiniMax-M2",
		"minimax-m1":      "MiniMax-M1",
		"minimax-text-01": "MiniMax-text-01", // 版本号不在此列，不硬凑大写
	} {
		if got := NormalizeModelName(in); got != want {
			t.Errorf("NormalizeModelName(%q) = %q，期望 %q", in, got, want)
		}
	}
	// 边界：`minimaxabab` 是另一个模型（\b 不成立），不该被改
	if got := NormalizeModelName("minimaxabab6.5s-chat"); got != "minimaxabab6.5s-chat" {
		t.Errorf("minimaxabab 系列不该被匹配：得到 %q", got)
	}
	// 幂等
	for _, in := range []string{"minimax-m3", "cn:minimax-m3", "MiniMax-M3"} {
		once := NormalizeModelName(in)
		if twice := NormalizeModelName(once); twice != once {
			t.Errorf("不幂等：%q → %q → %q", in, once, twice)
		}
	}
}

func TestNormalizeCNPrefix(t *testing.T) {
	// 开头 cn: 必须去掉，且不影响中间的冒号
	if got := NormalizeModelName("cn:deepseek-v4-pro"); got != "Deepseek-V4-Pro" {
		t.Errorf("cn: 前缀未去掉：%q", got)
	}
	if got := NormalizeModelName("cn:cn:cn:model"); got != "model" {
		t.Errorf("连续 cn: 前缀应一次去掉：%q", got)
	}
	// 中间的冒号不能动（这里只有 Deepseek 的 D 大写，冒号原样保留）
	if got := NormalizeModelName("deepseek/chat"); got != "Deepseek/chat" {
		t.Errorf("非开头冒号被误改：%q", got)
	}
	// 幂等：去完前缀再调不能继续变
	once := NormalizeModelName("cn:cn:model")
	if twice := NormalizeModelName(once); twice != once {
		t.Errorf("不幂等：%q → %q", once, twice)
	}
	// 剥空的情况保留原名，而不是返回空串（否则 /v1/models 会少一条）
	if got := NormalizeModelName("cn:"); got != "cn:" {
		t.Errorf("剥空时应保留原名，实际 %q", got)
	}
}

func TestNormalizeProOnlyAsWord(t *testing.T) {
	// Pro 只在独立成词时大写：不能把 prompt / proxy / prone 一起改掉
	for _, s := range []string{"prompt-model", "proxy-model", "prone-model", "prism"} {
		if got := NormalizeModelName(s); got != s {
			t.Errorf("词中的 pro 被误改：%q → %q", s, got)
		}
	}
	if got := NormalizeModelName("gpt-5.2-pro"); got != "gpt-5.2-Pro" {
		t.Errorf("独立成词的 pro 未大写：%q", got)
	}
	// 幂等：已大写的 Pro 不再变
	if got := NormalizeModelName("Deepseek-V4-Pro"); got != "Deepseek-V4-Pro" {
		t.Errorf("已规范的 Pro 被改动：%q", got)
	}
}

func TestNormalizeHyOnlyAtWordStart(t *testing.T) {
	// hy 只在词首且后接数字/连字符时大写，避免误伤其它词
	if got := NormalizeModelName("physical-model"); got != "physical-model" {
		t.Errorf("词中的 hy 被误改：%q", got)
	}
}

func TestIsFreeModel(t *testing.T) {
	free := []string{"gpt-4o:free", "model-free", "a/b:free", "x-free-y", "free-model"}
	for _, s := range free {
		if !IsFreeModel(s) {
			t.Errorf("%q 应判定为免费模型", s)
		}
	}
	notFree := []string{"gpt-4o", "deepseek-chat", "freedom", "preview"}
	for _, s := range notFree {
		if IsFreeModel(s) {
			t.Errorf("%q 不应判定为免费模型", s)
		}
	}
}

func TestEffectiveModelsKeepsRealID(t *testing.T) {
	// 自动拉取到的模型：对外是规范化后的展示名，但 ID 必须是上游原样。
	ch, err := newChannel(Upstream{
		Name: "t", DisplayName: "t", Source: SourceEmbedded, Enabled: true,
		BaseURL: "http://x", ModelPrefix: "p",
	}, testLogger(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	ch.modelCache = []string{"glm-5.3-flash-cn", "deepseek/deepseek-chat:free", "fake-alpha"}

	eff := ch.effectiveModels()
	if len(eff) != 3 {
		t.Fatalf("模型数 = %d，期望 3", len(eff))
	}
	byID := map[string]config.ChannelModel{}
	for _, m := range eff {
		byID[m.ID] = m
	}
	// 真实 ID 原样保留
	if _, ok := byID["glm-5.3-flash-cn"]; !ok {
		t.Fatal("真实 ID 被改写了，转发会 404")
	}
	// 展示名是规范化后的
	if got := byID["glm-5.3-flash-cn"].Outward(); got != "GLM-5.3-Flash" {
		t.Errorf("展示名 = %q，期望 GLM-5.3-Flash", got)
	}
	if got := byID["deepseek/deepseek-chat:free"].Outward(); got != "Deepseek/Deepseek-chat-Free" {
		t.Errorf("免费模型展示名 = %q", got)
	}
	// 无规则命中时 Alias 不设（避免配置噪声）
	if byID["fake-alpha"].Alias != "" {
		t.Errorf("未命中规则的模型不应设 Alias：%q", byID["fake-alpha"].Alias)
	}
}

func TestUpstreamForResolvesDisplayNameToRealID(t *testing.T) {
	// 关键回归：客户端拿 /v1/models 里看到的展示名调用，
	// 必须能用真实 ID 请求上游，否则 404。
	ch, err := newChannel(Upstream{
		Name: "t", DisplayName: "t", Source: SourceEmbedded, Enabled: true,
		BaseURL: "http://x", ModelPrefix: "p",
	}, testLogger(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	ch.modelCache = []string{"glm-5.3-flash-cn", "deepseek-v4-pro"}

	cases := map[string]string{
		"GLM-5.3-Flash":    "glm-5.3-flash-cn", // 展示名 → 真实 ID
		"Deepseek-V4-Pro":  "deepseek-v4-pro",  // 展示名 → 真实 ID
		"glm-5.3-flash-cn": "glm-5.3-flash-cn", // 真实 ID 原样
		"deepseek-v4-pro":  "deepseek-v4-pro",  // 真实 ID 原样
	}
	for short, want := range cases {
		got, ok := ch.upstreamFor(short)
		if !ok || got != want {
			t.Errorf("upstreamFor(%q) = %q(%v)，期望 %q", short, got, ok, want)
		}
		// 裸名匹配同样要能反查
		if got2, ok2 := ch.upstreamByDeclared(short); !ok2 || got2 != want {
			t.Errorf("upstreamByDeclared(%q) = %q(%v)，期望 %q", short, got2, ok2, want)
		}
	}
	// 未收录的名字仍然原样透传（上游上新模型的自由度不能被规范化堵死）：
	// ok=false 表示「没匹配到映射」，调用方按原样送出。
	if got, ok := ch.upstreamFor("brand-new-model"); ok || got != "brand-new-model" {
		t.Errorf("未收录模型应原样透传，实际 %q(%v)", got, ok)
	}
}

func TestFreeKind(t *testing.T) {
	// 三种免费必须能区分：混成一个「Free」标签会让人以为白天也免费
	cases := map[string]FreeKind{
		"hy4-preview-f":      FreeNightly, // -f 夜间（23:00–次日 08:00）
		"model-free":         FreeNightly,
		"hy3-x":              FreeLimited, // -x 限时
		"deepseek-chat:free": FreeOpen,    // :free 常规
		"gpt-4o-free":        FreeNightly, // -free 归夜间（与 -f 同族）
		"glm-5.3-flash":      FreeNone,
		"auto":               FreeNone,
	}
	for in, want := range cases {
		if got := FreeKindOf(in); got != want {
			t.Errorf("FreeKindOf(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestIsNightlyNow(t *testing.T) {
	// 跨零点的区间：23:00–24:00 与 00:00–08:00 两段都要为真
	day := func(h int) time.Time {
		return time.Date(2026, 10, 2, h, 0, 0, 0, time.Local)
	}
	for _, h := range []int{0, 3, 7, 23} {
		if !IsNightlyNow(day(h)) {
			t.Errorf("%02d:00 应在夜间免费时段内", h)
		}
	}
	for _, h := range []int{8, 12, 18, 22} {
		if IsNightlyNow(day(h)) {
			t.Errorf("%02d:00 不应在夜间免费时段内", h)
		}
	}
}

func TestDisplayNameFollowsRuleNotStaleAlias(t *testing.T) {
	// 存量 alias 是「上一次规则算出来的值」。规则改了，界面必须跟着变——
	// 否则同一个模型会因为在哪个渠道而显示成不同的名字。
	cases := []struct{ id, alias, want, why string }{
		{"cn:minimax-m3", "minimax-m3", "MiniMax-M3", "旧规则产物（只是 ID 的另一种写法）→ 重算"},
		{"cn:hy3-x", "hy3-x", "Hy3-Free", "同理：-x 现在算免费档"},
		{"cn:hy3-x", "Hy3-Free", "Hy3-Free", "已是最新，重算结果相同"},
		{"glm-4.6", "glm", "glm", "自定义短名 → 保留（不能抹掉用户的对外名）"},
		{"gpt-4o", "4o", "4o", "规则认不出 ID → alias 是唯一线索，保留"},
		{"fake-alpha", "", "fake-alpha", "无 alias 时回落到 ID"},
	}
	for _, c := range cases {
		got := displayModel(config.ChannelModel{ID: c.id, Alias: c.alias}).Outward()
		if got != c.want {
			t.Errorf("displayModel(%q, alias=%q) = %q，期望 %q（%s）",
				c.id, c.alias, got, c.want, c.why)
		}
		// 幂等：对结果再跑一遍不能变
		once := displayModel(config.ChannelModel{ID: c.id, Alias: c.alias})
		twice := displayModel(once)
		if twice.Outward() != once.Outward() {
			t.Errorf("displayModel 不幂等：%q → %q → %q",
				once.Outward(), twice.Outward(), once.Outward())
		}
	}
}

func TestSameNameVariant(t *testing.T) {
	// 判据的核心：只看「名字本身是否同一」，不看谁写的。
	same := [][2]string{
		{"minimax-m3", "cn:minimax-m3"},
		{"GLM-5.3-Flash", "cn:glm-5.3-flash"},
		{"cn:cn:auto", "auto"},
		{"Deepseek-V4-Pro", "cn:deepseek-v4-pro"},
	}
	for _, p := range same {
		if !sameNameVariant(p[0], p[1]) {
			t.Errorf("%q 与 %q 应判为同一名字的不同写法", p[0], p[1])
		}
	}
	diff := [][2]string{
		{"glm", "glm-4.6"},                       // 用户短名
		{"4o", "gpt-4o"},                         // 用户短名
		{"hy4-preview-free", "cn:hy4-preview-f"}, // -f 与 -Free 折叠后仍不同
	}
	for _, p := range diff {
		if sameNameVariant(p[0], p[1]) {
			t.Errorf("%q 与 %q 不该判为同一名字", p[0], p[1])
		}
	}
}
