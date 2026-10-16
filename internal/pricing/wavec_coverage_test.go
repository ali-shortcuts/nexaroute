package pricing

import (
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Wave C-6: behavior-first coverage for the versioned price book.
// Invariants: costs never negative, estimate ≥ any realizable cost
// at headline rates, cache-rate defaults, version shadowing rules.

func ptr(f float64) *float64 { return &f }

func TestEstimateConservativeAdmission(t *testing.T) {
	e := Entry{InputPerMTok: 2.0, OutputPerMTok: 8.0}
	if got := Estimate(e, 1000, 500); got != (1000*2.0+500*8.0)/1_000_000 {
		t.Fatalf("estimate=%v", got)
	}
	// Negative token counts clamp to zero: an estimate is never negative.
	if got := Estimate(e, -5, -7); got != 0 {
		t.Fatalf("negative estimate=%v", got)
	}
	// The estimate is conservative: for any realization within the
	// admission envelope (actual input <= estimate, actual output <=
	// allowance), the estimate must cover the actual cost at the same
	// headline rates.
	actual := Cost(e, 800, 400, 200, 0, true)
	est := Estimate(e, 1000, 500)
	if est < actual {
		t.Fatalf("estimate %v < realizable cost %v", est, actual)
	}
}

func TestCostDialectConventions(t *testing.T) {
	e := Entry{InputPerMTok: 10.0, OutputPerMTok: 20.0, CacheReadPerMTok: ptr(1.0), CacheWritePerMTok: ptr(5.0)}
	// OpenAI/Gemini convention: input already includes cache reads —
	// never double-charge the cached share at the input rate.
	withCache := Cost(e, 1000, 0, 400, 0, true)
	withoutCache := Cost(e, 1000, 0, 400, 0, false)
	want := (600*10.0 + 400*1.0) / 1_000_000
	if withCache != want {
		t.Fatalf("inputIncludesCache=true: %v want %v", withCache, want)
	}
	// Anthropic convention: input excludes cache tokens; additive.
	want2 := (1000*10.0 + 400*1.0) / 1_000_000
	if withoutCache != want2 {
		t.Fatalf("inputIncludesCache=false: %v want %v", withoutCache, want2)
	}
	// Cache-write tokens are always additive.
	withWrite := Cost(e, 1000, 0, 0, 100, true)
	if withWrite != (1000*10.0+100*5.0)/1_000_000 {
		t.Fatalf("cache write: %v", withWrite)
	}
}

func TestCostCacheRateDefaults(t *testing.T) {
	// Nil cache rates default to the input rate (documented rule).
	e := Entry{InputPerMTok: 10.0, OutputPerMTok: 20.0}
	if got := e.CacheRead(); got != 10.0 {
		t.Fatalf("CacheRead default=%v", got)
	}
	if got := e.CacheWrite(); got != 10.0 {
		t.Fatalf("CacheWrite default=%v", got)
	}
	if got := Cost(e, 1000, 0, 500, 0, true); got != (500*10.0+500*10.0)/1_000_000 {
		t.Fatalf("nil cache rates: %v", got)
	}
}

func TestCostNeverNegative(t *testing.T) {
	e := Entry{InputPerMTok: 10.0, OutputPerMTok: 20.0, CacheReadPerMTok: ptr(1.0)}
	// Negative usage reports clamp to zero; hostile token counts can
	// never produce a negative (credit) cost.
	if got := Cost(e, -100, -50, -30, -10, true); got != 0 {
		t.Fatalf("negative usage: %v", got)
	}
	// More cache-read than input clamps the billable input at zero.
	if got := Cost(e, 100, 0, 500, 0, true); got != (500*1.0)/1_000_000 {
		t.Fatalf("over-cache: %v", got)
	}
}

func TestResolveVersionShadowing(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	v1 := Version{Name: "v1", EffectiveFrom: t0, Prices: map[string]Entry{
		"p/m": {InputPerMTok: 1.0, OutputPerMTok: 2.0},
	}}
	v2 := Version{Name: "v2", EffectiveFrom: t0.Add(24 * time.Hour), Prices: map[string]Entry{
		"p/other": {InputPerMTok: 9.0, OutputPerMTok: 9.0},
	}}
	b := &Book{}
	// Build via FromConfig to exercise the constructor path too.
	cfg := config.Config{}
	b = FromConfig(cfg)
	if _, _, ok := b.Resolve("p/m", t0); ok {
		t.Fatal("empty book resolved")
	}
	b2 := &Book{versions: []Version{v1, v2}}
	// A later version that does not mention the deployment does NOT
	// shadow the earlier version's entry for it.
	e, name, ok := b2.Resolve("p/m", t0.Add(48*time.Hour))
	if !ok || name != "v1" || e.InputPerMTok != 1.0 {
		t.Fatalf("shadowing: %v %q %v", e, name, ok)
	}
	// A version effective in the future does not apply.
	if _, _, ok := b2.Resolve("p/other", t0.Add(time.Hour)); ok {
		t.Fatal("future version applied")
	}
	// Nil book is safe.
	var nilBook *Book
	if _, _, ok := nilBook.Resolve("p/m", t0); ok {
		t.Fatal("nil book resolved")
	}
}

func TestResolveForFallbackChain(t *testing.T) {
	at := time.Now()
	empty := FromConfig(config.Config{})
	cfg := config.Default()
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		Models: []config.ModelConfig{{ID: "m", Model: "m", InputCostPerMTok: 3.0, OutputCostPerMTok: 6.0}},
	}}
	// Model-config prices are the fallback (version "config").
	e, v := empty.ResolveFor(cfg, "p/m", at)
	if v != "config" || e.InputPerMTok != 3.0 || e.OutputPerMTok != 6.0 {
		t.Fatalf("config fallback: %v %q", e, v)
	}
	// Unknown deployments are never priced (version "unpriced").
	e, v = empty.ResolveFor(cfg, "p/ghost", at)
	if v != "unpriced" || e.InputPerMTok != 0 {
		t.Fatalf("unpriced: %v %q", e, v)
	}
	// The book wins over the config fallback.
	cfg2 := config.Config{}
	cfg2.Pricing.Versions = []config.PriceVersionConfig{{
		Version: "booked", EffectiveFrom: at.Add(-time.Hour).UTC().Format(time.RFC3339),
		Prices: map[string]config.PriceEntryConfig{"p/m": {InputPerMTok: 7.0, OutputPerMTok: 7.0}},
	}}
	book := FromConfig(cfg2)
	e, v = book.ResolveFor(cfg, "p/m", at)
	if v != "booked" || e.InputPerMTok != 7.0 {
		t.Fatalf("book priority: %v %q", e, v)
	}
	// Unparseable timestamps are skipped defensively, never served.
	cfg2.Pricing.Versions = append(cfg2.Pricing.Versions, config.PriceVersionConfig{
		Version: "bad", EffectiveFrom: "not-a-time",
		Prices: map[string]config.PriceEntryConfig{"p/m": {InputPerMTok: 100.0}},
	})
	book = FromConfig(cfg2)
	if e, v, ok := book.Resolve("p/m", at); !ok || v != "booked" || e.InputPerMTok != 7.0 {
		t.Fatalf("bad timestamp not skipped: %v %q %v", e, v, ok)
	}
	// Unsorted input versions are ordered by effective time.
	cfg3 := config.Config{}
	later := at.Add(-time.Minute).UTC().Format(time.RFC3339)
	earlier := at.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	cfg3.Pricing.Versions = []config.PriceVersionConfig{
		{Version: "newer", EffectiveFrom: later, Prices: map[string]config.PriceEntryConfig{"p/m": {InputPerMTok: 5.0}}},
		{Version: "older", EffectiveFrom: earlier, Prices: map[string]config.PriceEntryConfig{"p/m": {InputPerMTok: 1.0}}},
	}
	book3 := FromConfig(cfg3)
	if _, v, ok := book3.Resolve("p/m", at); !ok || v != "newer" {
		t.Fatalf("ordering: %q %v", v, ok)
	}
}
