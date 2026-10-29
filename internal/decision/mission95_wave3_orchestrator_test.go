package decision

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Wave-3 orchestrator coverage: hybrid budget derivation, hybrid
// exhausted/affinity/timeout outcomes, single-path budget and timeout
// clamps, error truncation, invalid-action fail-open, and the
// already-present normalization reason. These complement the wave-2
// single-path cases (long error, panic, cannot-select, missing chain,
// budget exhaustion) without duplicating them.

func wave3HybridOrchestrator(t *testing.T, reg *Registry, cfg config.DecisionConfig, chains []config.DecisionChainConfig, metrics *Metrics) *Orchestrator {
	t.Helper()
	full := config.Config{Decision: cfg, DecisionChains: chains}
	return NewOrchestratorWithConfig(reg, full, metrics)
}

func wave3Chain(t *testing.T, id string, steps ...config.DecisionChainStep) config.DecisionChainConfig {
	t.Helper()
	return config.DecisionChainConfig{ID: id, Steps: steps}
}

func TestWave3HybridBudgetDerivedFromChainLength(t *testing.T) {
	reg := NewRegistry()
	first := abstainProvider("step1")
	second := selectProvider("step2", "b")
	reg.Register(first)
	reg.Register(second)
	o := wave3HybridOrchestrator(t, reg,
		config.DecisionConfig{Mode: "hybrid", Chain: "c1", TimeoutMS: 1000},
		[]config.DecisionChainConfig{wave3Chain(t, "c1",
			config.DecisionChainStep{Provider: "step1"},
			config.DecisionChainStep{Provider: "step2"},
		)}, &Metrics{})
	ordered, res, trace := o.Decide(context.Background(), DecisionRequest{Candidates: makeChainEligible("a", "b")})
	if res.Action != ActionSelect || res.SelectedID != "b" {
		t.Fatalf("derived chain budget must let the second step select: %+v", res)
	}
	if trace.ChainTrace == nil || trace.ChainTrace.CallsUsed != 2 {
		t.Fatalf("both chain steps must run under the derived budget: %+v", trace.ChainTrace)
	}
	if first.Calls() != 1 || second.Calls() != 1 {
		t.Fatalf("step calls = %d/%d, want 1/1", first.Calls(), second.Calls())
	}
	if len(ordered) != 2 || ordered[0].ID != "b" {
		t.Fatalf("selected candidate must lead: %v", ordered)
	}
}

func TestWave3HybridBudgetFilledFromConfig(t *testing.T) {
	reg := NewRegistry()
	reg.Register(abstainProvider("step1"))
	reg.Register(selectProvider("step2", "b"))
	// Request budget is set (non-zero) but carries no call cap: the
	// configured per-decision cap fills it in.
	o := wave3HybridOrchestrator(t, reg,
		config.DecisionConfig{Mode: "hybrid", Chain: "c1", TimeoutMS: 1000, MaxProviderCalls: 3},
		[]config.DecisionChainConfig{wave3Chain(t, "c1",
			config.DecisionChainStep{Provider: "step1"},
			config.DecisionChainStep{Provider: "step2"},
		)}, &Metrics{})
	_, res, _ := o.Decide(context.Background(), DecisionRequest{
		Candidates: makeChainEligible("a", "b"),
		Budget:     Budget{Timeout: 5 * time.Second},
	})
	if res.Action != ActionSelect || res.SelectedID != "b" {
		t.Fatalf("config-filled budget must let the chain select: %+v", res)
	}
}

func TestWave3HybridBudgetFilledFromChainLengthWhenUnset(t *testing.T) {
	reg := NewRegistry()
	reg.Register(abstainProvider("step1"))
	reg.Register(selectProvider("step2", "b"))
	o := wave3HybridOrchestrator(t, reg,
		config.DecisionConfig{Mode: "hybrid", Chain: "c1", TimeoutMS: 1000},
		[]config.DecisionChainConfig{wave3Chain(t, "c1",
			config.DecisionChainStep{Provider: "step1"},
			config.DecisionChainStep{Provider: "step2"},
		)}, &Metrics{})
	_, res, _ := o.Decide(context.Background(), DecisionRequest{
		Candidates: makeChainEligible("a", "b"),
		Budget:     Budget{Timeout: 5 * time.Second},
	})
	if res.Action != ActionSelect || res.SelectedID != "b" {
		t.Fatalf("chain-length-filled budget must let the chain select: %+v", res)
	}
}

