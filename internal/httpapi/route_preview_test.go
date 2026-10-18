package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

type previewDecision struct {
	DeploymentID   string `json:"deployment_id"`
	Included       bool   `json:"included"`
	ExcludedReason string `json:"excluded_reason"`
}

type previewExplanation struct {
	Strategy         string            `json:"strategy"`
	Decisions        []previewDecision `json:"decisions"`
	CandidateCount   int               `json:"candidate_count"`
	KnownModel       bool              `json:"known_model"`
	UsedFallback     bool              `json:"used_unknown_model_fallback"`
	PinnedDeployment string            `json:"pinned_deployment"`
	PinnedApplied    bool              `json:"pinned_applied"`
	ConsideredCount  int               `json:"considered_count"`
	SessionAffinity  bool              `json:"session_affinity_enabled"`
}

// codingPreviewServer wires a stable public alias "coding" across two
// independent provider deployments of the same pool, with differing
// capability sets. (Deployment pB/m lacks tools.) The upstream handler
// increments a counter the tests assert stays at zero: the preview surface
// must never call a provider.
func codingPreviewServer(t *testing.T) (*Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "ready_queue"
	cfg.Routing.MaxAttempts = 3
	var deployments []string
	for i, name := range []string{"A", "B"} {
		id := "p" + name + "/m"
		deployments = append(deployments, id)
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{
			ID: "p" + name, Name: name, Type: "openai_compatible", BaseURL: up.URL,
			AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{
				ID: "m", Model: "physical-" + name, Aliases: []string{"physical-" + name, "shared-alias"}, Enabled: true,
				Priority: i, Weight: 1, ContextWindow: 200000,
				Capabilities: config.Capabilities{Streaming: true, Tools: i == 0},
			}},
		})
	}
	trueVal := true
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "coding-pool", Name: "Coding pool", Mode: "explicit", Deployments: deployments}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "coding-profile", CandidatePool: "coding-pool"}}
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "coding", PublicModel: "coding", RouteProfile: "coding-profile", Enabled: &trueVal, Protocols: []string{"openai"}}}
	return testGateway(t, cfg), &hits
}

func previewRequest(t *testing.T, s *Server, query string) (int, map[string]any, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/admin/api/route-preview?"+query, nil)
	req.Host = "127.0.0.1"
	req.RemoteAddr = "127.0.0.1:12345"
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	var resp map[string]any
	if strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode preview body: %v (%s)", err, rr.Body.String())
		}
	}
	return rr.Code, resp, rr
}

func previewExplanationOf(t *testing.T, resp map[string]any) previewExplanation {
	t.Helper()
	raw, err := json.Marshal(resp["explanation"])
	if err != nil {
		t.Fatalf("marshal explanation: %v", err)
	}
	var out previewExplanation
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode explanation: %v (%s)", err, raw)
	}
	return out
}

func TestRoutePreviewExplainsCandidatePoolAcrossProviders(t *testing.T) {
	s, hits := codingPreviewServer(t)
	code, resp, rr := previewRequest(t, s, "model=coding&protocol=openai&stream=1&tools=1&session_key=secret-session-key")
	if code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", code, rr.Body.String())
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("preview must not call providers, got %d upstream calls", n)
	}
	if resp["preview"] != true || resp["no_provider_call"] != true {
		t.Fatalf("preview flags missing: %#v", resp)
	}

	explanation := previewExplanationOf(t, resp)
	if explanation.Strategy != "ready_queue" {
		t.Fatalf("strategy = %q", explanation.Strategy)
	}
	if explanation.CandidateCount != 1 || len(explanation.Decisions) != 2 {
		t.Fatalf("explanation = %#v, want 1 candidate and 2 decisions", explanation)
	}
	byID := map[string]previewDecision{}
	for _, cd := range explanation.Decisions {
		byID[cd.DeploymentID] = cd
	}
	if cd := byID["pA/m"]; !cd.Included {
		t.Fatalf("pA/m should be included: %+v", cd)
	}
	if cd := byID["pB/m"]; cd.Included || cd.ExcludedReason != "capability_tools_unsupported" {
		t.Fatalf("pB/m decision = %+v, want excluded for missing tools", cd)
	}

	selected, ok := resp["selected"].(map[string]any)
	if !ok || selected["deployment"] != "pA/m" || selected["provider_id"] != "pA" {
		t.Fatalf("selected = %#v, want pA/m (provider pA)", resp["selected"])
	}
	eligible, ok := resp["eligible_candidates"].([]any)
	if !ok || len(eligible) != 1 {
		t.Fatalf("eligible candidates = %#v", resp["eligible_candidates"])
	}
	ve, ok := resp["virtual_endpoint"].(map[string]any)
	if !ok || ve["id"] != "coding" || ve["primary_pool_id"] != "coding-pool" {
		t.Fatalf("virtual endpoint block = %#v", resp["virtual_endpoint"])
	}
	if excluded, ok := resp["excluded_count"].(float64); !ok || int(excluded) != 1 {
		t.Fatalf("excluded_count = %#v, want 1", resp["excluded_count"])
	}
	// Operator session keys are inputs only, never echoed into the trace.
	if strings.Contains(rr.Body.String(), "secret-session-key") {
		t.Fatal("preview echoed the session key back; session keys must be consumed internally")
	}
}

