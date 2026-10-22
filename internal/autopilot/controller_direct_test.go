package autopilot

import (
	"strings"
	"testing"
)

// Direct tests for the five public Controller methods that previously had
// no package-level coverage (ADR 0005, Option A): TargetPolicyID,
// TargetMatches, Release, Evaluate and Rollback. The httpapi end-to-end
// suite drives some of them indirectly; these tests pin their edge
// semantics — identity matching, lease release without recording, gate
// evaluation without state mutation, and rollback state restoration —
// at the unit level where a regression is cheapest to localize.

func TestTargetPolicyIDAndMatchesDirect(t *testing.T) {
	c := newTestController(DefaultGateConfig())
	if got := c.TargetPolicyID(); got != "balanced" {
		t.Fatalf("TargetPolicyID=%q want %q", got, "balanced")
	}
	if !c.TargetMatches("balanced") {
		t.Fatal("TargetMatches(balanced)=false want true")
	}
	if c.TargetMatches("other-policy") {
		t.Fatal("TargetMatches(other-policy)=true want false")
	}
	if c.TargetMatches("") {
		t.Fatal("TargetMatches(empty)=true want false: empty id never matches")
	}

	// An empty target at construction falls back to the incumbent's id.
	fallback := New(PolicyRef{}, PolicyRef{ID: "solo", Version: "v1"}, DefaultGateConfig())
	if got := fallback.TargetPolicyID(); got != "solo" {
		t.Fatalf("fallback TargetPolicyID=%q want %q", got, "solo")
	}
	if !fallback.TargetMatches("solo") || fallback.TargetMatches("balanced") {
		t.Fatal("fallback controller matches the wrong policy ids")
	}
}

func TestReleaseDropsLeaseWithoutRecording(t *testing.T) {
	c := newTestController(GateConfig{MinSamples: 10, MinPromoteSamples: 100, MaxErrorRate: 0.2})
	if err := c.StartCanary(PolicyRef{ID: "balanced", Version: "auto:next"}, 100, 0); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	genBefore := c.Status().Generation

	canaryLease := c.Select("release-key")
	if !canaryLease.Canary {
		t.Fatalf("lease=%+v want canary", canaryLease)
	}
	if got := c.Status().InFlight; got != 1 {
		t.Fatalf("InFlight=%d want 1 after Select", got)
	}

	c.Release(canaryLease)
	st := c.Status()
	if st.InFlight != 0 {
		t.Fatalf("InFlight=%d want 0 after Release", st.InFlight)
	}
	if st.CandidateSignals.Requests != 0 || st.IncumbentSignals.Requests != 0 {
		t.Fatalf("Release recorded a sample: candidate=%d incumbent=%d want 0/0",
			st.CandidateSignals.Requests, st.IncumbentSignals.Requests)
	}
	if st.State != StateCanary || st.Generation != genBefore {
		t.Fatalf("Release mutated routing state: state=%s generation=%d want canary/%d",
			st.State, st.Generation, genBefore)
	}

	// Releasing an unknown or already-released lease is a no-op.
	c.Release(canaryLease)
	c.Release(Lease{ID: 999999})
	if got := c.Status().InFlight; got != 0 {
		t.Fatalf("InFlight=%d want 0 after redundant Release", got)
	}

	// The controller keeps serving after releases.
	l := c.Select("release-key")
	if !l.Canary || l.Policy.Version != "auto:next" {
		t.Fatalf("lease after Release=%+v want candidate", l)
	}
	c.Release(l)
}