func TestWave3HybridExhaustedUsesChainIDAndFallback(t *testing.T) {
	reg := NewRegistry()
	reg.Register(abstainProvider("step1"))
	o := wave3HybridOrchestrator(t, reg,
		config.DecisionConfig{Mode: "hybrid", Chain: "c1", TimeoutMS: 1000},
		[]config.DecisionChainConfig{wave3Chain(t, "c1",
			config.DecisionChainStep{Provider: "step1"},
		)}, &Metrics{})
	_, res, trace := o.Decide(context.Background(), DecisionRequest{Candidates: makeChainEligible("a", "b")})
	if res.Action != ActionAbstain {
		t.Fatalf("exhausted chain must abstain: %+v", res)
	}
	if trace.ProviderID != "c1" {
		t.Fatalf("exhausted chain trace provider = %q, want chain id", trace.ProviderID)
	}
	if !trace.FallbackUsed {
		t.Fatal("exhausted chain must report fallback used")
	}
	if trace.ChainTrace == nil || trace.ChainTrace.Outcome != ChainOutcomeExhausted {
		t.Fatalf("exhausted chain outcome = %+v", trace.ChainTrace)
	}
}

func TestWave3HybridAffinityPreservesPinnedCandidate(t *testing.T) {
	reg := NewRegistry()
	step := selectProvider("step1", "b")
	reg.Register(step)
	o := wave3HybridOrchestrator(t, reg,
		config.DecisionConfig{Mode: "hybrid", Chain: "c1", TimeoutMS: 1000},
		[]config.DecisionChainConfig{wave3Chain(t, "c1",
			config.DecisionChainStep{Provider: "step1"},
		)}, &Metrics{})
	ordered, res, trace := o.Decide(context.Background(), DecisionRequest{
		Candidates:        makeChainEligible("a", "b"),
		PinnedCandidateID: "a",
	})
	if res.Action != ActionSelect || res.SelectedID != "a" {
		t.Fatalf("pinned candidate must win the hybrid path: %+v", res)
	}
	if step.Calls() != 0 {
		t.Fatalf("affinity short-circuit must call no provider, got %d", step.Calls())
	}
	if trace.FallbackUsed {
		t.Fatal("affinity-preserved chain must not report fallback used")
	}
	if trace.ChainTrace == nil || trace.ChainTrace.Outcome != ChainOutcomeAffinityPreserved || trace.ChainTrace.CallsUsed != 0 {
		t.Fatalf("affinity chain trace = %+v", trace.ChainTrace)
	}
	if len(ordered) != 2 || ordered[0].ID != "a" {
		t.Fatalf("pinned candidate must lead: %v", ordered)
	}
}

func TestWave3HybridChainDeadlineCountsAsTimeoutMetric(t *testing.T) {
	reg := NewRegistry()
	slow := newCountingProvider("slow")
	slow.delay = 5 * time.Second
	reg.Register(slow)
	metrics := &Metrics{}
	o := wave3HybridOrchestrator(t, reg,
		config.DecisionConfig{Mode: "hybrid", Chain: "c1", TimeoutMS: 1},
		[]config.DecisionChainConfig{wave3Chain(t, "c1",
			config.DecisionChainStep{Provider: "slow"},
		)}, metrics)
	_, res, _ := o.Decide(context.Background(), DecisionRequest{Candidates: makeChainEligible("a", "b")})
	found := false
	for _, rc := range res.ReasonCodes {
		if rc == ReasonTimeout {
			found = true
		}
	}
	if !found {
		t.Fatalf("chain deadline must surface a timeout reason: %+v", res)
	}
	if got := o.MetricsSnapshot()["timeouts_total"]; got != 1 {
		t.Fatalf("timeouts_total = %d, want 1", got)
	}
}

