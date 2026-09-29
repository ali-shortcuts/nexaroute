package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func testDeployments() []router.Deployment {
	return []router.Deployment{
		{ID: "p1/m1", ProviderID: "p1", ProviderName: "P One", Model: "m1"},
		{ID: "p1/m2", ProviderID: "p1", ProviderName: "P One", Model: "m2"},
	}
}

// Empty: no deployments and no health yields an empty (non-nil) contract
// with no fabricated rows.
func TestNodeTelemetryEmpty(t *testing.T) {
	now := time.Now().UTC()
	out := buildNodeTelemetry(nil, nil, now)
	if out == nil {
		t.Fatal("empty contract must be non-nil (JSON [])")
	}
	if len(out) != 0 {
		t.Fatalf("empty input must yield empty contract, got %+v", out)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "[]" {
		t.Fatalf("empty contract must marshal to [], got %s", raw)
	}
}

// Unknown deployment with no observations: provider/state present with
// source, latency/error/cooldown/last_* omitted (never guessed).
func TestNodeTelemetryUnknownOmitsUnavailable(t *testing.T) {
	now := time.Now().UTC()
	deps := []router.Deployment{{ID: "p1/m1", ProviderID: "p1", ProviderName: "P One"}}
	states := []health.State{{Deployment: "p1/m1", Status: health.Unknown}}
	out := buildNodeTelemetry(deps, states, now)
	if len(out) != 1 {
		t.Fatalf("expected 1 row, got %+v", out)
	}
	row := out[0]
	if row.Provider == nil || row.Provider.Value != "p1" || row.Provider.Source == "" {
		t.Fatalf("provider must carry value+source: %+v", row.Provider)
	}
	if row.Provider.ObservedAt == "" || row.Provider.AgeMS == nil {
		t.Fatalf("provider must carry freshness: %+v", row.Provider)
	}
	if row.State == nil || row.State.Value != "UNKNOWN" || row.State.Source != "health.manager.status" {
		t.Fatalf("unknown state must be UNKNOWN with source: %+v", row.State)
	}
	// No observations yet: freshness timestamps absent for state, metrics omitted.
	if row.State.ObservedAt != "" || row.State.AgeMS != nil {
		t.Fatalf("never-checked state must omit freshness: %+v", row.State)
	}
	if row.LatencyMS != nil {
		t.Fatalf("latency must be omitted when unobserved: %+v", row.LatencyMS)
	}
	if row.ErrorRate != nil {
		t.Fatalf("error rate must be omitted with no traffic: %+v", row.ErrorRate)
	}
	if row.CooldownUntl != nil {
		t.Fatalf("cooldown must be omitted when not cooling: %+v", row.CooldownUntl)
	}
	if row.LastSuccess != nil || row.LastFailure != nil {
		t.Fatalf("last success/failure must be omitted when never observed: %+v", row)
	}
	// JSON omission check.
	raw, _ := json.Marshal(row)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"latency_ms", "error_rate", "cooldown_until", "last_success", "last_failure"} {
		if _, ok := m[absent]; ok {
			t.Fatalf("JSON must omit %q, got %v", absent, m)
		}
	}
}

// Degraded: failure recorded; state/error/last_failure present with
// source+freshness, latency/cooldown omitted.
func TestNodeTelemetryDegraded(t *testing.T) {
	now := time.Now().UTC()
	lastFail := now.Add(-30 * time.Second)
	lastCheck := now.Add(-30 * time.Second)
	states := []health.State{{
		Deployment: "p1/m1", Status: health.Degraded,
		Successes: 0, Failures: 1, ConsecutiveFailures: 1,
		EWMAFailureRate: 1.0,
		LastChecked:     lastCheck, LastFailure: lastFail, LastError: "boom",
	}}
	out := buildNodeTelemetry(testDeployments(), states, now)
	var row *NodeTelemetry
	for i := range out {
		if out[i].Deployment == "p1/m1" {
			row = &out[i]
		}
	}
	if row == nil {
		t.Fatalf("missing p1/m1 row: %+v", out)
	}
	if row.State == nil || row.State.Value != "DEGRADED" || row.State.Source == "" || row.State.ObservedAt == "" || row.State.AgeMS == nil {
		t.Fatalf("degraded state needs value+source+freshness: %+v", row.State)
	}
	if row.ErrorRate == nil || row.ErrorRate.Source != "health.state.ewma_failure_rate" || row.ErrorRate.ObservedAt == "" || row.ErrorRate.AgeMS == nil {
		t.Fatalf("error rate needs source+freshness: %+v", row.ErrorRate)
	}
	if row.LastFailure == nil || row.LastFailure.Source != "health.state.last_failure" || row.LastFailure.ObservedAt == "" || row.LastFailure.AgeMS == nil {
		t.Fatalf("last failure needs source+freshness: %+v", row.LastFailure)
	}
	// Regression guard: raw LastError must never surface in node_telemetry.
	// last_failure carries timestamp/source/freshness only, no error text.
	rawRow, _ := json.Marshal(row)
	if strings.Contains(string(rawRow), "boom") {
		t.Fatalf("last failure must not amplify raw LastError text: %s", rawRow)
	}
	if row.LatencyMS != nil {
		t.Fatalf("latency omitted with no latency sample: %+v", row.LatencyMS)
	}
	if row.CooldownUntl != nil {
		t.Fatalf("degraded must not carry cooldown_until: %+v", row.CooldownUntl)
	}
	if row.LastSuccess != nil {
		t.Fatalf("last success omitted when never succeeded: %+v", row.LastSuccess)
	}
}

