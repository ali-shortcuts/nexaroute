package probe

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// Classification and bounded-default branches.

func TestIssue663ProvidersClassifyAndDefaults(t *testing.T) {
	if got := classifyProbeFailure(http.StatusNotFound, context.DeadlineExceeded); got != classTransientTimeout {
		t.Fatalf("404 + deadline = %q, want transient_timeout", got)
	}
	cfg := config.Default()
	cfg.Probe.RecoveryAttempts = 0
	if got := recoveryAttempts(cfg); got != 5 {
		t.Fatalf("zero recovery attempts = %d, want default 5", got)
	}
	cfg.Probe.RecoveryAttempts = -3
	if got := recoveryAttempts(cfg); got != 5 {
		t.Fatalf("negative recovery attempts = %d, want default 5", got)
	}
	cfg.Probe.RecoveryAttempts = 3
	if got := recoveryAttempts(cfg); got != 3 {
		t.Fatalf("explicit recovery attempts = %d, want 3", got)
	}
}

// startRecoveryWorkers must ignore a nil or already-canceled context without
// consuming the sync.Once that gates worker startup.
func TestIssue663ProvidersStartRecoveryWorkersGuards(t *testing.T) {
	e, _, _, bus := coverageFixture(t, http.StatusOK, `{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`, "priority", "m")
	e.startRecoveryWorkers(nil)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	e.startRecoveryWorkers(canceled)

	// The engine must still run normally after the guarded calls.
	_, eventCh, unsubscribe, ok := bus.SubscribeSnapshot(20)
	if !ok {
		t.Fatal("could not subscribe to probe events")
	}
	defer unsubscribe()
	e.Trigger()
	ctx, stop := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		e.Run(ctx)
		close(runDone)
	}()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	seenReady := false
	for !seenReady {
		select {
		case <-deadline.C:
			stop()
			<-runDone
			t.Fatal("triggered Run did not execute a probe sweep after guarded worker starts")
		case ev := <-eventCh:
			seenReady = ev.Kind == "probe_ready" && ev.Deployment == "p/m"
		}
	}
	stop()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after context cancellation")
	}
}

// Run clamps a sub-second probe interval to one second and fires the timer
// branch (no Trigger needed).
func TestIssue663ProvidersRunIntervalClampAndTimerFire(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Routing.Strategy = "priority"
	cfg.Routing.CooldownSeconds = 300
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Probe.Concurrency = 2
	cfg.Probe.TimeoutMS = 1000
	cfg.Probe.RecoverySuccessThreshold = 1
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "p", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	cfg.ApplyDefaults()
	// ApplyDefaults would default a zero interval to 120s; set it after so
	// ProbeInterval() is 0 and Run must clamp it to one second.
	cfg.Probe.IntervalSeconds = 0
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	bus := events.New(80)
	e := New(cfg, reg, router.New(cfg, hm), hm, bus)
	_, eventCh, unsubscribe, ok := bus.SubscribeSnapshot(20)
	if !ok {
		t.Fatal("could not subscribe to probe events")
	}
	defer unsubscribe()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	runDone := make(chan struct{})
	go func() {
		e.Run(ctx)
		close(runDone)
	}()
	seenReady := false
	for !seenReady {
		select {
		case <-ctx.Done():
			t.Fatal("timer-fired Run did not execute a probe sweep within the timeout")
		case ev := <-eventCh:
			seenReady = ev.Kind == "probe_ready" && ev.Deployment == "p/m"
		}
	}
	cancel()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after context cancellation")
	}
}

func TestIssue663ProvidersScheduleRecoveryQueueFull(t *testing.T) {
	e, _, _, bus := coverageFixture(t, http.StatusOK, `{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`, "priority", "m")
	// Fill the bounded recovery queue with stale tasks.
	for e.enqueueRecovery(context.Background(), recoveryTask{id: "filler"}) {
	}
	e.recoveryMu.Lock()
	e.recovering["p/m"] = true
	e.recoveryGenerations["p/m"] = 7
	e.recoveryMu.Unlock()
	e.scheduleRecovery(context.Background(), recoveryTask{id: "p/m", attempt: 1, generation: 7}, 0)
	if e.isCurrentRecovery(recoveryTask{id: "p/m", generation: 7}) {
		t.Fatal("queue-full recovery task was not cleared")
	}
	found := false
	for _, ev := range bus.Snapshot() {
		if ev.Kind == "recovery_queue_full" && ev.Deployment == "p/m" {
			found = true
		}
	}
	if !found {
		t.Fatal("queue-full schedule did not publish a recovery_queue_full event")
	}
}

