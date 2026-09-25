package httpapi

import (
	"context"
	"strings"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/events"
	"github.com/ali-shortcuts/nexaroute/internal/feature"
	"github.com/ali-shortcuts/nexaroute/internal/route"
	"github.com/ali-shortcuts/nexaroute/internal/router"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

// decisionCandidates converts router.Scored to decision.Candidate snapshot.
func decisionCandidates(scored []router.Scored) []decision.Candidate {
	if len(scored) == 0 {
		return nil
	}
	out := make([]decision.Candidate, 0, len(scored))
	for _, s := range scored {
		out = append(out, decision.Candidate{
			ID:         s.Deployment.ID,
			ProviderID: s.Deployment.ProviderID,
			Model:      s.Deployment.Model,
			Priority:   s.Deployment.Priority,
			Weight:     s.Deployment.Weight,
		})
	}
	return out
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
) []router.Scored {
	// Fast path: check decision mode without lock? Need snapshot.
	s.runtimeMu.RLock()
	orch := s.decisionOrchestrator
	cfgDecision := s.cfg.Decision
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

	// Build decision request
	dc := decisionCandidates(candidates)

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

	req := decision.DecisionRequest{
		TaskProfile:       tp,
		Features:          fp,
		Candidates:        dc,
		VirtualEndpointID: veID,
		RouteProfileID:    rpID,
		CandidatePoolID:   poolID,
		Budget:            budget,
		RequestID:         requestID,
	}

	ordered, result, trace := orch.Decide(ctx, req)
	// Emit decision event (bounded, privacy-safe)
	s.emitDecisionEvent(requestID, result, trace, resolvedRoute, len(candidates))

	if len(ordered) == 0 {
		return candidates
	}
	// Reorder scored
	return reorderScoredByDecision(candidates, ordered)
}
