package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func coverageAdminRequest(s *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:29371"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func coverageAdminProviderBody(id, baseURL string) string {
	return `{"provider":{"id":"` + id + `","name":"` + id + `","type":"openai_compatible","base_url":"` + baseURL + `","auth_mode":"none","enabled":false,"models":[{"model":" model/alpha ","enabled":true}]}}`
}

func coverageAdminJSON(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("status=%d invalid JSON: %v; body=%s", rr.Code, err, rr.Body.String())
	}
	return out
}

func TestCoverageAdminProviderRoutesMutateAndReportContracts(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{{
		ID: "existing", Name: "Existing", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9",
		AuthMode: "none", Enabled: false,
		Models: []config.ModelConfig{{ID: "existing-model", Model: "existing-model", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)

	if rr := coverageAdminRequest(s, http.MethodGet, "/admin/api/providers", ""); rr.Code != http.StatusOK {
		t.Fatalf("GET providers status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := coverageAdminRequest(s, http.MethodPut, "/admin/api/providers", `{}`); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT provider collection status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := coverageAdminRequest(s, http.MethodPost, "/admin/api/providers", `{bad`); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid provider JSON status=%d body=%s", rr.Code, rr.Body.String())
	}
	duplicate := `{"provider":{"id":"existing","name":"duplicate","type":"openai_compatible","base_url":"http://127.0.0.1:9"}}`
	if rr := coverageAdminRequest(s, http.MethodPost, "/admin/api/providers", duplicate); rr.Code != http.StatusConflict {
		t.Fatalf("duplicate provider status=%d body=%s", rr.Code, rr.Body.String())
	}

	created := coverageAdminRequest(s, http.MethodPost, "/admin/api/providers", coverageAdminProviderBody("added", "http://127.0.0.1:8"))
	if created.Code != http.StatusCreated {
		t.Fatalf("create provider status=%d body=%s", created.Code, created.Body.String())
	}
	createdPayload := coverageAdminJSON(t, created)
	if createdPayload["saved"] != true {
		t.Fatalf("create response=%v", createdPayload)
	}
	live := s.currentConfig()
	idx := live.ProviderIndex("added")
	if idx < 0 || len(live.Providers[idx].Models) != 1 || live.Providers[idx].Models[0].ID != "model-alpha" || live.Providers[idx].Models[0].Weight != 1 {
		t.Fatalf("provider normalization was not persisted: %+v", live.Providers)
	}

	if rr := coverageAdminRequest(s, http.MethodGet, "/admin/api/providers/added", ""); rr.Code != http.StatusOK {
		t.Fatalf("GET added provider status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := coverageAdminRequest(s, http.MethodPost, "/admin/api/providers/added", `{}`); rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST provider-by-id status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := coverageAdminRequest(s, http.MethodGet, "/admin/api/providers/missing", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("GET missing provider status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := coverageAdminRequest(s, http.MethodGet, "/admin/api/providers/%25zz", ""); rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed provider id status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := coverageAdminRequest(s, http.MethodPut, "/admin/api/providers/added", `{bad`); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid provider update status=%d body=%s", rr.Code, rr.Body.String())
	}
	conflictingUpdate := `{"provider":{"id":"existing","name":"collision","type":"openai_compatible","base_url":"http://127.0.0.1:8"}}`
	if rr := coverageAdminRequest(s, http.MethodPut, "/admin/api/providers/added", conflictingUpdate); rr.Code != http.StatusConflict {
		t.Fatalf("conflicting provider update status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := coverageAdminRequest(s, http.MethodDelete, "/admin/api/providers/missing", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("DELETE missing provider status=%d body=%s", rr.Code, rr.Body.String())
	}
	deleted := coverageAdminRequest(s, http.MethodDelete, "/admin/api/providers/added", "")
	if deleted.Code != http.StatusOK || !strings.Contains(deleted.Body.String(), `"deleted":true`) {
		t.Fatalf("DELETE provider status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	if s.currentConfig().ProviderIndex("added") >= 0 {
		t.Fatal("deleted provider remained in effective configuration")
	}

	// A valid mutation that cannot be persisted must return the handler's error
	// contract and leave the in-memory/base configuration unchanged.
	s.configPath = t.TempDir()
	failed := coverageAdminRequest(s, http.MethodPost, "/admin/api/providers", coverageAdminProviderBody("not-saved", "http://127.0.0.1:7"))
	if failed.Code != http.StatusBadRequest || !strings.Contains(failed.Body.String(), "error") {
		t.Fatalf("persistence failure status=%d body=%s", failed.Code, failed.Body.String())
	}
	if s.currentConfig().ProviderIndex("not-saved") >= 0 {
		t.Fatal("provider became visible after persistence failure")
	}
}

func TestCoverageAdminProviderProbeAndDiscoveryRoutesUseLocalAPIs(t *testing.T) {
	status := http.StatusOK
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(status)
			if status == http.StatusOK {
				_, _ = io.WriteString(w, `{"data":[{"id":"models/zeta"},{"model":"alpha"}],"items":["beta"],"models":[{"name":"gamma"}]}`)
			}
			return
		}
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"probe","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"OK"}}]}`)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	s := testGateway(t, cfg)

	if rr := coverageAdminRequest(s, http.MethodPost, "/admin/api/provider-presets", `{}`); rr.Code != http.StatusForbidden {
		t.Fatalf("POST presets status=%d body=%s", rr.Code, rr.Body.String())
	}
	presets := coverageAdminRequest(s, http.MethodGet, "/admin/api/provider-presets", "")
	if presets.Code != http.StatusOK || !strings.Contains(presets.Body.String(), "presets") {
		t.Fatalf("GET presets status=%d body=%s", presets.Code, presets.Body.String())
	}

	if rr := coverageAdminRequest(s, http.MethodGet, "/admin/api/provider-check", ""); rr.Code != http.StatusForbidden {
		t.Fatalf("GET provider-check status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := coverageAdminRequest(s, http.MethodPost, "/admin/api/provider-check", `{bad`); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid provider-check JSON status=%d body=%s", rr.Code, rr.Body.String())
	}
	missingBase := `{"provider":{"id":"check","type":"openai_compatible"}}`
	if rr := coverageAdminRequest(s, http.MethodPost, "/admin/api/provider-check", missingBase); rr.Code != http.StatusBadRequest {
		t.Fatalf("provider-check missing base status=%d body=%s", rr.Code, rr.Body.String())
	}
	invalidURL := `{"provider":{"id":"check","type":"openai_compatible","base_url":"http://[::1"}}`
	if rr := coverageAdminRequest(s, http.MethodPost, "/admin/api/provider-check", invalidURL); rr.Code != http.StatusBadRequest {
		t.Fatalf("provider-check invalid adapter status=%d body=%s", rr.Code, rr.Body.String())
	}
	checkBody := `{"provider":{"id":"check","type":"openai_compatible","base_url":"` + upstream.URL + `","auth_mode":"none"}}`
	for _, tc := range []struct {
		name       string
		code       int
		wantOK     bool
		wantReach  bool
		wantAuthOK bool
	}{
		{name: "unauthorized", code: http.StatusUnauthorized, wantOK: false, wantReach: true, wantAuthOK: false},
		{name: "server error", code: http.StatusBadGateway, wantOK: false, wantReach: true, wantAuthOK: true},
		{name: "healthy", code: http.StatusOK, wantOK: true, wantReach: true, wantAuthOK: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status = tc.code
			out := coverageAdminJSON(t, coverageAdminRequest(s, http.MethodPost, "/admin/api/provider-check", checkBody))
			if out["ok"] != tc.wantOK || out["reachable"] != tc.wantReach || out["auth_ok"] != tc.wantAuthOK {
				t.Fatalf("check response=%v", out)
			}
		})
	}

	if rr := coverageAdminRequest(s, http.MethodGet, "/admin/api/provider-discover", ""); rr.Code != http.StatusForbidden {
		t.Fatalf("GET provider-discover status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := coverageAdminRequest(s, http.MethodPost, "/admin/api/provider-discover", `{bad`); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid provider-discover JSON status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := coverageAdminRequest(s, http.MethodPost, "/admin/api/provider-discover", missingBase); rr.Code != http.StatusBadRequest {
		t.Fatalf("provider-discover missing base status=%d body=%s", rr.Code, rr.Body.String())
	}
	status = http.StatusOK
	discovered := coverageAdminRequest(s, http.MethodPost, "/admin/api/provider-discover", checkBody)
	if discovered.Code != http.StatusOK {
		t.Fatalf("provider-discover success status=%d body=%s", discovered.Code, discovered.Body.String())
	}
	models, ok := coverageAdminJSON(t, discovered)["models"].([]any)
	if !ok || len(models) != 4 {
		t.Fatalf("discovered models=%v", coverageAdminJSON(t, discovered)["models"])
	}
	status = http.StatusBadGateway
	discoveryFailure := coverageAdminRequest(s, http.MethodPost, "/admin/api/provider-discover", checkBody)
	if discoveryFailure.Code != http.StatusOK || coverageAdminJSON(t, discoveryFailure)["ok"] != false {
		t.Fatalf("provider-discover failure status=%d body=%s", discoveryFailure.Code, discoveryFailure.Body.String())
	}

	if rr := coverageAdminRequest(s, http.MethodGet, "/admin/api/provider-test", ""); rr.Code != http.StatusForbidden {
		t.Fatalf("GET provider-test status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := coverageAdminRequest(s, http.MethodPost, "/admin/api/provider-test", `{bad`); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid provider-test JSON status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := coverageAdminRequest(s, http.MethodPost, "/admin/api/provider-test", `{"provider":{"type":"openai_compatible","base_url":"`+upstream.URL+`"},"test_models":["m"]}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("provider-test missing id status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := coverageAdminRequest(s, http.MethodPost, "/admin/api/provider-test", `{"provider":{"id":"p","type":"unsupported","base_url":"`+upstream.URL+`"},"test_models":["m"]}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("provider-test unsupported type status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := coverageAdminRequest(s, http.MethodPost, "/admin/api/provider-test", `{"provider":{"id":"p","type":"openai_compatible","base_url":"`+upstream.URL+`"}}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("provider-test empty models status=%d body=%s", rr.Code, rr.Body.String())
	}
	status = http.StatusOK
	testBody := `{"provider":{"id":"p","type":"openai_compatible","base_url":"` + upstream.URL + `","auth_mode":"none"},"test_models":["m"," m ",""],"mode":"quick"}`
	tested := coverageAdminRequest(s, http.MethodPost, "/admin/api/provider-test", testBody)
	if tested.Code != http.StatusOK {
		t.Fatalf("provider-test success status=%d body=%s", tested.Code, tested.Body.String())
	}
	result := coverageAdminJSON(t, tested)
	if result["total"] != float64(1) || result["passed"] != float64(1) || result["ok"] != true {
		t.Fatalf("provider-test result=%v", result)
	}
}

func TestCoverageAdminProbeAndSnapshotRouteContracts(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: "http://127.0.0.1:9", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "m1", Enabled: true, Weight: 1}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: "http://127.0.0.1:8", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "m2", Enabled: true, Weight: 1}}},
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "coverage-pool", Name: "Coverage pool", Mode: "all"}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "coverage-profile", CandidatePool: "coverage-pool"}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "coverage-ve", Name: "Coverage endpoint", PublicModel: "coverage-model", RouteProfile: "coverage-profile", Protocols: []string{"openai"}}}
	disabled := false
	cfg.DecisionProviders = []config.DecisionProviderConfig{{ID: "coverage-ext", Type: "jev", Enabled: &disabled, APIKeyEnv: "COVERAGE_ADMIN_MISSING_KEY", PrivacyMode: "metadata_only"}}
	cfg.DecisionChains = []config.DecisionChainConfig{{ID: "coverage-chain", Steps: []config.DecisionChainStep{{Provider: "local", TimeoutMS: 10}, {Provider: "policy"}, {Provider: "coverage-ext", TimeoutMS: 20}}}}
	s := testGateway(t, cfg)

	if rr := coverageAdminRequest(s, http.MethodPost, "/admin/api/snapshot", `{}`); rr.Code != http.StatusForbidden {
		t.Fatalf("POST snapshot status=%d body=%s", rr.Code, rr.Body.String())
	}
	snapshot := coverageAdminRequest(s, http.MethodGet, "/admin/api/snapshot?limit=1&events=not-a-number", "")
	if snapshot.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", snapshot.Code, snapshot.Body.String())
	}
	payload := coverageAdminJSON(t, snapshot)
	if payload["snapshot_truncated"] != true || payload["deployment_total"] != float64(2) {
		t.Fatalf("snapshot truncation contract=%v", payload)
	}
	if deployments, ok := payload["deployments"].([]any); !ok || len(deployments) != 1 {
		t.Fatalf("snapshot deployments=%v", payload["deployments"])
	}
	if _, ok := payload["virtual_endpoints"]; !ok {
		t.Fatalf("snapshot omitted virtual endpoint section: keys=%v", payload)
	}
	if _, ok := payload["decision"]; !ok {
		t.Fatalf("snapshot omitted enriched sections: keys=%v", payload)
	}
	if !strings.Contains(snapshot.Body.String(), "coverage-chain") || strings.Contains(snapshot.Body.String(), "COVERAGE_ADMIN_MISSING_KEY") {
		t.Fatalf("snapshot chain/key redaction contract failed: %s", snapshot.Body.String())
	}

	if rr := coverageAdminRequest(s, http.MethodGet, "/admin/api/probe", ""); rr.Code != http.StatusForbidden {
		t.Fatalf("GET probe status=%d body=%s", rr.Code, rr.Body.String())
	}
	if rr := coverageAdminRequest(s, http.MethodPost, "/admin/api/probe", `{bad`); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid probe JSON status=%d body=%s", rr.Code, rr.Body.String())
	}
	accepted := coverageAdminRequest(s, http.MethodPost, "/admin/api/probe", `{}`)
	if accepted.Code != http.StatusAccepted || !strings.Contains(accepted.Body.String(), `"accepted":true`) {
		t.Fatalf("async probe status=%d body=%s", accepted.Code, accepted.Body.String())
	}
	completed := coverageAdminRequest(s, http.MethodPost, "/admin/api/probe?wait=true", `{"reason":"coverage"}`)
	if completed.Code != http.StatusOK || !strings.Contains(completed.Body.String(), `"completed":true`) {
		t.Fatalf("sync probe status=%d body=%s", completed.Code, completed.Body.String())
	}
}