func TestIssue663ProvidersScheduleRecoveryTimers(t *testing.T) {
	e, _, _, _ := coverageFixture(t, http.StatusOK, `{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`, "priority", "m")
	e.recoveryMu.Lock()
	e.recovering["p/m"] = true
	e.recoveryGenerations["p/m"] = 11
	e.recoveryMu.Unlock()
	task := recoveryTask{id: "p/m", attempt: 1, generation: 11}

	// Rescheduling stops the previous timer and issues a fresh token.
	e.scheduleRecovery(context.Background(), task, 30*time.Second)
	e.recoveryMu.Lock()
	firstToken := e.recoveryTokens["p/m"]
	e.recoveryMu.Unlock()
	e.scheduleRecovery(context.Background(), task, 30*time.Second)
	e.recoveryMu.Lock()
	secondToken := e.recoveryTokens["p/m"]
	e.recoveryMu.Unlock()
	if firstToken == 0 || firstToken == secondToken {
		t.Fatalf("reschedule did not rotate the recovery token: %d -> %d", firstToken, secondToken)
	}
	e.clearRecoveryTask(task)

	// A stale timer callback (superseded token) returns without clearing:
	// schedule a 5ms timer, supersede it, wait past the first fire time,
	// and the task must still be live under the second timer.
	e.recoveryMu.Lock()
	e.recovering["p/m"] = true
	e.recoveryGenerations["p/m"] = 12
	e.recoveryMu.Unlock()
	staleTask := recoveryTask{id: "p/m", attempt: 1, generation: 12}
	e.scheduleRecovery(context.Background(), staleTask, 5*time.Millisecond)
	e.scheduleRecovery(context.Background(), staleTask, 30*time.Second)
	time.Sleep(100 * time.Millisecond)
	if !e.isCurrentRecovery(staleTask) {
		t.Fatal("stale timer callback cleared a live recovery task")
	}
	e.clearRecoveryTask(staleTask)

	// A timer callback with a canceled context clears the task.
	ctx, cancel := context.WithCancel(context.Background())
	e.recoveryMu.Lock()
	e.recovering["p/m"] = true
	e.recoveryGenerations["p/m"] = 13
	e.recoveryMu.Unlock()
	canceledTask := recoveryTask{id: "p/m", attempt: 1, generation: 13}
	e.scheduleRecovery(ctx, canceledTask, 5*time.Millisecond)
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.isCurrentRecovery(canceledTask) {
		time.Sleep(5 * time.Millisecond)
	}
	if e.isCurrentRecovery(canceledTask) {
		t.Fatal("canceled-context timer callback did not clear the recovery task")
	}
}

// Rescheduling a task that is not a current recovery clears it without
// creating a timer, and a timer callback that loses the token race to a
// reschedule returns without clearing the live task.
func TestIssue663ProvidersScheduleRecoveryGuards(t *testing.T) {
	e, _, _, _ := coverageFixture(t, http.StatusOK, `{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`, "priority", "m")
	stale := recoveryTask{id: "p/m", attempt: 1, generation: 99}
	e.scheduleRecovery(context.Background(), stale, 30*time.Second)
	e.recoveryMu.Lock()
	_, hasTimer := e.recoveryTimers["p/m"]
	e.recoveryMu.Unlock()
	if hasTimer {
		t.Fatal("non-current reschedule created a recovery timer")
	}

	e.recoveryMu.Lock()
	e.recovering["p/m"] = true
	e.recoveryGenerations["p/m"] = 31
	e.recoveryMu.Unlock()
	task := recoveryTask{id: "p/m", attempt: 1, generation: 31}
	e.scheduleRecovery(context.Background(), task, 5*time.Millisecond)
	// Block the timer callback behind a reschedule that is already queued
	// on the recovery mutex, so the callback observes the rotated token.
	e.recoveryMu.Lock()
	rescheduled := make(chan struct{})
	go func() {
		e.scheduleRecovery(context.Background(), task, 30*time.Second)
		close(rescheduled)
	}()
	time.Sleep(50 * time.Millisecond) // timer fires; callback queues behind reschedule
	e.recoveryMu.Unlock()
	select {
	case <-rescheduled:
	case <-time.After(2 * time.Second):
		t.Fatal("queued reschedule did not complete")
	}
	if !e.isCurrentRecovery(task) {
		t.Fatal("stale timer callback cleared a live recovery task")
	}
	e.clearRecoveryTask(task)
}

