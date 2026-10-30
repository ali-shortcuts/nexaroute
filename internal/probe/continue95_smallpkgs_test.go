package probe

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func TestContinue95ClassifyAndRecoveryBounds(t *testing.T) {
	cases := []struct {
		status int
		err    error
		want   probeFailureClass
	}{
		{http.StatusUnauthorized, nil, classCredentialFailure},
		{http.StatusForbidden, nil, classCredentialFailure},
		{http.StatusPaymentRequired, nil, classQuotaExhausted},
		{http.StatusTooManyRequests, nil, classRateLimited},
		{http.StatusRequestTimeout, nil, classTransientTimeout},
		{http.StatusGatewayTimeout, nil, classTransientTimeout},
		{0, context.DeadlineExceeded, classTransientTimeout},
		{0, context.Canceled, classTransientTimeout},
		{0, fmt.Errorf("boom"), classProviderIncident},
		{http.StatusInternalServerError, nil, classProviderIncident},
		{http.StatusBadRequest, context.DeadlineExceeded, classTransientTimeout},
		{http.StatusBadRequest, fmt.Errorf("boom"), classTransientTransport},
	}
	for _, c := range cases {
		if got := classifyProbeFailure(c.status, c.err); got != c.want {
			t.Errorf("classifyProbeFailure(%d, %v) = %v, want %v", c.status, c.err, got, c.want)
		}
	}
	cfg := config.Default()
	cfg.Probe.RecoveryAttempts = 0
	if got := recoveryAttempts(cfg); got != 5 {
		t.Errorf("recoveryAttempts(0) = %d, want 5", got)
	}
	cfg.Probe.RecoveryAttempts = 3
	if got := recoveryAttempts(cfg); got != 3 {
		t.Errorf("recoveryAttempts(3) = %d, want 3", got)
	}
}

