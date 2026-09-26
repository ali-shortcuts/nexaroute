package eval

import (
	"context"
	"testing"
)

func benchArtifacts(n int) []Outcome {
	suite, _ := LookupSuite("coding")
	out := make([]Outcome, 0, n)
	for i := 0; i < n; i++ {
		c := suite.Cases[i%len(suite.Cases)]
		out = append(out, Outcome{
			CaseID:    c.ID + "-" + string(rune('a'+i%26)),
			Status:    OutcomeOK,
			LatencyMS: int64(120 + i),
			UnitTests: &UnitTestResult{Compiled: true, Passed: 2},
		})
	}
	return out
}

func BenchmarkResolve_Verdicts(b *testing.B) {
	verdicts := []VerdictResult{
		{EvaluatorID: "unit_tests", Kind: KindDeterministic, Verdict: VerdictPass},
		{EvaluatorID: "llm_judge", Kind: KindJudge, Verdict: VerdictFail, Reason: "looks wrong"},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if v, _ := Resolve(verdicts, true); v != VerdictPass {
			b.Fatalf("unexpected verdict %s", v)
		}
	}
}

func BenchmarkReplayExecutor(b *testing.B) {
	outcomes := benchArtifacts(4)
	exec, err := NewReplayExecutor(outcomes)
	if err != nil {
		b.Fatal(err)
	}
	c := Case{ID: outcomes[0].CaseID, Weight: 1, Expectation: Expectation{Kind: ExpectUnitTests}}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := exec.Execute(context.Background(), c); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRun_CodingSuite(b *testing.B) {
	suite, _ := LookupSuite("coding")
	outcomes := make([]Outcome, 0, len(suite.Cases))
	for _, c := range suite.Cases {
		outcomes = append(outcomes, Outcome{
			CaseID:    c.ID,
			Status:    OutcomeOK,
			LatencyMS: 150,
			UnitTests: &UnitTestResult{Compiled: true, Passed: 3},
		})
	}
	r := NewRunner()
	req := Request{DeploymentID: "p1/m1", SuiteID: "coding", Outcomes: outcomes, LatencyTargetMS: 1000}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		res, err := r.Run(context.Background(), req)
		if err != nil {
			b.Fatal(err)
		}
		if !res.Scoreable {
			b.Fatalf("run not scoreable: %+v", res)
		}
	}
}

func BenchmarkRun_AllSuites(b *testing.B) {
	r := NewRunner()
	requests := make([]Request, 0, len(builtinSuites()))
	for _, s := range builtinSuites() {
		outcomes := make([]Outcome, 0, len(s.Cases))
		for _, c := range s.Cases {
			outcomes = append(outcomes, Outcome{
				CaseID:    c.ID,
				Status:    OutcomeOK,
				Output:    "42",
				LatencyMS: 90,
				UnitTests: &UnitTestResult{Compiled: true, Passed: 1},
				ToolCall:  &ToolCall{Name: "search_files"},
				Stream:    &StreamResult{Terminated: true, Events: 12},
			})
		}
		requests = append(requests, Request{DeploymentID: "p1/m1", SuiteID: s.ID, Outcomes: outcomes})
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for _, req := range requests {
			if _, err := r.Run(context.Background(), req); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkHealthFromRuns(b *testing.B) {
	runs := make([]Result, 0, 128)
	for i := 0; i < 128; i++ {
		runs = append(runs, Result{
			RunID: string(rune('a' + i%26)), SuiteID: "coding", DeploymentID: "p1/m" + string(rune('a'+i%26)),
			Samples: 4, Score: 0.75,
		})
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = HealthFromRuns(runs)
	}
}
