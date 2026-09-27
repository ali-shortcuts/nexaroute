package eval

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRunnerCaseTimeoutBoundsEvaluators is the cancellation regression for the
// grading half of a run.
//
// CaseTimeoutMS is documented as the bound on one case. The executor received it,
// but grading ran on the parent context — and the parent context was handed to
// the judge, the one evaluator the runner does not control. A judge that ignores
// the caller's cancellation could therefore block an evaluation run (and the
// admin request driving it) forever, whatever case_timeout_ms said.
func TestRunnerCaseTimeoutBoundsEvaluators(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewJudgeEvaluator("slow_judge", func(ctx context.Context, c Case, o Outcome) (Verdict, string) {
		select {
		case <-ctx.Done():
			return VerdictUnjudged, "case context expired"
		case <-time.After(2 * time.Second):
			return VerdictPass, "judge answered after the case timeout"
		}
	}))
	r := NewRunnerWithRegistry(reg)

	suite, ok := LookupSuite("reasoning")
	if !ok {
		t.Fatal("reasoning suite missing")
	}
	outcomes := make([]Outcome, 0, len(suite.Cases))
	for _, c := range suite.Cases {
		outcomes = append(outcomes, Outcome{CaseID: c.ID, Status: OutcomeOK, Output: "42"})
	}

	start := time.Now()
	res, err := r.Run(context.Background(), Request{
		DeploymentID:  "p1/m1",
		SuiteID:       suite.ID,
		Outcomes:      outcomes,
		JudgeEnabled:  true,
		CaseTimeoutMS: 20,
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("grading escaped the per-case timeout: %d cases with case_timeout_ms=20 took %v", len(suite.Cases), elapsed)
	}
	if len(res.Cases) != len(suite.Cases) {
		t.Fatalf("cases = %d, want %d", len(res.Cases), len(suite.Cases))
	}
	for _, cr := range res.Cases {
		sawJudge := false
		for _, v := range cr.Verdicts {
			if v.Kind != KindJudge {
				continue
			}
			sawJudge = true
			if v.Verdict != VerdictUnjudged {
				t.Fatalf("case %s: judge ran past the case timeout and returned %s (%s)", cr.CaseID, v.Verdict, v.Reason)
			}
		}
		if !sawJudge {
			t.Fatalf("case %s: judge verdict missing", cr.CaseID)
		}
	}
}

// TestOutcomeValidateBoundsErrorType closes an artifact hole: error_type is
// operator-supplied free text that the runner copies into the case result, which
// is what the store keeps, what the state file persists and what the admin
// surface returns. It has to be bounded like every other recorded string.
func TestOutcomeValidateBoundsErrorType(t *testing.T) {
	if err := (Outcome{CaseID: "c", ErrorType: strings.Repeat("e", MaxReasonBytes+1)}).Validate(); err == nil {
		t.Fatal("oversized error_type must be rejected")
	}
	if err := (Outcome{CaseID: "c", ErrorType: "http_500"}).Validate(); err != nil {
		t.Fatalf("bounded error_type must be accepted: %v", err)
	}
}

// TestRunnerRejectsArtifactWithOversizedErrorType proves the bound is enforced on
// the run path, not only by the validator in isolation.
func TestRunnerRejectsArtifactWithOversizedErrorType(t *testing.T) {
	r := NewRunner()
	_, err := r.Run(context.Background(), Request{
		DeploymentID: "p1/m1",
		SuiteID:      "coding",
		Outcomes: []Outcome{{
			CaseID:    "coding-bugfix",
			Status:    OutcomeError,
			ErrorType: strings.Repeat("e", MaxReasonBytes+1),
			UnitTests: &UnitTestResult{Compiled: true, Passed: 1},
		}},
	})
	if err == nil {
		t.Fatal("a run carrying an unbounded error_type must be rejected")
	}
}
