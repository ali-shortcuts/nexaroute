package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func settingsRequest(t *testing.T, s *Server, method, body string) *httptest.ResponseRecorder {
	return settingsRequestWithKey(t, s, method, body, "k")
}

func settingsRequestWithKey(t *testing.T, s *Server, method, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://gateway/admin/api/settings", strings.NewReader(body))
	req.Header.Set("x-admin-key", key)
	req.Header.Set("content-type", "application/json")
	if body == "" {
		req.Body = http.NoBody
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func TestAdminSettingsGetReturnsAllSections(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.APIKey = "k"
	cfg.Admin.BindLocalOnly = false // httptest clients are not loopback
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)

	rr := settingsRequest(t, s, http.MethodGet, "")
	if rr.Code != 200 {
		t.Fatalf("GET settings status = %d, body=%s", rr.Code, rr.Body.String())
	}
	var form settingsForm
	if err := json.Unmarshal(rr.Body.Bytes(), &form); err != nil {
		t.Fatal(err)
	}
	if form.Routing.Strategy == "" || form.Logging.MaxSizeMB == 0 || form.Listen == "" {
		t.Fatalf("settings GET returned incomplete sections: %+v", form)
	}
	if form.Admin.APIKey != "k" {
		t.Fatalf("settings GET should expose the stored admin key to an authorized admin, got %q", form.Admin.APIKey)
	}
}

func TestAdminSettingsPutPersistsLoggingAndAdmin(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.APIKey = "k"
	cfg.Admin.BindLocalOnly = false // httptest clients are not loopback
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)

	body := `{"routing":{"strategy":"ready_queue"},"probe":{"enabled":false},"logging":{"file":"off","max_size_mb":16,"max_backups":2,"access_mode":"errors","success_sample_every":50,"slow_request_ms":1500,"console_max_lines_per_minute":120},"admin":{"bind_local_only":false,"api_key":"rotated-key"}}`
	rr := settingsRequest(t, s, http.MethodPut, body)
	if rr.Code != 200 {
		t.Fatalf("PUT settings status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "rotated-key") {
		t.Fatal("PUT settings response must not echo the stored admin key")
	}

	// The rotation applies to the live server immediately: the old key is
	// rejected and the new key authorizes.
	rr = settingsRequestWithKey(t, s, http.MethodGet, "", "k")
	if rr.Code != 401 {
		t.Fatalf("old admin key should be rejected after rotation, got %d", rr.Code)
	}
	rr = settingsRequestWithKey(t, s, http.MethodGet, "", "rotated-key")
	if rr.Code != 200 {
		t.Fatalf("GET after rotation failed: %d", rr.Code)
	}

	// And the change is persisted atomically to the active config file.
	b, err := os.ReadFile(testConfigPath(t, s))
	if err != nil {
		t.Fatal(err)
	}
	var persisted config.Config
	if err := json.Unmarshal(b, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Logging.AccessMode != "errors" || persisted.Logging.MaxSizeMB != 16 || persisted.Logging.ConsoleMaxLinesPerMinute != 120 {
		t.Fatalf("logging section not persisted: %+v", persisted.Logging)
	}
	if persisted.Admin.APIKey != "rotated-key" || persisted.Admin.BindLocalOnly {
		t.Fatalf("admin section not persisted: %+v", persisted.Admin)
	}
	if persisted.Routing.Strategy != "ready_queue" {
		t.Fatalf("routing section not persisted: %+v", persisted.Routing)
	}
}

func TestAdminSettingsPutRejectsOpenAdminWithoutKey(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.APIKey = "k"
	cfg.Admin.BindLocalOnly = false // httptest clients are not loopback
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)

	body := `{"routing":{"strategy":"ready_mesh"},"probe":{"enabled":false},"logging":{"file":"auto"},"admin":{"bind_local_only":false,"api_key":""}}`
	rr := settingsRequest(t, s, http.MethodPut, body)
	if rr.Code != 400 {
		t.Fatalf("expected 400 for bind_local_only=false without api_key, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// testConfigPath returns the config path a testGateway server persists to.
func testConfigPath(t *testing.T, s *Server) string {
	t.Helper()
	return s.configPath
}

func TestAdminProviderTestSavedModeIncludesDisabledModels(t *testing.T) {
	var mu sync.Mutex
	var tested []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		tested = append(tested, body.Model)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c1", "object": "chat.completion",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	}))
	defer up.Close()

	cfg := config.Default()
	cfg.Admin.APIKey = "k"
	cfg.Admin.BindLocalOnly = false
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Name: "P1", Type: "openai_compatible", BaseURL: up.URL + "/v1",
		AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{
			{ID: "m1", Model: "enabled-model", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}},
			{ID: "m2", Model: "disabled-model", Enabled: false, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}},
		},
	}}
	s := testGateway(t, cfg)

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/provider-test", strings.NewReader(body))
		req.Header.Set("x-admin-key", "k")
		req.Header.Set("content-type", "application/json")
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}

	// Saved mode: explicitly test the disabled model without resending secrets.
	rr := post(`{"provider_id":"p1","test_models":["disabled-model"]}`)
	if rr.Code != 200 {
		t.Fatalf("saved-mode test status = %d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		OK      bool `json:"ok"`
		Passed  int  `json:"passed"`
		Results []struct {
			Model string `json:"model"`
			OK    bool   `json:"ok"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Passed != 1 || len(out.Results) != 1 || out.Results[0].Model != "disabled-model" || !out.Results[0].OK {
		t.Fatalf("unexpected saved-mode results: %+v", out)
	}

	// Saved mode with no explicit models probes every configured model,
	// including the disabled one.
	rr = post(`{"provider_id":"p1"}`)
	if rr.Code != 200 {
		t.Fatalf("saved-mode default test status = %d", rr.Code)
	}
	out = struct {
		OK      bool `json:"ok"`
		Passed  int  `json:"passed"`
		Results []struct {
			Model string `json:"model"`
			OK    bool   `json:"ok"`
		} `json:"results"`
	}{}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 2 || out.Passed != 2 {
		t.Fatalf("saved-mode default run should include disabled models: %+v", out)
	}

	// Unknown provider id is a 404.
	if rr := post(`{"provider_id":"nope","test_models":["x"]}`); rr.Code != 404 {
		t.Fatalf("unknown provider test status = %d", rr.Code)
	}
}
