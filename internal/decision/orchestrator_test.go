package decision

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type mockProvider struct {
	id     string
	typ    string
	calls  int
	result DecisionResult
	err    error
}

func (m *mockProvider) ID() string   { return m.id }
func (m *mockProvider) Type() string { return m.typ }
func (m *mockProvider) Capabilities() Capabilities {
	return Capabilities{CanSelect: true}
}
func (m *mockProvider) Health() Health { return Health{Available: true} }
func (m *mockProvider) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	m.calls++
	if m.err != nil {
		return DecisionResult{}, m.err
	}
	return m.result, nil
}

func orchCandidates() []Candidate {
	return []Candidate{cand("A", 0, 0), cand("B", 0, 0), cand("C", 1, 0)}
}

func TestOrchestratorDisabledMakesNoCall(t *testing.T) {
	m := &mockProvider{id: "x", typ: "jev"}
	o := NewOrchestrator(ModeOff, m, time.Second)
	out := o.Decide(context.Background(), orchCandidates(), "", RequestFeatures{}, "r1")
	if m.calls != 0 {
		t.Fatalf("calls=%d, want 0", m.calls)
	}
	if out.ExternalOutcome != OutcomeDisabled {
		t.Fatalf("outcome=%q", out.ExternalOutcome)
	}
	if !reflect.DeepEqual(out.FinalOrder, []string{"A", "B", "C"}) {
		t.Fatalf("order=%v", out.FinalOrder)
	}
}

func TestOrchestratorAffinityShortCircuit(t *testing.T) {
	m := &mockProvider{id: "jev-main", typ: "jev"}
	o := NewOrchestrator(ModeAssisted, m, time.Second)
	out := o.Decide(context.Background(), orchCandidates(), "B", RequestFeatures{}, "r1")
	if m.calls != 0 {
		t.Fatalf("provider calls=%d, want 0 (affinity must prevent the call)", m.calls)
	}
	if out.ProviderCalled || out.ProviderCalls != 0 {
		t.Fatalf("outcome reports a call: %+v", out)
	}
	if !reflect.DeepEqual(out.ReasonCodes, []ReasonCode{ReasonAffinityPreserved}) {
		t.Fatalf("reasons=%v", out.ReasonCodes)
	}
	if !reflect.DeepEqual(out.FinalOrder, []string{"A", "B", "C"}) {
		t.Fatalf("order=%v, want router order preserved", out.FinalOrder)
	}
}

func TestOrchestratorSingleChoiceSkip(t *testing.T) {
	m := &mockProvider{id: "jev-main", typ: "jev"}
	o := NewOrchestrator(ModeAssisted, m, time.Second)
	// Single allowed primary: A alone in the earliest pool/tier.
	in := []Candidate{cand("A", 0, 0), cand("C", 1, 0)}
	out := o.Decide(context.Background(), in, "", RequestFeatures{}, "r1")
	if m.calls != 0 {
		t.Fatalf("calls=%d, want 0", m.calls)
	}
	if out.ExternalOutcome != OutcomeSkippedSingle {
		t.Fatalf("outcome=%q", out.ExternalOutcome)
	}
}

func TestOrchestratorValidSelectionReordersPrimaryOnly(t *testing.T) {
	m := &mockProvider{id: "jev-main", typ: "jev", result: DecisionResult{
		Action: ActionSelect, SelectedID: "B", Confidence: 0.9,
		ReasonCodes: []ReasonCode{ReasonExternalSelected},
	}}
	o := NewOrchestrator(ModeAssisted, m, time.Second)
	out := o.Decide(context.Background(), orchCandidates(), "", RequestFeatures{}, "r1")
	if m.calls != 1 {
		t.Fatalf("calls=%d, want exactly 1", m.calls)
	}
	if !reflect.DeepEqual(out.FinalOrder, []string{"B", "A", "C"}) {
		t.Fatalf("order=%v, want [B A C]", out.FinalOrder)
	}
	if out.SelectedID != "B" || out.Confidence != 0.9 {
		t.Fatalf("outcome=%+v", out)
	}
	if out.ExternalOutcome != OutcomeSelected {
		t.Fatalf("outcome=%q", out.ExternalOutcome)
	}
}

