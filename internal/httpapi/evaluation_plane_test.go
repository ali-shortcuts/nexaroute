package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
)

// ---------------------------------------------------------------------------
// Phase H helpers
// ---------------------------------------------------------------------------

func phaseHProviders() []config.ProviderConfig {
	return []config.ProviderConfig{{
		ID: "p1", Name: "P1", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1", AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{
			{ID: "m1", Model: "model-a", Enabled: true, Weight: 1},
			{ID: "m2", Model: "model-b", Enabled: true, Weight: 1},
		},
	}}
}

func phaseHConfig(statePath, importPath string) config.Config {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Evaluation.Enabled = true
	cfg.Evaluation.StatePath = statePath
	cfg.Evaluation.ImportPath = importPath
	cfg.Evaluation.MaxRuns = 16
	cfg.Evaluation.MaxScorecards = 32
	cfg.Evaluation.MaxArtifacts = 8
	cfg.Providers = phaseHProviders()
	return cfg
}

func codingOutcomes(t *testing.T, passing bool) []eval.Outcome {
	t.Helper()
	suite, ok := eval.LookupSuite("coding")
	if !ok {
		t.Fatal("coding suite missing")
	}
	out := make([]eval.Outcome, 0, len(suite.Cases))
	for _, c := range suite.Cases {
		o := eval.Outcome{CaseID: c.ID, Status: eval.OutcomeOK, LatencyMS: 120, OutputTokens: 64, Output: "SECRET-MODEL-OUTPUT-9f2c41"}
		if passing {
			o.UnitTests = &eval.UnitTestResult{Compiled: true, Passed: 3}
		} else {
			o.UnitTests = &eval.UnitTestResult{Compiled: true, Failed: 2}
		}
		out = append(out, o)
	}
	return out
}

func evalRunBody(t *testing.T, deploymentID, suiteID string, artifacts []eval.Outcome, extra map[string]any) string {
	t.Helper()
	payload := map[string]any{"suite_id": suiteID, "deployment_id": deploymentID, "artifacts": artifacts}
	for k, v := range extra {
		payload[k] = v
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func adminDo(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, "http://gateway"+path, nil)
	} else {
		req = httptest.NewRequest(method, "http://gateway"+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func decodeJSON(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON (%d): %s", rr.Code, rr.Body.String())
	}
	return out
}

func metricValue(t *testing.T, metrics, family string) float64 {
	t.Helper()
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, family+" ") {
			var v float64
			if _, err := fmt.Sscanf(strings.TrimPrefix(line, family+" "), "%g", &v); err != nil {
				t.Fatalf("bad metric line %q: %v", line, err)
			}
			return v
		}
	}
	t.Fatalf("metric %s missing", family)
	return 0
}

func mustMap(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s: not an object: %#v", what, v)
	}
	return m
}

func planeStats(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rr := adminDo(t, s, http.MethodGet, "/admin/api/scorecards", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("scorecards status %d", rr.Code)
	}
	body := decodeJSON(t, rr)
	return mustMap(t, body["stats"], "stats")
}

