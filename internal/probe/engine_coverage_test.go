package probe

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/compat"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

type engineCoverageAdapter struct {
	mu       sync.Mutex
	calls    int
	payloads [][]byte
	streams  []bool
	forward  []http.Header
	response func(context.Context, []byte, bool, http.Header) (*http.Response, error)
}

func (a *engineCoverageAdapter) ID() string   { return "fake" }
func (a *engineCoverageAdapter) Kind() string { return "fake" }
func (a *engineCoverageAdapter) Stats() providers.ProviderStats {
	return providers.ProviderStats{ID: "fake"}
}
func (a *engineCoverageAdapter) CredentialsMatch([]string) bool { return false }
func (a *engineCoverageAdapter) RedactBody(b []byte) []byte {
	return []byte(strings.ReplaceAll(string(b), "secret", "[REDACTED]"))
}
func (a *engineCoverageAdapter) Do(ctx context.Context, p []byte, s bool, h http.Header) (*http.Response, error) {
	a.mu.Lock()
	a.calls++
	a.payloads = append(a.payloads, append([]byte(nil), p...))
	a.streams = append(a.streams, s)
	a.forward = append(a.forward, h.Clone())
	a.mu.Unlock()
	if a.response != nil {
		return a.response(ctx, p, s, h)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
}
func (a *engineCoverageAdapter) DoPath(ctx context.Context, method, path string, p []byte, s bool, h http.Header) (*http.Response, error) {
	return a.Do(ctx, p, s, h)
}
func (a *engineCoverageAdapter) CountTokens(ctx context.Context, p []byte, h http.Header) (*http.Response, error) {
	return a.Do(ctx, p, false, h)
}
func (a *engineCoverageAdapter) Probe(context.Context, string, int) (time.Duration, int, error) {
	return time.Millisecond, http.StatusOK, nil
}

func TestEngineCoverageAdapterTransportForwardsAndRedacts(t *testing.T) {
	fake := &engineCoverageAdapter{}
	payload := []byte(`{"prompt":"secret"}`)
	h := http.Header{"X-Trace": []string{"trace-1"}}
	transport := adapterTransport{a: fake}
	resp, err := transport.Do(context.Background(), payload, true, h)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "ok" {
		t.Fatalf("transport response=%q err=%v", body, err)
	}
	if got := string(fake.payloads[0]); got != string(payload) || !fake.streams[0] || fake.forward[0].Get("X-Trace") != "trace-1" {
		t.Fatalf("adapter did not receive original request: payload=%q stream=%v headers=%v", got, fake.streams[0], fake.forward[0])
	}
	if got := string(transport.RedactBody([]byte("secret token"))); got != "[REDACTED] token" {
		t.Fatalf("redaction=%q", got)
	}
}

func TestEngineCoverageCapabilityStoreGatesAndLearnsVerdicts(t *testing.T) {
	fake := &engineCoverageAdapter{response: func(_ context.Context, payload []byte, stream bool, _ http.Header) (*http.Response, error) {
		if strings.Contains(string(payload), `"temperature"`) {
			return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"temperature is not supported by this model"}}`))}, nil
		}
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
	e := New(cfg, nil, nil, health.New(3, time.Minute), events.New(40))
	d := router.Deployment{ID: "p/m", ProviderType: "openai_compatible", Model: "m"}
	ctx := context.Background()
	e.maybeProbeCapabilities(ctx, d, fake) // nil store is deliberately a no-op
	if fake.calls != 0 {
		t.Fatalf("capability probe ran without a store: %d calls", fake.calls)
	}
	store := compat.NewStore()
	e.SetCapabilityStore(store)
	cfg.Probe.CapabilityProbes = false
	e.Reload(cfg)
	e.maybeProbeCapabilities(ctx, d, fake)
	if fake.calls != 0 {
		t.Fatalf("disabled capability probes sent %d calls", fake.calls)
	}
	cfg.Probe.CapabilityProbes = true
	e.Reload(cfg)
	e.maybeProbeCapabilities(ctx, d, fake)
	contract := store.Get(d.ID)
	if contract.Capabilities.Text != compat.Supported {
		t.Fatalf("successful basic capability was not persisted: %+v", contract.Capabilities)
	}
	if contract.Capabilities.Temperature != compat.Unsupported {
		t.Fatalf("classified unsupported capability was not persisted: %+v", contract.Capabilities)
	}
	if ev := store.Get(d.ID).Evidence[compat.CapText]; ev.Source != compat.SourceProbe || ev.Confidence != compat.ConfidenceHigh {
		t.Fatalf("probe evidence provenance/confidence incorrect: %+v", ev)
	}
	calls := fake.calls
	if calls < 10 {
		t.Fatalf("capability suite exercised only %d requests", calls)
	}
	e.maybeProbeCapabilities(ctx, d, fake)
	if fake.calls != calls {
		t.Fatalf("cached text verdict failed to suppress repeat suite: before=%d after=%d", calls, fake.calls)
	}
	foundEvent := false
	for _, ev := range e.bus.Snapshot() {
		if ev.Kind == "capability_probe" && ev.Deployment == d.ID && strings.Contains(ev.Message, "failed=") {
			foundEvent = true
		}
	}
	if !foundEvent {
		t.Fatal("capability suite did not publish its aggregate event")
	}
}

func coverageFixture(t *testing.T, status int, body string, strategy string, modelIDs ...string) (*Engine, *health.Manager, *providers.Registry, *events.Bus) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Routing.Strategy = strategy
	cfg.Routing.CooldownSeconds = 300
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Probe.CapabilityProbes = false
	cfg.Probe.Concurrency = 2
	cfg.Probe.TimeoutMS = 1000
	pc := config.ProviderConfig{ID: "p", Name: "p", Type: "openai_compatible", BaseURL: up.URL, AuthMode: "none", Enabled: true}
	for _, id := range modelIDs {
		pc.Models = append(pc.Models, config.ModelConfig{ID: id, Model: id, Enabled: true, Weight: 1})
	}
	cfg.Providers = []config.ProviderConfig{pc}
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	bus := events.New(80)
	return New(cfg, reg, rt, hm, bus), hm, reg, bus
}

func TestEngineCoverageSweepOrderingSkipsAndStaleReadyLease(t *testing.T) {
	good := `{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`
	e, hm, _, _ := coverageFixture(t, http.StatusOK, good, "ready_queue", "fresh", "stale", "unknown", "cool", "degraded", "retired")
	now := time.Now()
	hm.RecordSuccess("p/fresh", time.Millisecond)
	freshChecked := hm.Get("p/fresh").LastChecked
	hm.SetNowFunc(func() time.Time { return now.Add(-time.Hour) })
	hm.RecordSuccess("p/stale", time.Millisecond)
	hm.SetNowFunc(time.Now)
	hm.ForceCooldown("p/cool", "quota", time.Minute)
	hm.Quarantine("p/degraded", "temporary", time.Millisecond)
	hm.Retire("p/retired", "removed", "operator")
	res := e.runOnce(context.Background(), false)
	if res.Total != 6 || res.Passed != 2 || res.Failed != 0 || res.SkippedReady != 1 || res.SkippedCooldown != 1 || res.SkippedRecovery != 1 || res.SkippedRetired != 1 {
		t.Fatalf("ready sweep accounting/state decisions mismatch: %+v", res)
	}
	if hm.Get("p/stale").Status != health.Healthy || hm.Get("p/unknown").Status != health.Healthy {
		t.Fatalf("probed models were not marked healthy: stale=%+v unknown=%+v", hm.Get("p/stale"), hm.Get("p/unknown"))
	}
	if !hm.Get("p/fresh").LastChecked.Equal(freshChecked) {
		t.Fatalf("fresh ready model was unexpectedly refreshed: %+v", hm.Get("p/fresh"))
	}
}

func TestEngineCoverageMissingAdapterAndDisabledSweep(t *testing.T) {
	good := `{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`
	e, _, reg, _ := coverageFixture(t, http.StatusOK, good, "priority", "m")
	cfg := e.current()
	cfg.Providers = nil
	if err := reg.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	res := e.RunOnce(context.Background())
	if res.Total != 1 || res.SkippedMissing != 1 || res.Passed != 0 {
		t.Fatalf("missing adapter was not accounted for: %+v", res)
	}
	cfg.Probe.Enabled = false
	e.Reload(cfg)
	res = e.runOnce(context.Background(), false)
	if res != (Result{}) {
		t.Fatalf("disabled background sweep should return an empty result, got %+v", res)
	}
}

func TestEngineCoverageProbeErrorsDistinguishCooldownAndOrdinaryFailure(t *testing.T) {
	for _, tc := range []struct {
		name, strategy, body string
		status               int
		wantCooldown         bool
	}{
		{"429-capped", "adaptive", `{"error":{"message":"rate limited"}}`, http.StatusTooManyRequests, true},
		{"ordinary-400", "adaptive", `{"error":{"message":"bad request"}}`, http.StatusBadRequest, false},
		{"ready-quarantine", "ready_queue", `{"error":{"message":"upstream unavailable"}}`, http.StatusServiceUnavailable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, hm, _, bus := coverageFixture(t, tc.status, tc.body, tc.strategy, "m")
			res := e.RunOnce(context.Background())
			if res.Total != 1 || res.Failed != 1 || res.Passed != 0 {
				t.Fatalf("probe error result=%+v", res)
			}
			st := hm.Get("p/m")
			if tc.wantCooldown {
				if st.Status != health.Cooldown || time.Until(st.CooldownUntil) > time.Minute+time.Second {
					t.Fatalf("429 did not enter capped cooldown: %+v remaining=%s", st, time.Until(st.CooldownUntil))
				}
			} else if tc.strategy == "ready_queue" {
				if st.Status != health.Degraded {
					t.Fatalf("ready-queue probe failure was not quarantined: %+v", st)
				}
			} else if st.Status == health.Cooldown {
				t.Fatalf("ordinary client error incorrectly forced cooldown: %+v", st)
			}
			kinds := map[string]bool{}
			for _, ev := range bus.Snapshot() {
				kinds[ev.Kind] = true
			}
			if !kinds["model_failed"] {
				t.Fatalf("failure event missing: %v", kinds)
			}
			if tc.status == http.StatusTooManyRequests && !kinds["provider_rate_limited"] {
				t.Fatalf("rate-limit event missing: %v", kinds)
			}
			if tc.strategy == "ready_queue" && !kinds["probe_quarantine"] {
				t.Fatalf("quarantine event missing: %v", kinds)
			}
		})
	}
}

func TestEngineCoverageReloadWakesProbeLimitWaiterAndRunCancellation(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Concurrency = 1
	cfg.Probe.Enabled = false
	e := New(cfg, nil, nil, health.New(3, time.Minute), events.New(10))
	if !e.acquireProbe(context.Background()) {
		t.Fatal("could not acquire initial concurrency slot")
	}
	waiter := make(chan bool, 1)
	go func() { waiter <- e.acquireProbe(context.Background()) }()
	cfg.Probe.Concurrency = 2
	e.Reload(cfg)
	select {
	case ok := <-waiter:
		if !ok {
			t.Fatal("raised live concurrency did not release waiting probe")
		}
	case <-time.After(time.Second):
		t.Fatal("probe limit waiter was not woken by reload")
	}
	if got := e.Stats().ActiveProbes; got != 2 {
		t.Fatalf("active probes=%d want 2", got)
	}
	e.releaseProbe()
	e.releaseProbe()
	e.releaseProbe() // excess release must not underflow
	if e.Stats().ActiveProbes != 0 {
		t.Fatalf("active probe count did not return to zero: %+v", e.Stats())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.Run(ctx)
	if e.context() != ctx {
		t.Fatal("Run did not publish its run context")
	}
	if e.wasPrimed() {
		t.Fatal("Run unexpectedly marked engine primed")
	}
}

func TestEngineCoverageRecoveryQueueHelpersAndProviderIncidentClassification(t *testing.T) {
	cfg := config.Default()
	e := New(cfg, nil, nil, health.New(3, time.Minute), events.New(10))
	if e.enqueueRecovery(nil, recoveryTask{id: "x"}) {
		t.Fatal("nil context accepted recovery work")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if e.enqueueRecovery(canceled, recoveryTask{id: "x"}) {
		t.Fatal("canceled context accepted recovery work")
	}
	ctx := context.Background()
	task := recoveryTask{id: "x", attempt: 1, generation: 7}
	e.recoveryMu.Lock()
	e.recovering[task.id] = true
	e.recoveryGenerations[task.id] = task.generation
	e.recoveryMu.Unlock()
	e.scheduleRecovery(ctx, task, -time.Millisecond)
	select {
	case got := <-e.recoveryQueue:
		if got != task {
			t.Fatalf("scheduled task=%+v want %+v", got, task)
		}
	default:
		t.Fatal("non-positive retry delay did not enqueue current recovery")
	}
	stale := recoveryTask{id: "stale", generation: 99}
	e.scheduleRecovery(ctx, stale, 0)
	if e.isRecovering(stale.id) {
		t.Fatal("stale recovery task left a tracking marker")
	}
	for _, tc := range []struct {
		status int
		want   bool
	}{{0, true}, {401, true}, {402, true}, {403, true}, {408, true}, {429, true}, {500, true}, {499, false}, {200, false}} {
		if got := providerIncidentProbeFailure(tc.status); got != tc.want {
			t.Errorf("providerIncidentProbeFailure(%d)=%v want %v", tc.status, got, tc.want)
		}
	}
}

func TestEngineCoveragePrimeSetsPrimedAndUsesManualSweep(t *testing.T) {
	good := `{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`
	e, _, _, _ := coverageFixture(t, http.StatusOK, good, "ready_queue", "m")
	ctx := context.Background()
	res := e.Prime(ctx)
	if res.Total != 1 || res.Passed != 1 || !e.wasPrimed() || e.context() != ctx {
		t.Fatalf("Prime result/context incorrect: result=%+v primed=%v", res, e.wasPrimed())
	}
}

func TestEngineCoverageRecoveryTaskStateTransitions(t *testing.T) {
	good := `{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`
	t.Run("canceled and legacy strategies clear tracking", func(t *testing.T) {
		e, _, _, _ := coverageFixture(t, http.StatusOK, good, "ready_queue", "m")
		task := recoveryTask{id: "p/m", generation: 1}
		e.recovering[task.id] = true
		e.recoveryGenerations[task.id] = task.generation
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		e.processRecoveryTask(ctx, task)
		if e.isRecovering(task.id) {
			t.Fatal("canceled recovery retained tracking")
		}
		e.recovering[task.id], e.recoveryGenerations[task.id] = true, task.generation
		legacy := e.current()
		legacy.Routing.Strategy = "adaptive"
		e.Reload(legacy)
		e.processRecoveryTask(context.Background(), task)
		if e.isRecovering(task.id) {
			t.Fatal("legacy strategy retained ready-queue recovery")
		}
	})
	t.Run("missing retired and healthy targets are discarded", func(t *testing.T) {
		e, hm, _, _ := coverageFixture(t, http.StatusOK, good, "ready_queue", "m", "already-healthy")
		for _, id := range []string{"absent", "p/m"} {
			task := recoveryTask{id: id, generation: 4}
			e.recovering[id], e.recoveryGenerations[id] = true, task.generation
			if id == "p/m" {
				hm.Retire(id, "removed", "operator")
			}
			e.processRecoveryTask(context.Background(), task)
			if e.isRecovering(id) {
				t.Fatalf("discarded recovery for %q left marker", id)
			}
		}
		hm.RecordSuccess("p/already-healthy", time.Millisecond)
		task := recoveryTask{id: "p/already-healthy", generation: 5}
		e.recovering[task.id], e.recoveryGenerations[task.id] = true, task.generation
		e.processRecoveryTask(context.Background(), task)
		if e.isRecovering(task.id) {
			t.Fatal("already healthy target was not cleared")
		}
	})
	t.Run("future cooldown schedules a deferred retry", func(t *testing.T) {
		e, hm, _, bus := coverageFixture(t, http.StatusOK, good, "ready_queue", "m")
		hm.ForceCooldown("p/m", "provider quota", time.Hour)
		task := recoveryTask{id: "p/m", attempt: 3, generation: 6}
		e.recovering[task.id], e.recoveryGenerations[task.id] = true, task.generation
		e.processRecoveryTask(context.Background(), task)
		e.recoveryMu.Lock()
		timer := e.recoveryTimers[task.id]
		e.recoveryMu.Unlock()
		if timer == nil || !e.isRecovering(task.id) {
			t.Fatal("future cooldown did not retain a timed recovery")
		}
		found := false
		for _, ev := range bus.Snapshot() {
			if ev.Kind == "recovery_wait" && ev.Deployment == task.id {
				found = true
			}
		}
		if !found {
			t.Fatal("cooldown deferral event missing")
		}
		e.cancelAllRecoveries()
	})
	t.Run("successful task revalidates and clears", func(t *testing.T) {
		e, hm, _, bus := coverageFixture(t, http.StatusOK, good, "ready_queue", "m")
		task := recoveryTask{id: "p/m", attempt: 1, generation: 8}
		e.recovering[task.id], e.recoveryGenerations[task.id] = true, task.generation
		e.processRecoveryTask(context.Background(), task)
		if hm.Get(task.id).Status != health.Healthy || e.isRecovering(task.id) {
			t.Fatalf("successful recovery did not restore health/clear marker: state=%+v tracked=%v", hm.Get(task.id), e.isRecovering(task.id))
		}
		kinds := map[string]bool{}
		for _, ev := range bus.Snapshot() {
			kinds[ev.Kind] = true
		}
		if !kinds["recovery_ready"] || !kinds["model_recovered"] {
			t.Fatalf("recovery success events missing: %v", kinds)
		}
	})
}

func TestEngineCoverageRecoveryGuardsQueueFullAndTimers(t *testing.T) {
	cfg := config.Default()
	e := New(cfg, nil, nil, health.New(3, time.Minute), events.New(20))
	e.Recover("  ") // whitespace-normalized empty ID is ignored
	e.Recover("no-context")
	if e.Stats().RecoveryTracked != 0 {
		t.Fatalf("recovery started without a run context: %+v", e.Stats())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.setRunContext(ctx)
	e.hm.Retire("retired", "removed", "operator")
	e.Recover("retired")
	if e.Stats().RecoveryTracked != 0 {
		t.Fatalf("retired deployment was queued: %+v", e.Stats())
	}
	// Disable worker startup for this deterministic queue-saturation test.
	e.recoveryWorkers.Do(func() {})
	for i := 0; i < cap(e.recoveryQueue); i++ {
		e.recoveryQueue <- recoveryTask{id: "filler"}
	}
	e.Recover("overflow")
	if e.isRecovering("overflow") {
		t.Fatal("queue-full recovery retained a false in-flight marker")
	}
	fullEvent := false
	for _, ev := range e.bus.Snapshot() {
		if ev.Kind == "recovery_queue_full" && ev.Deployment == "overflow" {
			fullEvent = true
		}
	}
	if !fullEvent {
		t.Fatal("queue saturation was not observable")
	}
	e.recoveryQueue = make(chan recoveryTask, maxRecoveryQueue)
	task := recoveryTask{id: "timer", generation: 99, attempt: 1}
	e.recovering[task.id], e.recoveryGenerations[task.id] = true, task.generation
	e.scheduleRecovery(ctx, task, time.Hour)
	e.recoveryMu.Lock()
	timer := e.recoveryTimers[task.id]
	e.recoveryMu.Unlock()
	if timer == nil {
		t.Fatal("positive retry delay did not install a timer")
	}
	e.cancelAllRecoveries()
}

func TestEngineCoverageTriggeredRunAndCanceledSweepCapacity(t *testing.T) {
	good := `{"id":"c","choices":[{"message":{"role":"assistant","content":"OK"}}]}`
	e, _, _, bus := coverageFixture(t, http.StatusOK, good, "priority", "m")
	_, eventCh, unsubscribe, ok := bus.SubscribeSnapshot(20)
	if !ok {
		t.Fatal("could not subscribe to probe events")
	}
	defer unsubscribe()
	e.Trigger()
	ctx, cancel := context.WithCancel(context.Background())
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
			cancel()
			<-runDone
			t.Fatal("triggered Run did not execute a probe sweep")
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

	// A full limiter plus a canceled caller must classify all jobs as canceled,
	// rather than recording a synthetic provider failure.
	e2, hm2, _, _ := coverageFixture(t, http.StatusOK, good, "priority", "m")
	e2.limitMu.Lock()
	e2.activeProbes = e2.current().Probe.Concurrency
	e2.limitMu.Unlock()
	canceled, stop := context.WithCancel(context.Background())
	stop()
	res := e2.runOnce(canceled, true)
	if res.Canceled != 1 || res.Failed != 0 || hm2.Get("p/m").Status != health.Unknown {
		t.Fatalf("canceled capacity wait changed result/health: result=%+v health=%+v", res, hm2.Get("p/m"))
	}
}
