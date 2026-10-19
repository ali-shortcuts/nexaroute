package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/autopilot"
	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// #418 operator actions: POST /admin/api/autopilot evaluates,
// promotes (gate-gated — the operator cannot override missing
// evidence) and rolls back, with refusals carrying the gate
// decision that explains them.
func TestAutopilotOperatorActions(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "balanced"
	cfg.Decision.TimeoutMS = 100
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		makePolicyConfig("balanced", config.DecisionPolicyWeights{RouterBaseline: 1}, nil, 0),
		makePolicyConfig("canary-bad", config.DecisionPolicyWeights{Latency: 1}, nil, 0),
	}
	cfg.Autopilot = config.AutopilotConfig{
		Enabled:           true,
		PolicyID:          "balanced",
		CandidatePolicyID: "canary-bad",
		CanaryPercent:     100,
		MinSamples:        5,
		MinPromoteSamples: 1000,
		MaxErrorRate:      0.2,
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1", DecisionPolicy: "balanced"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-auto", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1, Priority: 0}}},
	}
	cfg.Admin.APIKey = "autopilot-admin-secret"
	s := testGateway(t, cfg)
	if s.autopilot == nil || s.autopilot.Status().State != autopilot.StateCanary {
		t.Fatal("controller not in canary state")
	}

	post := func(action string, withKey bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/autopilot", strings.NewReader(`{"action":"`+action+`"}`))
		req.RemoteAddr = "127.0.0.1:12345"
		if !withKey {
			// A distinct source address: the failed attempt
			// must not trip the per-IP admin throttle for the
			// authenticated calls that follow.
			req.RemoteAddr = "127.0.0.2:12345"
		}
		req.Header.Set("Content-Type", "application/json")
		if withKey {
			req.Header.Set("x-admin-key", cfg.Admin.APIKey)
		}
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}

	if rr := post("evaluate", false); rr.Code != http.StatusUnauthorized && rr.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated action status = %d, want 401/403", rr.Code)
	}
	if rr := post("bogus", true); rr.Code != http.StatusBadRequest {
		t.Fatalf("bogus action status = %d, want 400", rr.Code)
	}

	rr := post("evaluate", true)
	if rr.Code != http.StatusOK {
		t.Fatalf("evaluate status = %d body = %s", rr.Code, rr.Body.String())
	}
	var evalPayload struct {
		Gate autopilot.GateResult `json:"gate"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &evalPayload); err != nil || evalPayload.Gate.Verdict != autopilot.VerdictHold {
		t.Fatalf("evaluate payload = %s (err %v), want a hold verdict on thin data", rr.Body.String(), err)
	}
	if s.autopilot.Status().State != autopilot.StateCanary {
		t.Fatal("evaluate changed the lifecycle state")
	}

	// Promotion is refused while the gate cannot permit it —
	// and the refusal explains itself with the gate decision.
	rr = post("promote", true)
	if rr.Code != http.StatusConflict {
		t.Fatalf("promote status = %d, want 409 (body %s)", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "insufficient_samples") {
		t.Fatalf("promote refusal lacks the gate reasons: %s", rr.Body.String())
	}
	if s.autopilot.Status().State != autopilot.StateCanary {
		t.Fatal("refused promotion changed the lifecycle state")
	}

	// Operator rollback succeeds and is recorded with its cause.
	rr = post("rollback", true)
	if rr.Code != http.StatusOK {
		t.Fatalf("rollback status = %d body = %s", rr.Code, rr.Body.String())
	}
	st := s.autopilot.Status()
	if st.State != autopilot.StateRolledBack || st.LastRollback == nil || st.LastRollback.Kind != "operator_rollback" {
		t.Fatalf("status after operator rollback = %+v", st)
	}
	if rr := post("rollback", true); rr.Code != http.StatusConflict {
		t.Fatalf("second rollback status = %d, want 409 (no candidate)", rr.Code)
	}

	// Disabled autopilot: actions conflict, GET still reports disabled.
	cfg2 := config.Default()
	cfg2.Admin.APIKey = "autopilot-admin-secret"
	s2 := testGateway(t, cfg2)
	req := httptest.NewRequest(http.MethodPost, "http://gateway/admin/api/autopilot", strings.NewReader(`{"action":"evaluate"}`))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("x-admin-key", cfg2.Admin.APIKey)
	rr2 := httptest.NewRecorder()
	s2.Handler().ServeHTTP(rr2, req)
	if rr2.Code != http.StatusConflict {
		t.Fatalf("disabled-autopilot action status = %d, want 409 (body %s)", rr2.Code, rr2.Body.String())
	}
}
