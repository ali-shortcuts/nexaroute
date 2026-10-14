package httpapi

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
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

func TestSnapshotRouteEligibilityIgnoresLimitTruncation(t *testing.T) {
	// Three healthy deployments in the pool, but the snapshot is
	// requested with limit=1: the ordinary deployments list is
	// truncated while route eligibility must still be computed from the
	// complete health snapshot (M3-3 review finding 2).
	cfg := eligTestConfig()
	cfg.Providers = append(cfg.Providers, config.ProviderConfig{
		ID: "c", Name: "C", Type: "openai_compatible", BaseURL: "https://c.example.com/v1", Enabled: true, APIKeyEnv: "K",
		DataHandling: config.DataHandlingConfig{TrainsOnData: "no"},
		Models:       []config.ModelConfig{{ID: "m", Model: "model-c", Enabled: true}},
	})
	cfg.CandidatePools[0].Deployments = []string{"a/m", "b/m", "c/m"}
	s := testGateway(t, cfg)

	rr := adminDo(t, s, http.MethodGet, "/admin/api/snapshot?limit=1", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", rr.Code, rr.Body.String())
	}
	snap := decodeJSON(t, rr)
	deployments, _ := snap["deployments"].([]any)
	if len(deployments) != 1 {
		t.Fatalf("expected limit-truncated deployments list of 1, got %d", len(deployments))
	}
	if snap["snapshot_truncated"] != true {
		t.Fatalf("expected snapshot_truncated=true, got %v", snap["snapshot_truncated"])
	}
	row := profileRow(t, snap, "r1")
	if row["candidates"] != float64(3) || row["eligible_now"] != float64(3) {
		t.Fatalf("eligibility must ignore the output limit: expected 3/3, got %v", row)
	}
	if got := excludedCount(row, "health_not_established"); got != 0 {
		t.Fatalf("truncated health must not fabricate health_not_established, got %v (row %v)", got, row)
	}
	if row["state"] != "ok" {
		t.Fatalf("expected state ok, got %v", row["state"])
	}
}

func TestSnapshotConcurrentWithConfigReplacement(t *testing.T) {
	// Race regression (M3-3 review finding 1): admin snapshots read the
	// config + route resolver pair while settings saves replace both
	// under runtimeMu. Run with -race; the detector is the assertion.
	// Many route profiles widen the per-request resolver-read window so
	// an unsynchronized read cannot hide between lock acquisitions.
	cfg := eligTestConfig()
	cfg.RouteProfiles = nil
	for i := 0; i < 200; i++ {
		cfg.RouteProfiles = append(cfg.RouteProfiles, config.RouteProfileConfig{
			ID: "prof-" + string(rune('a'+i%26)) + "-" + mustJSON(t, i), CandidatePool: "pool",
		})
	}
	s := testGateway(t, cfg)
	routing := mustJSON(t, s.currentConfig().Routing)
	probe := mustJSON(t, s.currentConfig().Probe)
	bodyA := `{"client_base_url":"https://a.example.com","routing":` + routing + `,"probe":` + probe + `}`
	bodyB := `{"client_base_url":"https://b.example.com","routing":` + routing + `,"probe":` + probe + `}`

	// Request volume stays under the per-IP admin token bucket
	// (capacity 90) while still overlapping readers and writers.
	var wg sync.WaitGroup
	errs := make(chan string, 256)
	start := make(chan struct{})
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 8; i++ {
				rr := adminDo(t, s, http.MethodGet, "/admin/api/snapshot", "")
				if rr.Code != http.StatusOK {
					errs <- "snapshot status " + rr.Body.String()
					return
				}
			}
		}()
	}
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			<-start
			for i := 0; i < 15; i++ {
				body := bodyA
				if (i+g)%2 == 1 {
					body = bodyB
				}
				rr := adminDo(t, s, http.MethodPut, "/admin/api/settings", body)
				if rr.Code != http.StatusOK {
					errs <- "settings status " + rr.Body.String()
					return
				}
			}
		}(g)
	}
	close(start)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	// The server must still serve a coherent snapshot afterwards.
	snap := getSnapshot(t, s)
	_ = profileRow(t, snap, "prof-a-0")
}

func TestSnapshotRouteEligibilityCoherentAcrossConfigReplacement(t *testing.T) {
	// Coherence regression (M3-3 final consistency fix): config
	// replacement changes deployment membership (provider b removed and
	// restored). Every snapshot taken during the swaps must reflect ONE
	// runtime generation: for the profile row, eligible_now plus all
	// exclusion buckets must sum to candidates. A view that mixed an
	// old resolver expansion with a newer deployment list would
	// silently drop members and break that sum.
	cfgA := eligTestConfig() // providers a+b, pool [a/m, b/m]
	cfgB := eligTestConfig()
	cfgB.Providers = cfgB.Providers[:1] // provider a only
	cfgB.CandidatePools[0].Deployments = []string{"a/m"}

	s := testGateway(t, cfgA)

	var wg sync.WaitGroup
	errs := make(chan string, 128)
	start := make(chan struct{})
	// Writer: alternate membership-changing configs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 15; i++ {
			cfg := cfgB
			if i%2 == 1 {
				cfg = cfgA
			}
			if err := s.applyConfig(cfg); err != nil {
				errs <- "applyConfig: " + err.Error()
				return
			}
		}
	}()
	// Readers: assert the coherence invariant on every snapshot.
	// (No t.Fatalf inside goroutines; failures go through errs.)
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 8; i++ {
				rr := adminDo(t, s, http.MethodGet, "/admin/api/snapshot", "")
				if rr.Code != http.StatusOK {
					errs <- "snapshot status " + rr.Body.String()
					return
				}
				var snap map[string]any
				if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil {
					errs <- "snapshot JSON: " + err.Error()
					return
				}
				re, _ := snap["route_eligibility"].(map[string]any)
				profiles, _ := re["profiles"].([]any)
				var row map[string]any
				for _, p := range profiles {
					if r, _ := p.(map[string]any); r["profile_id"] == "r1" {
						row = r
					}
				}
				if row == nil {
					errs <- "profile r1 missing from route_eligibility"
					return
				}
				candidates, _ := row["candidates"].(float64)
				if candidates != 1 && candidates != 2 {
					errs <- "candidates neither generation's size"
					return
				}
				eligible, _ := row["eligible_now"].(float64)
				sum := eligible
				if ex, ok := row["excluded"].(map[string]any); ok {
					for _, v := range ex {
						if n, ok := v.(float64); ok {
							sum += n
						}
					}
				}
				if sum != candidates {
					errs <- "incoherent row: eligible+excluded != candidates"
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
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