func TestEvaluateDirectReflectsWindowsWithoutMutatingState(t *testing.T) {
	// No candidate and no samples: hold for insufficient samples, and the
	// evaluation is recorded as the last gate decision without any state
	// change.
	c := newTestController(GateConfig{MinSamples: 5, MinPromoteSamples: 10, MaxErrorRate: 0.2})
	res := c.Evaluate()
	if res.Verdict != VerdictHold {
		t.Fatalf("verdict=%s want hold on empty windows", res.Verdict)
	}
	if len(res.Reasons) == 0 || res.Reasons[0] != "insufficient_samples:0<5" {
		t.Fatalf("reasons=%v want insufficient_samples:0<5", res.Reasons)
	}
	st := c.Status()
	if st.State != StateDraft || st.Candidate != nil {
		t.Fatalf("Evaluate mutated state: state=%s candidate=%+v", st.State, st.Candidate)
	}
	if st.LastGateDecision == nil || st.LastGateDecision.Verdict != VerdictHold {
		t.Fatalf("LastGateDecision=%+v want the recorded hold", st.LastGateDecision)
	}

	// Clean-but-thin candidate evidence holds below MinPromoteSamples;
	// sufficient clean evidence promotes — and Evaluate alone never
	// performs the promotion.
	if err := c.StartCanary(PolicyRef{ID: "balanced", Version: "auto:good"}, 100, 0); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	serveCanarySamples(c, 6, Sample{Success: true, LatencyMS: 10})
	res = c.Evaluate()
	if res.Verdict != VerdictHold || len(res.Reasons) == 0 ||
		res.Reasons[0] != "awaiting_promotion_samples:6<10" {
		t.Fatalf("thin clean evidence: verdict=%s reasons=%v want hold/awaiting_promotion_samples:6<10",
			res.Verdict, res.Reasons)
	}
	serveCanarySamples(c, 4, Sample{Success: true, LatencyMS: 10})
	res = c.Evaluate()
	if res.Verdict != VerdictPromote {
		t.Fatalf("sufficient clean evidence: verdict=%s reasons=%v want promote", res.Verdict, res.Reasons)
	}
	st = c.Status()
	if st.State != StateCanary || st.Incumbent.Version != "auto:incumbent" {
		t.Fatalf("Evaluate promoted on its own: state=%s incumbent=%+v", st.State, st.Incumbent)
	}
	if _, ok := c.Promote(); !ok {
		t.Fatal("Promote refused after Evaluate returned promote: Evaluate must not consume the gate")
	}
}

func TestEvaluateDirectRegressionVerdictConsumedByRecord(t *testing.T) {
	// The candidate's absolute error rate stays under the ceiling, so its
	// own samples cannot trip the gate. The regression verdict appears
	// only once the incumbent window completes the comparison — and
	// because Record evaluates the gate synchronously, the completing
	// Record consumes that verdict as an automatic rollback. Evaluate
	// therefore observes hold right up to the completing sample; a
	// rollback verdict is never left sitting for a later Evaluate to
	// find. This test pins that invariant and the recorded gate evidence.
	cfg := GateConfig{MinSamples: 5, MinPromoteSamples: 100, MaxErrorRate: 0.9, MaxErrorRateDelta: 0.05}
	c := newTestController(cfg)
	if err := c.StartCanary(PolicyRef{ID: "balanced", Version: "auto:worse"}, 100, 0); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	// 1 error in 5 candidate samples (rate 0.20 < 0.90 ceiling).
	serveCanarySamples(c, 4, Sample{Success: true, LatencyMS: 10})
	serveCanarySamples(c, 1, Sample{Success: false, ErrorClass: ClassServerError, LatencyMS: 10})
	if got := c.Status().State; got != StateCanary {
		t.Fatalf("state=%s want canary: no verdict without incumbent evidence", got)
	}
	// Incumbent samples over the same window: zero errors. Recorded via
	// non-canary leases, exactly as the data plane records incumbent
	// traffic. Four samples are below MinSamples, so the regression
	// comparison does not exist yet and Evaluate must hold.
	for i := 0; i < 4; i++ {
		c.Record(Lease{Canary: false}, Sample{Success: true, LatencyMS: 10,
			HasNoEligibleSignal: true, HasBudgetAnomalySignal: true})
	}
	res := c.Evaluate()
	if res.Verdict != VerdictHold {
		t.Fatalf("verdict=%s reasons=%v want hold while incumbent evidence is thin", res.Verdict, res.Reasons)
	}
	if got := c.Status().State; got != StateCanary {
		t.Fatalf("state=%s want canary before the completing sample", got)
	}
	// The fifth incumbent sample completes the comparison (delta
	// 0.20 > 0.05): that Record trips the automatic rollback, and the
	// incident carries the rollback gate with the regression reason.
	c.Record(Lease{Canary: false}, Sample{Success: true, LatencyMS: 10,
		HasNoEligibleSignal: true, HasBudgetAnomalySignal: true})
	st := c.Status()
	if st.State != StateRolledBack {
		t.Fatalf("state=%s want rolled_back after the completing incumbent sample", st.State)
	}
	if st.LastRollback == nil || st.LastRollback.Gate.Verdict != VerdictRollback {
		t.Fatalf("last rollback=%+v want a recorded rollback gate", st.LastRollback)
	}
	if len(st.LastRollback.Gate.Reasons) == 0 ||
		!strings.HasPrefix(st.LastRollback.Gate.Reasons[0], "error_rate_regression:") {
		t.Fatalf("gate reasons=%v want error_rate_regression:*", st.LastRollback.Gate.Reasons)
	}
}

