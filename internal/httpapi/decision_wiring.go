package httpapi

import (
	"context"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
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
	if len(candidates) <= 1 {
		return candidates
	}
	// Fast path: check decision mode without lock? Need snapshot.
	s.runtimeMu.RLock()
	orch := s.decisionOrchestrator
	cfgDecision := s.cfg.Decision
	s.runtimeMu.RUnlock()

	if orch == nil {
		return candidates
	}
	// OFF check: zero overhead
	if cfgDecision.Mode == "" || cfgDecision.Mode == "off" {
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

	// Budget from config
	budget := decision.Budget{
		Timeout: time.Duration(cfgDecision.TimeoutMS) * time.Millisecond,
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

	ordered, _, _ := orch.Decide(ctx, req)
	if len(ordered) == 0 {
		return candidates
	}
	// Reorder scored
	return reorderScoredByDecision(candidates, ordered)
}