func TestIssue663ProvidersScheduleRecoveryQueueFullCallback(t *testing.T) {
	e, _, _, bus := coverageFixture(t, http.StatusOK, `{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`, "priority", "m")
	for e.enqueueRecovery(context.Background(), recoveryTask{id: "filler"}) {
	}
	e.recoveryMu.Lock()
	e.recovering["p/m"] = true
	e.recoveryGenerations["p/m"] = 21
	e.recoveryMu.Unlock()
	task := recoveryTask{id: "p/m", attempt: 1, generation: 21}
	e.scheduleRecovery(context.Background(), task, 5*time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.isCurrentRecovery(task) {
		time.Sleep(5 * time.Millisecond)
	}
	if e.isCurrentRecovery(task) {
		t.Fatal("queue-full timer callback did not clear the recovery task")
	}
	found := false
	for _, ev := range bus.Snapshot() {
		if ev.Kind == "recovery_queue_full" && ev.Deployment == "p/m" {
			found = true
		}
	}
	if !found {
		t.Fatal("queue-full timer callback did not publish a recovery_queue_full event")
	}
}

// The recovery worker skips a stale queued task and then processes a current
// task for a missing deployment (deployment lookup failure path).
func TestIssue663ProvidersRecoveryWorkerStaleAndMissing(t *testing.T) {
	e, _, _, _, ctx, cancel := recoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}, 5, 60)
	defer cancel()
	go e.recoveryWorker(ctx)
	e.recoveryQueue <- recoveryTask{id: "ghost", generation: 99}
	e.recoveryMu.Lock()
	e.recovering["nope/x"] = true
	e.recoveryGenerations["nope/x"] = 3
	e.recoveryMu.Unlock()
	missing := recoveryTask{id: "nope/x", attempt: 1, generation: 3}
	e.recoveryQueue <- missing
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.isCurrentRecovery(missing) {
		time.Sleep(5 * time.Millisecond)
	}
	if e.isCurrentRecovery(missing) {
		t.Fatal("recovery worker did not clear the missing-deployment task")
	}
}

// processRecoveryTask clears the task when probe acquisition fails because
// the caller cancels while all probe slots are occupied.
func TestIssue663ProvidersProcessRecoveryAcquireProbeCanceled(t *testing.T) {
	e, _, _, _, _, cancel := recoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}, 5, 60)
	defer cancel()
	e.limitMu.Lock()
	e.activeProbes = e.current().Probe.Concurrency
	e.limitMu.Unlock()
	e.recoveryMu.Lock()
	e.recovering["p/m"] = true
	e.recoveryGenerations["p/m"] = 4
	e.recoveryMu.Unlock()
	task := recoveryTask{id: "p/m", attempt: 1, generation: 4}
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.processRecoveryTask(ctx, task)
		close(done)
	}()
	// Give the task a moment to block inside acquireProbe, then cancel.
	time.Sleep(50 * time.Millisecond)
	stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("processRecoveryTask did not return after cancellation")
	}
	if e.isCurrentRecovery(task) {
		t.Fatal("canceled probe acquisition did not clear the recovery task")
	}
	e.limitMu.Lock()
	e.activeProbes = 0
	e.limitMu.Unlock()
}

