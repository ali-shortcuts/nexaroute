package decision

import (
	"fmt"
	"math"
)

// Validation errors are fail-open: orchestrator treats invalid results as abstain.
var (
	ErrUnknownCandidate  = fmt.Errorf("decision contains unknown candidate")
	ErrDuplicateID       = fmt.Errorf("decision contains duplicate candidate ID")
	ErrInvalidConfidence = fmt.Errorf("decision confidence out of bounds")
	ErrInvalidAction     = fmt.Errorf("decision action invalid")
	ErrEmptyEligible     = fmt.Errorf("eligible set empty")
	ErrInvalidResult     = fmt.Errorf("decision result invalid")
	ErrTooManyRanked     = fmt.Errorf("ranked_ids exceeds eligible size")
	ErrReasonCodeInvalid = fmt.Errorf("reason code invalid")
	ErrReasonCodeTooMany = fmt.Errorf("too many reason codes")
)

// ValidateResult enforces strict contract:
// - eligible may be empty (handled by orchestrator as EMPTY_ELIGIBLE, not error for provider? But for direct validation we return ErrEmptyEligible)
// - Action must be known non-empty (SELECT, RANK, ABSTAIN) — empty/unknown is INVALID
// - Confidence must be finite, not NaN/Inf, in [0,1]
// - ReasonCodes bounded: count <= MaxReasonCodes, each len <= MaxReasonCodeLen, each must be canonical
// - SelectedID len bounded, ProviderID len bounded
// - SELECT: selected_id required, must be in eligible, ranked_ids must be empty
// - RANK: ranked_ids required non-empty, every ID in eligible, no duplicates, len <= len(eligible) and <= MaxRankedIDs, selected_id must be empty
// - ABSTAIN: selected_id empty, ranked_ids empty
func ValidateResult(eligible []Candidate, result DecisionResult) error {
	if len(eligible) == 0 {
		return ErrEmptyEligible
	}
	// Action must be known non-empty
	if !result.ValidAction() {
		return fmt.Errorf("%w: %q", ErrInvalidAction, result.Action)
	}
	// Confidence must be finite and in [0,1], reject NaN/Inf
	if math.IsNaN(result.Confidence) || math.IsInf(result.Confidence, 0) {
		return fmt.Errorf("%w: NaN or Inf", ErrInvalidConfidence)
	}
	if result.Confidence < 0 || result.Confidence > 1 {
		return fmt.Errorf("%w: %f", ErrInvalidConfidence, result.Confidence)
	}
	// Bounds for IDs
	if len(result.SelectedID) > MaxSelectedIDLen {
		return fmt.Errorf("%w: selected_id too long", ErrInvalidResult)
	}
	if len(result.ProviderID) > MaxProviderIDLen {
		return fmt.Errorf("%w: provider_id too long", ErrInvalidResult)
	}
	// Reason codes bounded and canonical
	if len(result.ReasonCodes) > MaxReasonCodes {
		return fmt.Errorf("%w: %d > %d", ErrReasonCodeTooMany, len(result.ReasonCodes), MaxReasonCodes)
	}
	for _, rc := range result.ReasonCodes {
		if len(string(rc)) > MaxReasonCodeLen {
			return fmt.Errorf("%w: reason code too long: %q", ErrReasonCodeInvalid, rc)
		}
		if !IsValidReasonCode(rc) {
			return fmt.Errorf("%w: %q", ErrReasonCodeInvalid, rc)
		}
	}
	// Ranked IDs overall bound
	if len(result.RankedIDs) > MaxRankedIDs {
		return fmt.Errorf("%w: ranked_ids %d > hard limit %d", ErrTooManyRanked, len(result.RankedIDs), MaxRankedIDs)
	}
	if len(result.RankedIDs) > len(eligible) {
		return fmt.Errorf("%w: %d > eligible %d", ErrTooManyRanked, len(result.RankedIDs), len(eligible))
	}

	// Build eligible set
	eligibleSet := make(map[string]struct{}, len(eligible))
	for _, c := range eligible {
		eligibleSet[c.ID] = struct{}{}
	}

	switch result.Action {
	case ActionSelect:
		if result.SelectedID == "" {
			return fmt.Errorf("%w: SELECT requires selected_id", ErrInvalidResult)
		}
		if len(result.RankedIDs) != 0 {
			return fmt.Errorf("%w: SELECT must have empty ranked_ids", ErrInvalidResult)
		}
		if _, ok := eligibleSet[result.SelectedID]; !ok {
			return fmt.Errorf("%w: %s", ErrUnknownCandidate, result.SelectedID)
		}
	case ActionRank:
		if result.SelectedID != "" {
			return fmt.Errorf("%w: RANK must have empty selected_id", ErrInvalidResult)
		}
		if len(result.RankedIDs) == 0 {
			return fmt.Errorf("%w: RANK requires non-empty ranked_ids", ErrInvalidResult)
		}
		seen := make(map[string]struct{}, len(result.RankedIDs))
		for _, id := range result.RankedIDs {
			if len(id) > MaxSelectedIDLen {
				return fmt.Errorf("%w: ranked id too long", ErrInvalidResult)
			}
			if _, ok := eligibleSet[id]; !ok {
				return fmt.Errorf("%w: %s", ErrUnknownCandidate, id)
			}
			if _, dup := seen[id]; dup {
				return fmt.Errorf("%w: %s", ErrDuplicateID, id)
			}
			seen[id] = struct{}{}
		}
	case ActionAbstain:
		if result.SelectedID != "" {
			return fmt.Errorf("%w: ABSTAIN must have empty selected_id", ErrInvalidResult)
		}
		if len(result.RankedIDs) != 0 {
			return fmt.Errorf("%w: ABSTAIN must have empty ranked_ids", ErrInvalidResult)
		}
	default:
		return fmt.Errorf("%w: %q", ErrInvalidAction, result.Action)
	}

	return nil
}

