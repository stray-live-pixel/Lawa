package reviewstore

import (
	"math"
	"testing"
)

func ptr(n int64) *int64 { return &n }
func TestUsagePricingCacheNotDoubleCounted(t *testing.T) {
	p := Pricing{Input: 2, CachedInput: .5, Output: 8}
	u := PriceUsage(Usage{InputTokens: ptr(1000000), CachedInputTokens: ptr(400000), OutputTokens: ptr(100000)}, &p)
	if u.CostUSD == nil || math.Abs(*u.CostUSD-2.2) > 1e-9 || !u.Complete {
		t.Fatalf("стоимость = %#v", u)
	}
	for _, v := range []Usage{{InputTokens: ptr(100), OutputTokens: ptr(30)}, {InputTokens: ptr(10), CachedInputTokens: ptr(11), OutputTokens: ptr(0)}} {
		got := PriceUsage(v, &p)
		if got.CostUSD != nil || got.Complete {
			t.Fatal("неизвестные/неверные данные выданы за измеренные")
		}
	}
	got := PriceUsage(u, nil)
	if got.CostUSD != nil {
		t.Fatal("неизвестный тариф выдан за бесплатный")
	}
}
func TestEvidenceReferencesValidated(t *testing.T) {
	s, r := fixture(t)
	_, e := s.Update(r.ID, func(v *Review) error {
		v.Findings = []Finding{{ID: "f1", Priority: "P1", Title: "Ошибка", Anchors: []Anchor{{FileID: "missing", StartLine: 3, EndLine: 4}}}}
		return nil
	})
	if e == nil {
		t.Fatal("доказательство ссылается в пустоту")
	}
	_, e = s.Update(r.ID, func(v *Review) error {
		v.Context.ProjectFiles = []File{{ID: "file", Path: "src/login.tsx", SnapshotPath: "artifacts/login.tsx"}}
		v.Findings = []Finding{{ID: "f1", Priority: "P1", Title: "Ошибка", Anchors: []Anchor{{FileID: "file", StartLine: 3, EndLine: 4}}}}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
}
func TestMergeConfigPreservesUnspecifiedStages(t *testing.T) {
	base := DefaultConfig()
	base.Prices = map[string]Pricing{"a": {Input: 1}}
	merged := MergeConfig(base, Config{Review: AgentConfig{Effort: "xhigh"}, Prices: map[string]Pricing{"b": {Input: 2}}})
	if merged.Review.Model != base.Review.Model || merged.Review.Effort != "xhigh" || merged.Context != base.Context || len(merged.Prices) != 2 || len(base.Prices) != 1 {
		t.Fatalf("неверное наложение: %#v", merged)
	}
}

// Встроенные цены — воспроизводимая оценка, не фактическое списание подписки.
func TestDefaultPricingSnapshot(t *testing.T) {
	c := ResolveConfig(Config{})
	for model, rates := range map[string][3]float64{"gpt-6-astra": {10, 1, 50}, "gpt-6-sol": {2, .2, 10}, "gpt-6-luna": {.1, .01, .5}} {
		p, ok := c.Prices[model]
		if !ok || p.Input != rates[0] || p.CachedInput != rates[1] || p.Output != rates[2] || p.Source == "" || p.AsOf != "2026-09-26" || p.Basis == "" {
			t.Fatalf("неверный снимок тарифа %s: %#v", model, p)
		}
	}
	custom := ResolveConfig(Config{Prices: map[string]Pricing{"custom": {Input: 3}, "gpt-6-sol": {Input: 9}}})
	if custom.Prices["gpt-6-sol"].Input != 9 || custom.Prices["custom"].Input != 3 || custom.Prices["gpt-6-astra"].Input != 10 {
		t.Fatal("частичное переопределение потеряло цены")
	}
	if _, ok := c.Prices["unknown-model"]; ok {
		t.Fatal("выдуман неизвестный тариф")
	}
}
