package probe

// Tests for the Phase 3 recovery policy (issue #395):
//   - threshold recovery (default 3 of 5) and strict N-of-N;
//   - five failed probes -> cooldown -> automatic recovery;
//   - failure classification (auth / quota / rate-limit / transport / incident);
//   - disabled deployments are never probed;
//   - hot reload adopts a new success threshold;
//   - duplicate recovery requests do not amplify probes.

import (
	"context"
	"encoding/json"
	"errors"
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

func okBody() string {
	return `{"id":"ok","choices":[{"message":{"role":"assistant","content":"OK"}}]}`
}

// thresholdFixture builds a ready-strategy engine whose upstream is the given
// handler. attempts/threshold/cooldownSeconds are applied verbatim (no
// clamping) so the test states the policy it exercises.
func thresholdFixture(t *testing.T, handler http.HandlerFunc, attempts, threshold, cooldownSeconds int) (*Engine, *health.Manager, *router.Router, *atomic.Int32, context.Context, context.CancelFunc) {
	t.Helper()
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(up.Close)

	cfg := config.Default()
	cfg.Routing.Strategy = "ready_queue"
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = true
	cfg.Probe.Concurrency = 8
	cfg.Probe.TimeoutMS = 1000
	cfg.Probe.RecoveryAttempts = attempts
	cfg.Probe.RecoveryRetryMS = 5
	cfg.Probe.RecoverySuccessThreshold = threshold
	cfg.Probe.RecoveryCooldownSeconds = cooldownSeconds
	cfg.Probe.CapabilityProbes = false
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
	e := New(cfg, reg, rt, hm, events.New(200))
	ctx, cancel := context.WithCancel(context.Background())
	return e, hm, rt, &calls, ctx, cancel
}

// TestRecoveryThresholdThreeOfFiveReAdmits verifies that the default-threshold
// policy re-admits once the configured number of successful probes is reached,
// even when a transient failure is interleaved.
func TestRecoveryThresholdThreeOfFiveReAdmits(t *testing.T) {
	var recoveryCall atomic.Int32
	e, hm, rt, calls, ctx, cancel := thresholdFixture(t, func(w http.ResponseWriter, r *http.Request) {
		// Initial health check (call 1) fails. Recovery probes: success,
		// success, failure, success -> 3 successes within 4 probes.
		if recoveryCall.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"temporary"}`))
			return
		}
		n := recoveryCall.Load()
		if n == 4 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"blip"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody()))
	}, 5, 3, 1800)
	defer cancel()

	if res := e.Prime(ctx); res.Failed != 1 {
		t.Fatalf("prime=%+v want one initial failure", res)
	}
	st := waitForState(t, hm, "p/m", health.Healthy, 3*time.Second)
	if st.RecoverySuccesses != 0 {
		t.Fatalf("re-admission must reset the recovery success count: %+v", st)
	}
	sawRecoveryFailure := false
	for _, ev := range e.bus.Snapshot() {
		if ev.Kind == "recovery_fail" && ev.Deployment == "p/m" {
			sawRecoveryFailure = true
		}
	}
	if !sawRecoveryFailure {
		t.Fatal("interleaved failure was not recorded as a recovery_fail event")
	}
	if n := calls.Load(); n != 5 {
		t.Fatalf("upstream calls=%d want 1 initial + 4 recovery probes", n)
	}
	if got := rt.Candidates(router.Requirement{Model: "auto", Streaming: true}); len(got) != 1 || got[0].Deployment.ID != "p/m" {
		t.Fatalf("threshold-recovered deployment not routable: %#v", got)
	}
}

// TestRecoveryThresholdNotFoundReEntersCooldown verifies that a cycle which
// never reaches the threshold ends in cooldown after exactly the configured
// number of probes (no sixth-hand failure).
func TestRecoveryThresholdNotFoundReEntersCooldown(t *testing.T) {
	e, hm, _, calls, ctx, cancel := thresholdFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"down"}`))
	}, 5, 3, 1800)
	defer cancel()

	if res := e.Prime(ctx); res.Failed != 1 {
		t.Fatalf("prime=%+v want one failure", res)
	}
	st := waitForState(t, hm, "p/m", health.Cooldown, 3*time.Second)
	if st.RecoveryFailures != 5 {
		t.Fatalf("recovery failures=%d want exactly 5", st.RecoveryFailures)
	}
	if n := calls.Load(); n != 6 {
		t.Fatalf("upstream calls=%d want 1 initial + 5 recovery probes", n)
	}
	if st.NextRetryAt.IsZero() {
		t.Fatalf("cooldown state must expose a next retry time: %+v", st)
	}
}

