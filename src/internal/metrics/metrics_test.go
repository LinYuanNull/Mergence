// metrics_test.go 计量与计价的回归测试。
//
// 重点覆盖三类容易出错的地方：
//  1. 口径归一（OpenAI 的 prompt_tokens 含缓存，Anthropic 分开给）
//  2. 「未知」不等于「零」——没价格时不能返回 $0.00
//  3. 缓存命中率的分母为 0 时不给数字
package metrics

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageNormalizeOpenAI(t *testing.T) {
	// OpenAI: prompt_tokens 含 cached 部分
	u := Usage{PromptTokens: 1000, CompletionTokens: 200, TotalTokens: 1200, CachedTokens: 400}
	n := u.Normalize()
	if n.Input != 600 {
		t.Errorf("未缓存输入 = %d，期望 600", n.Input)
	}
	if n.CachedInput != 400 {
		t.Errorf("缓存输入 = %d，期望 400", n.CachedInput)
	}
	if n.Output != 200 {
		t.Errorf("输出 = %d，期望 200", n.Output)
	}
	if n.Total != 1200 {
		t.Errorf("总量 = %d，期望 1200", n.Total)
	}
}

func TestUsageNormalizeAnthropic(t *testing.T) {
	// Anthropic: cache_read_input_tokens 与 input_tokens 分开给
	u := Usage{PromptTokens: 800, CompletionTokens: 100, CachedTokens: 300, CacheWriteTokens: 50}
	n := u.Normalize()
	if n.Input != 500 {
		t.Errorf("未缓存输入 = %d，期望 500（800-300）", n.Input)
	}
	if n.CacheWrite != 50 {
		t.Errorf("缓存写入 = %d，期望 50", n.CacheWrite)
	}
	if n.Total != 900 {
		t.Errorf("总量 = %d，期望 900（缺失时由 prompt+completion 推出）", n.Total)
	}
}

func TestUsageNormalizeOnlyTotal(t *testing.T) {
	// 只给 total_tokens 的上游：无法拆分，按总量减输出算输入
	u := Usage{TotalTokens: 1000, CompletionTokens: 300}
	n := u.Normalize()
	if n.Input != 700 {
		t.Errorf("输入 = %d，期望 700", n.Input)
	}
	if n.Output != 300 {
		t.Errorf("输出 = %d，期望 300", n.Output)
	}
}

func TestCacheHitRateUnknownWhenNoPrompt(t *testing.T) {
	var a Agg
	if _, ok := a.CacheHitRate(); ok {
		t.Error("没有输入 token 时不应给出命中率")
	}
	a.CachedTokens = 0
	a.PromptTotal = 1000
	rate, ok := a.CacheHitRate()
	if !ok {
		t.Fatal("有输入 token 时应给出命中率")
	}
	if rate != 0 {
		t.Errorf("命中率 = %v，期望 0", rate)
	}
}

func TestLookupExactPrefixFamily(t *testing.T) {
	p := NewPricer()
	if pr, ok, how := p.Lookup(Price{}, false, "gpt-4o"); !ok || how != "exact" || pr.InputPerM != 2.5 {
		t.Errorf("gpt-4o 精确匹配失败：ok=%v how=%s price=%+v", ok, how, pr)
	}
	// 带日期后缀应命中前缀
	if _, ok, how := p.Lookup(Price{}, false, "claude-sonnet-4-5-20250929"); !ok || how != "prefix" {
		t.Errorf("带后缀的 sonnet 应命中前缀：ok=%v how=%s", ok, how)
	}
	// 完全未收录 → 家族兜底
	if _, ok, how := p.Lookup(Price{}, false, "gpt-5-turbo-xl"); !ok || how != "family" {
		t.Errorf("未收录模型应命中家族：ok=%v how=%s", ok, how)
	}
	// 彻底无关的名字 → 明确未知
	if _, ok, _ := p.Lookup(Price{}, false, "my-own-tiny-model"); ok {
		t.Error("无关模型名不应命中任何价格条目")
	}
}

