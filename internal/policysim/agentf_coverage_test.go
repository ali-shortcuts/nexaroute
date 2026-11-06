package policysim

import (
	"context"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Agent F coverage: branches left uncovered in the 2026-11-06 baseline.

// TestAgentFLoadPoliciesRejectsInvalidPolicy verifies the first invalid
// policy aborts the load with its error instead of silently dropping it.
func TestAgentFLoadPoliciesRejectsInvalidPolicy(t *testing.T) {
	cfg := config.Config{DecisionPolicies: []config.DecisionPolicyConfig{
		{ID: "good", Weights: config.DecisionPolicyWeights{RouterBaseline: 1}},
		{ID: "", Weights: config.DecisionPolicyWeights{Latency: 1}},
	}}
	if _, err := LoadPolicies(cfg); err == nil {
		t.Fatal("empty policy id must fail the load")
	} else if !strings.Contains(err.Error(), "policy id is required") {
		t.Fatalf("error = %q, want policy-id error", err.Error())
	}
}

// TestAgentFSimulateRejectsInvalidConfig verifies simulation refuses a
// config whose policies cannot load.
func TestAgentFSimulateRejectsInvalidConfig(t *testing.T) {
	cfg := config.Config{DecisionPolicies: []config.DecisionPolicyConfig{
		{ID: "", Weights: config.DecisionPolicyWeights{Latency: 1}},
	}}
	if _, err := Simulate(context.Background(), cfg, "baseline-first", []Fixture{simFixture()}); err == nil {
		t.Fatal("simulation with an invalid policy must fail")
	}
}

// TestAgentFSimulateCancelledContextAbstains verifies a cancelled context
// makes every simulated case abstain instead of producing a winner from
// no evidence.
func TestAgentFSimulateCancelledContextAbstains(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := Simulate(ctx, simConfig(), "baseline-first", []Fixture{simFixture()})
	if err != nil {
		t.Fatalf("cancelled simulation must not error, got %v", err)
	}
	if len(rep.Cases) != 1 {
		t.Fatalf("cases = %d, want 1", len(rep.Cases))
	}
	if !rep.Cases[0].Abstained {
		t.Fatalf("case = %+v, want abstained on cancellation", rep.Cases[0])
	}
	if rep.Cases[0].Winner != "" {
		t.Fatalf("winner = %q, want none on cancellation", rep.Cases[0].Winner)
	}
}

// TestAgentFReplayRejectsUnknownPolicy verifies replay refuses an
// undefined policy id.
func TestAgentFReplayRejectsUnknownPolicy(t *testing.T) {
	records := []ReplayRecord{{Request: simFixture().Request, RecordedWinner: "a"}}
	if _, err := Replay(context.Background(), simConfig(), "ghost", records); err == nil {
		t.Fatal("replay under an undefined policy must fail")
	}
}
