package probe

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

func recoveryFixture(t *testing.T, handler http.HandlerFunc, recoveryAttempts, cooldownSeconds int) (*Engine, *health.Manager, *router.Router, *atomic.Int32, context.Context, context.CancelFunc) {
	t.Helper()
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(up.Close)

	cfg := config.Default()
	cfg.Routing.Strategy = "ready_queue"
	cfg.Routing.CooldownSeconds = cooldownSeconds
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = true
	cfg.Probe.Concurrency = 8
	cfg.Probe.TimeoutMS = 1000
	cfg.Probe.RecoveryAttempts = recoveryAttempts
	cfg.Probe.RecoveryRetryMS = 5
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Priority: 0, Weight: 1, Capabilities: config.Capabilities{Streaming: true}}},
	}}
	cfg.ApplyDefaults()

	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	e := New(cfg, reg, rt, hm, events.New(100))
	ctx, cancel := context.WithCancel(context.Background())
	return e, hm, rt, &calls, ctx, cancel
}

func waitForState(t *testing.T, hm *health.Manager, id string, wanted health.Status, timeout time.Duration) health.State {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st := hm.Get(id)
		if st.Status == wanted {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	st := hm.Get(id)
	t.Fatalf("deployment %s status=%s want=%s", id, st.Status, wanted)
	return st
}

func TestSupervisorReturnsModelToReadyQueueOnFirstSuccessfulRecovery(t *testing.T) {
	var upstreamCalls atomic.Int32
	e, hm, rt, _, ctx, cancel := recoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		n := upstreamCalls.Add(1)
		if n <= 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"temporary"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}, 5, 1800)
	defer cancel()

	res := e.Prime(ctx)
	if res.Passed != 0 || res.Failed != 1 {
		t.Fatalf("initial prime=%+v want first health check to fail", res)
	}
	st := waitForState(t, hm, "p/m", health.Healthy, time.Second)
	if st.RecoveryFailures != 0 {
		t.Fatalf("successful recovery must reset recovery failures: %+v", st)
	}
	got := rt.Candidates(router.Requirement{Model: "auto", Streaming: true})
	if len(got) != 1 || got[0].Deployment.ID != "p/m" {
		t.Fatalf("recovered deployment did not re-enter ready queue: %#v", got)
	}
	if n := upstreamCalls.Load(); n != 4 {
		t.Fatalf("upstream calls=%d want 1 initial + 3 recovery attempts", n)
	}
}

func TestModelUnavailable400EntersSupervisedRecoveryAndRecoversOnFirstSuccess(t *testing.T) {
	var calls atomic.Int32
	e, hm, rt, _, ctx, cancel := recoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"model_unavailable","message":"model temporarily unavailable"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}, 5, 1800)
	defer cancel()

	res := e.Prime(ctx)
	if res.Failed != 1 || hm.Get("p/m").Status != health.Degraded || !hm.Get("p/m").Quarantined {
		t.Fatalf("model_unavailable was not quarantined: result=%+v state=%+v", res, hm.Get("p/m"))
	}
	st := waitForState(t, hm, "p/m", health.Healthy, time.Second)
	if st.Quarantined || st.RecoveryFailures != 0 {
		t.Fatalf("first verified recovery did not return deployment to ready: %+v", st)
	}
	if got := rt.Candidates(router.Requirement{Model: "auto", Streaming: true}); len(got) != 1 {
		t.Fatalf("recovered deployment missing from route candidates: %+v", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("probe calls=%d want failed initial probe + first successful recovery", calls.Load())
	}
}

