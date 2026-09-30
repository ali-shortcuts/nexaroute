package probe

// v0.12.0 A4 (issue #76): recovered deployment routable within one
// health-check interval.
//
// Timing contract: a deployment that transitions from cooldown/unhealthy to
// recovered becomes eligible for selection within one configured
// health-check interval (ProbeInterval), without a process restart.
//
// The deterministic subtest below drives health.Manager + router.Router with
// a bounded test clock (SetNowFunc seam) and performs zero sleeps. The
// supervisor subtest drives the real recovery supervisor against a fake
// upstream with bounded polling only (5ms poll, 10s cap) and asserts exact
// probe counts and wall-clock timing evidence.

import (
	"context"
	"net/http"
	"net/http/httptest"
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

func a4TestConfig(intervalSeconds int) config.Config {
	cfg := config.Default()
	cfg.Routing.Strategy = "ready_queue" // only Healthy deployments are routable
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = true
	cfg.Probe.Concurrency = 8
	cfg.Probe.TimeoutMS = 2000
	cfg.Probe.MaxTokens = 1
	cfg.Probe.IntervalSeconds = intervalSeconds
	cfg.Probe.RecoveryAttempts = 5
	cfg.Probe.RecoveryRetryMS = 5
	cfg.Probe.CapabilityProbes = false
	cfg.Routing.CooldownSeconds = 1800
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1",
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Priority: 0, Weight: 1, Capabilities: config.Capabilities{Streaming: true}}},
	}}
	cfg.ApplyDefaults()
	return cfg
}

func TestA4_RecoveredRoutableWithinOneHealthInterval(t *testing.T) {
	// Bounded test clock: no sleeps anywhere in this subtest.
	var mu sync.Mutex
	fakeNow := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return fakeNow
	}
	advance := func(d time.Duration) time.Time {
		mu.Lock()
		defer mu.Unlock()
		fakeNow = fakeNow.Add(d)
		return fakeNow
	}

	const intervalSeconds = 60
	interval := time.Duration(intervalSeconds) * time.Second
	cfg := a4TestConfig(intervalSeconds)

	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	hm.SetNowFunc(nowFn)
	rt := router.New(cfg, hm)
	req := router.Requirement{Model: "auto", Streaming: true}
	candidates := func() []router.Scored { return rt.Candidates(req) }
	t0 := nowFn()

	// Unhealthy path: a quarantined (Degraded) deployment must not be
	// selected before readiness.
	hm.Quarantine("p/m", "real request failed", time.Millisecond)
	if got := candidates(); len(got) != 0 {
		t.Fatalf("quarantined deployment prematurely routable: %#v", got)
	}
	// The first successful recovery probe restores routability with zero
	// clock advance: fake_elapsed=0 < interval.
	hm.RecordSuccess("p/m", time.Millisecond)
	if got := candidates(); len(got) != 1 || got[0].Deployment.ID != "p/m" {
		t.Fatalf("recovered deployment not selected after first success probe: %#v", got)
	}
	if elapsed := nowFn().Sub(t0); elapsed >= interval {
		t.Fatalf("unhealthy->recovered took %s, exceeding one health-check interval %s", elapsed, interval)
	}
	t.Logf("A4 timing evidence (unhealthy path): fake_elapsed=%s interval=%s status=%s",
		nowFn().Sub(t0), interval, hm.Get("p/m").Status)

	// Cooldown path: a short 5s cooldown under the 60s health-check interval.
	hm.ForceCooldown("p/m", "429 rate limited", 5*time.Second)
	if got := candidates(); len(got) != 0 {
		t.Fatalf("cooldown deployment prematurely routable: %#v", got)
	}
	advance(2 * time.Second)
	if s := hm.Get("p/m"); s.Status != health.Cooldown {
		t.Fatalf("after 2s: status=%s want cooldown", s.Status)
	}
	if got := candidates(); len(got) != 0 {
		t.Fatalf("cooling deployment prematurely routable after 2s: %#v", got)
	}
	advance(4 * time.Second) // t0+6s total, past the 5s cooldown deadline
	if s := hm.Get("p/m"); s.Status != health.HalfOpen {
		t.Fatalf("after 6s: status=%s want half_open", s.Status)
	}
	// HalfOpen is not Healthy, so ready_queue must still exclude it: no
	// premature selection before the readiness probe succeeds.
	if got := candidates(); len(got) != 0 {
		t.Fatalf("half-open deployment prematurely routable before readiness probe: %#v", got)
	}
	// The first successful recovery probe after expiry restores routability.
	hm.RecordSuccess("p/m", time.Millisecond)
	if got := candidates(); len(got) != 1 || got[0].Deployment.ID != "p/m" {
		t.Fatalf("recovered deployment not selected after first success probe: %#v", got)
	}
	totalElapsed := nowFn().Sub(t0)
	if totalElapsed >= interval {
		t.Fatalf("cooldown->recovered took %s, exceeding one health-check interval %s", totalElapsed, interval)
	}
	t.Logf("A4 timing evidence (cooldown path): fake_elapsed=%s interval=%s status=%s",
		totalElapsed, interval, hm.Get("p/m").Status)
}

