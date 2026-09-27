package decision

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// mock provider for testing
type mockProvider struct {
	id           string
	result       DecisionResult
	err          error
	panic        bool
	delay        time.Duration
	capabilities Capabilities
	healthStatus string
	callCount    int
	mu           sync.Mutex
}

func (m *mockProvider) ID() string { return m.id }
func (m *mockProvider) Capabilities() Capabilities {
	// default both true if not set
	if m.capabilities.CanRank == false && m.capabilities.CanSelect == false {
		// If explicitly set to false/false via zero value, we want default true/true unless overridden
		// Use a flag: if id contains "no-cap", return false, else true
		// Simpler: return what is set, but if both false and not explicitly intended, return true/true
		// For tests that need specific caps, they set field directly
		return Capabilities{CanRank: true, CanSelect: true}
	}
	return m.capabilities
}
func (m *mockProvider) Health() ProviderHealth {
	status := m.healthStatus
	if status == "" {
		status = HealthHealthy
	}
	return ProviderHealth{Status: status, CheckedAt: time.Now()}
}
func (m *mockProvider) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	m.mu.Lock()
	m.callCount++
	m.mu.Unlock()
	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return DecisionResult{
				Action:      ActionAbstain,
				Abstained:   true,
				ReasonCodes: []ReasonCode{ReasonTimeout},
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
func (m *mockProvider) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.callCount
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
	if result.Action != ActionAbstain {
		t.Fatalf("off mode should abstain")
	}
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
	if result.Action != ActionAbstain {
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
		capabilities: Capabilities{CanRank: true, CanSelect: true},
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "test", TimeoutMS: 100}, &Metrics{})
	eligible := makeEligible("A", "B", "C", "D")
	req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 100 * time.Millisecond, MaxProviderCalls: 1}}
	ordered, result, _ := orch.Decide(context.Background(), req)
	if result.Action == ActionAbstain {
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
			RankedIDs:  []string{"A", "Z"},
			Confidence: 0.9,
		},
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "bad", TimeoutMS: 100}, &Metrics{})
	eligible := makeEligible("A", "B")
	req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 100 * time.Millisecond, MaxProviderCalls: 1}}
	ordered, result, _ := orch.Decide(context.Background(), req)
	for i := range eligible {
		if ordered[i].ID != eligible[i].ID {
			t.Fatalf("fail-open should preserve original order")
		}
	}
	if result.Action != ActionAbstain {
		t.Fatalf("invalid result should result in abstain")
	}
}

func TestOrchestrator_TimeoutFailOpen(t *testing.T) {
	reg := &Registry{providers: map[string]DecisionProvider{}}
	mock := &mockProvider{
		id:    "slow",
		delay: 50 * time.Millisecond,
		result: DecisionResult{
			Action:     ActionRank,
			RankedIDs:  []string{"B", "A"},
			Confidence: 0.5,
		},
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "slow", TimeoutMS: 5}, &Metrics{})
	eligible := makeEligible("A", "B")
	req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 5 * time.Millisecond, MaxProviderCalls: 1}}
	ordered, result, _ := orch.Decide(context.Background(), req)
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
	req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 100 * time.Millisecond, MaxProviderCalls: 1}}
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
	req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 100 * time.Millisecond, MaxProviderCalls: 1}}
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
	reg := &Registry{providers: map[string]DecisionProvider{}}
	mock := &mockProvider{
		id: "inject",
		result: DecisionResult{
			Action:     ActionRank,
			RankedIDs:  []string{"A", "B", "EVIL"},
			Confidence: 0.5,
		},
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "inject", TimeoutMS: 100}, &Metrics{})
	eligible := makeEligible("A", "B")
	req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 100 * time.Millisecond, MaxProviderCalls: 1}}
	ordered, _, _ := orch.Decide(context.Background(), req)
	for _, c := range ordered {
		if c.ID == "EVIL" {
			t.Fatalf("eligible-set invariant violated: EVIL injected")
		}
	}
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
		capabilities: Capabilities{CanRank: true, CanSelect: true},
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "sel", TimeoutMS: 100}, &Metrics{})
	eligible := makeEligible("A", "B", "C", "D")
	req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 100 * time.Millisecond, MaxProviderCalls: 1}}
	ordered, _, _ := orch.Decide(context.Background(), req)
	expected := []string{"C", "A", "B", "D"}
	for i, id := range expected {
		if ordered[i].ID != id {
			t.Fatalf("select normalization failed: at %d got %s expected %s", i, ordered[i].ID, id)
		}
	}
}

