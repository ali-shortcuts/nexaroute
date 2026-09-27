package evallive

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
)

func benchLiveServer(b *testing.B) *httptest.Server {
	b.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"42"}}],"usage":{"prompt_tokens":9,"completion_tokens":2}}`))
	}))
	b.Cleanup(srv.Close)
	return srv
}

// BenchmarkLiveExecutor_Execute measures the end-to-end cost of one live
// evaluation case: request build, isolated upstream dispatch and evidence
// conversion. It excludes the runner, which is already benchmarked in
// internal/eval.
func BenchmarkLiveExecutor_Execute(b *testing.B) {
	srv := benchLiveServer(b)
	exec, err := NewLiveEvaluationExecutor(liveDeployment(), liveTwin(b, srv.URL),
		[]Prompt{{CaseID: "reasoning-multi-step-arithmetic", Prompt: "compute 42"}}, 0)
	if err != nil {
		b.Fatal(err)
	}
	c := eval.Case{ID: "reasoning-multi-step-arithmetic", Weight: 1,
		Expectation: eval.Expectation{Kind: eval.ExpectExactMatch, Expected: "42"}}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := exec.Execute(context.Background(), c); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLiveExecutor_RunSuite measures a whole live suite through the real
// eval runner: three upstream calls, grading and scorecard conversion.
func BenchmarkLiveExecutor_RunSuite(b *testing.B) {
	srv := benchLiveServer(b)
	prompts := []Prompt{
		{CaseID: "reasoning-multi-step-arithmetic", Prompt: "compute 42"},
		{CaseID: "reasoning-logic-deduction", Prompt: "who"},
		{CaseID: "reasoning-numeric-estimate", Prompt: "pi"},
	}
	exec, err := NewLiveEvaluationExecutor(liveDeployment(), liveTwin(b, srv.URL), prompts, 0)
	if err != nil {
		b.Fatal(err)
	}
	runner := eval.NewRunner()
	req := eval.Request{DeploymentID: "p1/m1", ProviderID: "p1", Model: "model-a", SuiteID: "reasoning"}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := runner.RunWithExecutor(context.Background(), req, exec); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLiveExecutor_PromptValidation measures the construction boundary,
// which every admin live run pays exactly once.
func BenchmarkLiveExecutor_PromptValidation(b *testing.B) {
	srv := benchLiveServer(b)
	twin := liveTwin(b, srv.URL)
	prompts := make([]Prompt, 0, 32)
	for i := 0; i < 32; i++ {
		prompts = append(prompts, Prompt{CaseID: "case-" + string(rune('a'+i%26)) + strings.Repeat("x", i%7), Prompt: "prompt body " + strings.Repeat("p", 64)})
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := NewLiveEvaluationExecutor(liveDeployment(), twin, prompts, 0); err != nil {
			b.Fatal(err)
		}
	}
}
