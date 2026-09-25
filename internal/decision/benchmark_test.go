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

func BenchmarkValidator_10(b *testing.B) {
	eligible := makeEligible("A", "B", "C", "D", "E", "F", "G", "H", "I", "J")
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

func BenchmarkValidator_100(b *testing.B) {
	ids := make([]string, 100)
	for i := 0; i < 100; i++ {
		ids[i] = string(rune('A'+i%26)) + string(rune('0'+i/26))
	}
	eligible := makeEligible(ids...)
	result := DecisionResult{
		Action:     ActionRank,
		RankedIDs:  []string{ids[10], ids[20], ids[30]},
		Confidence: 0.9,
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ValidateResult(eligible, result)
	}
}

func BenchmarkNormalize_10(b *testing.B) {
	eligible := makeEligible("A", "B", "C", "D", "E", "F", "G", "H", "I", "J")
	result := DecisionResult{
		Action:    ActionRank,
		RankedIDs: []string{"C", "A"},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = NormalizeResult(eligible, result)
	}
}

func BenchmarkNormalize_100(b *testing.B) {
	ids := make([]string, 100)
	for i := 0; i < 100; i++ {
		ids[i] = string(rune('A'+i%26)) + string(rune('0'+i/26))
	}
	eligible := makeEligible(ids...)
	result := DecisionResult{
		Action:    ActionRank,
		RankedIDs: []string{ids[10], ids[20]},
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = NormalizeResult(eligible, result)
	}
}

// Legacy aliases for report compatibility
func BenchmarkValidator(b *testing.B) { BenchmarkValidator_10(b) }
func BenchmarkNormalize(b *testing.B) { BenchmarkNormalize_10(b) }
