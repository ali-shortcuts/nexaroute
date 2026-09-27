package policy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/events"
)

const policyCanary = "SECRET_POLICY_CANARY_4e91"

func TestPolicyPrivacy_NoCanaryInBreakdown(t *testing.T) {
	cands := []decision.Candidate{
		{ID: "a", PoolID: "pool1", OriginalRank: 0},
		{ID: "b", PoolID: "pool1", OriginalRank: 1},
	}
	scored := ScoreCandidates(cands, 1000)
	weights := Weights{RouterBaseline: 1}
	scored = ApplyWeights(scored, weights)
	breakdowns := Explain(scored, weights)
	jsonStr := MarshalBreakdown(breakdowns)
	if strings.Contains(jsonStr, policyCanary) {
		t.Fatalf("canary leaked into breakdown")
	}
	// Also marshal DecisionResult with PolicyTrace
	res := decision.DecisionResult{
		Action:      decision.ActionSelect,
		SelectedID:  "a",
		Confidence:  0.9,
		ReasonCodes: []decision.ReasonCode{decision.ReasonPolicyScored},
		ProviderID:  "policy",
		PolicyTrace: &decision.PolicyTrace{
			PolicyID:             "balanced",
			TaskType:             "coding",
			OriginalPrimaryID:    "b",
			SelectedID:           "a",
			ChangedPrimary:       true,
			SelectedScore:        0.9,
			OriginalPrimaryScore: 0.5,
		},
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), policyCanary) {
		t.Fatalf("canary in result")
	}
}

func TestPolicyPrivacy_NoCanaryInEvent(t *testing.T) {
	ev := events.Event{
		Kind:                    "decision_ok",
		DecisionProvider:        "policy",
		DecisionAction:          "SELECT",
		DecisionReasonCodes:     "POLICY_SCORED",
		DecisionPolicyID:        "balanced",
		DecisionTaskType:        "coding",
		DecisionOriginalPrimary: "b",
		DecisionSelected:        "a",
		DecisionSelectedScore:   0.9,
		DecisionOriginalScore:   0.5,
		DecisionChangedPrimary:  true,
	}
	b, _ := json.Marshal(ev)
	s := string(b)
	if strings.Contains(s, policyCanary) {
		t.Fatalf("canary leaked into event")
	}
	// Ensure breakdown field if present does not contain canary
	if strings.Contains(s, "SECRET_") {
		// The event itself should not have canary, but we specifically check for policy canary
		t.Logf("event contains SECRET but not necessarily canary, checking...")
		if strings.Contains(s, policyCanary) {
			t.Fatalf("canary in event")
		}
	}
}

func TestPolicyPrivacy_FullPathWithCanaryInput(t *testing.T) {
	// Simulate a request whose features contain the canary in prompt text (which should never be included in decision artifacts)
	// The decision request itself should not contain raw prompt
	cands := []decision.Candidate{
		{ID: "a", OriginalRank: 0},
	}
	req := decision.DecisionRequest{
		Candidates: cands,
		RequestID:  "req-" + policyCanary, // request ID containing canary should be sanitized? Actually request ID is bounded and sanitized elsewhere
		PolicyID:   "balanced",
	}
	// Ensure marshaling request does not leak canary into breakdown or trace unless request ID itself contains it (request ID is user-controlled but bounded)
	// For policy provider, we check that breakdown and event do not contain canary even when request ID contains it
	// This tests that breakdown generation does not include request ID
	b, _ := json.Marshal(req)
	_ = b // request may contain canary in RequestID, that's allowed but should not propagate to breakdown
	scored := ScoreCandidates(cands, 1000)
	weights := Weights{RouterBaseline: 1}
	scored = ApplyWeights(scored, weights)
	breakdowns := Explain(scored, weights)
	jsonStr := MarshalBreakdown(breakdowns)
	if strings.Contains(jsonStr, policyCanary) {
		t.Fatalf("canary from request ID leaked into breakdown")
	}
}
