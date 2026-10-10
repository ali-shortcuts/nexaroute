package decision

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

type coverageDecisionProvider struct {
	id         string
	result     DecisionResult
	err        error
	panicValue any
	health     string
	caps       Capabilities
	calls      int
}

func (p *coverageDecisionProvider) ID() string                 { return p.id }
func (p *coverageDecisionProvider) Capabilities() Capabilities { return p.caps }
func (p *coverageDecisionProvider) Health() ProviderHealth {
	status := p.health
	if status == "" {
		status = HealthHealthy
	}
	return ProviderHealth{Status: status}
}
func (p *coverageDecisionProvider) Decide(context.Context, DecisionRequest) (DecisionResult, error) {
	p.calls++
	if p.panicValue != nil {
		panic(p.panicValue)
	}
	return p.result, p.err
}

func coverageDecisionCandidates() []Candidate {
	return []Candidate{{ID: "a", OriginalRank: 0}, {ID: "b", OriginalRank: 1}, {ID: "c", OriginalRank: 2}}
}
func coverageDecisionHasReason(codes []ReasonCode, want ReasonCode) bool {
	for _, code := range codes {
		if code == want {
			return true
		}
	}
	return false
}
func coverageDecisionRegistry(provider DecisionProvider) *Registry {
	r := &Registry{providers: map[string]DecisionProvider{}}
	r.Register(provider)
	return r
}
func coverageDecisionAssertOrder(t *testing.T, got, want []Candidate) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("candidate count got %d want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].ID != want[i].ID {
			t.Fatalf("candidate %d got %q want %q", i, got[i].ID, want[i].ID)
		}
	}
}

func TestCoverageDecisionNonHybridFallbackOutcomes(t *testing.T) {
	candidates := coverageDecisionCandidates()
	tests := []struct {
		name     string
		provider DecisionProvider
		cfg      config.DecisionConfig
		budget   Budget
		wantCode ReasonCode
		wantCall int
	}{
		{
			name:     "unknown provider",
			cfg:      config.DecisionConfig{Mode: "local", Provider: "missing", TimeoutMS: 10},
			budget:   Budget{Timeout: time.Second, MaxProviderCalls: 1},
			wantCode: ReasonProviderError,
		},
		{
			name:     "zero call budget",
			provider: &coverageDecisionProvider{id: "budget", caps: Capabilities{CanRank: true, CanSelect: true}},
			cfg:      config.DecisionConfig{Mode: "local", Provider: "budget", TimeoutMS: 10},
			budget:   Budget{Timeout: time.Second, MaxProviderCalls: 0},
			wantCode: ReasonBudgetExceeded,
		},
		{
			name:     "unavailable provider",
			provider: &coverageDecisionProvider{id: "unavailable", health: HealthUnavailable, caps: Capabilities{CanRank: true, CanSelect: true}},
			cfg:      config.DecisionConfig{Mode: "local", Provider: "unavailable", TimeoutMS: 10},
			budget:   Budget{Timeout: time.Second, MaxProviderCalls: 1},
			wantCode: ReasonProviderUnhealthy,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry()
			if tc.provider != nil {
				reg.Register(tc.provider)
			}
			o := NewOrchestrator(reg, tc.cfg, &Metrics{})
			ordered, result, trace := o.Decide(context.Background(), DecisionRequest{Candidates: candidates, Budget: tc.budget})
			coverageDecisionAssertOrder(t, ordered, candidates)
			if !coverageDecisionHasReason(result.ReasonCodes, tc.wantCode) || trace.ProviderID == "" || !trace.FallbackUsed {
				t.Fatalf("fallback evidence result=%+v trace=%+v", result, trace)
			}
			if tc.provider != nil && tc.provider.(*coverageDecisionProvider).calls != tc.wantCall {
				t.Fatalf("calls got %d want %d", tc.provider.(*coverageDecisionProvider).calls, tc.wantCall)
			}
		})
	}
}

