package router

import (
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

// M5-3 quality-ordering invariant: candidate order follows MEASURED
// quality — a deployment with a high EWMA failure rate and latency
// is deprioritized behind an identical healthy one by the router's
// scoring, not by config weight alone. (Under ready strategies the
// degraded deployment would be excluded outright — M4-8 semantics —
// so ordering is observed under a non-ready strategy, where score
// is what decides.)
func TestQualityOrderingFollowsMeasuredHealth(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "priority"
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://x", Enabled: true, Models: []config.ModelConfig{
		{ID: "a", Model: "a", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
		{ID: "b", Model: "b", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}},
	}}}
	h := health.New(100, time.Hour) // high threshold: ordering, not exclusion
	r := New(cfg, h)

	// Both healthy and fast: either order is acceptable, scores close.
	h.RecordSuccess("p/a", 20*time.Millisecond)
	h.RecordSuccess("p/b", 20*time.Millisecond)
	base := r.Candidates(Requirement{Model: "auto", Streaming: true})
	if len(base) != 2 {
		t.Fatalf("baseline candidates=%v", base)
	}

	// A turns slow and flaky (below the breaker threshold).
	for i := 0; i < 10; i++ {
		h.RecordFailure("p/a", "boom", 900*time.Millisecond)
		h.RecordSuccess("p/b", 20*time.Millisecond)
	}
	got := r.Candidates(Requirement{Model: "auto", Streaming: true})
	if len(got) != 2 {
		t.Fatalf("candidates=%v", got)
	}
	if got[0].Deployment.ID != "p/b" {
		t.Fatalf("measured quality ignored: order=%s,%s scores=%.1f,%.1f",
			got[0].Deployment.ID, got[1].Deployment.ID, got[0].Score, got[1].Score)
	}
	if !(got[0].Score > got[1].Score) {
		t.Fatalf("score does not reflect quality: %.1f vs %.1f", got[0].Score, got[1].Score)
	}
}
