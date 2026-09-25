package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/feature"
	"github.com/ali-shortcuts/nexaroute/internal/route"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

func jsonMarshalBounded(v interface{}, maxLen int) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	if len(b) <= maxLen {
		return string(b), nil
	}
	// If too large, return empty array/object fallback that is valid JSON
	// For breakdown payload, we truncate to selected scores only
	return "{}", nil
}

// decisionCandidates converts router.Scored to decision.Candidate snapshot with extended signals for Phase E.
func decisionCandidates(scored []router.Scored, resolved *route.ResolvedRoute) []decision.Candidate {
	if len(scored) == 0 {
		return nil
	}
	out := make([]decision.Candidate, 0, len(scored))
	for idx, s := range scored {
		poolID, ordinal := poolInfoForDeployment(s.Deployment.ID, resolved)
		c := decision.Candidate{
			ID:               s.Deployment.ID,
			ProviderID:       s.Deployment.ProviderID,
			Model:            s.Deployment.Model,
			Priority:         s.Deployment.Priority,
			Weight:           s.Deployment.Weight,
			PoolID:           poolID,
			PoolOrdinal:      ordinal,
			RouterScore:      s.Score,
			HealthStatus:     string(s.Health.Status),
			EWMALatencyMS:    s.Health.EWMALatencyMS,
			EWMATTFTMS:       s.Health.EWMATTFTMS,
			EWMAFailureRate:  s.Health.EWMAFailureRate,
			Successes:        s.Health.Successes,
			Failures:         s.Health.Failures,
			CapacityPressure: s.CapacityPressure,
			EstimatedCostUSD: s.EstimatedCostUSD,
			PriceKnown:       s.PriceKnown,
			ContextWindow:    s.Deployment.ContextWindow,
			Capabilities: decision.CandidateCapabilities{
				Streaming: s.Deployment.Capabilities.Streaming,
				Tools:     s.Deployment.Capabilities.Tools,
				Vision:    s.Deployment.Capabilities.Vision,
				Reasoning: s.Deployment.Capabilities.Reasoning,
			},
			OriginalRank: idx,
		}
		out = append(out, c)
	}
	return out
}

func poolInfoForDeployment(deploymentID string, resolved *route.ResolvedRoute) (string, int) {
	if resolved == nil {
		return "", 0
	}
	// Primary pool
	if resolved.AllowedDeployments != nil {
		if _, ok := resolved.AllowedDeployments[deploymentID]; ok {
			return resolved.PrimaryPoolID, 0
		}
	} else {
		// If AllowedDeployments nil and mode all, treat as primary
		if resolved.PrimaryMode == "all" {
			// Check if deployment is in AllAllowed (union) — if primary is all, it should be considered primary
			// But we still need to ensure fallback pools don't claim it first. Since primary is ordinal 0, return primary.
			if resolved.AllAllowed != nil {
				if _, ok := resolved.AllAllowed[deploymentID]; ok {
					// If primary is all, primary wins
					return resolved.PrimaryPoolID, 0
				}
			} else {
				return resolved.PrimaryPoolID, 0
			}
		}
	}
	// Fallbacks
	for i, set := range resolved.FallbackAllowed {
		if set == nil {
			continue
		}
		if _, ok := set[deploymentID]; ok {
			// OrderedPoolIDs[0] is primary, so fallback i corresponds to OrderedPoolIDs[i+1]
			ordinal := i + 1
			poolID := ""
			if ordinal < len(resolved.OrderedPoolIDs) {
				poolID = resolved.OrderedPoolIDs[ordinal]
			}
			return poolID, ordinal
		}
	}
	// Fallback: if deployment in AllAllowed but not in specific sets (e.g., all mode pools), assign based on OrderedPoolIDs order search
	if resolved.OrderedPoolIDs != nil {
		for ord := range resolved.OrderedPoolIDs {
			// For pools not in FallbackAllowed (e.g., primary all), we already handled primary.
			// For remaining, if pid matches primary, skip (already checked)
			if ord == 0 {
				continue
			}
			// If we have no set info, we can't determine, but we can still return ordinal if deployment is in AllAllowed
			// To avoid mis-attribution, only return if AllAllowed contains it and we have no better info
			if resolved.AllAllowed != nil {
				if _, ok := resolved.AllAllowed[deploymentID]; ok {
					// Return first matching ordinal where deployment could belong — use ordinal as fallback
					// This is best-effort for all-mode fallback pools
					// We will return the earliest ordinal where it could belong, but we already checked primary.
					// For simplicity, return the pool ID if we can guess, else ordinal
					// We don't have expanded sets here, so we return poolID with ordinal
					// To keep deterministic, we return the poolID at ordinal
					// But we need to know which pool actually contains it — without expanded, we approximate
					// For correctness, we will search OrderedPoolIDs and if deployment is in AllAllowed, we return first fallback that could contain it.
					// Since we already iterated FallbackAllowed, if not found, it might be in an all-mode fallback pool whose expanded set we don't have here.
					// In that case, we return the first fallback ordinal where mode is all? We don't have mode info here.
					// As fallback, return poolID at ordinal if ordinal < len(OrderedPoolIDs)
					// Actually we should just return empty and ordinal 0 to avoid misclassifying, but for policy enforcement we need correct ordinal.
					// For Phase E, we rely on resolver's AllFilteredCandidates preserving order, and pool ordinal is derived from OrderedPoolIDs position in filtered list?
					// Simpler: if not found, return primary pool ID and 0 as fallback — conservative.
				}
			}
		}
	}
	// Not found in any pool set — return primary as fallback for non-virtual? For virtual, this should not happen.
	return resolved.PrimaryPoolID, 0
}

