package probe

// Part A4: recovery-within-one-interval evidence.
// A deployment that fails its health check leaves the routable set; after the
// upstream recovers, exactly one further health-check sweep (one interval)
// makes it routable again with no restart and no config reload.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func TestFailoverEvidence_RecoveredRoutableWithinOneInterval(t *testing.T) {
	var healthy atomic.Bool // upstream starts down
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"mock unavailable"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"probe","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Routing.Strategy = "ready_queue" // only Healthy deployments are routable
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Probe.IntervalSeconds = 60 // the health-check interval under test
	cfg.Probe.MaxTokens = 1
	cfg.Probe.CapabilityProbes = false
	cfg.Probe.TimeoutMS = 3000
	cfg.Providers = []config.ProviderConfig{{
		ID: "mock", Name: "Mock", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "model-m", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}}},
	}}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	e := New(cfg, reg, rt, hm, events.New(16))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	before := rt.All()
	sweep1 := e.RunOnce(ctx) // upstream down: deployment must leave the routable set
	if sweep1.Total != 1 || sweep1.Failed != 1 {
		t.Fatalf("down sweep should fail the single deployment: %+v", sweep1)
	}
	if got := rt.Candidates(router.Requirement{Model: "auto", Streaming: true}); len(got) != 0 {
		t.Fatalf("failed deployment still routable: %#v", got)
	}

	healthy.Store(true) // upstream recovers; no restart, no reload follows
	start := time.Now()
	sweep2 := e.RunOnce(ctx) // exactly one more health-check interval
	elapsed := time.Since(start)
	if sweep2.Total != 1 || sweep2.Passed != 1 {
		t.Fatalf("recovery sweep should pass the single deployment: %+v", sweep2)
	}
	if elapsed >= cfg.ProbeInterval() {
		t.Fatalf("recovery took %s, exceeding one health-check interval %s", elapsed, cfg.ProbeInterval())
	}
	got := rt.Candidates(router.Requirement{Model: "auto", Streaming: true})
	if len(got) != 1 || got[0].Deployment.ID != "mock/m" {
		t.Fatalf("recovered deployment not routable after one interval: %#v", got)
	}
	if st := hm.Get("mock/m"); st.Status != health.Healthy {
		t.Fatalf("recovered deployment health=%s want healthy", st.Status)
	}
	after := rt.All()
	if len(before) != len(after) || before[0].ID != after[0].ID {
		t.Fatalf("topology changed without reload: before=%v after=%v", before, after)
	}
}
