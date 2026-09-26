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
	mu              sync.RWMutex
	policies        map[string]Policy
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
// Returns nil if not found. Explicit config required — no implicit single-policy magic.
func (p *Provider) resolvePolicy(req decision.DecisionRequest) *Policy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	// Try request policy ID first
	if req.PolicyID != "" {
		if pol, ok := p.policies[req.PolicyID]; ok {
			// copy to avoid referencing loop variable
			c := pol
			return &c
		}
	}
	// Fallback to default
	if p.defaultPolicyID != "" {
		if pol, ok := p.policies[p.defaultPolicyID]; ok {
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

	// Resolve policy — explicit required, no silent no-op magic
	pol := p.resolvePolicy(req)
	if pol == nil {
		return decision.DecisionResult{
			Action:      decision.ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: []decision.ReasonCode{decision.ReasonAbstained, decision.ReasonExistingOrderPreserved},
			ProviderID:  p.ID(),
		}, nil
	}

	// Enforce selection band: earliest PoolOrdinal only (fallback hard boundary)
	minOrdinal := candidates[0].PoolOrdinal
	for _, c := range candidates[1:] {
		if c.PoolOrdinal < minOrdinal {
			minOrdinal = c.PoolOrdinal
		}
	}
	bandPool := make([]decision.Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.PoolOrdinal == minOrdinal {
			bandPool = append(bandPool, c)
		}
	}
	reasonCodes := []decision.ReasonCode{}
	if len(bandPool) < len(candidates) {
		reasonCodes = append(reasonCodes, decision.ReasonPoolBoundaryEnforced)
	}
	if len(bandPool) == 0 {
		bandPool = candidates
	}

	// Affinity preserved BEFORE priority: if pinned eligible inside earliest permitted pool, preserve it
	if req.PinnedCandidateID != "" {
		for _, c := range bandPool {
			if c.ID == req.PinnedCandidateID {
				rc := append([]decision.ReasonCode{decision.ReasonAffinityPreserved, decision.ReasonPolicySelectFirst}, reasonCodes...)
				if len(rc) > decision.MaxReasonCodes {
					rc = rc[:decision.MaxReasonCodes]
				}
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
				// Policy trace for affinity case
				pt := &decision.PolicyTrace{
					PolicyID:             pol.ID,
					TaskType:             string(req.TaskProfile.Type),
					OriginalPrimaryID:    originalPrimaryID(bandPool),
					SelectedID:           c.ID,
					ChangedPrimary:       c.ID != originalPrimaryID(bandPool),
					SelectedScore:        1.0,
					OriginalPrimaryScore: 1.0,
				}
				return decision.DecisionResult{
					Action:      decision.ActionSelect,
					SelectedID:  c.ID,
					Confidence:  1.0,
					ReasonCodes: rc,
					ProviderID:  p.ID(),
					PolicyTrace: pt,
				}, nil
			}
		}
	}

	// Minimum Priority tier only (hard guardrail) — after affinity check
	band := bandPool
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
		band = bandPool
		if len(band) == 0 {
			band = candidates
		}
	}

	// If only one candidate in band, select it directly
	if len(band) == 1 {
		rc := []decision.ReasonCode{decision.ReasonPolicyScored, decision.ReasonPolicySelectFirst}
		rc = append(rc, reasonCodes...)
		rc = dedupReasonCodes(rc)
		origID := originalPrimaryID(band)
		pt := &decision.PolicyTrace{
			PolicyID:             pol.ID,
			TaskType:             string(req.TaskProfile.Type),
			OriginalPrimaryID:    origID,
			SelectedID:           band[0].ID,
			ChangedPrimary:       false,
			SelectedScore:        1.0,
			OriginalPrimaryScore: 1.0,
		}
		return decision.DecisionResult{
			Action:      decision.ActionSelect,
			SelectedID:  band[0].ID,
			Confidence:  1.0,
			ReasonCodes: rc,
			ProviderID:  p.ID(),
			PolicyTrace: pt,
		}, nil
	}

	// Resolve task-aware weights
	taskType := ""
	if req.TaskProfile.Type != "" {
		taskType = string(req.TaskProfile.Type)
	}
	effectiveWeights, taskAware := pol.ResolveWeights(taskType)

	// Determine required context for headroom scoring
	required := req.MinContextWindow
	if required <= 0 {
		// Fallback to estimated tokens if MinContextWindow not set
		est := req.EstimatedInputTokens + req.MaxOutputTokens
		if est > 0 {
			required = est
		}
	}

	// Score candidates with request-relative context
	scored := ScoreCandidates(band, required)
	scored = ApplyWeights(scored, effectiveWeights)

	// Sort by weighted score desc, then OriginalRank asc for determinism
	sort.SliceStable(scored, func(i, j int) bool {
		if math.Abs(scored[i].WeightedScore-scored[j].WeightedScore) > 1e-9 {
			return scored[i].WeightedScore > scored[j].WeightedScore
		}
		return scored[i].OriginalRank < scored[j].OriginalRank
	})

	// Find original primary: earliest OriginalRank in band
	origPrimaryID := originalPrimaryID(band)
	var origPrimaryScore float64
	var origPrimaryFound bool
	var origBreakdown map[string]float64
	for _, sc := range scored {
		if sc.Candidate.ID == origPrimaryID {
			origPrimaryScore = sc.WeightedScore
			origPrimaryFound = true
			origBreakdown = sc.Components
			break
		}
	}
	if !origPrimaryFound && len(scored) > 0 {
		// Fallback: if original primary not in scored (should not happen), use first by OriginalRank
		origPrimaryID = band[0].ID
		for _, c := range band[1:] {
			if c.OriginalRank < band[0].OriginalRank {
				// Actually need to find min OriginalRank
			}
		}
		// Find min OriginalRank
		minRank := band[0].OriginalRank
		origPrimaryID = band[0].ID
		for _, c := range band[1:] {
			if c.OriginalRank < minRank {
				minRank = c.OriginalRank
				origPrimaryID = c.ID
			}
		}
		// Try again
		for _, sc := range scored {
			if sc.Candidate.ID == origPrimaryID {
				origPrimaryScore = sc.WeightedScore
				origBreakdown = sc.Components
				break
			}
		}
	}

	topCandidate := scored[0]
	// If best already IS original primary -> preserve order, abstain is preferable to avoid churn
	if topCandidate.Candidate.ID == origPrimaryID {
		rc := []decision.ReasonCode{decision.ReasonExistingOrderPreserved, decision.ReasonPolicyScored}
		rc = append(rc, reasonCodes...)
		if taskAware {
			rc = append(rc, decision.ReasonTaskAwareWeights)
		}
		rc = dedupReasonCodes(rc)
		pt := &decision.PolicyTrace{
			PolicyID:             pol.ID,
			TaskType:             taskType,
			OriginalPrimaryID:    origPrimaryID,
			SelectedID:           origPrimaryID,
			ChangedPrimary:       false,
			SelectedScore:        topCandidate.WeightedScore,
			OriginalPrimaryScore: origPrimaryScore,
			SelectedBreakdown:    topCandidate.Components,
			OriginalBreakdown:    origBreakdown,
			Weights:              weightsToMap(effectiveWeights),
		}
		return decision.DecisionResult{
			Action:      decision.ActionAbstain,
			Abstained:   true,
			Confidence:  0,
			ReasonCodes: rc,
			ProviderID:  p.ID(),
			PolicyTrace: pt,
		}, nil
	}

	// Check min_score_delta against ORIGINAL PRIMARY, not second-best
	if pol.MinScoreDelta > 0 {
		delta := topCandidate.WeightedScore - origPrimaryScore
		if delta < 0 {
			delta = -delta
		}
		// Actually we want best beats original by at least delta
		improvement := topCandidate.WeightedScore - origPrimaryScore
		if improvement < pol.MinScoreDelta {
			rc := []decision.ReasonCode{decision.ReasonMinDeltaNotMet, decision.ReasonPolicyScored, decision.ReasonExistingOrderPreserved}
			rc = append(rc, reasonCodes...)
			if taskAware {
				rc = append(rc, decision.ReasonTaskAwareWeights)
			}
			rc = dedupReasonCodes(rc)
			pt := &decision.PolicyTrace{
				PolicyID:             pol.ID,
				TaskType:             taskType,
				OriginalPrimaryID:    origPrimaryID,
				SelectedID:           topCandidate.Candidate.ID,
				ChangedPrimary:       false,
				SelectedScore:        topCandidate.WeightedScore,
				OriginalPrimaryScore: origPrimaryScore,
				SelectedBreakdown:    topCandidate.Components,
				OriginalBreakdown:    origBreakdown,
				Weights:              weightsToMap(effectiveWeights),
			}
			return decision.DecisionResult{
				Action:      decision.ActionAbstain,
				Abstained:   true,
				Confidence:  0,
				ReasonCodes: rc,
				ProviderID:  p.ID(),
				PolicyTrace: pt,
			}, nil
		}
	}

	// Select top
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

	pt := &decision.PolicyTrace{
		PolicyID:             pol.ID,
		TaskType:             taskType,
		OriginalPrimaryID:    origPrimaryID,
		SelectedID:           topCandidate.Candidate.ID,
		ChangedPrimary:       topCandidate.Candidate.ID != origPrimaryID,
		SelectedScore:        topCandidate.WeightedScore,
		OriginalPrimaryScore: origPrimaryScore,
		SelectedBreakdown:    topCandidate.Components,
		OriginalBreakdown:    origBreakdown,
		Weights:              weightsToMap(effectiveWeights),
	}

	return decision.DecisionResult{
		Action:      decision.ActionSelect,
		SelectedID:  topCandidate.Candidate.ID,
		Confidence:  conf,
		ReasonCodes: rc,
		ProviderID:  p.ID(),
		PolicyTrace: pt,
	}, nil
}

func originalPrimaryID(band []decision.Candidate) string {
	if len(band) == 0 {
		return ""
	}
	minRank := band[0].OriginalRank
	id := band[0].ID
	for _, c := range band[1:] {
		if c.OriginalRank < minRank {
			minRank = c.OriginalRank
			id = c.ID
		}
	}
	return id
}

func weightsToMap(w Weights) map[string]float64 {
	return map[string]float64{
		CompRouterBaseline: w.RouterBaseline,
		CompReliability:    w.Reliability,
		CompLatency:        w.Latency,
		CompTTFT:           w.TTFT,
		CompCapacity:       w.Capacity,
		CompCost:           w.Cost,
		CompContext:        w.Context,
	}
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
