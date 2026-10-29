package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
)

// Issue #663 (httpapi scope): behavior tests for the evaluation-run
// branches the ba5ff2f coverprofile left uncovered. The live/runner/save
// branches are exercised by direct handler calls with a deterministic
// local deployment (phaseH providers, dead upstream) so no live network
// is involved: a dead upstream only produces recorded case errors,
// never a real provider call.

func issue663EvalServer(t *testing.T) *Server {
	t.Helper()
	cfg := phaseHConfig(t.TempDir()+"/evaluation-state.json", "")
	cfg.Evaluation.LiveEnabled = true
	return testGateway(t, cfg)
}

func issue663EvalDep(model string) deploymentView {
	return deploymentView{ID: "p1/m1", ProviderID: "p1", Model: model, ProviderType: "openai_compatible"}
}

func TestIssue663HttpapiLiveRunRunnerError(t *testing.T) {
	s := issue663EvalServer(t)
	plane := s.evaluationSnapshot()
	cfgEval := plane.config()
	cfgEval.LiveEnabled = true
	suite, ok := eval.LookupSuite("coding")
	if !ok || len(suite.Cases) == 0 {
		t.Fatal("coding suite missing")
	}
	// A suite that fails ValidateSuite only at the runner must be
	// rejected after prompt validation, not executed.
	suite.MinSamples = 0
	in := evaluationRunRequest{
		SuiteID: "coding",
		Prompts: []livePrompt{{CaseID: suite.Cases[0].ID, Prompt: "hello"}},
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/evaluation/run", nil)
	s.adminEvaluationRunLive(w, req, plane, cfgEval, in, issue663EvalDep("model-a"), suite)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "min_samples") {
		t.Fatalf("invalid suite live run: %d %s", w.Code, w.Body.String())
	}
}

func TestIssue663HttpapiLiveRunSaveError(t *testing.T) {
	s := issue663EvalServer(t)
	plane := s.evaluationSnapshot()
	cfgEval := plane.config()
	cfgEval.LiveEnabled = true
	suite, ok := eval.LookupSuite("coding")
	if !ok || len(suite.Cases) == 0 {
		t.Fatal("coding suite missing")
	}
	// The runner records the dead upstream as case errors and still
	// produces a run; the run is then un-storable because its model
	// name exceeds the store's safe limit, so the handler must fail
	// closed instead of reporting success.
	in := evaluationRunRequest{
		SuiteID: "coding",
		Prompts: []livePrompt{{CaseID: suite.Cases[0].ID, Prompt: "hello"}},
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/evaluation/run", nil)
	s.adminEvaluationRunLive(w, req, plane, cfgEval, in, issue663EvalDep(strings.Repeat("m", 2000)), suite)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "cannot store evaluation run") {
		t.Fatalf("unstorable live run: %d %s", w.Code, w.Body.String())
	}
}

func TestIssue663HttpapiReplayRunRunnerAndSaveErrors(t *testing.T) {
	s := issue663EvalServer(t)
	plane := s.evaluationSnapshot()
	cfgEval := plane.config()

	// An empty suite passes the replay pre-checks (valid artifact,
	// under the artifact cap) and fails inside the runner.
	in := evaluationRunRequest{
		SuiteID:   "coding",
		Artifacts: []eval.Outcome{{CaseID: "c", Status: eval.OutcomeOK, Output: "ok", LatencyMS: 10}},
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/evaluation/run", nil)
	s.adminEvaluationRunReplay(w, req, plane, cfgEval, in, issue663EvalDep("model-a"), eval.Suite{})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "unknown evaluation suite") {
		t.Fatalf("empty-suite replay: %d %s", w.Code, w.Body.String())
	}

	// A valid replay whose run carries an over-long model name must
	// reach the store error, not a success response.
	suite, ok := eval.LookupSuite("coding")
	if !ok {
		t.Fatal("coding suite missing")
	}
	in = evaluationRunRequest{SuiteID: "coding", Artifacts: codingOutcomes(t, true)}
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/evaluation/run", nil)
	s.adminEvaluationRunReplay(w, req, plane, cfgEval, in, issue663EvalDep(strings.Repeat("m", 2000)), suite)
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "cannot store evaluation run") {
		t.Fatalf("unstorable replay run: %d %s", w.Code, w.Body.String())
	}
}

func TestIssue663HttpapiEvaluationRunResponseScorecardError(t *testing.T) {
	s := issue663EvalServer(t)
	plane := s.evaluationSnapshot()
	// Scoreable with a quality value, but no finished timestamp: the
	// scorecard conversion must fail and the response must say so
	// instead of claiming a scorecard was written.
	run := eval.Result{
		RunID: "issue663-run", SuiteID: "coding", SuiteVersion: "1",
		DeploymentID: "p1/m1", ProviderID: "p1", Model: "model-a",
		Scoreable: true,
		Quality:   &eval.DimensionScore{Dimension: scorecards.DimCoding, Score: 0.5, Raw: 0.5, Samples: 3},
	}
	resp := s.evaluationRunResponse(plane, run)
	if resp["scorecard_written"] != false {
		t.Fatalf("scorecard_written=%v want false: %v", resp["scorecard_written"], resp)
	}
	errText, _ := resp["scorecard_error"].(string)
	if !strings.Contains(errText, "timestamp is required") {
		t.Fatalf("scorecard_error=%q want timestamp error", errText)
	}
	if _, ok := resp["run"]; !ok {
		t.Fatal("run row missing from response")
	}
}
