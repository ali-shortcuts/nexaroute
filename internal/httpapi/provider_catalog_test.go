package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Issue #394 provider catalog / model onboarding contracts: the presets
// endpoint must be a searchable, categorized catalog backed by real
// implemented dialects; custom endpoints must persist without any models
// discovery; discovery is bounded and cancellable; and model provenance
// (manual/discovered, untested/passed/failed/degraded) is persisted and
// surfaced through the provider summaries.

func presetIDs(t *testing.T, raw []map[string]any) []string {
	t.Helper()
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		id, _ := p["id"].(string)
		out = append(out, id)
	}
	return out
}

func fetchPresets(t *testing.T, s *Server, query string) (raw []map[string]any, categories []string, code int) {
	t.Helper()
	rr := adminRequest(s, http.MethodGet, "/admin/api/provider-presets"+query, "")
	var out map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		return nil, nil, rr.Code
	}
	var list []map[string]any
	if b, ok := out["presets"]; ok {
		if err := json.Unmarshal(b, &list); err != nil {
			return nil, nil, rr.Code
		}
	}
	var cats []string
	if b, ok := out["categories"]; ok {
		_ = json.Unmarshal(b, &cats)
	}
	return list, cats, rr.Code
}

func TestAdminProviderPresetsCatalogSearchAndCategory(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = nil
	s := testGateway(t, cfg)

	all, categories, code := fetchPresets(t, s, "")
	if code != http.StatusOK || len(all) < 10 {
		t.Fatalf("GET presets status=%d count=%d", code, len(all))
	}
	if len(categories) < 4 {
		t.Fatalf("expected derived categories, got %v", categories)
	}
	// The response must surface the three catalog concepts explicitly.
	first := all[0]
	for _, key := range []string{"id", "name", "category", "type", "protocol", "status", "capabilities"} {
		if _, ok := first[key]; !ok {
			t.Errorf("preset entry missing %q", key)
		}
	}

	for _, tc := range []struct {
		name, query, wantID string
		wantCountAtLeast    int
	}{
		{"search: gemini", "?q=gemini", "google-gemini", 1},
		{"search: vertex", "?q=vertex", "google-vertex", 1},
		{"search: azure", "?q=azure", "azure-openai", 1},
		{"search: bedrock", "?q=bedrock", "amazon-bedrock", 1},
		{"search: responses", "?q=responses", "openai-responses", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list, _, code := fetchPresets(t, s, tc.query)
			if code != http.StatusOK || len(list) < tc.wantCountAtLeast || !containsString(presetIDs(t, list), tc.wantID) {
				t.Fatalf("%s -> status=%d ids=%v", tc.query, code, presetIDs(t, list))
			}
		})
	}

	// Category filter uses exact categories and composes with the search.
	local, _, code := fetchPresets(t, s, "?category=Local")
	if code != http.StatusOK || len(local) == 0 {
		t.Fatalf("category Local filter empty (code=%d)", code)
	}
	for _, p := range local {
		if cat, _ := p["category"].(string); cat != "Local" {
			t.Fatalf("category leak: %v", p)
		}
	}
	none, _, _ := fetchPresets(t, s, "?category=NoSuch")
	if len(none) != 0 {
		t.Fatalf("unknown category leaked entries: %d", len(none))
	}
	// Invalid search input must not 500 (best-effort catalog).
	if _, _, code := fetchPresets(t, s, "?q="+"%zz"); code != http.StatusOK {
		t.Fatalf("weird query broke the catalog: %d", code)
	}
}