func TestUnsupportedProbeParameterDoesNotQuarantineDeployment(t *testing.T) {
	e, hm, _, calls, ctx, cancel := recoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"unsupported_parameter","param":"max_tokens","message":"max_tokens is not supported"}}`))
	}, 5, 1800)
	defer cancel()

	result := e.RunOnce(ctx)
	state := hm.Get("p/m")
	if result.Failed != 1 || state.Status != health.Unknown || state.Quarantined {
		t.Fatalf("compatibility rejection was treated as health failure: result=%+v state=%+v", result, state)
	}
	if calls.Load() != 1 || e.Stats().RecoveryTracked != 0 {
		t.Fatalf("inconclusive probe entered recovery: calls=%d stats=%+v", calls.Load(), e.Stats())
	}
}

func TestRetiredModelIsNeverScheduledForRecoveryProbe(t *testing.T) {
	var calls atomic.Int32
	e, hm, rt, _, ctx, cancel := recoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusGone)
		_, _ = w.Write([]byte(`{"error":{"code":"model_eol","type":"model_retired","message":"model has reached end of life"}}`))
	}, 5, 1800)
	defer cancel()
	e.setRunContext(ctx)

	res := e.RunOnce(ctx)
	if res.Failed != 1 {
		t.Fatalf("probe result=%+v want one EOL failure", res)
	}
	st := hm.Get("p/m")
	if st.Status != health.Retired || st.LastErrorClass != "model_retired" {
		t.Fatalf("model EOL did not persist as retired: %+v", st)
	}
	if calls.Load() != 1 {
		t.Fatalf("EOL deployment received %d probes; want only the failed initial probe", calls.Load())
	}
	if e.Stats().RecoveryTracked != 0 {
		t.Fatalf("retired deployment entered recovery supervisor: %+v", e.Stats())
	}
	if got := rt.Candidates(router.Requirement{Model: "auto", Streaming: true}); len(got) != 0 {
		t.Fatalf("retired deployment remained routable: %+v", got)
	}
	e.Recover("p/m")
	if e.Stats().RecoveryTracked != 0 || calls.Load() != 1 {
		t.Fatalf("manual recovery rescheduled a retired model: stats=%+v calls=%d", e.Stats(), calls.Load())
	}
}

func TestSupervisorFiveFailuresEnterCooldownWithoutSixthFailure(t *testing.T) {
	e, hm, rt, calls, ctx, cancel := recoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"down"}`))
	}, 5, 30)
	defer cancel()

	res := e.Prime(ctx)
	if res.Failed != 1 {
		t.Fatalf("initial prime=%+v want failure", res)
	}
	st := waitForState(t, hm, "p/m", health.Cooldown, time.Second)
	if st.RecoveryFailures != 5 {
		t.Fatalf("recovery failures=%d want exactly 5: %+v", st.RecoveryFailures, st)
	}
	if n := calls.Load(); n != 6 {
		t.Fatalf("upstream calls=%d want 1 initial check + 5 recovery probes", n)
	}
	if wait := time.Until(st.CooldownUntil); wait < 28*time.Second || wait > 31*time.Second {
		t.Fatalf("cooldown duration=%s want about 30s in test fixture", wait)
	}
	if got := rt.Candidates(router.Requirement{Model: "auto", Streaming: true}); len(got) != 0 {
		t.Fatalf("cooldown model must not be routable: %#v", got)
	}
}

func TestSupervisorDoesNotConsumeRecoveryBudgetWhileCredentialRateLimited(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Routing.Strategy = "ready_queue"
	cfg.Routing.MaxRetryAfterSeconds = 2
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = true
	cfg.Probe.RecoveryAttempts = 5
	cfg.Probe.RecoveryRetryMS = 5
	cfg.Probe.TimeoutMS = 1000
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL,
		APIKey: "k", AuthMode: "bearer", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	cfg.ApplyDefaults()

	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	e := New(cfg, reg, rt, hm, events.New(100))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	res := e.Prime(ctx)
	if res.Failed != 1 {
		t.Fatalf("prime=%+v want initial 429 failure", res)
	}
	st := waitForState(t, hm, "p/m", health.Healthy, 3*time.Second)
	if st.RecoveryFailures != 0 {
		t.Fatalf("rate-limit wait consumed recovery budget: %+v", st)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d want initial 429 + one post-cooldown recovery", calls.Load())
	}
}