func TestWave3SinglePathConfigBudgetIsHonored(t *testing.T) {
	reg := NewRegistry()
	p := selectProvider("ext", "a")
	reg.Register(p)
	o := NewOrchestrator(reg, config.DecisionConfig{Mode: "assisted", Provider: "ext", TimeoutMS: 1000, MaxProviderCalls: 2}, &Metrics{})
	_, res, _ := o.Decide(context.Background(), DecisionRequest{Candidates: makeChainEligible("a", "b")})
	if res.Action != ActionSelect || res.SelectedID != "a" {
		t.Fatalf("configured single-path budget must allow the call: %+v", res)
	}
	if p.Calls() != 1 {
		t.Fatalf("provider calls = %d, want 1", p.Calls())
	}
}

func TestWave3SinglePathEmptyProviderDefaultsToLocal(t *testing.T) {
	reg := NewRegistry()
	local := selectProvider("local", "a")
	reg.Register(local)
	// Bypass the constructor defaults on purpose: an empty provider
	// name must resolve to "local" inside Decide.
	o := &Orchestrator{registry: reg, metrics: &Metrics{}, cfg: config.DecisionConfig{Mode: "local", TimeoutMS: 1000}, chains: map[string]config.DecisionChainConfig{}}
	_, res, _ := o.Decide(context.Background(), DecisionRequest{Candidates: makeChainEligible("a", "b")})
	if res.Action != ActionSelect || res.SelectedID != "a" {
		t.Fatalf("empty provider must resolve to local: %+v", res)
	}
}

func TestWave3SinglePathTimeoutClamps(t *testing.T) {
	for _, tc := range []struct {
		name      string
		timeoutMS int
	}{
		{"zero timeout clamps to 10ms", 0},
		{"oversize timeout clamps to 5s", 6000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := NewRegistry()
			reg.Register(selectProvider("local", "a"))
			o := &Orchestrator{registry: reg, metrics: &Metrics{}, cfg: config.DecisionConfig{Mode: "local", Provider: "local", TimeoutMS: tc.timeoutMS}, chains: map[string]config.DecisionChainConfig{}}
			_, res, _ := o.Decide(context.Background(), DecisionRequest{Candidates: makeChainEligible("a", "b")})
			if res.Action != ActionSelect || res.SelectedID != "a" {
				t.Fatalf("clamped timeout must still let a fast provider select: %+v", res)
			}
		})
	}
}

func TestWave3AbstainErrorTruncatedAndReasonNotDuplicated(t *testing.T) {
	reg := NewRegistry()
	p := newCountingProvider("ext")
	p.result = DecisionResult{
		Action: ActionAbstain, Abstained: true,
		ReasonCodes: []ReasonCode{ReasonAbstained},
		Error:       strings.Repeat("e", 300),
	}
	reg.Register(p)
	o := NewOrchestrator(reg, config.DecisionConfig{Mode: "assisted", Provider: "ext", TimeoutMS: 1000}, &Metrics{})
	_, res, _ := o.Decide(context.Background(), DecisionRequest{Candidates: makeChainEligible("a", "b")})
	if len(res.Error) != 256 {
		t.Fatalf("abstain error length = %d, want 256", len(res.Error))
	}
	count := 0
	for _, rc := range res.ReasonCodes {
		if rc == ReasonAbstained {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("abstained reason appears %d times, want exactly 1: %v", count, res.ReasonCodes)
	}
}

func TestWave3ProviderErrorWithInvalidActionAbstains(t *testing.T) {
	reg := NewRegistry()
	p := newCountingProvider("ext")
	p.result = DecisionResult{Action: "bogus-action"}
	p.err = errors.New("boom")
	reg.Register(p)
	o := NewOrchestrator(reg, config.DecisionConfig{Mode: "assisted", Provider: "ext", TimeoutMS: 1000}, &Metrics{})
	_, res, _ := o.Decide(context.Background(), DecisionRequest{Candidates: makeChainEligible("a", "b")})
	if res.Action != ActionAbstain || !res.Abstained {
		t.Fatalf("invalid action on provider error must abstain: %+v", res)
	}
	if res.Error != "boom" {
		t.Fatalf("provider error = %q, want boom", res.Error)
	}
	found := false
	for _, rc := range res.ReasonCodes {
		if rc == ReasonProviderError {
			found = true
		}
	}
	if !found {
		t.Fatalf("provider error reason missing: %v", res.ReasonCodes)
	}
}
