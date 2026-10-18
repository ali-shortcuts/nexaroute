package httpapi

import (
	"net/http"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/autopilot"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision/policy"
)

// Phase 18: routing-policy canary autopilot wiring.
//
// The controller only orchestrates policy versions that already exist as
// decision_policies[] entries. Selection is a per-request lease; the outcome is
// attributed to the lease at request finish so a rollback can never rewrite an
// in-flight request. Signals are real gateway outcomes — status, terminal
// state and duration — never synthesized telemetry.

func gateConfigFrom(c config.AutopilotConfig) autopilot.GateConfig {
	return autopilot.GateConfig{
		MinSamples:         c.MinSamples,
		MinPromoteSamples:  c.MinPromoteSamples,
		MaxErrorRate:       c.MaxErrorRate,
		MaxErrorRateDelta:  c.MaxErrorRateDelta,
		MaxFailoverRate:    c.MaxFailoverRate,
		MaxNoEligibleRate:  c.MaxNoEligibleRate,
		MaxBudgetAnomalies: c.MaxBudgetAnomalies,
		MaxLatencyP95MS:    c.MaxLatencyP95MS,
		MaxTTFTP95MS:       c.MaxTTFTP95MS,
	}
}

// newAutopilotController builds the canary controller from config. It returns
// nil when autopilot is disabled or the referenced policies are absent.
func newAutopilotController(cfg config.Config, policies []policy.Policy) *autopilot.Controller {
	if !cfg.Autopilot.Enabled {
		return nil
	}
	target := cfg.Autopilot.PolicyID
	if target == "" {
		target = cfg.Decision.Policy
	}
	byID := make(map[string]policy.Policy, len(policies))
	for _, p := range policies {
		byID[p.ID] = p
	}
	incumbent, ok := byID[target]
	if !ok {
		return nil
	}
	ctrl := autopilot.New(
		autopilot.PolicyRef{ID: incumbent.ID, Version: incumbent.Version},
		autopilot.PolicyRef{ID: incumbent.ID, Version: incumbent.Version},
		gateConfigFrom(cfg.Autopilot),
	)
	if candID := cfg.Autopilot.CandidatePolicyID; candID != "" {
		if cand, ok := byID[candID]; ok {
			ref := autopilot.PolicyRef{ID: cand.ID, Version: cand.Version}
			if cfg.Autopilot.CanaryPercent > 0 || cfg.Autopilot.CanaryMaxRequests > 0 {
				_ = ctrl.StartCanary(ref, cfg.Autopilot.CanaryPercent, cfg.Autopilot.CanaryMaxRequests)
			} else {
				ctrl.BeginShadow(ref)
			}
		}
	}
	return ctrl
}

func (s *Server) autopilotSnapshot() *autopilot.Controller {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.autopilot
}

// autopilotSelectPolicy overrides the requested policy id with the autopilot's
// lease when the policy under change is the one selected for this request. It
// stores the lease for outcome attribution at request finish.
func (s *Server) autopilotSelectPolicy(policyID, requestID string) (string, bool) {
	ctrl := s.autopilotSnapshot()
	if ctrl == nil || !ctrl.TargetMatches(policyID) {
		return policyID, false
	}
	lease := ctrl.Select(requestID)
	if requestID != "" {
		s.autopilotLeases.Store(requestID, lease)
	}
	return lease.Policy.ID, true
}

// autopilotRecordFinish attributes a data-plane request outcome to the policy
// version it started under. Cancelled requests are released without being
// counted as policy failures.
func (s *Server) autopilotRecordFinish(requestID string, status int, terminal string, duration time.Duration) {
	ctrl := s.autopilotSnapshot()
	if ctrl == nil || requestID == "" {
		return
	}
	raw, ok := s.autopilotLeases.LoadAndDelete(requestID)
	if !ok {
		return
	}
	lease, ok := raw.(autopilot.Lease)
	if !ok {
		return
	}
	if terminal == "cancelled" {
		ctrl.Release(lease)
		return
	}
	ctrl.Record(lease, autopilot.Sample{
		Success:    status < http.StatusBadRequest,
		LatencyMS:  float64(duration.Milliseconds()),
		ErrorClass: classifyAutopilotStatus(status),
	})
}

func classifyAutopilotStatus(status int) autopilot.ErrorClass {
	switch {
	case status < http.StatusBadRequest:
		return autopilot.ClassNone
	case status == http.StatusTooManyRequests:
		return autopilot.ClassRateLimit
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return autopilot.ClassAuthError
	case status == http.StatusGatewayTimeout || status == http.StatusRequestTimeout:
		return autopilot.ClassTimeout
	case status >= http.StatusInternalServerError:
		return autopilot.ClassServerError
	default:
		return autopilot.ClassClientError
	}
}

// adminAutopilot serves GET /admin/api/autopilot — the bounded operator surface
// the UI "Autopilot" block reads. It is fed entirely by controller state; when
// autopilot is disabled it reports enabled=false rather than an assumed status.
func (s *Server) adminAutopilot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctrl := s.autopilotSnapshot()
	if ctrl == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":   true,
		"autopilot": ctrl.Status(),
	})
}
