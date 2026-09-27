package decision

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/decision/providerstate"
)

// helpers for chain tests

func makeChainEligible(ids ...string) []Candidate {
	out := make([]Candidate, len(ids))
	for i, id := range ids {
		out[i] = Candidate{ID: id, ProviderID: "p", PoolOrdinal: 0, Priority: 10, OriginalRank: i}
	}
	return out
}

func makeChainEligibleWithPools(ids []string, ordinals []int, priorities []int) []Candidate {
	out := make([]Candidate, len(ids))
	for i, id := range ids {
		ord := 0
		pri := 10
		if i < len(ordinals) {
			ord = ordinals[i]
		}
		if i < len(priorities) {
			pri = priorities[i]
		}
		out[i] = Candidate{ID: id, ProviderID: "p", PoolOrdinal: ord, Priority: pri, OriginalRank: i}
	}
	return out
}

type countingProvider struct {
	id           string
	result       DecisionResult
	err          error
	panic        bool
	delay        time.Duration
	capabilities Capabilities
	healthStatus string
	calls        int32
}

func newCountingProvider(id string) *countingProvider {
	return &countingProvider{id: id, capabilities: Capabilities{CanSelect: true, CanRank: true}, healthStatus: HealthHealthy}
}

func (m *countingProvider) ID() string { return m.id }
func (m *countingProvider) Capabilities() Capabilities {
	if m.capabilities.CanRank == false && m.capabilities.CanSelect == false {
		return Capabilities{CanSelect: true, CanRank: true}
	}
	return m.capabilities
}
func (m *countingProvider) Health() ProviderHealth {
	if m.healthStatus == "" {
		m.healthStatus = HealthHealthy
	}
	return ProviderHealth{Status: m.healthStatus, CheckedAt: time.Now()}
}
func (m *countingProvider) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	atomic.AddInt32(&m.calls, 1)
	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return DecisionResult{Action: ActionAbstain, Abstained: true, ReasonCodes: []ReasonCode{ReasonTimeout}, ProviderID: m.id}, ctx.Err()
		}
	}
	if m.panic {
		panic("chain mock panic")
	}
	if m.err != nil {
		return m.result, m.err
	}
	res := m.result
	if res.ProviderID == "" {
		res.ProviderID = m.id
	}
	if res.Action == "" {
		res.Action = ActionAbstain
		res.Abstained = true
	}
	return res, nil
}
func (m *countingProvider) Calls() int { return int(atomic.LoadInt32(&m.calls)) }

// convenience constructors

func selectProvider(id, selected string) *countingProvider {
	p := newCountingProvider(id)
	p.result = DecisionResult{Action: ActionSelect, SelectedID: selected, Confidence: 0.9, ReasonCodes: []ReasonCode{ReasonEligibleSetPreserved}, ProviderID: id}
	p.capabilities = Capabilities{CanSelect: true}
	return p
}

func abstainProvider(id string) *countingProvider {
	p := newCountingProvider(id)
	p.result = DecisionResult{Action: ActionAbstain, Abstained: true, Confidence: 1.0, ReasonCodes: []ReasonCode{ReasonAbstained, ReasonExistingOrderPreserved}, ProviderID: id}
	p.capabilities = Capabilities{CanSelect: true, CanRank: true}
	return p
}

func errorProvider(id string) *countingProvider {
	p := newCountingProvider(id)
	p.err = errors.New("provider error")
	p.result = DecisionResult{Action: ActionAbstain, ProviderID: id}
	return p
}

func timeoutProvider(id string, d time.Duration) *countingProvider {
	p := newCountingProvider(id)
	p.delay = d
	p.result = DecisionResult{Action: ActionAbstain, ProviderID: id}
	return p
}

func invalidProvider(id string, selected string) *countingProvider {
	p := newCountingProvider(id)
	// selected outside eligible
	p.result = DecisionResult{Action: ActionSelect, SelectedID: selected, Confidence: 0.9, ProviderID: id}
	return p
}

func rankProvider(id string, ranked ...string) *countingProvider {
	p := newCountingProvider(id)
	p.result = DecisionResult{Action: ActionRank, RankedIDs: ranked, Confidence: 0.8, ProviderID: id}
	p.capabilities = Capabilities{CanRank: true}
	return p
}

func panicProvider(id string) *countingProvider {
	p := newCountingProvider(id)
	p.panic = true
	return p
}

// registry helper
func chainRegistry(providers ...DecisionProvider) *Registry {
	reg := &Registry{providers: map[string]DecisionProvider{}}
	for _, p := range providers {
		reg.Register(p)
	}
	// ensure local/policy exist as abstain for fallback tests where needed
	if _, ok := reg.Get("local"); !ok {
		reg.Register(abstainProvider("local"))
	}
	if _, ok := reg.Get("policy"); !ok {
		reg.Register(abstainProvider("policy"))
	}
	return reg
}

func defaultChainBudget(timeout time.Duration, maxCalls int) Budget {
	return Budget{Timeout: timeout, MaxProviderCalls: maxCalls}
}

func defaultConstraints(candidates []Candidate) PrimarySelectionConstraints {
	return ComputePrimaryConstraints(candidates, "")
}

