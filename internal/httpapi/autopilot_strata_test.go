package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/autopilot"
)

// #418 stratification: a leased request's caller label is
// recorded on its sample, and the candidate window's snapshot
// breaks the evidence down by that label. The label derivation
// never uses a key value or a session key.
func TestAutopilotCallerStratification(t *testing.T) {
	incumbent := autopilot.PolicyRef{ID: "balanced", Version: "incumbent"}
	ctrl := autopilot.New(incumbent, incumbent, autopilot.GateConfig{
		MinSamples: 1, MinPromoteSamples: 10, MaxErrorRate: 1, MaxErrorRateDelta: 0.99,
		MaxFailoverRate: 1, MaxNoEligibleRate: 1, MaxBudgetAnomalies: 100,
	})
	if err := ctrl.StartCanary(autopilot.PolicyRef{ID: "balanced", Version: "candidate"}, 100, 0); err != nil {
		t.Fatalf("StartCanary: %v", err)
	}
	s := &Server{autopilot: ctrl}
	if _, leased := s.autopilotSelectPolicy("balanced", "strata-request"); !leased {
		t.Fatal("expected a policy lease")
	}
	s.autopilotNoteCaller("strata-request", "vk-1")
	s.autopilotRecordFinish("strata-request", http.StatusOK, "succeeded", 10*time.Millisecond)

	sig := ctrl.Status().CandidateSignals
	if len(sig.Strata) != 1 || sig.Strata[0].Stratum != "vk-1" || sig.Strata[0].Signals.Requests != 1 {
		t.Fatalf("candidate strata = %+v, want one vk-1 entry with 1 request", sig.Strata)
	}

	// Label derivation rules.
	if got := autopilotCallerStratum(clientIdentity{Virtual: true, ID: "vk-9"}, true); got != "vk-9" {
		t.Fatalf("virtual label = %q", got)
	}
	if got := autopilotCallerStratum(clientIdentity{TenantID: "acme"}, true); got != "tenant:acme" {
		t.Fatalf("tenant label = %q", got)
	}
	if got := autopilotCallerStratum(clientIdentity{}, false); got != "anonymous" {
		t.Fatalf("anonymous label = %q", got)
	}
	if strings.Contains(autopilotCallerStratum(clientIdentity{Virtual: true, ID: "vk-9"}, true), "key") {
		t.Fatal("label leaks key material")
	}
}
