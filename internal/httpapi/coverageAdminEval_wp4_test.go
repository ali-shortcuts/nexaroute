package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/evallive"
)

func TestCoverageAdminEvalMethodsFiltersAndNilPlane(t *testing.T) {
	cfg := phaseHConfig(filepath.Join(t.TempDir(), "state.json"), "")
	s := testGateway(t, cfg)

	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/admin/api/scorecards"},
		{http.MethodPost, "/admin/api/scorecards/p1/m1"},
	} {
		rr := adminDo(t, s, tc.method, tc.path, "")
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s status = %d, want 405: %s", tc.method, tc.path, rr.Code, rr.Body.String())
		}
	}

	for _, path := range []string{
		"/admin/api/scorecards?limit=0",
		"/admin/api/scorecards?limit=not-a-number",
		"/admin/api/scorecards?deployment=missing-deployment",
	} {
		rr := adminDo(t, s, http.MethodGet, path, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d: %s", path, rr.Code, rr.Body.String())
		}
	}
	for _, path := range []string{"/admin/api/scorecards/bad%20id", "/admin/api/scorecards/" + strings.Repeat("x", 513)} {
		rr := adminDo(t, s, http.MethodGet, path, "")
		if rr.Code != http.StatusBadRequest {
			t.Errorf("invalid detail path %q status = %d, want 400: %s", path, rr.Code, rr.Body.String())
		}
	}
	if rr := adminDo(t, s, http.MethodGet, "/admin/api/scorecards/p1/missing", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown scorecard detail status = %d, want 404", rr.Code)
	}
	if rr := adminDo(t, s, http.MethodGet, "/admin/api/evaluation/runs?run=not-stored", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown run detail status = %d, want 404", rr.Code)
	}

	// A nil plane is a valid fail-closed runtime state for list/detail endpoints.
	s.runtimeMu.Lock()
	plane := s.evaluation
	s.evaluation = nil
	s.runtimeMu.Unlock()
	for _, path := range []string{"/admin/api/scorecards", "/admin/api/evaluation/runs", "/admin/api/scorecards/p1/m1"} {
		rr := adminDo(t, s, http.MethodGet, path, "")
		want := http.StatusOK
		if strings.HasPrefix(path, "/admin/api/scorecards/") {
			want = http.StatusNotFound
		}
		if rr.Code != want {
			t.Errorf("nil-plane GET %s status = %d, want %d: %s", path, rr.Code, want, rr.Body.String())
		}
	}
	s.runtimeMu.Lock()
	s.evaluation = plane
	s.runtimeMu.Unlock()
}

func TestCoverageAdminEvalCatalogLimitsAndRunValidation(t *testing.T) {
	cfg := phaseHConfig(filepath.Join(t.TempDir(), "state.json"), "")
	s := testGateway(t, cfg)

	s.evaluation.setConfig(config.EvaluationConfig{Enabled: true, MaxScorecards: 0})
	rr := adminDo(t, s, http.MethodGet, "/admin/api/evaluation/suites", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("suite catalog status = %d: %s", rr.Code, rr.Body.String())
	}
	catalog := decodeJSON(t, rr)
	bounds := mustMap(t, catalog["bounds"], "catalog bounds")
	if bounds["max_scorecards"] != float64(0) || catalog["enabled"] != true {
		t.Fatalf("catalog did not report current limits/enablement: %#v", catalog)
	}

	cases := []struct {
		name string
		body string
		code int
	}{
		{"malformed JSON", "{", http.StatusBadRequest},
		{"unknown mode", `{"mode":"guess","suite_id":"coding","deployment_id":"p1/m1","artifacts":[]}`, http.StatusBadRequest},
		{"missing suite", `{"deployment_id":"p1/m1","artifacts":[]}`, http.StatusBadRequest},
		{"missing deployment", `{"suite_id":"coding","artifacts":[]}`, http.StatusBadRequest},
		{"unknown deployment", evalRunBody(t, "p9/m1", "coding", codingOutcomes(t, true)[:1], nil), http.StatusNotFound},
		{"replay prompts", evalRunBody(t, "p1/m1", "coding", codingOutcomes(t, true)[:1], map[string]any{"prompts": []map[string]string{{"case_id": "x", "prompt": "input"}}}), http.StatusBadRequest},
		{"duplicate replay artifacts", evalRunBody(t, "p1/m1", "coding", func() []eval.Outcome { a := codingOutcomes(t, true); return append(a, a[0]) }(), nil), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run", tc.body)
			if res.Code != tc.code {
				t.Fatalf("status = %d, want %d: %s", res.Code, tc.code, res.Body.String())
			}
		})
	}
	stats := planeStats(t, s)
	if stats["runs_stored"] != float64(0) {
		t.Fatalf("validation failures stored runs: %#v", stats)
	}
}

