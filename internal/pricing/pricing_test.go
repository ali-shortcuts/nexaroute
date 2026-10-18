package pricing

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// M5-6 pricing properties: randomized invariants over version
// resolution and cost.

func bookConfig(versions ...config.PriceVersionConfig) config.Config {
	cfg := config.Config{}
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Models: []config.ModelConfig{{ID: "m", InputCostPerMTok: 3, OutputCostPerMTok: 15}},
	}}
	cfg.Pricing.Versions = versions
	return cfg
}

func rate(f float64) *float64 { return &f }

func TestPricingResolveVersioning(t *testing.T) {
	cfg := bookConfig(
		config.PriceVersionConfig{Version: "v1", EffectiveFrom: "2026-01-01T00:00:00Z", Prices: map[string]config.PriceEntryConfig{
			"p/m": {InputPerMTok: 1, OutputPerMTok: 2},
		}},
		config.PriceVersionConfig{Version: "v2", EffectiveFrom: "2026-06-01T00:00:00Z", Prices: map[string]config.PriceEntryConfig{
			"p/m": {InputPerMTok: 5, OutputPerMTok: 6, CacheReadPerMTok: rate(0.5)},
		}},
	)
	book := FromConfig(cfg)
	// Before any version: book misses, fallback is config prices.
	if _, _, ok := book.Resolve("p/m", time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)); ok {
		t.Fatal("resolved before the first version took effect")
	}
	if e, v := book.ResolveFor(cfg, "p/m", time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)); v != "config" || e.InputPerMTok != 3 {
		t.Fatalf("fallback wrong: %v %+v", v, e)
	}
	// Between versions: v1.
	if e, v, ok := book.Resolve("p/m", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)); !ok || v != "v1" || e.InputPerMTok != 1 {
		t.Fatalf("v1 window wrong: %v %v %+v", ok, v, e)
	}
	// After v2: v2 with the explicit cache rate.
	if e, v, ok := book.Resolve("p/m", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)); !ok || v != "v2" || e.CacheRead() != 0.5 {
		t.Fatalf("v2 window wrong: %v %v %+v", ok, v, e)
	}
	// Cache rates default to the input rate when unset.
	if e, _, _ := book.Resolve("p/m", time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)); e.CacheRead() != 1 || e.CacheWrite() != 1 {
		t.Fatalf("cache default rule wrong: %+v", e)
	}
}

// Cost is non-negative and monotonic non-decreasing in every token
// count, for both cache conventions, over randomized inputs.
func TestPricingCostProperties(t *testing.T) {
	rng := rand.New(rand.NewSource(20261015))
	for i := 0; i < 2000; i++ {
		e := Entry{
			InputPerMTok:  rng.Float64() * 100,
			OutputPerMTok: rng.Float64() * 100,
		}
		if rng.Intn(2) == 0 {
			e.CacheReadPerMTok = rate(rng.Float64() * 100)
		}
		if rng.Intn(2) == 0 {
			e.CacheWritePerMTok = rate(rng.Float64() * 100)
		}
		in := int64(rng.Intn(1_000_000))
		out := int64(rng.Intn(100_000))
		cr := int64(rng.Intn(1_000_000))
		cw := int64(rng.Intn(100_000))
		for _, includes := range []bool{false, true} {
			base := Cost(e, in, out, cr, cw, includes)
			if base < 0 || math.IsNaN(base) {
				t.Fatalf("negative/NaN cost: %v (%+v)", base, e)
			}
			if c := Cost(e, in+997, out, cr, cw, includes); c < base {
				t.Fatalf("cost decreased with more input: %v < %v", c, base)
			}
			if c := Cost(e, in, out+997, cr, cw, includes); c < base {
				t.Fatalf("cost decreased with more output: %v < %v", c, base)
			}
			// Cache reads: under the EXCLUSIVE convention they are
			// additive, so cost grows. Under the INCLUSIVE
			// convention a larger cached share of the SAME input
			// legitimately costs LESS (the cache rate is the
			// discount) — the monotonic axis there is adding a
			// cached token to the total input.
			if includes {
				if c := Cost(e, in+997, out, cr+997, cw, includes); c < base {
					t.Fatalf("inclusive cost decreased when adding cached input: %v < %v", c, base)
				}
			} else {
				if c := Cost(e, in, out, cr+997, cw, includes); c < base {
					t.Fatalf("cost decreased with more cache reads: %v < %v", c, base)
				}
			}
			if c := Cost(e, in, out, cr, cw+997, includes); c < base {
				t.Fatalf("cost decreased with more cache writes: %v < %v", c, base)
			}
		}
	}
}

// The inclusive convention never double-charges cached tokens:
// pricing 100 input of which 100 are cached equals pricing 100
// cached reads alone.
func TestPricingInclusiveConventionNoDoubleCharge(t *testing.T) {
	e := Entry{InputPerMTok: 10, OutputPerMTok: 30, CacheReadPerMTok: rate(1)}
	inclusive := Cost(e, 100, 0, 100, 0, true)
	if math.Abs(inclusive-0.0001) > 1e-12 {
		t.Fatalf("inclusive cost = %v, want 0.0001 (100 cache reads @ $1/M)", inclusive)
	}
	// Exclusive (Anthropic): input excludes cache, both priced.
	exclusive := Cost(e, 100, 0, 100, 0, false)
	want := (100*10 + 100*1) / 1e6
	if math.Abs(exclusive-want) > 1e-12 {
		t.Fatalf("exclusive cost = %v, want %v", exclusive, want)
	}
}

func TestPricingRejectsNonFiniteAndNegativeRates(t *testing.T) {
	e := Entry{InputPerMTok: math.NaN(), OutputPerMTok: math.Inf(1), CacheReadPerMTok: func() *float64 { v := -1.0; return &v }(), CacheWritePerMTok: func() *float64 { v := math.NaN(); return &v }()}
	if got := Cost(e, 1000, 1000, 1000, 1000, true); got != 0 {
		t.Fatalf("non-finite cost = %v, want 0", got)
	}
	if got := Estimate(e, 1000, 1000); got != 0 {
		t.Fatalf("non-finite estimate = %v, want 0", got)
	}
	// Negative token counts are already sanitized; cost stays finite.
	e2 := Entry{InputPerMTok: 10, OutputPerMTok: 30}
	if got := Cost(e2, -5, -5, -5, -5, false); got != 0 {
		t.Fatalf("negative tokens cost = %v, want 0", got)
	}
}