func reloadPhaseH(t *testing.T, s *Server, cfg config.Config) {
	t.Helper()
	if err := s.applyConfigLocked(cfg); err != nil {
		t.Fatalf("apply config: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Disabled plane
// ---------------------------------------------------------------------------

func TestPhaseH_DisabledPlaneAcceptsNothingAndInventsNothing(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = phaseHProviders()
	s := testGateway(t, cfg)

	rr := adminDo(t, s, http.MethodGet, "/admin/api/scorecards", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("scorecards status %d", rr.Code)
	}
	body := decodeJSON(t, rr)
	if body["total"] != float64(0) {
		t.Fatalf("disabled plane reported scorecards: %v", body["total"])
	}
	if rows, ok := body["scorecards"].([]any); !ok || len(rows) != 0 {
		t.Fatalf("disabled plane must return an empty list, got %#v", body["scorecards"])
	}

	rr = adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run",
		evalRunBody(t, "p1/m1", "coding", codingOutcomes(t, true), nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("disabled plane run status = %d, want 409 (%s)", rr.Code, rr.Body.String())
	}

	rr = adminDo(t, s, http.MethodGet, "/admin/api/evaluation/suites", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("suites status %d", rr.Code)
	}
	suites := decodeJSON(t, rr)
	if suites["enabled"] != false {
		t.Fatalf("suites must report enabled=false: %v", suites["enabled"])
	}
	if list, ok := suites["suites"].([]any); !ok || len(list) != 8 {
		t.Fatalf("suite catalog = %#v", suites["suites"])
	}
	if suites["judge_available"] != false {
		t.Fatalf("Phase H must not advertise a judge: %v", suites["judge_available"])
	}

	rr = adminDo(t, s, http.MethodGet, "/admin/api/evaluation/runs", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("runs status %d", rr.Code)
	}
	if runs, ok := decodeJSON(t, rr)["runs"].([]any); !ok || len(runs) != 0 {
		t.Fatalf("disabled plane must have no runs")
	}

	if rr := adminDo(t, s, http.MethodGet, "/admin/api/scorecards/p1/m1", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("no-evidence scorecard status = %d, want 404", rr.Code)
	}

	rr = adminDo(t, s, http.MethodGet, "/metrics", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("metrics status %d", rr.Code)
	}
	metrics := rr.Body.String()
	if !strings.Contains(metrics, "nexaroute_evaluation_enabled 0") {
		t.Fatal("metrics must report the disabled evaluation plane")
	}
	if !strings.Contains(metrics, "nexaroute_scorecards_total 0") {
		t.Fatal("metrics must report zero scorecards")
	}

	// The admin snapshot always carries the Phase H sections.
	rr = adminDo(t, s, http.MethodGet, "/admin/api/snapshot", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("snapshot status %d", rr.Code)
	}
	snap := decodeJSON(t, rr)
	if _, ok := snap["scorecards"]; !ok {
		t.Fatal("snapshot missing scorecards section")
	}
	if _, ok := snap["evaluation"]; !ok {
		t.Fatal("snapshot missing evaluation section")
	}
}

// ---------------------------------------------------------------------------
// Runs produce provenance-complete scorecards, never fabricated ones
// ---------------------------------------------------------------------------

func TestPhaseH_RunWritesProvenanceScorecard(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "evaluation-state.json")
	s := testGateway(t, phaseHConfig(statePath, ""))

	rr := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run",
		evalRunBody(t, "p1/m1", "coding", codingOutcomes(t, true), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("run status %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeJSON(t, rr)
	if resp["scorecard_written"] != true {
		t.Fatalf("scorecard not written: %s", rr.Body.String())
	}
	run := mustMap(t, resp["run"], "run")
	if run["scoreable"] != true {
		t.Fatalf("run not scoreable: %v", run)
	}
	if run["upstream_calls"] != float64(0) {
		t.Fatalf("evaluation made upstream calls: %v", run["upstream_calls"])
	}
	if run["judge_used"] != false {
		t.Fatalf("judge was used in Phase H: %v", run["judge_used"])
	}
	sc := mustMap(t, resp["scorecard"], "scorecard")
	if sc["version"] != float64(1) || sc["deployment_id"] != "p1/m1" {
		t.Fatalf("unexpected scorecard: %v", sc)
	}

	// Admin rows carry provenance on every single value.
	rr = adminDo(t, s, http.MethodGet, "/admin/api/scorecards", "")
	body := decodeJSON(t, rr)
	if body["total"] != float64(1) {
		t.Fatalf("scorecard total = %v", body["total"])
	}
	rows, _ := body["scorecards"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows = %#v", body["scorecards"])
	}
	row := mustMap(t, rows[0], "row")
	values, _ := row["values"].([]any)
	if len(values) == 0 {
		t.Fatal("scorecard has no values")
	}
	for _, v := range values {
		vm := mustMap(t, v, "value")
		if vm["provenance"] != string(scorecards.ProvenanceEvaluation) {
			t.Fatalf("value without evaluation provenance: %#v", vm)
		}
		if vm["source"] == "" || vm["source"] == nil {
			t.Fatalf("value without source: %#v", vm)
		}
		if vm["sample_count"].(float64) < 1 || vm["evaluated_at"] == "" {
			t.Fatalf("value without samples/timestamp: %#v", vm)
		}
	}
	if ref, ok := row["evaluation"].(map[string]any); !ok || ref["suite_id"] != "coding" {
		t.Fatalf("scorecard missing evaluation reference: %#v", row["evaluation"])
	}
	if cov, ok := row["quality_coverage"].(map[string]any); !ok || cov["present"].(float64) < 1 {
		t.Fatalf("quality coverage missing: %#v", row["quality_coverage"])
	}

	// Single-deployment view includes version history.
	rr = adminDo(t, s, http.MethodGet, "/admin/api/scorecards/p1/m1", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("detail status %d", rr.Code)
	}
	detail := mustMap(t, decodeJSON(t, rr)["scorecard"], "detail")
	if hist, ok := detail["history"].([]any); !ok || len(hist) != 1 {
		t.Fatalf("history = %#v", detail["history"])
	}

	// Run history + evaluation-only health namespace.
	rr = adminDo(t, s, http.MethodGet, "/admin/api/evaluation/runs?limit=5", "")
	runsBody := decodeJSON(t, rr)
	runs, _ := runsBody["runs"].([]any)
	if len(runs) != 1 {
		t.Fatalf("stored runs = %#v", runsBody["runs"])
	}
	if _, ok := runsBody["health"].([]any); !ok {
		t.Fatalf("health namespace missing: %#v", runsBody["health"])
	}
	runID := mustMap(t, runs[0], "stored run")["run_id"].(string)
	rr = adminDo(t, s, http.MethodGet, "/admin/api/evaluation/runs?run="+runID, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("run detail status %d", rr.Code)
	}

	// Durable state file: 0600, version 1, runs + scorecards.
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("state file missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state file mode = %o, want 600", perm)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("state file invalid JSON: %v", err)
	}
	if doc["version"] != float64(1) {
		t.Fatalf("state version = %v", doc["version"])
	}
	if runs, _ := doc["runs"].([]any); len(runs) != 1 {
		t.Fatalf("state runs = %#v", doc["runs"])
	}
	if scs, _ := doc["scorecards"].([]any); len(scs) != 1 {
		t.Fatalf("state scorecards = %#v", doc["scorecards"])
	}

	stats := planeStats(t, s)
	if stats["runs_total"] != float64(1) || stats["scorecards_written"] != float64(1) {
		t.Fatalf("unexpected stats: %#v", stats)
	}
	if stats["state_writes_failed"] != float64(0) || stats["import_error"] != nil {
		t.Fatalf("state/import errors recorded: %#v", stats)
	}

	// Metrics families reflect the run.
	rr = adminDo(t, s, http.MethodGet, "/metrics", "")
	metrics := rr.Body.String()
	for _, want := range []string{
		"nexaroute_scorecards_total 1",
		`nexaroute_scorecard_values{provenance="evaluation"}`,
		`nexaroute_evaluation_runs_total{outcome="stored"} 1`,
		"nexaroute_evaluation_scorecards_written_total 1",
		"nexaroute_evaluation_stored_runs 1",
		`nexaroute_evaluation_cases_total{verdict="pass"}`,
		"nexaroute_evaluation_enabled 1",
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics missing %q", want)
		}
	}
}

func TestPhaseH_ThinEvidenceWritesNoScorecard(t *testing.T) {
	s := testGateway(t, phaseHConfig(filepath.Join(t.TempDir(), "state.json"), ""))

	artifacts := codingOutcomes(t, true)[:1] // below the suite's min_samples
	rr := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run",
		evalRunBody(t, "p1/m1", "coding", artifacts, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("run status %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeJSON(t, rr)
	if resp["scorecard_written"] != false {
		t.Fatalf("insufficient evidence must not produce a scorecard: %s", rr.Body.String())
	}
	if resp["scorecard"] != nil {
		t.Fatalf("insufficient evidence produced a scorecard: %#v", resp["scorecard"])
	}
	if reason, _ := resp["reason"].(string); !strings.Contains(reason, "insufficient") {
		t.Fatalf("missing insufficient-samples reason: %#v", resp)
	}
	if insufficient := planeStats(t, s)["runs_insufficient_samples"]; insufficient != float64(1) {
		t.Fatalf("insufficient counter = %v", insufficient)
	}
	if rr := adminDo(t, s, http.MethodGet, "/admin/api/scorecards/p1/m1", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("no scorecard should exist, got %d", rr.Code)
	}
	rr = adminDo(t, s, http.MethodGet, "/metrics", "")
	if !strings.Contains(rr.Body.String(), `nexaroute_evaluation_runs_total{outcome="insufficient_samples"} 1`) {
		t.Fatal("metrics missing insufficient_samples outcome")
	}
}

func TestPhaseH_RunRejectsFabricationAndMutation(t *testing.T) {
	cfg := phaseHConfig(filepath.Join(t.TempDir(), "state.json"), "")
	cfg.Evaluation.MaxArtifacts = 2
	s := testGateway(t, cfg)

	cases := []struct {
		name   string
		body   string
		status int
	}{
		{"unknown deployment", evalRunBody(t, "p9/m1", "coding", codingOutcomes(t, true)[:1], nil), http.StatusNotFound},
		{"provider mismatch", evalRunBody(t, "p1/m1", "coding", codingOutcomes(t, true)[:1], map[string]any{"provider_id": "p9"}), http.StatusBadRequest},
		{"model mismatch", evalRunBody(t, "p1/m1", "coding", codingOutcomes(t, true)[:1], map[string]any{"model": "other"}), http.StatusBadRequest},
		{"no artifacts", evalRunBody(t, "p1/m1", "coding", nil, nil), http.StatusBadRequest},
		{"unknown suite", evalRunBody(t, "p1/m1", "not-a-suite", codingOutcomes(t, true)[:1], nil), http.StatusBadRequest},
		{"unknown field", evalRunBody(t, "p1/m1", "coding", codingOutcomes(t, true)[:1], map[string]any{"prompt": "invent this"}), http.StatusBadRequest},
		{"missing case id", evalRunBody(t, "p1/m1", "coding", nil, map[string]any{"artifacts": []map[string]any{{"status": "ok"}}}), http.StatusBadRequest},
		{"too many artifacts", evalRunBody(t, "p1/m1", "coding", codingOutcomes(t, true), nil), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run", tc.body)
			if rr.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rr.Code, tc.status, rr.Body.String())
			}
		})
	}

	// Oversized payload is refused before any parsing.
	rr := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run", strings.Repeat("a", maxEvaluationRunBodyBytes+1))
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413", rr.Code)
	}

	stats := planeStats(t, s)
	if stats["runs_stored"] != float64(0) {
		t.Fatalf("rejected runs were stored: %#v", stats["runs_stored"])
	}
	if sc := mustMap(t, stats["scorecard_registry"], "registry stats"); sc["deployments"] != float64(0) {
		t.Fatalf("rejections produced scorecards: %#v", sc)
	}
}