func TestContinue95StartRecoveryWorkersGuards(t *testing.T) {
	(&Engine{}).startRecoveryWorkers(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := New(config.Default(), nil, nil, nil, nil)
	e.startRecoveryWorkers(ctx)
}

func Continue95ProbeEngine(bus *events.Bus) *Engine {
	cfg := config.Default()
	cfg.ApplyDefaults()
	return New(cfg, nil, nil, nil, bus)
}

func Continue95BusHasKind(bus *events.Bus, kind string) bool {
	for _, ev := range bus.Snapshot() {
		if ev.Kind == kind {
			return true
		}
	}
	return false
}

func Continue95FillQueue(t *testing.T, e *Engine, ctx context.Context) {
	t.Helper()
	for i := 0; i < maxRecoveryQueue; i++ {
		if !e.enqueueRecovery(ctx, recoveryTask{id: "fill", attempt: 1, generation: 0}) {
			t.Fatalf("queue fill failed at %d", i)
		}
	}
}

func TestContinue95ScheduleRecoveryBranches(t *testing.T) {
	ctx := context.Background()
	// Delay zero, stale task: cleared without queueing.
	bus := events.New(100)
	e := Continue95ProbeEngine(bus)
	e.scheduleRecovery(ctx, recoveryTask{id: "stale", attempt: 1, generation: 1}, 0)
	if e.isRecovering("stale") {
		t.Error("stale task should not be recovering")
	}
	// Delay zero, current task, queue free: enqueued.
	e.recoveryMu.Lock()
	e.recovering["now"] = true
	e.recoveryGenerations["now"] = 7
	e.recoveryMu.Unlock()
	e.scheduleRecovery(ctx, recoveryTask{id: "now", attempt: 1, generation: 7}, 0)
	select {
	case task := <-e.recoveryQueue:
		if task.id != "now" || task.generation != 7 {
			t.Errorf("queued task = %+v, want id=now generation=7", task)
		}
	default:
		t.Error("current zero-delay task should be enqueued")
	}
	// Delay zero, current task, queue full: cleared + bus event.
	e.recoveryMu.Lock()
	e.recovering["full"] = true
	e.recoveryGenerations["full"] = 5
	e.recoveryMu.Unlock()
	Continue95FillQueue(t, e, ctx)
	e.scheduleRecovery(ctx, recoveryTask{id: "full", attempt: 1, generation: 5}, 0)
	if e.isRecovering("full") {
		t.Error("queue-full task should be cleared")
	}
	if !Continue95BusHasKind(bus, "recovery_queue_full") {
		t.Error("queue-full schedule should emit recovery_queue_full")
	}
	// Positive delay, stale task: cleared.
	e2 := Continue95ProbeEngine(events.New(100))
	e2.scheduleRecovery(ctx, recoveryTask{id: "stale2", attempt: 1, generation: 1}, time.Hour)
	if e2.isRecovering("stale2") {
		t.Error("stale delayed task should not be recovering")
	}
	// Positive delay twice: the old timer is stopped and replaced.
	e2.recoveryMu.Lock()
	e2.recovering["twice"] = true
	e2.recoveryGenerations["twice"] = 3
	e2.recoveryMu.Unlock()
	e2.scheduleRecovery(ctx, recoveryTask{id: "twice", attempt: 1, generation: 3}, time.Hour)
	e2.scheduleRecovery(ctx, recoveryTask{id: "twice", attempt: 1, generation: 3}, time.Hour)
	if !e2.isRecovering("twice") {
		t.Error("twice-scheduled task should still be recovering")
	}
	e2.cancelAllRecoveries()
	// Positive delay, timer fires with a cancelled context: cleared.
	e3 := Continue95ProbeEngine(events.New(100))
	e3.recoveryMu.Lock()
	e3.recovering["cancelled"] = true
	e3.recoveryGenerations["cancelled"] = 2
	e3.recoveryMu.Unlock()
	cctx, cancel := context.WithCancel(ctx)
	e3.scheduleRecovery(cctx, recoveryTask{id: "cancelled", attempt: 1, generation: 2}, 5*time.Millisecond)
	cancel()
	time.Sleep(100 * time.Millisecond)
	if e3.isRecovering("cancelled") {
		t.Error("timer-fired task with cancelled context should be cleared")
	}
	// Positive delay, timer fires with a full queue: cleared + bus event.
	bus4 := events.New(100)
	e4 := Continue95ProbeEngine(bus4)
	Continue95FillQueue(t, e4, ctx)
	e4.recoveryMu.Lock()
	e4.recovering["late-full"] = true
	e4.recoveryGenerations["late-full"] = 4
	e4.recoveryMu.Unlock()
	e4.scheduleRecovery(ctx, recoveryTask{id: "late-full", attempt: 1, generation: 4}, 5*time.Millisecond)
	time.Sleep(150 * time.Millisecond)
	if e4.isRecovering("late-full") {
		t.Error("timer-fired task with full queue should be cleared")
	}
	if !Continue95BusHasKind(bus4, "recovery_queue_full") {
		t.Error("timer-fired queue-full schedule should emit recovery_queue_full")
	}
}

func TestContinue95RecoveryWorkerStaleTask(t *testing.T) {
	e := Continue95ProbeEngine(events.New(100))
	e.recoveryMu.Lock()
	e.recovering["s"] = true
	e.recoveryGenerations["s"] = 9
	e.recoveryMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.recoveryWorker(ctx)
	if !e.enqueueRecovery(ctx, recoveryTask{id: "s", attempt: 1, generation: 8}) {
		t.Fatal("enqueue stale task")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(e.recoveryQueue) > 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(e.recoveryQueue) > 0 {
		t.Fatal("worker did not drain the stale task")
	}
	if !e.isRecovering("s") {
		t.Error("stale task must be skipped, not cleared, by the worker")
	}
}

func Continue95ProbeHarness(t *testing.T, srv *httptest.Server, mutate func(*config.Config)) *Engine {
	t.Helper()
	cfg := config.Default()
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Probe.TimeoutMS = 5000
	cfg.Probe.Concurrency = 4
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: srv.URL, AuthMode: "bearer", APIKey: "k",
		Enabled: true,
		Models:  []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	if mutate != nil {
		mutate(&cfg)
	}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	return New(cfg, reg, rt, hm, events.New(200))
}

func TestContinue95ProcessRecoveryAcquireCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}))
	defer srv.Close()
	e := Continue95ProbeHarness(t, srv, func(cfg *config.Config) {
		cfg.Probe.Concurrency = 1
	})
	e.recoveryMu.Lock()
	e.recovering["p/m"] = true
	e.recoveryGenerations["p/m"] = 1
	e.limitMu.Lock()
	e.activeProbes = 1
	e.limitMu.Unlock()
	e.recoveryMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.processRecoveryTask(ctx, recoveryTask{id: "p/m", attempt: 1, generation: 1})
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("processRecoveryTask did not return after context cancel")
	}
	if e.isRecovering("p/m") {
		t.Error("acquire-cancelled recovery task should be cleared")
	}
}