func TestCoverageDecisionProviderOutcomeEvidence(t *testing.T) {
	candidates := coverageDecisionCandidates()
	cases := []struct {
		name      string
		provider  *coverageDecisionProvider
		wantCode  ReasonCode
		wantError string
	}{
		{
			name:      "error preserves existing reason",
			provider:  &coverageDecisionProvider{id: "error", result: DecisionResult{Action: ActionAbstain, ReasonCodes: []ReasonCode{ReasonProviderError}}, err: errors.New(strings.Repeat("e", 300)), caps: Capabilities{CanRank: true, CanSelect: true}},
			wantCode:  ReasonProviderError,
			wantError: strings.Repeat("e", 256),
		},
		{
			name:     "invalid action becomes abstain",
			provider: &coverageDecisionProvider{id: "invalid-action", result: DecisionResult{Action: "unknown", Confidence: .5}, caps: Capabilities{CanRank: true, CanSelect: true}},
			wantCode: ReasonInvalidResult,
		},
		{
			name:      "panic is bounded",
			provider:  &coverageDecisionProvider{id: "panic", panicValue: strings.Repeat("p", 300), caps: Capabilities{CanRank: true, CanSelect: true}},
			wantCode:  ReasonProviderPanic,
			wantError: "decision provider panic:",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := NewOrchestrator(coverageDecisionRegistry(tc.provider), config.DecisionConfig{Mode: "local", Provider: tc.provider.id, TimeoutMS: 50}, &Metrics{})
			ordered, result, trace := o.Decide(context.Background(), DecisionRequest{Candidates: candidates, Budget: Budget{Timeout: time.Second, MaxProviderCalls: 1}})
			coverageDecisionAssertOrder(t, ordered, candidates)
			if !coverageDecisionHasReason(result.ReasonCodes, tc.wantCode) || !trace.FallbackUsed {
				t.Fatalf("provider outcome result=%+v trace=%+v", result, trace)
			}
			if tc.wantError != "" && (len(result.Error) != 256 || (tc.name == "panic is bounded" && !strings.HasPrefix(result.Error, tc.wantError)) || (tc.name != "panic is bounded" && !strings.HasPrefix(result.Error, tc.wantError[:12]))) {
				t.Fatalf("bounded error length/value got %q", result.Error)
			}
		})
	}
}

func TestCoverageDecisionAffinityAndConstraintEvidence(t *testing.T) {
	candidates := coverageDecisionCandidates()
	provider := &coverageDecisionProvider{id: "affinity-provider", result: DecisionResult{Action: ActionSelect, SelectedID: "a", Confidence: 1}, caps: Capabilities{CanRank: true, CanSelect: true}}
	o := NewOrchestrator(coverageDecisionRegistry(provider), config.DecisionConfig{Mode: "local", Provider: provider.id, TimeoutMS: -1}, &Metrics{})
	ordered, result, trace := o.Decide(context.Background(), DecisionRequest{Candidates: candidates, PinnedCandidateID: "b", Budget: Budget{Timeout: time.Second, MaxProviderCalls: 1}})
	if provider.calls != 0 || result.SelectedID != "b" || trace.SelectedID != "b" || trace.FallbackUsed || !coverageDecisionHasReason(result.ReasonCodes, ReasonAffinityPreserved) {
		t.Fatalf("affinity short circuit result=%+v trace=%+v calls=%d", result, trace, provider.calls)
	}
	coverageDecisionAssertOrder(t, ordered, []Candidate{candidates[1], candidates[0], candidates[2]})

	violating := &coverageDecisionProvider{id: "constraint", result: DecisionResult{Action: ActionSelect, SelectedID: "c", Confidence: .9}, caps: Capabilities{CanRank: true, CanSelect: true}}
	o = NewOrchestrator(coverageDecisionRegistry(violating), config.DecisionConfig{Mode: "local", Provider: violating.id, TimeoutMS: 6001}, &Metrics{})
	constrained := []Candidate{{ID: "a", OriginalRank: 0, Priority: 0}, {ID: "b", OriginalRank: 1, Priority: 0}, {ID: "c", OriginalRank: 2, Priority: 1}}
	ordered, result, trace = o.Decide(context.Background(), DecisionRequest{Candidates: constrained, Budget: Budget{Timeout: time.Second, MaxProviderCalls: 1}})
	coverageDecisionAssertOrder(t, ordered, constrained)
	if !coverageDecisionHasReason(result.ReasonCodes, ReasonPrimaryConstraintViolation) || !trace.FallbackUsed {
		t.Fatalf("primary constraint fallback result=%+v trace=%+v", result, trace)
	}
}

