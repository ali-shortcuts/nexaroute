package probe

// Issue #77 (parent #49) v0.12.0 A5 contract: health probes permanently
// enforce MaxTokens=1.
//
// Every health-check request must carry a one-token budget on the wire, and
// no config value, provider metadata, or future caller change may raise it.
// The test inspects the captured outbound payload at the adapter boundary
// (httptest server receiving exactly what the adapter sends) for both the
// OpenAI-compatible and Anthropic-compatible probe shapes, rejects any wire
// value other than 1, and never depends on unrelated routing settings
// (adapter layers use no router; the engine layer runs under multiple
// routing strategies asserting only the wire budget).

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

// wireTokenBudget extracts the token-budget field from a captured probe
// payload. OpenAI-compatible and Anthropic-compatible health probes both use
// the "max_tokens" key.
func wireTokenBudget(t *testing.T, body map[string]any) float64 {
	t.Helper()
	v, ok := body["max_tokens"]
	if !ok {
		t.Fatalf("health-check payload missing max_tokens: %#v", body)
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		t.Fatalf("health-check max_tokens has non-numeric type %T (%v)", v, v)
		return -1
	}
}

func TestHealthProbesPermanentlyEnforceMaxTokensOne(t *testing.T) {
	// Layer 1: configuration can never raise the budget.
	t.Run("config_rejects_raised_budget", func(t *testing.T) {
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
	})

	// Layer 2: adapter boundary pins the budget for every dialect, no matter
	// what budget the caller passes (config, provider metadata, or future
	// code cannot raise it).
	t.Run("adapter_boundary_pins_budget", func(t *testing.T) {
		cases := []struct {
			name         string
			providerType string
			successBody  string
		}{
			{
				name:         "openai_compatible",
				providerType: "openai_compatible",
				successBody:  `{"id":"probe","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
			},
			{
				name:         "anthropic_compatible",
				providerType: "anthropic_compatible",
				successBody:  `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"model":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				var observed atomic.Value
				observed.Store(float64(-1))
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("invalid probe JSON: %v", err)
						return
					}
					observed.Store(wireTokenBudget(t, body))
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(tc.successBody))
				}))
				defer up.Close()

				cfg := config.Default()
				cfg.Providers = []config.ProviderConfig{{
					ID: "probe-under-test", Name: "Probe", Type: tc.providerType,
					BaseURL: up.URL, AuthMode: "none", Enabled: true,
					Models: []config.ModelConfig{{ID: "m", Model: "probe-model", Enabled: true, Weight: 1}},
				}}
				reg, err := providers.NewRegistry(cfg)
				if err != nil {
					t.Fatal(err)
				}
				a, ok := reg.Get("probe-under-test")
				if !ok {
					t.Fatal("adapter missing for probe-under-test")
				}
				// Attempt to raise the budget through the caller argument,
				// the exact vector config/provider-metadata/future changes
				// would use. Every value must still land as 1 on the wire.
				for _, raised := range []int{0, 2, 16, 64} {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					_, status, err := a.Probe(ctx, "probe-model", raised)
					cancel()
					if err != nil || status != http.StatusOK {
						t.Fatalf("probe with caller budget %d failed: status=%d err=%v", raised, status, err)
					}
					if got := observed.Load().(float64); got != 1 {
						t.Fatalf("%s probe with caller budget %d sent max_tokens=%v; must always be 1", tc.name, raised, got)
					}
				}
			})
		}
	})

	// Layer 3: the sweep engine emits max_tokens=1 even when handed a
	// tampered config claiming a larger budget (validation bypassed on
	// purpose). Runs under unrelated routing strategies to prove the
	// contract does not depend on routing settings.
	t.Run("engine_ignores_tampered_config", func(t *testing.T) {
		for _, strategy := range []string{"ready_queue", "adaptive"} {
			t.Run("strategy_"+strategy, func(t *testing.T) {
				var observed atomic.Value
				observed.Store(float64(-1))
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("invalid probe JSON: %v", err)
						return
					}
					observed.Store(wireTokenBudget(t, body))
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"probe","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
				}))
				defer up.Close()

				cfg := config.Default()
				cfg.Routing.Strategy = strategy
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
				// NOTE: ApplyDefaults/Validate intentionally skipped to
				// simulate a tampered config bypassing validation.
				hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
				reg, err := providers.NewRegistry(cfg)
				if err != nil {
					t.Fatal(err)
				}
				rt := router.New(cfg, hm)
				e := New(cfg, reg, rt, hm, events.New(16))
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				res := e.RunOnce(ctx)
				if res.Passed != 1 {
					t.Fatalf("tampered-budget sweep did not pass under %s: %+v", strategy, res)
				}
				if got := observed.Load().(float64); got != 1 {
					t.Fatalf("health check sent max_tokens=%v under %s; budget must always be 1", got, strategy)
				}
			})
		}
	})
}
