package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/autopilot"
	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// TestAutopilotCanaryRollsBackBadPolicy is the Phase 18 acceptance E2E:
// a deliberately bad policy version is canaried, tripped by a gate built from
// real gateway outcomes, and automatically rolled back to the incumbent, with
// the incident visible on the operator surface.
func TestAutopilotCanaryRollsBackBadPolicy(t *testing.T) {
	var healthyHits, failingHits atomic.Int64
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthyHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(healthy.Close)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failingHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"injected bad-policy failure"}}`)
	}))
	t.Cleanup(failing.Close)

	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Routing.Strategy = "priority"
	cfg.Routing.MaxAttempts = 1
	cfg.Routing.RetryBackoffMS = 0
	// Keep the failing deployment eligible for the whole test so the candidate
	// policy's real failure rate is observable (no health-driven escape).
	cfg.Routing.FailureThreshold = 1000
	cfg.Routing.ProviderFailureThreshold = 1000
	cfg.Decision.Mode = "local"
	cfg.Decision.Provider = "policy"
	cfg.Decision.Policy = "balanced"
	cfg.Decision.TimeoutMS = 100
	cfg.DecisionPolicies = []config.DecisionPolicyConfig{
		// Incumbent prefers the router order → healthy deployment p1/m1.
		makePolicyConfig("balanced", config.DecisionPolicyWeights{RouterBaseline: 1}, nil, 0),
		// Bad candidate prefers the lowest-latency deployment → p2/m2, which
		// always fails.
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
	cfg.CandidatePools = []config.CandidatePoolConfig{{ID: "pool1", Mode: "explicit", Deployments: []string{"p1/m1", "p2/m2"}}}
	cfg.RouteProfiles = []config.RouteProfileConfig{{ID: "rp1", CandidatePool: "pool1", DecisionPolicy: "balanced"}}
	trueVal := true
	cfg.VirtualEndpoints = []config.VirtualEndpointConfig{{ID: "ve1", PublicModel: "nexa-auto", RouteProfile: "rp1", Enabled: &trueVal, Protocols: []string{"openai"}}}
	cfg.Providers = []config.ProviderConfig{
		// Equal priority keeps both deployments in the same policy band so the
		// candidate policy can legitimately prefer either one.
		{ID: "p1", Type: "openai_compatible", BaseURL: healthy.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m1", Model: "model-a", Enabled: true, Weight: 1, Priority: 0}}},
		{ID: "p2", Type: "openai_compatible", BaseURL: failing.URL, AuthMode: "none", Enabled: true, Models: []config.ModelConfig{{ID: "m2", Model: "model-b", Enabled: true, Weight: 1, Priority: 0}}},
	}
	cfg.Admin.APIKey = "autopilot-admin-secret"

	s := testGateway(t, cfg)
	if s.autopilot == nil {
		t.Fatal("autopilot controller not built from enabled config")
	}
	// Seed latency telemetry so the bad candidate deterministically prefers the
	// failing deployment (p2/m2) while the incumbent prefers p1/m1.
	s.hm.RecordSuccess("p1/m1", 100*time.Millisecond)
	s.hm.RecordSuccess("p2/m2", time.Millisecond)

	doReq := func(id string) *httptest.ResponseRecorder {
		body := `{"model":"nexa-auto","messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "http://gateway/v1/chat/completions", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-Id", id)
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, req)
		return rr
	}

	// Canary phase: requests are served by the bad policy → they fail.
	var sawCanaryFailure bool
	for i := 0; i < 12; i++ {
		rr := doReq(fmt.Sprintf("canary-%d", i))
		if rr.Code == http.StatusServiceUnavailable {
			sawCanaryFailure = true
		}
		if s.autopilot.Status().State == autopilot.StateRolledBack {
			break
		}
	}
	if !sawCanaryFailure {
		t.Fatal("canary phase produced no failing requests; bad policy was not exercised")
	}

	st := s.autopilot.Status()
	if st.State != autopilot.StateRolledBack {
		t.Fatalf("state=%s want rolled_back (gate did not trip on real errors)", st.State)
	}
	if st.LastRollback == nil {
		t.Fatal("no rollback incident recorded")
	}
	if st.LastRollback.PolicyID != "balanced" {
		t.Fatalf("incident policy=%q want balanced", st.LastRollback.PolicyID)
	}
	if st.LastRollback.BadVersion == "" || st.LastRollback.BadVersion == st.LastRollback.RestoredVersion {
		t.Fatalf("incident versions not distinct: %+v", st.LastRollback)
	}
	if len(st.LastRollback.Gate.Reasons) == 0 {
		t.Fatal("incident gate carries no evidence reason")
	}
	if st.LastRollback.Gate.Signals.Requests < 5 {
		t.Fatalf("incident gate has too few samples: %+v", st.LastRollback.Gate.Signals)
	}

	// Post-rollback: served traffic is back on the incumbent and succeeds.
	healthyBefore := healthyHits.Load()
	rr := doReq("post-rollback")
	if rr.Code != http.StatusOK {
		t.Fatalf("post-rollback request status=%d body=%s want 200", rr.Code, rr.Body.String())
	}
	if healthyHits.Load() <= healthyBefore {
		t.Fatal("post-rollback request was not served by the restored incumbent deployment")
	}
	if got := s.autopilot.Status().State; got != autopilot.StateRolledBack {
		t.Fatalf("state changed without new canary: %s", got)
	}

	// The incident is visible on the operator surface the UI reads.
	adminReq := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/autopilot", nil)
	adminReq.RemoteAddr = "127.0.0.1:12345"
	adminReq.Header.Set("x-admin-key", cfg.Admin.APIKey)
	adminRR := httptest.NewRecorder()
	s.Handler().ServeHTTP(adminRR, adminReq)
	if adminRR.Code != http.StatusOK {
		t.Fatalf("autopilot endpoint status=%d body=%s", adminRR.Code, adminRR.Body.String())
	}
	var payload struct {
		Enabled   bool             `json:"enabled"`
		Autopilot autopilot.Status `json:"autopilot"`
	}
	if err := json.Unmarshal(adminRR.Body.Bytes(), &payload); err != nil {
		t.Fatalf("autopilot endpoint payload: %v body=%s", err, adminRR.Body.String())
	}
	if !payload.Enabled || payload.Autopilot.State != autopilot.StateRolledBack {
		t.Fatalf("endpoint state=%+v want enabled rolled_back", payload)
	}
	if payload.Autopilot.LastRollback == nil || len(payload.Autopilot.Incidents) == 0 {
		t.Fatalf("endpoint does not expose the rollback incident: %s", adminRR.Body.String())
	}

	// The UI "Autopilot" block reads the admin snapshot; verify the incident is
	// present there too (the surface is real backend state, not a second store).
	snapReq := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
	snapReq.RemoteAddr = "127.0.0.1:12345"
	snapReq.Header.Set("x-admin-key", cfg.Admin.APIKey)
	snapRR := httptest.NewRecorder()
	s.Handler().ServeHTTP(snapRR, snapReq)
	if snapRR.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", snapRR.Code, snapRR.Body.String())
	}
	var snap struct {
		Autopilot struct {
			Enabled bool             `json:"enabled"`
			Status  autopilot.Status `json:"status"`
		} `json:"autopilot"`
	}
	if err := json.Unmarshal(snapRR.Body.Bytes(), &snap); err != nil {
		t.Fatalf("snapshot payload: %v", err)
	}
	if !snap.Autopilot.Enabled || snap.Autopilot.Status.State != autopilot.StateRolledBack {
		t.Fatalf("snapshot autopilot=%+v want enabled rolled_back", snap.Autopilot)
	}
	if snap.Autopilot.Status.LastRollback == nil {
		t.Fatalf("snapshot missing rollback incident: %s", snapRR.Body.String())
	}
}

