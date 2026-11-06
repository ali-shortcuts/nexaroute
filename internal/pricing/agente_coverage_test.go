package pricing

import (
	"math"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Agent E coverage: the same-effective-from tie-break in FromConfig's
// version sort (pricing.go:81). Two versions effective at the same
// instant sort by name ascending, so the later name wins resolution.
func TestAgentEVersionTieBreakByName(t *testing.T) {
	cfg := bookConfig(
		config.PriceVersionConfig{Version: "v-b", EffectiveFrom: "2026-06-01T00:00:00Z", Prices: map[string]config.PriceEntryConfig{
			"p/m": {InputPerMTok: 5, OutputPerMTok: 6},
		}},
		config.PriceVersionConfig{Version: "v-a", EffectiveFrom: "2026-06-01T00:00:00Z", Prices: map[string]config.PriceEntryConfig{
			"p/m": {InputPerMTok: 1, OutputPerMTok: 2},
		}},
	)
	book := FromConfig(cfg)
	e, v, ok := book.Resolve("p/m", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	if !ok || v != "v-b" || e.InputPerMTok != 5 || e.OutputPerMTok != 6 {
		t.Fatalf("tie-break resolution wrong: ok=%v version=%q entry=%+v", ok, v, e)
	}
}

// Agent E coverage: Cost's final non-finite guard (pricing.go:180-181).
// Rates are sanitized up front, so the only way the computed cost can
// go non-finite is float64 overflow from huge token counts at a large
// (but finite, valid) rate; that must price as zero, never +Inf.
func TestAgentECostOverflowReturnsZero(t *testing.T) {
	e := Entry{InputPerMTok: 1e290, OutputPerMTok: 1e290}
	if got := Cost(e, math.MaxInt64, math.MaxInt64, 0, 0, false); got != 0 {
		t.Fatalf("overflowing cost = %v, want 0", got)
	}
	// Same guard via the cache-read leg: a huge cache rate (valid until
	// the multiplication) overflows the sum.
	e2 := Entry{InputPerMTok: 1, OutputPerMTok: 1, CacheReadPerMTok: rate(1e290)}
	if got := Cost(e2, 0, 0, math.MaxInt64, 0, false); got != 0 {
		t.Fatalf("overflowing cache-read cost = %v, want 0", got)
	}
	// A negative cache rate is sanitized to "not priced separately", so
	// the input rate applies instead of the negative rate.
	e3 := Entry{InputPerMTok: 10, OutputPerMTok: 30, CacheReadPerMTok: rate(-1)}
	if got := Cost(e3, 0, 0, 1_000_000, 0, false); got != 10 {
		t.Fatalf("negative cache rate cost = %v, want 10 (input-rate fallback)", got)
	}
}

// Agent E coverage: Estimate's final non-finite guard (pricing.go:203-204).
// Same overflow shape as Cost: a finite rate times MaxInt64 tokens
// overflows float64, and the admission estimate must be zero, never +Inf.
func TestAgentEEstimateOverflowReturnsZero(t *testing.T) {
	e := Entry{InputPerMTok: 1e290, OutputPerMTok: 1e290}
	if got := Estimate(e, math.MaxInt64, math.MaxInt64); got != 0 {
		t.Fatalf("overflowing estimate = %v, want 0", got)
	}
	// Sanity: ordinary estimates are unaffected.
	if got := Estimate(Entry{InputPerMTok: 10, OutputPerMTok: 30}, 1000, 500); got != (1000*10+500*30)/1e6 {
		t.Fatalf("ordinary estimate = %v, want %v", got, (1000*10+500*30)/1e6)
	}
}
