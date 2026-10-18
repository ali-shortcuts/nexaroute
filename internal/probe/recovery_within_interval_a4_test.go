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
	// A4 asserts the first successful probe restores routability within one
	// interval. That is now the explicit threshold=1 escape hatch; the
	// production default is 3-of-5.
	cfg.Probe.RecoverySuccessThreshold = 1
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

// waitForRecoveryQuiescent polls until the recovery lifecycle for id is fully
// complete: the deployment is Healthy, a recovery_ready event was emitted for
// it, and the engine cleared its recovery bookkeeping. All three are
// required: Healthy alone can be observed while the worker is still between
// RecordSuccess and clearRecoveryTask, and the event alone can precede the
// bookkeeping clear. Poll-until-condition on state (5ms poll, bounded) —
// never a fixed delay, never a mid-flight snapshot — so an absolute probe
// count asserted afterwards is exact no matter when the worker was
// scheduled. Exceeding the bound means the supervisor path genuinely stalled.
func waitForRecoveryQuiescent(t *testing.T, hm *health.Manager, bus *events.Bus, e *Engine, id string, timeout time.Duration) health.State {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st := hm.Get(id)
		ready := false
		for _, ev := range bus.Snapshot() {
			if ev.Kind == "recovery_ready" && ev.Deployment == id {
				ready = true
				break
			}
		}
		if st.Status == health.Healthy && ready && !e.isRecovering(id) {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	st := hm.Get(id)
	t.Fatalf("recovery for %s did not reach quiescence within %s (status=%s recovering=%v)",
		id, timeout, st.Status, e.isRecovering(id))
	return st
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
	bus := events.New(100)
	e := New(cfg, reg, rt, hm, bus)
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
	// NOTE (issue #404, round-7 post-mortem): the mid-flight "not Healthy =>
	// not selected" check that used to live here was deleted. hm.Get and
	// rt.Candidates are two non-atomic observations; the recovery worker may
	// legitimately finish between them, so the deployment can be legally
	// selectable at the moment Candidates runs while the earlier Get still
	// showed Degraded. Asserting on that interleaving tests the test
	// scheduler, not the product. Premature-selection coverage stays
	// deterministic via the unprobed/Unknown check above (no engine worker
	// exists yet at that point).

	// Bounded polling only (5ms poll, 10s cap): the first successful
	// recovery probe must restore health and routability.
	//
	// Deterministic design (issue #404, revised after the round-7 gate
	// failure): the previous revision took an unsynchronized snapshot of the
	// upstream call count and asserted exactly one more probe after it. That
	// encoded "exactly one probe after my snapshot", not "exactly one
	// recovery probe after prime": Prime's runOnce enqueues the recovery
	// task before returning and the first attempt carries no delay, so the
	// worker may legitimately finish before the test goroutine's next
	// statement under load. The count is now absolute and taken only after
	// quiescence — Healthy observed, recovery_ready emitted, and the
	// engine's recovery bookkeeping cleared — via poll-until-condition on
	// state, never a fixed delay and never a mid-flight snapshot. A
	// legitimately early worker can no longer break the assertion, while a
	// genuine duplicate probe still fails it (total 3+): the red/green
	// proof is by mutation, see the fix commit.
	//
	// Prime's runOnce already enqueued the recovery task for the failed
	// deployment; the explicit Recover below exercises the public supervisor
	// path and either dedups (worker still in flight) or hits the Healthy
	// guard (worker already done) — zero additional probes either way.
	e.Recover("p/m")
	st := waitForRecoveryQuiescent(t, hm, bus, e, "p/m", 10*time.Second)
	recoveredElapsed := time.Since(start)
	got := rt.Candidates(req)
	if len(got) != 1 || got[0].Deployment.ID != "p/m" {
		t.Fatalf("recovered deployment not routable after first success probe: %#v (health=%+v)", got, st)
	}
	// Absolute total, not snapshot-relative: exactly 1 prime probe (503)
	// + exactly 1 recovery probe (200). Exactly one recovery task can exist
	// (runOnce enqueues one per failed deployment; the explicit Recover
	// above dedups), and on success the worker clears the task and
	// schedules nothing further — so 2 is exact once quiescence holds.
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls=%d want exactly 1 prime probe + 1 recovery probe", n)
	}
	// Sync-path guard: quiescence already required the recovery_ready event,
	// which proves the recovery went through the engine's recovery worker
	// rather than a direct health-manager write.
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