func TestOrchestratorRejectsFallbackLeapfrog(t *testing.T) {
	// §53: C is eligible but in the fallback pool; selecting it must fail.
	m := &mockProvider{id: "jev-main", typ: "jev", result: DecisionResult{
		Action: ActionSelect, SelectedID: "C",
		ReasonCodes: []ReasonCode{ReasonExternalSelected},
	}}
	o := NewOrchestrator(ModeAssisted, m, time.Second)
	out := o.Decide(context.Background(), orchCandidates(), "", RequestFeatures{}, "r1")
	if !reflect.DeepEqual(out.FinalOrder, []string{"A", "B", "C"}) {
		t.Fatalf("order=%v, want verbatim [A B C]", out.FinalOrder)
	}
	if !reflect.DeepEqual(out.ReasonCodes, []ReasonCode{ReasonPrimaryConstraintViolation}) {
		t.Fatalf("reasons=%v", out.ReasonCodes)
	}
	if out.SelectedID != "" {
		t.Fatalf("selected=%q, want empty", out.SelectedID)
	}
}

func TestOrchestratorRejectsLowerPriorityLeapfrog(t *testing.T) {
	// §54: B is eligible but outside the minimum priority tier.
	m := &mockProvider{id: "jev-main", typ: "jev", result: DecisionResult{
		Action: ActionSelect, SelectedID: "B",
		ReasonCodes: []ReasonCode{ReasonExternalSelected},
	}}
	o := NewOrchestrator(ModeAssisted, m, time.Second)
	in := []Candidate{cand("A", 0, 0), cand("B", 0, 10)}
	out := o.Decide(context.Background(), in, "", RequestFeatures{}, "r1")
	// Only A competes, so the provider must not even be consulted.
	if m.calls != 0 {
		t.Fatalf("calls=%d, want 0 (single-choice skip)", m.calls)
	}
	if !reflect.DeepEqual(out.FinalOrder, []string{"A", "B"}) {
		t.Fatalf("order=%v", out.FinalOrder)
	}
}

func TestOrchestratorPriorityBandViolationFailsOpen(t *testing.T) {
	// Same-tier A,B plus lower-tier D: selecting D must be rejected even
	// though the provider was legitimately consulted for A vs B.
	m := &mockProvider{id: "jev-main", typ: "jev", result: DecisionResult{
		Action: ActionSelect, SelectedID: "D",
		ReasonCodes: []ReasonCode{ReasonExternalSelected},
	}}
	o := NewOrchestrator(ModeAssisted, m, time.Second)
	in := []Candidate{cand("A", 0, 0), cand("B", 0, 0), cand("D", 0, 5)}
	out := o.Decide(context.Background(), in, "", RequestFeatures{}, "r1")
	if m.calls != 1 {
		t.Fatalf("calls=%d, want 1", m.calls)
	}
	if !reflect.DeepEqual(out.FinalOrder, []string{"A", "B", "D"}) {
		t.Fatalf("order=%v", out.FinalOrder)
	}
	if !reflect.DeepEqual(out.ReasonCodes, []ReasonCode{ReasonPrimaryConstraintViolation}) {
		t.Fatalf("reasons=%v", out.ReasonCodes)
	}
}

func TestOrchestratorAbstainKeepsOrder(t *testing.T) {
	m := &mockProvider{id: "jev-main", typ: "jev", result: DecisionResult{
		Action: ActionAbstain, ReasonCodes: []ReasonCode{ReasonExternalAbstained},
	}}
	o := NewOrchestrator(ModeAssisted, m, time.Second)
	out := o.Decide(context.Background(), orchCandidates(), "", RequestFeatures{}, "r1")
	if !reflect.DeepEqual(out.FinalOrder, []string{"A", "B", "C"}) {
		t.Fatalf("order=%v", out.FinalOrder)
	}
	if out.ExternalOutcome != OutcomeAbstained {
		t.Fatalf("outcome=%q", out.ExternalOutcome)
	}
}

type codedErr struct{ reason ReasonCode }

func (e codedErr) Error() string              { return "typed" }
func (e codedErr) DecisionReason() ReasonCode { return e.reason }

