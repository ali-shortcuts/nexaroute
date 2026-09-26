package evallive

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// liveUpstream is a real HTTP upstream. It answers with the text after the
// <<ANSWER:...>> marker in the incoming prompt, which lets one server serve
// several cases deterministically.
func liveUpstream(t testing.TB, hits *atomic.Int64, status int, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			io.WriteString(w, `{"error":"upstream exploded"}`)
			return
		}
		var in struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		answer := ""
		if len(in.Messages) > 0 {
			if i := strings.Index(in.Messages[0].Content, "<<ANSWER:"); i >= 0 {
				rest := in.Messages[0].Content[i+len("<<ANSWER:"):]
				if j := strings.Index(rest, ">>"); j >= 0 {
					answer = rest[:j]
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		out := map[string]any{
			"id": "chatcmpl-live", "object": "chat.completion", "created": 1, "model": "model-a",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18},
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func liveTwin(t testing.TB, baseURL string) providers.Adapter {
	t.Helper()
	p := config.ProviderConfig{ID: "p1", Name: "P1", Type: "openai_compatible", BaseURL: baseURL, AuthMode: "none", Enabled: true}
	p.ApplyDefaults()
	a, err := providers.NewAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	twin, ok := providers.EvaluationTwin(a)
	if !ok {
		t.Fatal("provider adapter does not support an evaluation twin")
	}
	return twin
}

func liveDeployment() Deployment {
	return Deployment{ID: "p1/m1", ProviderID: "p1", Model: "model-a", ProviderType: "openai_compatible"}
}

func liveSuiteCase(t testing.TB, suiteID, caseID string) eval.Case {
	t.Helper()
	suite, ok := eval.LookupSuite(suiteID)
	if !ok {
		t.Fatalf("suite %q missing", suiteID)
	}
	for _, c := range suite.Cases {
		if c.ID == caseID {
			return c
		}
	}
	t.Fatalf("case %q not in suite %q", caseID, suiteID)
	return eval.Case{}
}

// ---------------------------------------------------------------------------
// construction guards
// ---------------------------------------------------------------------------

func TestLiveExecutorRejectsProductionAdapter(t *testing.T) {
	srv := liveUpstream(t, nil, http.StatusOK, 0)
	p := config.ProviderConfig{ID: "p1", Type: "openai_compatible", BaseURL: srv.URL, AuthMode: "none", Enabled: true}
	p.ApplyDefaults()
	production, err := providers.NewAdapter(p, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewLiveEvaluationExecutor(liveDeployment(), production, []Prompt{{CaseID: "c", Prompt: "hi"}}, 0); err == nil {
		t.Fatal("live evaluation must refuse a production adapter: isolation is not optional")
	}
}

func TestLiveExecutorValidatesInputs(t *testing.T) {
	srv := liveUpstream(t, nil, http.StatusOK, 0)
	twin := liveTwin(t, srv.URL)
	dup := []Prompt{{CaseID: "a", Prompt: "x"}, {CaseID: "a", Prompt: "y"}}
	if _, err := NewLiveEvaluationExecutor(liveDeployment(), twin, dup, 0); err == nil {
		t.Fatal("duplicate prompts must be rejected")
	}
	if _, err := NewLiveEvaluationExecutor(liveDeployment(), twin, nil, 0); err == nil {
		t.Fatal("live evaluation without prompts must be rejected")
	}
	if _, err := NewLiveEvaluationExecutor(liveDeployment(), twin, []Prompt{{CaseID: "", Prompt: "x"}}, 0); err == nil {
		t.Fatal("empty case id must be rejected")
	}
	if _, err := NewLiveEvaluationExecutor(liveDeployment(), twin, []Prompt{{CaseID: "a", Prompt: "  "}}, 0); err == nil {
		t.Fatal("empty prompt must be rejected")
	}
	if _, err := NewLiveEvaluationExecutor(Deployment{ID: "p1/m1", ProviderID: "p1"}, twin, []Prompt{{CaseID: "a", Prompt: "x"}}, 0); err == nil {
		t.Fatal("deployment without model must be rejected")
	}
	if _, err := NewLiveEvaluationExecutor(liveDeployment(), nil, []Prompt{{CaseID: "a", Prompt: "x"}}, 0); err == nil {
		t.Fatal("nil adapter must be rejected")
	}
	exec, err := NewLiveEvaluationExecutor(liveDeployment(), twin, []Prompt{{CaseID: "a", Prompt: "x"}}, DefaultOutputTokens*100)
	if err != nil {
		t.Fatal(err)
	}
	if exec.MaxOutputTokensForRun() != MaxOutputTokens {
		t.Fatalf("max output tokens = %d, want clamped to %d", exec.MaxOutputTokensForRun(), MaxOutputTokens)
	}
}

// ---------------------------------------------------------------------------
// execution
// ---------------------------------------------------------------------------

func TestLiveExecutorMakesExactlyOneUpstreamCallPerCase(t *testing.T) {
	var hits atomic.Int64
	srv := liveUpstream(t, &hits, http.StatusOK, 0)
	exec, err := NewLiveEvaluationExecutor(liveDeployment(), liveTwin(t, srv.URL),
		[]Prompt{{CaseID: "reasoning-multi-step-arithmetic", Prompt: "compute <<ANSWER:42>>"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if exec.UpstreamCalls() != 0 {
		t.Fatalf("upstream calls before execution = %d, want 0", exec.UpstreamCalls())
	}
	out, err := exec.Execute(context.Background(), liveSuiteCase(t, "reasoning", "reasoning-multi-step-arithmetic"))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out.Status != eval.OutcomeOK {
		t.Fatalf("status = %q, want ok (error=%q)", out.Status, out.ErrorType)
	}
	if out.Output != "42" {
		t.Fatalf("output = %q, want 42", out.Output)
	}
	if out.PromptTokens != 11 || out.OutputTokens != 7 {
		t.Fatalf("tokens = (%d,%d), want (11,7)", out.PromptTokens, out.OutputTokens)
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1", hits.Load())
	}
	if exec.UpstreamCalls() != 1 {
		t.Fatalf("reported upstream calls = %d, want 1", exec.UpstreamCalls())
	}
	if exec.Calls() != 1 || exec.Failures() != 0 || exec.Missing() != 0 {
		t.Fatalf("counters = calls=%d failures=%d missing=%d", exec.Calls(), exec.Failures(), exec.Missing())
	}
}

func TestLiveExecutorRecordsMissingPromptAsNoEvidence(t *testing.T) {
	var hits atomic.Int64
	srv := liveUpstream(t, &hits, http.StatusOK, 0)
	exec, err := NewLiveEvaluationExecutor(liveDeployment(), liveTwin(t, srv.URL),
		[]Prompt{{CaseID: "other-case", Prompt: "hi"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.Execute(context.Background(), liveSuiteCase(t, "reasoning", "reasoning-multi-step-arithmetic")); err == nil {
		t.Fatal("a case without a prompt must produce no evidence, never a synthetic verdict")
	} else {
		var missing *MissingPromptError
		if !errors.As(err, &missing) {
			t.Fatalf("error = %T, want *MissingPromptError", err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0 for an unprompted case", hits.Load())
	}
	if exec.UpstreamCalls() != 0 {
		t.Fatalf("reported upstream calls = %d, want 0", exec.UpstreamCalls())
	}
	if exec.Missing() != 1 {
		t.Fatalf("missing = %d, want 1", exec.Missing())
	}
}

func TestLiveExecutorGradesHTTPFailure(t *testing.T) {
	var hits atomic.Int64
	srv := liveUpstream(t, &hits, http.StatusInternalServerError, 0)
	exec, err := NewLiveEvaluationExecutor(liveDeployment(), liveTwin(t, srv.URL),
		[]Prompt{{CaseID: "reasoning-multi-step-arithmetic", Prompt: "hi"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Execute(context.Background(), liveSuiteCase(t, "reasoning", "reasoning-multi-step-arithmetic"))
	if err != nil {
		t.Fatalf("an upstream HTTP failure is evidence, not an executor error: %v", err)
	}
	if out.Status != eval.OutcomeError {
		t.Fatalf("status = %q, want error", out.Status)
	}
	if out.ErrorType != "http_500" {
		t.Fatalf("error type = %q, want http_500", out.ErrorType)
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream calls = %d, want exactly 1 (no retry, no fallback)", hits.Load())
	}
	if exec.Failures() != 1 {
		t.Fatalf("failures = %d, want 1", exec.Failures())
	}
}

func TestLiveExecutorGradesTimeout(t *testing.T) {
	var hits atomic.Int64
	srv := liveUpstream(t, &hits, http.StatusOK, 400*time.Millisecond)
	exec, err := NewLiveEvaluationExecutor(liveDeployment(), liveTwin(t, srv.URL),
		[]Prompt{{CaseID: "reasoning-multi-step-arithmetic", Prompt: "hi"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	out, err := exec.Execute(ctx, liveSuiteCase(t, "reasoning", "reasoning-multi-step-arithmetic"))
	if err != nil {
		t.Fatalf("a timeout is evidence, not an executor error: %v", err)
	}
	if out.Status != eval.OutcomeTimeout {
		t.Fatalf("status = %q, want timeout", out.Status)
	}
	if out.ErrorType != "upstream_timeout" {
		t.Fatalf("error type = %q, want upstream_timeout", out.ErrorType)
	}
	if exec.Failures() != 1 {
		t.Fatalf("failures = %d, want 1", exec.Failures())
	}
}

func TestLiveExecutorGradesEmptyCompletion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":""}}]}`)
	}))
	defer srv.Close()
	exec, err := NewLiveEvaluationExecutor(liveDeployment(), liveTwin(t, srv.URL),
		[]Prompt{{CaseID: "reasoning-multi-step-arithmetic", Prompt: "hi"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Execute(context.Background(), liveSuiteCase(t, "reasoning", "reasoning-multi-step-arithmetic"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != eval.OutcomeError || out.ErrorType != "empty_completion" {
		t.Fatalf("outcome = %+v, want empty_completion error", out)
	}
}

// TestLiveExecutorRunProducesScorecardEvidence drives a full suite through the
// real eval runner: the live executor is a drop-in eval.Executor, so grading,
// verdict resolution and scorecard creation are the same code paths replay uses.
func TestLiveExecutorRunProducesScorecardEvidence(t *testing.T) {
	var hits atomic.Int64
	srv := liveUpstream(t, &hits, http.StatusOK, 0)
	prompts := []Prompt{
		{CaseID: "reasoning-multi-step-arithmetic", Prompt: "compute <<ANSWER:42>>"},
		{CaseID: "reasoning-logic-deduction", Prompt: "who? <<ANSWER:carol>>"},
		{CaseID: "reasoning-numeric-estimate", Prompt: "pi? <<ANSWER:3.14>>"},
	}
	exec, err := NewLiveEvaluationExecutor(liveDeployment(), liveTwin(t, srv.URL), prompts, 0)
	if err != nil {
		t.Fatal(err)
	}
	run, err := eval.NewRunner().RunWithExecutor(context.Background(), eval.Request{
		DeploymentID: "p1/m1", ProviderID: "p1", Model: "model-a", SuiteID: "reasoning",
	}, exec)
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 3 {
		t.Fatalf("upstream calls = %d, want 3 (one per prompted case)", hits.Load())
	}
	if run.Upstream != 3 {
		t.Fatalf("run reported upstream calls = %d, want 3", run.Upstream)
	}
	if run.Score != 1 {
		t.Fatalf("score = %v, want 1 (all three answers were correct)", run.Score)
	}
	if !run.Scoreable {
		t.Fatal("run must be scoreable with three decisive samples")
	}
	sc, written, err := run.Scorecard()
	if err != nil || !written {
		t.Fatalf("scorecard: written=%v err=%v", written, err)
	}
	if sc.DeploymentID != "p1/m1" {
		t.Fatalf("scorecard deployment = %q", sc.DeploymentID)
	}
	if _, ok := sc.Quality(scorecards.DimReasoning); !ok {
		t.Fatalf("scorecard has no reasoning quality dimension: %+v", sc.Values)
	}
}

// TestLiveExecutorNeverPersistsPrompts guards the privacy contract at the
// executor boundary: nothing the run records may echo a prompt or a completion.
func TestLiveExecutorNeverPersistsPrompts(t *testing.T) {
	const canary = "SECRET_EVAL_DATASET_CANARY_7b91"
	var hits atomic.Int64
	srv := liveUpstream(t, &hits, http.StatusOK, 0)
	prompts := []Prompt{
		{CaseID: "reasoning-multi-step-arithmetic", Prompt: canary + " <<ANSWER:42>>"},
		{CaseID: "reasoning-logic-deduction", Prompt: canary + " <<ANSWER:carol>>"},
		{CaseID: "reasoning-numeric-estimate", Prompt: canary + " <<ANSWER:3.14>>"},
	}
	exec, err := NewLiveEvaluationExecutor(liveDeployment(), liveTwin(t, srv.URL), prompts, 0)
	if err != nil {
		t.Fatal(err)
	}
	run, err := eval.NewRunner().RunWithExecutor(context.Background(), eval.Request{
		DeploymentID: "p1/m1", ProviderID: "p1", Model: "model-a", SuiteID: "reasoning",
	}, exec)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(blob), canary) {
		t.Fatal("evaluation run record leaked the live prompt canary")
	}
	sc, _, err := run.Scorecard()
	if err != nil {
		t.Fatal(err)
	}
	scBlob, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(scBlob), canary) {
		t.Fatal("scorecard leaked the live prompt canary")
	}
	if hits.Load() != 3 {
		t.Fatalf("upstream calls = %d, want 3", hits.Load())
	}
}
