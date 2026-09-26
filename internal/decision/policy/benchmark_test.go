package policy

import (
	"context"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

func benchCandidates(n int) []decision.Candidate {
	cands := make([]decision.Candidate, n)
	for i := 0; i < n; i++ {
		cands[i] = decision.Candidate{
			ID:               string(rune('a'+i%26)) + string(rune('0'+i/26)),
			ProviderID:       "p",
			Model:            "m",
			Priority:         10,
			PoolOrdinal:      0,
			RouterScore:      float64(i) * 0.01,
			HealthStatus:     "healthy",
			EWMALatencyMS:    float64(100 + i*5),
			EWMATTFTMS:       float64(50 + i*2),
			EWMAFailureRate:  0.01,
			Successes:        10,
			Failures:         1,
			CapacityPressure: 0.2,
			EstimatedCostUSD: 0.001,
			PriceKnown:       true,
			ContextWindow:    8192 + i*100,
			OriginalRank:     i,
		}
	}
	return cands
}

func BenchmarkScoreCandidates_2(b *testing.B) {
	cands := benchCandidates(2)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ScoreCandidates(cands, 1000)
	}
}

func BenchmarkScoreCandidates_10(b *testing.B) {
	cands := benchCandidates(10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ScoreCandidates(cands, 1000)
	}
}

func BenchmarkScoreCandidates_100(b *testing.B) {
	cands := benchCandidates(100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ScoreCandidates(cands, 1000)
	}
}

func BenchmarkApplyWeights_2(b *testing.B) {
	cands := benchCandidates(2)
	weights := Weights{RouterBaseline: 1, Reliability: 1, Latency: 1, TTFT: 1, Capacity: 1, Cost: 1, Context: 1}
	scored := ScoreCandidates(cands, 1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ApplyWeights(scored, weights)
	}
}

func BenchmarkApplyWeights_10(b *testing.B) {
	cands := benchCandidates(10)
	weights := Weights{RouterBaseline: 1, Reliability: 1, Latency: 1, TTFT: 1, Capacity: 1, Cost: 1, Context: 1}
	scored := ScoreCandidates(cands, 1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ApplyWeights(scored, weights)
	}
}

func BenchmarkApplyWeights_100(b *testing.B) {
	cands := benchCandidates(100)
	weights := Weights{RouterBaseline: 1, Reliability: 1, Latency: 1, TTFT: 1, Capacity: 1, Cost: 1, Context: 1}
	scored := ScoreCandidates(cands, 1000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ApplyWeights(scored, weights)
	}
}

func BenchmarkProviderDecide_2(b *testing.B) {
	pol := Policy{
		ID:            "test",
		SelectionMode: "select_first",
		Weights:       Weights{RouterBaseline: 1, Reliability: 1, Latency: 1, TTFT: 1, Capacity: 1, Cost: 1, Context: 1},
		MinScoreDelta: 0.05,
	}
	p := NewProvider([]Policy{pol}, "test")
	cands := benchCandidates(2)
	req := decision.DecisionRequest{
		Candidates:       cands,
		PolicyID:         "test",
		MinContextWindow: 1000,
		RequestID:        "bench",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = p.Decide(context.Background(), req)
	}
}

func BenchmarkProviderDecide_10(b *testing.B) {
	pol := Policy{
		ID:            "test",
		SelectionMode: "select_first",
		Weights:       Weights{RouterBaseline: 1, Reliability: 1, Latency: 1, TTFT: 1, Capacity: 1, Cost: 1, Context: 1},
		MinScoreDelta: 0.05,
	}
	p := NewProvider([]Policy{pol}, "test")
	cands := benchCandidates(10)
	req := decision.DecisionRequest{
		Candidates:       cands,
		PolicyID:         "test",
		MinContextWindow: 1000,
		RequestID:        "bench",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = p.Decide(context.Background(), req)
	}
}

func BenchmarkProviderDecide_100(b *testing.B) {
	pol := Policy{
		ID:            "test",
		SelectionMode: "select_first",
		Weights:       Weights{RouterBaseline: 1, Reliability: 1, Latency: 1, TTFT: 1, Capacity: 1, Cost: 1, Context: 1},
		MinScoreDelta: 0.05,
	}
	p := NewProvider([]Policy{pol}, "test")
	cands := benchCandidates(100)
	req := decision.DecisionRequest{
		Candidates:       cands,
		PolicyID:         "test",
		MinContextWindow: 1000,
		RequestID:        "bench",
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = p.Decide(context.Background(), req)
	}
}

func BenchmarkResolveWeights(b *testing.B) {
	pol := Policy{
		ID:            "test",
		SelectionMode: "select_first",
		Weights:       Weights{RouterBaseline: 1},
		TaskOverrides: map[string]Weights{
			"coding":    {Latency: 1},
			"debugging": {Reliability: 1},
		},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = pol.ResolveWeights("coding")
	}
}

func BenchmarkContextScoring(b *testing.B) {
	cands := benchCandidates(10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		computeContext(cands, 12000)
	}
}
