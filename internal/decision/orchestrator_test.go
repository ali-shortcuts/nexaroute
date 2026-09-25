package decision

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// mock provider for testing
type mockProvider struct {
	id     string
	result DecisionResult
	err    error
	panic  bool
	delay  time.Duration
}

func (m *mockProvider) ID() string { return m.id }
func (m *mockProvider) Capabilities() Capabilities {
	return Capabilities{CanRank: true, CanSelect: true}
}
func (m *mockProvider) Health() ProviderHealth {
	return ProviderHealth{Status: HealthHealthy, CheckedAt: time.Now()}
}
func (m *mockProvider) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return DecisionResult{
				Action:      ActionAbstain,
				Abstained:   true,
				ReasonCodes: []string{ReasonTimeout},
				ProviderID:  m.id,
			}, ctx.Err()
		}
	}
	if m.panic {
		panic("mock panic")
	}
	if m.err != nil {
		return m.result, m.err
	}
	res := m.result
	if res.ProviderID == "" {
		res.ProviderID = m.id
	}
	return res, nil
}

func TestOrchestrator_OffMode(t *testing.T) {
	reg := NewRegistry()
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "off", Provider: "local", TimeoutMS: 10}, &Metrics{})
	eligible := makeEligible("A", "B", "C")
	req := DecisionRequest{Candidates: eligible, Budget: DefaultBudget()}
	ordered, result, _ := orch.Decide(context.Background(), req)
	if len(ordered) != len(eligible) {
		t.Fatalf("off mode length mismatch")
	}
	for i := range eligible {
		if ordered[i].ID != eligible[i].ID {
			t.Fatalf("off mode should preserve order")
		}
	}
	if !result.IsAbstain() {
		t.Fatalf("off mode should abstain")
	}
	// Ensure reason OFF_MODE present
	found := false
	for _, rc := range result.ReasonCodes {
		if rc == ReasonOffMode {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected OFF_MODE reason")
	}
}

func TestOrchestrator_LocalPreservesOrder(t *testing.T) {
	reg := NewRegistry()
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "local", TimeoutMS: 10}, &Metrics{})
	eligible := makeEligible("A", "B", "C", "D")
	req := DecisionRequest{Candidates: eligible, Budget: DefaultBudget()}
	ordered, result, _ := orch.Decide(context.Background(), req)
	if len(ordered) != len(eligible) {
		t.Fatalf("length mismatch")
	}
	for i := range eligible {
		if ordered[i].ID != eligible[i].ID {
			t.Fatalf("local should preserve order, got %v", ordered)
		}
	}
	if !result.IsAbstain() {
		t.Fatalf("local should abstain")
	}
}

func TestOrchestrator_ValidRank(t *testing.T) {
	reg := &Registry{providers: map[string]DecisionProvider{}}
	mock := &mockProvider{
		id: "test",
		result: DecisionResult{
			Action:     ActionRank,
			RankedIDs:  []string{"C", "A"},
			Confidence: 0.8,
		},
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "test", TimeoutMS: 100}, &Metrics{})
	eligible := makeEligible("A", "B", "C", "D")
	req := DecisionRequest{Candidates: eligible}
	ordered, result, _ := orch.Decide(context.Background(), req)
	if result.IsAbstain() {
		t.Fatalf("should not abstain on valid rank")
	}
	expected := []string{"C", "A", "B", "D"}
	if len(ordered) != len(expected) {
		t.Fatalf("len mismatch: got %v", ordered)
	}
	for i, id := range expected {
		if ordered[i].ID != id {
			t.Fatalf("at %d: got %s expected %s", i, ordered[i].ID, id)
		}
	}
}

func TestOrchestrator_InvalidResultFailOpen(t *testing.T) {
	reg := &Registry{providers: map[string]DecisionProvider{}}
	mock := &mockProvider{
		id: "bad",
		result: DecisionResult{
			Action:     ActionRank,
			RankedIDs:  []string{"A", "Z"}, // Z unknown
			Confidence: 0.9,
		},
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "bad", TimeoutMS: 100}, &Metrics{})
	eligible := makeEligible("A", "B")
	req := DecisionRequest{Candidates: eligible}
	ordered, result, _ := orch.Decide(context.Background(), req)
	// Should fail-open to original order
	for i := range eligible {
		if ordered[i].ID != eligible[i].ID {
			t.Fatalf("fail-open should preserve original order")
		}
	}
	if !result.IsAbstain() {
		t.Fatalf("invalid result should result in abstain")
	}
}