func TestCoverageDecisionCooldownAndPolicyTrace(t *testing.T) {
	candidates := coverageDecisionCandidates()
	external := &coverageDecisionProvider{id: "external-cooldown", result: DecisionResult{Action: ActionSelect, SelectedID: "a", Confidence: .8}, caps: Capabilities{CanRank: true, CanSelect: true}}
	o := NewOrchestrator(coverageDecisionRegistry(external), config.DecisionConfig{Mode: "local", Provider: external.id, TimeoutMS: 20}, &Metrics{})
	o.ProviderState().RecordFailure(external.id)
	o.ProviderState().RecordFailure(external.id)
	o.ProviderState().RecordFailure(external.id)
	ordered, result, trace := o.Decide(context.Background(), DecisionRequest{Candidates: candidates, Budget: Budget{Timeout: time.Second, MaxProviderCalls: 1}})
	coverageDecisionAssertOrder(t, ordered, candidates)
	if external.calls != 0 || !coverageDecisionHasReason(result.ReasonCodes, ReasonDecisionProviderCooldown) || !trace.FallbackUsed {
		t.Fatalf("cooldown evidence result=%+v trace=%+v calls=%d", result, trace, external.calls)
	}

	policyTrace := &PolicyTrace{PolicyID: "coverage-policy", SelectedID: "a", SelectedScore: .9}
	policy := &coverageDecisionProvider{id: "policy-evidence", result: DecisionResult{Action: ActionSelect, SelectedID: "a", Confidence: .9, PolicyTrace: policyTrace}, caps: Capabilities{CanRank: true, CanSelect: true}}
	o = NewOrchestrator(coverageDecisionRegistry(policy), config.DecisionConfig{Mode: "local", Provider: policy.id, TimeoutMS: 20}, &Metrics{})
	ordered, result, trace = o.Decide(context.Background(), DecisionRequest{Candidates: candidates, Budget: Budget{Timeout: time.Second, MaxProviderCalls: 1}})
	if result.Action != ActionSelect || trace.PolicyTrace != policyTrace || trace.SelectedID != "a" {
		t.Fatalf("policy evidence result=%+v trace=%+v", result, trace)
	}
	coverageDecisionAssertOrder(t, ordered, []Candidate{candidates[0], candidates[1], candidates[2]})
}

func TestCoverageDecisionHybridEvidence(t *testing.T) {
	candidates := coverageDecisionCandidates()
	selected := &coverageDecisionProvider{id: "hybrid-selected", result: DecisionResult{Action: ActionSelect, SelectedID: "b", Confidence: .9}, caps: Capabilities{CanRank: true, CanSelect: true}}
	reg := coverageDecisionRegistry(selected)
	full := config.Default()
	full.Decision = config.DecisionConfig{Mode: "hybrid", Chain: "coverage-chain", TimeoutMS: 50, MaxProviderCalls: 1}
	full.DecisionChains = []config.DecisionChainConfig{{ID: "coverage-chain", Steps: []config.DecisionChainStep{{Provider: selected.id}}}}
	o := NewOrchestratorWithConfig(reg, full, &Metrics{})
	ordered, result, trace := o.Decide(context.Background(), DecisionRequest{Candidates: candidates})
	if result.Action != ActionSelect || trace.ChainTrace == nil || trace.ChainTrace.Outcome != ChainOutcomeSelected || trace.ProviderID != selected.id || trace.FallbackUsed || selected.calls != 1 {
		t.Fatalf("hybrid selected evidence result=%+v trace=%+v calls=%d", result, trace, selected.calls)
	}
	coverageDecisionAssertOrder(t, ordered, []Candidate{candidates[1], candidates[0], candidates[2]})

	full.Decision.Chain = "missing-chain"
	o.UpdateFullConfig(full)
	ordered, result, trace = o.Decide(context.Background(), DecisionRequest{Candidates: candidates})
	coverageDecisionAssertOrder(t, ordered, candidates)
	if !coverageDecisionHasReason(result.ReasonCodes, ReasonChainExhausted) || trace.ChainTrace == nil || !trace.FallbackUsed {
		t.Fatalf("missing chain evidence result=%+v trace=%+v", result, trace)
	}
}