func TestSupervisorFakeClockFiveAttemptsAndCooldownExpiryReentry(t *testing.T) {
	var mu sync.Mutex
	fakeNow := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return fakeNow
	}
	advance := func(d time.Duration) {
		mu.Lock()
		fakeNow = fakeNow.Add(d)
		mu.Unlock()
	}

	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"offline"}`))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Routing.Strategy = "ready_queue"
	cfg.Routing.CooldownSeconds = 1800 // 30-minute cooldown
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = true
	cfg.Probe.Concurrency = 8
	cfg.Probe.TimeoutMS = 1000
	cfg.Probe.RecoveryAttempts = 5
	cfg.Probe.RecoveryRetryMS = 5
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Priority: 0, Weight: 1, Capabilities: config.Capabilities{Streaming: true}}},
	}}
	cfg.ApplyDefaults()

	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	hm.SetNowFunc(nowFn)
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	e := New(cfg, reg, rt, hm, events.New(100))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	res := e.Prime(ctx)
	if res.Failed != 1 {
		t.Fatalf("prime failed=%d want 1", res.Failed)
	}

	// Wait for supervisor to exhaust all 5 recovery attempts and enter Cooldown.
	st := waitForState(t, hm, "p/m", health.Cooldown, 2*time.Second)
	if st.RecoveryFailures != 5 {
		t.Fatalf("recovery failures=%d want exactly 5", st.RecoveryFailures)
	}
	if n := calls.Load(); n != 6 {
		t.Fatalf("calls=%d want 1 initial + 5 recovery attempts", n)
	}
	if got := rt.Candidates(router.Requirement{Model: "auto", Streaming: true}); len(got) != 0 {
		t.Fatalf("cooldown model must not be routable: %#v", got)
	}

	// Advance fake clock by 10 minutes: still in cooldown, not routable.
	advance(10 * time.Minute)
	if s := hm.Get("p/m"); s.Status != health.Cooldown {
		t.Fatalf("after 10 min: status=%s want Cooldown", s.Status)
	}
	if got := rt.Candidates(router.Requirement{Model: "auto", Streaming: true}); len(got) != 0 {
		t.Fatalf("cooldown model must not be routable after 10m: %#v", got)
	}

	// Advance fake clock past the 30-minute cooldown window (by 21 more minutes = 31 total).
	advance(21 * time.Minute)

	// Now status normalizes to HalfOpen upon access.
	stExpired := hm.Get("p/m")
	if stExpired.Status != health.HalfOpen {
		t.Fatalf("after 31 min: status=%s want HalfOpen", stExpired.Status)
	}
	if stExpired.RecoveryFailures != 0 {
		t.Fatalf("recovery failures after expiry=%d want 0", stExpired.RecoveryFailures)
	}

	// Priority router (which admits HalfOpen candidates) includes the model.
	priorityCfg := cfg
	priorityCfg.Routing.Strategy = "priority"
	rtPriority := router.New(priorityCfg, hm)
	priorityCandidates := rtPriority.Candidates(router.Requirement{Model: "auto", Streaming: true})
	if len(priorityCandidates) != 1 || priorityCandidates[0].Deployment.ID != "p/m" {
		t.Fatalf("expired cooldown model must re-enter priority candidates: %#v", priorityCandidates)
	}

	// A successful recovery observation marks it Healthy.
	hm.RecordSuccess("p/m", 15*time.Millisecond)
	stHealthy := hm.Get("p/m")
	if stHealthy.Status != health.Healthy {
		t.Fatalf("after success: status=%s want Healthy", stHealthy.Status)
	}
	if stHealthy.ConsecutiveFailures != 0 || stHealthy.RecoveryFailures != 0 {
		t.Fatalf("healthy state has non-zero failure counts: %+v", stHealthy)
	}

	// In ready_queue strategy, the recovered Healthy model now re-enters the ready candidate pool.
	candidates := rt.Candidates(router.Requirement{Model: "auto", Streaming: true})
	if len(candidates) != 1 || candidates[0].Deployment.ID != "p/m" {
		t.Fatalf("recovered model must re-enter ready_queue candidates pool: %#v", candidates)
	}
}
