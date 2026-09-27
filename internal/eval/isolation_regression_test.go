package eval

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestRunnerDisabledJudgeIsNeverExecuted(t *testing.T) {
	reg := &Registry{}
	var calls atomic.Int64
	if err := reg.Register(NewJudgeEvaluator("counting-judge", func(context.Context, Case, Outcome) (Verdict, string) {
		calls.Add(1)
		return VerdictPass, "should not run"
	})); err != nil {
		t.Fatal(err)
	}
	suite, ok := LookupSuite("coding")
	if !ok {
		t.Fatal("coding suite missing")
	}
	outcomes := make([]Outcome, 0, len(suite.Cases))
	for _, c := range suite.Cases {
		outcomes = append(outcomes, Outcome{CaseID: c.ID, Status: OutcomeOK})
	}
	res, err := NewRunnerWithRegistry(reg).Run(context.Background(), Request{
		DeploymentID: "p/m", SuiteID: suite.ID, JudgeEnabled: false, Outcomes: outcomes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatalf("disabled judge executed %d times", calls.Load())
	}
	if res.JudgeUsed {
		t.Fatal("disabled judge reported as used")
	}
}

type missingEvidenceExecutor struct{}

func (missingEvidenceExecutor) Execute(context.Context, Case) (Outcome, error) {
	return Outcome{}, ErrMissingEvidence
}

func TestRunnerClassifiesMissingEvidenceWithoutExecutorFailure(t *testing.T) {
	res, err := NewRunner().RunWithExecutor(context.Background(), Request{
		DeploymentID: "p/m", SuiteID: "coding",
	}, missingEvidenceExecutor{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Counts[VerdictMissing] == 0 {
		t.Fatalf("missing evidence was not classified as missing: %+v", res.Counts)
	}
	for _, c := range res.Cases {
		if c.Verdict != VerdictMissing || c.ErrorType != "missing_evidence" {
			t.Fatalf("case misclassified: %+v", c)
		}
	}
}
