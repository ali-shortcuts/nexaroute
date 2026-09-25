package policy

import (
	"context"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

func TestPolicySelectsFirstAllowed(t *testing.T) {
	p := New()
	if p.ID() != decision.BuiltinPolicyID || p.Type() != decision.BuiltinPolicyID {
		t.Fatalf("id=%q type=%q", p.ID(), p.Type())
	}
	if !p.Capabilities().CanSelect || p.Capabilities().CanRank {
		t.Fatalf("caps=%+v", p.Capabilities())
	}
	req := decision.DecisionRequest{
		Candidates:        []decision.Candidate{{ID: "A"}, {ID: "B"}, {ID: "C"}},
		AllowedPrimaryIDs: []string{"B", "A"},
	}
	res, err := p.Decide(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != decision.ActionSelect || res.SelectedID != "B" {
		t.Fatalf("res=%+v", res)
	}
	if len(res.ReasonCodes) != 1 || res.ReasonCodes[0] != decision.ReasonPolicySelected {
		t.Fatalf("reasons=%v", res.ReasonCodes)
	}
	// The policy selection must always validate: policy can never violate
	// the guardrails it shares with the orchestrator.
	if err := decision.Validate(req, res); err != nil {
		t.Fatalf("policy result must validate: %v", err)
	}
}

func TestPolicyHonorsForcedPin(t *testing.T) {
	p := New()
	req := decision.DecisionRequest{
		Candidates:        []decision.Candidate{{ID: "A"}, {ID: "B"}},
		AllowedPrimaryIDs: []string{"B"},
		ForcedPrimaryID:   "B",
	}
	res, err := p.Decide(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedID != "B" || res.ReasonCodes[0] != decision.ReasonAffinityPreserved {
		t.Fatalf("res=%+v", res)
	}
}

func TestPolicyAbstainsWhenNothingAllowed(t *testing.T) {
	p := New()
	res, err := p.Decide(context.Background(), decision.DecisionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != decision.ActionAbstain {
		t.Fatalf("res=%+v", res)
	}
}