// TestRecoveryStrictFiveOfFiveRequiresEverySuccess verifies the strict N-of-N
// escape hatch: a single failed probe in the cycle prevents re-admission.
func TestRecoveryStrictFiveOfFiveRequiresEverySuccess(t *testing.T) {
	var n atomic.Int32
	e, hm, _, calls, ctx, cancel := thresholdFixture(t, func(w http.ResponseWriter, r *http.Request) {
		// Call 1 fails (prime). Recovery calls 2..6: one failure at call 4
		// means the strict 5-of-5 threshold is unreachable.
		c := n.Add(1)
		if c == 1 || c == 4 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody()))
	}, 5, 5, 1800)
	defer cancel()

	if res := e.Prime(ctx); res.Failed != 1 {
		t.Fatalf("prime=%+v want one failure", res)
	}
	st := waitForState(t, hm, "p/m", health.Cooldown, 3*time.Second)
	if st.RecoveryFailures != 1 {
		t.Fatalf("strict cycle recovery failures=%d want 1", st.RecoveryFailures)
	}
	if n := calls.Load(); n != 6 {
		t.Fatalf("upstream calls=%d want 1 initial + 5 recovery probes", n)
	}
}

// TestRecoveryFailedCycleEntersCooldownThenAutomaticallyRecovers drives the
// full five-failed-probes -> cooldown -> automatic recovery path with a
// one-second cooldown so the deterministic timer can be observed.
func TestRecoveryFailedCycleEntersCooldownThenAutomaticallyRecovers(t *testing.T) {
	var n atomic.Int32
	e, hm, _, calls, ctx, cancel := thresholdFixture(t, func(w http.ResponseWriter, r *http.Request) {
		// Calls 1..6 fail (initial + 5 recovery probes). After the cooldown
		// the upstream is healthy again and the next cycle recovers.
		if n.Add(1) <= 6 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody()))
	}, 5, 3, 1)
	defer cancel()

	if res := e.Prime(ctx); res.Failed != 1 {
		t.Fatalf("prime=%+v want one failure", res)
	}
	if st := waitForState(t, hm, "p/m", health.Cooldown, 3*time.Second); st.RecoveryFailures != 5 {
		t.Fatalf("want 5 recovery failures before cooldown: %+v", st)
	}
	// Automatic recovery: no explicit Recover call, no process restart.
	st := waitForState(t, hm, "p/m", health.Healthy, 5*time.Second)
	if st.RecoveryFailures != 0 || st.RecoverySuccesses != 0 {
		t.Fatalf("recovered state must clear counters: %+v", st)
	}
	// 1 initial + 5 failed probes + 3 successful probes to reach threshold.
	if got := calls.Load(); got != 9 {
		t.Fatalf("upstream calls=%d want 9 (1 + 5 + 3)", got)
	}
}

// TestClassifyProbeFailure pins the deterministic failure taxonomy.
func TestClassifyProbeFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		err    error
		want   probeFailureClass
	}{
		{"unauthorized", http.StatusUnauthorized, errors.New("401"), classCredentialFailure},
		{"forbidden", http.StatusForbidden, errors.New("403"), classCredentialFailure},
		{"payment_required", http.StatusPaymentRequired, errors.New("402"), classQuotaExhausted},
		{"too_many_requests", http.StatusTooManyRequests, errors.New("429"), classRateLimited},
		{"server_error", http.StatusInternalServerError, errors.New("500"), classProviderIncident},
		{"timeout_status", http.StatusRequestTimeout, errors.New("408"), classTransientTimeout},
		{"deadline", 0, context.DeadlineExceeded, classTransientTimeout},
		{"transport", 0, errors.New("connection reset"), classProviderIncident},
		{"bad_request", http.StatusBadRequest, errors.New("400"), classTransientTransport},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyProbeFailure(tc.status, tc.err); got != tc.want {
				t.Fatalf("classify(%d, %v)=%s want %s", tc.status, tc.err, got, tc.want)
			}
		})
	}
}

// TestProbeFailureClassRecordedOnState verifies the classification is exposed
// on deployment state and on events without leaking secrets.
func TestProbeFailureClassRecordedOnState(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Routing.Strategy = "ready_queue"
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Probe.CapabilityProbes = false
	cfg.Probe.TimeoutMS = 1000
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m", Model: "m", Enabled: true, Weight: 1}},
	}}
	cfg.ApplyDefaults()

	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	bus := events.New(50)
	e := New(cfg, reg, rt, hm, bus)
	res := e.RunOnce(context.Background())
	if res.Failed != 1 {
		t.Fatalf("sweep=%+v want failure", res)
	}
	if got := hm.Get("p/m").LastErrorClass; got != string(classCredentialFailure) {
		t.Fatalf("last_error_class=%q want %q", got, classCredentialFailure)
	}
	found := false
	for _, ev := range bus.Snapshot() {
		if ev.Deployment == "p/m" && ev.ErrorType == string(classCredentialFailure) {
			found = true
		}
	}
	if !found {
		t.Fatalf("no event carried error_type=%s", classCredentialFailure)
	}
}