// A 429-cooled credential makes the next probe return a RetryAfter error;
// processRecoveryTask clamps the wait to MaxRetryAfterSeconds and defers.
func TestIssue663ProvidersProcessRecoveryRetryAfterClamp(t *testing.T) {
	t.Setenv("ISSUE663_PROBE_KEY", "dummy-test-key")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Routing.Strategy = "ready_queue"
	cfg.Routing.CooldownSeconds = 60
	cfg.Routing.MaxRetryAfterSeconds = 5
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Probe.Concurrency = 8
	cfg.Probe.TimeoutMS = 1000
	cfg.Probe.RecoveryAttempts = 5
	cfg.Probe.RecoveryRetryMS = 5
	cfg.Probe.RecoverySuccessThreshold = 1
	cfg.Probe.RecoveryCooldownSeconds = 60
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "bearer", APIKeyEnv: "ISSUE663_PROBE_KEY", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	bus := events.New(100)
	e := New(cfg, reg, router.New(cfg, hm), hm, bus)
	adapter, ok := reg.Get("p")
	if !ok {
		t.Fatal("registry has no adapter for p")
	}
	// First probe cools the only credential down with a 60s retry-after.
	if _, _, err := adapter.Probe(context.Background(), "m", 1); err == nil {
		t.Fatal("expected the 429 probe to fail")
	}
	e.recoveryMu.Lock()
	e.recovering["p/m"] = true
	e.recoveryGenerations["p/m"] = 6
	e.recoveryMu.Unlock()
	task := recoveryTask{id: "p/m", attempt: 1, generation: 6}
	e.processRecoveryTask(context.Background(), task)
	next := hm.Get("p/m").NextRetryAt
	if next.IsZero() {
		t.Fatal("retry-after recovery did not set a next retry time")
	}
	wait := time.Until(next)
	if wait < 3*time.Second || wait > 7*time.Second {
		t.Fatalf("clamped retry wait = %v, want ~5s (MaxRetryAfterSeconds)", wait)
	}
	found := false
	for _, ev := range bus.Snapshot() {
		if ev.Kind == "recovery_deferred" && ev.Deployment == "p/m" && strings.Contains(ev.Message, "retry after") {
			found = true
		}
	}
	if !found {
		t.Fatal("retry-after recovery did not publish a clamped recovery_deferred event")
	}
	e.clearRecoveryTask(task)
}

// acquireProbe treats a configured concurrency below one as one.
func TestIssue663ProvidersAcquireProbeLimitFloor(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Concurrency = 0
	e := New(cfg, nil, nil, health.New(3, time.Minute), events.New(10))
	if !e.acquireProbe(context.Background()) {
		t.Fatal("acquireProbe with zero configured concurrency did not acquire")
	}
	e.limitMu.Lock()
	active := e.activeProbes
	e.limitMu.Unlock()
	if active != 1 {
		t.Fatalf("active probes = %d, want 1 after floor acquisition", active)
	}
	e.releaseProbe()
}

// Capability suites run for each provider dialect, and a Supported verdict
// for an already-Unsupported capability is deliberately not re-learned.
func TestIssue663ProvidersCapabilitySuiteDialectsAndSupportedSkip(t *testing.T) {
	fake := &engineCoverageAdapter{response: func(_ context.Context, payload []byte, stream bool, _ http.Header) (*http.Response, error) {
		h := make(http.Header)
		body := `{"choices":[{"message":{"content":"OK"}}]}`
		if stream {
			h.Set("Content-Type", "text/event-stream")
			body = "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n"
		}
		return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader(body))}, nil
	}}
	cfg := config.Default()
	cfg.Probe.CapabilityProbes = true
	for _, providerType := range []string{"anthropic_compatible", "openai_responses", "gemini"} {
		e := New(cfg, nil, nil, health.New(3, time.Minute), events.New(40))
		e.SetCapabilityStore(compat.NewStore())
		before := fake.calls
		e.maybeProbeCapabilities(context.Background(), router.Deployment{ID: "p/m", ProviderType: providerType, Model: "m"}, fake)
		if fake.calls <= before {
			t.Fatalf("capability suite for %s sent no requests", providerType)
		}
	}

	// Pre-mark Text unsupported; a fresh Supported text verdict must not flip it.
	store := compat.NewStore()
	store.LearnUnsupported("p/m", compat.CapText, compat.SourceProbe, "unsupported by operator note", "")
	e := New(cfg, nil, nil, health.New(3, time.Minute), events.New(40))
	e.SetCapabilityStore(store)
	e.maybeProbeCapabilities(context.Background(), router.Deployment{ID: "p/m", ProviderType: "openai_compatible", Model: "m"}, fake)
	if got := store.Get("p/m").Capabilities.Text; got != compat.Unsupported {
		t.Fatalf("supported verdict overwrote unsupported text capability: %v", got)
	}
}
