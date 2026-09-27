package decision

import (
	"context"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/decision/providerstate"
)

// BenchmarkChain_ThreeSteps covers the hot path for hybrid chains: jev -> policy -> local
func BenchmarkChain_ThreeSteps(b *testing.B) {
	reg := NewRegistry()
	p1 := &benchProvider{id: "jev-main", sel: "A"}
	p2 := &benchProvider{id: "policy", sel: "B"}
	p3 := &benchProvider{id: "local", sel: "C"}
	reg.Register(p1)
	reg.Register(p2)
	reg.Register(p3)
	mgr := providerstate.New(providerstate.Config{FailureThreshold: 2, FailureWindow: 30 * time.Second, Cooldown: 60 * time.Second}, nil)
	exec := NewChainExecutor(reg, mgr, &Metrics{})
	chain := ChainConfig{ID: "bench-chain", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}, {Provider: "local"}}}
	eligible := makeChainEligible("A", "B", "C", "D")
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(50*time.Millisecond, 3)}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = exec.Execute(ctx, chain, req, constraints, req.Budget, 50*time.Millisecond)
	}
}

func BenchmarkChain_AbstainContinues(b *testing.B) {
	reg := NewRegistry()
	p1 := &benchProvider{id: "jev-main", abstain: true}
	p2 := &benchProvider{id: "policy", sel: "B"}
	reg.Register(p1)
	reg.Register(p2)
	mgr := providerstate.New(providerstate.Config{FailureThreshold: 2, FailureWindow: 30 * time.Second, Cooldown: 60 * time.Second}, nil)
	exec := NewChainExecutor(reg, mgr, &Metrics{})
	chain := ChainConfig{ID: "bench", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}}}
	eligible := makeChainEligible("A", "B", "C")
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(50*time.Millisecond, 2)}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = exec.Execute(ctx, chain, req, constraints, req.Budget, 50*time.Millisecond)
	}
}

func BenchmarkChain_CooldownSkip(b *testing.B) {
	reg := NewRegistry()
	p1 := &benchProvider{id: "jev-main", sel: "A"}
	p2 := &benchProvider{id: "policy", sel: "B"}
	reg.Register(p1)
	reg.Register(p2)
	mgr := providerstate.New(providerstate.Config{FailureThreshold: 2, FailureWindow: 30 * time.Second, Cooldown: 60 * time.Second}, nil)
	mgr.RecordFailure("jev-main")
	mgr.RecordFailure("jev-main")
	exec := NewChainExecutor(reg, mgr, &Metrics{})
	chain := ChainConfig{ID: "bench", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}}}
	eligible := makeChainEligible("A", "B", "C")
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(50*time.Millisecond, 2)}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = exec.Execute(ctx, chain, req, constraints, req.Budget, 50*time.Millisecond)
	}
}

type benchProvider struct {
	id      string
	sel     string
	abstain bool
}

func (p *benchProvider) ID() string { return p.id }
func (p *benchProvider) Capabilities() Capabilities {
	return Capabilities{CanSelect: true, CanRank: true}
}
func (p *benchProvider) Health() ProviderHealth {
	return ProviderHealth{Status: HealthHealthy, CheckedAt: time.Now()}
}
func (p *benchProvider) Decide(_ context.Context, req DecisionRequest) (DecisionResult, error) {
	if p.abstain {
		return DecisionResult{Action: ActionAbstain, Abstained: true, Confidence: 1.0, ReasonCodes: []ReasonCode{ReasonAbstained}, ProviderID: p.id}, nil
	}
	return DecisionResult{Action: ActionSelect, SelectedID: p.sel, Confidence: 0.9, ReasonCodes: []ReasonCode{ReasonEligibleSetPreserved}, ProviderID: p.id}, nil
}
