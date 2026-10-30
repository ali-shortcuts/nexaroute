package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/autopilot"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision/policy"
)

// Wave 2 (issue #663): autopilot.go blocks uncovered in the 2026-10-29
// baseline profile. All controller interactions use the real autopilot
// package, not a stub, so the assertions pin observable controller state
// (in-flight leases, candidate presence, signal windows, promotion).

func TestWave2AutopilotControllerPolicyFallback(t *testing.T) {
	cfg := config.Default()
	cfg.Autopilot.Enabled = true
	cfg.Decision.Policy = "p1"
	// No explicit Autopilot.PolicyID: the decision policy is the target.
	ctrl := newAutopilotController(cfg, []policy.Policy{{ID: "p1", Version: "v1"}})
	if ctrl == nil {
		t.Fatal("decision-policy fallback must build a controller")
	}
	if ctrl.TargetPolicyID() != "p1" {
		t.Fatalf("target = %q, want p1", ctrl.TargetPolicyID())
	}
}

func TestWave2AutopilotControllerTargetMissing(t *testing.T) {
	cfg := config.Default()
	cfg.Autopilot.Enabled = true
	cfg.Autopilot.PolicyID = "ghost"
	if ctrl := newAutopilotController(cfg, []policy.Policy{{ID: "p1", Version: "v1"}}); ctrl != nil {
		t.Fatal("an absent target policy must yield no controller")
	}
}

func TestWave2AutopilotControllerShadowCandidate(t *testing.T) {
	cfg := config.Default()
	cfg.Autopilot.Enabled = true
	cfg.Autopilot.PolicyID = "p1"
	cfg.Autopilot.CandidatePolicyID = "cand"
	// Zero canary settings: the candidate begins as a shadow, not a canary.
	ctrl := newAutopilotController(cfg, []policy.Policy{
		{ID: "p1", Version: "v1"}, {ID: "cand", Version: "v2"},
	})
	if ctrl == nil {
		t.Fatal("shadow-candidate config must build a controller")
	}
	st := ctrl.Status()
	if st.Candidate == nil || st.Candidate.ID != "cand" {
		t.Fatalf("status candidate = %+v, want cand", st.Candidate)
	}
	if st.State != autopilot.StateShadow {
		t.Fatalf("state = %v, want shadow", st.State)
	}
}

func TestWave2AutopilotSignalsForEmptyAndWrongType(t *testing.T) {
	s := &Server{}
	if got := s.autopilotSignalsFor(""); got != nil {
		t.Fatalf("empty request ID must yield no signals, got %+v", got)
	}
	s.autopilotLeases.Store("r1", "not-a-lease-entry")
	if got := s.autopilotSignalsFor("r1"); got != nil {
		t.Fatalf("wrong-type lease entry must yield no signals, got %+v", got)
	}
}

func TestWave2AutopilotCallerStratum(t *testing.T) {
	if got := autopilotCallerStratum(clientIdentity{ProjectID: "proj-7"}, true); got != "project:proj-7" {
		t.Fatalf("project stratum = %q", got)
	}
	if got := autopilotCallerStratum(clientIdentity{ID: "user-9"}, true); got != "user-9" {
		t.Fatalf("plain ID stratum = %q", got)
	}
	if got := autopilotCallerStratum(clientIdentity{}, true); got != "anonymous" {
		t.Fatalf("empty identity stratum = %q", got)
	}
}

func TestWave2AutopilotObserveNoEligibleTargetMismatch(t *testing.T) {
	ctrl := autopilot.New(
		autopilot.PolicyRef{ID: "target", Version: "v1"},
		autopilot.PolicyRef{ID: "target", Version: "v1"},
		autopilot.GateConfig{},
	)
	s := &Server{autopilot: ctrl}
	cfg := config.Default()
	cfg.Decision.Policy = "other-policy"
	before := ctrl.Status().IncumbentSignals.Requests
	s.autopilotObserveNoEligible(cfg, nil, "req-key", "stratum")
	if after := ctrl.Status().IncumbentSignals.Requests; after != before {
		t.Fatalf("mismatched target must not observe: requests %d -> %d", before, after)
	}
}

func TestWave2AutopilotRecordTTFTNegative(t *testing.T) {
	signals := &autopilotRequestSignals{}
	s := &Server{}
	s.autopilotLeases.Store("r1", autopilotLeaseEntry{signals: signals})
	s.autopilotRecordTTFT("r1", -time.Second)
	signals.mu.Lock()
	defer signals.mu.Unlock()
	if signals.hasTTFT {
		t.Fatal("a negative TTFT duration must not be recorded")
	}
}

