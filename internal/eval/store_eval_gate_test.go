package eval

import (
	"testing"
	"time"
)

// Phase 19 eval gate (#510): HasPassingEvidence is the gate's
// evidence query — latest run for the (suite, deployment) pair
// decides, and only a scoreable run with passes and no fail/error
// verdicts counts.
func gateRun(id, suite, dep string, scoreable bool, counts map[Verdict]int) Result {
	return Result{
		RunID: id, SuiteID: suite, SuiteVersion: "v1", DeploymentID: dep,
		Scoreable: scoreable, Counts: counts,
		StartedAt: time.Now(), FinishedAt: time.Now(),
	}
}

func TestStoreHasPassingEvidence(t *testing.T) {
	pass := map[Verdict]int{VerdictPass: 3}
	s := NewStore(16)
	if s.HasPassingEvidence("coding-suite", "dep-a") {
		t.Fatal("no runs must mean no evidence")
	}
	if err := s.Save(gateRun("r1", "coding-suite", "dep-a", true, pass)); err != nil {
		t.Fatal(err)
	}
	if !s.HasPassingEvidence("coding-suite", "dep-a") {
		t.Fatal("passing run must satisfy the gate")
	}
	if s.HasPassingEvidence("other-suite", "dep-a") {
		t.Fatal("evidence for another suite must not count")
	}
	if s.HasPassingEvidence("coding-suite", "dep-b") {
		t.Fatal("evidence for another deployment must not count")
	}
	// A newer failing run revokes the gate.
	if err := s.Save(gateRun("r2", "coding-suite", "dep-a", true, map[Verdict]int{VerdictPass: 2, VerdictFail: 1})); err != nil {
		t.Fatal(err)
	}
	if s.HasPassingEvidence("coding-suite", "dep-a") {
		t.Fatal("latest failing run must revoke passing evidence")
	}
	// A newer passing run restores it.
	if err := s.Save(gateRun("r3", "coding-suite", "dep-a", true, pass)); err != nil {
		t.Fatal(err)
	}
	if !s.HasPassingEvidence("coding-suite", "dep-a") {
		t.Fatal("latest passing run must restore the gate")
	}
	// Error verdicts and non-scoreable runs never count.
	if err := s.Save(gateRun("r4", "coding-suite", "dep-a", true, map[Verdict]int{VerdictPass: 2, VerdictError: 1})); err != nil {
		t.Fatal(err)
	}
	if s.HasPassingEvidence("coding-suite", "dep-a") {
		t.Fatal("a run with error verdicts must not count as passing")
	}
	if err := s.Save(gateRun("r5", "coding-suite", "dep-a", false, pass)); err != nil {
		t.Fatal(err)
	}
	if s.HasPassingEvidence("coding-suite", "dep-a") {
		t.Fatal("a non-scoreable run must not count as passing")
	}
}