func TestCoverageAdminEvalLiveValidationAndExecutorBounds(t *testing.T) {
	cfg := phaseHConfig(filepath.Join(t.TempDir(), "state.json"), "")
	cfg.Evaluation.LiveEnabled = true
	s := testGateway(t, cfg)

	liveBody := func(extra map[string]any) string {
		t.Helper()
		payload := map[string]any{"mode": "live", "suite_id": "reasoning", "deployment_id": "p1/m1"}
		for k, v := range extra {
			payload[k] = v
		}
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	cases := []struct {
		name string
		body string
	}{
		{"prompts required", liveBody(nil)},
		{"live rejects artifacts", liveBody(map[string]any{"artifacts": codingOutcomes(t, true)[:1], "prompts": []map[string]string{{"case_id": "reasoning-multi-step-arithmetic", "prompt": "offline only"}}})},
		{"unknown case", liveBody(map[string]any{"prompts": []map[string]string{{"case_id": "not-in-suite", "prompt": "deterministic local input"}}})},
		{"prompt byte bound", liveBody(map[string]any{"prompts": []map[string]string{{"case_id": "reasoning-multi-step-arithmetic", "prompt": strings.Repeat("x", evallive.MaxPromptBytes+1)}}})},
		{"prompt count bound", func() string {
			ps := make([]map[string]string, evallive.MaxPromptsPerRun+1)
			for i := range ps {
				ps[i] = map[string]string{"case_id": "reasoning-multi-step-arithmetic", "prompt": fmt.Sprintf("local prompt %d", i)}
			}
			return liveBody(map[string]any{"prompts": ps})
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run", tc.body)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", res.Code, res.Body.String())
			}
		})
	}
	if got := planeStats(t, s)["runs_stored"]; got != float64(0) {
		t.Fatalf("rejected live validations stored a run: %v", got)
	}
}

func TestCoverageAdminEvalReplayHistoryRedactionAndScorecardError(t *testing.T) {
	cfg := phaseHConfig(filepath.Join(t.TempDir(), "state.json"), "")
	cfg.Evaluation.MaxScorecards = 1
	s := testGateway(t, cfg)
	const secret = "SECRET-ADMIN-EVAL-OUTPUT-CANARY-31d7"
	artifacts := codingOutcomes(t, true)
	for i := range artifacts {
		artifacts[i].Output = secret
	}

	first := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run", evalRunBody(t, "p1/m1", "coding", artifacts, nil))
	if first.Code != http.StatusOK {
		t.Fatalf("replay status = %d: %s", first.Code, first.Body.String())
	}
	if strings.Contains(first.Body.String(), secret) || !strings.Contains(first.Body.String(), `"mode":"replay"`) {
		t.Fatalf("replay response leaked output or omitted mode: %s", first.Body.String())
	}
	firstPayload := decodeJSON(t, first)
	if firstPayload["scorecard_written"] != true {
		t.Fatalf("first scorecard not written: %s", first.Body.String())
	}
	firstRun := mustMap(t, firstPayload["run"], "first run")
	firstID := firstRun["run_id"].(string)
	if firstRun["upstream_calls"] != float64(0) {
		t.Fatalf("offline replay unexpectedly used upstream: %#v", firstRun)
	}

	rr := adminDo(t, s, http.MethodGet, "/admin/api/evaluation/runs?limit=bad", "")
	history := decodeJSON(t, rr)
	if history["run_limit"] != float64(20) || len(history["runs"].([]any)) != 1 {
		t.Fatalf("invalid pagination was not defaulted safely: %#v", history)
	}
	rows := history["runs"].([]any)
	if strings.Contains(fmt.Sprint(rows), secret) {
		t.Fatalf("run history leaked recorded output: %#v", rows)
	}
	for _, limit := range []struct {
		query string
		want  float64
	}{{"0", 20}, {"9999", 100}, {"1", 1}} {
		page := adminDo(t, s, http.MethodGet, "/admin/api/evaluation/runs?limit="+limit.query, "")
		out := decodeJSON(t, page)
		if out["run_limit"] != limit.want {
			t.Errorf("limit %q resolved to %v, want %v", limit.query, out["run_limit"], limit.want)
		}
	}
	detail := adminDo(t, s, http.MethodGet, "/admin/api/evaluation/runs?run="+firstID, "")
	if detail.Code != http.StatusOK || strings.Contains(detail.Body.String(), secret) {
		t.Fatalf("run detail status/redaction = %d: %s", detail.Code, detail.Body.String())
	}
	if rr := adminDo(t, s, http.MethodGet, "/admin/api/scorecards?deployment=does-not-exist", ""); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "no evidence recorded") {
		t.Fatalf("unknown deployment filter response = %d %s", rr.Code, rr.Body.String())
	}
	if rr := adminDo(t, s, http.MethodGet, "/admin/api/scorecards?deployment=p1/m1", ""); rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "p1/m1") {
		t.Fatalf("filtered scorecard response = %d %s", rr.Code, rr.Body.String())
	}

	// The second deployment still gets a valid replay result, but the bounded
	// registry rejects its scorecard. The API returns the error without leaking
	// artifact output and its trace event records no_scorecard.
	second := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run", evalRunBody(t, "p1/m2", "coding", artifacts, nil))
	if second.Code != http.StatusOK || strings.Contains(second.Body.String(), secret) {
		t.Fatalf("bounded scorecard replay status/redaction = %d: %s", second.Code, second.Body.String())
	}
	secondPayload := decodeJSON(t, second)
	if secondPayload["scorecard_written"] != false || secondPayload["scorecard_error"] == nil {
		t.Fatalf("expected registry-bound error in response: %#v", secondPayload)
	}
	var foundTrace bool
	for _, e := range s.bus.Snapshot() {
		if e.Kind == "eval_run" && strings.Contains(e.Message, "status=no_scorecard") {
			foundTrace = true
		}
		if strings.Contains(e.Message, secret) {
			t.Fatalf("evaluation trace leaked artifact output: %#v", e)
		}
	}
	if !foundTrace {
		t.Fatal("missing bounded, no_scorecard evaluation trace")
	}
}
