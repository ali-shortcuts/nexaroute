package httpapi

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/health"
	"github.com/ali-shortcuts/nexaroute/internal/probe"
	"github.com/ali-shortcuts/nexaroute/internal/providers"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

func eligTestConfig() config.Config {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{
		{ID: "a", Name: "A", Type: "openai_compatible", BaseURL: "https://a.example.com/v1", Enabled: true, APIKeyEnv: "K",
			DataHandling: config.DataHandlingConfig{TrainsOnData: "no"},
			Models:       []config.ModelConfig{{ID: "m", Model: "model-a", Enabled: true}}},
		{ID: "b", Name: "B", Type: "openai_compatible", BaseURL: "https://b.example.com/v1", Enabled: true, APIKeyEnv: "K",
			DataHandling: config.DataHandlingConfig{TrainsOnData: "yes"},
			Models:       []config.ModelConfig{{ID: "m", Model: "model-b", Enabled: true}}},
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"a/m", "b/m"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "r1", CandidatePool: "pool"}}
	return cfg
}

// gatewayWithoutProbes builds a server like testGateway but never records
// health successes, so deployments stay in the Unknown state.
func gatewayWithoutProbes(t *testing.T, cfg config.Config) *Server {
	t.Helper()
	cfg.ApplyDefaults()
	hm := health.New(cfg.Routing.FailureThreshold, cfg.Cooldown())
	hm.ConfigureProviderIncidents(cfg.Routing.ProviderFailureThreshold, cfg.ProviderFailureWindow(), cfg.ProviderCooldown())
	reg, err := providers.NewRegistry(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rt := router.New(cfg, hm)
	bus := events.New(100)
	pe := probe.New(cfg, reg, rt, hm, bus)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, dir+"/config.json", reg, rt, hm, bus, pe, log.New(io.Discard, "", 0))
	if err := srv.SecurityStoreError(); err != nil {
		t.Fatalf("security store: %v", err)
	}
	t.Cleanup(func() { _ = srv.CloseSecurityStore() })
	return srv
}

func getSnapshot(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rr := adminDo(t, s, http.MethodGet, "/admin/api/snapshot", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", rr.Code, rr.Body.String())
	}
	return decodeJSON(t, rr)
}

func profileRow(t *testing.T, snap map[string]any, profileID string) map[string]any {
	t.Helper()
	re, ok := snap["route_eligibility"].(map[string]any)
	if !ok {
		t.Fatalf("snapshot missing route_eligibility: %v", snap["route_eligibility"])
	}
	if note, _ := re["note"].(string); !strings.Contains(note, "not applied") {
		t.Fatalf("route_eligibility must carry the request-filter caveat note, got %q", note)
	}
	profiles, ok := re["profiles"].([]any)
	if !ok || len(profiles) == 0 {
		t.Fatalf("route_eligibility.profiles missing/empty: %v", re["profiles"])
	}
	for _, p := range profiles {
		row, _ := p.(map[string]any)
		if row["profile_id"] == profileID {
			return row
		}
	}
	t.Fatalf("profile %s not in route_eligibility: %v", profileID, profiles)
	return nil
}

func excludedCount(row map[string]any, key string) float64 {
	ex, _ := row["excluded"].(map[string]any)
	v, _ := ex[key].(float64)
	return v
}

func TestSnapshotRouteEligibilityHealthyAndCompat(t *testing.T) {
	s := testGateway(t, eligTestConfig())
	snap := getSnapshot(t, s)

	// Backward compatibility: pre-existing fields remain.
	for _, field := range []string{"deployments", "health", "health_counts", "provider_health", "version"} {
		if _, ok := snap[field]; !ok {
			t.Fatalf("snapshot lost pre-existing field %q", field)
		}
	}
	// Static warnings surface, empty for this healthy config.
	warnings, ok := snap["eligibility_warnings"].([]any)
	if !ok {
		t.Fatalf("eligibility_warnings missing or not an array: %T", snap["eligibility_warnings"])
	}
	if len(warnings) != 0 {
		t.Fatalf("healthy config must have no eligibility warnings, got %v", warnings)
	}

	row := profileRow(t, snap, "r1")
	if row["candidates"] != float64(2) || row["eligible_now"] != float64(2) {
		t.Fatalf("expected 2 candidates / 2 eligible, got %v", row)
	}
	if row["state"] != "ok" {
		t.Fatalf("expected state ok, got %v", row["state"])
	}
	if row["candidate_pool"] != "pool" {
		t.Fatalf("expected candidate_pool pool, got %v", row["candidate_pool"])
	}
}