func TestContinue95ProcessRecoveryRetryAfterClamp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer srv.Close()
	e := Continue95ProbeHarness(t, srv, func(cfg *config.Config) {
		cfg.Routing.MaxRetryAfterSeconds = 60
		cfg.Routing.FailureThreshold = 10
		cfg.Probe.RecoveryRetryMS = 3600000
	})
	defer e.cancelAllRecoveries()
	task := recoveryTask{id: "p/m", attempt: 1, generation: 3}
	e.recoveryMu.Lock()
	e.recovering["p/m"] = true
	e.recoveryGenerations["p/m"] = 3
	e.recoveryMu.Unlock()
	ctx := context.Background()
	e.processRecoveryTask(ctx, task)
	// The registry builds adapters with retryAfterCap =
	// MaxRetryAfterSeconds, so with one fixed config the credential
	// cooldown (~60s) can never exceed maxWait and the clamp below
	// cannot fire. Reloading the engine config to a 10s cap leaves
	// the already-built adapter at 60s, so the second call's
	// RetryAfterError (~60s) must be clamped to exactly 10s.
	reloaded := e.current()
	reloaded.Routing.MaxRetryAfterSeconds = 10
	e.Reload(reloaded)
	e.processRecoveryTask(ctx, task)
	found := false
	for _, ev := range e.bus.Snapshot() {
		if ev.Kind == "recovery_deferred" && strings.Contains(ev.Message, "retry after 10s") {
			found = true
		}
	}
	if !found {
		t.Errorf("want recovery_deferred with retry after 10s; bus events: %+v", e.bus.Snapshot())
	}
}

func TestContinue95RunLoopTimerAndTrigger(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.ApplyDefaults()
	// Set after ApplyDefaults: ApplyDefaults fills a default interval,
	// but the clamp under test needs ProbeInterval() below one second.
	cfg.Probe.IntervalSeconds = 0
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	rt := router.New(cfg, hm)
	e := New(cfg, nil, rt, hm, events.New(100))
	e.recoveryMu.Lock()
	e.recovering["x"] = true
	e.recoveryGenerations["x"] = 1
	e.recoveryTimers["x"] = time.AfterFunc(time.Hour, func() {})
	e.recoveryMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.Run(ctx)
		close(done)
	}()
	time.Sleep(1300 * time.Millisecond)
	e.Trigger()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
	if e.isRecovering("x") {
		t.Error("Run's deferred cancelAllRecoveries should clear tracked recoveries")
	}
}

func TestContinue95ScheduleTimerStaleGeneration(t *testing.T) {
	e := Continue95ProbeEngine(events.New(100))
	e.recoveryMu.Lock()
	e.recovering["stale-gen"] = true
	e.recoveryGenerations["stale-gen"] = 5
	e.recoveryMu.Unlock()
	e.scheduleRecovery(context.Background(), recoveryTask{id: "stale-gen", attempt: 1, generation: 5}, 50*time.Millisecond)
	// Invalidate the generation before the timer fires: the callback
	// must bail out without queueing or clearing the newer generation.
	e.recoveryMu.Lock()
	e.recoveryGenerations["stale-gen"] = 99
	e.recoveryMu.Unlock()
	time.Sleep(150 * time.Millisecond)
	if !e.isRecovering("stale-gen") {
		t.Error("stale-generation timer must not clear the newer generation")
	}
	if len(e.recoveryQueue) != 0 {
		t.Error("stale-generation timer must not queue the task")
	}
	e.cancelAllRecoveries()
}

func TestContinue95AcquireProbeLimitClamp(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Concurrency = 0
	e := New(cfg, nil, nil, nil, events.New(10))
	if !e.acquireProbe(context.Background()) {
		t.Fatal("acquireProbe with Concurrency 0 should clamp the limit to 1 and succeed")
	}
	e.releaseProbe()
	if got := e.Stats().ActiveProbes; got != 0 {
		t.Errorf("ActiveProbes after release = %d, want 0", got)
	}
}

func TestContinue95RunOnceAcquireCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}))
	defer srv.Close()
	e := Continue95ProbeHarness(t, srv, func(cfg *config.Config) {
		cfg.Probe.Concurrency = 1
	})
	e.limitMu.Lock()
	e.activeProbes = 1
	e.limitMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() {
		done <- e.RunOnce(ctx)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case res := <-done:
		if res.Canceled < 1 {
			t.Errorf("RunOnce acquire-canceled result = %+v, want Canceled >= 1", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunOnce did not return after context cancel")
	}
	e.limitMu.Lock()
	e.activeProbes = 0
	e.limitMu.Unlock()
}

func TestContinue95RunOnceCanceledProbe(t *testing.T) {
	// Bounded wait: the handler exits on client cancel, or after 2s so
	// httptest Close can never wedge on an idle keep-alive connection.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()
	e := Continue95ProbeHarness(t, srv, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	res := e.RunOnce(ctx)
	if res.Canceled < 1 {
		t.Errorf("RunOnce canceled result = %+v, want Canceled >= 1", res)
	}
}
