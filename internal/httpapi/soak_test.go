package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func requireHTTPSoak(t *testing.T) {
	t.Helper()
	if os.Getenv("NEXAROUTE_SOAK") != "1" {
		t.Skip("set NEXAROUTE_SOAK=1 to run long-form soak checks")
	}
}

func TestSoakConcurrentTrafficAndHotReload(t *testing.T) {
	requireHTTPSoak(t)

	var upstreamCalls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		time.Sleep(time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.MaxInflightRequests = 64
	cfg.Routing.MaxAttempts = 1
	pc := config.ProviderConfig{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true, MaxConcurrency: 64,
	}
	for i := 0; i < 10; i++ {
		pc.Models = append(pc.Models, config.ModelConfig{
			ID: fmt.Sprintf("m%d", i), Model: fmt.Sprintf("model-%d", i),
			Aliases: []string{"coding"}, Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true},
		})
	}
	cfg.Providers = []config.ProviderConfig{pc}
	s := testGateway(t, cfg)
	h := s.Handler()

	const workers = 24
	const perWorker = 250
	var failed atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			for i := 0; i < perWorker; i++ {
				req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
					strings.NewReader(`{"model":"coding","messages":[{"role":"user","content":"hello"}]}`))
				req.Header.Set("x-session-id", fmt.Sprintf("soak-%d-%d", g, i%50))
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, req)
				if rr.Code != http.StatusOK {
					failed.Add(1)
					return
				}
			}
		}(g)
	}
	close(start)

	for i := 0; i < 100; i++ {
		next := s.currentConfig()
		next.Providers[0].Name = fmt.Sprintf("P-%d", i)
		next.Routing.CapacityWeight = float64(i % 50)
		if err := s.applyConfig(next); err != nil {
			t.Fatalf("hot reload %d failed: %v", i, err)
		}
	}
	wg.Wait()

	if failed.Load() != 0 {
		t.Fatalf("traffic failures=%d", failed.Load())
	}
	if want := int64(workers * perWorker); upstreamCalls.Load() != want {
		t.Fatalf("upstream calls=%d want %d", upstreamCalls.Load(), want)
	}
	if s.inflight.Load() != 0 {
		t.Fatalf("global inflight leaked: %d", s.inflight.Load())
	}
	stats := s.reg.Stats()
	if len(stats) != 1 || stats[0].ActiveRequests != 0 || stats[0].WaitingRequests != 0 {
		t.Fatalf("provider capacity leaked after soak: %+v", stats)
	}
	if got := s.rt.SessionCount(); got > 10000 {
		t.Fatalf("session affinity state escaped bound: %d", got)
	}
}

