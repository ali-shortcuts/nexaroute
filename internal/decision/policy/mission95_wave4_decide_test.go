package policy

import (
	"context"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

func wave4HasReason(t *testing.T, codes []decision.ReasonCode, want decision.ReasonCode) {
	t.Helper()
	for _, c := range codes {
		if c == want {
			return
		}
	}
	t.Fatalf("reason codes %v do not contain %s", codes, want)
}

// A task-aware win that is too small for MinScoreDelta abstains and the
// abstention carries both MIN_DELTA_NOT_MET and TASK_AWARE_WEIGHTS: the
// winner (reliability 1.0) beats the original primary (reliability 0.9)
// by only 0.1, below the 0.6 delta, so order is preserved.
func TestWave4DecideMinDeltaWithTaskAwareWeights(t *testing.T) {
	pol := Policy{
		ID: "wave4", Version: "v1", SelectionMode: "select_first",
		Weights:       Weights{Reliability: 1},
		TaskOverrides: map[string]Weights{"code_edit": {Reliability: 1}},
		MinScoreDelta: 0.6,
	}
	provider := NewProvider([]Policy{pol}, "wave4")
	req := decision.DecisionRequest{
		PolicyID: "wave4",
		TaskProfile: taskprofile.TaskProfile{
			Type: taskprofile.TaskCodeEdit,
		},
		Candidates: []decision.Candidate{
			makeCandidate("victim", func(c *decision.Candidate) {
				c.HealthStatus = "healthy"
				c.Successes = 10
				c.EWMAFailureRate = 0.2
				c.OriginalRank = 0
			}),
			makeCandidate("candidate", func(c *decision.Candidate) {
				c.HealthStatus = "healthy"
				c.Successes = 10
				c.EWMAFailureRate = 0
				c.OriginalRank = 1
			}),
		},
	}
	res, err := provider.Decide(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != decision.ActionAbstain || !res.Abstained {
		t.Fatalf("action = %s abstained=%v, want ABSTAIN/true for an unmet delta", res.Action, res.Abstained)
	}
	wave4HasReason(t, res.ReasonCodes, decision.ReasonMinDeltaNotMet)
	wave4HasReason(t, res.ReasonCodes, decision.ReasonTaskAwareWeights)
	if res.PolicyTrace == nil {
		t.Fatal("expected a policy trace on the min-delta abstention")
	}
	if res.PolicyTrace.SelectedID != "candidate" || res.PolicyTrace.OriginalPrimaryID != "victim" {
		t.Fatalf("trace = %+v, want selected candidate over original victim", res.PolicyTrace)
	}
	if res.PolicyTrace.ChangedPrimary {
		t.Fatal("min-delta abstention must not report a changed primary")
	}
}
