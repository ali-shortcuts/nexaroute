package router

import (
	"math"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

// #421 item 1: the decision trace must quantify the cache
// economics of a session pin, not just name it. With a priced
// cache-read discount on the pinned deployment and one observed
// turn, Explain reports turns, the last cache-read token count,
// and benefit = cacheReadTokens × (input − cacheRead) / 1e6.
// Without evidence — or without a priced discount — it reports
// no figure rather than inventing one.
func TestExplainQuantifiesPinCacheBenefit(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.SessionAffinity = true
	cfg.Routing.SessionTTLSeconds = 3600
	cacheRead := 0.30
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://x", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}}}}}
	cfg.Pricing.Versions = []config.PriceVersionConfig{{
		Version: "v1", EffectiveFrom: "2020-01-01T00:00:00Z",
		Prices: map[string]config.PriceEntryConfig{"p/m": {InputPerMTok: 3.00, OutputPerMTok: 15.00, CacheReadPerMTok: &cacheRead}},
	}}
	h := health.New(4, time.Hour)
	h.RecordSuccess("p/m", time.Millisecond)
	r := New(cfg, h)
	req := Requirement{Model: "m", SessionKey: "sess-1", Streaming: true}

	// No pin yet: the trace stays silent on cache economics.
	exp := r.Explain(req)
	if exp.PinnedDeployment != "" || exp.PinnedSessionTurns != 0 || exp.PinnedCacheBenefitUSD != 0 {
		t.Fatalf("unpinned explain invented cache economics: %+v", exp)
	}

	// One served turn with observed cache evidence.
	r.ObserveSession(req, "p/m")
	r.ObserveSessionCache(req, "p/m", 10000, 8000)

	exp = r.Explain(req)
	if exp.PinnedDeployment != "p/m" {
		t.Fatalf("pinned = %q, want p/m", exp.PinnedDeployment)
	}
	if exp.PinnedSessionTurns != 1 {
		t.Fatalf("turns = %d, want 1", exp.PinnedSessionTurns)
	}
	if exp.PinnedLastCacheReadTokens != 8000 {
		t.Fatalf("cache read tokens = %d, want 8000", exp.PinnedLastCacheReadTokens)
	}
	want := 8000 * (3.00 - 0.30) / 1e6 // 0.0216 USD
	if math.Abs(exp.PinnedCacheBenefitUSD-want) > 1e-12 {
		t.Fatalf("benefit = %v, want %v", exp.PinnedCacheBenefitUSD, want)
	}

	// A second turn refreshes the record; a zero cache
	// observation must not erase the measured warm prefix.
	r.ObserveSession(req, "p/m")
	r.ObserveSessionCache(req, "p/m", 12000, 0)
	exp = r.Explain(req)
	if exp.PinnedSessionTurns != 2 || exp.PinnedLastCacheReadTokens != 8000 {
		t.Fatalf("after turn 2: turns=%d cacheRead=%d, want 2/8000", exp.PinnedSessionTurns, exp.PinnedLastCacheReadTokens)
	}

	// Cache evidence for a DIFFERENT deployment is ignored.
	r.ObserveSessionCache(req, "p/other", 5000, 5000)
	exp = r.Explain(req)
	if exp.PinnedLastCacheReadTokens != 8000 {
		t.Fatalf("foreign deployment evidence leaked into the pin record: %d", exp.PinnedLastCacheReadTokens)
	}
}