func TestOrchestrator_EmptyEligible(t *testing.T) {
	reg := &Registry{providers: map[string]DecisionProvider{}}
	mock := &mockProvider{
		id: "should-not-be-called",
		result: DecisionResult{
			Action:    ActionRank,
			RankedIDs: []string{"A"},
		},
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "should-not-be-called", TimeoutMS: 100}, &Metrics{})
	req := DecisionRequest{Candidates: []Candidate{}, Budget: Budget{Timeout: 100 * time.Millisecond, MaxProviderCalls: 1}}
	ordered, result, _ := orch.Decide(context.Background(), req)
	if len(ordered) != 0 {
		t.Fatalf("empty eligible should return empty")
	}
	if mock.Calls() != 0 {
		t.Fatalf("provider should not be called for empty eligible, got %d", mock.Calls())
	}
	found := false
	for _, rc := range result.ReasonCodes {
		if rc == ReasonEmptyEligible {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected EMPTY_ELIGIBLE reason")
	}
}

func TestOrchestrator_SingleCandidate(t *testing.T) {
	reg := &Registry{providers: map[string]DecisionProvider{}}
	mock := &mockProvider{
		id: "should-not-be-called-single",
		result: DecisionResult{
			Action:    ActionRank,
			RankedIDs: []string{"A"},
		},
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "should-not-be-called-single", TimeoutMS: 100}, &Metrics{})
	eligible := makeEligible("A")
	req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 100 * time.Millisecond, MaxProviderCalls: 1}}
	ordered, result, _ := orch.Decide(context.Background(), req)
	if len(ordered) != 1 || ordered[0].ID != "A" {
		t.Fatalf("single candidate should return same")
	}
	if mock.Calls() != 0 {
		t.Fatalf("provider should not be called for single candidate, got %d", mock.Calls())
	}
	found := false
	for _, rc := range result.ReasonCodes {
		if rc == ReasonSingleCandidate {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected SINGLE_CANDIDATE reason, got %v", result.ReasonCodes)
	}
}

func TestOrchestrator_CapabilitiesEnforced(t *testing.T) {
	// Provider cannot rank but returns RANK
	reg := &Registry{providers: map[string]DecisionProvider{}}
	mock := &mockProvider{
		id: "no-rank",
		result: DecisionResult{
			Action:     ActionRank,
			RankedIDs:  []string{"B", "A"},
			Confidence: 0.8,
		},
		capabilities: Capabilities{CanRank: false, CanSelect: true},
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "no-rank", TimeoutMS: 100}, &Metrics{})
	eligible := makeEligible("A", "B")
	req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 100 * time.Millisecond, MaxProviderCalls: 1}}
	ordered, result, _ := orch.Decide(context.Background(), req)
	// Should fail-open
	for i := range eligible {
		if ordered[i].ID != eligible[i].ID {
			t.Fatalf("capability mismatch should preserve order")
		}
	}
	if result.Action != ActionAbstain {
		t.Fatalf("should abstain on capability mismatch")
	}

	// Provider cannot select but returns SELECT
	reg2 := &Registry{providers: map[string]DecisionProvider{}}
	mock2 := &mockProvider{
		id: "no-select",
		result: DecisionResult{
			Action:     ActionSelect,
			SelectedID: "A",
			Confidence: 0.9,
		},
		capabilities: Capabilities{CanRank: true, CanSelect: false},
	}
	reg2.Register(mock2)
	orch2 := NewOrchestrator(reg2, config.DecisionConfig{Mode: "local", Provider: "no-select", TimeoutMS: 100}, &Metrics{})
	ordered2, result2, _ := orch2.Decide(context.Background(), req)
	for i := range eligible {
		if ordered2[i].ID != eligible[i].ID {
			t.Fatalf("capability mismatch select should preserve order")
		}
	}
	if result2.Action != ActionAbstain {
		t.Fatalf("should abstain on select capability mismatch")
	}
}

