package autopilot

import (
	"math"
	"strings"
	"testing"
)

func TestMission95PolicyRefStringWithoutVersion(t *testing.T) {
	if got := (PolicyRef{ID: "balanced"}).String(); got != "balanced" {
		t.Fatalf("String()=%q want %q", got, "balanced")
	}
	if got := (PolicyRef{ID: "balanced", Version: "v2"}).String(); got != "balanced@v2" {
		t.Fatalf("String()=%q want %q", got, "balanced@v2")
	}
}

// TestMission95WindowObserveErrorClasses covers every error-class counter
// plus failover, budget-anomaly and TTFT accumulation in one window snapshot.
func TestMission95WindowObserveErrorClasses(t *testing.T) {
	var w window
	classes := []struct {
		class ErrorClass
		check func(Signals) int64
	}{
		{ClassRateLimit, func(s Signals) int64 { return s.RateLimits }},
		{ClassAuthError, func(s Signals) int64 { return s.AuthErrors }},
		{ClassTimeout, func(s Signals) int64 { return s.Timeouts }},
		{ClassTransportError, func(s Signals) int64 { return s.TransportErrors }},
	}
	for _, tc := range classes {
		w.observe(Sample{Success: false, ErrorClass: tc.class})
	}
	w.observe(Sample{Success: true, Failover: true})
	w.observe(Sample{Success: true, HasBudgetAnomalySignal: true, BudgetAnomaly: true})
	w.observe(Sample{Success: true, HasTTFT: true, TTFTMS: 12.5, LatencyMS: 20})
	snap := w.snapshot()
	if snap.Requests != 7 || snap.Errors != 4 {
		t.Fatalf("requests=%d errors=%d want 7/4", snap.Requests, snap.Errors)
	}
	for _, tc := range classes {
		if got := tc.check(snap); got != 1 {
			t.Fatalf("class %s count=%d want 1", tc.class, got)
		}
	}
	if snap.Failovers != 1 {
		t.Fatalf("failovers=%d want 1", snap.Failovers)
	}
	if snap.BudgetAnomalies != 1 || snap.BudgetAnomaliesObserved != 1 {
		t.Fatalf("budget anomalies=%d observed=%d want 1/1", snap.BudgetAnomalies, snap.BudgetAnomaliesObserved)
	}
	if snap.TTFTCount != 1 || snap.TTFTP95MS != 12.5 {
		t.Fatalf("ttft count=%d p95=%v want 1/12.5", snap.TTFTCount, snap.TTFTP95MS)
	}
	if snap.LatencyP50MS != 20 || snap.LatencyP95MS != 20 {
		t.Fatalf("latency p50/p95=%v/%v want 20/20", snap.LatencyP50MS, snap.LatencyP95MS)
	}
}

func TestMission95AppendBoundedDropsOldest(t *testing.T) {
	var dst []float64
	for i := 0; i < maxLatencySamples+2; i++ {
		dst = appendBounded(dst, float64(i))
	}
	if len(dst) != maxLatencySamples {
		t.Fatalf("len=%d want %d", len(dst), maxLatencySamples)
	}
	if dst[0] != 2 {
		t.Fatalf("oldest=%v want 2 (first two samples evicted)", dst[0])
	}
	if dst[len(dst)-1] != float64(maxLatencySamples+1) {
		t.Fatalf("newest=%v want %v", dst[len(dst)-1], float64(maxLatencySamples+1))
	}
}

func TestMission95PercentileExtremes(t *testing.T) {
	samples := []float64{3, 1, 2}
	if got := percentile(samples, 0); got != 1 {
		t.Fatalf("percentile(p=0)=%v want 1", got)
	}
	if got := percentile(samples, -0.5); got != 1 {
		t.Fatalf("percentile(p<0)=%v want minimum 1", got)
	}
	if got := percentile(samples, 1); got != 3 {
		t.Fatalf("percentile(p=1)=%v want 3", got)
	}
	if got := percentile(samples, 1.5); got != 3 {
		t.Fatalf("percentile(p>1)=%v want maximum 3", got)
	}
	if got := percentile(nil, 0.5); got != 0 {
		t.Fatalf("percentile(empty)=%v want 0", got)
	}
	// NaN is not <= 0 and not >= 1, so the nearest-rank index computation
	// yields an out-of-range index and the defensive clamp applies.
	if got := percentile(samples, math.NaN()); got != 1 {
		t.Fatalf("percentile(NaN)=%v want minimum 1", got)
	}
}