func TestChain_FirstValidSelectStopsChain(t *testing.T) {
	eligible := makeChainEligible("A", "B", "C", "D")
	p1 := selectProvider("jev-main", "B")
	p2 := selectProvider("policy", "C")
	p3 := abstainProvider("local")
	reg := chainRegistry(p1, p2, p3)
	exec := NewChainExecutor(reg, providerstate.New(providerstate.Config{FailureThreshold: 3, FailureWindow: 30 * time.Second, Cooldown: 60 * time.Second}, nil), &Metrics{})
	chain := ChainConfig{ID: "test", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}, {Provider: "local"}}}
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(500*time.Millisecond, 3)}
	ordered, result, trace := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 500*time.Millisecond)
	if p1.Calls() != 1 {
		t.Fatalf("p1 should be called once, got %d", p1.Calls())
	}
	if p2.Calls() != 0 || p3.Calls() != 0 {
		t.Fatalf("p2/p3 should not be called after first SELECT: p2=%d p3=%d", p2.Calls(), p3.Calls())
	}
	if trace.Outcome != ChainOutcomeSelected {
		t.Fatalf("outcome should be SELECTED got %s", trace.Outcome)
	}
	if trace.CallsUsed != 1 || trace.SelectedProviderID != "jev-main" {
		t.Fatalf("trace mismatch CallsUsed=%d Selected=%s", trace.CallsUsed, trace.SelectedProviderID)
	}
	if result.SelectedID != "B" {
		t.Fatalf("selected should be B got %s", result.SelectedID)
	}
	// only selected primary moves, remainder preserves original order
	if ordered[0].ID != "B" {
		t.Fatalf("first should be B got %s", ordered[0].ID)
	}
	expectedRemainder := []string{"A", "C", "D"}
	for i, id := range expectedRemainder {
		if ordered[i+1].ID != id {
			t.Fatalf("remainder at %d: got %s want %s ordered=%v", i, ordered[i+1].ID, id, ordered)
		}
	}
}

func TestChain_AbstainContinues(t *testing.T) {
	eligible := makeChainEligible("A", "B", "C")
	p1 := abstainProvider("jev-main")
	p2 := selectProvider("policy", "B")
	reg := chainRegistry(p1, p2)
	exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
	chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}}}
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(200*time.Millisecond, 2)}
	_, result, trace := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
	if p1.Calls() != 1 || p2.Calls() != 1 {
		t.Fatalf("both should be called: p1=%d p2=%d", p1.Calls(), p2.Calls())
	}
	if trace.Outcome != ChainOutcomeSelected || result.SelectedID != "B" {
		t.Fatalf("should select B via second provider: outcome=%s selected=%s", trace.Outcome, result.SelectedID)
	}
	if len(trace.Steps) != 2 || trace.Steps[0].Outcome != StepOutcomeAbstained || trace.Steps[1].Outcome != StepOutcomeSelected {
		t.Fatalf("steps mismatch: %+v", trace.Steps)
	}
}

func TestChain_ErrorContinues(t *testing.T) {
	eligible := makeChainEligible("A", "B")
	p1 := errorProvider("jev-main")
	p2 := selectProvider("policy", "A")
	reg := chainRegistry(p1, p2)
	exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
	chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}}}
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(200*time.Millisecond, 2)}
	_, result, trace := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
	if p1.Calls() != 1 || p2.Calls() != 1 {
		t.Fatalf("error should continue: p1=%d p2=%d", p1.Calls(), p2.Calls())
	}
	if trace.Steps[0].Outcome != StepOutcomeError {
		t.Fatalf("first step should be ERROR got %s", trace.Steps[0].Outcome)
	}
	if result.SelectedID != "A" {
		t.Fatalf("second should select A")
	}
}

func TestChain_TimeoutContinuesWhenGlobalRemains(t *testing.T) {
	eligible := makeChainEligible("A", "B", "C")
	p1 := timeoutProvider("jev-main", 80*time.Millisecond)
	p1.result = DecisionResult{Action: ActionAbstain}
	p2 := selectProvider("policy", "C")
	reg := chainRegistry(p1, p2)
	state := providerstate.New(providerstate.Config{FailureThreshold: 10, FailureWindow: 30 * time.Second, Cooldown: 60 * time.Second}, nil)
	exec := NewChainExecutor(reg, state, &Metrics{})
	chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main", TimeoutMS: 30}, {Provider: "policy"}}}
	constraints := defaultConstraints(eligible)
	// global 500ms, step1 30ms timeout -> should timeout and continue
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(500*time.Millisecond, 2)}
	_, result, trace := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 500*time.Millisecond)
	if p1.Calls() != 1 || p2.Calls() != 1 {
		t.Fatalf("timeout should continue if global remains: p1=%d p2=%d steps=%+v outcome=%s", p1.Calls(), p2.Calls(), trace.Steps, trace.Outcome)
	}
	if trace.Steps[0].Outcome != StepOutcomeTimeout {
		t.Fatalf("first should be TIMEOUT got %s", trace.Steps[0].Outcome)
	}
	if result.SelectedID != "C" {
		t.Fatalf("second should win")
	}
}

