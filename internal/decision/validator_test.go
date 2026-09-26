package decision

import (
	"math"
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
	tests := []struct {
		name string
		conf float64
	}{
		{"too high", 1.5},
		{"too low", -0.1},
		{"NaN", math.NaN()},
		{"+Inf", math.Inf(1)},
		{"-Inf", math.Inf(-1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := DecisionResult{
				Action:     ActionAbstain,
				Confidence: tc.conf,
			}
			if err := ValidateResult(eligible, result); err == nil {
				t.Fatalf("expected error for confidence %v", tc.conf)
			}
		})
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

func TestValidateResult_StrictAction(t *testing.T) {
	eligible := makeEligible("A", "B")
	cases := []DecisionResult{
		{Action: "", Confidence: 0.5},
		{Action: "FOO", Confidence: 0.5},
		{Action: ActionSelect, SelectedID: "", Confidence: 0.5},
		{Action: ActionSelect, SelectedID: "A", RankedIDs: []string{"A"}, Confidence: 0.5},
		{Action: ActionRank, RankedIDs: []string{}, Confidence: 0.5},
		{Action: ActionRank, SelectedID: "A", RankedIDs: []string{"A"}, Confidence: 0.5},
		{Action: ActionAbstain, SelectedID: "A", Confidence: 0.5},
		{Action: ActionAbstain, RankedIDs: []string{"A"}, Confidence: 0.5},
	}
	for i, r := range cases {
		if err := ValidateResult(eligible, r); err == nil {
			t.Fatalf("case %d should be invalid: %+v", i, r)
		}
	}
}

func TestValidateResult_BoundedRanked(t *testing.T) {
	eligible := makeEligible("A", "B")
	result := DecisionResult{
		Action:     ActionRank,
		RankedIDs:  []string{"A", "B", "C"},
		Confidence: 0.5,
	}
	if err := ValidateResult(eligible, result); err == nil {
		t.Fatal("expected error for ranked > eligible")
	}
}

func TestValidateResult_ReasonCodeBounded(t *testing.T) {
	eligible := makeEligible("A", "B")
	// Unknown reason code
	result := DecisionResult{
		Action:      ActionAbstain,
		Confidence:  0.5,
		ReasonCodes: []ReasonCode{"UNKNOWN_CODE"},
	}
	if err := ValidateResult(eligible, result); err == nil {
		t.Fatal("expected error for unknown reason code")
	}
	// Too many reason codes
	many := make([]ReasonCode, MaxReasonCodes+1)
	for i := range many {
		many[i] = ReasonAbstained
	}
	result = DecisionResult{
		Action:      ActionAbstain,
		Confidence:  0.5,
		ReasonCodes: many,
	}
	if err := ValidateResult(eligible, result); err == nil {
		t.Fatal("expected error for too many reason codes")
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
	ordered, _, reason := NormalizeResult(eligible, result)
	if ordered != nil && len(ordered) != 0 {
		t.Fatalf("expected nil or empty for empty eligible")
	}
	if reason != ReasonEmptyEligible {
		t.Fatalf("expected EMPTY_ELIGIBLE reason")
	}
}
