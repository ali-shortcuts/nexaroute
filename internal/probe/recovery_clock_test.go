package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/clock"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func clockRecoveryFixture(t *testing.T, handler http.HandlerFunc, attempts int) (*Engine, *health.Manager, *router.Router, *clock.Fake, *atomic.Int32, context.Context, context.CancelFunc) {
	t.Helper()
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(up.Close)

	cfg := config.Default()
	cfg.Routing.Strategy = "ready_queue"
	cfg.Routing.CooldownSeconds = 1800
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = true
	cfg.Probe.Concurrency = 8
	cfg.Probe.TimeoutMS = 1000
	cfg.Probe.RecoveryAttempts = attempts
	cfg.Probe.RecoveryRetryMS = 0
	cfg.Probe.CapabilityProbes = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Priority: 0, Weight: 1, Capabilities: config.Capabilities{Streaming: true}}},
	}}
	cfg.ApplyDefaults()

	clk := clock.NewFake(time.Unix(1_700_000_000, 0).UTC())
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	hm.SetNow(clk.Now)
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	e := New(cfg, reg, rt, hm, events.New(100))
	e.SetClock(clk)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return e, hm, rt, clk, &calls, ctx, cancel
}

func TestRecoveryThreeAttemptsReturnToPoolWithFakeClock(t *testing.T) {
	var n atomic.Int32
	e, hm, rt, _, _, ctx, cancel := clockRecoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) <= 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"temporary"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}, 5)
	defer cancel()

	res := e.Prime(ctx)
	if res.Passed != 0 || res.Failed != 1 {
		t.Fatalf("prime=%+v", res)
	}
	st := waitForState(t, hm, "p/m", health.Healthy, 2*time.Second)
	if st.RecoveryFailures != 0 {
		t.Fatalf("recovery failures not reset: %+v", st)
	}
	got := rt.Candidates(router.Requirement{Model: "auto", Streaming: true})
	if len(got) != 1 || got[0].Deployment.ID != "p/m" {
		t.Fatalf("not eligible after 3rd recovery success: %#v", got)
	}
}

func TestRecoveryFiveFailuresEnterThirtyMinuteCooldownThenReenter(t *testing.T) {
	e, hm, rt, clk, calls, ctx, cancel := clockRecoveryFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"down"}`))
	}, 5)
	defer cancel()

	res := e.Prime(ctx)
	if res.Failed != 1 {
		t.Fatalf("prime=%+v", res)
	}
	st := waitForState(t, hm, "p/m", health.Cooldown, 2*time.Second)
	if st.RecoveryFailures != 5 {
		t.Fatalf("recovery failures=%d want 5: %+v", st.RecoveryFailures, st)
	}
	if got := rt.Candidates(router.Requirement{Model: "auto", Streaming: true}); len(got) != 0 {
		t.Fatalf("cooldown must not be routable: %#v", got)
	}
	before := calls.Load()
	clk.Advance(30*time.Minute + time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if calls.Load() > before {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() <= before {
		t.Fatalf("deployment did not become eligible for recovery after 30-minute advance; calls=%d before=%d", calls.Load(), before)
	}
}
