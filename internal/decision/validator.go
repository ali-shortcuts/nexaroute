package decision

import (
	"fmt"
)

// Validation errors are fail-open: orchestrator treats invalid results as abstain.
var (
	ErrUnknownCandidate  = fmt.Errorf("decision contains unknown candidate")
	ErrDuplicateID       = fmt.Errorf("decision contains duplicate candidate ID")
	ErrInvalidConfidence = fmt.Errorf("decision confidence out of bounds")
	ErrInvalidAction     = fmt.Errorf("decision action invalid")
	ErrEmptyEligible     = fmt.Errorf("eligible set empty")
)

// ValidateResult enforces the eligible-set invariant and other contracts.
// - All IDs in result must be subset of eligible
// - No duplicates
// - Confidence in [0,1]
// - Action known
func ValidateResult(eligible []Candidate, result DecisionResult) error {
	if len(eligible) == 0 {
		return ErrEmptyEligible
	}
	if !result.ValidAction() {
		return ErrInvalidAction
	}
	if result.Confidence < 0 || result.Confidence > 1 {
		return ErrInvalidConfidence
	}
	// Build eligible set
	eligibleSet := make(map[string]struct{}, len(eligible))
	for _, c := range eligible {
		eligibleSet[c.ID] = struct{}{}
	}
	// Check SelectedID if SELECT
	if result.Action == ActionSelect {
		if result.SelectedID == "" {
			return fmt.Errorf("SELECT action requires selected_id")
		}
		if _, ok := eligibleSet[result.SelectedID]; !ok {
			return fmt.Errorf("%w: %s", ErrUnknownCandidate, result.SelectedID)
		}
	}
	// Check RankedIDs if RANK
	if result.Action == ActionRank {
		seen := make(map[string]struct{}, len(result.RankedIDs))
		for _, id := range result.RankedIDs {
			if _, ok := eligibleSet[id]; !ok {
				return fmt.Errorf("%w: %s", ErrUnknownCandidate, id)
			}
			if _, dup := seen[id]; dup {
				return fmt.Errorf("%w: %s", ErrDuplicateID, id)
			}
			seen[id] = struct{}{}
		}
	}
	// For SELECT with also RankedIDs? Allow but validate ranked too if present
	if len(result.RankedIDs) > 0 && result.Action != ActionRank {
		seen := make(map[string]struct{}, len(result.RankedIDs))
		for _, id := range result.RankedIDs {
			if _, ok := eligibleSet[id]; !ok {
				return fmt.Errorf("%w: %s", ErrUnknownCandidate, id)
			}
			if _, dup := seen[id]; dup {
				return fmt.Errorf("%w: %s", ErrDuplicateID, id)
			}
			seen[id] = struct{}{}
		}
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
func NormalizeResult(eligible []Candidate, result DecisionResult) (ordered []Candidate, applied bool, reason string) {
	if len(eligible) == 0 {
		return nil, false, ReasonAbstained
	}
	// Fast path abstain
	if result.IsAbstain() {
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
			// Treat empty select as abstain
			return CloneCandidates(eligible), false, ReasonAbstained
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
		// Empty action treated as abstain
		return CloneCandidates(eligible), false, ReasonAbstained
	}
}
