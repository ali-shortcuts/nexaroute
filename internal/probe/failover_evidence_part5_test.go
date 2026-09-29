package probe

// Part A5: fixed one-token health-check budget evidence.
// Health checks must always use MaxTokens:1; this test proves the budget can
// never be raised: config validation rejects anything above 1, and the sweep
// engine sends max_tokens=1 on the wire even when handed a tampered config
// that bypassed validation.

import (
	"context"
	"encoding/json"
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

func TestFailoverEvidence_HealthChecksAlwaysUseOneToken(t *testing.T) {
	// Layer 1: configuration can never raise the budget.
	for _, raised := range []int{2, 16, 64} {
		cfg := config.Default()
		cfg.Probe.MaxTokens = raised
		if err := cfg.Validate(); err == nil {
			t.Fatalf("probe.max_tokens=%d was accepted; budget must be pinned to 1", raised)
		}
	}
	fresh := config.Default()
	fresh.ApplyDefaults()
	if fresh.Probe.MaxTokens != 1 {
		t.Fatalf("default probe budget=%d want 1", fresh.Probe.MaxTokens)
	}

	// Layer 2: the sweep engine emits max_tokens=1 even with a tampered
	// config that claims a larger budget (validation bypassed on purpose).
	var observed atomic.Value
	observed.Store(float64(0))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("invalid probe JSON: %v", err)
		}
		if v, ok := body["max_tokens"].(float64); ok {
			observed.Store(v)
		} else {
			observed.Store(float64(-1))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"probe","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Routing.Strategy = "ready_queue"
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Probe.Concurrency = 4
	cfg.Probe.MaxTokens = 64 // tampered: must still emit 1 on the wire
	cfg.Probe.CapabilityProbes = false
	cfg.Probe.TimeoutMS = 3000
	cfg.Providers = []config.ProviderConfig{{
		ID: "mock", Name: "Mock", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "model-m", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true}}},
	}}
	// NOTE: ApplyDefaults/Validate intentionally skipped to simulate tampering.
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	e := New(cfg, reg, rt, hm, events.New(16))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if res := e.RunOnce(ctx); res.Passed != 1 {
		t.Fatalf("tampered-budget sweep did not pass: %+v", res)
	}
	if got := observed.Load().(float64); got != 1 {
		t.Fatalf("health check sent max_tokens=%v; budget must always be 1", got)
	}
}
