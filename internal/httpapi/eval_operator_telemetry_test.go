package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/eval"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/scorecards"
)

const operatorSuitePackJSON = `{"id":"operator-smoke","version":"1","dimension":"coding","MinSamples":1,` +
	`"cases":[{"id":"op-case-one","weight":1,"expectation":{"kind":"exact_match","expected":"ok"}},` +
	`{"id":"op-case-two","weight":1,"expectation":{"kind":"exact_match","expected":"ok"}}]}`

func operatorArtifacts() []eval.Outcome {
	return []eval.Outcome{
		{CaseID: "op-case-one", Status: eval.OutcomeOK, Output: "ok"},
		{CaseID: "op-case-two", Status: eval.OutcomeOK, Output: "ok"},
	}
}

func phaseHConfigWithSuites(t *testing.T, suitesPath string) config.Config {
	t.Helper()
	cfg := phaseHConfig(filepath.Join(t.TempDir(), "state.json"), "")
	cfg.Evaluation.SuitesPath = suitesPath
	return cfg
}

func suiteIDs(t *testing.T, s *Server) []string {
	t.Helper()
	rr := adminDo(t, s, http.MethodGet, "/admin/api/evaluation/suites", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("suites status %d: %s", rr.Code, rr.Body.String())
	}
	body := decodeJSON(t, rr)
	list, ok := body["suites"].([]any)
	if !ok {
		t.Fatalf("suites missing: %s", rr.Body.String())
	}
	var ids []string
	for _, v := range list {
		m := mustMap(t, v, "suite")
		id, _ := m["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// PR-A: a valid operator pack loads, lists alongside built-ins and runs.
func TestOperatorSuites_LoadListAndRun(t *testing.T) {
	dir := t.TempDir()
	pack := filepath.Join(dir, "pack.json")
	if err := os.WriteFile(pack, []byte(operatorSuitePackJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	s := testGateway(t, phaseHConfigWithSuites(t, pack))

	ids := suiteIDs(t, s)
	if len(ids) != 9 {
		t.Fatalf("suite catalog = %v, want 8 built-ins + 1 operator", ids)
	}
	found := false
	for _, id := range ids {
		if id == "operator-smoke" {
			found = true
		}
	}
	if !found {
		t.Fatalf("operator suite missing from catalog: %v", ids)
	}

	// The operator suite runs like a built-in.
	rr := adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run",
		evalRunBody(t, "p1/m1", "operator-smoke", operatorArtifacts(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("operator run status %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeJSON(t, rr)
	if resp["scorecard_written"] != true {
		t.Fatalf("operator run wrote no scorecard: %s", rr.Body.String())
	}

	// Built-ins are unchanged and still runnable.
	rr = adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run",
		evalRunBody(t, "p1/m1", "coding", codingOutcomes(t, true), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("built-in run status %d: %s", rr.Code, rr.Body.String())
	}
}

// PR-A: a malformed pack refuses the whole load, keeps prior suites and
// never blocks built-ins.
func TestOperatorSuites_MalformedRejectedAtomically(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(operatorSuitePackJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := phaseHConfigWithSuites(t, good)
	s := testGateway(t, cfg)
	if len(suiteIDs(t, s)) != 9 {
		t.Fatal("valid pack did not load")
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"id":`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgBad := cfg
	cfgBad.Evaluation.SuitesPath = bad
	reloadPhaseH(t, s, cfgBad)

	rr := adminDo(t, s, http.MethodGet, "/admin/api/evaluation/suites", "")
	body := decodeJSON(t, rr)
	if errMsg, _ := body["suites_error"].(string); errMsg == "" {
		t.Fatal("malformed pack must surface a suites_error")
	}
	// Previously loaded operator suite is retained (no partial replacement).
	if len(suiteIDs(t, s)) != 9 {
		t.Fatalf("rejected reload changed the catalog: %s", rr.Body.String())
	}
	// Built-ins still run after a rejected reload.
	rr = adminDo(t, s, http.MethodPost, "/admin/api/evaluation/run",
		evalRunBody(t, "p1/m1", "coding", codingOutcomes(t, true), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("built-in run after rejected reload: %d %s", rr.Code, rr.Body.String())
	}
}

// PR-B: ingestion is off by default — traffic writes no production_telemetry.
func TestTelemetryIngest_DisabledByDefault(t *testing.T) {
	cfg := phaseHConfig(filepath.Join(t.TempDir(), "state.json"), "")
	if cfg.Evaluation.TelemetryIngestEnabled {
		t.Fatal("telemetry ingestion must be opt-in: default config enables it")
	}
	s := testGateway(t, cfg)

	s.recordRouteSuccess(router.Requirement{Model: "model-a"}, "p1/m1", "p1", 120*time.Millisecond, "")
	s.recordProviderFailure("p1", "p1/m1", "boom", upstreamFailurePolicy{SignalProvider: true}, "")
	s.syncProductionTelemetry()

	rr := adminDo(t, s, http.MethodGet, "/admin/api/scorecards", "")
	body := decodeJSON(t, rr)
	if body["total"] != float64(0) {
		t.Fatalf("disabled ingestion produced scorecards: %v", body["total"])
	}
	stats := planeStats(t, s)
	if stats["telemetry_ingest_enabled"] != false {
		t.Fatalf("stats must report ingestion disabled: %#v", stats["telemetry_ingest_enabled"])
	}
}

// PR-B: enabling ingestion produces production_telemetry values from
// aggregated stats only — no content, no error text, no user data.
func TestTelemetryIngest_AggregatedValuesOnly(t *testing.T) {
	cfg := phaseHConfig(filepath.Join(t.TempDir(), "state.json"), "")
	cfg.Evaluation.TelemetryIngestEnabled = true
	cfg.Evaluation.TelemetryMinSamples = 5
	cfg.Evaluation.LatencyTargetMS = 1000
	cfg.Evaluation.TTFTTargetMS = 500
	s := testGateway(t, cfg)

	const secretCanary = "CANARY-PROMPT-RESPONSE-9f2c41"
	// 4 successes + 1 failure = 5 samples: availability 0.8, failure 0.2.
	for i := 0; i < 4; i++ {
		s.recordRouteSuccess(router.Requirement{Model: "model-a"}, "p1/m1", "p1", 200*time.Millisecond, "")
	}
	s.recordProviderFailure("p1", "p1/m1", secretCanary, upstreamFailurePolicy{SignalProvider: true}, "")
	s.observeTelemetryTTFT("p1/m1", 100*time.Millisecond)
	s.syncProductionTelemetry()

	rr := adminDo(t, s, http.MethodGet, "/admin/api/scorecards", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("scorecards status %d", rr.Code)
	}
	body := decodeJSON(t, rr)
	if body["total"] != float64(1) {
		t.Fatalf("ingestion wrote no scorecard: %s", rr.Body.String())
	}
	rows, _ := body["scorecards"].([]any)
	row := mustMap(t, rows[0], "row")
	values, _ := row["values"].([]any)
	if len(values) == 0 {
		t.Fatal("telemetry scorecard has no values")
	}
	byDim := map[string]map[string]any{}
	for _, v := range values {
		vm := mustMap(t, v, "value")
		dim, _ := vm["dimension"].(string)
		byDim[dim] = vm
		if vm["provenance"] != string(scorecards.ProvenanceProductionTelemetry) {
			t.Fatalf("telemetry value without production_telemetry provenance: %#v", vm)
		}
	}
	avail, ok := byDim[string(scorecards.DimAvailability)]
	if !ok {
		t.Fatalf("availability missing: %v", byDim)
	}
	if avail["score"] != float64(0.8) {
		t.Fatalf("availability = %v, want 0.8 (4/5 aggregated successes)", avail["score"])
	}
	if fr, ok := byDim[string(scorecards.DimFailureRate)]; !ok {
		t.Fatalf("failure_rate missing: %v", byDim)
	} else if fr["score"] != float64(0.8) {
		t.Fatalf("failure_rate score = %v, want 0.8 (1 - 0.2 aggregated)", fr["score"])
	}
	if lat, ok := byDim[string(scorecards.DimLatencyMS)]; !ok {
		t.Fatalf("latency_ms missing (need latency samples + target): %v", byDim)
	} else if lat["score"] != float64(0.8) {
		t.Fatalf("latency score = %v, want 0.8 (1 - p95 200/target 1000)", lat["score"])
	}
	if ttft, ok := byDim[string(scorecards.DimTTFTMS)]; !ok {
		t.Fatalf("ttft_ms missing (need ttft samples + target): %v", byDim)
	} else if ttft["score"] != float64(0.8) {
		t.Fatalf("ttft score = %v, want 0.8 (1 - 100/500)", ttft["score"])
	}

	// Privacy: the evaluation plane emits aggregated numbers only. The
	// failure reason canary (which the health manager records in its own
	// pre-existing last-error surface) must not appear in anything the
	// evaluation plane writes: scorecards, evaluation sections, metrics or
	// eval events. Telemetry observe APIs accept no string channel at all.
	if strings.Contains(rr.Body.String(), secretCanary) {
		t.Fatal("failure reason leaked into scorecards surface")
	}
	rr = adminDo(t, s, http.MethodGet, "/admin/api/snapshot", "")
	snap := decodeJSON(t, rr)
	for _, section := range []string{"scorecards", "evaluation"} {
		encoded, _ := json.Marshal(snap[section])
		if strings.Contains(string(encoded), secretCanary) {
			t.Fatalf("failure reason leaked into snapshot %s section", section)
		}
	}
	rr = adminDo(t, s, http.MethodGet, "/metrics", "")
	if strings.Contains(rr.Body.String(), secretCanary) {
		t.Fatal("failure reason leaked into metrics")
	}
	for _, ev := range s.bus.Snapshot() {
		encoded, _ := json.Marshal(ev)
		if strings.Contains(string(encoded), secretCanary) {
			t.Fatalf("telemetry path leaked failure text into event: %s", encoded)
		}
	}

	stats := planeStats(t, s)
	if stats["telemetry_ingest_enabled"] != true {
		t.Fatalf("stats must report ingestion enabled: %#v", stats)
	}
}

// PR-B: enabling ingestion changes no routing or health behavior.
func TestTelemetryIngest_DoesNotChangeRoutingOrHealth(t *testing.T) {
	mkServer := func(enabled bool) *Server {
		cfg := phaseHConfig(filepath.Join(t.TempDir(), "state.json"), "")
		cfg.Evaluation.TelemetryIngestEnabled = enabled
		cfg.Evaluation.TelemetryMinSamples = 3
		cfg.Evaluation.LatencyTargetMS = 1000
		return testGateway(t, cfg)
	}
	sOff := mkServer(false)
	sOn := mkServer(true)

	traffic := func(s *Server) {
		for i := 0; i < 4; i++ {
			s.recordRouteSuccess(router.Requirement{Model: "model-a"}, "p1/m1", "p1", 120*time.Millisecond, "")
		}
		s.recordProviderFailure("p1", "p1/m1", "boom", upstreamFailurePolicy{SignalProvider: true}, "")
		s.syncProductionTelemetry()
	}
	traffic(sOff)
	traffic(sOn)

	healthOff := healthByDeployment(sOff.hm.Snapshot())
	healthOn := healthByDeployment(sOn.hm.Snapshot())
	// Compare counters and EWMA values only (timestamps inherently differ).
	for id, off := range healthOff {
		on, ok := healthOn[id]
		if !ok {
			t.Fatalf("deployment %s missing with ingestion on", id)
		}
		if off.Successes != on.Successes || off.Failures != on.Failures ||
			off.EWMALatencyMS != on.EWMALatencyMS || off.Status != on.Status {
			t.Fatalf("health differs with ingestion on:\noff=%+v\non=%+v", off, on)
		}
	}
	cands := func(s *Server) []string {
		var out []string
		for _, c := range s.rt.Candidates(router.Requirement{Model: "model-a"}) {
			out = append(out, c.Deployment.ID)
		}
		return out
	}
	if !reflect.DeepEqual(cands(sOff), cands(sOn)) {
		t.Fatalf("candidate order differs with ingestion on: %v vs %v", cands(sOff), cands(sOn))
	}
}

func scorecardTotal(t *testing.T, s *Server) int {
	t.Helper()
	rr := adminDo(t, s, http.MethodGet, "/admin/api/scorecards", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("scorecards status %d: %s", rr.Code, rr.Body.String())
	}
	body := decodeJSON(t, rr)
	total, ok := body["total"].(float64)
	if !ok {
		t.Fatalf("scorecards missing total: %s", rr.Body.String())
	}
	return int(total)
}

// PR-B defect fix: the data-plane hook must only aggregate. Flushing
// (bucket snapshots, per-bucket percentile sorts, registry upserts and
// state-file persistence) is owned by the periodic loop, never by a request.
func TestTelemetryIngest_HotPathDoesNotFlush(t *testing.T) {
	cfg := phaseHConfig(filepath.Join(t.TempDir(), "state.json"), "")
	cfg.Evaluation.TelemetryIngestEnabled = true
	cfg.Evaluation.TelemetryMinSamples = 5
	cfg.Evaluation.LatencyTargetMS = 1000
	cfg.Evaluation.TTFTTargetMS = 500
	s := testGateway(t, cfg)

	// 4 successes + 1 failure through the real data-plane hooks: enough
	// aggregated samples to flush, yet nothing may materialize until the
	// periodic flush entry runs.
	for i := 0; i < 4; i++ {
		s.recordRouteSuccess(router.Requirement{Model: "model-a"}, "p1/m1", "p1", 200*time.Millisecond, "")
	}
	s.recordProviderFailure("p1", "p1/m1", "boom", upstreamFailurePolicy{SignalProvider: true}, "")

	if got := scorecardTotal(t, s); got != 0 {
		t.Fatalf("hot path flushed telemetry: %d scorecards before any flush", got)
	}

	// The periodic flush entry still materializes the aggregates.
	s.syncProductionTelemetry()
	if got := scorecardTotal(t, s); got != 1 {
		t.Fatalf("periodic flush wrote %d scorecards, want 1", got)
	}
}