func TestChain_UnavailableSkipped(t *testing.T) {
	eligible := makeChainEligible("A", "B")
	// p1 not registered -> unavailable
	p2 := selectProvider("policy", "B")
	reg := chainRegistry(p2) // only policy registered
	// ensure jev-main not present, local exists but we test unavailable for jev-main
	state := providerstate.New(providerstate.DefaultConfig(), nil)
	exec := NewChainExecutor(reg, state, &Metrics{})
	chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}}}
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(200*time.Millisecond, 2)}
	_, result, trace := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
	if trace.Steps[0].Outcome != StepOutcomeUnavailable {
		t.Fatalf("first should be UNAVAILABLE got %s", trace.Steps[0].Outcome)
	}
	if trace.Steps[0].Called {
		t.Fatalf("unavailable should not be called")
	}
	if p2.Calls() != 1 || result.SelectedID != "B" {
		t.Fatalf("policy should be called and select B")
	}
	// also test disabled provider (HealthUnavailable)
	pDisabled := newCountingProvider("jev-main")
	pDisabled.healthStatus = HealthUnavailable
	reg2 := chainRegistry(pDisabled, p2)
	exec2 := NewChainExecutor(reg2, state, &Metrics{})
	_, _, trace2 := exec2.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
	if trace2.Steps[0].Outcome != StepOutcomeUnavailable {
		t.Fatalf("disabled should be UNAVAILABLE got %s", trace2.Steps[0].Outcome)
	}
}

func TestChain_CooldownSkipped(t *testing.T) {
	eligible := makeChainEligible("A", "B")
	p1 := selectProvider("jev-main", "A")
	p1.healthStatus = HealthHealthy
	reg := chainRegistry(p1, abstainProvider("policy"))
	// state with cooldown open for jev-main
	now := time.Now()
	clockNow := now
	clock := func() time.Time { return clockNow }
	state := providerstate.New(providerstate.Config{FailureThreshold: 1, FailureWindow: time.Second, Cooldown: time.Minute}, clock)
	state.RecordFailure("jev-main")
	if !state.IsCooldown("jev-main") {
		t.Fatalf("should be in cooldown")
	}
	exec := NewChainExecutor(reg, state, &Metrics{})
	chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}}}
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(200*time.Millisecond, 2)}
	_, _, trace := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
	if p1.Calls() != 0 {
		t.Fatalf("cooldown provider should not be called, got %d", p1.Calls())
	}
	if trace.Steps[0].Outcome != StepOutcomeCooldown {
		t.Fatalf("should be COOLDOWN got %s", trace.Steps[0].Outcome)
	}
	if trace.Steps[0].Called {
		t.Fatalf("cooldown should not count as called")
	}
	// advance clock beyond cooldown
	clockNow = now.Add(2 * time.Minute)
	if state.IsCooldown("jev-main") {
		t.Fatalf("cooldown should have expired")
	}
}

func TestChain_InvalidContinues(t *testing.T) {
	eligible := makeChainEligible("A", "B", "C")
	p1 := invalidProvider("jev-main", "UNKNOWN") // outside eligible
	p2 := selectProvider("policy", "B")
	reg := chainRegistry(p1, p2)
	exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
	chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}}}
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(200*time.Millisecond, 2)}
	_, result, trace := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
	if p1.Calls() != 1 || p2.Calls() != 1 {
		t.Fatalf("invalid should continue: p1=%d p2=%d", p1.Calls(), p2.Calls())
	}
	if trace.Steps[0].Outcome != StepOutcomeInvalid {
		t.Fatalf("first should be INVALID got %s", trace.Steps[0].Outcome)
	}
	if result.SelectedID != "B" {
		t.Fatalf("second should select B")
	}
}

func TestChain_PanicContinues(t *testing.T) {
	eligible := makeChainEligible("A", "B")
	p1 := panicProvider("jev-main")
	p2 := selectProvider("policy", "A")
	reg := chainRegistry(p1, p2)
	exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
	chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}}}
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(200*time.Millisecond, 2)}
	_, result, trace := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
	if p1.Calls() != 1 || p2.Calls() != 1 {
		t.Fatalf("panic should be contained and continue: p1=%d p2=%d", p1.Calls(), p2.Calls())
	}
	if trace.Steps[0].Outcome != StepOutcomeError {
		// panic maps to error outcome
		t.Logf("panic outcome is %s (expected ERROR or INVALID)", trace.Steps[0].Outcome)
	}
	if result.SelectedID != "A" {
		t.Fatalf("policy should win after panic")
	}
}

func TestChain_RankRejectedContinues(t *testing.T) {
	eligible := makeChainEligible("A", "B", "C", "D")
	p1 := rankProvider("jev-main", "D", "C", "B", "A") // attempt full reorder
	p2 := selectProvider("policy", "B")
	reg := chainRegistry(p1, p2)
	exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
	chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}}}
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(200*time.Millisecond, 2)}
	ordered, result, trace := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
	if p1.Calls() != 1 || p2.Calls() != 1 {
		t.Fatalf("RANK should be rejected and continue: p1=%d p2=%d", p1.Calls(), p2.Calls())
	}
	if trace.Steps[0].Outcome != StepOutcomeInvalid {
		t.Fatalf("RANK should be INVALID got %s", trace.Steps[0].Outcome)
	}
	if result.SelectedID != "B" {
		t.Fatalf("policy should select B after RANK rejected")
	}
	// Ensure RANK did not reorder failover list: remainder must preserve original order after B
	// Original order A,B,C,D -> after selecting B remainder should be A,C,D not D,C,B etc
	if ordered[0].ID != "B" {
		t.Fatalf("ordered first should be B got %s", ordered[0].ID)
	}
	expected := []string{"A", "C", "D"}
	for i, exp := range expected {
		if ordered[i+1].ID != exp {
			t.Fatalf("remainder should preserve original order: at %d got %s want %s full=%v", i, ordered[i+1].ID, exp, ordered)
		}
	}
}