func TestRoutePreviewExcludesTooSmallContextWithNoSelection(t *testing.T) {
	s, _ := codingPreviewServer(t)
	code, resp, _ := previewRequest(t, s, "model=coding&protocol=openai&stream=1&min_context_window=999999999")
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, resp)
	}
	explanation := previewExplanationOf(t, resp)
	if explanation.CandidateCount != 0 {
		t.Fatalf("no deployment satisfies the giant context window; expected zero candidates, got %#v", explanation.CandidateCount)
	}
	if resp["selected"] != nil {
		t.Fatalf("selected must be nil when no candidate is eligible: %#v", resp["selected"])
	}
	for _, cd := range explanation.Decisions {
		if cd.Included || cd.ExcludedReason != "context_window_too_small" {
			t.Fatalf("decision = %+v, want context_window_too_small exclusion", cd)
		}
	}
}

func TestRoutePreviewExcludesCooledDeploymentWithReason(t *testing.T) {
	s, hits := codingPreviewServer(t)
	s.hm.ForceCooldown("pA/m", "pre-stream terminal failure after commit", time.Hour)
	code, resp, _ := previewRequest(t, s, "model=coding&protocol=openai&stream=1")
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, resp)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("preview must never call a provider, got %d calls", n)
	}
	explanation := previewExplanationOf(t, resp)
	for _, cd := range explanation.Decisions {
		if cd.DeploymentID == "pA/m" && (cd.Included || cd.ExcludedReason != "health_not_ready") {
			t.Fatalf("pA/m decision = %+v, want excluded as health_not_ready (ready_queue strategy)", cd)
		}
		if cd.DeploymentID == "pB/m" && !cd.Included {
			t.Fatalf("independent healthy fallback must be included: %+v", cd)
		}
	}
	selected, ok := resp["selected"].(map[string]any)
	if !ok || selected["deployment"] != "pB/m" {
		t.Fatalf("expected healthy fallback selected, got %#v", resp["selected"])
	}
	eligible := previewCandidatesOf(t, resp)
	if len(eligible) != 1 {
		t.Fatalf("eligible candidates = %#v, want only pB/m", eligible)
	}
}

func previewCandidatesOf(t *testing.T, resp map[string]any) []any {
	t.Helper()
	cands, ok := resp["eligible_candidates"].([]any)
	if !ok {
		t.Fatalf("eligible_candidates missing: %#v", resp)
	}
	return cands
}

func TestRoutePreviewSurfacesVirtualEndpointProtocolRestriction(t *testing.T) {
	s, _ := codingPreviewServer(t)
	// The virtual endpoint only admits the OpenAI Chat ingress protocol; a
	// responses-protocol request must receive a structured, redacted reason.
	code, resp, _ := previewRequest(t, s, "model=coding&protocol=openai_responses")
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, resp)
	}
	errMsg, _ := resp["error"].(string)
	if !strings.Contains(errMsg, "not allowed") {
		t.Fatalf("expected protocol restriction in error, got %q", errMsg)
	}
	if _, ok := resp["virtual_endpoint"].(map[string]any); !ok {
		t.Fatalf("virtual endpoint block missing: %#v", resp)
	}
	if resp["selected"] != nil {
		t.Fatalf("protocol-restricted request cannot have a selection: %#v", resp["selected"])
	}
}