// ---------------------------------------------------------------------------
// Imports and state: all-or-nothing, never partially trusted
// ---------------------------------------------------------------------------

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

const validImportedScorecard = `{"scorecards":[
	{"deployment_id":"p1/m1","provider_id":"p1","model":"model-a","version":1,
	 "generated_at":"2026-09-26T12:00:00Z",
	 "values":{"coding":{"score":0.8,"provenance":"imported","sample_count":12,"confidence":0.375,"source":"vendor-bench"}}}]}`

const invalidImportedScorecard = `{"scorecards":[
	{"deployment_id":"p1/m1","provider_id":"p1","model":"model-a","version":1,
	 "generated_at":"2026-09-26T12:00:00Z",
	 "values":{"coding":{"score":0.8,"provenance":"imported","sample_count":12,"source":"vendor-bench"}}},
	{"deployment_id":"p1/m2","provider_id":"p1","model":"model-b","version":1,
	 "generated_at":"2026-09-26T12:00:00Z",
	 "values":{"reasoning":{"score":0.9,"sample_count":9}}}]}`

func TestPhaseH_ImportArtifactIsAllOrNothing(t *testing.T) {
	dir := t.TempDir()
	importPath := filepath.Join(dir, "scorecards.json")
	cfg := phaseHConfig(filepath.Join(dir, "state.json"), importPath)
	s := testGateway(t, cfg) // the artifact does not exist yet

	stats := planeStats(t, s)
	if err, _ := stats["import_error"].(string); err == "" {
		t.Fatalf("a missing import artifact must be reported: %#v", stats)
	}
	if stats["imported_scorecards"] != float64(0) {
		t.Fatalf("missing artifact imported scorecards: %v", stats["imported_scorecards"])
	}

	// A partially invalid artifact rejects the whole import: no value from it may
	// become visible.
	writeFile(t, importPath, invalidImportedScorecard)
	reloadPhaseH(t, s, cfg)
	stats = planeStats(t, s)
	if stats["imported_scorecards"] != float64(0) {
		t.Fatalf("invalid artifact was partially imported: %v", stats["imported_scorecards"])
	}
	if reg := mustMap(t, stats["scorecard_registry"], "registry"); reg["deployments"] != float64(0) {
		t.Fatalf("invalid artifact reached the registry: %#v", reg)
	}
	if err, _ := stats["import_error"].(string); err == "" {
		t.Fatal("rejected import must be visible as import_error")
	}
	rr := adminDo(t, s, http.MethodGet, "/metrics", "")
	if !strings.Contains(rr.Body.String(), "nexaroute_scorecard_import_failures_total ") {
		t.Fatal("import failure metric missing")
	}
	if failures := metricValue(t, rr.Body.String(), "nexaroute_scorecard_import_failures_total"); failures < 2 {
		t.Fatalf("import failures = %v, want >= 2 (missing + invalid artifact)", failures)
	}

	// A valid artifact imports (attribution preserved, provenance mandatory).
	writeFile(t, importPath, validImportedScorecard)
	reloadPhaseH(t, s, cfg)
	stats = planeStats(t, s)
	if stats["imported_scorecards"] != float64(1) {
		t.Fatalf("valid artifact not imported: %#v", stats)
	}
	if err, _ := stats["import_error"].(string); err != "" {
		t.Fatalf("import error survived a good artifact: %q", err)
	}
	rr = adminDo(t, s, http.MethodGet, "/admin/api/scorecards", "")
	body := decodeJSON(t, rr)
	if body["total"] != float64(1) {
		t.Fatalf("imported scorecard missing: %v", body["total"])
	}
	row := mustMap(t, body["scorecards"].([]any)[0], "row")
	v := mustMap(t, row["values"].([]any)[0], "value")
	if v["provenance"] != string(scorecards.ProvenanceImported) || v["source"] != "vendor-bench" {
		t.Fatalf("import provenance lost: %#v", v)
	}
	rr = adminDo(t, s, http.MethodGet, "/metrics", "")
	if !strings.Contains(rr.Body.String(), "nexaroute_scorecard_imported 1") {
		t.Fatal("imported gauge missing")
	}

	// Removing the deployment from config also removes imported evidence.
	cfg2 := cfg
	cfg2.Providers = []config.ProviderConfig{{
		ID: "p1", Name: "P1", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1", AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1}},
	}}
	reloadPhaseH(t, s, cfg2)
	if reg := mustMap(t, planeStats(t, s)["scorecard_registry"], "registry"); reg["deployments"] != float64(0) {
		t.Fatalf("removed deployment kept its imported scorecard: %#v", reg)
	}
}

