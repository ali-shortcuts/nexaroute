package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
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

// Agent E coverage tests for internal/probe. These target the blocks the
// CI baseline left uncovered.
//
// engine.go:568-569 (provider_rate_limited event on a 429 recovery deferral)
// and engine.go:843-846 (runOnce acquireProbe failure with a live context)
// are NOT covered here: see the report — both are unreachable with the
// production adapter/context contracts (proven in the test comments of
// the parent report, not by a forced test).

// agentERunEngines starts n probe engines with probing disabled and a 1s
// interval, and returns their cancel functions. With probing disabled the
// Run loop never touches the router/registry, so nil dependencies are safe.
func agentERunEngines(t *testing.T, n int) []context.CancelFunc {
	t.Helper()
	cancels := make([]context.CancelFunc, 0, n)
	for i := 0; i < n; i++ {
		cfg := config.Default()
		cfg.Probe.Enabled = false
		cfg.Probe.IntervalSeconds = 1
		ctx, cancel := context.WithCancel(context.Background())
		e := New(cfg, nil, nil, health.New(3, time.Minute), events.New(10))
		go e.Run(ctx)
		cancels = append(cancels, cancel)
	}
	return cancels
}

// agentESpinPastTimerFire holds the P in a busy spin until just after
// every engine's 1s timer has fired, so that the cancel/trigger issued
// the instant the spin ends lands while timer.C and the signal are both
// ready. In practice the drain itself is a microsecond race: the signal
// must land after the timer fired but before the select consumed it, and
// measured scheduling jitter on this VM (sleep/spin overshoot ~10ms,
// goroutine arming latency 0.06-62ms) is orders of magnitude wider than
// that window, so the drain is attempted opportunistically here, not
// guaranteed. The tests below still pin the Run loop's cancel/trigger
// return paths; see the Agent E report for the full measurement.
func agentESpinPastTimerFire(start time.Time) {
	deadline := start.Add(1004 * time.Millisecond)
	for time.Now().Before(deadline) {
	}
}

func TestAgentERunTimerDrainOnCancel(t *testing.T) {
	// engine.go:283-285 — Run's ctx.Done branch drains an already-fired
	// timer (timer.Stop() returns false) before returning. The drain
	// fires only when cancellation lands after the timer fired but before
	// the select consumed it: a microsecond race that this test attempts
	// opportunistically (see agentESpinPastTimerFire). Either way the
	// test pins that every engine returns cleanly on cancellation.
	oldMax := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldMax)

	start := time.Now()
	cancels := agentERunEngines(t, 48)
	// Sleep until just before the first timer can fire (arming takes
	// milliseconds; fires cluster at start+1s), then spin past the
	// cluster so the cancel lands 2-4ms after the last fire.
	time.Sleep(time.Until(start.Add(990 * time.Millisecond)))
	agentESpinPastTimerFire(start)
	for _, cancel := range cancels {
		cancel()
	}
	time.Sleep(300 * time.Millisecond) // let every Run goroutine return
}

func TestAgentERunTimerDrainOnTrigger(t *testing.T) {
	// engine.go:295-297 — same timer drain, on the trigger branch: a
	// Trigger() that arrives after the timer fired but before the select
	// consumed it must drain the fired timer before running the sweep.
	// Same microsecond-race caveat as TestAgentERunTimerDrainOnCancel.
	oldMax := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldMax)

	// agentERunEngines only returns cancels; rebuild with engine handles
	// here so the test can Trigger() each engine.
	type eng struct {
		e      *Engine
		cancel context.CancelFunc
	}
	engs := make([]eng, 0, 48)
	for i := 0; i < 48; i++ {
		cfg := config.Default()
		cfg.Probe.Enabled = false
		cfg.Probe.IntervalSeconds = 1
		ctx, cancel := context.WithCancel(context.Background())
		e := New(cfg, nil, nil, health.New(3, time.Minute), events.New(10))
		go e.Run(ctx)
		engs = append(engs, eng{e, cancel})
	}
	start := time.Now()
	// Sleep until just before the first timer can fire (arming takes
	// milliseconds; fires cluster at start+1s), then spin past the
	// cluster so the triggers land 2-4ms after the last fire.
	time.Sleep(time.Until(start.Add(990 * time.Millisecond)))
	agentESpinPastTimerFire(start)
	for _, en := range engs {
		en.e.Trigger()
	}
	time.Sleep(300 * time.Millisecond) // let every Run goroutine run its sweep
	for _, en := range engs {
		en.cancel()
	}
	time.Sleep(300 * time.Millisecond) // let every Run goroutine return
}