func TestA4_SupervisorFirstSuccessfulProbeRestoresRoutability(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"temporary"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"ok","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}))
	defer up.Close()

	const intervalSeconds = 60
	cfg := a4TestConfig(intervalSeconds)
	for i := range cfg.Providers {
		cfg.Providers[i].BaseURL = up.URL
	}
	cfg.ApplyDefaults()

	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	e := New(cfg, reg, rt, hm, events.New(100))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req := router.Requirement{Model: "auto", Streaming: true}
	// No premature selection before readiness: the unprobed deployment is
	// Unknown, which ready_queue excludes.
	if got := rt.Candidates(req); len(got) != 0 {
		t.Fatalf("unprobed deployment prematurely routable: %#v", got)
	}

	start := time.Now()
	res := e.Prime(ctx) // single engine instance; no restart or reload follows
	primeElapsed := time.Since(start)
	if res.Total != 1 || res.Failed != 1 {
		t.Fatalf("down sweep should fail the single deployment: %+v", res)
	}
	// Invariant regardless of the recovery-worker race: a deployment that is
	// not Healthy must never be selected.
	if st := hm.Get("p/m"); st.Status != health.Healthy {
		if got := rt.Candidates(req); len(got) != 0 {
			t.Fatalf("premature selection while status=%s: %#v", st.Status, got)
		}
	}

	// Bounded polling only (5ms poll inside waitForState, 10s cap): the first
	// successful recovery probe must restore health and routability.
	st := waitForState(t, hm, "p/m", health.Healthy, 10*time.Second)
	recoveredElapsed := time.Since(start)
	got := rt.Candidates(req)
	if len(got) != 1 || got[0].Deployment.ID != "p/m" {
		t.Fatalf("recovered deployment not routable after first success probe: %#v (health=%+v)", got, st)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls=%d want exactly 1 initial + 1 first-recovery probe", n)
	}
	if recoveredElapsed >= cfg.ProbeInterval() {
		t.Fatalf("recovery took %s, exceeding one health-check interval %s", recoveredElapsed, cfg.ProbeInterval())
	}
	t.Logf("A4 supervisor timing evidence: prime_elapsed=%s recovery_total=%s interval=%s upstream_calls=%d status=%s",
		primeElapsed, recoveredElapsed, cfg.ProbeInterval(), calls.Load(), st.Status)
}

func TestA4_RecoverySelectionRace(t *testing.T) {
	cfg := a4TestConfig(60)
	// A huge threshold keeps concurrent RecordFailure observations in Degraded
	// (never Cooldown), so the closing RecordSuccess deterministically
	// restores Healthy and the final selection assertion is exact.
	hm := health.New(1<<20, time.Minute)
	rt := router.New(cfg, hm)
	req := router.Requirement{Model: "auto", Streaming: true}
	hm.RecordSuccess("p/m", time.Millisecond)

	start := time.Now()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				switch (i + g) % 5 {
				case 0:
					hm.RecordSuccess("p/m", time.Millisecond)
				case 1:
					hm.RecordFailure("p/m", "race", time.Millisecond)
				case 2:
					hm.Quarantine("p/m", "race", time.Millisecond)
				case 3:
					_ = hm.Get("p/m")
				default:
					_ = rt.Candidates(req)
				}
			}
		}(g)
	}
	wg.Wait()
	elapsed := time.Since(start)

	hm.RecordSuccess("p/m", time.Millisecond)
	got := rt.Candidates(req)
	if len(got) != 1 || got[0].Deployment.ID != "p/m" {
		t.Fatalf("recovered deployment not selected after race: %#v (health=%+v)", got, hm.Get("p/m"))
	}
	t.Logf("A4 race timing evidence: 8x200 ops elapsed=%s status=%s", elapsed, hm.Get("p/m").Status)
}