// TestSoakFlappingUpstreamFailureSemantics (M4-8) drives sustained
// traffic through an upstream that alternates between healthy and
// hard-failure (connection cut) phases. Invariants pinned: every
// request resolves (200 or the documented 5xx — never a hang, never
// an unexpected status); the gateway never invents a success the
// upstream did not serve; the deployment breaker sheds load at the
// edge during failure phases (client errors exceed upstream hits);
// once the upstream is healthy again and the cooldown elapses,
// traffic recovers fully; no in-flight or goroutine leaks remain.
func TestSoakFlappingUpstreamFailureSemantics(t *testing.T) {
	requireHTTPSoak(t)

	var healthy atomic.Bool
	healthy.Store(true)
	var servedOK atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Millisecond)
		if !healthy.Load() {
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
			return
		}
		servedOK.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer up.Close()

	cfg := config.Default()
	// Default ready_mesh strategy: eligibility requires Healthy and
	// recovery is probe-driven, so the probe engine runs with fast
	// recovery settings (this is the production-shaped configuration;
	// the probes-off half-open trial path for non-ready strategies is
	// pinned separately in the failure-injection suite).
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = true
	cfg.Probe.IntervalSeconds = 1
	cfg.Probe.ReadyLeaseSeconds = 3
	cfg.Probe.TimeoutMS = 2000
	cfg.Probe.RecoveryAttempts = 100
	cfg.Probe.RecoveryRetryMS = 200
	cfg.Routing.MaxAttempts = 1
	cfg.Routing.MaxRepairAttempts = 0
	cfg.Routing.MaxInflightRequests = 128
	cfg.Routing.FailureThreshold = 3
	cfg.Routing.CooldownSeconds = 1
	cfg.Routing.ProviderFailureThreshold = 3
	cfg.Routing.ProviderCooldownSeconds = 1
	cfg.Routing.CapabilityFailureThreshold = 3
	cfg.Routing.CapabilityCooldownSeconds = 1
	cfg.Providers = []config.ProviderConfig{{
		ID: "flap", Name: "Flap", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true, MaxConcurrency: 64,
		Models: []config.ModelConfig{{
			ID: "m", Model: "flap-model", Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true},
		}},
	}}
	s := testGateway(t, cfg)
	s.SyncCapabilityContracts()
	h := s.Handler()
	// testGateway never starts the probe engine (main.go does);
	// ready_mesh recovery is probe-driven, so start it explicitly.
	probeCtx, stopProbe := context.WithCancel(context.Background())
	defer stopProbe()
	s.probe.Start(probeCtx)

	// Flapper: 1.2s healthy / 1.2s failing for the traffic window.
	stopFlap := make(chan struct{})
	go func() {
		tick := time.NewTicker(1200 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopFlap:
				return
			case <-tick.C:
				healthy.Store(!healthy.Load())
			}
		}
	}()

	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	var clientOK, clientErr, clientOther, clientTotal atomic.Int64
	deadline := time.Now().Add(6 * time.Second)
	var wg sync.WaitGroup
	for g := 0; g < 12; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
					strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hello"}]}`))
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, req)
				clientTotal.Add(1)
				switch {
				case rr.Code == http.StatusOK:
					clientOK.Add(1)
				case rr.Code >= 500 && rr.Code <= 599:
					clientErr.Add(1)
				default:
					clientOther.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	close(stopFlap)

	if clientOther.Load() != 0 {
		t.Fatalf("unexpected statuses: %d (ok=%d err=%d)", clientOther.Load(), clientOK.Load(), clientErr.Load())
	}
	if clientOK.Load()+clientErr.Load() != clientTotal.Load() || clientTotal.Load() == 0 {
		t.Fatalf("requests lost: ok=%d err=%d total=%d", clientOK.Load(), clientErr.Load(), clientTotal.Load())
	}
	if clientOK.Load() == 0 || clientErr.Load() == 0 {
		t.Fatalf("both phases must bite: ok=%d err=%d", clientOK.Load(), clientErr.Load())
	}
	if clientOK.Load() > servedOK.Load() {
		t.Fatalf("gateway invented successes: client=%d upstream=%d", clientOK.Load(), servedOK.Load())
	}
	if s.inflight.Load() != 0 {
		t.Fatalf("global inflight leaked: %d", s.inflight.Load())
	}

	// Recovery: upstream permanently healthy; after the cooldown the
	// full traffic flow must succeed again.
	healthy.Store(true)
	recoverStart := time.Now()
	for i := 0; i < 120; i++ {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"probe"}]}`))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code == http.StatusOK {
			t.Logf("recovered after %s", time.Since(recoverStart))
			break
		}
		if i == 119 {
			t.Fatalf("no recovery within %s", time.Since(recoverStart))
		}
		time.Sleep(250 * time.Millisecond)
	}
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"recover"}]}`))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("recovery request %d: status=%d body=%s", i, rr.Code, rr.Body.String())
		}
	}

	drain := time.Now().Add(10 * time.Second)
	for time.Now().Before(drain) {
		if runtime.NumGoroutine() <= baseline+16 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+16 {
		t.Fatalf("goroutines after soak: %d (baseline %d) — failure paths leak", got, baseline)
	}
	t.Logf("flapping soak: total=%d ok=%d err=%d upstreamOK=%d goroutines %d->%d",
		clientTotal.Load(), clientOK.Load(), clientErr.Load(), servedOK.Load(), baseline, runtime.NumGoroutine())
}