func TestRoutePreviewDirectModelBrokenFirstCandidateHasHealthyFallback(t *testing.T) {
	s, _ := codingPreviewServer(t)
	s.hm.ForceCooldown("pA/m", "terminal", time.Hour)
	code, resp, _ := previewRequest(t, s, "model=shared-alias&stream=1")
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, resp)
	}
	explanation := previewExplanationOf(t, resp)
	if len(explanation.Decisions) != 2 {
		t.Fatalf("both direct-model deployments must be considered, got %#v", explanation.Decisions)
	}
	converged := map[string]previewDecision{}
	for _, cd := range explanation.Decisions {
		converged[cd.DeploymentID] = cd
	}
	if cd := converged["pA/m"]; cd.Included || cd.ExcludedReason != "health_not_ready" {
		t.Fatalf("ready_queue must not route cooled deployments: %+v", cd)
	}
	if cd := converged["pB/m"]; !cd.Included {
		t.Fatalf("healthy same-alias deployment must be included: %+v", cd)
	}
	if selected, ok := resp["selected"].(map[string]any); !ok || selected["deployment"] != "pB/m" {
		t.Fatalf("selected = %#v, want pB/m", resp["selected"])
	}
}

func TestRoutePreviewUnknownModelNoEligibleCandidate(t *testing.T) {
	s, _ := codingPreviewServer(t)
	code, resp, _ := previewRequest(t, s, "model=never-existed")
	if code != http.StatusOK {
		t.Fatalf("status=%d body=%s", code, resp)
	}
	explanation := previewExplanationOf(t, resp)
	// The default policy falls back to eligible deployments when an
	// unknown client model is requested; the trace must declare it.
	if !explanation.UsedFallback {
		t.Fatalf("expected declared unknown-model fallback: %#v", explanation)
	}
	if explanation.KnownModel {
		t.Fatalf("unknown model must not be reported as known: %#v", explanation)
	}
	if explanation.CandidateCount != 2 {
		t.Fatalf("expected the full pool as fallback candidates: %#v", explanation)
	}
	if _, ok := resp["selected"].(map[string]any); !ok {
		t.Fatalf("fallback must still select a lead candidate: %#v", resp["selected"])
	}

	// A request every deployment fails is the true no-eligible-candidate
	// case: the cooled deployment is not ready and the other one cannot
	// serve tools.
	s.hm.ForceCooldown("pA/m", "bankrupt quota", time.Hour)
	code, resp2, _ := previewRequest(t, s, "model=coding&protocol=openai&tools=1")
	if code != http.StatusOK {
		t.Fatalf("no-eligible preview status=%d", code)
	}
	expl2 := previewExplanationOf(t, resp2)
	if expl2.CandidateCount != 0 {
		t.Fatalf("expected zero candidates under opposed requirements: %#v", expl2)
	}
	if resp2["selected"] != nil {
		t.Fatalf("selected must be nil with no eligible candidate: %#v", resp2["selected"])
	}
	converged2 := map[string]previewDecision{}
	for _, cd := range expl2.Decisions {
		converged2[cd.DeploymentID] = cd
	}
	if cd := converged2["pA/m"]; cd.Included || cd.ExcludedReason != "health_not_ready" {
		t.Fatalf("pA/m = %+v, want health_not_ready", cd)
	}
	if cd := converged2["pB/m"]; cd.Included || cd.ExcludedReason != "capability_tools_unsupported" {
		t.Fatalf("pB/m = %+v, want capability_tools_unsupported", cd)
	}
}

func TestRoutePreviewValidationBounds(t *testing.T) {
	s, _ := codingPreviewServer(t)
	code, resp, _ := previewRequest(t, s, "model=coding&min_context_window=-1")
	if code != http.StatusBadRequest {
		t.Fatalf("negative context window: status %d body=%s, want 400", code, resp)
	}
	code, _, _ = previewRequest(t, s, "model=coding&min_context_window=abc")
	if code != http.StatusBadRequest {
		t.Fatalf("non-numeric context window: status %d, want 400", code)
	}
	code, _, _ = previewRequest(t, s, "model="+strings.Repeat("x", 257))
	if code != http.StatusBadRequest {
		t.Fatalf("oversized model: status %d, want 400", code)
	}
}

func TestRoutePreviewMatrixDeniesNonReadOnlyMethods(t *testing.T) {
	s, _ := codingPreviewServer(t)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "http://127.0.0.1/admin/api/route-preview?model=coding", nil)
		req.Host = "127.0.0.1"
		req.RemoteAddr = "127.0.0.1:12345"
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		// The fail-closed authorization matrix denies mutation before the
		// handler runs.
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s on route-preview: status %d body=%s, want 403 from the authorization matrix", method, rr.Code, rr.Body.String())
		}
	}
}
