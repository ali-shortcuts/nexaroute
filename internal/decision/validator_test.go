package decision

import (
	"testing"
)

func makeEligible(ids ...string) []Candidate {
	out := make([]Candidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, Candidate{ID: id, ProviderID: "p1"})
	}
	return out
}

func TestValidateResult_ValidRank(t *testing.T) {
	eligible := makeEligible("A", "B", "C", "D")
	result := DecisionResult{
		Action:     ActionRank,
		RankedIDs:  []string{"C", "A"},
		Confidence: 0.9,
	}
	if err := ValidateResult(eligible, result); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestValidateResult_UnknownCandidate(t *testing.T) {
	eligible := makeEligible("A", "B")
	result := DecisionResult{
		Action:     ActionRank,
		RankedIDs:  []string{"A", "X"},
		Confidence: 0.5,
	}
	if err := ValidateResult(eligible, result); err == nil {
		t.Fatal("expected error for unknown candidate")
	}
}

func TestValidateResult_Duplicate(t *testing.T) {
	eligible := makeEligible("A", "B")
	result := DecisionResult{
		Action:     ActionRank,
		RankedIDs:  []string{"A", "A"},
		Confidence: 0.5,
	}
	if err := ValidateResult(eligible, result); err == nil {
		t.Fatal("expected error for duplicate")
	}
}

func TestValidateResult_InvalidConfidence(t *testing.T) {
	eligible := makeEligible("A")
	result := DecisionResult{
		Action:     ActionAbstain,
		Confidence: 1.5,
	}
	if err := ValidateResult(eligible, result); err == nil {
		t.Fatal("expected error for confidence out of bounds")
	}
}

func TestValidateResult_SelectUnknown(t *testing.T) {
	eligible := makeEligible("A", "B")
	result := DecisionResult{
		Action:     ActionSelect,
		SelectedID: "Z",
		Confidence: 0.9,
	}
	if err := ValidateResult(eligible, result); err == nil {
		t.Fatal("expected error for unknown selected")
	}
}

func TestNormalizeResult_RankPartial(t *testing.T) {
	eligible := makeEligible("A", "B", "C", "D")
	result := DecisionResult{
		Action:    ActionRank,
		RankedIDs: []string{"C", "A"},
	}
	ordered, applied, reason := NormalizeResult(eligible, result)
	if !applied {
		t.Fatal("expected applied")
	}
	if reason != ReasonNormalizationApplied {
		t.Fatalf("expected normalization reason, got %s", reason)
	}
	// Expected: [C,A,B,D]
	expected := []string{"C", "A", "B", "D"}
	if len(ordered) != len(expected) {
		t.Fatalf("len mismatch: got %d expected %d", len(ordered), len(expected))
	}
	for i, id := range expected {
		if ordered[i].ID != id {
			t.Fatalf("at %d: got %s expected %s", i, ordered[i].ID, id)
		}
	}
}

func TestNormalizeResult_Select(t *testing.T) {
	eligible := makeEligible("A", "B", "C")
	result := DecisionResult{
		Action:     ActionSelect,
		SelectedID: "C",
	}
	ordered, _, _ := NormalizeResult(eligible, result)
	expected := []string{"C", "A", "B"}
	for i, id := range expected {
		if ordered[i].ID != id {
			t.Fatalf("at %d: got %s expected %s", i, ordered[i].ID, id)
		}
	}
}

func TestNormalizeResult_AbstainPreservesOrder(t *testing.T) {
	eligible := makeEligible("A", "B", "C")
	result := DecisionResult{
		Action:    ActionAbstain,
		Abstained: true,
	}
	ordered, applied, _ := NormalizeResult(eligible, result)
	if applied {
		t.Fatal("abstain should not be applied")
	}
	for i := range eligible {
		if ordered[i].ID != eligible[i].ID {
			t.Fatalf("order changed on abstain: %v vs %v", ordered, eligible)
		}
	}
}

func TestNormalizeResult_EmptyEligible(t *testing.T) {
	var eligible []Candidate
	result := DecisionResult{Action: ActionRank, RankedIDs: []string{"A"}}
	ordered, _, _ := NormalizeResult(eligible, result)
	if ordered != nil && len(ordered) != 0 {
		t.Fatalf("expected nil or empty for empty eligible")
	}
}
