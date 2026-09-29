package router

// Part A3: proportional routing evidence.
// Deployments with worse latency / higher error rate score lower and receive
// proportionally fewer primary requests (healthy-fast serves everything;
// degraded-slow serves only failover traffic, never primaries).

import (
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

func TestFailoverEvidence_ProportionalRouting_FewerForWorse(t *testing.T) {
	cfg := config.Default()
	cfg.Routing.Strategy = "adaptive"
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid", Enabled: true, Models: []config.ModelConfig{
		{ID: "good", Model: "good", Aliases: []string{"coding"}, Enabled: true, Weight: 1},
		{ID: "mid", Model: "mid", Aliases: []string{"coding"}, Enabled: true, Weight: 1},
		{ID: "bad", Model: "bad", Aliases: []string{"coding"}, Enabled: true, Weight: 1},
	}}}
	h := health.New(100, time.Hour) // high threshold: failures degrade score without cooldown exile
	for i := 0; i < 5; i++ {
		h.RecordSuccess("p/good", 10*time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		h.RecordFailure("p/mid", "timeout", 300*time.Millisecond)
	}
	for i := 0; i < 5; i++ {
		h.RecordSuccess("p/mid", 300*time.Millisecond)
	}
	for i := 0; i < 5; i++ {
		h.RecordFailure("p/bad", "timeout", 1500*time.Millisecond)
	}
	for i := 0; i < 2; i++ {
		h.RecordSuccess("p/bad", 1500*time.Millisecond)
	}
	for _, id := range []string{"p/good", "p/mid", "p/bad"} {
		if st := h.Get(id); st.Status != health.Healthy {
			t.Fatalf("%s should be healthy (last observation success), got %s", id, st.Status)
		}
	}
	if r := h.Get("p/good").EWMAFailureRate; r != 0 {
		t.Fatalf("good failure rate=%v want 0", r)
	}
	if h.Get("p/mid").EWMAFailureRate >= h.Get("p/bad").EWMAFailureRate {
		t.Fatalf("failure EWMA not graded: mid=%v bad=%v",
			h.Get("p/mid").EWMAFailureRate, h.Get("p/bad").EWMAFailureRate)
	}

	r := New(cfg, h)
	first := r.Candidates(Requirement{Model: "coding"})
	if len(first) != 3 {
		t.Fatalf("want 3 candidates, got %d", len(first))
	}
	for i := 0; i+1 < len(first); i++ {
		if first[i].Score <= first[i+1].Score {
			t.Fatalf("scores not quality-ordered: %#v", first)
		}
	}
	if first[0].Deployment.ID != "p/good" || first[2].Deployment.ID != "p/bad" {
		t.Fatalf("quality order wrong: %#v", first)
	}

	counts := map[string]int{}
	const n = 200
	for i := 0; i < n; i++ {
		got := r.Candidates(Requirement{Model: "coding"})
		counts[got[0].Deployment.ID]++
	}
	if counts["p/good"] != n {
		t.Fatalf("best deployment should win every primary: %v", counts)
	}
	if counts["p/bad"] != 0 || counts["p/mid"] != 0 {
		t.Fatalf("worse deployments must receive proportionally fewer (here zero) primaries: %v", counts)
	}
	// Worse deployments stay eligible as failover depth, not exiled.
	if len(first) != 3 {
		t.Fatalf("worse deployments must remain failover-eligible: %#v", first)
	}
	t.Logf("primary distribution over %d requests: %v", n, counts)
}