func TestMission95GateConfigDefaultsRaiseAndClamp(t *testing.T) {
	// Zero config falls back to the documented defaults.
	res := EvaluateGate(Signals{Requests: 10, Successes: 10}, Signals{}, GateConfig{})
	if res.Verdict != VerdictHold || res.Reasons[0] != "insufficient_samples:10<20" {
		t.Fatalf("zero config verdict=%s reasons=%v want hold insufficient_samples:10<20", res.Verdict, res.Reasons)
	}
	// MinPromoteSamples below MinSamples is raised to MinSamples, and
	// negative anomaly/latency caps are clamped to disabled (0).
	cfg := GateConfig{
		MinSamples:         10,
		MinPromoteSamples:  3,
		MaxErrorRate:       0.9,
		MaxErrorRateDelta:  0.9,
		MaxFailoverRate:    0.9,
		MaxNoEligibleRate:  0.9,
		MaxBudgetAnomalies: -5,
		MaxLatencyP95MS:    -1,
		MaxTTFTP95MS:       -2,
	}
	good := Signals{
		Requests: 10, Successes: 10, ErrorRate: 0,
		NoEligibleObserved: 10, BudgetAnomaliesObserved: 10,
		LatencyP95MS: 99999, TTFTP95MS: 99999, TTFTCount: 3,
	}
	res = EvaluateGate(good, Signals{}, cfg)
	if res.Verdict != VerdictPromote {
		t.Fatalf("clamped config verdict=%s reasons=%v want promote", res.Verdict, res.Reasons)
	}
}