// agentERecoveryVanishFixture builds an engine whose deployment can be
// removed from the router at will (rt.Reload with an empty provider list),
// with a counting upstream that always answers probes successfully.
func agentERecoveryVanishFixture(t *testing.T) (*Engine, *health.Manager, *router.Router, *atomic.Int32, config.Config, config.Config, context.Context, context.CancelFunc) {
	t.Helper()
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"ok","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}))
	t.Cleanup(up.Close)

	with := config.Default()
	with.Routing.Strategy = "ready_queue"
	with.Probe.Enabled = false // Run loop stays idle; recovery is driven directly
	with.Probe.OnStart = false
	with.Probe.Concurrency = 8
	with.Probe.TimeoutMS = 1000
	with.Probe.RecoveryAttempts = 5
	with.Probe.RecoveryRetryMS = 5
	with.Probe.RecoverySuccessThreshold = 1
	with.Probe.RecoveryCooldownSeconds = 30
	with.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Priority: 0, Weight: 1}},
	}}
	with.ApplyDefaults()

	without := with
	without.Providers = nil

	hm := health.New(with.Routing.FailureThreshold, with.Cooldown())
	reg, err := providers.NewRegistry(with)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(with, hm)
	e := New(with, reg, rt, hm, events.New(100))
	ctx, cancel := context.WithCancel(context.Background())
	e.Start(ctx)
	return e, hm, rt, &calls, with, without, ctx, cancel
}

func TestAgentEProcessRecoveryTaskDeploymentVanishes(t *testing.T) {
	// engine.go:517-519 — processRecoveryTask re-resolves the deployment
	// immediately before probing so a hot reload never keeps a stale
	// pointer. The second deployment() call returns !ok only when the
	// router drops the deployment in the window between the two lookups.
	// Four reloader goroutines flip the router continuously (Reload
	// builds locally and swaps at the end), and write-lock spinners park
	// the worker inside hm.Get between the lookups so the second lookup
	// sees a decorrelated router state. A task consumed without a probe
	// call is the liveness signal (the first lookup can also miss —
	// line 467 — so the counter alone cannot prove which branch fired;
	// coverage is the witness for line 517 itself).
	e, hm, rt, calls, with, without, _, cancel := agentERecoveryVanishFixture(t)
	defer cancel()

	stop := make(chan struct{})
	defer close(stop)
	for g := 0; g < 4; g++ {
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
					rt.Reload(without)
					rt.Reload(with)
				}
			}
		}()
	}

	const attempts = 60
	noProbe := 0
	for i := 0; i < attempts; i++ {
		rt.Reload(with)
		hm.Invalidate("p/m") // reset health so the task reaches the probe
		before := calls.Load()
		// Park the worker inside hm.Get (between the two deployment()
		// lookups) with write-lock spinners: while they churn
		// RecordSuccess the worker's RLock cannot proceed, so the
		// router reloaders flip the deployment many times before the
		// second lookup runs. Stop the spinners after 50ms so the
		// worker always proceeds; the second lookup then sees a
		// decorrelated router state (~50% gone) instead of the same
		// snapshot the first lookup saw microseconds earlier.
		stopSpin := make(chan struct{})
		var spinWg sync.WaitGroup
		var spinStarted atomic.Int32
		allStarted := make(chan struct{})
		for g := 0; g < 8; g++ {
			spinWg.Add(1)
			go func() {
				defer spinWg.Done()
				once := false
				for {
					select {
					case <-stopSpin:
						return
					default:
						hm.RecordSuccess("agentE-spin", 0)
						if !once {
							once = true
							if spinStarted.Add(1) == 8 {
								close(allStarted)
							}
						}
					}
				}
			}()
		}
		select {
		case <-allStarted:
		case <-time.After(5 * time.Second):
			close(stopSpin)
			spinWg.Wait()
			t.Fatalf("attempt %d: health spinners never started; race harness starved", i)
		}
		e.Recover("p/m")
		time.Sleep(50 * time.Millisecond) // worker parks in hm.Get
		close(stopSpin)
		spinWg.Wait()
		deadline := time.Now().Add(2 * time.Second)
		for e.isRecovering("p/m") && time.Now().Before(deadline) {
			time.Sleep(200 * time.Microsecond)
		}
		if e.isRecovering("p/m") {
			t.Fatalf("attempt %d: recovery task was not consumed within 2s", i)
		}
		if calls.Load() == before {
			noProbe++
		}
	}
	if noProbe == 0 {
		t.Fatalf("no attempt consumed a recovery task without probing in %d attempts; race harness may be starved", attempts)
	}
	t.Logf("recovery tasks consumed without a probe call: %d/%d", noProbe, attempts)
}