func TestOrchestrator_ProviderHealthUnavailable(t *testing.T) {
	reg := &Registry{providers: map[string]DecisionProvider{}}
	mock := &mockProvider{
		id: "unhealthy",
		result: DecisionResult{
			Action:    ActionRank,
			RankedIDs: []string{"B", "A"},
		},
		healthStatus: HealthUnavailable,
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "unhealthy", TimeoutMS: 100}, &Metrics{})
	eligible := makeEligible("A", "B")
	req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 100 * time.Millisecond, MaxProviderCalls: 1}}
	ordered, result, _ := orch.Decide(context.Background(), req)
	if mock.Calls() != 0 {
		t.Fatalf("unhealthy provider should not be called")
	}
	for i := range eligible {
		if ordered[i].ID != eligible[i].ID {
			t.Fatalf("unhealthy should preserve order")
		}
	}
	found := false
	for _, rc := range result.ReasonCodes {
		if rc == ReasonProviderUnhealthy {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected PROVIDER_UNHEALTHY reason")
	}
}

func TestOrchestrator_BudgetMaxProviderCalls(t *testing.T) {
	reg := &Registry{providers: map[string]DecisionProvider{}}
	mock := &mockProvider{
		id: "budget",
		result: DecisionResult{
			Action:    ActionRank,
			RankedIDs: []string{"B", "A"},
		},
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "budget", TimeoutMS: 100}, &Metrics{})
	eligible := makeEligible("A", "B")
	// Budget with 0 calls
	req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 100 * time.Millisecond, MaxProviderCalls: 0}}
	ordered, result, _ := orch.Decide(context.Background(), req)
	if mock.Calls() != 0 {
		t.Fatalf("budget 0 should not call provider")
	}
	for i := range eligible {
		if ordered[i].ID != eligible[i].ID {
			t.Fatalf("budget exhausted should preserve order")
		}
	}
	found := false
	for _, rc := range result.ReasonCodes {
		if rc == ReasonBudgetExceeded {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected BUDGET_EXCEEDED reason, got %v", result.ReasonCodes)
	}
}

func TestOrchestrator_NaNConfidenceRejected(t *testing.T) {
	reg := &Registry{providers: map[string]DecisionProvider{}}
	mock := &mockProvider{
		id: "nan",
		result: DecisionResult{
			Action:     ActionRank,
			RankedIDs:  []string{"A"},
			Confidence: math.NaN(),
		},
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "nan", TimeoutMS: 100}, &Metrics{})
	eligible := makeEligible("A", "B")
	req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 100 * time.Millisecond, MaxProviderCalls: 1}}
	ordered, result, _ := orch.Decide(context.Background(), req)
	for i := range eligible {
		if ordered[i].ID != eligible[i].ID {
			t.Fatalf("NaN should preserve order")
		}
	}
	if result.Action != ActionAbstain {
		t.Fatalf("NaN should be invalid -> abstain")
	}
}

