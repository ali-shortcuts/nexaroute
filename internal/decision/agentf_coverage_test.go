package decision

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
	"github.com/ali-shortcuts/nexaroute/internal/decision/providerstate"
)

// Agent F coverage: branches left uncovered in the 2026-11-06 baseline.

// TestAgentFPrimaryConstraintsMinScan verifies the min scans find a minimum
// that is not the first element, for both PoolOrdinal and Priority.
func TestAgentFPrimaryConstraintsMinScan(t *testing.T) {
	cands := []Candidate{
		{ID: "a", PoolOrdinal: 2, Priority: 30, OriginalRank: 0},
		{ID: "b", PoolOrdinal: 0, Priority: 20, OriginalRank: 1},
		{ID: "c", PoolOrdinal: 0, Priority: 10, OriginalRank: 2},
		{ID: "d", PoolOrdinal: 1, Priority: 5, OriginalRank: 3},
	}
	got := ComputePrimaryConstraints(cands, "")
	if got.MinOrdinal != 0 {
		t.Fatalf("MinOrdinal = %d, want 0", got.MinOrdinal)
	}
	if got.MinPriority != 10 {
		t.Fatalf("MinPriority = %d, want 10", got.MinPriority)
	}
	if len(got.AllowedPrimaryIDs) != 1 || got.AllowedPrimaryIDs[0] != "c" {
		t.Fatalf("AllowedPrimaryIDs = %v, want [c]", got.AllowedPrimaryIDs)
	}
	if !got.IsAllowedPrimary("c") || got.IsAllowedPrimary("b") {
		t.Fatalf("IsAllowedPrimary inconsistent with allowed set %v", got.AllowedPrimaryIDs)
	}
}

