package autopilot

import (
	"testing"
	"time"
)

func fixedClock(t0 time.Time) func() time.Time {
	cur := t0
	return func() time.Time {
		cur = cur.Add(time.Millisecond)
		return cur
	}
}

func newTestController(cfg GateConfig) *Controller {
	c := New(
		PolicyRef{ID: "balanced", Version: "auto:incumbent"},
		PolicyRef{ID: "balanced", Version: "auto:incumbent"},
		cfg,
	)
	c.SetClock(fixedClock(time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)))
	return c
}

// ServeCandidates records n samples against the currently issued canary lease.
func serveCanarySamples(c *Controller, n int, s Sample) []Lease {
	// Unit samples are fully observed by construction. HTTP integration
	// tests that lack a source leave these coverage bits unset and fail closed.
	s.HasNoEligibleSignal = true
	s.HasBudgetAnomalySignal = true
	leases := make([]Lease, 0, n)
	for i := 0; i < n; i++ {
		l := c.Select("canary-key")
		leases = append(leases, l)
		c.Record(l, s)
	}
	return leases
}

func TestGateNeverPromotesOnMissingData(t *testing.T) {
	cfg := GateConfig{MinSamples: 10, MinPromoteSamples: 20}
	// Zero samples: no verdict but hold.
	res := EvaluateGate(Signals{}, Signals{}, cfg)
	if res.Verdict != VerdictHold {
		t.Fatalf("empty signals verdict=%s want hold", res.Verdict)
	}
	if res.Reasons[0] != "insufficient_samples:0<10" {
		t.Fatalf("reasons=%v", res.Reasons)
	}
	// Thin data below MinSamples must hold even with a perfect error rate.
	thin := Signals{Requests: 5, Successes: 5, ErrorRate: 0}
	res = EvaluateGate(thin, Signals{}, cfg)
	if res.Verdict != VerdictHold {
		t.Fatalf("thin data verdict=%s want hold", res.Verdict)
	}
	// Between MinSamples and MinPromoteSamples clean data still holds (no
	// promotion without enough evidence).
	mid := Signals{Requests: 15, Successes: 15, ErrorRate: 0}
	res = EvaluateGate(mid, Signals{}, cfg)
	if res.Verdict != VerdictHold {
		t.Fatalf("mid data verdict=%s want hold (not promote)", res.Verdict)
	}
	if res.Reasons[0] != "awaiting_promotion_samples:15<20" {
		t.Fatalf("reasons=%v", res.Reasons)
	}
}

func TestGatePromotesOnlyWhenSatisfied(t *testing.T) {
	cfg := GateConfig{MinSamples: 10, MinPromoteSamples: 20, MaxErrorRate: 0.2}
	good := Signals{Requests: 25, Successes: 24, Errors: 1, ErrorRate: 0.04, NoEligibleObserved: 25, BudgetAnomaliesObserved: 25}
	res := EvaluateGate(good, Signals{}, cfg)
	if res.Verdict != VerdictPromote {
		t.Fatalf("verdict=%s reasons=%v want promote", res.Verdict, res.Reasons)
	}
	bad := Signals{Requests: 25, Errors: 10, ErrorRate: 0.4, NoEligibleObserved: 25, BudgetAnomaliesObserved: 25}
	res = EvaluateGate(bad, Signals{}, cfg)
	if res.Verdict != VerdictRollback {
		t.Fatalf("verdict=%s reasons=%v want rollback", res.Verdict, res.Reasons)
	}
}

func TestGateRollsBackOnIncumbentRegression(t *testing.T) {
	cfg := GateConfig{MinSamples: 10, MinPromoteSamples: 20, MaxErrorRate: 0.9, MaxErrorRateDelta: 0.05}
	cand := Signals{Requests: 30, Errors: 6, ErrorRate: 0.20, NoEligibleObserved: 30, BudgetAnomaliesObserved: 30}
	inc := Signals{Requests: 30, Errors: 1, ErrorRate: 0.033, NoEligibleObserved: 30, BudgetAnomaliesObserved: 30}
	res := EvaluateGate(cand, inc, cfg)
	if res.Verdict != VerdictRollback {
		t.Fatalf("verdict=%s reasons=%v want rollback", res.Verdict, res.Reasons)
	}
}

