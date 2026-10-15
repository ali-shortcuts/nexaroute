package decision

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/decision/providerstate"
)

// Regression (audit 2026-10-15): the per-step cancel func was
// registered with defer INSIDE the chain loop, so every step's
// context stayed alive until Execute returned — a provider relying
// on ctx cancellation for teardown kept running through all later
// steps. The step's cancel must fire when the step finishes.
type ctxCaptureProvider struct {
	id     string
	mu     sync.Mutex
	ctx    context.Context
	result DecisionResult
}

func (p *ctxCaptureProvider) ID() string { return p.id }
func (p *ctxCaptureProvider) Capabilities() Capabilities {
	return Capabilities{CanSelect: true, CanRank: true}
}
func (p *ctxCaptureProvider) Health() ProviderHealth {
	return ProviderHealth{Status: HealthHealthy, CheckedAt: time.Now()}
}
func (p *ctxCaptureProvider) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	p.mu.Lock()
	p.ctx = ctx
	p.mu.Unlock()
	res := p.result
	if res.ProviderID == "" {
		res.ProviderID = p.id
	}
	if res.Action == "" {
		res.Action = ActionAbstain
		res.Abstained = true
	}
	return res, nil
}
func (p *ctxCaptureProvider) captured() context.Context {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ctx
}

type ctxProbeProvider struct {
	id       string
	watch    *ctxCaptureProvider
	observed chan bool
}

func (p *ctxProbeProvider) ID() string { return p.id }
func (p *ctxProbeProvider) Capabilities() Capabilities {
	return Capabilities{CanSelect: true, CanRank: true}
}
func (p *ctxProbeProvider) Health() ProviderHealth {
	return ProviderHealth{Status: HealthHealthy, CheckedAt: time.Now()}
}
func (p *ctxProbeProvider) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	cancelled := false
	if c := p.watch.captured(); c != nil && c.Err() != nil {
		cancelled = true
	}
	select {
	case p.observed <- cancelled:
	default:
	}
	return DecisionResult{Action: ActionSelect, SelectedID: "C", Confidence: 0.9, ReasonCodes: []ReasonCode{ReasonEligibleSetPreserved}, ProviderID: p.id}, nil
}

func TestChain_StepContextCancelledWhenStepEnds(t *testing.T) {
	eligible := makeChainEligible("A", "B", "C")
	first := &ctxCaptureProvider{id: "jev-main"}
	observed := make(chan bool, 1)
	second := &ctxProbeProvider{id: "policy", watch: first, observed: observed}
	reg := chainRegistry(first, second)
	exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
	chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main", TimeoutMS: 100}, {Provider: "policy"}}}
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(500*time.Millisecond, 2)}
	_, result, _ := exec.Execute(context.Background(), chain, req, defaultConstraints(eligible), req.Budget, 500*time.Millisecond)
	if result.SelectedID != "C" {
		t.Fatalf("second step should win, got %+v", result)
	}
	select {
	case cancelled := <-observed:
		if !cancelled {
			t.Fatal("first step's context was still live when the second step ran (cancel deferred to Execute return)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second step never reported")
	}
}