// TestAgentFIsAbstain documents IsAbstain semantics after removing the
// unreachable second ActionAbstain branch: only an explicit ABSTAIN action
// abstains; the Abstained flag alone never does.
func TestAgentFIsAbstain(t *testing.T) {
	cases := []struct {
		name   string
		result DecisionResult
		want   bool
	}{
		{"explicit abstain", DecisionResult{Action: ActionAbstain, Abstained: true}, true},
		{"abstain action without flag", DecisionResult{Action: ActionAbstain}, true},
		{"flag with empty action", DecisionResult{Abstained: true}, false},
		{"flag with select action", DecisionResult{Action: ActionSelect, Abstained: true, SelectedID: "a"}, false},
		{"plain select", DecisionResult{Action: ActionSelect, SelectedID: "a"}, false},
	}
	for _, tc := range cases {
		if got := tc.result.IsAbstain(); got != tc.want {
			t.Errorf("%s: IsAbstain() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestAgentFChainCapabilityRankRejectedWithState covers the CanRank capability
// rejection in the chain when metrics and provider state are wired: the step
// is invalid, the failure is recorded, and the chain exhausts fail-open.
func TestAgentFChainCapabilityRankRejectedWithState(t *testing.T) {
	p := newCountingProvider("jev")
	p.capabilities = Capabilities{CanSelect: true, CanRank: false}
	p.result = DecisionResult{Action: ActionRank, RankedIDs: []string{"b", "a"}, Confidence: 0.8, ProviderID: "jev"}
	reg := chainRegistry(p)
	metrics := &Metrics{}
	state := providerstate.New(providerstate.DefaultConfig(), nil)
	exec := NewChainExecutor(reg, state, metrics)
	cands := makeChainEligible("a", "b")
	constraints := ComputePrimaryConstraints(cands, "")
	ordered, result, trace := exec.Execute(context.Background(),
		ChainConfig{ID: "c1", Steps: []ChainStepConfig{{Provider: "jev"}}},
		DecisionRequest{Candidates: cands}, constraints,
		Budget{Timeout: time.Second, MaxProviderCalls: 1}, time.Second)
	if trace.Outcome != ChainOutcomeExhausted {
		t.Fatalf("outcome = %q, want %q", trace.Outcome, ChainOutcomeExhausted)
	}
	if len(trace.Steps) != 1 || trace.Steps[0].Outcome != StepOutcomeInvalid {
		t.Fatalf("steps = %+v, want one INVALID step", trace.Steps)
	}
	if !result.IsAbstain() {
		t.Fatalf("result = %+v, want abstain (fail-open)", result)
	}
	if len(ordered) != 2 || ordered[0].ID != "a" || ordered[1].ID != "b" {
		t.Fatalf("ordered = %v, want original order preserved", ordered)
	}
	if got := metrics.Snapshot()["chain_step_invalid"]; got != 1 {
		t.Fatalf("chain_step_invalid = %d, want 1", got)
	}
	st, ok := state.SnapshotOne("jev")
	if !ok || st.ConsecutiveFailures != 1 {
		t.Fatalf("provider state = %+v ok=%v, want 1 consecutive failure", st, ok)
	}
}

// TestAgentFChainCapabilitySelectRejectedWithState covers the CanSelect
// capability rejection in the chain with metrics and state wired.
func TestAgentFChainCapabilitySelectRejectedWithState(t *testing.T) {
	p := newCountingProvider("jev")
	p.capabilities = Capabilities{CanSelect: false, CanRank: true}
	p.result = DecisionResult{Action: ActionSelect, SelectedID: "a", Confidence: 0.9, ProviderID: "jev"}
	reg := chainRegistry(p)
	metrics := &Metrics{}
	state := providerstate.New(providerstate.DefaultConfig(), nil)
	exec := NewChainExecutor(reg, state, metrics)
	cands := makeChainEligible("a", "b")
	constraints := ComputePrimaryConstraints(cands, "")
	_, result, trace := exec.Execute(context.Background(),
		ChainConfig{ID: "c1", Steps: []ChainStepConfig{{Provider: "jev"}}},
		DecisionRequest{Candidates: cands}, constraints,
		Budget{Timeout: time.Second, MaxProviderCalls: 1}, time.Second)
	if trace.Outcome != ChainOutcomeExhausted {
		t.Fatalf("outcome = %q, want %q", trace.Outcome, ChainOutcomeExhausted)
	}
	if len(trace.Steps) != 1 || trace.Steps[0].Outcome != StepOutcomeInvalid {
		t.Fatalf("steps = %+v, want one INVALID step", trace.Steps)
	}
	if !result.IsAbstain() {
		t.Fatalf("result = %+v, want abstain (fail-open)", result)
	}
	if got := metrics.Snapshot()["chain_step_invalid"]; got != 1 {
		t.Fatalf("chain_step_invalid = %d, want 1", got)
	}
	st, ok := state.SnapshotOne("jev")
	if !ok || st.ConsecutiveFailures != 1 {
		t.Fatalf("provider state = %+v ok=%v, want 1 consecutive failure", st, ok)
	}
}

// TestAgentFChainSelectAlreadyCarriesChainSelected verifies a SELECT whose
// reason codes already contain ReasonChainSelected is returned without
// duplicating the code.
func TestAgentFChainSelectAlreadyCarriesChainSelected(t *testing.T) {
	p := selectProvider("local", "b")
	p.result.ReasonCodes = []ReasonCode{ReasonEligibleSetPreserved, ReasonChainSelected}
	reg := chainRegistry(p)
	exec := NewChainExecutor(reg, nil, nil)
	cands := makeChainEligible("a", "b")
	constraints := ComputePrimaryConstraints(cands, "")
	ordered, result, trace := exec.Execute(context.Background(),
		ChainConfig{ID: "c1", Steps: []ChainStepConfig{{Provider: "local"}}},
		DecisionRequest{Candidates: cands}, constraints,
		Budget{Timeout: time.Second, MaxProviderCalls: 1}, time.Second)
	if trace.Outcome != ChainOutcomeSelected {
		t.Fatalf("outcome = %q, want %q", trace.Outcome, ChainOutcomeSelected)
	}
	if result.SelectedID != "b" {
		t.Fatalf("SelectedID = %q, want b", result.SelectedID)
	}
	if len(ordered) != 2 || ordered[0].ID != "b" {
		t.Fatalf("ordered = %v, want b first", ordered)
	}
	count := 0
	for _, rc := range result.ReasonCodes {
		if rc == ReasonChainSelected {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("ReasonChainSelected appears %d times in %v, want exactly 1", count, result.ReasonCodes)
	}
}

// TestAgentFRegistryRejectsEmptyID verifies Register drops a provider with an
// empty ID instead of storing it under "".
func TestAgentFRegistryRejectsEmptyID(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&coverageDecisionProvider{id: "", caps: Capabilities{CanSelect: true}})
	if _, ok := reg.Get(""); ok {
		t.Fatal("empty-ID provider must not be registered")
	}
	// The built-in local provider must still be present.
	if _, ok := reg.Get("local"); !ok {
		t.Fatal("local provider missing after empty-ID register")
	}
}

// TestAgentFValidateResultUnknownAction verifies an unknown action fails
// strict validation.
func TestAgentFValidateResultUnknownAction(t *testing.T) {
	cands := makeChainEligible("a", "b")
	err := ValidateResult(cands, DecisionResult{
		Action: "BOGUS", Confidence: 0.5, ReasonCodes: []ReasonCode{ReasonAbstained},
	})
	if err == nil {
		t.Fatal("unknown action must fail validation")
	}
	if !strings.Contains(err.Error(), "BOGUS") {
		t.Fatalf("error %q must name the invalid action", err.Error())
	}
}

// TestAgentFOrchestratorCopiesPolicyTrace verifies the orchestrator copies a
// chain-selected result's PolicyTrace onto the decision trace.
func TestAgentFOrchestratorCopiesPolicyTrace(t *testing.T) {
	want := &PolicyTrace{PolicyID: "p1", PolicyVersion: "v3", TaskType: "coding", SelectedID: "a", OriginalPrimaryID: "a"}
	prov := &coverageDecisionProvider{
		id:   "policy",
		caps: Capabilities{CanSelect: true, CanRank: false},
		result: DecisionResult{
			Action: ActionSelect, SelectedID: "a", Confidence: 0.9,
			ReasonCodes: []ReasonCode{ReasonPolicyScored, ReasonEligibleSetPreserved},
			PolicyTrace: want,
		},
	}
	reg := &Registry{providers: map[string]DecisionProvider{}}
	reg.Register(prov)
	full := config.Config{
		Decision: config.DecisionConfig{Mode: "hybrid", Chain: "c1", TimeoutMS: 5000},
		DecisionChains: []config.DecisionChainConfig{
			{ID: "c1", Steps: []config.DecisionChainStep{{Provider: "policy", TimeoutMS: 1000}}},
		},
	}
	orch := NewOrchestratorWithConfig(reg, full, &Metrics{})
	_, result, trace := orch.Decide(context.Background(), DecisionRequest{
		Candidates: coverageDecisionCandidates(),
	})
	if result.Action != ActionSelect || result.SelectedID != "a" {
		t.Fatalf("result = %+v, want SELECT a", result)
	}
	if trace.PolicyTrace == nil {
		t.Fatal("trace.PolicyTrace is nil, want the chain result's policy trace")
	}
	if trace.PolicyTrace.PolicyID != "p1" || trace.PolicyTrace.PolicyVersion != "v3" {
		t.Fatalf("trace.PolicyTrace = %+v, want policy p1 v3", trace.PolicyTrace)
	}
	if trace.FallbackUsed {
		t.Fatal("FallbackUsed must be false for a chain-selected result")
	}
}
