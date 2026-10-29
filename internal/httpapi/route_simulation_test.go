package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/router"
)

// roundRobinPreviewServer mirrors codingPreviewServer but with the
// rotating strategy and a direct (non-virtual) model, so live rotation
// is observable through the router itself.
func roundRobinPreviewServer(t *testing.T) (*Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "round_robin"
	cfg.Routing.MaxAttempts = 3
	for i, name := range []string{"A", "B"} {
		cfg.Providers = append(cfg.Providers, config.ProviderConfig{
			ID: "p" + name, Name: name, Type: "openai_compatible", BaseURL: up.URL,
			AuthMode: "none", Enabled: true,
			Models: []config.ModelConfig{{
				ID: "m", Model: "physical-" + name, Aliases: []string{"shared-alias"}, Enabled: true,
				Priority: i, Weight: 1, ContextWindow: 200000,
				Capabilities: config.Capabilities{Streaming: true},
			}},
		})
	}
	return testGateway(t, cfg), &hits
}

func previewSelectedID(t *testing.T, resp map[string]any) string {
	t.Helper()
	sel, ok := resp["selected"].(map[string]any)
	if !ok {
		return ""
	}
	id, _ := sel["deployment"].(string)
	return id
}

// The preview endpoint must be a pure simulation even under a rotating
// strategy: one preview between two real router selections must not move
// the second real selection, and two previews must agree with each other.
func TestRoutePreviewDoesNotAdvanceLiveRotationOrCallUpstream(t *testing.T) {
	s, hits := roundRobinPreviewServer(t)
	req := router.Requirement{Model: "shared-alias"}
	first := s.rt.Candidates(req)
	if len(first) != 2 {
		t.Fatalf("first real candidates = %d, want 2", len(first))
	}
	code, resp, rr := previewRequest(t, s, "model=shared-alias&stream=1")
	if code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", code, rr.Body.String())
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("preview called upstream %d times, want 0", n)
	}
	if resp["simulated"] != true || resp["selected_is_simulated"] != true {
		t.Fatalf("preview must label its selection simulated: %#v", resp)
	}
	if exp := previewExplanationOf(t, resp); !exp.Simulated || exp.RotationPredictive {
		t.Fatalf("round_robin explanation = %#v, want simulated=true rotation_predictive=false", exp)
	}
	second := s.rt.Candidates(req)
	if len(second) != 2 || second[0].Deployment.ID == first[0].Deployment.ID {
		t.Fatalf("second real pick %v did not rotate from first %v; preview consumed the rotation counter (or rotation is broken)",
			second[0].Deployment.ID, first[0].Deployment.ID)
	}
	code2, resp2, _ := previewRequest(t, s, "model=shared-alias&stream=1")
	if code2 != http.StatusOK {
		t.Fatalf("second preview status=%d", code2)
	}
	if previewSelectedID(t, resp) != previewSelectedID(t, resp2) {
		t.Fatalf("repeated preview selected %q then %q; simulations must be deterministic",
			previewSelectedID(t, resp), previewSelectedID(t, resp2))
	}
	third := s.rt.Candidates(req)
	if third[0].Deployment.ID != first[0].Deployment.ID {
		t.Fatalf("third real pick %v, want rotation back to first %v", third[0].Deployment.ID, first[0].Deployment.ID)
	}
}