// TestProbeSkipsDisabledDeployments verifies disabled models are never probed.
func TestProbeSkipsDisabledDeployments(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody()))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Routing.Strategy = "ready_queue"
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Probe.CapabilityProbes = false
	cfg.Probe.TimeoutMS = 1000
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{
			{ID: "live", Model: "live", Enabled: true, Weight: 1},
			{ID: "off", Model: "off", Enabled: false, Weight: 1},
		},
	}}
	cfg.ApplyDefaults()

	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	if n := len(rt.All()); n != 1 {
		t.Fatalf("router has %d deployments; disabled model must be excluded", n)
	}
	e := New(cfg, reg, rt, hm, events.New(20))
	res := e.RunOnce(context.Background())
	if res.Total != 1 || res.Passed != 1 {
		t.Fatalf("sweep=%+v want only the enabled deployment probed", res)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls=%d want 1 (disabled deployment must not be probed)", got)
	}
}

// TestSingleFailingModelDoesNotDisableSiblingsOrProvider verifies that a probe
// failure quarantines only the failing deployment and never ejects the whole
// provider without corroborating evidence.
func TestSingleFailingModelDoesNotDisableSiblingsOrProvider(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if model, _ := body["model"].(string); model == "bad" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody()))
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Routing.Strategy = "ready_queue"
	cfg.Probe.Enabled = true
	cfg.Probe.OnStart = false
	cfg.Probe.CapabilityProbes = false
	cfg.Probe.TimeoutMS = 1000
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: up.URL,
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{
			{ID: "bad", Model: "bad", Enabled: true, Weight: 1},
			{ID: "good", Model: "good", Enabled: true, Weight: 1},
		},
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
	e.setRunContext(ctx)

	res := e.RunOnce(ctx)
	if res.Passed != 1 || res.Failed != 1 {
		t.Fatalf("sweep=%+v want one pass and one fail", res)
	}
	if st := hm.Get("p/good"); st.Status != health.Healthy {
		t.Fatalf("unrelated sibling impaired by a peer failure: %+v", st)
	}
	if st := hm.Get("p/bad"); st.Status == health.Healthy {
		t.Fatalf("failing deployment was not quarantined: %+v", st)
	}
	if !hm.ProviderAvailable("p") {
		t.Fatal("provider ejected on a single deployment failure without evidence")
	}
}

// TestRecoveryHotReloadAdoptsThreshold verifies that lowering the success
// threshold on a live reload takes effect for the in-flight recovery cycle.
func TestRecoveryHotReloadAdoptsThreshold(t *testing.T) {
	var n atomic.Int32
	e, hm, _, calls, ctx, cancel := thresholdFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody()))
	}, 5, 5, 1800)
	defer cancel()
	// Slow the spacing so the reload lands deterministically between probes.
	slow := func() config.Config {
		c := e.current()
		c.Probe.RecoveryRetryMS = 150
		c.Probe.RecoverySuccessThreshold = 5
		return c
	}
	e.Reload(slow())

	if res := e.Prime(ctx); res.Failed != 1 {
		t.Fatalf("prime=%+v want one failure", res)
	}
	// Wait until at least one recovery probe succeeded under strict 5-of-5.
	deadline := time.Now().Add(3 * time.Second)
	for hm.Get("p/m").RecoverySuccesses < 1 {

		if time.Now().After(deadline) {
			t.Fatalf("no partial recovery progress observed: %+v", hm.Get("p/m"))
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Hot reload lowers the threshold; the next successful probe re-admits.
	relaxed := e.current()
	relaxed.Probe.RecoverySuccessThreshold = 1
	e.Reload(relaxed)

	st := waitForState(t, hm, "p/m", health.Healthy, 3*time.Second)
	if st.RecoverySuccesses != 0 {
		t.Fatalf("recovered state kept recovery successes: %+v", st)
	}
	if got := calls.Load(); got > 4 {
		t.Fatalf("hot reload did not take effect; upstream calls=%d", got)
	}
}

// TestRecoveryDuplicateRequestsDoNotAmplifyProbes verifies that repeated
// Recover calls for the same deployment during one cycle are coalesced.
func TestRecoveryDuplicateRequestsDoNotAmplifyProbes(t *testing.T) {
	var n atomic.Int32
	e, hm, _, calls, ctx, cancel := thresholdFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okBody()))
	}, 5, 3, 1800)
	defer cancel()

	if res := e.Prime(ctx); res.Failed != 1 {
		t.Fatalf("prime=%+v want one failure", res)
	}
	for i := 0; i < 50; i++ {
		e.Recover("p/m")
	}
	waitForState(t, hm, "p/m", health.Healthy, 3*time.Second)
	// 1 initial + 3 successful probes; duplicate Recover calls add no probes.
	if got := calls.Load(); got != 4 {
		t.Fatalf("upstream calls=%d want 4; duplicate Recover calls amplified probes", got)
	}
}