func TestOrchestratorProviderErrorFailsOpen(t *testing.T) {
	m := &mockProvider{id: "jev-main", typ: "jev", err: codedErr{ReasonExternalHTTPError}}
	o := NewOrchestrator(ModeAssisted, m, time.Second)
	out := o.Decide(context.Background(), orchCandidates(), "", RequestFeatures{}, "r1")
	if m.calls != 1 {
		t.Fatalf("calls=%d, want exactly 1 (no retry)", m.calls)
	}
	if !reflect.DeepEqual(out.FinalOrder, []string{"A", "B", "C"}) {
		t.Fatalf("order=%v", out.FinalOrder)
	}
	if out.ExternalOutcome != OutcomeError {
		t.Fatalf("outcome=%q", out.ExternalOutcome)
	}
}

func TestOrchestratorUnknownErrorMapsToProviderError(t *testing.T) {
	m := &mockProvider{id: "jev-main", typ: "jev", err: errors.New("boom")}
	o := NewOrchestrator(ModeAssisted, m, time.Second)
	out := o.Decide(context.Background(), orchCandidates(), "", RequestFeatures{}, "r1")
	if !reflect.DeepEqual(out.ReasonCodes, []ReasonCode{ReasonProviderError}) {
		t.Fatalf("reasons=%v", out.ReasonCodes)
	}
	if !reflect.DeepEqual(out.FinalOrder, []string{"A", "B", "C"}) {
		t.Fatalf("order=%v", out.FinalOrder)
	}
}

func TestOrchestratorTimeoutFailsOpen(t *testing.T) {
	m := &mockProvider{id: "jev-main", typ: "jev", err: context.DeadlineExceeded}
	o := NewOrchestrator(ModeAssisted, m, time.Second)
	out := o.Decide(context.Background(), orchCandidates(), "", RequestFeatures{}, "r1")
	if !reflect.DeepEqual(out.ReasonCodes, []ReasonCode{ReasonExternalTimeout}) {
		t.Fatalf("reasons=%v", out.ReasonCodes)
	}
	if out.ExternalOutcome != OutcomeTimeout {
		t.Fatalf("outcome=%q", out.ExternalOutcome)
	}
}

func TestOrchestratorEnforcesTimeout(t *testing.T) {
	m := &blockingProvider{id: "jev-main", typ: "jev"}
	o := NewOrchestrator(ModeAssisted, m, 20*time.Millisecond)
	start := time.Now()
	out := o.Decide(context.Background(), orchCandidates(), "", RequestFeatures{}, "r1")
	if time.Since(start) > 5*time.Second {
		t.Fatal("orchestrator did not enforce its timeout")
	}
	if !reflect.DeepEqual(out.FinalOrder, []string{"A", "B", "C"}) {
		t.Fatalf("order=%v", out.FinalOrder)
	}
	if m.calls != 1 {
		t.Fatalf("calls=%d, want 1", m.calls)
	}
}

type blockingProvider struct {
	id    string
	typ   string
	calls int
}

func (m *blockingProvider) ID() string   { return m.id }
func (m *blockingProvider) Type() string { return m.typ }
func (m *blockingProvider) Capabilities() Capabilities {
	return Capabilities{CanSelect: true}
}
func (m *blockingProvider) Health() Health { return Health{Available: true} }
func (m *blockingProvider) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	m.calls++
	<-ctx.Done()
	return DecisionResult{}, ctx.Err()
}

func TestOrchestratorPriorityAffinityCase(t *testing.T) {
	// §55: A(prio 0), B(prio 10), session pinned to B. The provider must not
	// be called and B stays primary via the existing order.
	m := &mockProvider{id: "jev-main", typ: "jev", result: DecisionResult{
		Action: ActionSelect, SelectedID: "A",
		ReasonCodes: []ReasonCode{ReasonExternalSelected},
	}}
	o := NewOrchestrator(ModeAssisted, m, time.Second)
	// Router order with the pin honored: B first.
	in := []Candidate{cand("B", 0, 10), cand("A", 0, 0)}
	out := o.Decide(context.Background(), in, "B", RequestFeatures{}, "r1")
	if m.calls != 0 {
		t.Fatalf("calls=%d, want 0", m.calls)
	}
	if !reflect.DeepEqual(out.FinalOrder, []string{"B", "A"}) {
		t.Fatalf("order=%v, want [B A]", out.FinalOrder)
	}
	if !reflect.DeepEqual(out.ReasonCodes, []ReasonCode{ReasonAffinityPreserved}) {
		t.Fatalf("reasons=%v", out.ReasonCodes)
	}
}
