package probe

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func requireProbeSoak(t *testing.T) {
	t.Helper()
	if os.Getenv("NEXAROUTE_SOAK") != "1" {
		t.Skip("set NEXAROUTE_SOAK=1 to run long-form soak checks")
	}
}

func TestSoakRepeatedRecoveryCyclesReleaseState(t *testing.T) {
	requireProbeSoak(t)

	var calls atomic.Int64
	var failedCalls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// Fail every third call. Recovery re-admission requires 3 successful
		// probes within a 5-probe cycle (ProbeRecoverySuccessThreshold), so
		// the failure density must leave at least 3 successes in any 5
		// consecutive calls; failing 2 of 3 made the threshold unreachable
		// by construction. One failure per cycle still exercises the
		// recovery failure/progress accounting.
		if n%3 == 0 {
			failedCalls.Add(1)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`)
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Probe.RecoveryAttempts = 5
	cfg.Probe.RecoveryRetryMS = 1
	cfg.Probe.TimeoutMS = 1000
	cfg.Probe.Concurrency = 4
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
	e := New(cfg, reg, rt, hm, events.New(128))
	ctx, cancel := context.WithCancel(context.Background())
	e.Start(ctx)
	time.Sleep(10 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	const cycles = 50
	for cycle := 0; cycle < cycles; cycle++ {
		hm.Quarantine("p/m", "soak", time.Millisecond)
		e.Recover("p/m")
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if hm.Get("p/m").Status == health.Healthy && !e.isRecovering("p/m") {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if st := hm.Get("p/m"); st.Status != health.Healthy {
			cancel()
			t.Fatalf("cycle %d did not recover: state=%+v calls=%d", cycle, st, calls.Load())
		}
		if e.isRecovering("p/m") {
			cancel()
			t.Fatalf("cycle %d left recovery marker behind", cycle)
		}
	}

	if failedCalls.Load() == 0 {
		cancel()
		t.Fatal("recovery soak never exercised a failed probe")
	}
	if successes := calls.Load() - failedCalls.Load(); successes < cycles*3 {
		cancel()
		t.Fatalf("recovery soak observed only %d successful probes; the re-admission threshold requires at least %d", successes, cycles*3)
	}

	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+8 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 8 {
		t.Fatalf("goroutine growth after recovery soak: delta=%d", delta)
	}
	if stats := e.Stats(); stats.RecoveryTracked != 0 || stats.ActiveProbes != 0 {
		t.Fatalf("recovery/probe state leaked after soak: %+v", stats)
	}
}