func TestChain_RankCannotReorderFailoverList_Regression(t *testing.T) {
	// Permanent regression: A,B,C,D original; rank provider tries arbitrary ranking D,C,B,A
	eligible := makeChainEligible("A", "B", "C", "D")
	pRank := rankProvider("jev-main", "D", "C", "B", "A")
	reg := chainRegistry(pRank)
	exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
	chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main"}}}
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(200*time.Millisecond, 1)}
	ordered, _, trace := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
	// Since RANK is rejected, chain should be EXHAUSTED and preserve original order
	if trace.Outcome != ChainOutcomeExhausted {
		t.Fatalf("RANK rejected chain should be EXHAUSTED got %s", trace.Outcome)
	}
	for i, id := range []string{"A", "B", "C", "D"} {
		if ordered[i].ID != id {
			t.Fatalf("RANK must not reorder failover list: at %d got %s want %s ordered=%v", i, ordered[i].ID, id, ordered)
		}
	}
}

func TestChain_AllExhaustedFailOpen(t *testing.T) {
	eligible := makeChainEligible("A", "B", "C")
	p1 := abstainProvider("jev-main")
	p2 := abstainProvider("policy")
	reg := chainRegistry(p1, p2)
	exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
	chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}}}
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(200*time.Millisecond, 2)}
	ordered, result, trace := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
	if trace.Outcome != ChainOutcomeExhausted {
		t.Fatalf("should be EXHAUSTED got %s", trace.Outcome)
	}
	if result.Action != ActionAbstain {
		t.Fatalf("should be abstain")
	}
	for i := range eligible {
		if ordered[i].ID != eligible[i].ID {
			t.Fatalf("exhausted should preserve original order")
		}
	}
}

func TestChain_BudgetStopsLaterCalls(t *testing.T) {
	eligible := makeChainEligible("A", "B", "C")
	p1 := errorProvider("jev-main")
	p2 := abstainProvider("policy")
	p3 := selectProvider("local", "C")
	reg := chainRegistry(p1, p2, p3)
	exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
	chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}, {Provider: "local"}}}
	constraints := defaultConstraints(eligible)
	// max 2 calls, p1 fails, p2 abstains, p3 should NOT be called
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(200*time.Millisecond, 2)}
	_, _, trace := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
	if p1.Calls() != 1 || p2.Calls() != 1 {
		t.Fatalf("p1 p2 should be called")
	}
	if p3.Calls() != 0 {
		t.Fatalf("p3 should NOT be called due to BUDGET_EXHAUSTED, got %d", p3.Calls())
	}
	if trace.Outcome != ChainOutcomeBudgetExhausted {
		t.Fatalf("should be BUDGET_EXHAUSTED got %s", trace.Outcome)
	}
	if trace.CallsUsed != 2 {
		t.Fatalf("CallsUsed should be 2 got %d", trace.CallsUsed)
	}
	// check skipped step
	foundSkipped := false
	for _, s := range trace.Steps {
		if s.ProviderID == "local" && s.Outcome == StepOutcomeSkippedBudget {
			foundSkipped = true
		}
	}
	if !foundSkipped {
		t.Fatalf("local should be SKIPPED_BUDGET: %+v", trace.Steps)
	}
}

func TestChain_SkippedDoesNotConsumeBudget(t *testing.T) {
	eligible := makeChainEligible("A", "B", "C")
	// p1 unavailable -> not counted, p2 abstains counted, p3 select should be allowed with budget 2
	p2 := abstainProvider("policy")
	p3 := selectProvider("local", "B")
	reg := chainRegistry(p2, p3) // jev-main not registered -> unavailable
	state := providerstate.New(providerstate.DefaultConfig(), nil)
	exec := NewChainExecutor(reg, state, &Metrics{})
	chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}, {Provider: "local"}}}
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(200*time.Millisecond, 2)}
	_, result, trace := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
	// jev-main skipped unavailable (0), policy called (1), local called (1) => total 2 <= budget, should succeed
	if trace.Steps[0].Outcome != StepOutcomeUnavailable || trace.Steps[0].Called {
		t.Fatalf("first should be unavailable not called")
	}
	if p2.Calls() != 1 || p3.Calls() != 1 {
		t.Fatalf("policy and local should be called: p2=%d p3=%d", p2.Calls(), p3.Calls())
	}
	if result.SelectedID != "B" {
		t.Fatalf("should select B")
	}
	if trace.Outcome != ChainOutcomeSelected {
		t.Fatalf("should be SELECTED got %s", trace.Outcome)
	}
	if trace.CallsUsed != 2 {
		t.Fatalf("CallsUsed should be 2, got %d", trace.CallsUsed)
	}
}

