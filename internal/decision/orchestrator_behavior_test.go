package decision

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

func TestCoverageBehaviorOrchestratorSnapshotsAndReload(t *testing.T) {
	full := config.Default()
	full.Decision.Mode = "off"
	full.Decision.TimeoutMS = 25
	full.DecisionChains = []config.DecisionChainConfig{{ID: "primary", Steps: []config.DecisionChainStep{{Provider: "local"}}}}
	o := NewOrchestratorWithConfig(nil, full, nil)
	if o.Config().Mode != "off" || o.Config().TimeoutMS != 25 {
		t.Fatalf("config snapshot=%+v", o.Config())
	}
	chains := o.Chains()
	chains["primary"].Steps[0].Provider = "mutated"
	if o.Chains()["primary"].Steps[0].Provider != "local" {
		t.Fatal("chain snapshot leaked mutable step slice")
	}
	if o.ProviderState() == nil || o.MetricsSnapshot() == nil {
		t.Fatal("orchestrator dependencies missing")
	}
	full.Decision.Mode = "hybrid"
	full.Decision.Provider = "local"
	full.DecisionProviderHealth.FailureThreshold = 5
	o.UpdateFullConfig(full)
	if o.Config().Mode != "hybrid" || o.ProviderState().Config().FailureThreshold != 5 {
		t.Fatalf("full reload not applied: cfg=%+v state=%+v", o.Config(), o.ProviderState().Config())
	}
	o.UpdateConfig(config.DecisionConfig{})
	if o.Config().Mode != "off" || o.Config().Provider != "local" || o.Config().TimeoutMS != 10 {
		t.Fatalf("defaults not normalized: %+v", o.Config())
	}
}

func TestCoverageBehaviorOrchestratorFastPathsAndBoundedErrors(t *testing.T) {
	o := NewOrchestrator(nil, config.DecisionConfig{Mode: "off"}, nil)
	ordered, result, trace := o.Decide(context.Background(), DecisionRequest{})
	if len(ordered) != 0 || !result.IsStrictAbstain() || trace.ProviderID != "none" {
		t.Fatalf("empty fast path ordered=%v result=%+v trace=%+v", ordered, result, trace)
	}
	candidates := []Candidate{{ID: "only", OriginalRank: 0}}
	ordered, result, trace = o.Decide(context.Background(), DecisionRequest{Candidates: candidates})
	if len(ordered) != 1 || ordered[0].ID != "only" || trace.FallbackUsed || !result.IsStrictAbstain() {
		t.Fatalf("single fast path ordered=%v result=%+v trace=%+v", ordered, result, trace)
	}
	if got := boundedError(errors.New(strings.Repeat("x", 300))); len(got) != 256 {
		t.Fatalf("bounded error len=%d", len(got))
	}
	if boundedError(nil) != "" || len(boundedPanic(strings.Repeat("x", 300))) != 256 {
		t.Fatal("nil/panic bounds failed")
	}
}
