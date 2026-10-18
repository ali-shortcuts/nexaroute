package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/autopilot"
	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision/policy"
	"github.com/ali-shortcuts/nexaroute/internal/route"
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

func autopilotConfigNeedsRebuild(current *autopilot.Controller, previous, next config.AutopilotConfig, target string, policyDefinitionsChanged bool) bool {
	return current == nil || previous != next || current.TargetPolicyID() != target || policyDefinitionsChanged
}

func (s *Server) autopilotSnapshot() *autopilot.Controller {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.autopilot
}

// autopilotRequestKeyContext carries a gateway-generated per-request key. The
// public x-request-id is caller controlled and must not be used as a map key
// for in-flight leases because concurrent requests can legitimately reuse it.
type autopilotRequestKeyContext struct{}

func autopilotRequestKeyFromContext(ctx context.Context, fallback string) string {
	if ctx != nil {
		if key, ok := ctx.Value(autopilotRequestKeyContext{}).(string); ok && key != "" {
			return key
		}
	}
	return fallback
}

// autopilotLeaseEntry pins each in-flight request to the controller that issued
// its lease. Config reload may replace or disable Server.autopilot while the
// request is still running; the terminal result must finish on the original
// controller, not be folded into a newly configured policy window.
type autopilotLeaseEntry struct {
	controller *autopilot.Controller
	lease      autopilot.Lease
	signals    *autopilotRequestSignals
}

// autopilotSelectPolicy overrides the requested policy id with the autopilot's
// lease when the policy under change is the one selected for this request. It
// stores the lease and issuing controller for outcome attribution at finish.
func (s *Server) autopilotSelectPolicy(policyID, requestID string) (string, bool) {
	ctrl := s.autopilotSnapshot()
	if ctrl == nil || !ctrl.TargetMatches(policyID) {
		return policyID, false
	}
	lease := ctrl.Select(requestID)
	if requestID != "" {
		s.autopilotLeases.Store(requestID, autopilotLeaseEntry{
			controller: ctrl,
			lease:      lease,
			signals:    &autopilotRequestSignals{},
		})
	}
	return lease.Policy.ID, true
}

// autopilotRequestSignals holds authoritative request-scoped observations until
// the middleware closes the policy lease. It is separate from the controller
// so signals are captured at the data-plane source of truth.
type autopilotRequestSignals struct {
	mu              sync.Mutex
	closed          bool
	failover        bool
	terminalFailure bool
	ttftMS          float64
	hasTTFT         bool
	// dispatched records that the request reached candidate
	// selection (at least one route attempt was emitted). It is
	// the leased half of the gate's REAL no-eligible signal
	// (#418): a leased request demonstrably found eligible
	// candidates, so it counts as a no-eligible OBSERVATION
	// with NoEligible=false. Requests that found NO candidates
	// never lease (the decision plane is never reached); their
	// outcomes enter through Controller.ObserveNoEligible.
	dispatched bool
}

func (s *Server) autopilotSignalsFor(requestID string) *autopilotRequestSignals {
	if requestID == "" {
		return nil
	}
	raw, leased := s.autopilotLeases.Load(requestID)
	if !leased {
		return nil
	}
	entry, ok := raw.(autopilotLeaseEntry)
	if !ok {
		return nil
	}
	return entry.signals
}

func (s *Server) autopilotRecordFailover(requestID string) {
	if signals := s.autopilotSignalsFor(requestID); signals != nil {
		signals.mu.Lock()
		if !signals.closed {
			signals.failover = true
		}
		signals.mu.Unlock()
	}
}

// autopilotNoteDispatched records that a leased request reached
// candidate selection (a route attempt was emitted for it).
func (s *Server) autopilotNoteDispatched(requestID string) {
	if signals := s.autopilotSignalsFor(requestID); signals != nil {
		signals.mu.Lock()
		if !signals.closed {
			signals.dispatched = true
		}
		signals.mu.Unlock()
	}
}

// autopilotObserveNoEligible attributes a no-compatible-healthy-
// deployment rejection to the autopilot controller when the
// request's route resolves to the policy under change. The
// request holds no lease (the decision plane was never
// reached), so the controller attributes the outcome to the
// policy version the request would have been leased to. The
// policy resolution mirrors applyDecisionPlane: the route
// profile's decision policy wins over the global default.
func (s *Server) autopilotObserveNoEligible(cfg config.Config, resolvedRoute *route.ResolvedRoute, requestKey string) {
	ctrl := s.autopilotSnapshot()
	if ctrl == nil {
		return
	}
	policyID := cfg.Decision.Policy
	if resolvedRoute != nil && resolvedRoute.RouteProfileID != "" {
		for _, rp := range cfg.RouteProfiles {
			if rp.ID == resolvedRoute.RouteProfileID && rp.DecisionPolicy != "" {
				policyID = rp.DecisionPolicy
				break
			}
		}
	}
	if !ctrl.TargetMatches(policyID) {
		return
	}
	ctrl.ObserveNoEligible(requestKey)
}