func TestOrchestrator_InfConfidenceRejected(t *testing.T) {
	for _, conf := range []float64{math.Inf(1), math.Inf(-1)} {
		mock := &mockProvider{
			id: "inf",
			result: DecisionResult{
				Action:     ActionRank,
				RankedIDs:  []string{"A"},
				Confidence: conf,
			},
		}
		reg2 := &Registry{providers: map[string]DecisionProvider{}}
		reg2.Register(mock)
		orch := NewOrchestrator(reg2, config.DecisionConfig{Mode: "local", Provider: "inf", TimeoutMS: 100}, &Metrics{})
		eligible := makeEligible("A", "B")
		req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 100 * time.Millisecond, MaxProviderCalls: 1}}
		ordered, result, _ := orch.Decide(context.Background(), req)
		for i := range eligible {
			if ordered[i].ID != eligible[i].ID {
				t.Fatalf("Inf should preserve order")
			}
		}
		if result.Action != ActionAbstain {
			t.Fatalf("Inf should be invalid -> abstain")
		}
	}
}

func TestOrchestrator_StrictContract(t *testing.T) {
	tests := []struct {
		name   string
		result DecisionResult
	}{
		{"empty action", DecisionResult{Action: "", Confidence: 0.5}},
		{"unknown action", DecisionResult{Action: "FOO", Confidence: 0.5}},
		{"SELECT empty id", DecisionResult{Action: ActionSelect, Confidence: 0.5}},
		{"SELECT with ranked", DecisionResult{Action: ActionSelect, SelectedID: "A", RankedIDs: []string{"A"}, Confidence: 0.5}},
		{"RANK empty ranked", DecisionResult{Action: ActionRank, Confidence: 0.5}},
		{"RANK with selected", DecisionResult{Action: ActionRank, SelectedID: "A", RankedIDs: []string{"A"}, Confidence: 0.5}},
		{"ABSTAIN with selected", DecisionResult{Action: ActionAbstain, SelectedID: "A", Confidence: 0.5}},
		{"ABSTAIN with ranked", DecisionResult{Action: ActionAbstain, RankedIDs: []string{"A"}, Confidence: 0.5}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockProvider{id: "strict", result: tc.result}
			r := &Registry{providers: map[string]DecisionProvider{}}
			r.Register(mock)
			orch := NewOrchestrator(r, config.DecisionConfig{Mode: "local", Provider: "strict", TimeoutMS: 100}, &Metrics{})
			eligible := makeEligible("A", "B")
			req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 100 * time.Millisecond, MaxProviderCalls: 1}}
			ordered, result, _ := orch.Decide(context.Background(), req)
			for i := range eligible {
				if ordered[i].ID != eligible[i].ID {
					t.Fatalf("strict contract violation should preserve order")
				}
			}
			if result.Action != ActionAbstain {
				t.Fatalf("should be abstain on strict violation, got %v", result.Action)
			}
		})
	}
}

func TestOrchestrator_BoundedRankedIDs(t *testing.T) {
	reg := &Registry{providers: map[string]DecisionProvider{}}
	mock := &mockProvider{
		id: "too-many",
		result: DecisionResult{
			Action:     ActionRank,
			RankedIDs:  []string{"A", "B", "C"}, // eligible only 2
			Confidence: 0.5,
		},
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "too-many", TimeoutMS: 100}, &Metrics{})
	eligible := makeEligible("A", "B")
	req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 100 * time.Millisecond, MaxProviderCalls: 1}}
	ordered, result, _ := orch.Decide(context.Background(), req)
	for i := range eligible {
		if ordered[i].ID != eligible[i].ID {
			t.Fatalf("too many ranked should preserve order")
		}
	}
	if result.Action != ActionAbstain {
		t.Fatalf("should abstain when ranked > eligible")
	}
}