func TestPhaseH_ImportRespectsRegistryBoundAtomically(t *testing.T) {
	dir := t.TempDir()
	importPath := filepath.Join(dir, "scorecards.json")
	cfg := phaseHConfig(filepath.Join(dir, "state.json"), importPath)
	cfg.Evaluation.MaxScorecards = 1
	s := testGateway(t, cfg)

	// Fill the single scorecard slot with evaluation evidence.
	rr := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run",
		evalRunBody(t, "p1/m1", "coding", codingOutcomes(t, true), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("run status %d: %s", rr.Code, rr.Body.String())
	}

	// An artifact that adds a second deployment must be rejected as a whole: the
	// existing scorecard stays and nothing from the artifact becomes visible.
	artifact := `{"scorecards":[{"deployment_id":"p1/m2","provider_id":"p1","model":"model-b","version":1,
		"generated_at":"2026-09-26T12:00:00Z",
		"values":{"coding":{"score":0.5,"provenance":"imported","sample_count":4,"source":"vendor-bench"}}}]}`
	writeFile(t, importPath, artifact)
	reloadPhaseH(t, s, cfg)

	stats := planeStats(t, s)
	if err, _ := stats["import_error"].(string); !strings.Contains(err, "registry bound") {
		t.Fatalf("over-bound import not reported: %#v", stats)
	}
	if stats["imported_scorecards"] != float64(0) {
		t.Fatalf("over-bound artifact was imported: %v", stats["imported_scorecards"])
	}
	reg := mustMap(t, stats["scorecard_registry"], "registry")
	if reg["deployments"] != float64(1) {
		t.Fatalf("registry changed during a rejected import: %#v", reg)
	}
	if rr := adminDo(t, s, http.MethodGet, "/admin/api/scorecards/p1/m2", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("rejected import created a scorecard: %d %s", rr.Code, rr.Body.String())
	}
	if rr := adminDo(t, s, http.MethodGet, "/admin/api/scorecards/p1/m1", ""); rr.Code != http.StatusOK {
		t.Fatalf("rejected import dropped existing evidence: %d", rr.Code)
	}
}