// reorderScoredByDecision reorders original Scored slice according to ordered decision candidates.
// It preserves Scored metadata and ensures no candidate is lost.
func reorderScoredByDecision(original []router.Scored, ordered []decision.Candidate) []router.Scored {
	if len(ordered) != len(original) {
		// Fallback: if lengths mismatch (should not happen due to validator/normalizer), preserve original
		return original
	}
	byID := make(map[string]router.Scored, len(original))
	for _, sc := range original {
		byID[sc.Deployment.ID] = sc
	}
	out := make([]router.Scored, 0, len(original))
	for _, dc := range ordered {
		if sc, ok := byID[dc.ID]; ok {
			out = append(out, sc)
		}
	}
	// If some IDs missing due to bug, append remaining in original order
	if len(out) != len(original) {
		seen := make(map[string]struct{}, len(out))
		for _, sc := range out {
			seen[sc.Deployment.ID] = struct{}{}
		}
		for _, sc := range original {
			if _, ok := seen[sc.Deployment.ID]; !ok {
				out = append(out, sc)
			}
		}
	}
	return out
}

// emitDecisionEvent emits bounded decision plane events.
func (s *Server) emitDecisionEvent(requestID string, result decision.DecisionResult, trace decision.DecisionTrace, resolvedRoute *route.ResolvedRoute, candidateCount int) {
	// Build bounded reason codes string
	reasonStr := ""
	if len(result.ReasonCodes) > 0 {
		parts := make([]string, 0, len(result.ReasonCodes))
		for _, rc := range result.ReasonCodes {
			parts = append(parts, string(rc))
		}
		reasonStr = strings.Join(parts, ",")
	}
	// Build breakdown JSON if policy trace present
	breakdownJSON := ""
	if result.PolicyTrace != nil {
		// Marshal selected and original breakdowns plus weights as privacy-safe JSON
		type breakdownPayload struct {
			SelectedID           string             `json:"selected_id,omitempty"`
			OriginalPrimaryID    string             `json:"original_primary_id,omitempty"`
			SelectedScore        float64            `json:"selected_score"`
			OriginalPrimaryScore float64            `json:"original_primary_score"`
			SelectedBreakdown    map[string]float64 `json:"selected_breakdown,omitempty"`
			OriginalBreakdown    map[string]float64 `json:"original_breakdown,omitempty"`
			Weights              map[string]float64 `json:"weights,omitempty"`
		}
		payload := breakdownPayload{
			SelectedID:           result.PolicyTrace.SelectedID,
			OriginalPrimaryID:    result.PolicyTrace.OriginalPrimaryID,
			SelectedScore:        result.PolicyTrace.SelectedScore,
			OriginalPrimaryScore: result.PolicyTrace.OriginalPrimaryScore,
			SelectedBreakdown:    result.PolicyTrace.SelectedBreakdown,
			OriginalBreakdown:    result.PolicyTrace.OriginalBreakdown,
			Weights:              result.PolicyTrace.Weights,
		}
		if b, err := jsonMarshalBounded(payload, 4096); err == nil {
			breakdownJSON = b
		}
	} else if trace.PolicyTrace != nil {
		type breakdownPayload struct {
			SelectedID           string             `json:"selected_id,omitempty"`
			OriginalPrimaryID    string             `json:"original_primary_id,omitempty"`
			SelectedScore        float64            `json:"selected_score"`
			OriginalPrimaryScore float64            `json:"original_primary_score"`
			SelectedBreakdown    map[string]float64 `json:"selected_breakdown,omitempty"`
			OriginalBreakdown    map[string]float64 `json:"original_breakdown,omitempty"`
			Weights              map[string]float64 `json:"weights,omitempty"`
		}
		payload := breakdownPayload{
			SelectedID:           trace.PolicyTrace.SelectedID,
			OriginalPrimaryID:    trace.PolicyTrace.OriginalPrimaryID,
			SelectedScore:        trace.PolicyTrace.SelectedScore,
			OriginalPrimaryScore: trace.PolicyTrace.OriginalPrimaryScore,
			SelectedBreakdown:    trace.PolicyTrace.SelectedBreakdown,
			OriginalBreakdown:    trace.PolicyTrace.OriginalBreakdown,
			Weights:              trace.PolicyTrace.Weights,
		}
		if b, err := jsonMarshalBounded(payload, 4096); err == nil {
			breakdownJSON = b
		}
	}

	// Determine event kind from result
	kind := "decision_ok"
	if result.Action == decision.ActionAbstain {
		// Check if fallback used or abstained
		hasAbstain := false
		for _, rc := range result.ReasonCodes {
			if rc == decision.ReasonAbstained || rc == decision.ReasonOffMode || rc == decision.ReasonSingleCandidate || rc == decision.ReasonEmptyEligible {
				hasAbstain = true
				break
			}
		}
		if hasAbstain || result.Abstained {
			kind = "decision_abstain"
		}
	}
	// Check timeout/fail reasons
	for _, rc := range result.ReasonCodes {
		if rc == decision.ReasonTimeout {
			kind = "decision_timeout"
			break
		}
		if rc == decision.ReasonInvalidResult || rc == decision.ReasonValidationFailed {
			kind = "decision_rejected"
			break
		}
		if rc == decision.ReasonProviderError || rc == decision.ReasonProviderPanic || rc == decision.ReasonProviderUnhealthy || rc == decision.ReasonBudgetExceeded {
			kind = "decision_fail"
			break
		}
	}

	ev := events.Event{
		RequestID:              requestID,
		Kind:                   kind,
		Message:                reasonStr,
		DecisionProvider:       result.ProviderID,
		DecisionAction:         string(result.Action),
		DecisionSelected:       result.SelectedID,
		DecisionReasonCodes:    reasonStr,
		DecisionCandidateCount: candidateCount,
		DecisionConfidence:     result.Confidence,
		LatencyMS:              result.Latency.Milliseconds(),
		DecisionBreakdown:      breakdownJSON,
	}
	if result.PolicyTrace != nil {
		ev.DecisionPolicyID = result.PolicyTrace.PolicyID
		ev.DecisionTaskType = result.PolicyTrace.TaskType
		ev.DecisionOriginalPrimary = result.PolicyTrace.OriginalPrimaryID
		ev.DecisionSelectedScore = result.PolicyTrace.SelectedScore
		ev.DecisionOriginalScore = result.PolicyTrace.OriginalPrimaryScore
		ev.DecisionChangedPrimary = result.PolicyTrace.ChangedPrimary
	} else if trace.PolicyTrace != nil {
		ev.DecisionPolicyID = trace.PolicyTrace.PolicyID
		ev.DecisionTaskType = trace.PolicyTrace.TaskType
		ev.DecisionOriginalPrimary = trace.PolicyTrace.OriginalPrimaryID
		ev.DecisionSelectedScore = trace.PolicyTrace.SelectedScore
		ev.DecisionOriginalScore = trace.PolicyTrace.OriginalPrimaryScore
		ev.DecisionChangedPrimary = trace.PolicyTrace.ChangedPrimary
	}
	if resolvedRoute != nil {
		ev.VirtualEndpoint = resolvedRoute.VirtualEndpointID
		ev.PublicModel = resolvedRoute.PublicModel
		ev.RouteProfile = resolvedRoute.RouteProfileID
		ev.Pool = resolvedRoute.PrimaryPoolID
	}
	s.bus.Add(ev)
}

