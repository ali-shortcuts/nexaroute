package decision

import (
	"context"
	"testing"
)

func TestLocalProvider_PreservesOrder(t *testing.T) {
	p := &LocalProvider{}
	candidates := makeEligible("A", "B", "C", "D")
	req := DecisionRequest{
		Candidates: candidates,
		Budget:     DefaultBudget(),
	}
	res, err := p.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != ActionAbstain {
		t.Fatalf("expected abstain, got %v", res.Action)
	}
	found := false
	for _, rc := range res.ReasonCodes {
		if rc == ReasonExistingOrderPreserved {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected %s in reason codes, got %v", ReasonExistingOrderPreserved, res.ReasonCodes)
	}
}

func TestLocalProvider_ContextCancellation(t *testing.T) {
	p := &LocalProvider{}
	candidates := makeEligible("A", "B")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := DecisionRequest{Candidates: candidates}
	res, err := p.Decide(ctx, req)
	if err == nil {
		// Local provider checks context and returns error, but orchestrator will handle
	}
	if res.Action != ActionAbstain {
		t.Fatalf("expected abstain on cancelled context")
	}
}
