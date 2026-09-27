package eval

import (
	"testing"
)

func validStoredResult() Result {
	return Result{
		RunID: "run-1", SuiteID: "coding", SuiteVersion: "1", DeploymentID: "p/m",
		Cases: []CaseResult{{
			CaseID: "case-1", Verdict: VerdictPass, Resolution: ResolutionDeterministic, Weight: 1,
			Verdicts: []VerdictResult{{EvaluatorID: "det", Kind: KindDeterministic, Verdict: VerdictPass, Reason: "ok"}},
		}},
		Counts: map[Verdict]int{VerdictPass: 1},
		Score: 1, Samples: 1, Scoreable: true,
		Evaluators: []string{"det"},
	}
}

func TestStoreOwnsDeepCopyOnSaveAndRead(t *testing.T) {
	s := NewStore(4)
	in := validStoredResult()
	if err := s.Save(in); err != nil {
		t.Fatal(err)
	}
	in.Counts[VerdictPass] = 99
	in.Cases[0].Verdicts[0].Reason = "mutated-after-save"

	got, ok := s.Get("run-1")
	if !ok {
		t.Fatal("stored result missing")
	}
	if got.Counts[VerdictPass] != 1 || got.Cases[0].Verdicts[0].Reason != "ok" {
		t.Fatalf("store leaked caller mutation: %+v", got)
	}
	got.Counts[VerdictPass] = 77
	got.Cases[0].Verdicts[0].Reason = "mutated-after-read"
	again, _ := s.Get("run-1")
	if again.Counts[VerdictPass] != 1 || again.Cases[0].Verdicts[0].Reason != "ok" {
		t.Fatalf("store leaked returned mutation: %+v", again)
	}
}
