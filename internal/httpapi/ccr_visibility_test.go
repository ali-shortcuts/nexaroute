package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func ccrGateway(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Admin.APIKey = "k"
	cfg.Admin.BindLocalOnly = false
	cfg.Probe.Enabled = false
	cfg.ClientAuth = config.ClientAuthConfig{Required: true, Keys: []config.ClientKey{{ID: "k1", Name: "laptop", Key: "nr-app-key-123456", Enabled: true}}}
	cfg.Providers = []config.ProviderConfig{{
		ID: "p1", Name: "Prov One", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1/v1",
		AuthMode: "bearer", APIKeyEnv: "P1_KEY", Enabled: true,
		Credentials: []config.CredentialConfig{{Name: "backup", APIKey: "sk-literal-backup-key-9876", Enabled: true}},
		Models:      []config.ModelConfig{{ID: "m1", Model: "model-1", Enabled: true, Weight: 1, Capabilities: config.Capabilities{Streaming: true, Tools: true}}},
	}}
	s := testGateway(t, cfg)
	t.Setenv("P1_KEY", "sk-env-resolved-key-abcdef-123456")
	return s
}

func TestProviderCardShowsMaskedKeyAndEditRevealsResolved(t *testing.T) {
	s := ccrGateway(t)
	adminReq := func(method, path string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://gateway"+path, strings.NewReader(string(body)))
		req.Header.Set("x-admin-key", "k")
		if body != nil {
			req.Header.Set("content-type", "application/json")
		}
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}

	// List always shows a masked key next to the base URL.
	rr := adminReq(http.MethodGet, "/admin/api/providers", nil)
	var list struct {
		Providers []struct {
			ID        string `json:"id"`
			BaseURL   string `json:"base_url"`
			MaskedKey string `json:"masked_key"`
			KeySource string `json:"key_source"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Providers) != 1 {
		t.Fatalf("expected 1 provider: %s", rr.Body.String())
	}
	p0 := list.Providers[0]
	if p0.BaseURL == "" {
		t.Fatal("base_url must always be present in the provider list")
	}
	if !strings.HasPrefix(p0.MaskedKey, "sk-env") || !strings.Contains(p0.MaskedKey, "…") || p0.MaskedKey == "sk-env-resolved-key-abcdef-123456" {
		t.Fatalf("masked key must hide the full value: %q", p0.MaskedKey)
	}
	if p0.KeySource != "pool" {
		t.Fatalf("key source should be env, got %q", p0.KeySource)
	}
	if strings.Contains(rr.Body.String(), "sk-env-resolved-key-abcdef-123456") {
		t.Fatal("provider list must never contain the full resolved key")
	}

	// Edit reveal shows the actual resolved credentials for primary + pool.
	rr = adminReq(http.MethodGet, "/admin/api/providers/p1?reveal=1", nil)
	var detail struct {
		ResolvedAPIKey      string `json:"resolved_api_key"`
		ResolvedCredentials []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
			Key    string `json:"key"`
		} `json:"resolved_credentials"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.ResolvedAPIKey != "sk-env-resolved-key-abcdef-123456" {
		t.Fatalf("reveal should resolve the env key, got %q", detail.ResolvedAPIKey)
	}
	if len(detail.ResolvedCredentials) != 2 {
		t.Fatalf("expected primary + pool resolved credentials: %+v", detail.ResolvedCredentials)
	}
	if detail.ResolvedCredentials[0].Name != "primary" || detail.ResolvedCredentials[0].Source != "env:P1_KEY" {
		t.Fatalf("primary resolved credential wrong: %+v", detail.ResolvedCredentials[0])
	}
	if detail.ResolvedCredentials[1].Name != "backup" || detail.ResolvedCredentials[1].Key != "sk-literal-backup-key-9876" {
		t.Fatalf("pool resolved credential wrong: %+v", detail.ResolvedCredentials[1])
	}
}

func TestRecentRequestsLogCapturesRouteLatencyTokensAndAuthRejections(t *testing.T) {
	var upOK bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upOK = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "c1", "object": "chat.completion",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "hey"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 7},
		})
	}))
	defer up.Close()

	s := ccrGateway(t)
	// Point the provider at the live test server.
	cfg := s.currentConfig()
	cfg.Providers[0].BaseURL = up.URL + "/v1"
	if err := s.applyConfig(cfg); err != nil {
		t.Fatal(err)
	}
	s.hm.RecordSuccess("p1/m1", 1_000_000) // ready_mesh only routes verified-healthy deployments

	// Rejected request (no key) must appear in the log with 401.
	req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"model-1","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 401 {
		t.Fatalf("expected 401, got %d", rr.Code)
	}

	// Successful request records route + tokens.
	req = httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(`{"model":"model-1","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer nr-app-key-123456")
	req.Header.Set("content-type", "application/json")
	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != 200 || !upOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}

	recs := s.usage.RecentRequests(10)
	if len(recs) != 2 {
		t.Fatalf("expected 2 recent records, got %d", len(recs))
	}
	newest := recs[0]
	if newest.Status != 200 || newest.Deployment != "p1/m1" || newest.Model != "model-1" || newest.KeyName != "laptop" {
		t.Fatalf("resolved route wrong: %+v", newest)
	}
	if newest.InputTokens != 11 || newest.OutputTokens != 7 {
		t.Fatalf("tokens wrong: %+v", newest)
	}
	if recs[1].Status != 401 || recs[1].Deployment != "" {
		t.Fatalf("rejected request record wrong: %+v", recs[1])
	}

	// Admin endpoint exposes the same log.
	areq := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/requests?limit=10", nil)
	areq.Header.Set("x-admin-key", "k")
	arr := httptest.NewRecorder()
	s.Handler().ServeHTTP(arr, areq)
	if arr.Code != 200 || !strings.Contains(arr.Body.String(), `"deployment":"p1/m1"`) {
		t.Fatalf("admin requests log wrong: %d %s", arr.Code, arr.Body.String())
	}
}

func TestWebUIServesPowerDashboardElements(t *testing.T) {
	cfg := config.Default()
	cfg.Admin.APIKey = "k"
	cfg.Admin.BindLocalOnly = false
	s := testGateway(t, cfg)
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "http://gateway"+path, nil)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		if rr.Code != 200 {
			t.Fatalf("GET %s: status %d", path, rr.Code)
		}
		return rr
	}
	index := get("/").Body.String()
	appjs := get("/app.js").Body.String()
	styles := get("/styles.css").Body.String()

	// Overview command center: KPI strip, live traffic chart, top deployments,
	// activity strip, window selector.
	for _, id := range []string{"kRpm", "kSuccess", "kTokens", "trafficChart", "topDeployments", "activityStrip", "chartPeak"} {
		if !strings.Contains(index, `id="`+id+`"`) {
			t.Errorf("overview index.html missing #%s", id)
		}
	}
	if !strings.Contains(index, "chart-win") {
		t.Error("overview index.html missing chart window selector")
	}
	// Live client logic present and wired.
	for _, fn := range []string{"renderTrafficChart", "renderTopDeployments", "renderActivityStrip", "evClass", "agoStr", "testCurlFor", "renderCLI", "PW.series"} {
		if !strings.Contains(appjs, fn) {
			t.Errorf("app.js missing power-UI symbol %s", fn)
		}
	}
	// Events filters, request explorer controls, model table controls.
	for _, id := range []string{"evChips", "evQuery", "evPauseBtn", "reqStatus", "reqSSE", "reqAuto", "reqQuery", "mwQuery", "mwStatus", "mwSort"} {
		if !strings.Contains(index, `id="`+id+`"`) {
			t.Errorf("index.html missing control #%s", id)
		}
	}
	// Route preview capabilities are static inputs; the E2E test and curl-copy
	// buttons are injected by the preview renderer into #previewOut.
	for _, id := range []string{"pvTools", "pvVision", "pvStream", "pvReason"} {
		if !strings.Contains(index, `id="`+id+`"`) {
			t.Errorf("index.html missing preview capability #%s", id)
		}
	}
	for _, id := range []string{"pvRunBtn", "pvCurlBtn"} {
		if !strings.Contains(appjs, id) {
			t.Errorf("app.js missing injected preview control %s", id)
		}
	}
	// CLI snippets render from live origin (container static, buttons injected),
	// config export and header controls are static.
	for _, token := range []string{"cliGrid", "copyConfigBtn", "downloadConfigBtn", "pauseBtn", "refreshNowBtn"} {
		if !strings.Contains(index, token) || !strings.Contains(appjs, token) {
			t.Errorf("power-UI wiring incomplete for %s", token)
		}
	}
	if !strings.Contains(appjs, "cli-copy") {
		t.Error("app.js missing injected CLI copy buttons")
	}
	// New component styles shipped.
	for _, cls := range []string{".traffic-chart", ".chip-row", ".latcell", ".top-deployments", ".req-detail"} {
		if !strings.Contains(styles, cls) {
			t.Errorf("styles.css missing %s", cls)
		}
	}
}