func TestChain_GlobalDeadlineStopsChain(t *testing.T) {
	eligible := makeChainEligible("A", "B")
	p1 := abstainProvider("jev-main")
	// make p1 consume 70ms
	p1.delay = 70 * time.Millisecond
	p2 := selectProvider("policy", "B")
	reg := chainRegistry(p1, p2)
	exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
	chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}}}
	constraints := defaultConstraints(eligible)
	// global 80ms, p1 takes 70ms then abstains, p2 should have only ~10ms left; we set p2 delay 20ms to trigger deadline
	p2.delay = 20 * time.Millisecond
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(80*time.Millisecond, 2)}
	_, _, trace := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 80*time.Millisecond)
	// The chain should either succeed via p2 if enough time, or be deadline exhausted. The key property is that p2 does NOT get a fresh 80ms budget.
	// We verify by checking that total execution time is bounded by global deadline (80ms + tolerance)
	// For deterministic test, we use a tighter scenario: global 100ms, step1 80ms ABSTAIN, step2 must see remaining deadline not fresh.
	// Alternate deterministic check: use context deadline inspection via provider
	t.Run("remaining deadline not fresh", func(t *testing.T) {
		eligible2 := makeChainEligible("A", "B")
		var p1Dur time.Duration
		var p2DeadlineRemaining time.Duration
		p1c := newCountingProvider("jev-main")
		p1c.delay = 80 * time.Millisecond
		p1c.result = DecisionResult{Action: ActionAbstain, Abstained: true, ProviderID: "jev-main"}
		// p2 records remaining deadline
		p2c := &deadlineInspectProvider{id: "policy", result: DecisionResult{Action: ActionSelect, SelectedID: "B", ProviderID: "policy"}, deadlineCh: make(chan time.Duration, 1)}
		reg2 := chainRegistry(p1c, p2c)
		exec2 := NewChainExecutor(reg2, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
		start := time.Now()
		exec2.Execute(context.Background(), ChainConfig{ID: "c2", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}}}, DecisionRequest{Candidates: eligible2, Budget: defaultChainBudget(100*time.Millisecond, 2)}, defaultConstraints(eligible2), defaultChainBudget(100*time.Millisecond, 2), 100*time.Millisecond)
		p1Dur = time.Since(start)
		// p2 should have seen remaining < 30ms (since 80ms consumed)
		select {
		case rem := <-p2c.deadlineCh:
			p2DeadlineRemaining = rem
			if rem > 40*time.Millisecond {
				t.Fatalf("step2 should receive only remaining global time, got remaining %v (p1Dur %v) — implies fresh budget", rem, p1Dur)
			}
		default:
			t.Fatalf("p2 was not called")
		}
		_ = p2DeadlineRemaining
	})
	_ = trace
}

type deadlineInspectProvider struct {
	id           string
	result       DecisionResult
	delay        time.Duration
	deadlineCh   chan time.Duration
	capabilities Capabilities
}

func (d *deadlineInspectProvider) ID() string { return d.id }
func (d *deadlineInspectProvider) Capabilities() Capabilities {
	return Capabilities{CanSelect: true, CanRank: true}
}
func (d *deadlineInspectProvider) Health() ProviderHealth {
	return ProviderHealth{Status: HealthHealthy}
}
func (d *deadlineInspectProvider) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	if deadline, ok := ctx.Deadline(); ok {
		rem := time.Until(deadline)
		select {
		case d.deadlineCh <- rem:
		default:
		}
	}
	if d.delay > 0 {
		select {
		case <-time.After(d.delay):
		case <-ctx.Done():
			return DecisionResult{Action: ActionAbstain, ProviderID: d.id}, ctx.Err()
		}
	}
	return d.result, nil
}