// TestAutopilotDisabledReportsDisabled proves the operator surface never
// fabricates a status when the feature is off.
func TestAutopilotDisabledReportsDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.Probe.Enabled = false
	cfg.Admin.APIKey = "autopilot-admin-secret"
	s := testGateway(t, cfg)
	if s.autopilot != nil {
		t.Fatal("autopilot built while disabled")
	}
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/autopilot", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("x-admin-key", cfg.Admin.APIKey)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"enabled":false`) {
		t.Fatalf("disabled payload=%s", rr.Body.String())
	}
}

func TestAutopilotRecordsFailoverTTFTAndPost200StreamFailure(t *testing.T) {
	incumbent := autopilot.PolicyRef{ID: "balanced", Version: "incumbent"}
	ctrl := autopilot.New(incumbent, incumbent, autopilot.GateConfig{
		MinSamples: 1, MinPromoteSamples: 10, MaxErrorRate: 1, MaxErrorRateDelta: 0.99,
		MaxFailoverRate: 1, MaxNoEligibleRate: 1, MaxBudgetAnomalies: 100,
	})
	if err := ctrl.StartCanary(autopilot.PolicyRef{ID: "balanced", Version: "candidate"}, 100, 0); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	s := &Server{autopilot: ctrl}
	if _, leased := s.autopilotSelectPolicy("balanced", "signal-request"); !leased {
		t.Fatal("expected a policy lease")
	}
	s.autopilotRecordFailover("signal-request")
	s.autopilotRecordTTFT("signal-request", 15*time.Millisecond)
	s.autopilotMarkTerminalFailure("signal-request")
	// HTTP 200 is intentional: a failed stream terminal must override status-only success.
	s.autopilotRecordFinish("signal-request", http.StatusOK, "succeeded", 50*time.Millisecond)

	signals := ctrl.Status().CandidateSignals
	if signals.Requests != 1 || signals.Successes != 0 || signals.Errors != 1 || signals.TransportErrors != 1 {
		t.Fatalf("terminal sample incorrectly classified: %+v", signals)
	}
	if signals.Failovers != 1 || signals.FailoverRate != 1 {
		t.Fatalf("real failover not recorded: %+v", signals)
	}
	if signals.TTFTCount != 1 || signals.TTFTP95MS != 15 {
		t.Fatalf("measured TTFT not recorded: %+v", signals)
	}
	if signals.NoEligibleObserved != 0 || signals.BudgetAnomaliesObserved != 0 {
		t.Fatalf("unsupported signal dimensions were falsely counted as zero observations: %+v", signals)
	}
	if _, exists := s.autopilotLeases.Load("signal-request"); exists {
		t.Fatal("policy lease leaked after request finish")
	}
	if _, exists := s.autopilotRequestSignals.Load("signal-request"); exists {
		t.Fatal("request signal state leaked after request finish")
	}
}
