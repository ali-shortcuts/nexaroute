package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
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

func TestManualRunOnceWorksWhenBackgroundProbesDisabled(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Probe.OnStart = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	e := New(cfg, reg, rt, hm, events.New(20))

	res := e.RunOnce(context.Background())
	if res.Total != 1 || res.Passed != 1 || res.Failed != 0 {
		t.Fatalf("manual probe should run while background probes are disabled: %+v", res)
	}
	if calls.Load() != 1 {
		t.Fatalf("manual probe calls=%d want 1", calls.Load())
	}
}

func TestRunOnceReadyQueueAuthFailureQuarantinesDeployment(t *testing.T) {
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
	if st := hm.Get("p/m"); st.Status != health.Degraded {
		t.Fatalf("status=%s want degraded quarantine", st.Status)
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

func TestBackgroundSweepSkipsHealthyReadyModels(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Probe.Concurrency = 4
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{
			{ID: "ready", Model: "ready-model", Enabled: true, Weight: 1},
			{ID: "new", Model: "new-model", Enabled: true, Weight: 1},
		},
	}}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	hm.RecordSuccess("p/ready", 10*time.Millisecond)
	before := hm.Get("p/ready").LastChecked

	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	e := New(cfg, reg, rt, hm, events.New(20))
	res := e.runOnce(context.Background(), false)

	if res.Total != 2 || res.Passed != 1 || res.SkippedReady != 1 {
		t.Fatalf("unexpected background result: %+v", res)
	}
	if calls.Load() != 1 {
		t.Fatalf("background supervisor re-probed healthy ready model; calls=%d want 1", calls.Load())
	}
	after := hm.Get("p/ready").LastChecked
	if !after.Equal(before) {
		t.Fatalf("healthy ready model was touched by supervisor: before=%v after=%v", before, after)
	}
}

func TestReadyLeaseExpiry(t *testing.T) {
	now := time.Now()
	lease := 5 * time.Minute

	fresh := health.State{Status: health.Healthy, LastChecked: now.Add(-30 * time.Second)}
	if readyLeaseExpired(fresh, now, lease) {
		t.Fatal("fresh health proof must not be re-probed")
	}

	stale := health.State{Status: health.Healthy, LastChecked: now.Add(-6 * time.Minute)}
	if !readyLeaseExpired(stale, now, lease) {
		t.Fatal("idle stale health proof must be revalidated")
	}

	missingTimestamp := health.State{Status: health.Healthy}
	if !readyLeaseExpired(missingTimestamp, now, lease) {
		t.Fatal("missing health timestamp must be treated as expired")
	}
}

func TestBackgroundSweepAtScaleTouchesOnlyUnverifiedModels(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Probe.Concurrency = 16
	cfg.Probe.TimeoutMS = 2000
	for p := 0; p < 24; p++ {
		pc := config.ProviderConfig{
			ID: fmt.Sprintf("p%02d", p), Name: fmt.Sprintf("P%02d", p),
			Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none",
			Enabled: true, MaxConcurrency: 32,
		}
		for m := 0; m < 5; m++ {
			pc.Models = append(pc.Models, config.ModelConfig{
				ID: fmt.Sprintf("m%d", m), Model: fmt.Sprintf("model-%02d-%d", p, m),
				Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true},
			})
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
	all := rt.All()
	if len(all) != 120 {
		t.Fatalf("deployments=%d want 120", len(all))
	}
	for i := 0; i < 100; i++ {
		hm.RecordSuccess(all[i].ID, time.Millisecond)
	}

	e := New(cfg, reg, rt, hm, events.New(500))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res := e.runOnce(ctx, false)

	if res.Total != 120 || res.Passed != 20 || res.SkippedReady != 100 || res.Failed != 0 {
		t.Fatalf("unexpected selective sweep result: %+v", res)
	}
	if got := calls.Load(); got != 20 {
		t.Fatalf("background sweep sent %d upstream probes; want exactly 20 unknown models", got)
	}
	for i := 0; i < 120; i++ {
		if st := hm.Get(all[i].ID); st.Status != health.Healthy {
			t.Fatalf("deployment %s status=%s want healthy", all[i].ID, st.Status)
		}
	}
}

func TestLegacyAdaptiveBackgroundSweepStillReprobesHealthyModels(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Routing.Strategy = "adaptive"
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	hm.RecordSuccess("p/m", time.Millisecond)
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	e := New(cfg, reg, rt, hm, events.New(20))
	res := e.runOnce(context.Background(), false)

	if res.Passed != 1 || res.SkippedReady != 0 || calls.Load() != 1 {
		t.Fatalf("legacy adaptive health semantics changed: result=%+v calls=%d", res, calls.Load())
	}
}

func TestRecoverFloodQueuesWithoutPerDeploymentGoroutines(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://example.invalid",
		AuthMode: "none", Enabled: true, MaxConcurrency: 32,
	}}
	for i := 0; i < 500; i++ {
		cfg.Providers[0].Models = append(cfg.Providers[0].Models, config.ModelConfig{
			ID: fmt.Sprintf("m%d", i), Model: fmt.Sprintf("model-%d", i), Enabled: true, Weight: 1,
		})
	}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	e := New(cfg, reg, rt, hm, events.New(20))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.setRunContext(ctx)

	before := runtime.NumGoroutine()
	for _, d := range rt.All() {
		hm.ForceCooldown(d.ID, "test", time.Minute)
		e.Recover(d.ID)
	}
	time.Sleep(20 * time.Millisecond)
	after := runtime.NumGoroutine()
	if delta := after - before; delta > 10 {
		t.Fatalf("Recover created per-deployment goroutines: delta=%d", delta)
	}
	if got := len(e.recoveryQueue); got != 500 {
		t.Fatalf("queued recovery tasks=%d want 500", got)
	}
	if got := len(e.recovering); got != 500 {
		t.Fatalf("tracked recoveries=%d want 500", got)
	}
}

func TestRecoveryWorkersRetryAndReturnModelToHealthy(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"temporary"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Probe.RecoveryAttempts = 5
	cfg.Probe.RecoveryRetryMS = 5
	cfg.Probe.TimeoutMS = 1000
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true, MaxConcurrency: 8,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	e := New(cfg, reg, rt, hm, events.New(50))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Start(ctx)

	hm.Quarantine("p/m", "initial", time.Millisecond)
	e.Recover("p/m")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hm.Get("p/m").Status == health.Healthy && calls.Load() >= 3 {
			if e.isRecovering("p/m") {
				t.Fatal("recovery marker remained after success")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("model did not recover: calls=%d state=%+v", calls.Load(), hm.Get("p/m"))
}