func TestPhaseH_StateRoundTripAndCorruptionRejectedWholesale(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	cfg := phaseHConfig(statePath, "")
	s := testGateway(t, cfg)

	rr := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run",
		evalRunBody(t, "p1/m1", "coding", codingOutcomes(t, true), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("run status %d: %s", rr.Code, rr.Body.String())
	}

	// A fresh gateway over the same state file sees the evidence.
	s2 := testGateway(t, cfg)
	stats := planeStats(t, s2)
	if stats["runs_stored"] != float64(1) || stats["imported_scorecards"] != float64(0) {
		t.Fatalf("state not reloaded: %#v", stats)
	}
	if reg := mustMap(t, stats["scorecard_registry"], "registry"); reg["deployments"] != float64(1) {
		t.Fatalf("scorecards not reloaded: %#v", reg)
	}

	// Corrupt file: rejected as a whole, nothing from it becomes visible.
	corrupt := []struct {
		name    string
		content string
	}{
		{"invalid JSON", `{"version":1,"runs":[`},
		{"unsupported version", `{"version":99,"runs":[]}`},
		{"invalid run", `{"version":1,"runs":[{"run_id":"r1","suite_id":"","deployment_id":"p1/m1"}]}`},
		{"unknown field", `{"version":1,"runs":[],"mystery":true}`},
		{"provenance-less scorecard", `{"version":1,"runs":[],"scorecards":[{"deployment_id":"p1/m1","version":1,"generated_at":"2026-09-26T12:00:00Z","values":{"coding":{"score":0.5,"sample_count":3}}}]}`},
	}
	for _, tc := range corrupt {
		t.Run(tc.name, func(t *testing.T) {
			writeFile(t, statePath, tc.content)
			s3 := testGateway(t, cfg)
			stats := planeStats(t, s3)
			if err, _ := stats["import_error"].(string); !strings.Contains(err, "evaluation state") {
				t.Fatalf("corrupt state not reported: %#v", stats)
			}
			if stats["runs_stored"] != float64(0) {
				t.Fatalf("corrupt state produced runs: %v", stats["runs_stored"])
			}
			if reg := mustMap(t, stats["scorecard_registry"], "registry"); reg["deployments"] != float64(0) {
				t.Fatalf("corrupt state produced scorecards: %#v", reg)
			}
		})
	}

	// Oversized state file is refused by size before parsing.
	writeFile(t, statePath, `{"version":1,"runs":[]}`)
	if err := os.Truncate(statePath, maxEvaluationStateBytes+1); err != nil {
		t.Fatal(err)
	}
	s4 := testGateway(t, cfg)
	if err, _ := planeStats(t, s4)["import_error"].(string); !strings.Contains(err, "exceeds") {
		t.Fatalf("oversized state not reported: %#v", planeStats(t, s4))
	}
}

