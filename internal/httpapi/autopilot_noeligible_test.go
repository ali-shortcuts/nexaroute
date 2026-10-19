package httpapi

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/autopilot"
	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// #418: the gate's no-eligible signal is REAL telemetry now.
// A canary whose candidate set is empty (the pool's only
// deployment is disabled) produces no-compatible-deployment
// rejections; those outcomes are attributed to the policy
// version the requests would have leased to, the candidate
// window's no-eligible rate reaches 1.0 on observed data, and
// the gate rolls the canary back BY ITSELF — no leased sample
// is involved. Requests rejected before route resolution
// (malformed bodies) observe nothing.
func TestAutopilotNoEligibleSignalTripsRollback(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
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
		// Error-rate gate deliberately inert (rate 1.0 is not
		// > 1.0): only the no-eligible gate can trip.
		MaxErrorRate:      1.0,
		MaxNoEligibleRate: 0.5,
	}
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1", DecisionPolicy: "balanced"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-auto", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		{ID: "p1", Type: "openai_compatible", BaseURL: "http://127.0.0.1:1", AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: false, Weight: 1, Priority: 0}}},
	}
	s := testGateway(t, cfg)
	if s.autopilot == nil {
		t.Fatal("autopilot controller not built from enabled config")
	}

	doReq := func(id, body string) int {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-Id", id)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr.Code
	}
	goodBody := `{"model":"nexa-auto","messages":[{"role":"user","content":"hi"}]}`

	// Pre-dispatch rejections observe nothing.
	for i := 0; i < 2; i++ {
		if code := doReq(fmt.Sprintf("bad-%d", i), `{"model":`); code != http.StatusBadRequest {
			t.Fatalf("malformed request status = %d, want 400", code)
		}
	}
	if got := s.autopilot.Status().CandidateSignals.NoEligibleObserved; got != 0 {
		t.Fatalf("no-eligible observations after pre-dispatch rejections = %d, want 0", got)
	}

	// The no-eligible storm: every request 503s; the gate must
	// trip on the no-eligible signal alone.
	for i := 0; i < 12; i++ {
		if code := doReq(fmt.Sprintf("ne-%d", i), goodBody); code != http.StatusServiceUnavailable {
			t.Fatalf("request %d status = %d, want 503", i, code)
		}
		if s.autopilot.Status().State == autopilot.StateRolledBack {
			break
		}
	}
	st := s.autopilot.Status()
	if st.State != autopilot.StateRolledBack {
		t.Fatalf("state = %s, want rolled_back (candidate signals %+v)", st.State, st.CandidateSignals)
	}
	// The rollback resets the candidate window for the next
	// canary; the evidence that tripped it is preserved in the
	// gate decision's signal snapshot.
	if st.LastGateDecision == nil {
		t.Fatal("no gate decision recorded")
	}
	cs := st.LastGateDecision.Signals
	if cs.NoEligibleObserved < 5 || cs.NoEligible != cs.NoEligibleObserved || cs.NoEligibleRate != 1 {
		t.Fatalf("tripping gate signals = %+v, want observed>=5, all no-eligible, rate 1", cs)
	}
	if !strings.Contains(strings.Join(st.LastGateDecision.Reasons, " "), "no_eligible_rate") {
		t.Fatalf("gate reasons = %v, want a no_eligible_rate violation", st.LastGateDecision.Reasons)
	}
	if st.LastGateDecision.Verdict != autopilot.VerdictRollback {
		t.Fatalf("gate verdict = %s, want rollback", st.LastGateDecision.Verdict)
	}
	if st.LastRollback == nil {
		t.Fatal("no rollback incident recorded")
	}
	// Attribution follows state: after the rollback, further
	// no-eligible outcomes land in the incumbent window.
	before := s.autopilot.Status().IncumbentSignals.NoEligibleObserved
	_ = doReq("ne-after", goodBody)
	after := s.autopilot.Status().IncumbentSignals.NoEligibleObserved
	if after != before+1 {
		t.Fatalf("incumbent no-eligible observed %d -> %d, want +1 after rollback", before, after)
	}
}
