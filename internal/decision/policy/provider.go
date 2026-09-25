package policy

import (
	"context"
	"math"
	"sort"
	"sync"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

// Provider implements decision.DecisionProvider for deterministic policy engine.
// ID is "policy", CanSelect true, CanRank false, healthy.
type Provider struct {
	mu       sync.RWMutex
	policies map[string]Policy
	// default policy ID for global fallback
	defaultPolicyID string
}

func NewProvider(policies []Policy, defaultPolicyID string) *Provider {
	m := make(map[string]Policy, len(policies))
	for _, p := range policies {
		m[p.ID] = p
	}
	return &Provider{
		policies:        m,
		defaultPolicyID: defaultPolicyID,
	}
}

// UpdatePolicies hot-reloads policies atomically.
func (p *Provider) UpdatePolicies(policies []Policy, defaultPolicyID string) {
	m := make(map[string]Policy, len(policies))
	for _, pol := range policies {
		m[pol.ID] = pol
	}
	p.mu.Lock()
	p.policies = m
	p.defaultPolicyID = defaultPolicyID
	p.mu.Unlock()
}

func (p *Provider) ID() string {
	return "policy"
}

func (p *Provider) Capabilities() decision.Capabilities {
	return decision.Capabilities{
		CanSelect: true,
		CanRank:   false,
	}
}

func (p *Provider) Health() decision.ProviderHealth {
	return decision.ProviderHealth{
		Status: decision.HealthHealthy,
	}
}

// resolvePolicy returns policy for request, using request PolicyID or default.
// Returns nil if not found.
func (p *Provider) resolvePolicy(req decision.DecisionRequest) *Policy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	// Try request policy ID first
	if req.PolicyID != "" {
		if pol, ok := p.policies[req.PolicyID]; ok {
			return &pol
		}
	}
	// Fallback to default
	if p.defaultPolicyID != "" {
		if pol, ok := p.policies[p.defaultPolicyID]; ok {
			return &pol
		}
	}
	// If only one policy exists, use it as implicit default
	if len(p.policies) == 1 {
		for _, pol := range p.policies {
			// copy to avoid referencing loop variable
			c := pol
			return &c
		}
	}
	return nil
}

