package decision

import (
	"context"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func BenchmarkOrchestrator_OffMode(b *testing.B) {
	reg := NewRegistry()
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "off", Provider: "local", TimeoutMS: 10}, &Metrics{})
	eligible := makeEligible("A", "B", "C", "D", "E", "F", "G", "H")
	req := DecisionRequest{Candidates: eligible, Budget: DefaultBudget()}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = orch.Decide(ctx, req)
	}
}

func BenchmarkOrchestrator_Local(b *testing.B) {
	reg := NewRegistry()
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "local", TimeoutMS: 10}, &Metrics{})
	eligible := makeEligible("A", "B", "C", "D", "E", "F", "G", "H")
	req := DecisionRequest{Candidates: eligible, Budget: DefaultBudget()}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = orch.Decide(ctx, req)
	}
}

func BenchmarkValidator(b *testing.B) {
	eligible := makeEligible("A", "B", "C", "D", "E", "F", "G", "H")
	result := DecisionResult{
		Action:     ActionRank,
		RankedIDs:  []string{"C", "A", "F"},
		Confidence: 0.9,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ValidateResult(eligible, result)
	}
}

func BenchmarkNormalize(b *testing.B) {
	eligible := makeEligible("A", "B", "C", "D", "E", "F", "G", "H")
	result := DecisionResult{
		Action:    ActionRank,
		RankedIDs: []string{"C", "A"},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = NormalizeResult(eligible, result)
	}
}