func TestRoutePreviewCompareProfileShowsOnlyWhatChanges(t *testing.T) {
	s, hits := codingPreviewServer(t)
	code, resp, rr := previewRequest(t, s, "model=coding&protocol=openai&stream=1&compare_profile=a-only-profile&session_key=compare-secret")
	if code != http.StatusOK {
		t.Fatalf("compare preview status=%d body=%s", code, rr.Body.String())
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("compare preview called upstream %d times, want 0", n)
	}
	if strings.Contains(rr.Body.String(), "compare-secret") {
		t.Fatal("compare preview echoed the session key")
	}
	comp, ok := resp["comparison"].(map[string]any)
	if !ok {
		t.Fatalf("comparison missing: %#v", resp)
	}
	if comp["profile_id"] != "a-only-profile" || comp["simulated"] != true {
		t.Fatalf("comparison = %#v, want a-only-profile simulated", comp)
	}
	if previewSelectedID(t, resp) != "pA/m" {
		t.Fatalf("baseline selected = %q, want pA/m", previewSelectedID(t, resp))
	}
	altSel, ok := comp["selected"].(map[string]any)
	if !ok || altSel["deployment"] != "pA/m" {
		t.Fatalf("alternative selected = %#v, want pA/m", comp["selected"])
	}
	if comp["selected_changed"] != false {
		t.Fatalf("selected_changed = %v, want false (both select pA/m)", comp["selected_changed"])
	}
	removed := comp["removed_deployments"].([]any)
	if len(removed) != 1 || removed[0] != "pB/m" {
		t.Fatalf("removed_deployments = %#v, want [pB/m]", comp["removed_deployments"])
	}
	if added := comp["added_deployments"].([]any); len(added) != 0 {
		t.Fatalf("added_deployments = %#v, want empty", added)
	}
	// The alternative explanation must layer pool membership with the
	// stable profile-pool reason, not the funnel's own verdict.
	raw, err := json.Marshal(comp["explanation"])
	if err != nil {
		t.Fatalf("marshal comparison explanation: %v", err)
	}
	var alt previewExplanation
	if err := json.Unmarshal(raw, &alt); err != nil {
		t.Fatalf("decode comparison explanation: %v", err)
	}
	for _, cd := range alt.Decisions {
		if cd.DeploymentID == "pB/m" && (cd.Included || cd.ExcludedReason != "not_in_route_profile_pools") {
			t.Fatalf("pB/m alternative decision = %+v, want not_in_route_profile_pools", cd)
		}
	}
}

func TestRoutePreviewCompareProfilePrivacyTightensAndUnknownIsRejected(t *testing.T) {
	s, _ := codingPreviewServer(t)
	// Neither fixture provider declares trains_on_data="no", so a
	// no_training profile must leave the alternative with no selection.
	code, resp, _ := previewRequest(t, s, "model=coding&protocol=openai&stream=1&compare_profile=private-profile")
	if code != http.StatusOK {
		t.Fatalf("private compare status=%d", code)
	}
	comp, ok := resp["comparison"].(map[string]any)
	if !ok {
		t.Fatalf("comparison missing: %#v", resp)
	}
	if comp["selected"] != nil || comp["selected_changed"] != true {
		t.Fatalf("no_training alternative must select nothing and change the selection: %#v", comp)
	}
	code, _, _ = previewRequest(t, s, "model=coding&compare_profile=no-such-profile")
	if code != http.StatusBadRequest {
		t.Fatalf("unknown compare_profile: status %d, want 400", code)
	}
}

func TestRoutePreviewScoreBreakdownSumsToScore(t *testing.T) {
	s, _ := codingPreviewServer(t)
	code, resp, _ := previewRequest(t, s, "model=coding&protocol=openai&stream=1")
	if code != http.StatusOK {
		t.Fatalf("preview status=%d", code)
	}
	eligible, ok := resp["eligible_candidates"].([]any)
	if !ok || len(eligible) == 0 {
		t.Fatalf("eligible candidates missing: %#v", resp)
	}
	for _, raw := range eligible {
		cand, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("candidate not an object: %#v", raw)
		}
		bd, ok := cand["score_breakdown"].(map[string]any)
		if !ok {
			t.Fatalf("candidate %v missing score_breakdown", cand["deployment"])
		}
		sum := bd["base"].(float64) + bd["health_adjustment"].(float64) + bd["weight_contribution"].(float64) -
			bd["priority_penalty"].(float64) - bd["latency_penalty"].(float64) -
			bd["capacity_penalty"].(float64) - bd["failure_penalty"].(float64)
		if sum != cand["score"].(float64) {
			t.Fatalf("candidate %v score %v != breakdown sum %v (%#v)", cand["deployment"], cand["score"], sum, bd)
		}
	}
}