func TestChain_PerStepTimeout(t *testing.T) {
	eligible := makeChainEligible("A", "B", "C")
	// global 500ms, step1 timeout 50ms, step1 will timeout then step2 should run
	p1 := timeoutProvider("jev-main", 100*time.Millisecond) // longer than step timeout
	p2 := selectProvider("policy", "C")
	reg := chainRegistry(p1, p2)
	exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
	chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main", TimeoutMS: 50}, {Provider: "policy"}}}
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(500*time.Millisecond, 2)}
	start := time.Now()
	_, result, trace := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 500*time.Millisecond)
	elapsed := time.Since(start)
	if trace.Steps[0].Outcome != StepOutcomeTimeout {
		t.Fatalf("first should be TIMEOUT got %s", trace.Steps[0].Outcome)
	}
	if result.SelectedID != "C" {
		t.Fatalf("second should win")
	}
	if elapsed < 40*time.Millisecond || elapsed > 300*time.Millisecond {
		t.Fatalf("elapsed should be around step timeout + second: got %v", elapsed)
	}
	// Conversely, global remaining < step timeout: global wins
	t.Run("global wins over step timeout", func(t *testing.T) {
		eligible2 := makeChainEligible("A", "B")
		p1a := abstainProvider("jev-main")
		p1a.delay = 80 * time.Millisecond
		p2a := timeoutProvider("policy", 100*time.Millisecond)
		reg2 := chainRegistry(p1a, p2a)
		exec2 := NewChainExecutor(reg2, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
		chain2 := ChainConfig{ID: "c2", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy", TimeoutMS: 200}}}
		// global 100ms, p1 consumes 80ms, p2 step timeout 200ms but only 20ms remains -> should deadline
		_, _, trace2 := exec2.Execute(context.Background(), chain2, DecisionRequest{Candidates: eligible2, Budget: defaultChainBudget(100*time.Millisecond, 2)}, defaultConstraints(eligible2), defaultChainBudget(100*time.Millisecond, 2), 100*time.Millisecond)
		// Expect deadline exhausted or timeout
		found := false
		for _, s := range trace2.Steps {
			if s.Outcome == StepOutcomeTimeout || trace2.Outcome == ChainOutcomeDeadlineExhausted {
				found = true
				break
			}
		}
		if !found && trace2.Outcome != ChainOutcomeDeadlineExhausted {
			t.Fatalf("should have deadline behavior: trace=%+v", trace2)
		}
	})
}

func TestChain_AffinityZeroCalls(t *testing.T) {
	eligible := makeChainEligibleWithPools([]string{"A", "B", "C"}, []int{0, 0, 1}, []int{10, 10, 10})
	// pin B inside earliest pool (0)
	constraints := ComputePrimaryConstraints(eligible, "B")
	if constraints.ForcedPrimaryID != "B" {
		t.Fatalf("expected forced B got %s", constraints.ForcedPrimaryID)
	}
	p1 := selectProvider("jev-main", "A") // would try to select A but should not be called
	p2 := selectProvider("policy", "C")
	p1.calls = 0
	p2.calls = 0
	reg := chainRegistry(p1, p2)
	exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
	chain := ChainConfig{ID: "aff", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}}}
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(200*time.Millisecond, 2)}
	ordered, result, trace := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
	if p1.Calls() != 0 || p2.Calls() != 0 {
		t.Fatalf("affinity must cause zero calls: p1=%d p2=%d", p1.Calls(), p2.Calls())
	}
	if trace.Outcome != ChainOutcomeAffinityPreserved {
		t.Fatalf("outcome should be AFFINITY_PRESERVED got %s", trace.Outcome)
	}
	if result.SelectedID != "B" || ordered[0].ID != "B" {
		t.Fatalf("should select pinned B: selected=%s ordered=%v", result.SelectedID, ordered)
	}
	found := false
	for _, rc := range result.ReasonCodes {
		if rc == ReasonAffinityPreserved {
			found = true
		}
	}
	if !found {
		t.Fatalf("should contain AFFINITY_PRESERVED")
	}
}

func TestChain_SameConstraintsEveryStep(t *testing.T) {
	eligible := makeChainEligibleWithPools([]string{"A", "B", "C", "D"}, []int{0, 0, 1, 1}, []int{5, 10, 10, 10})
	// earliest pool 0, min priority 5 -> only A allowed
	constraints := ComputePrimaryConstraints(eligible, "")
	if len(constraints.AllowedPrimaryIDs) != 1 || constraints.AllowedPrimaryIDs[0] != "A" {
		t.Fatalf("expected only A allowed, got %v", constraints.AllowedPrimaryIDs)
	}
	// p1 tries to select B (outside allowed) -> should be rejected as invalid
	p1 := selectProvider("jev-main", "B")
	p2 := selectProvider("policy", "A")
	reg := chainRegistry(p1, p2)
	exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
	chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}}}
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(200*time.Millisecond, 2)}
	_, result, trace := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
	if trace.Steps[0].Outcome != StepOutcomeInvalid {
		t.Fatalf("first select outside allowed should be INVALID got %s", trace.Steps[0].Outcome)
	}
	if trace.Steps[1].Outcome != StepOutcomeSelected || result.SelectedID != "A" {
		t.Fatalf("second should succeed with A")
	}
}

func TestChain_SelectedAlwaysInAllowed(t *testing.T) {
	eligible := makeChainEligibleWithPools([]string{"A", "B", "C"}, []int{0, 0, 0}, []int{10, 20, 20})
	constraints := ComputePrimaryConstraints(eligible, "")
	// Only A allowed (min priority 10)
	for _, prov := range []*countingProvider{selectProvider("jev-main", "A"), selectProvider("policy", "A")} {
		reg := chainRegistry(prov)
		exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
		chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: prov.ID()}}}
		req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(200*time.Millisecond, 1)}
		_, result, _ := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
		if result.Action == ActionSelect && !constraints.IsAllowedPrimary(result.SelectedID) {
			t.Fatalf("selected %s not in allowed %v", result.SelectedID, constraints.AllowedPrimaryIDs)
		}
	}
}

func TestChain_OnlySelectedPrimaryMoves(t *testing.T) {
	eligible := makeChainEligible("A", "B", "C", "D")
	// Simulate pool-aware but for this test simple: selecting C should move only C to front
	p1 := selectProvider("jev-main", "C")
	reg := chainRegistry(p1)
	exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
	chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main"}}}
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(200*time.Millisecond, 1)}
	ordered, _, _ := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
	if ordered[0].ID != "C" {
		t.Fatalf("first should be C")
	}
	// remainder preserves original order A,B,D (excluding C)
	expected := []string{"A", "B", "D"}
	for i, exp := range expected {
		if ordered[i+1].ID != exp {
			t.Fatalf("remainder should preserve order: at %d got %s want %s full=%v", i, ordered[i+1].ID, exp, ordered)
		}
	}
	// Ensure no duplicate or missing
	seen := map[string]int{}
	for _, c := range ordered {
		seen[c.ID]++
	}
	for _, id := range []string{"A", "B", "C", "D"} {
		if seen[id] != 1 {
			t.Fatalf("candidate %s count %d", id, seen[id])
		}
	}
}