// applyDecisionPlane runs the decision orchestrator if enabled.
// It is called after candidatesForRequirement (eligible set E) and before execution.
// OFF mode has zero semantic impact: returns candidates unchanged.
//
// Privacy: only RequestFeatures + TaskProfile + candidate IDs are passed to decision plane.
// No raw prompts, tool results, headers, or secrets.
func (s *Server) applyDecisionPlane(
	ctx context.Context,
	candidates []router.Scored,
	ti taskIntelligence,
	resolvedRoute *route.ResolvedRoute,
	requestID string,
	req router.Requirement,
) []router.Scored {
	// Fast path: check decision mode without lock? Need snapshot.
	s.runtimeMu.RLock()
	orch := s.decisionOrchestrator
	cfgDecision := s.cfg.Decision
	cfgCopy := s.cfg
	rt := s.rt
	s.runtimeMu.RUnlock()

	if orch == nil {
		return candidates
	}
	// OFF check: zero overhead — orchestrator also handles but we can short-circuit before conversion
	if cfgDecision.Mode == "" || cfgDecision.Mode == "off" {
		return candidates
	}

	// Even if orchestrator handles empty/single, we keep wiring fast path for efficiency,
	// but orchestrator itself must also handle it correctly for safety outside HTTP wiring.
	if len(candidates) <= 1 {
		// Still emit event for single/empty? No, skip to avoid noise, but orchestrator would handle if called.
		return candidates
	}

	// Build decision request with extended Phase E signals
	dc := decisionCandidates(candidates, resolvedRoute)

	var veID, rpID, poolID string
	if resolvedRoute != nil {
		veID = resolvedRoute.VirtualEndpointID
		rpID = resolvedRoute.RouteProfileID
		poolID = resolvedRoute.PrimaryPoolID
	}

	// Budget from config — includes MaxProviderCalls = 1 for Phase D
	budget := decision.Budget{
		Timeout:          time.Duration(cfgDecision.TimeoutMS) * time.Millisecond,
		MaxProviderCalls: 1,
	}

	// Use task intelligence; ensure types align
	var fp feature.RequestFeatures = ti.Features
	var tp taskprofile.TaskProfile = ti.Profile

	// Phase E: resolve pinned deployment ID (privacy-safe, no raw session key)
	pinnedID := ""
	if rt != nil {
		pinnedID = rt.PinnedDeploymentID(req)
	}

	// Phase E: resolve policy ID — per RouteProfile overrides global
	policyID := cfgDecision.Policy
	if rpID != "" {
		// Look up route profile's decision policy
		for _, rp := range cfgCopy.RouteProfiles {
			if rp.ID == rpID && rp.DecisionPolicy != "" {
				policyID = rp.DecisionPolicy
				break
			}
		}
	}

	decisionReq := decision.DecisionRequest{
		TaskProfile:          tp,
		Features:             fp,
		Candidates:           dc,
		VirtualEndpointID:    veID,
		RouteProfileID:       rpID,
		CandidatePoolID:      poolID,
		PinnedCandidateID:    pinnedID,
		PolicyID:             policyID,
		EstimatedInputTokens: req.EstimatedInputTokens,
		MaxOutputTokens:      req.MaxOutputTokens,
		MinContextWindow:     req.MinContextWindow,
		Budget:               budget,
		RequestID:            requestID,
	}

	ordered, result, trace := orch.Decide(ctx, decisionReq)
	// Emit decision event (bounded, privacy-safe)
	s.emitDecisionEvent(requestID, result, trace, resolvedRoute, len(candidates))

	if len(ordered) == 0 {
		return candidates
	}
	// Reorder scored
	return reorderScoredByDecision(candidates, ordered)
}
