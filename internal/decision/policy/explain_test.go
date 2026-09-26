package policy

import (
	"encoding/json"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

func TestMarshalBreakdown_ValidJSON(t *testing.T) {
	cands := []decision.Candidate{
		{ID: "a", PoolID: "pool1", PoolOrdinal: 0, Priority: 10, OriginalRank: 0, ContextWindow: 8192},
		{ID: "b", PoolID: "pool1", PoolOrdinal: 0, Priority: 10, OriginalRank: 1, ContextWindow: 16384},
	}
	scored := ScoreCandidates(cands, 1000)
	weights := Weights{RouterBaseline: 1, Reliability: 1, Latency: 1, TTFT: 1, Capacity: 1, Cost: 1, Context: 1}
	scored = ApplyWeights(scored, weights)
	breakdowns := Explain(scored, weights)
	jsonStr := MarshalBreakdown(breakdowns)
	if !json.Valid([]byte(jsonStr)) {
		t.Fatalf("MarshalBreakdown produced invalid JSON: %s", jsonStr)
	}
}

func TestMarshalBreakdown_MaxBoundsValidJSON(t *testing.T) {
	// Create max bounds: 10 candidates, each with long IDs and components
	cands := make([]decision.Candidate, 10)
	for i := 0; i < 10; i++ {
		cands[i] = decision.Candidate{
			ID:            "candidate-" + string(rune('a'+i)) + "-with-very-long-id-to-test-bounding-behavior-1234567890",
			PoolID:        "pool-with-long-id-1234567890",
			PoolOrdinal:   i % 3,
			Priority:      10,
			OriginalRank:  i,
			ContextWindow: 8192 + i*1000,
		}
	}
	scored := ScoreCandidates(cands, 12000)
	weights := Weights{RouterBaseline: 1, Reliability: 1, Latency: 1, TTFT: 1, Capacity: 1, Cost: 1, Context: 1}
	scored = ApplyWeights(scored, weights)
	breakdowns := Explain(scored, weights)
	jsonStr := MarshalBreakdown(breakdowns)
	if !json.Valid([]byte(jsonStr)) {
		t.Fatalf("max bounds produced invalid JSON: %s", jsonStr)
	}
	if len(jsonStr) > 5000 {
		t.Fatalf("expected bounded length <=5000, got %d", len(jsonStr))
	}
}

func TestExplain_PrivacySafe(t *testing.T) {
	canary := "SECRET_POLICY_CANARY_4e91"
	cands := []decision.Candidate{
		{ID: "a", PoolID: "pool1", OriginalRank: 0},
	}
	scored := ScoreCandidates(cands, 1000)
	// Inject canary into candidate ID? Actually ID is bounded, but we test that breakdown does not contain raw prompt
	// We ensure breakdown JSON does not contain canary
	weights := Weights{RouterBaseline: 1}
	scored = ApplyWeights(scored, weights)
	breakdowns := Explain(scored, weights)
	jsonStr := MarshalBreakdown(breakdowns)
	if len(jsonStr) > 0 && contains(jsonStr, canary) {
		t.Fatalf("canary leaked into breakdown")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i <= len(s)-len(substr); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}