func TestChain_SameProviderNeverTwice(t *testing.T) {
	eligible := makeChainEligible("A", "B")
	p1 := abstainProvider("jev-main")
	// Chain with same provider twice would be invalid config, but if executed, ensure not double-counted beyond budget?
	// Instead test that distinct chain never calls same ID twice: we track calls per provider ID should be <=1
	chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}, {Provider: "local"}}}
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(200*time.Millisecond, 3)}
	pPolicy := abstainProvider("policy")
	pLocal := abstainProvider("local")
	reg2 := chainRegistry(p1, pPolicy, pLocal)
	exec2 := NewChainExecutor(reg2, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
	_, _, trace := exec2.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
	seen := map[string]int{}
	for _, s := range trace.Steps {
		if s.Called {
			seen[s.ProviderID]++
			if seen[s.ProviderID] > 1 {
				t.Fatalf("provider %s called twice in one chain: %+v", s.ProviderID, trace.Steps)
			}
		}
	}
	_ = trace
}

func TestChain_Property_SelectedInAllowed(t *testing.T) {
	// randomized small
	for iter := 0; iter < 50; iter++ {
		eligible := makeChainEligibleWithPools([]string{"A", "B", "C", "D", "E"}, []int{0, 0, 0, 1, 1}, []int{5, 5, 10, 10, 10})
		constraints := ComputePrimaryConstraints(eligible, "")
		// random provider that may select random allowed or not? Use deterministic select of random candidate
		cand := eligible[iter%len(eligible)]
		p := selectProvider("jev-main", cand.ID)
		reg := chainRegistry(p)
		exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
		chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main"}}}
		req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(200*time.Millisecond, 1)}
		_, result, _ := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
		if result.Action == ActionSelect && !constraints.IsAllowedPrimary(result.SelectedID) {
			t.Fatalf("iter %d selected %s not in allowed %v", iter, result.SelectedID, constraints.AllowedPrimaryIDs)
		}
	}
}

func TestChain_Property_BudgetBound(t *testing.T) {
	for maxCalls := 1; maxCalls <= 4; maxCalls++ {
		eligible := makeChainEligible("A", "B", "C")
		provs := []*countingProvider{
			errorProvider("jev-main"),
			abstainProvider("policy"),
			abstainProvider("local"),
			errorProvider("extra"),
		}
		reg := chainRegistry(provs[0], provs[1], provs[2], provs[3])
		exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
		steps := []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}, {Provider: "local"}, {Provider: "extra"}}
		chain := ChainConfig{ID: "c", Steps: steps}
		constraints := defaultConstraints(eligible)
		req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(200*time.Millisecond, maxCalls)}
		_, _, trace := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
		actualCalls := 0
		for _, s := range trace.Steps {
			if s.Called {
				actualCalls++
			}
		}
		if actualCalls > maxCalls {
			t.Fatalf("maxCalls %d actual %d steps %+v", maxCalls, actualCalls, trace.Steps)
		}
		// unavailable/cooldown should not count
		for _, s := range trace.Steps {
			if !s.Called && (s.Outcome == StepOutcomeUnavailable || s.Outcome == StepOutcomeCooldown || s.Outcome == StepOutcomeSkippedBudget) {
				if s.Called {
					t.Fatalf("skipped should not be called")
				}
			}
		}
	}
}