func TestMission95GateViolations(t *testing.T) {
	cfg := GateConfig{
		MinSamples: 10, MinPromoteSamples: 10,
		MaxErrorRate: 0.9, MaxErrorRateDelta: 0.9,
		MaxFailoverRate: 0.5, MaxNoEligibleRate: 0.9,
		MaxBudgetAnomalies: 0, MaxLatencyP95MS: 100, MaxTTFTP95MS: 50,
	}
	cases := []struct {
		name   string
		sig    Signals
		prefix string
	}{
		{"failover", Signals{Requests: 10, FailoverRate: 0.6, NoEligibleObserved: 10, BudgetAnomaliesObserved: 10}, "failover_rate:"},
		{"budget", Signals{Requests: 10, BudgetAnomaliesObserved: 10, BudgetAnomalies: 2, NoEligibleObserved: 10}, "budget_anomalies:"},
		{"latency", Signals{Requests: 10, LatencyP95MS: 150, NoEligibleObserved: 10, BudgetAnomaliesObserved: 10}, "latency_p95_ms:"},
		{"ttft", Signals{Requests: 10, TTFTP95MS: 75, TTFTCount: 2, NoEligibleObserved: 10, BudgetAnomaliesObserved: 10}, "ttft_p95_ms:"},
	}
	for _, tc := range cases {
		res := EvaluateGate(tc.sig, Signals{}, cfg)
		if res.Verdict != VerdictRollback {
			t.Fatalf("%s: verdict=%s reasons=%v want rollback", tc.name, res.Verdict, res.Reasons)
		}
		found := false
		for _, r := range res.Reasons {
			if strings.HasPrefix(r, tc.prefix) {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: reasons=%v want a reason with prefix %q", tc.name, res.Reasons, tc.prefix)
		}
	}
	// Full sample count but partial budget-anomaly signal coverage holds
	// promotion even though no violation fired.
	partial := Signals{Requests: 10, Successes: 10, NoEligibleObserved: 10, BudgetAnomaliesObserved: 4}
	res := EvaluateGate(partial, Signals{}, cfg)
	if res.Verdict != VerdictHold || !strings.HasPrefix(res.Reasons[0], "insufficient_budget_anomaly_signal_coverage:") {
		t.Fatalf("partial budget coverage verdict=%s reasons=%v want hold with budget coverage reason", res.Verdict, res.Reasons)
	}
}

func TestMission95StartCanaryValidationAndClamps(t *testing.T) {
	c := newTestController(GateConfig{MinSamples: 1, MinPromoteSamples: 1})
	if err := c.StartCanary(PolicyRef{}, 50, 0); err == nil {
		t.Fatal("empty candidate id must be rejected")
	}
	if err := c.StartCanary(PolicyRef{ID: "balanced", Version: "v-neg"}, 150, -7); err != nil {
		t.Fatalf("StartCanary err=%v", err)
	}
	st := c.Status()
	if st.CanaryPercent != 100 {
		t.Fatalf("canary percent=%v want 100 (clamped from 150)", st.CanaryPercent)
	}
	if st.CanaryMaxRequests != 0 {
		t.Fatalf("canary max requests=%d want 0 (negative clamped)", st.CanaryMaxRequests)
	}
	if err := c.StartCanary(PolicyRef{ID: "balanced", Version: "v-neg2"}, -25, 0); err != nil {
		t.Fatalf("StartCanary err=%v", err)
	}
	if got := c.Status().CanaryPercent; got != 0 {
		t.Fatalf("canary percent=%v want 0 (clamped from -25)", got)
	}
}

func TestMission95ShouldCanarySelectionPaths(t *testing.T) {
	c := newTestController(GateConfig{MinSamples: 1, MinPromoteSamples: 1})
	if err := c.StartCanary(PolicyRef{ID: "balanced", Version: "v2"}, 0, 0); err != nil {
		t.Fatalf("StartCanary err=%v", err)
	}
	if l := c.Select("any-key"); l.Canary {
		t.Fatal("0%% canary must never serve the candidate")
	}
	// Empty request key falls back to the deterministic counter share.
	if err := c.StartCanary(PolicyRef{ID: "balanced", Version: "v3"}, 50, 0); err != nil {
		t.Fatalf("StartCanary err=%v", err)
	}
	first := c.Select("")
	if !first.Canary {
		t.Fatal("first empty-key select at 50%% must hit the counter share (served=0)")
	}
	// Hashed keys are deterministic: the same key selects the same side.
	if err := c.StartCanary(PolicyRef{ID: "balanced", Version: "v4"}, 50, 0); err != nil {
		t.Fatalf("StartCanary err=%v", err)
	}
	a := c.Select("stable-key")
	b := c.Select("stable-key")
	if a.Canary != b.Canary {
		t.Fatalf("same key selected inconsistently: %v vs %v", a.Canary, b.Canary)
	}
}

func TestMission95PromoteOutsideCanaryIsNoop(t *testing.T) {
	c := newTestController(GateConfig{})
	if _, ok := c.Promote(); ok {
		t.Fatal("Promote without a canary must return false")
	}
}

func TestMission95IncidentHistoryIsBounded(t *testing.T) {
	c := newTestController(GateConfig{MinSamples: 1, MinPromoteSamples: 1})
	for i := 0; i < maxHistory+5; i++ {
		if err := c.StartCanary(PolicyRef{ID: "balanced", Version: "v-roll"}, 100, 0); err != nil {
			t.Fatalf("iteration %d StartCanary err=%v", i, err)
		}
		if _, ok := c.Rollback("test_rollback"); !ok {
			t.Fatalf("iteration %d Rollback returned false", i)
		}
	}
	st := c.Status()
	if len(st.Incidents) != maxHistory {
		t.Fatalf("incidents=%d want bounded %d", len(st.Incidents), maxHistory)
	}
	if st.Incidents[0].ID == "inc-1" {
		t.Fatal("oldest incident must have been evicted once the bound is exceeded")
	}
	if st.Incidents[len(st.Incidents)-1].ID != "inc-69" {
		t.Fatalf("newest incident=%q want inc-69", st.Incidents[len(st.Incidents)-1].ID)
	}
}
