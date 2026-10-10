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

func coverageVirtualCanonicalRequest(s *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:23456"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	return rr
}

func coverageVirtualCanonicalStatus(t *testing.T, rr *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rr.Code != want {
		t.Fatalf("status=%d want %d body=%s", rr.Code, want, rr.Body.String())
	}
}

func TestCoverageVirtualCanonicalAdminCRUD(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool0", Mode: "explicit", Deployments: []string{"provider/model"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile0", CandidatePool: "pool0"}}
	cfg.FallbackChains = []config.FallbackChainConfig{{ID: "chain0", Pools: []string{"pool0"}}}
	cfg.RouteProfiles[0].FallbackChain = "chain0"
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve0", PublicModel: "public0", RouteProfile: "profile0"}}
	s := testGateway(t, cfg)

	for _, path := range []string{
		"/admin/api/virtual-endpoints",
		"/admin/api/route-profiles",
		"/admin/api/candidate-pools",
		"/admin/api/fallback-chains",
	} {
		coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodGet, path, "", nil), http.StatusOK)
	}

	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodPost, "/admin/api/virtual-endpoints", `{"id":"ve1","public_model":"public1","route_profile":"profile0"}`, nil), http.StatusCreated)
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodPost, "/admin/api/virtual-endpoints", `{"id":"ve1","public_model":"again","route_profile":"profile0"}`, nil), http.StatusBadRequest)
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodPut, "/admin/api/virtual-endpoints/ve1", `{"public_model":"updated","route_profile":"profile0"}`, nil), http.StatusOK)
	ve := coverageVirtualCanonicalRequest(s, http.MethodGet, "/admin/api/virtual-endpoints/ve1", "", nil)
	coverageVirtualCanonicalStatus(t, ve, http.StatusOK)
	if !strings.Contains(ve.Body.String(), "updated") {
		t.Fatalf("updated virtual endpoint missing from response: %s", ve.Body.String())
	}
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodDelete, "/admin/api/virtual-endpoints/ve1", "", nil), http.StatusOK)

	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodPost, "/admin/api/route-profiles", `{"id":"profile1","candidate_pool":"pool0"}`, nil), http.StatusCreated)
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodPost, "/admin/api/route-profiles", `{"id":"profile1","candidate_pool":"pool0"}`, nil), http.StatusBadRequest)
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodPut, "/admin/api/route-profiles/profile1", `{"candidate_pool":"pool0"}`, nil), http.StatusOK)
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodGet, "/admin/api/route-profiles/profile1", "", nil), http.StatusOK)
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodDelete, "/admin/api/route-profiles/profile1", "", nil), http.StatusOK)

	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodPost, "/admin/api/candidate-pools", `{"id":"pool1","mode":"explicit","deployments":["provider/model"]}`, nil), http.StatusCreated)
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodPost, "/admin/api/candidate-pools", `{"id":"pool1"}`, nil), http.StatusBadRequest)
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodPut, "/admin/api/candidate-pools/pool1", `{"mode":"all"}`, nil), http.StatusOK)
	pool := coverageVirtualCanonicalRequest(s, http.MethodGet, "/admin/api/candidate-pools/pool1", "", nil)
	coverageVirtualCanonicalStatus(t, pool, http.StatusOK)
	if !strings.Contains(pool.Body.String(), `"mode":"all"`) {
		t.Fatalf("updated candidate pool missing from response: %s", pool.Body.String())
	}
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodDelete, "/admin/api/candidate-pools/pool1", "", nil), http.StatusOK)

	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodPost, "/admin/api/fallback-chains", `{"id":"chain1","pools":["pool0"]}`, nil), http.StatusCreated)
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodPost, "/admin/api/fallback-chains", `{"id":"chain1","pools":["pool0"]}`, nil), http.StatusBadRequest)
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodPut, "/admin/api/fallback-chains/chain1", `{"pools":["pool0"]}`, nil), http.StatusOK)
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodGet, "/admin/api/fallback-chains/chain1", "", nil), http.StatusOK)
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodDelete, "/admin/api/fallback-chains/chain1", "", nil), http.StatusOK)

	// Reference-integrity errors are distinct from ordinary not-found errors.
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodDelete, "/admin/api/candidate-pools/pool0", "", nil), http.StatusConflict)
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodDelete, "/admin/api/route-profiles/profile0", "", nil), http.StatusConflict)
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodDelete, "/admin/api/fallback-chains/chain0", "", nil), http.StatusConflict)
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodPatch, "/admin/api/virtual-endpoints/ve0", "", nil), http.StatusForbidden)
}