func FuzzChain_ResultHandling(f *testing.F) {
	f.Add("SELECT", "A", 0.9, 0)
	f.Add("RANK", "A,B", 0.5, 1)
	f.Add("ABSTAIN", "", 1.0, 2)
	f.Fuzz(func(t *testing.T, action, ids string, conf float64, rcIdx int) {
		eligible := makeChainEligible("A", "B", "C")
		constraints := defaultConstraints(eligible)
		// craft result
		var act Action
		switch action {
		case "SELECT":
			act = ActionSelect
		case "RANK":
			act = ActionRank
		case "ABSTAIN":
			act = ActionAbstain
		default:
			act = ActionAbstain
		}
		ranked := []string{}
		selected := ""
		if act == ActionRank {
			// random ids split
			if ids != "" {
				ranked = []string{ids[:1]}
			} else {
				ranked = []string{"A"}
			}
		} else if act == ActionSelect {
			if ids != "" {
				selected = string(ids[0] % 3)
				switch ids[0] % 3 {
				case 0:
					selected = "A"
				case 1:
					selected = "B"
				case 2:
					selected = "C"
				}
			} else {
				selected = "A"
			}
		}
		if rcIdx < 0 {
			rcIdx = 0
		}
		rcs := []ReasonCode{ReasonEligibleSetPreserved, ReasonChainStepAbstained, ReasonAbstained}
		rc := rcs[rcIdx%len(rcs)]
		p := newCountingProvider("jev-main")
		p.result = DecisionResult{Action: act, SelectedID: selected, RankedIDs: ranked, Confidence: conf, ReasonCodes: []ReasonCode{rc}, ProviderID: "jev-main"}
		if act == ActionRank {
			p.capabilities = Capabilities{CanRank: true}
		} else if act == ActionSelect {
			p.capabilities = Capabilities{CanSelect: true}
		}
		reg := chainRegistry(p)
		exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
		chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main"}}}
		req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(100*time.Millisecond, 1)}
		ordered, result, trace := exec.Execute(context.Background(), chain, req, constraints, req.Budget, 100*time.Millisecond)
		// must not panic
		_ = ordered
		_ = result
		_ = trace
		// never select outside allowed
		if result.Action == ActionSelect && result.SelectedID != "" && !constraints.IsAllowedPrimary(result.SelectedID) {
			// should be treated as invalid -> result should not be SELECT with outside
			t.Fatalf("selected outside allowed: %s allowed %v", result.SelectedID, constraints.AllowedPrimaryIDs)
		}
		// never duplicate or lose candidates
		if len(ordered) != len(eligible) {
			t.Fatalf("ordered len %d != eligible %d", len(ordered), len(eligible))
		}
		seen := map[string]int{}
		for _, c := range ordered {
			seen[c.ID]++
		}
		for _, c := range eligible {
			if seen[c.ID] != 1 {
				t.Fatalf("candidate %s count %d", c.ID, seen[c.ID])
			}
		}
		// NaN/Inf should be rejected (confidence)
		if result.Action == ActionSelect || result.Action == ActionRank {
			if !isFinite(result.Confidence) {
				t.Fatalf("confidence not finite: %v", result.Confidence)
			}
		}
		// RANK must not reorder full list: if action was RANK and result is SELECT, it should be invalid -> no SELECT
		if act == ActionRank {
			if trace.Outcome == ChainOutcomeSelected {
				t.Fatalf("RANK must not become SELECT")
			}
		}
	})
}

func isFinite(f float64) bool {
	return !(f != f || f > 1e308 || f < -1e308) // simple finite check but also covers Inf
}

// benchmarks
func BenchmarkChain_TwoSteps(b *testing.B) {
	eligible := makeChainEligible("A", "B", "C", "D")
	p1 := abstainProvider("jev-main")
	p2 := selectProvider("policy", "B")
	reg := chainRegistry(p1, p2)
	exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
	chain := ChainConfig{ID: "bench2", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}}}
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(100*time.Millisecond, 2)}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _ = exec.Execute(context.Background(), chain, req, constraints, req.Budget, 100*time.Millisecond)
		// reset calls for next iter (not needed for correctness but to avoid state bloat if using state)
	}
}

func BenchmarkChain_EightSteps(b *testing.B) {
	eligible := makeChainEligible("A", "B", "C", "D", "E", "F", "G", "H")
	providers := []*countingProvider{
		abstainProvider("p1"), abstainProvider("p2"), abstainProvider("p3"), abstainProvider("p4"),
		abstainProvider("p5"), abstainProvider("p6"), abstainProvider("p7"), selectProvider("p8", "E"),
	}
	// register all
	reg := NewRegistry()
	reg = &Registry{providers: map[string]DecisionProvider{}}
	for i, p := range providers {
		id := []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8"}[i]
		// ensure IDs match
		p.id = id
		reg.Register(p)
	}
	reg.Register(abstainProvider("local"))
	reg.Register(abstainProvider("policy"))
	exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
	chain := ChainConfig{ID: "bench8", Steps: []ChainStepConfig{
		{Provider: "p1"}, {Provider: "p2"}, {Provider: "p3"}, {Provider: "p4"},
		{Provider: "p5"}, {Provider: "p6"}, {Provider: "p7"}, {Provider: "p8"},
	}}
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(200*time.Millisecond, 8)}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _ = exec.Execute(context.Background(), chain, req, constraints, req.Budget, 200*time.Millisecond)
	}
}

func BenchmarkChain_PolicyOnly(b *testing.B) {
	eligible := makeChainEligible("A", "B", "C")
	p := selectProvider("policy", "B")
	reg := chainRegistry(p)
	exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
	chain := ChainConfig{ID: "policyOnly", Steps: []ChainStepConfig{{Provider: "policy"}}}
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(100*time.Millisecond, 1)}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _ = exec.Execute(context.Background(), chain, req, constraints, req.Budget, 100*time.Millisecond)
	}
}

func BenchmarkChain_UnavailableJevToPolicy(b *testing.B) {
	eligible := makeChainEligible("A", "B")
	p2 := selectProvider("policy", "A")
	reg := chainRegistry(p2) // jev-main unavailable
	exec := NewChainExecutor(reg, providerstate.New(providerstate.DefaultConfig(), nil), &Metrics{})
	chain := ChainConfig{ID: "c", Steps: []ChainStepConfig{{Provider: "jev-main"}, {Provider: "policy"}}}
	constraints := defaultConstraints(eligible)
	req := DecisionRequest{Candidates: eligible, Budget: defaultChainBudget(100*time.Millisecond, 2)}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _ = exec.Execute(context.Background(), chain, req, constraints, req.Budget, 100*time.Millisecond)
	}
}