func TestOrchestrator_ReasonCodeValidation(t *testing.T) {
	reg := &Registry{providers: map[string]DecisionProvider{}}
	mock := &mockProvider{
		id: "bad-reason",
		result: DecisionResult{
			Action:      ActionRank,
			RankedIDs:   []string{"A"},
			Confidence:  0.5,
			ReasonCodes: []ReasonCode{"ARBITRARY_UNKNOWN_CODE"},
		},
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "bad-reason", TimeoutMS: 100}, &Metrics{})
	eligible := makeEligible("A", "B")
	req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 100 * time.Millisecond, MaxProviderCalls: 1}}
	ordered, result, _ := orch.Decide(context.Background(), req)
	for i := range eligible {
		if ordered[i].ID != eligible[i].ID {
			t.Fatalf("invalid reason code should preserve order")
		}
	}
	if result.Action != ActionAbstain {
		t.Fatalf("invalid reason code should be rejected")
	}
}

func TestOrchestrator_HotReloadRace(t *testing.T) {
	reg := NewRegistry()
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "off", Provider: "local", TimeoutMS: 10}, &Metrics{})

	eligible := makeEligible("A", "B", "C")
	req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 20 * time.Millisecond, MaxProviderCalls: 1}}

	// Concurrent Decide and UpdateConfig
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_, _, _ = orch.Decide(context.Background(), req)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			newCfg := config.DecisionConfig{Mode: "local", Provider: "local", TimeoutMS: 10 + i%10}
			orch.UpdateConfig(newCfg)
			_ = orch.Config()
		}
	}()
	wg.Wait()
	// If race detector is enabled, this test will fail on data race
}

func TestOrchestrator_MetricsIsolation(t *testing.T) {
	reg1 := NewRegistry()
	m1 := &Metrics{}
	orch1 := NewOrchestrator(reg1, config.DecisionConfig{Mode: "local", Provider: "local", TimeoutMS: 10}, m1)

	reg2 := NewRegistry()
	m2 := &Metrics{}
	orch2 := NewOrchestrator(reg2, config.DecisionConfig{Mode: "local", Provider: "local", TimeoutMS: 10}, m2)

	eligible := makeEligible("A", "B")
	req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 10 * time.Millisecond, MaxProviderCalls: 1}}

	_, _, _ = orch1.Decide(context.Background(), req)
	_, _, _ = orch1.Decide(context.Background(), req)

	snap1 := m1.Snapshot()
	snap2 := m2.Snapshot()

	if snap1["decisions_total"] != 2 {
		t.Fatalf("expected 2 decisions in m1, got %d", snap1["decisions_total"])
	}
	if snap2["decisions_total"] != 0 {
		t.Fatalf("m2 should be isolated, got %d", snap2["decisions_total"])
	}

	_, _, _ = orch2.Decide(context.Background(), req)
	snap2 = m2.Snapshot()
	if snap2["decisions_total"] != 1 {
		t.Fatalf("expected 1 in m2 after call")
	}
	snap1 = m1.Snapshot()
	if snap1["decisions_total"] != 2 {
		t.Fatalf("m1 should remain 2 after m2 call, got %d", snap1["decisions_total"])
	}
}

func TestOrchestrator_ContextAware(t *testing.T) {
	reg := &Registry{providers: map[string]DecisionProvider{}}
	// Provider that respects context
	mock := &mockProvider{
		id:    "ctx-aware",
		delay: 100 * time.Millisecond,
		result: DecisionResult{
			Action:    ActionRank,
			RankedIDs: []string{"B", "A"},
		},
	}
	reg.Register(mock)
	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "ctx-aware", TimeoutMS: 50}, &Metrics{})
	eligible := makeEligible("A", "B")
	req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 50 * time.Millisecond, MaxProviderCalls: 1}}

	start := time.Now()
	ordered, result, _ := orch.Decide(context.Background(), req)
	elapsed := time.Since(start)

	// Should timeout around 50ms, not 100ms
	if elapsed > 90*time.Millisecond {
		t.Fatalf("should have timed out early, elapsed %v", elapsed)
	}
	for i := range eligible {
		if ordered[i].ID != eligible[i].ID {
			t.Fatalf("timeout should preserve order")
		}
	}
	found := false
	for _, rc := range result.ReasonCodes {
		if rc == ReasonTimeout {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected timeout")
	}
}