// A custom endpoint must be configurable and retained even when it exposes
// no /models resource at all, and manual models must persist with their
// own provenance states.
func TestAdminCustomEndpointPersistsWithoutDiscovery(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = nil
	s := testGateway(t, cfg)

	body := `{"provider":{"id":"custom-lab","name":"Custom Lab","type":"openai_compatible","base_url":"http://127.0.0.1:9/noapi","auth_mode":"bearer","enabled":true,"models":[{"id":"hand-entered","model":"hand-entered","enabled":true,"weight":1,"capabilities":{"streaming":true}}],"data_handling":{"trains_on_data":"no","retention":"none"}},"preserve_secret":false}`
	rr := adminRequest(s, http.MethodPost, "/admin/api/providers", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("POST status=%d body=%s", rr.Code, rr.Body.String())
	}
	// No discovery call has ever been made.

	rr = adminRequest(s, http.MethodGet, "/admin/api/providers/custom-lab", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", rr.Code, rr.Body.String())
	}
	var detail struct {
		Provider config.ProviderConfig `json:"provider"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Provider.BaseURL != "http://127.0.0.1:9/noapi" {
		t.Fatalf("base url not persisted: %+v", detail.Provider)
	}
}

// The summary layer must distinguish onboarding states: manual vs
// discovered models and untested/passed/failed/degraded test states.
func TestAdminProviderSummarySurfacesModelProvenance(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p", Name: "P", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		Enabled: true, AuthMode: "bearer",
		Models: []config.ModelConfig{
			{ID: "m1", Model: "m-one", Enabled: true, Weight: 1},
			{ID: "m2", Model: "m-two", Enabled: true, Weight: 1, Source: config.ModelSourceDiscovered},
			{ID: "m3", Model: "m-three", Enabled: true, Weight: 1, Source: config.ModelSourceDiscovered, TestState: config.ModelTestPassed},
			{ID: "m4", Model: "m-four", Enabled: true, Weight: 1, TestState: config.ModelTestDegraded},
			{ID: "m5", Model: "m-five", Enabled: true, Weight: 1, TestState: config.ModelTestFailed},
		},
	}}
	s := testGateway(t, cfg)

	rr := adminRequest(s, http.MethodGet, "/admin/api/providers", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET providers status=%d", rr.Code)
	}
	var out struct {
		Providers []struct {
			ID           string           `json:"id"`
			Models       []string         `json:"models"`
			ModelStates  map[string]int   `json:"model_states"`
			ModelDetails []map[string]any `json:"model_details"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	p := out.Providers[0]
	wantStates := map[string]int{"manual": 3, "discovered": 2, "count": 5, "passed": 1, "failed": 1, "degraded": 1, "untested": 2, "tested": 2}
	for k, want := range wantStates {
		if got := p.ModelStates[k]; got != want {
			t.Errorf("model_states[%q]=%d want %d (full: %v)", k, got, want, p.ModelStates)
		}
	}
	foundp := false
	for _, d := range p.ModelDetails {
		if d["model"] == "m-three" {
			foundp = true
			if d["source"] != "discovered" || d["test_state"] != "passed" {
				t.Fatalf("m-three details = %v", d)
			}
		}
	}
	if !foundp {
		t.Fatal("m-three missing from model_details")
	}

	// Provenance persists across a save round-trip via the PUT path.
	put := `{"provider":{"id":"p","name":"P","type":"openai_compatible","base_url":"http://127.0.0.1:9","auth_mode":"bearer","enabled":true,"models":[{"id":"m2","model":"m-two","enabled":true,"weight":1,"capabilities":{},"source":"discovered","test_state":"passed"}]},"preserve_secret":true,"preserve_headers":true,"preserve_proxy":true}`
	rr = adminRequest(s, http.MethodPut, "/admin/api/providers/p", put)
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rr.Code, rr.Body.String())
	}
	rr = adminRequest(s, http.MethodGet, "/admin/api/providers/p", "")
	var detail struct {
		Provider config.ProviderConfig `json:"provider"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Provider.Models) != 1 {
		t.Fatalf("expected single model after PUT, got %d", len(detail.Provider.Models))
	}
	m := detail.Provider.Models[0]
	if m.Source != config.ModelSourceDiscovered || m.TestState != config.ModelTestPassed {
		t.Fatalf("provenance not preserved: %+v", m)
	}
}

// Saving the same deployment id twice is rejected (duplicate model ids are
// invalid, not silently deduplicated).
func TestAdminSaveRejectsDuplicateDeploymentIDs(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)
	body := `{"provider":{"id":"dup","name":"Dup","type":"openai_compatible","base_url":"http://127.0.0.1:9","enabled":true,"models":[{"id":"same","model":"same","enabled":true,"weight":1},{"id":"same","model":"same","enabled":true,"weight":1}]}}`
	rr := adminRequest(s, http.MethodPost, "/admin/api/providers", body)
	if rr.Code == http.StatusCreated {
		t.Fatalf("duplicate deployment accepted: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "duplicate deployment") {
		t.Fatalf("expected duplicate deployment error, got %d %s", rr.Code, rr.Body.String())
	}
}

// Model discovery is best-effort: a failing inventory endpoint returns a
// 200 with ok=false + models [] and must not corrupt the provider config.
func TestAdminProviderDiscoverFailureIsBestEffort(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r
		http.Error(w, `{"error":"locked"}`, http.StatusForbidden)
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = nil
	s := testGateway(t, cfg)

	form := fmt.Sprintf(`{"provider":{"id":"lab","name":"Lab","type":"openai_compatible","base_url":%q,"auth_mode":"bearer","enabled":false}}`, up.URL)
	rr := adminRequest(s, http.MethodPost, "/admin/api/provider-discover", form)
	if rr.Code != http.StatusOK {
		t.Fatalf("discover status=%d body=%s", rr.Code, rr.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res["ok"] != false {
		t.Fatalf("discovery must fail quietly, got %v", res)
	}
	models, _ := res["models"].([]any)
	if len(models) != 0 {
		t.Fatalf("failed discovery must not invent models: %v", res)
	}
	if res["error"] == "" {
		t.Fatal("failure diagnostics missing")
	}

	// Discovery stays best-effort: the provider without a working
	// inventory endpoint can still be saved immediately, including with
	// a manually added model.
	form = fmt.Sprintf(`{"provider":{"id":"lab","name":"Lab","type":"openai_compatible","base_url":%q,"auth_mode":"bearer","enabled":true,"models":[{"id":"manual-one","model":"manual-one","enabled":true,"weight":1}],"data_handling":{"trains_on_data":"no","retention":"none"}}}`, up.URL)
	saved := adminRequest(s, http.MethodPost, "/admin/api/providers", form)
	if saved.Code != http.StatusCreated {
		t.Fatalf("save after failed discovery status=%d body=%s", saved.Code, saved.Body.String())
	}
	var savedOut struct {
		Provider struct {
			ModelStates map[string]int `json:"model_states"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(saved.Body.Bytes(), &savedOut); err != nil {
		t.Fatal(err)
	}
	if savedOut.Provider.ModelStates["manual"] != 1 || savedOut.Provider.ModelStates["count"] != 1 {
		t.Fatalf("manual model not counted: %v", savedOut.Provider.ModelStates)
	}
}

// Discovery is bounded and cancellable: a hanging inventory endpoint must
// stop when the caller cancels the request context, and never outlive the
// 8-second bounded timeout.
func TestDiscoverModelsIsCancellable(t *testing.T) {
	release := make(chan struct{}, 1)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() { release <- struct{}{}; up.Close() }()

	p := config.ProviderConfig{
		ID: "hang", Name: "Hang", Type: "openai_compatible",
		BaseURL: up.URL, AuthMode: "bearer", Enabled: true, MaxConcurrency: 1,
	}
	p.ApplyDefaults()

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, _, err := discoverModels(ctx, p)
	if err == nil {
		t.Fatal("canceled discovery must return an error")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("canceled discovery took %v; not cancellable", d)
	}
}