func TestLookupStripsVendorPrefix(t *testing.T) {
	p := NewPricer()
	// OpenRouter 风格的「openai/gpt-4o」应能命中
	if _, ok, _ := p.Lookup(Price{}, false, "openai/gpt-4o"); !ok {
		t.Error("带厂商前缀的模型名应能命中价格表")
	}
}

func TestUserPriceOverridesBuiltin(t *testing.T) {
	p := NewPricer()
	user := Price{InputPerM: 1, OutputPerM: 2}
	pr, ok, how := p.Lookup(user, true, "gpt-4o")
	if !ok || how != "user" || pr.InputPerM != 1 {
		t.Errorf("渠道自定义价应优先：ok=%v how=%s price=%+v", ok, how, pr)
	}
}

func TestCostSplitsCached(t *testing.T) {
	p := NewPricer()
	pr := Price{InputPerM: 10, CachedPerM: 1, OutputPerM: 20}
	c := p.Cost(pr, true, "user", Normalized{Input: 1e6, CachedInput: 1e6, Output: 1e6})
	if c.Input != 10 {
		t.Errorf("输入费 = %v，期望 10", c.Input)
	}
	if c.Cached != 1 {
		t.Errorf("缓存费 = %v，期望 1", c.Cached)
	}
	if c.Output != 20 {
		t.Errorf("输出费 = %v，期望 20", c.Output)
	}
	if c.Total != 31 {
		t.Errorf("总费 = %v，期望 31", c.Total)
	}
	if !c.Known || !c.Complete {
		t.Error("拿到价格时 Known/Complete 应为 true")
	}
}

func TestCostUnknownNotZero(t *testing.T) {
	p := NewPricer()
	c := p.Cost(Price{}, false, "", Normalized{Input: 1e6})
	if c.Known {
		t.Error("没价格时 Known 必须为 false")
	}
	if c.Total != 0 {
		t.Errorf("没价格时 Total 应为 0（由 Known=false 表示未知），实际 %v", c.Total)
	}
}

func TestAddCostPartialKnown(t *testing.T) {
	// 一个已知、一个未知 → 总额是「已知部分」，但 Complete 必须为 false，
	// 否则用户会以为那就是全部花费。
	a := Cost{Total: 5, Known: true, Complete: true}
	b := Cost{Total: 0, Known: false}
	s := addCost(a, b)
	if !s.Known {
		t.Error("部分已知时 Known 应为 true")
	}
	if s.Complete {
		t.Error("部分已知时 Complete 必须为 false")
	}
	if s.Total != 5 {
		t.Errorf("总额 = %v，期望 5", s.Total)
	}
}

func TestStoreQueryAggregates(t *testing.T) {
	s := NewStore("")
	now := time.Now()
	s.Record(Record{Time: now, Channel: "a", Source: "embedded", Model: "oai/gpt-4o",
		Usage: Usage{PromptTokens: 100, CompletionTokens: 10, CachedTokens: 20}, OK: true})
	s.Record(Record{Time: now, Channel: "a", Source: "embedded", Model: "oai/gpt-4o",
		Usage: Usage{PromptTokens: 200, CompletionTokens: 20, CachedTokens: 40}, OK: true})
	s.Record(Record{Time: now, Channel: "b", Source: "managed", Model: "wb/auto",
		Usage: Usage{PromptTokens: 50, CompletionTokens: 5}, OK: false})

	q := s.Query(7)
	if !q.HasData {
		t.Fatal("应有数据")
	}
	if q.Total.Requests != 2 || q.Total.Errors != 1 {
		t.Errorf("成功/失败 = %d/%d，期望 2/1", q.Total.Requests, q.Total.Errors)
	}
	if len(q.ByChannel) != 2 {
		t.Errorf("渠道数 = %d，期望 2", len(q.ByChannel))
	}
	if len(q.ByPair) != 2 {
		t.Errorf("渠道×模型桶数 = %d，期望 2", len(q.ByPair))
	}
	if q.Sources["b"] != "managed" {
		t.Errorf("渠道 b 的来源 = %q，期望 managed", q.Sources["b"])
	}
	rate, ok := q.Total.CacheHitRate()
	if !ok {
		t.Fatal("应能算出缓存命中率")
	}
	// 缓存 20+40 = 60；分母是各记录的 prompt 总量 100+200+50 = 350
	if rate < 0.17 || rate > 0.18 {
		t.Errorf("缓存命中率 = %v，期望约 0.171（60/350）", rate)
	}
}

