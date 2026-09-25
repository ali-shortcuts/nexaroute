package decision

// PrimarySelectionConstraints is the permitted primary band derived from the
// eligible set E. It is computed by shared NexaRoute code — not by any
// provider — so external intelligence can never bypass it.
type PrimarySelectionConstraints struct {
	// AllowedPrimaryIDs is the ordered band a SELECT must come from.
	AllowedPrimaryIDs []string
	// ForcedPrimaryID is the eligible session pin when one exists. The
	// orchestrator short-circuits (no provider call) in that case.
	ForcedPrimaryID string
	// ReasonCodes explains the applied guardrails with canonical codes.
	ReasonCodes []ReasonCode
}

// ComputeConstraints derives the permitted primary band from the full
// eligible set (in router order) and the session pin ("" when none):
//
//	A. The earliest (minimum) PoolOrdinal is mandatory: only that pool may
//	   supply the primary.
//	B. An eligible session pin is authoritative: when the pin belongs to E,
//	   it is forced and no provider is consulted. The Phase F brief requires
//	   this at minimum for in-pool pins; NexaRoute conservatively extends it
//	   to any eligible pin, because skipping an external call is always safe
//	   and affinity must never be displaced by remote intelligence.
//	C. Otherwise only the minimum Priority tier within the earliest pool may
//	   compete.
//
// The full candidate list is untouched: fallback candidates are never
// deleted, and eligibility is never changed. Only the primary band narrows.
func ComputeConstraints(candidates []Candidate, sessionPin string) PrimarySelectionConstraints {
	if len(candidates) == 0 {
		return PrimarySelectionConstraints{}
	}
	inE := make(map[string]struct{}, len(candidates))
	for _, c := range candidates {
		if c.ID != "" {
			inE[c.ID] = struct{}{}
		}
	}
	// Rule B: eligible affinity pin is authoritative.
	if sessionPin != "" {
		if _, ok := inE[sessionPin]; ok {
			return PrimarySelectionConstraints{
				AllowedPrimaryIDs: []string{sessionPin},
				ForcedPrimaryID:   sessionPin,
				ReasonCodes:       []ReasonCode{ReasonAffinityPreserved},
			}
		}
	}
	// Rule A: earliest pool is mandatory.
	minPool := candidates[0].PoolOrdinal
	for _, c := range candidates[1:] {
		if c.PoolOrdinal < minPool {
			minPool = c.PoolOrdinal
		}
	}
	pool := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		if c.PoolOrdinal == minPool {
			pool = append(pool, c)
		}
	}
	// Rule C: only the minimum priority tier within that pool competes.
	minPriority := pool[0].Priority
	for _, c := range pool[1:] {
		if c.Priority < minPriority {
			minPriority = c.Priority
		}
	}
	allowed := make([]string, 0, len(pool))
	for _, c := range pool {
		if c.Priority == minPriority && c.ID != "" {
			allowed = append(allowed, c.ID)
		}
	}
	return PrimarySelectionConstraints{AllowedPrimaryIDs: allowed}
}