func TestAutoRollbackOnBadCandidate(t *testing.T) {
	c := newTestController(GateConfig{MinSamples: 10, MinPromoteSamples: 100, MaxErrorRate: 0.2})
	if err := c.StartCanary(PolicyRef{ID: "balanced", Version: "auto:bad"}, 100, 0); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	if got := c.Status().State; got != StateCanary {
		t.Fatalf("state=%s want canary", got)
	}

	// The bad candidate routes most requests to a failing deployment: errors
	// dominate. The gate must trip and roll back automatically.
	serveCanarySamples(c, 15, Sample{Success: false, ErrorClass: ClassServerError, LatencyMS: 5})

	st := c.Status()
	if st.State != StateRolledBack {
		t.Fatalf("state=%s want rolled_back after gate trip", st.State)
	}
	if st.LastRollback == nil {
		t.Fatal("no incident recorded")
	}
	if st.LastRollback.BadVersion != "auto:bad" {
		t.Fatalf("incident bad_version=%q", st.LastRollback.BadVersion)
	}
	if st.LastRollback.RestoredVersion != "auto:incumbent" {
		t.Fatalf("incident restored=%q", st.LastRollback.RestoredVersion)
	}
	if len(st.Incidents) != 1 {
		t.Fatalf("incidents=%d want 1", len(st.Incidents))
	}
	if st.Candidate != nil {
		t.Fatalf("candidate still present after rollback: %+v", st.Candidate)
	}

	// After rollback every new request is served by the restored incumbent.
	l := c.Select("k")
	if l.Canary || l.Policy.Version != "auto:incumbent" {
		t.Fatalf("post-rollback lease=%+v want incumbent", l)
	}
}

// TestRollbackDoesNotSwitchInFlightRequest proves the pinning invariant: a
// lease issued before a rollback still records against (and is served by) the
// policy it started with, even after the controller restores the incumbent.
func TestRollbackDoesNotSwitchInFlightRequest(t *testing.T) {
	c := newTestController(GateConfig{MinSamples: 10, MinPromoteSamples: 100, MaxErrorRate: 0.2})
	if err := c.StartCanary(PolicyRef{ID: "balanced", Version: "auto:bad"}, 100, 0); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}

	// A request starts under the candidate.
	inFlight := c.Select("in-flight")
	if !inFlight.Canary || inFlight.Policy.Version != "auto:bad" {
		t.Fatalf("in-flight lease=%+v want candidate", inFlight)
	}
	if inFlight.Generation != 0 {
		t.Fatalf("lease generation=%d want 0", inFlight.Generation)
	}

	// Enough concurrent bad samples trip the auto-rollback.
	serveCanarySamples(c, 12, Sample{Success: false, ErrorClass: ClassServerError})
	if c.Status().State != StateRolledBack {
		t.Fatalf("expected auto rollback, state=%s", c.Status().State)
	}

	// The pre-rollback lease is unchanged: same policy, same generation.
	if inFlight.Policy.Version != "auto:bad" || inFlight.Generation != 0 {
		t.Fatalf("in-flight lease mutated by rollback: %+v", inFlight)
	}
	// Recording it still attributes the sample to the candidate generation and
	// does not resurrect the candidate lifecycle or promote it.
	before := c.Status().Generation
	c.Record(inFlight, Sample{Success: true})
	after := c.Status()
	if after.State != StateRolledBack {
		t.Fatalf("recording in-flight lease changed state to %s", after.State)
	}
	if after.Generation != before {
		t.Fatalf("recording in-flight lease changed generation %d->%d", before, after.Generation)
	}
}

func TestPromoteOnlyOnSufficientEvidence(t *testing.T) {
	c := newTestController(GateConfig{MinSamples: 5, MinPromoteSamples: 10, MaxErrorRate: 0.2})
	if err := c.StartCanary(PolicyRef{ID: "balanced", Version: "auto:good"}, 100, 0); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	// Partial clean evidence: cannot promote yet.
	serveCanarySamples(c, 6, Sample{Success: true, LatencyMS: 10})
	if _, ok := c.Promote(); ok {
		t.Fatal("promoted before MinPromoteSamples")
	}
	serveCanarySamples(c, 4, Sample{Success: true, LatencyMS: 10})
	tr, ok := c.Promote()
	if !ok {
		t.Fatalf("promote refused with sufficient clean evidence; gate=%+v", c.Evaluate())
	}
	if tr.To != StatePromoted || tr.Version != "auto:good" {
		t.Fatalf("transition=%+v", tr)
	}
	st := c.Status()
	if st.State != StatePromoted {
		t.Fatalf("state=%s", st.State)
	}
	if st.Incumbent.Version != "auto:good" {
		t.Fatalf("incumbent=%+v want promoted candidate", st.Incumbent)
	}
	// New requests now serve the promoted version.
	l := c.Select("k")
	if l.Canary || l.Policy.Version != "auto:good" {
		t.Fatalf("lease after promote=%+v", l)
	}
}