// NormalizeResult takes a validated result and produces a final ordered list
// that preserves the eligible-set invariant and failover coverage.
//
// Rules (Phase D):
// - If abstain: return original order unchanged.
// - If SELECT: [selected] + rest of original order (excluding selected) preserving original order.
// - If RANK: ranked IDs first in provider order, then any omitted eligible IDs appended in original order.
// This ensures no eligible candidate is dropped from failover.
func NormalizeResult(eligible []Candidate, result DecisionResult) (ordered []Candidate, applied bool, reason ReasonCode) {
	if len(eligible) == 0 {
		return nil, false, ReasonEmptyEligible
	}
	// Fast path abstain
	if result.Action == ActionAbstain {
		return CloneCandidates(eligible), false, ReasonAbstained
	}

	eligibleByID := make(map[string]Candidate, len(eligible))
	for _, c := range eligible {
		eligibleByID[c.ID] = c
	}

	switch result.Action {
	case ActionSelect:
		selID := result.SelectedID
		if selID == "" {
			// Should have been caught by validator; fail-open
			return CloneCandidates(eligible), false, ReasonInvalidResult
		}
		sel, ok := eligibleByID[selID]
		if !ok {
			// Should have been caught by validator; fail-open
			return CloneCandidates(eligible), false, ReasonInvalidResult
		}
		out := make([]Candidate, 0, len(eligible))
		out = append(out, sel)
		for _, c := range eligible {
			if c.ID == selID {
				continue
			}
			out = append(out, c)
		}
		return out, true, ReasonEligibleSetPreserved

	case ActionRank:
		if len(result.RankedIDs) == 0 {
			return CloneCandidates(eligible), false, ReasonAbstained
		}
		out := make([]Candidate, 0, len(eligible))
		seen := make(map[string]struct{}, len(result.RankedIDs))
		for _, id := range result.RankedIDs {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			if c, ok := eligibleByID[id]; ok {
				out = append(out, c)
			}
		}
		// Append omitted in original order
		omitted := 0
		for _, c := range eligible {
			if _, ok := seen[c.ID]; !ok {
				out = append(out, c)
				omitted++
			}
		}
		if omitted > 0 {
			return out, true, ReasonNormalizationApplied
		}
		return out, true, ReasonEligibleSetPreserved

	default:
		// Should not happen after validation, but treat as abstain
		return CloneCandidates(eligible), false, ReasonAbstained
	}
}
