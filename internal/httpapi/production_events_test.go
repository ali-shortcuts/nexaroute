package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
)

func TestProductionEventKindsAreExact(t *testing.T) {
	want := []string{
		"model_healthy", "model_failed", "model_recovered", "model_cooldown",
		"provider_rate_limited", "route_changed", "request_failover", "recovery_failed",
	}
	got := append([]string(nil), productionEventKinds...)
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("production kinds=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("production kinds=%v want %v", got, want)
		}
	}
	if len(events.ProductionEventKinds) != 8 {
		t.Fatalf("events.ProductionEventKinds=%v want 8 entries", events.ProductionEventKinds)
	}
}

// TestAdminHealthViewHasAdditiveFields verifies the per-deployment admin
// contract: circuit_state, state, last_success, last_failure,
// consecutive_failures, average_latency, recent_error_rate, cooldown_until.
func TestAdminHealthViewHasAdditiveFields(t *testing.T) {
	st := health.State{
		Deployment:          "p/m",
		Status:              health.Cooldown,
		ConsecutiveFailures: 3,
		EWMALatencyMS:       12.5,
		EWMAFailureRate:     0.5,
		LastSuccess:         time.Now().Add(-time.Minute).UTC(),
		LastFailure:         time.Now().UTC(),
		CooldownUntil:       time.Now().Add(time.Minute).UTC(),
	}
	views := enrichHealthForAdmin([]health.State{st})
	if len(views) != 1 {
		t.Fatal("expected one view")
	}
	raw, err := json.Marshal(views[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"circuit_state", "state", "last_success", "last_failure",
		"consecutive_failures", "average_latency", "recent_error_rate", "cooldown_until",
	} {
		if _, ok := m[field]; !ok {
			t.Fatalf("admin health view missing %q: %v", field, m)
		}
	}
	if m["circuit_state"] != "OPEN" {
		t.Fatalf("circuit_state=%v want OPEN", m["circuit_state"])
	}
	if m["state"] != "COOLDOWN" {
		t.Fatalf("state=%v want COOLDOWN", m["state"])
	}
	if m["average_latency"] != 12.5 {
		t.Fatalf("average_latency=%v want 12.5", m["average_latency"])
	}
	if m["recent_error_rate"] != 0.5 {
		t.Fatalf("recent_error_rate=%v want 0.5", m["recent_error_rate"])
	}
	// Existing internal status must be preserved for decision-engine compat.
	if m["status"] != "cooldown" {
		t.Fatalf("status=%v want cooldown (preserved)", m["status"])
	}
}

// TestAdminSnapshotHealthHasAllFields exercises the live snapshot endpoint.
func TestAdminSnapshotHealthHasAllFields(t *testing.T) {
	cfg := config.Default()
	srv := testGateway(t, cfg)
	srv.hm.RecordFailure("p/m", "boom", time.Millisecond)
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
	rows, _ := body["health"].([]any)
	if len(rows) == 0 {
		t.Fatal("expected at least one health row")
	}
	row, _ := rows[0].(map[string]any)
	for _, field := range []string{
		"circuit_state", "state", "last_success", "last_failure",
		"consecutive_failures", "average_latency", "recent_error_rate",
	} {
		if _, ok := row[field]; !ok {
			t.Fatalf("snapshot health row missing %q: %v", field, row)
		}
	}
}