// Recovering: half-open with latency + success history; cooldown omitted
// (no bogus deadline), error rate kept from real traffic.
func TestNodeTelemetryRecovering(t *testing.T) {
	now := time.Now().UTC()
	lastOK := now.Add(-10 * time.Second)
	lastCheck := now.Add(-5 * time.Second)
	states := []health.State{{
		Deployment: "p1/m2", Status: health.HalfOpen,
		Successes: 4, Failures: 1,
		EWMALatencyMS: 42.5, EWMAFailureRate: 0.2,
		LastChecked: lastCheck, LastSuccess: lastOK,
	}}
	out := buildNodeTelemetry(testDeployments(), states, now)
	var row *NodeTelemetry
	for i := range out {
		if out[i].Deployment == "p1/m2" {
			row = &out[i]
		}
	}
	if row == nil {
		t.Fatalf("missing p1/m2 row: %+v", out)
	}
	if row.State == nil || row.State.Value != "RECOVERING" {
		t.Fatalf("half_open maps to RECOVERING, got %+v", row.State)
	}
	if row.LatencyMS == nil || row.LatencyMS.Value != 42.5 || row.LatencyMS.Source == "" || row.LatencyMS.AgeMS == nil {
		t.Fatalf("latency needs value+source+freshness: %+v", row.LatencyMS)
	}
	if row.ErrorRate == nil || row.ErrorRate.Value != 0.2 {
		t.Fatalf("error rate kept from real traffic: %+v", row.ErrorRate)
	}
	if row.CooldownUntl != nil {
		t.Fatalf("recovering must omit cooldown_until: %+v", row.CooldownUntl)
	}
	if row.LastSuccess == nil || row.LastSuccess.Source == "" || row.LastSuccess.AgeMS == nil {
		t.Fatalf("last success needs source+freshness: %+v", row.LastSuccess)
	}
	if row.LastFailure != nil {
		t.Fatalf("last failure omitted when never failed: %+v", row.LastFailure)
	}
}

// Cooldown: future deadline present with source+freshness; expired
// deadlines are omitted (normalization never reports a bogus deadline).
func TestNodeTelemetryCooldownAndExpiry(t *testing.T) {
	now := time.Now().UTC()
	active := []health.State{{
		Deployment: "p1/m1", Status: health.Cooldown,
		Successes: 1, Failures: 5,
		LastChecked: now.Add(-time.Second), LastFailure: now.Add(-time.Second),
		CooldownUntil: now.Add(time.Minute),
	}}
	out := buildNodeTelemetry(testDeployments(), active, now)
	var row *NodeTelemetry
	for i := range out {
		if out[i].Deployment == "p1/m1" {
			row = &out[i]
		}
	}
	if row == nil || row.CooldownUntl == nil || row.CooldownUntl.Source != "health.state.cooldown_until" {
		t.Fatalf("active cooldown needs cooldown_until+source: %+v", row)
	}
	expired := []health.State{{
		Deployment: "p1/m1", Status: health.Cooldown,
		LastChecked:   now.Add(-time.Hour),
		CooldownUntil: now.Add(-time.Minute),
	}}
	out2 := buildNodeTelemetry(testDeployments(), expired, now)
	for i := range out2 {
		if out2[i].Deployment == "p1/m1" && out2[i].CooldownUntl != nil {
			t.Fatalf("expired cooldown must be omitted: %+v", out2[i].CooldownUntl)
		}
	}
}

