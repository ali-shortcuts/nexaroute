package decision

// PrimarySelectionConstraints represents the allowed primary band after applying
// Phase E safety guardrails: earliest pool, affinity, priority.
// This is the authoritative primary-selection safety rule that external providers must not bypass.
type PrimarySelectionConstraints struct {
	// AllowedPrimaryIDs is the set of candidate IDs that may be selected as new primary.
	// It is derived from earliest PoolOrdinal and minimum Priority tier.
	AllowedPrimaryIDs []string
	// ForcedPrimaryID is set when an eligible session pin exists inside earliest pool.
	// If non-empty, external DecisionProviders must NOT be called; pinned candidate is authoritative.
	ForcedPrimaryID string
	// ReasonCodes that explain why band was constrained (pool boundary, priority, affinity)
	ReasonCodes []ReasonCode
	// MinOrdinal is the earliest PoolOrdinal found (for debugging, not for external exposure)
	MinOrdinal int
	// MinPriority is the minimum Priority in earliest pool (for debugging)
	MinPriority int
}

// ComputePrimaryConstraints extracts the primary band from eligible candidates.
// Rules:
// A. Earliest PoolOrdinal is mandatory: minOrdinal = min(PoolOrdinal), bandPool = PoolOrdinal==minOrdinal
// B. If PinnedCandidateID exists and is inside bandPool, it is authoritative -> ForcedPrimaryID
// C. Otherwise only minimum Priority tier may compete: minPriority = min(Priority) in bandPool, band = Priority==minPriority
// Full candidate list remains intact for failover; only primary selection is constrained.
func ComputePrimaryConstraints(candidates []Candidate, pinnedID string) PrimarySelectionConstraints {
	if len(candidates) == 0 {
		return PrimarySelectionConstraints{}
	}
	// Find min ordinal
	minOrdinal := candidates[0].PoolOrdinal
	for _, c := range candidates[1:] {
		if c.PoolOrdinal < minOrdinal {
			minOrdinal = c.PoolOrdinal
		}
	}
	bandPool := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.PoolOrdinal == minOrdinal {
			bandPool = append(bandPool, c)
		}
	}
	reasonCodes := []ReasonCode{}
	if len(bandPool) < len(candidates) {
		reasonCodes = append(reasonCodes, ReasonPoolBoundaryEnforced)
	}
	if len(bandPool) == 0 {
		bandPool = candidates
	}

	// Affinity authoritative before priority, but not over pool boundary
	if pinnedID != "" {
		for _, c := range bandPool {
			if c.ID == pinnedID {
				// Forced primary
				return PrimarySelectionConstraints{
					AllowedPrimaryIDs: []string{pinnedID},
					ForcedPrimaryID:   pinnedID,
					ReasonCodes:       append([]ReasonCode{ReasonAffinityPreserved}, reasonCodes...),
					MinOrdinal:        minOrdinal,
					MinPriority:       c.Priority,
				}
			}
		}
	}

	// Minimum priority tier only
	band := bandPool
	if len(band) > 0 {
		minPriority := band[0].Priority
		for _, c := range band[1:] {
			if c.Priority < minPriority {
				minPriority = c.Priority
			}
		}
		filtered := make([]Candidate, 0, len(band))
		for _, c := range band {
			if c.Priority == minPriority {
				filtered = append(filtered, c)
			}
		}
		if len(filtered) < len(band) {
			reasonCodes = append(reasonCodes, ReasonPriorityGuardrail)
		}
		if len(filtered) > 0 {
			band = filtered
		}
		// Prepare allowed IDs
		allowed := make([]string, 0, len(band))
		for _, c := range band {
			allowed = append(allowed, c.ID)
		}
		// Determine minPriority for debugging
		mp := 0
		if len(band) > 0 {
			mp = band[0].Priority
		}
		return PrimarySelectionConstraints{
			AllowedPrimaryIDs: allowed,
			ForcedPrimaryID:   "",
			ReasonCodes:       reasonCodes,
			MinOrdinal:        minOrdinal,
			MinPriority:       mp,
		}
	}

	// Fallback: all candidates allowed (defensive)
	allowed := make([]string, 0, len(candidates))
	for _, c := range candidates {
		allowed = append(allowed, c.ID)
	}
	return PrimarySelectionConstraints{
		AllowedPrimaryIDs: allowed,
		ReasonCodes:       reasonCodes,
		MinOrdinal:        minOrdinal,
	}
}

// IsAllowedPrimary reports whether id is in allowed set
func (p PrimarySelectionConstraints) IsAllowedPrimary(id string) bool {
	for _, allowed := range p.AllowedPrimaryIDs {
		if allowed == id {
			return true
		}
	}
	return false
}
