package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func recoveryFixture(t *testing.T, handler http.HandlerFunc, recoveryAttempts, cooldownSeconds int) (*Engine, *health.Manager, *router.Router, *atomic.Int32, context.CancelFunc) {
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
	return e, hm, rt, &calls, cancel
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
	e, hm, rt, _, cancel := recoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
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

	res := e.Prime(context.Background())
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

func TestSupervisorFiveFailuresEnterCooldownWithoutSixthFailure(t *testing.T) {
	e, hm, rt, calls, cancel := recoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"down"}`))
	}, 5, 30)
	defer cancel()

	res := e.Prime(context.Background())
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