func TestStoreQueryEmptyHasData(t *testing.T) {
	s := NewStore("")
	q := s.Query(7)
	if q.HasData {
		t.Error("空库不应报告有数据")
	}
}

func TestStoreQueryRangeFilter(t *testing.T) {
	s := NewStore("")
	old := time.Now().AddDate(0, 0, -30)
	s.Record(Record{Time: old, Channel: "a", Model: "m", OK: true, Usage: Usage{TotalTokens: 10}})
	all := s.Query(0)
	week := s.Query(7)
	if all.Total.Requests != 1 {
		t.Errorf("全部范围应有 1 条，实际 %d", all.Total.Requests)
	}
	if week.Total.Requests != 0 {
		t.Errorf("近 7 天应排除 30 天前的记录，实际 %d", week.Total.Requests)
	}
	if week.HasData {
		t.Error("窗口内无记录时 HasData 应为 false")
	}
}

func TestStorePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	s.Record(Record{Time: time.Now(), Channel: "a", Source: "embedded", Model: "oai/gpt-4o",
		Usage: Usage{PromptTokens: 1000, CompletionTokens: 100, CachedTokens: 200}, OK: true})
	if err := s.Close(); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "usage-"+time.Now().Format("2006-01-02")+".jsonl")); err != nil {
		t.Fatalf("未生成当日 JSONL：%v", err)
	}

	// 模拟重启：新 Store 从磁盘读回
	s2 := NewStore(dir)
	q := s2.Query(7)
	if !q.HasData {
		t.Fatal("重启后应读回历史")
	}
	if !q.Restored {
		t.Error("重启后读回的数据应标记 Restored")
	}
	if q.Total.Requests != 1 {
		t.Errorf("重启后调用数 = %d，期望 1", q.Total.Requests)
	}
}

func TestBuildReportCost(t *testing.T) {
	s := NewStore("")
	p := NewPricer()
	s.Record(Record{Time: time.Now(), Channel: "oai", Source: "embedded", Model: "oai/gpt-4o",
		Usage: Usage{PromptTokens: 1e6, CompletionTokens: 1e6, CachedTokens: 0}, OK: true})
	rep := s.Build(p, 7, nil, 10)
	if !rep.Total.Cost.Known {
		t.Fatal("gpt-4o 应能算出费用")
	}
	// 2.5 + 10 = 12.5
	if rep.Total.Cost.Total < 12.4 || rep.Total.Cost.Total > 12.6 {
		t.Errorf("总费用 = %v，期望约 12.5", rep.Total.Cost.Total)
	}
	if len(rep.ByChannel) != 1 || rep.ByChannel[0].Name != "oai" {
		t.Fatalf("按渠道聚合失败：%+v", rep.ByChannel)
	}
	if !rep.ByChannel[0].RateKnown {
		t.Error("有输入 token 时渠道应有缓存命中率")
	}
}

func TestBuildReportUnknownModelListed(t *testing.T) {
	s := NewStore("")
	p := NewPricer()
	s.Record(Record{Time: time.Now(), Channel: "mine", Source: "embedded",
		Model: "my-own-tiny-model", Usage: Usage{TotalTokens: 1000}, OK: true})
	rep := s.Build(p, 7, nil, 0)
	if len(rep.UnknownModels) != 1 || rep.UnknownModels[0] != "my-own-tiny-model" {
		t.Errorf("未收录模型应被列出：%v", rep.UnknownModels)
	}
	if rep.Total.Cost.Known {
		t.Error("全部模型无价格时总额不应为「已知」")
	}
}