func TestPhaseH_ReloadKeepsEvidenceForLiveDeploymentsOnly(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	cfg := phaseHConfig(statePath, "")
	s := testGateway(t, cfg)

	run := func(suite, deployment string) {
		t.Helper()
		rr := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run",
			evalRunBody(t, deployment, suite, codingOutcomes(t, true), nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("run %s status %d: %s", deployment, rr.Code, rr.Body.String())
		}
	}
	run("coding", "p1/m1")
	if total := mustMap(t, planeStats(t, s)["scorecard_registry"], "registry")["deployments"]; total != float64(1) {
		t.Fatalf("scorecard missing before reload: %v", total)
	}

	// Unrelated reload (same state path) must not roll memory back to the file on
	// disk: an operator may have edited the config while evidence was newer.
	writeFile(t, statePath, `{"version":1,"runs":[]}`)
	cfgSame := cfg
	cfgSame.Providers = phaseHProviders()
	cfgSame.Providers[0].Models[0].Weight = 2
	reloadPhaseH(t, s, cfgSame)
	stats := planeStats(t, s)
	if stats["runs_stored"] != float64(1) {
		t.Fatalf("unrelated reload rolled back runs: %#v", stats)
	}
	if reg := mustMap(t, stats["scorecard_registry"], "registry"); reg["deployments"] != float64(1) {
		t.Fatalf("unrelated reload dropped scorecards: %#v", reg)
	}

	// Removing the deployment drops its evidence.
	cfgDrop := cfg
	cfgDrop.Providers = []config.ProviderConfig{{
		ID: "p1", Name: "P1", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1", AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1}},
	}}
	reloadPhaseH(t, s, cfgDrop)
	stats = planeStats(t, s)
	if reg := mustMap(t, stats["scorecard_registry"], "registry"); reg["deployments"] != float64(0) {
		t.Fatalf("removed deployment kept evidence: %#v", reg)
	}
	if stats["runs_stored"] != float64(0) {
		t.Fatalf("removed deployment kept runs: %v", stats["runs_stored"])
	}

	// A changed state path is read (fresh evidence namespace) without discarding
	// what is already in memory.
	run("coding", "p1/m2")
	cfgMove := cfgDrop
	cfgMove.Evaluation.StatePath = filepath.Join(dir, "moved-state.json")
	reloadPhaseH(t, s, cfgMove)
	if total := mustMap(t, planeStats(t, s)["scorecard_registry"], "registry")["deployments"]; total != float64(1) {
		t.Fatalf("state path change discarded live evidence: %v", total)
	}
	// The new state path only materializes once a run persists.
	run("coding", "p1/m2")
	data, err := os.ReadFile(cfgMove.Evaluation.StatePath)
	if err != nil {
		t.Fatalf("moved state file not written: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("moved state file invalid: %v", err)
	}
	if runs, _ := doc["runs"].([]any); len(runs) != 2 {
		t.Fatalf("moved state runs = %#v", doc["runs"])
	}
}

// ---------------------------------------------------------------------------
// Isolation: evaluation cannot touch routing or leak outputs
// ---------------------------------------------------------------------------

func TestPhaseH_EvaluationDoesNotTouchRoutingOrUpstreams(t *testing.T) {
	var upstreamCalls int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c","object":"chat.completion","created":1,"model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer up.Close()

	cfg := phaseHConfig(filepath.Join(t.TempDir(), "state.json"), "")
	cfg.Providers = phaseHProviders()
	cfg.Providers[0].BaseURL = up.URL
	s := testGateway(t, cfg)

	healthBefore := healthByDeployment(s.hm.Snapshot())
	candidatesBefore := s.rt.Candidates(routerRequirement("model-a"))

	rr := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run",
		evalRunBody(t, "p1/m1", "coding", codingOutcomes(t, true), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("run status %d: %s", rr.Code, rr.Body.String())
	}
	if upstreamCalls != 0 {
		t.Fatalf("evaluation called the upstream %d times", upstreamCalls)
	}
	if after := healthByDeployment(s.hm.Snapshot()); !reflect.DeepEqual(healthBefore, after) {
		t.Fatalf("evaluation changed routing health:\nbefore=%#v\nafter=%#v", healthBefore, after)
	}
	candidatesAfter := s.rt.Candidates(routerRequirement("model-a"))
	if len(candidatesBefore) != len(candidatesAfter) {
		t.Fatalf("evaluation changed candidate set: %d -> %d", len(candidatesBefore), len(candidatesAfter))
	}
	for i := range candidatesBefore {
		if candidatesBefore[i].Deployment.ID != candidatesAfter[i].Deployment.ID {
			t.Fatalf("evaluation changed candidate order at %d", i)
		}
	}
}

func TestPhaseH_EventsAndRunsCarryNoModelOutputs(t *testing.T) {
	s := testGateway(t, phaseHConfig(filepath.Join(t.TempDir(), "state.json"), ""))
	const secret = "SECRET-MODEL-OUTPUT-9f2c41"

	rr := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run",
		evalRunBody(t, "p1/m1", "coding", codingOutcomes(t, true), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("run status %d: %s", rr.Code, rr.Body.String())
	}
	var evalEvents int
	for _, ev := range s.bus.Snapshot() {
		if ev.Kind != "eval_run" {
			continue
		}
		evalEvents++
		if strings.Contains(ev.Message, secret) {
			t.Fatalf("event leaked model output: %s", ev.Message)
		}
		encoded, _ := json.Marshal(ev)
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("event JSON leaked model output: %s", encoded)
		}
	}
	if evalEvents == 0 {
		t.Fatal("no eval_run event was emitted")
	}
	rr = adminDo(t, s, http.MethodGet, "/admin/api/evaluation/runs?limit=5", "")
	if strings.Contains(rr.Body.String(), secret) {
		t.Fatal("admin run history leaked model output")
	}
	rr = adminDo(t, s, http.MethodGet, "/admin/api/snapshot", "")
	if strings.Contains(rr.Body.String(), secret) {
		t.Fatal("admin snapshot leaked model output")
	}
}

func TestPhaseH_CloneConfigCarriesEvaluation(t *testing.T) {
	cfg := phaseHConfig("/tmp/phase-h-state.json", "/tmp/phase-h-scorecards.json")
	cfg.Evaluation.LatencyTargetMS = 2500
	cfg.Evaluation.TTFTTargetMS = 900
	clone := cloneConfig(cfg)
	if clone.Evaluation != cfg.Evaluation {
		t.Fatalf("clone lost evaluation config: %+v", clone.Evaluation)
	}
	clone.Evaluation.StatePath = "mutated"
	if cfg.Evaluation.StatePath == "mutated" {
		t.Fatal("clone mutated the source evaluation config")
	}
}

// healthByDeployment indexes the (unordered) health snapshot so a comparison
// cannot fail on map iteration order.
func healthByDeployment(states []health.State) map[string]health.State {
	out := make(map[string]health.State, len(states))
	for _, st := range states {
		out[st.Deployment] = st
	}
	return out
}

func routerRequirement(model string) router.Requirement {
	return router.Requirement{Model: model}
}