func TestWave2AutopilotRecordFinishEmptyAndNilController(t *testing.T) {
	s := &Server{}
	// Empty request ID returns before any lease lookup.
	s.autopilotRecordFinish("", 200, "", time.Second)
	// A lease entry without a controller returns without recording.
	s.autopilotLeases.Store("r1", autopilotLeaseEntry{})
	s.autopilotRecordFinish("r1", 500, "failed", time.Second)
	if _, leased := s.autopilotLeases.Load("r1"); leased {
		t.Fatal("finished lease must be removed even on the nil-controller path")
	}
}

func TestWave2AutopilotRecordFinishCancelledReleases(t *testing.T) {
	ctrl := autopilot.New(
		autopilot.PolicyRef{ID: "target", Version: "v1"},
		autopilot.PolicyRef{ID: "target", Version: "v1"},
		autopilot.GateConfig{},
	)
	lease := ctrl.Select("req-key")
	s := &Server{}
	s.autopilotLeases.Store("r1", autopilotLeaseEntry{controller: ctrl, lease: lease, signals: &autopilotRequestSignals{}})
	if ctrl.Status().InFlight != 1 {
		t.Fatalf("in-flight = %d, want 1 before finish", ctrl.Status().InFlight)
	}
	s.autopilotRecordFinish("r1", 200, "cancelled", time.Second)
	if ctrl.Status().InFlight != 0 {
		t.Fatalf("cancelled finish must release the lease, in-flight = %d", ctrl.Status().InFlight)
	}
	if _, leased := s.autopilotLeases.Load("r1"); leased {
		t.Fatal("cancelled finish must remove the lease entry")
	}
}

func TestWave2AutopilotClassifyStatus(t *testing.T) {
	cases := []struct {
		status int
		want   autopilot.ErrorClass
	}{
		{http.StatusTooManyRequests, autopilot.ClassRateLimit},
		{http.StatusUnauthorized, autopilot.ClassAuthError},
		{http.StatusForbidden, autopilot.ClassAuthError},
		{http.StatusGatewayTimeout, autopilot.ClassTimeout},
		{http.StatusRequestTimeout, autopilot.ClassTimeout},
		{http.StatusBadRequest, autopilot.ClassClientError},
	}
	for _, tc := range cases {
		if got := classifyAutopilotStatus(tc.status); got != tc.want {
			t.Fatalf("status %d: class = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestWave2AutopilotActionInvalidJSON(t *testing.T) {
	ctrl := autopilot.New(
		autopilot.PolicyRef{ID: "target", Version: "v1"},
		autopilot.PolicyRef{ID: "target", Version: "v1"},
		autopilot.GateConfig{},
	)
	s := &Server{autopilot: ctrl}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/autopilot", strings.NewReader("not json"))
	rr := httptest.NewRecorder()
	s.adminAutopilot(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed action body: status=%d body=%s, want 400", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "invalid JSON body") {
		t.Fatalf("body=%s, want the invalid-JSON error", rr.Body.String())
	}
}

func TestWave2AutopilotActionPromoteSuccess(t *testing.T) {
	ctrl := autopilot.New(
		autopilot.PolicyRef{ID: "target", Version: "v1"},
		autopilot.PolicyRef{ID: "target", Version: "v1"},
		autopilot.GateConfig{
			MinSamples: 2, MinPromoteSamples: 2,
			MaxErrorRate: 1, MaxErrorRateDelta: 1, MaxFailoverRate: 1,
			MaxNoEligibleRate: 1, MaxBudgetAnomalies: 100,
		},
	)
	if err := ctrl.StartCanary(autopilot.PolicyRef{ID: "cand", Version: "v2"}, 100, 0); err != nil {
		t.Fatal(err)
	}
	// Two successful canary samples with full signal coverage satisfy
	// every gate: no errors, no failovers, and both the no-eligible and
	// budget-anomaly signals observed on every request.
	for i := 0; i < 2; i++ {
		lease := ctrl.Select("promote-key")
		if !lease.Canary {
			t.Fatalf("100%% canary must issue canary leases, got %+v", lease)
		}
		ctrl.Record(lease, autopilot.Sample{
			Success: true, HasNoEligibleSignal: true, HasBudgetAnomalySignal: true,
		})
	}
	if res := ctrl.Evaluate(); res.Verdict != autopilot.VerdictPromote {
		t.Fatalf("gate verdict = %v (%v), want promote", res.Verdict, res.Reasons)
	}
	s := &Server{autopilot: ctrl}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/autopilot", strings.NewReader(`{"action":"promote"}`))
	rr := httptest.NewRecorder()
	s.adminAutopilot(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("promote action: status=%d body=%s, want 200", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"action":"promote"`) {
		t.Fatalf("body=%s, want the promote action echo", rr.Body.String())
	}
	st := ctrl.Status()
	if st.State != autopilot.StatePromoted || st.Incumbent.ID != "cand" {
		t.Fatalf("post-promote state=%v incumbent=%q, want promoted/cand", st.State, st.Incumbent.ID)
	}
}