func TestCoverageVirtualCanonicalResponsesValidationAndAuth(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.ClientAuth.Enabled = true
	cfg.ClientAuth.Keys = []string{"coverage-secret"}
	s := testGateway(t, cfg)

	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodGet, "/v1/responses", "", nil), http.StatusMethodNotAllowed)
	missing := coverageVirtualCanonicalRequest(s, http.MethodPost, "/v1/responses", `{}`, nil)
	coverageVirtualCanonicalStatus(t, missing, http.StatusUnauthorized)
	if !strings.Contains(missing.Body.String(), "invalid or missing API key") {
		t.Fatalf("unexpected auth error: %s", missing.Body.String())
	}
	auth := map[string]string{"Authorization": "Bearer coverage-secret"}
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodPost, "/v1/responses", "{", auth), http.StatusBadRequest)
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodPost, "/v1/responses", `{}`, auth), http.StatusBadRequest)
	coverageVirtualCanonicalStatus(t, coverageVirtualCanonicalRequest(s, http.MethodPost, "/v1/responses", `{"model":"m","input":{"unsupported":true}}`, auth), http.StatusBadRequest)
}

func TestCoverageVirtualCanonicalAnthropicToResponsesTranslation(t *testing.T) {
	var upstreamRequest map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("upstream path=%s want /v1/messages", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamRequest); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"translated hello"}],"model":"anthropic-model","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"anthropic/model"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile", CandidatePool: "pool"}}
	cfg.Providers = []config.ProviderConfig{{
		ID: "anthropic", Type: "anthropic_compatible", BaseURL: upstream.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "model", Model: "anthropic-model", Aliases: []string{"client-model"}, Enabled: true, Weight: 1,
			Capabilities: config.Capabilities{Streaming: true, Tools: true}},
		},
	}}
	s := testGateway(t, cfg)
	rr := coverageVirtualCanonicalRequest(s, http.MethodPost, "/v1/responses", `{"model":"client-model","input":"hello","max_output_tokens":32}`, nil)
	coverageVirtualCanonicalStatus(t, rr, http.StatusOK)
	var response map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode translated response: %v body=%s", err, rr.Body.String())
	}
	if response["model"] != "client-model" {
		t.Fatalf("response model=%v want client-model", response["model"])
	}
	if !strings.Contains(rr.Body.String(), "translated hello") {
		t.Fatalf("translated response omitted text: %s", rr.Body.String())
	}
	if upstreamRequest["model"] != "anthropic-model" {
		t.Fatalf("upstream model=%v want anthropic-model", upstreamRequest["model"])
	}
}

func TestCoverageVirtualCanonicalResponsesNoDeployment(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.FallbackOnUnknownModel = false
	s := testGateway(t, cfg)
	rr := coverageVirtualCanonicalRequest(s, http.MethodPost, "/v1/responses", `{"model":"missing-model","input":"hello"}`, nil)
	coverageVirtualCanonicalStatus(t, rr, http.StatusServiceUnavailable)
	if !strings.Contains(rr.Body.String(), "no compatible healthy deployment") {
		t.Fatalf("unexpected no-deployment response: %s", rr.Body.String())
	}
}

func TestCoverageVirtualCanonicalResponsesUpstreamErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"message":"upstream rejected request","type":"upstream_error"}}`)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 1
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool", Mode: "explicit", Deployments: []string{"responses/model"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "profile", CandidatePool: "pool"}}
	cfg.Providers = []config.ProviderConfig{{
		ID: "responses", Type: "openai_responses", BaseURL: upstream.URL, AuthMode: "none", Enabled: true,
		Models: []config.ModelConfig{{ID: "model", Model: "responses-model", Enabled: true, Weight: 1}},
	}}
	s := testGateway(t, cfg)
	badStatus := coverageVirtualCanonicalRequest(s, http.MethodPost, "/v1/responses", `{"model":"responses-model","input":"hello"}`, nil)
	coverageVirtualCanonicalStatus(t, badStatus, http.StatusBadGateway)
	if !strings.Contains(badStatus.Body.String(), "upstream rejected request") {
		t.Fatalf("upstream error was not translated: %s", badStatus.Body.String())
	}
	malformedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"not":`)
	}))
	defer malformedUpstream.Close()
	cfg.Providers[0].BaseURL = malformedUpstream.URL
	s = testGateway(t, cfg)
	malformed := coverageVirtualCanonicalRequest(s, http.MethodPost, "/v1/responses", `{"model":"responses-model","input":"hello"}`, nil)
	coverageVirtualCanonicalStatus(t, malformed, http.StatusBadGateway)
	if !strings.Contains(malformed.Body.String(), "upstream returned an invalid response") {
		t.Fatalf("malformed response was not translated: %s", malformed.Body.String())
	}
}