// Builder must not mutate inputs and must not change routing semantics:
// same health input yields byte-identical existing admin health view.
func TestNodeTelemetryDoesNotMutateInputs(t *testing.T) {
	now := time.Now().UTC()
	deps := testDeployments()
	states := []health.State{{Deployment: "p1/m1", Status: health.Degraded, Failures: 1}}
	before, _ := json.Marshal(states)
	_ = buildNodeTelemetry(deps, states, now)
	after, _ := json.Marshal(states)
	if string(before) != string(after) {
		t.Fatalf("builder mutated health states: %s vs %s", before, after)
	}
	viewsBefore, _ := json.Marshal(enrichHealthForAdmin(states))
	_ = buildNodeTelemetry(deps, states, now)
	viewsAfter, _ := json.Marshal(enrichHealthForAdmin(states))
	if string(viewsBefore) != string(viewsAfter) {
		t.Fatal("node telemetry changed the existing admin health view")
	}
}

// API: snapshot exposes additive node_telemetry alongside health without
// changing existing fields; degraded + recovering rows covered end to end.
func TestAdminSnapshotNodeTelemetry(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Name: "P One", Type: "openai_compatible",
		BaseURL: "http://127.0.0.1:1", AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{
			{ID: "m1", Model: "m1", Enabled: true, Weight: 1},
			{ID: "m2", Model: "m2", Enabled: true, Weight: 1},
		},
	}}
	srv := testGateway(t, cfg)
	now := time.Now()
	srv.hm.SetNowFunc(func() time.Time { return now })
	srv.hm.RecordFailure("p1/m1", "boom", 0)
	srv.hm.RecordSuccess("p1/m2", 20*time.Millisecond)
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["health"]; !ok {
		t.Fatal("existing health field must be preserved")
	}
	rows, _ := body["node_telemetry"].([]any)
	if len(rows) == 0 {
		t.Fatalf("node_telemetry must be present and non-empty: keys=%v", keysOf(body))
	}
	found := map[string]map[string]any{}
	for _, r := range rows {
		m, _ := r.(map[string]any)
		if dep, _ := m["deployment"].(string); dep != "" {
			found[dep] = m
		}
	}
	deg, ok := found["p1/m1"]
	if !ok {
		t.Fatalf("node_telemetry missing degraded p1/m1: %+v", found)
	}
	for _, kpi := range []string{"state", "error_rate", "last_failure", "provider"} {
		m, _ := deg[kpi].(map[string]any)
		if m == nil || m["value"] == nil || m["source"] == nil {
			t.Fatalf("degraded %q needs value+source: %+v", kpi, deg[kpi])
		}
	}
	if _, ok := deg["cooldown_until"]; ok {
		t.Fatalf("degraded single failure must omit cooldown_until: %+v", deg)
	}
	rec, ok := found["p1/m2"]
	if !ok {
		t.Fatalf("node_telemetry missing recovering p1/m2: %+v", found)
	}
	if st, _ := rec["state"].(map[string]any); st["value"] != "HEALTHY" {
		t.Fatalf("p1/m2 after success must be HEALTHY: %+v", rec["state"])
	}
	if lat, _ := rec["latency_ms"].(map[string]any); lat == nil || lat["source"] == nil {
		t.Fatalf("p1/m2 latency needs source: %+v", rec["latency_ms"])
	}
	if _, ok := rec["cooldown_until"]; ok {
		t.Fatalf("healthy row must omit cooldown_until: %+v", rec)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Contract test: frontend popover view-model helper consumes only
// node_telemetry rows and omits absent KPIs (mirrors web helper logic).
func TestNodeTelemetryViewModelOmitsAbsent(t *testing.T) {
	raw := `[{"deployment":"p1/m1","state":{"value":"DEGRADED","source":"health.manager.status"},"last_failure":{"value":"2026-09-29T00:00:00Z","source":"health.state.last_failure"}}]`
	var rows []NodeTelemetry
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].LatencyMS != nil || rows[0].CooldownUntl != nil {
		t.Fatalf("absent KPIs must decode to nil (omitted): %+v", rows[0])
	}
	// Serialization round-trip must still omit them.
	out, _ := json.Marshal(rows[0])
	s := string(out)
	for _, absent := range []string{"latency_ms", "cooldown_until", "last_success", "error_rate"} {
		if strings.Contains(s, absent) {
			t.Fatalf("round-trip must omit %q: %s", absent, s)
		}
	}
}