func TestSnapshotRouteEligibilityCooldownExclusion(t *testing.T) {
	s := testGateway(t, eligTestConfig())
	// Drive deployment b/m into cooldown via the health manager
	// (failure threshold is 5 in the default routing config).
	for i := 0; i < 5; i++ {
		s.hm.RecordFailure("b/m", "boom", time.Millisecond)
	}
	snap := getSnapshot(t, s)
	row := profileRow(t, snap, "r1")
	if got := excludedCount(row, "cooldown"); got != 1 {
		t.Fatalf("expected 1 cooldown exclusion, got %v (row %v)", got, row)
	}
	if row["eligible_now"] != float64(1) {
		t.Fatalf("expected 1 eligible after cooldown, got %v", row)
	}
	if row["state"] != "degraded" {
		t.Fatalf("expected state degraded, got %v", row["state"])
	}
}

func TestSnapshotRouteEligibilityPendingHealth(t *testing.T) {
	// ready_mesh with no completed probes: deployments are Unknown and
	// must be reported as health_not_established, never as eligible and
	// never conflated with failures.
	s := gatewayWithoutProbes(t, eligTestConfig())
	snap := getSnapshot(t, s)
	row := profileRow(t, snap, "r1")
	if row["candidates"] != float64(2) {
		t.Fatalf("expected 2 candidates, got %v", row)
	}
	if got := excludedCount(row, "health_not_established"); got != 2 {
		t.Fatalf("expected 2 health_not_established, got %v (row %v)", got, row)
	}
	if row["eligible_now"] != float64(0) || row["state"] != "unavailable" {
		t.Fatalf("expected 0 eligible / unavailable, got %v", row)
	}
}

func TestSnapshotRouteEligibilityPrivacyFiltered(t *testing.T) {
	cfg := eligTestConfig()
	cfg.RouteProfiles[0].Privacy = "no_training"
	s := testGateway(t, cfg)
	snap := getSnapshot(t, s)
	row := profileRow(t, snap, "r1")
	if row["privacy_satisfying"] != float64(1) {
		t.Fatalf("expected 1 privacy-satisfying deployment, got %v", row)
	}
	if got := excludedCount(row, "privacy_filtered"); got != 1 {
		t.Fatalf("expected 1 privacy_filtered exclusion, got %v (row %v)", got, row)
	}
	if row["eligible_now"] != float64(1) {
		t.Fatalf("expected 1 eligible, got %v", row)
	}
}

func TestSnapshotEligibilityWarningsExposed(t *testing.T) {
	cfg := eligTestConfig()
	cfg.CandidatePools[0].Deployments = []string{"ghost/model"}
	s := testGateway(t, cfg)
	snap := getSnapshot(t, s)
	warnings, _ := snap["eligibility_warnings"].([]any)
	found := false
	for _, w := range warnings {
		if str, _ := w.(string); strings.Contains(str, "candidate_pools[pool]") {
			found = true
		}
	}
	if !found {
		t.Fatalf("snapshot must expose the static pool warning, got %v", snap["eligibility_warnings"])
	}
	row := profileRow(t, snap, "r1")
	if row["candidates"] != float64(0) || row["state"] != "empty" {
		t.Fatalf("expected empty profile state, got %v", row)
	}
}

func TestSettingsPutExposesEligibilityWarnings(t *testing.T) {
	cfg := eligTestConfig()
	cfg.CandidatePools[0].Deployments = []string{"ghost/model"}
	s := testGateway(t, cfg)
	body := `{"client_base_url":"","routing":` + mustJSON(t, cfg.Routing) + `,"probe":` + mustJSON(t, cfg.Probe) + `}`
	rr := adminDo(t, s, http.MethodPut, "/admin/api/settings", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("settings PUT status=%d body=%s", rr.Code, rr.Body.String())
	}
	out := decodeJSON(t, rr)
	warnings, ok := out["eligibility_warnings"].([]any)
	if !ok || len(warnings) == 0 {
		t.Fatalf("settings save response must expose eligibility warnings, got %v", out)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