func TestBuildReportRecent(t *testing.T) {
	s := NewStore("")
	p := NewPricer()
	for i := 0; i < 3; i++ {
		s.Record(Record{Time: time.Now(), Channel: "oai", Source: "embedded",
			Model: "oai/gpt-4o", Usage: Usage{TotalTokens: 100}, OK: i != 1, Status: 200})
	}
	rep := s.Build(p, 7, nil, 10)
	if len(rep.Recent) != 3 {
		t.Fatalf("最近调用数 = %d，期望 3", len(rep.Recent))
	}
	// 三条里应恰好有一条失败，且失败原因不影响其它两条被记录
	fails := 0
	for _, r := range rep.Recent {
		if !r.OK {
			fails++
		}
	}
	if fails != 1 {
		t.Errorf("失败记录数 = %d，期望 1", fails)
	}
	// 顺序：时间正序（首条不晚于末条）
	if rep.Recent[0].Time > rep.Recent[len(rep.Recent)-1].Time {
		t.Error("最近调用应按时间正序返回")
	}
}

func TestRecentRingOrder(t *testing.T) {
	s := NewStore("")
	base := time.Now()
	for i := 0; i < recentCap+10; i++ {
		s.Record(Record{Time: base.Add(time.Duration(i) * time.Second), Channel: "a", Model: "m", OK: true})
	}
	got := s.Recent(5)
	if len(got) != 5 {
		t.Fatalf("取 5 条得到 %d 条", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Time.Before(got[i-1].Time) {
			t.Error("Recent 应返回时间正序")
		}
	}
}

func TestBuildExcludesManagedFromDollarCost(t *testing.T) {
	// 积分型平台的花费以积分计（真实账面在上游），
	// 本地的 token×单价估算不能混进同一个美元总额 ——
	// 两套口径相加会得到一个毫无意义的数字。
	s := NewStore("")
	p := NewPricer()
	// embedded：gpt-4o，有内置价 → 应计入总额
	s.Record(Record{Time: time.Now(), Channel: "oai", Source: "embedded",
		Model: "oai/gpt-4o", Usage: Usage{PromptTokens: 1e6, CompletionTokens: 0}, OK: true})
	// managed：模型名也命中价格表（假设叫 gpt-4o），但必须被排除
	s.Record(Record{Time: time.Now(), Channel: "wb", Source: "managed",
		Model: "wb/gpt-4o", Usage: Usage{PromptTokens: 1e6, CompletionTokens: 0}, OK: true})

	rep := s.Build(p, 7, nil, 0)
	// 只有 embedded 的 2.5（1M 输入）
	if rep.Total.Cost.Total < 2.49 || rep.Total.Cost.Total > 2.51 {
		t.Errorf("总额应只含 embedded 渠道的费用 2.5，实际 %v", rep.Total.Cost.Total)
	}
	// managed 渠道自己的报告里费用必须是「未知」，而不是一个估算值
	for _, c := range rep.ByChannel {
		if c.Name == "wb" {
			if c.Cost.Known {
				t.Errorf("managed 渠道不应有美元费用估算：%+v", c.Cost)
			}
		}
	}
	// managed 的模型不能出现在「价格未知」清单里 —— 那个提示只对
	// API 型平台有意义（积分型平台本来就不按美元计）。
	for _, m := range rep.UnknownModels {
		if m == "wb/gpt-4o" {
			t.Error("managed 渠道的模型不应出现在价格未知清单里")
		}
	}
	// 但 token 统计必须包含 managed（调用次数与 token 是同一口径）
	if rep.Total.Requests != 2 {
		t.Errorf("调用次数应含 managed 渠道，实际 %d", rep.Total.Requests)
	}
}