func TestRollbackDirectRestoresIncumbent(t *testing.T) {
	c := newTestController(GateConfig{MinSamples: 10, MinPromoteSamples: 100, MaxErrorRate: 0.2})

	// No candidate staged: rollback is refused and records nothing.
	if inc, ok := c.Rollback("operator_abort"); ok || inc.ID != "" {
		t.Fatalf("Rollback without candidate=(%+v,%v) want (zero,false)", inc, ok)
	}
	if st := c.Status(); len(st.Incidents) != 0 || st.Generation != 0 {
		t.Fatalf("refused Rollback mutated state: incidents=%d generation=%d", len(st.Incidents), st.Generation)
	}

	if err := c.StartCanary(PolicyRef{ID: "balanced", Version: "auto:bad"}, 100, 0); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	serveCanarySamples(c, 3, Sample{Success: true, LatencyMS: 10})

	// A request is in flight under the candidate when the operator aborts.
	inFlight := c.Select("direct-rollback")
	if !inFlight.Canary || inFlight.Policy.Version != "auto:bad" {
		t.Fatalf("in-flight lease=%+v want candidate", inFlight)
	}

	inc, ok := c.Rollback("operator_abort")
	if !ok {
		t.Fatal("Rollback with candidate refused")
	}
	if inc.ID != "inc-1" || inc.Kind != "operator_abort" || inc.PolicyID != "balanced" {
		t.Fatalf("incident=%+v want inc-1/operator_abort/balanced", inc)
	}
	if inc.BadVersion != "auto:bad" || inc.RestoredVersion != "auto:incumbent" {
		t.Fatalf("incident versions: bad=%q restored=%q", inc.BadVersion, inc.RestoredVersion)
	}
	if !strings.Contains(inc.Message, "rolled back") {
		t.Fatalf("incident message=%q want a rollback description", inc.Message)
	}

	st := c.Status()
	if st.State != StateRolledBack {
		t.Fatalf("state=%s want rolled_back", st.State)
	}
	if st.Candidate != nil {
		t.Fatalf("candidate still staged after rollback: %+v", st.Candidate)
	}
	if st.Generation != 1 {
		t.Fatalf("generation=%d want 1 after rollback", st.Generation)
	}
	if st.CandidateSignals.Requests != 0 {
		t.Fatalf("candidate window not reset: requests=%d", st.CandidateSignals.Requests)
	}
	if st.LastRollback == nil || st.LastRollback.ID != inc.ID || len(st.Incidents) != 1 {
		t.Fatalf("status rollback record: last=%+v incidents=%d", st.LastRollback, len(st.Incidents))
	}
	// The in-flight lease is untouched by the rollback and still tracked.
	if inFlight.Policy.Version != "auto:bad" || inFlight.Generation != 0 {
		t.Fatalf("in-flight lease mutated by rollback: %+v", inFlight)
	}
	if st.InFlight != 1 {
		t.Fatalf("InFlight=%d want 1 (the pre-rollback lease)", st.InFlight)
	}
	// New selections serve the restored incumbent.
	l := c.Select("after")
	if l.Canary || l.Policy.Version != "auto:incumbent" {
		t.Fatalf("post-rollback lease=%+v want incumbent", l)
	}
	// A second rollback has nothing to withdraw.
	if _, ok := c.Rollback("operator_abort"); ok {
		t.Fatal("second Rollback succeeded with no candidate staged")
	}
}