func TestOrchestrator_TimeoutFailOpen(t *testing.T) {
	reg := &Registry{providers: map[string]DecisionProvider{}}
	mock := &mockProvider{
		id:    "slow",
		delay: 50 * time.Millisecond,
		result: DecisionResult{
			Action:    ActionRank,
			RankedIDs: []string{"B", "A"},
		},
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "slow", TimeoutMS: 5}, &Metrics{})
	eligible := makeEligible("A", "B")
	req := DecisionRequest{Candidates: eligible}
	ordered, result, _ := orch.Decide(context.Background(), req)
	// Should timeout and preserve order
	for i := range eligible {
		if ordered[i].ID != eligible[i].ID {
			t.Fatalf("timeout should preserve order")
		}
	}
	foundTimeout := false
	for _, rc := range result.ReasonCodes {
		if rc == ReasonTimeout {
			foundTimeout = true
		}
	}
	if !foundTimeout {
		t.Fatalf("expected timeout reason, got %v", result.ReasonCodes)
	}
}

func TestOrchestrator_ErrorFailOpen(t *testing.T) {
	reg := &Registry{providers: map[string]DecisionProvider{}}
	mock := &mockProvider{
		id:  "err",
		err: fmt.Errorf("provider error"),
		result: DecisionResult{
			Action: ActionAbstain,
		},
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "err", TimeoutMS: 100}, &Metrics{})
	eligible := makeEligible("A", "B", "C")
	req := DecisionRequest{Candidates: eligible}
	ordered, _, _ := orch.Decide(context.Background(), req)
	for i := range eligible {
		if ordered[i].ID != eligible[i].ID {
			t.Fatalf("error should preserve order")
		}
	}
}

func TestOrchestrator_PanicRecovery(t *testing.T) {
	reg := &Registry{providers: map[string]DecisionProvider{}}
	mock := &mockProvider{
		id:    "panic",
		panic: true,
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "panic", TimeoutMS: 100}, &Metrics{})
	eligible := makeEligible("A", "B")
	req := DecisionRequest{Candidates: eligible}
	ordered, result, _ := orch.Decide(context.Background(), req)
	for i := range eligible {
		if ordered[i].ID != eligible[i].ID {
			t.Fatalf("panic should preserve order")
		}
	}
	found := false
	for _, rc := range result.ReasonCodes {
		if rc == ReasonProviderPanic {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected panic reason, got %v", result.ReasonCodes)
	}
}

func TestOrchestrator_EligibleSetInvariant(t *testing.T) {
	// Property test: no provider can inject unknown candidate
	reg := &Registry{providers: map[string]DecisionProvider{}}
	mock := &mockProvider{
		id: "inject",
		result: DecisionResult{
			Action:    ActionRank,
			RankedIDs: []string{"A", "B", "EVIL"},
		},
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "inject", TimeoutMS: 100}, &Metrics{})
	eligible := makeEligible("A", "B")
	req := DecisionRequest{Candidates: eligible}
	ordered, _, _ := orch.Decide(context.Background(), req)
	// Must not contain EVIL
	for _, c := range ordered {
		if c.ID == "EVIL" {
			t.Fatalf("eligible-set invariant violated: EVIL injected")
		}
	}
	// Must contain exactly A,B
	if len(ordered) != 2 {
		t.Fatalf("length should be 2, got %d", len(ordered))
	}
}

func TestOrchestrator_SelectNormalization(t *testing.T) {
	reg := &Registry{providers: map[string]DecisionProvider{}}
	mock := &mockProvider{
		id: "sel",
		result: DecisionResult{
			Action:     ActionSelect,
			SelectedID: "C",
			Confidence: 0.9,
		},
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "sel", TimeoutMS: 100}, &Metrics{})
	eligible := makeEligible("A", "B", "C", "D")
	req := DecisionRequest{Candidates: eligible}
	ordered, _, _ := orch.Decide(context.Background(), req)
	expected := []string{"C", "A", "B", "D"}
	for i, id := range expected {
		if ordered[i].ID != id {
			t.Fatalf("select normalization failed: at %d got %s expected %s", i, ordered[i].ID, id)
		}
	}
}