func TestCanaryShareIsBoundedByCount(t *testing.T) {
	c := newTestController(GateConfig{MinSamples: 1000, MinPromoteSamples: 2000})
	if err := c.StartCanary(PolicyRef{ID: "balanced", Version: "auto:bad"}, 100, 3); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	canary := 0
	for i := 0; i < 20; i++ {
		l := c.Select("k")
		if l.Canary {
			canary++
		}
		c.Record(l, Sample{Success: true})
	}
	if canary != 3 {
		t.Fatalf("canary leases=%d want bounded to 3", canary)
	}
}

func TestSelectNeverTargetsCandidateOutsideCanary(t *testing.T) {
	c := newTestController(DefaultGateConfig())
	c.BeginShadow(PolicyRef{ID: "balanced", Version: "auto:shadow"})
	for i := 0; i < 20; i++ {
		if l := c.Select("k"); l.Canary || l.Policy.Version != "auto:incumbent" {
			t.Fatalf("shadow selection served candidate: %+v", l)
		}
	}
}

func TestStartCanaryRejectsIdenticalCandidate(t *testing.T) {
	c := newTestController(DefaultGateConfig())
	if err := c.StartCanary(PolicyRef{ID: "balanced", Version: "auto:incumbent"}, 10, 0); err == nil {
		t.Fatal("expected error for identical candidate")
	}
}

func TestStatusHistoryIsBounded(t *testing.T) {
	c := newTestController(GateConfig{MinSamples: 100000, MinPromoteSamples: 100000})
	for i := 0; i < maxHistory*3; i++ {
		c.BeginShadow(PolicyRef{ID: "balanced", Version: "auto:x"})
	}
	st := c.Status()
	if len(st.Transitions) > maxHistory {
		t.Fatalf("transitions=%d exceeds bound %d", len(st.Transitions), maxHistory)
	}
}

func TestGateHoldsPromotionWhenRequiredTelemetryCoverageIsMissing(t *testing.T) {
	cfg := GateConfig{MinSamples: 2, MinPromoteSamples: 2, MaxErrorRate: 0.2}
	candidate := Signals{Requests: 2, Successes: 2, ErrorRate: 0}
	res := EvaluateGate(candidate, Signals{}, cfg)
	if res.Verdict != VerdictHold {
		t.Fatalf("verdict=%s want hold for missing telemetry coverage", res.Verdict)
	}
	if len(res.Reasons) == 0 || res.Reasons[0] != "insufficient_no_eligible_signal_coverage:0<2" {
		t.Fatalf("reasons=%v want missing no-eligible signal coverage", res.Reasons)
	}

	candidate.NoEligibleObserved = 2
	candidate.BudgetAnomaliesObserved = 2
	res = EvaluateGate(candidate, Signals{}, cfg)
	if res.Verdict != VerdictPromote {
		t.Fatalf("verdict=%s reasons=%v want promote with complete signal coverage", res.Verdict, res.Reasons)
	}
}

func TestGateHoldsTTFTConfiguredWithoutTTFTSamples(t *testing.T) {
	cfg := GateConfig{MinSamples: 1, MinPromoteSamples: 1, MaxErrorRate: 0.2, MaxTTFTP95MS: 100}
	candidate := Signals{Requests: 1, Successes: 1, ErrorRate: 0, NoEligibleObserved: 1, BudgetAnomaliesObserved: 1}
	res := EvaluateGate(candidate, Signals{}, cfg)
	if res.Verdict != VerdictHold {
		t.Fatalf("verdict=%s want hold for missing TTFT samples", res.Verdict)
	}
	if len(res.Reasons) == 0 || res.Reasons[0] != "insufficient_ttft_signal_coverage:0" {
		t.Fatalf("reasons=%v want missing TTFT signal coverage", res.Reasons)
	}
}
