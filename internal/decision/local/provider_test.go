package local

import (
	"context"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

func TestLocalAlwaysAbstains(t *testing.T) {
	p := New()
	if p.ID() != decision.BuiltinLocalID || p.Type() != decision.BuiltinLocalID {
		t.Fatalf("id=%q type=%q", p.ID(), p.Type())
	}
	if p.Capabilities().CanSelect || p.Capabilities().CanRank {
		t.Fatalf("caps=%+v", p.Capabilities())
	}
	if !p.Health().Available {
		t.Fatal("local must always be available")
	}
	res, err := p.Decide(context.Background(), decision.DecisionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != decision.ActionAbstain || res.SelectedID != "" {
		t.Fatalf("res=%+v", res)
	}
	if len(res.ReasonCodes) != 1 || res.ReasonCodes[0] != decision.ReasonLocalAbstained {
		t.Fatalf("reasons=%v", res.ReasonCodes)
	}
}
