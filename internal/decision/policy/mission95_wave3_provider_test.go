package policy

import (
	"context"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

// Wave-3 policy provider coverage: the empty-eligible path, the
// task-aware abstain and select paths, a normalized confidence of 1,
// and the estimated-token fallback for the context-window
// requirement. The band-fallback, reason-cap and confidence-clamp
// branches are unreachable by construction (a non-empty band always
// survives filtering, the affinity reason list can never exceed
// MaxReasonCodes, and WeightedSum already normalizes scores into
// [0,1]) and are not faked here.

func wave3PolicyCandidates() []decision.Candidate {
	return []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, Priority: 10, OriginalRank: 0},
		{ID: "b", PoolOrdinal: 0, Priority: 10, OriginalRank: 1},
	}
}

func TestWave3DecideEmptyCandidatesAbstains(t *testing.T) {
	p := NewProvider([]Policy{makePolicy("test", Weights{RouterBaseline: 1}, 0)}, "test")
	res, err := p.Decide(context.Background(), decision.DecisionRequest{Candidates: nil, PolicyID: "test"})
	if err != nil {
		t.Fatalf("empty candidates must not error: %v", err)
	}
	if res.Action != decision.ActionAbstain || !res.Abstained {
		t.Fatalf("empty candidates must abstain: %+v", res)
	}
	found := false
	for _, rc := range res.ReasonCodes {
		if rc == decision.ReasonEmptyEligible {
			found = true
		}
	}
	if !found {
		t.Fatalf("empty-eligible reason missing: %v", res.ReasonCodes)
	}
}

func TestWave3DecideTaskAwareAbstainKeepsOriginalPrimary(t *testing.T) {
	pol := makePolicy("test", Weights{RouterBaseline: 1}, 0)
	pol.TaskOverrides = map[string]Weights{"coding": {Latency: 1}}
	p := NewProvider([]Policy{pol}, "test")
	cands := wave3PolicyCandidates()
	cands[0].EWMALatencyMS = 100
	cands[1].EWMALatencyMS = 100
	req := decision.DecisionRequest{
		Candidates:  cands,
		PolicyID:    "test",
		TaskProfile: decision.TaskProfile{Type: "coding"},
	}
	res, err := p.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("task-aware decide must not error: %v", err)
	}
	if res.Action != decision.ActionAbstain {
		t.Fatalf("equal scores must keep the original primary (abstain): %+v", res)
	}
	found := false
	for _, rc := range res.ReasonCodes {
		if rc == decision.ReasonTaskAwareWeights {
			found = true
		}
	}
	if !found {
		t.Fatalf("task-aware reason missing: %v", res.ReasonCodes)
	}
	if res.PolicyTrace == nil || res.PolicyTrace.TaskType != "coding" {
		t.Fatalf("policy trace must carry the task type: %+v", res.PolicyTrace)
	}
}

func TestWave3DecideSelectConfidenceOne(t *testing.T) {
	// WeightedSum normalizes by total weight, so a dominant latency
	// weight yields a normalized confidence of exactly 1 for the
	// fast challenger (the >1 clamp itself is defensive: scores
	// leaving WeightedSum are already within [0,1]).
	pol := makePolicy("test", Weights{Latency: 5}, 0)
	p := NewProvider([]Policy{pol}, "test")
	cands := wave3PolicyCandidates()
	cands[0].EWMALatencyMS = 1000 // original primary is slow: latency score 0
	cands[1].EWMALatencyMS = 100  // challenger is fast: latency score 1
	res, err := p.Decide(context.Background(), decision.DecisionRequest{Candidates: cands, PolicyID: "test"})
	if err != nil {
		t.Fatalf("decide must not error: %v", err)
	}
	if res.Action != decision.ActionSelect || res.SelectedID != "b" {
		t.Fatalf("fast challenger must be selected: %+v", res)
	}
	if res.Confidence != 1 {
		t.Fatalf("confidence = %v, want 1", res.Confidence)
	}
}

func TestWave3DecideTaskAwareSelectChangesPrimary(t *testing.T) {
	// The task override replaces the base weights for this task, so
	// the faster challenger wins where the base policy would tie.
	pol := makePolicy("test", Weights{RouterBaseline: 1}, 0)
	pol.TaskOverrides = map[string]Weights{"coding": {Latency: 1}}
	p := NewProvider([]Policy{pol}, "test")
	cands := wave3PolicyCandidates()
	cands[0].EWMALatencyMS = 1000
	cands[1].EWMALatencyMS = 100
	req := decision.DecisionRequest{
		Candidates:  cands,
		PolicyID:    "test",
		TaskProfile: decision.TaskProfile{Type: "coding"},
	}
	res, err := p.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("task-aware decide must not error: %v", err)
	}
	if res.Action != decision.ActionSelect || res.SelectedID != "b" {
		t.Fatalf("task-aware weights must select the faster challenger: %+v", res)
	}
	found := false
	for _, rc := range res.ReasonCodes {
		if rc == decision.ReasonTaskAwareWeights {
			found = true
		}
	}
	if !found {
		t.Fatalf("task-aware reason missing: %v", res.ReasonCodes)
	}
	if res.PolicyTrace == nil || !res.PolicyTrace.ChangedPrimary || res.PolicyTrace.TaskType != "coding" {
		t.Fatalf("policy trace must show a task-driven primary change: %+v", res.PolicyTrace)
	}
}

func TestWave3DecideContextRequirementFallsBackToEstimate(t *testing.T) {
	pol := makePolicy("test", Weights{Context: 1}, 0)
	p := NewProvider([]Policy{pol}, "test")
	cands := wave3PolicyCandidates()
	cands[0].ContextWindow = 100
	cands[1].ContextWindow = 100000
	// No explicit MinContextWindow: the estimated tokens must supply
	// the requirement (40000 + 10000 = 50000), so only the large
	// window clears it and must be selected.
	withEstimate := decision.DecisionRequest{
		Candidates:           cands,
		PolicyID:             "test",
		EstimatedInputTokens: 40000,
		MaxOutputTokens:      10000,
	}
	res, err := p.Decide(context.Background(), withEstimate)
	if err != nil {
		t.Fatalf("decide must not error: %v", err)
	}
	if res.Action != decision.ActionSelect || res.SelectedID != "b" {
		t.Fatalf("estimated requirement must select the large window: %+v", res)
	}
	// Control: with no requirement at all, context scores are neutral
	// and the original primary is preserved.
	withoutEstimate := decision.DecisionRequest{Candidates: cands, PolicyID: "test"}
	res, err = p.Decide(context.Background(), withoutEstimate)
	if err != nil {
		t.Fatalf("decide must not error: %v", err)
	}
	if res.Action != decision.ActionAbstain {
		t.Fatalf("no requirement must preserve the original primary: %+v", res)
	}
}
