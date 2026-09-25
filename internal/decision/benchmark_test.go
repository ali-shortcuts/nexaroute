package decision

import (
	"context"
	"testing"
)

// Local-work benchmarks only: no network, no Internet latency.

func benchCandidates(n int) []Candidate {
	out := make([]Candidate, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Candidate{
			ID: "p/model", PoolOrdinal: i % 3, Priority: (i % 2) * 5,
			ContextWindow: 128000, Tools: true, Streaming: true,
			EWMALatencyMS: 120, EWMAFailureRate: 0.01, Observations: 100,
		})
	}
	return out
}

func BenchmarkComputeConstraints2(b *testing.B) {
	c := benchCandidates(2)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = ComputeConstraints(c, "")
	}
}

func BenchmarkComputeConstraints10(b *testing.B) {
	c := benchCandidates(10)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = ComputeConstraints(c, "")
	}
}

func BenchmarkComputeConstraints100(b *testing.B) {
	c := benchCandidates(100)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = ComputeConstraints(c, "")
	}
}

func BenchmarkValidateSelect(b *testing.B) {
	c := benchCandidates(10)
	req := DecisionRequest{Candidates: c, AllowedPrimaryIDs: []string{"p/model"}}
	// Distinct IDs collapse to one; rebuild with unique IDs for realism.
	for i := range req.Candidates {
		req.Candidates[i].ID = string(rune('A'+i)) + req.Candidates[i].ID
	}
	req.AllowedPrimaryIDs = []string{req.Candidates[3].ID}
	res := DecisionResult{Action: ActionSelect, SelectedID: req.Candidates[3].ID, Confidence: 0.5, ReasonCodes: []ReasonCode{ReasonExternalSelected}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = Validate(req, res)
	}
}

type benchAbstainProvider struct{}

func (benchAbstainProvider) ID() string   { return "bench" }
func (benchAbstainProvider) Type() string { return "local" }
func (benchAbstainProvider) Capabilities() Capabilities {
	return Capabilities{}
}
func (benchAbstainProvider) Health() Health { return Health{Available: true} }
func (benchAbstainProvider) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	return DecisionResult{Action: ActionAbstain, ReasonCodes: []ReasonCode{ReasonLocalAbstained}}, nil
}

func BenchmarkOrchestratorAbstain(b *testing.B) {
	c := benchCandidates(10)
	o := NewOrchestrator(ModeLocal, benchAbstainProvider{}, 0)
	ctx := context.Background()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = o.Decide(ctx, c, "", RequestFeatures{}, "r")
	}
}
