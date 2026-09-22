package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/universal-llm-gateway/internal/config"
	"github.com/ali-shortcuts/universal-llm-gateway/internal/events"
	"github.com/ali-shortcuts/universal-llm-gateway/internal/health"
	"github.com/ali-shortcuts/universal-llm-gateway/internal/providers"
	"github.com/ali-shortcuts/universal-llm-gateway/internal/router"
)

func TestRunOnceTwentyProvidersHundredModels(t *testing.T) {
	var calls atomic.Int32
	var active atomic.Int32
	var maxActive atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		n := active.Add(1)
		for {
			m := maxActive.Load()
			if n <= m || maxActive.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(3 * time.Millisecond)
		active.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Probe.Concurrency = 16
	cfg.Probe.TimeoutMS = 2000
	for p := 0; p < 20; p++ {
		pc := config.ProviderConfig{ID: fmt.Sprintf("p%02d", p), Name: fmt.Sprintf("P%02d", p), Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, MaxConcurrency: 32}
		for m := 0; m < 5; m++ {
			pc.Models = append(pc.Models, config.ModelConfig{ID: fmt.Sprintf("m%d", m), Model: fmt.Sprintf("model-%02d-%d", p, m), Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}})
		}
		cfg.Providers = append(cfg.Providers, pc)
	}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	bus := events.New(500)
	e := New(cfg, reg, rt, hm, bus)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res := e.RunOnce(ctx)
	if res.Total != 100 || res.Passed != 100 || res.Failed != 0 {
		t.Fatalf("unexpected probe result: %+v", res)
	}
	if got := int(calls.Load()); got != 100 {
		t.Fatalf("upstream calls=%d want 100", got)
	}
	if got := maxActive.Load(); got < 2 || got > int32(cfg.Probe.Concurrency) {
		t.Fatalf("max active=%d want 2..%d", got, cfg.Probe.Concurrency)
	}
}

func TestRunOnceAuthFailureImmediatelyCoolsDeployment(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer up.Close()
	cfg := config.Default()
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Providers = []config.ProviderConfig{{ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}}}}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, _ := providers.NewRegistry(cfg)
	rt := router.New(cfg, hm)
	e := New(cfg, reg, rt, hm, events.New(10))
	res := e.RunOnce(context.Background())
	if res.Failed != 1 {
		t.Fatalf("result=%+v", res)
	}
	if st := hm.Get("p/m"); st.Status != health.Cooldown {
		t.Fatalf("status=%s want cooldown", st.Status)
	}
}

func TestRunOncePrioritizesUnknownBeforeHealthyWhenConcurrencyOne(t *testing.T) {
	var mu sync.Mutex
	order := []string{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		order = append(order, body.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Probe.Concurrency = 1
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{
			{ID: "healthy", Model: "healthy-model", Enabled: true, Weight: 1},
			{ID: "unknown", Model: "unknown-model", Enabled: true, Weight: 1},
		},
	}}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	hm.RecordSuccess("p/healthy", 10*time.Millisecond)
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	e := New(cfg, reg, rt, hm, events.New(20))
	res := e.RunOnce(context.Background())
	if res.Passed != 2 {
		t.Fatalf("result=%+v", res)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "unknown-model" {
		t.Fatalf("probe priority order=%v, want unknown first", order)
	}
}