func (p *Provider) Decide(ctx context.Context, req decision.DecisionRequest) (decision.DecisionResult, error) {
	// Respect ctx cancellation contract
	select {
	case <-ctx.Done():
		return decision.DecisionResult{
			Action:      decision.ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []decision.ReasonCode{decision.ReasonAbstained, decision.ReasonExistingOrderPreserved},
			ProviderID:  p.ID(),
		}, ctx.Err()
	default:
	}

	candidates := req.Candidates
	if len(candidates) == 0 {
		return decision.DecisionResult{
			Action:      decision.ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []decision.ReasonCode{decision.ReasonEmptyEligible, decision.ReasonExistingOrderPreserved},
			ProviderID:  p.ID(),
		}, nil
	}

	// Resolve policy
	pol := p.resolvePolicy(req)
	if pol == nil {
		// No policy configured → abstain, preserve order
		return decision.DecisionResult{
			Action:      decision.ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []decision.ReasonCode{decision.ReasonAbstained, decision.ReasonExistingOrderPreserved},
			ProviderID:  p.ID(),
		}, nil
	}

	// Enforce selection band: earliest PoolOrdinal only
	minOrdinal := candidates[0].PoolOrdinal
	for _, c := range candidates[1:] {
		if c.PoolOrdinal < minOrdinal {
			minOrdinal = c.PoolOrdinal
		}
	}
	band := make([]decision.Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.PoolOrdinal == minOrdinal {
			band = append(band, c)
		}
	}
	reasonCodes := []decision.ReasonCode{}
	poolEnforced := len(band) < len(candidates)
	if poolEnforced {
		reasonCodes = append(reasonCodes, decision.ReasonPoolBoundaryEnforced)
	}

	// Minimum Priority tier only (hard guardrail)
	if len(band) > 0 {
		minPriority := band[0].Priority
		for _, c := range band[1:] {
			if c.Priority < minPriority {
				minPriority = c.Priority
			}
		}
		filtered := make([]decision.Candidate, 0, len(band))
		for _, c := range band {
			if c.Priority == minPriority {
				filtered = append(filtered, c)
			}
		}
		if len(filtered) < len(band) {
			reasonCodes = append(reasonCodes, decision.ReasonPriorityGuardrail)
		}
		band = filtered
	}

	if len(band) == 0 {
		// Should not happen, but fallback
		band = candidates
	}

	// Affinity preserved: if pinned candidate in band, select it
	if req.PinnedCandidateID != "" {
		for _, c := range band {
			if c.ID == req.PinnedCandidateID {
				// Select pinned
				rc := append([]decision.ReasonCode{decision.ReasonAffinityPreserved, decision.ReasonPolicySelectFirst}, reasonCodes...)
				// Ensure bounded
				if len(rc) > decision.MaxReasonCodes {
					rc = rc[:decision.MaxReasonCodes]
				}
				// Include policy scored for observability
				hasScored := false
				for _, r := range rc {
					if r == decision.ReasonPolicyScored {
						hasScored = true
						break
					}
				}
				if !hasScored {
					rc = append(rc, decision.ReasonPolicyScored)
				}
				return decision.DecisionResult{
					Action:      decision.ActionSelect,
					SelectedID:  c.ID,
					Confidence:  1.0,
					ReasonCodes: rc,
					ProviderID:  p.ID(),
				}, nil
			}
		}
	}

	// If only one candidate in band, select it directly
	if len(band) == 1 {
		rc := []decision.ReasonCode{decision.ReasonPolicyScored, decision.ReasonPolicySelectFirst}
		rc = append(rc, reasonCodes...)
		// Deduplicate and bound
		rc = dedupReasonCodes(rc)
		return decision.DecisionResult{
			Action:      decision.ActionSelect,
			SelectedID:  band[0].ID,
			Confidence:  1.0,
			ReasonCodes: rc,
			ProviderID:  p.ID(),
		}, nil
	}

	// Resolve task-aware weights
	taskType := ""
	if req.TaskProfile.Type != "" {
		taskType = string(req.TaskProfile.Type)
	}
	effectiveWeights, taskAware := pol.ResolveWeights(taskType)

	// Score candidates
	scored := ScoreCandidates(band)
	scored = ApplyWeights(scored, effectiveWeights)

	// Sort by weighted score desc, then OriginalRank asc for determinism (preserve router order on tie)
	sort.SliceStable(scored, func(i, j int) bool {
		if math.Abs(scored[i].WeightedScore-scored[j].WeightedScore) > 1e-9 {
			return scored[i].WeightedScore > scored[j].WeightedScore
		}
		// Tie: preserve original router order
		return scored[i].OriginalRank < scored[j].OriginalRank
	})

	// Check min_score_delta
	if len(scored) >= 2 {
		top := scored[0].WeightedScore
		second := scored[1].WeightedScore
		delta := top - second
		if delta < 0 {
			delta = -delta
		}
		if delta < pol.MinScoreDelta {
			// Abstain, preserve order
			rc := []decision.ReasonCode{decision.ReasonMinDeltaNotMet, decision.ReasonPolicyScored, decision.ReasonExistingOrderPreserved}
			rc = append(rc, reasonCodes...)
			if taskAware {
				rc = append(rc, decision.ReasonTaskAwareWeights)
			}
			rc = dedupReasonCodes(rc)
			return decision.DecisionResult{
				Action:      decision.ActionAbstain,
				Abstained:   true,
				Confidence:  0,
				ReasonCodes: rc,
				ProviderID:  p.ID(),
			}, nil
		}
	}

	// Select top
	topCandidate := scored[0]
	rc := []decision.ReasonCode{decision.ReasonPolicyScored, decision.ReasonPolicySelectFirst}
	rc = append(rc, reasonCodes...)
	if taskAware {
		rc = append(rc, decision.ReasonTaskAwareWeights)
	}
	rc = dedupReasonCodes(rc)

	conf := topCandidate.WeightedScore
	if math.IsNaN(conf) || math.IsInf(conf, 0) {
		conf = 0.5
	}
	if conf < 0 {
		conf = 0
	}
	if conf > 1 {
		conf = 1
	}

	return decision.DecisionResult{
		Action:      decision.ActionSelect,
		SelectedID:  topCandidate.Candidate.ID,
		Confidence:  conf,
		ReasonCodes: rc,
		ProviderID:  p.ID(),
	}, nil
}

func dedupReasonCodes(in []decision.ReasonCode) []decision.ReasonCode {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[decision.ReasonCode]struct{}, len(in))
	out := make([]decision.ReasonCode, 0, len(in))
	for _, rc := range in {
		if _, ok := seen[rc]; ok {
			continue
		}
		seen[rc] = struct{}{}
		out = append(out, rc)
		if len(out) >= decision.MaxReasonCodes {
			break
		}
	}
	return out
}