func (s *Server) autopilotRecordTTFT(requestID string, duration time.Duration) {
	if duration < 0 {
		return
	}
	if signals := s.autopilotSignalsFor(requestID); signals != nil {
		signals.mu.Lock()
		if !signals.closed {
			signals.ttftMS = float64(duration) / float64(time.Millisecond)
			signals.hasTTFT = true
		}
		signals.mu.Unlock()
	}
}

// This is called only when a stream has already committed headers, so HTTP 200
// cannot be treated as proof that the upstream stream completed successfully.
func (s *Server) autopilotMarkTerminalFailure(requestID string) {
	if signals := s.autopilotSignalsFor(requestID); signals != nil {
		signals.mu.Lock()
		if !signals.closed {
			signals.terminalFailure = true
		}
		signals.mu.Unlock()
	}
}

// autopilotRecordFinish attributes a data-plane request outcome to the policy
// version it started under. Cancelled requests are released without being
// counted as policy failures.
func (s *Server) autopilotRecordFinish(requestID string, status int, terminal string, duration time.Duration) {
	if requestID == "" {
		return
	}
	raw, leased := s.autopilotLeases.LoadAndDelete(requestID)
	if !leased {
		return
	}
	entry, ok := raw.(autopilotLeaseEntry)
	if !ok || entry.controller == nil {
		return
	}
	// Do not look up Server.autopilot here: a reload may have swapped it after
	// this request acquired its lease. Complete on the controller that issued it.
	ctrl := entry.controller
	lease := entry.lease

	var failover, terminalFailure, hasTTFT bool
	var ttftMS float64
	if signals := entry.signals; signals != nil {
		// Close the signal record before snapshotting it. A late callback that
		// already obtained this pointer cannot race a post-finish mutation into
		// an outcome that has already been recorded.
		signals.mu.Lock()
		signals.closed = true
		failover = signals.failover
		terminalFailure = signals.terminalFailure
		hasTTFT = signals.hasTTFT
		ttftMS = signals.ttftMS
		signals.mu.Unlock()
	}
	if terminal == "cancelled" {
		ctrl.Release(lease)
		return
	}
	failed := status >= http.StatusBadRequest || terminal == "failed" || terminalFailure
	errorClass := classifyAutopilotStatus(status)
	if failed && status < http.StatusBadRequest {
		errorClass = autopilot.ClassTransportError
	}
	var dispatched bool
	if signals := entry.signals; signals != nil {
		signals.mu.Lock()
		dispatched = signals.dispatched
		signals.mu.Unlock()
	}
	ctrl.Record(lease, autopilot.Sample{
		Success:             !failed,
		ErrorClass:          errorClass,
		Failover:            failover,
		LatencyMS:           float64(duration.Milliseconds()),
		TTFTMS:              ttftMS,
		HasTTFT:             hasTTFT,
		HasNoEligibleSignal: dispatched,
		NoEligible:          false,
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
	switch r.Method {
	case http.MethodGet:
		ctrl := s.autopilotSnapshot()
		if ctrl == nil {
			writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":             true,
			"autopilot":           ctrl.Status(),
			"unsupported_signals": []string{"budget_anomalies"},
		})
	case http.MethodPost:
		s.adminAutopilotAction(w, r)
	default:
		errorJSON(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// adminAutopilotAction performs one operator gate action
// (#418): evaluate (record the current gate decision without
// changing state), promote (only when the gate itself permits
// — the operator cannot override missing or failing evidence),
// or rollback (withdraw the candidate, restoring the
// incumbent; routing policy only, in-flight leases finish on
// the version they started with). Every outcome — including
// refusals — is returned with the gate decision that explains
// it, and successful actions are visible in the controller's
// transition/incident history.
func (s *Server) adminAutopilotAction(w http.ResponseWriter, r *http.Request) {
	ctrl := s.autopilotSnapshot()
	if ctrl == nil {
		errorJSON(w, http.StatusConflict, "autopilot is not enabled")
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorJSON(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	switch body.Action {
	case "evaluate":
		res := ctrl.Evaluate()
		writeJSON(w, http.StatusOK, map[string]any{
			"action":    "evaluate",
			"gate":      res,
			"autopilot": ctrl.Status(),
		})
	case "promote":
		tr, ok := ctrl.Promote()
		if !ok {
			res := ctrl.Evaluate()
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":     "gate does not permit promotion",
				"gate":      res,
				"autopilot": ctrl.Status(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"action":     "promote",
			"transition": tr,
			"autopilot":  ctrl.Status(),
		})
	case "rollback":
		inc, ok := ctrl.Rollback("operator_rollback")
		if !ok {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":     "no candidate to roll back",
				"autopilot": ctrl.Status(),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"action":    "rollback",
			"incident":  inc,
			"autopilot": ctrl.Status(),
		})
	default:
		errorJSON(w, http.StatusBadRequest, "action must be one of: evaluate, promote, rollback")
	}
}
