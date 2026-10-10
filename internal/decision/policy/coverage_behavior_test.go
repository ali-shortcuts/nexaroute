package policy

import (
	"context"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

func TestCoverageBehaviorProviderReloadAndHealth(t *testing.T) {
	p := NewProvider(nil, "")
	if p.ID() != "policy" || p.Health().Status != decision.HealthHealthy || !p.Capabilities().CanSelect || p.Capabilities().CanRank {
		t.Fatal("policy provider contract mismatch")
	}
	p.UpdatePolicies([]Policy{{ID: "default", Weights: Weights{RouterBaseline: 1}}}, "default")
	ctx := context.Background()
	res, err := p.Decide(ctx, decision.DecisionRequest{Candidates: []decision.Candidate{{ID: "a", OriginalRank: 0}}, PolicyID: "missing"})
	if err != nil || res.Action != decision.ActionSelect || res.SelectedID != "a" {
		t.Fatalf("default policy fallback result=%+v err=%v", res, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.Decide(cancelled, decision.DecisionRequest{Candidates: []decision.Candidate{{ID: "a"}}}); err == nil {
		t.Fatal("canceled policy request should return context error")
	}
}