// Regression: raw LastError (possibly upstream-controlled response text) must
// never appear in node_telemetry JSON or the node popover output, while the
// last-failure timestamp/source/freshness contract is preserved and no
// replacement message is fabricated.
func TestNodeTelemetryNeverExposesRawLastError(t *testing.T) {
	const canary = "CANARY_NT_7f3a9c1e_UPSTREAM_RESPONSE_BODY_DO_NOT_DISPLAY_<sk>secret</sk>"
	now := time.Now().UTC()
	lastFail := now.Add(-30 * time.Second)
	states := []health.State{{
		Deployment: "p1/m1", Status: health.Degraded,
		Successes: 0, Failures: 1, ConsecutiveFailures: 1,
		EWMAFailureRate: 1.0,
		LastChecked:     lastFail, LastFailure: lastFail, LastError: canary,
	}}
	out := buildNodeTelemetry(testDeployments(), states, now)
	if len(out) == 0 {
		t.Fatal("expected node_telemetry rows")
	}
	var row *NodeTelemetry
	for i := range out {
		if out[i].Deployment == "p1/m1" {
			row = &out[i]
		}
	}
	if row == nil {
		t.Fatalf("missing p1/m1 row: %+v", out)
	}
	// Timestamp/source/freshness for last failure must be preserved from real data.
	if row.LastFailure == nil {
		t.Fatal("last_failure must be present when LastFailure is set")
	}
	if row.LastFailure.Value == "" || row.LastFailure.Source != "health.state.last_failure" {
		t.Fatalf("last_failure must carry real timestamp+source: %+v", row.LastFailure)
	}
	if row.LastFailure.ObservedAt == "" || row.LastFailure.AgeMS == nil {
		t.Fatalf("last_failure must carry freshness: %+v", row.LastFailure)
	}
	// No raw error text anywhere in the new node_telemetry JSON.
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), canary) {
		t.Fatalf("node_telemetry JSON must never contain raw LastError canary: %s", raw)
	}
	if strings.Contains(string(raw), `"detail"`) {
		t.Fatalf("node_telemetry JSON must not contain a detail field: %s", raw)
	}
	// The new popover renderer must not amplify/display raw error text.
	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	popover := string(js)
	// Extract only the B3 popover renderer to avoid false positives from
	// unrelated UI (e.g. capability detail lines).
	start := strings.Index(popover, "function nodeTelemetryRowHTML")
	if start < 0 {
		t.Fatal("app.js missing nodeTelemetryRowHTML")
	}
	end := strings.Index(popover[start:], "\nfunction showNodeTelemetryPopover")
	if end < 0 {
		t.Fatal("app.js popover renderer boundary not found")
	}
	renderer := popover[start : start+end]
	if strings.Contains(renderer, "last_failure.detail") || strings.Contains(renderer, "lastFailure.detail") {
		t.Fatalf("popover renderer must not read last_failure detail")
	}
	if strings.Contains(renderer, canary) {
		t.Fatalf("popover renderer must not embed raw error canary")
	}
	if strings.Contains(renderer, "nt-err") {
		t.Fatalf("popover renderer must not render a raw-error element (nt-err)")
	}
}

// End-to-end: admin snapshot node_telemetry must preserve last_failure
// timestamp/source/freshness without leaking raw error text.
func TestAdminSnapshotNodeTelemetryHidesRawLastError(t *testing.T) {
	const canary = "CANARY_NT_SNAPSHOT_4b8d2f6a_UPSTREAM_ERROR_BODY_DO_NOT_DISPLAY"
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Name: "P One", Type: "openai_compatible",
		BaseURL: "http://127.0.0.1:1", AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{
			{ID: "m1", Model: "m1", Enabled: true, Weight: 1},
		},
	}}
	srv := testGateway(t, cfg)
	now := time.Now()
	srv.hm.SetNowFunc(func() time.Time { return now })
	srv.hm.RecordFailure("p1/m1", canary, 0)
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d", rr.Code)
	}
	raw := rr.Body.String()
	if strings.Contains(raw, canary) {
		// The canary may legitimately exist in the pre-existing health view;
		// it must never appear inside the new node_telemetry section.
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		ntRaw, _ := json.Marshal(body["node_telemetry"])
		if strings.Contains(string(ntRaw), canary) {
			t.Fatalf("snapshot node_telemetry must never contain raw LastError canary: %s", ntRaw)
		}
		var body2 map[string]any
		_ = body2
	} else {
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		ntRaw, _ := json.Marshal(body["node_telemetry"])
		if strings.Contains(string(ntRaw), `"detail"`) {
			t.Fatalf("snapshot node_telemetry must not contain detail: %s", ntRaw)
		}
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	rows, _ := body["node_telemetry"].([]any)
	found := false
	for _, r := range rows {
		m, _ := r.(map[string]any)
		if m["deployment"] == "p1/m1" {
			found = true
			lf, _ := m["last_failure"].(map[string]any)
			if lf == nil || lf["value"] == nil || lf["source"] == nil {
				t.Fatalf("last_failure timestamp/source must be preserved: %+v", m)
			}
			if _, ok := lf["detail"]; ok {
				t.Fatalf("last_failure must not carry detail: %+v", lf)
			}
		}
	}
	if !found {
		t.Fatalf("missing p1/m1 in node_telemetry: %v", rows)
	}
}
