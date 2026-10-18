package policy

import (
	"context"
	"math"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

// Phase 19 feedback loop (#510 item 5): the quality component
// consumes per-task scorecard evidence carried on candidates.
// Thin evidence blends to neutral; rich evidence shifts the
// selection; a task override is where the operator turns it on.

// effectiveSelected reads the winner from the trace when the
// provider abstains to preserve the existing order (its way of
// saying "the primary already wins").
func effectiveSelected(res decision.DecisionResult) string {
	if res.SelectedID != "" {
		return res.SelectedID
	}
	if res.PolicyTrace != nil {
		return res.PolicyTrace.SelectedID
	}
	return ""
}

func qualityCandidates(aSamples int, aScore float64, bSamples int, bScore float64) []decision.Candidate {
	return []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, OriginalRank: 0, Priority: 10, RouterScore: 0.9, TaskQuality: aScore, TaskQualitySamples: aSamples},
		{ID: "b", PoolOrdinal: 0, OriginalRank: 1, Priority: 10, RouterScore: 0.1, TaskQuality: bScore, TaskQualitySamples: bSamples},
	}
}

func TestQualityComponent_Blending(t *testing.T) {
	// No samples: exactly neutral, whatever the score claims.
	if got := qualityComponent(decision.Candidate{TaskQuality: 1, TaskQualitySamples: 0}); got != 0.5 {
		t.Fatalf("no-sample quality = %v, want neutral 0.5", got)
	}
	// One sample of 32: mostly neutral.
	got := qualityComponent(decision.Candidate{TaskQuality: 1, TaskQualitySamples: 1})
	if got <= 0.5 || got > 0.52 {
		t.Fatalf("thin quality = %v, want a small step above neutral", got)
	}
	// Full confidence: the evidence speaks at full strength.
	if got := qualityComponent(decision.Candidate{TaskQuality: 0.9, TaskQualitySamples: 32}); math.Abs(got-0.9) > 1e-9 {
		t.Fatalf("rich quality = %v, want 0.9", got)
	}
	if got := qualityComponent(decision.Candidate{TaskQuality: 0.1, TaskQualitySamples: 64}); math.Abs(got-0.1) > 1e-9 {
		t.Fatalf("saturated quality = %v, want 0.1", got)
	}
}

func TestProvider_TaskQualityShiftsSelection(t *testing.T) {
	// Global weights use only the router baseline; the coding
	// task override turns quality on for coding requests.
	pol := makePolicy("q", Weights{RouterBaseline: 1}, 0)
	pol.TaskOverrides = map[string]Weights{"coding": {RouterBaseline: 1, Quality: 4}}
	p := NewProvider([]Policy{pol}, "q")
	req := decision.DecisionRequest{
		PolicyID:    "q",
		TaskProfile: decision.TaskProfile{Type: taskprofile.TaskCoding},
		// b has weaker baseline position but far stronger coding
		// evidence with deep samples.
		Candidates: qualityCandidates(40, 0.2, 40, 0.95),
	}
	res, err := p.Decide(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedID != "b" {
		t.Fatalf("selected = %q, want b (quality evidence must shift the coding route)", res.SelectedID)
	}
	// Same request as general chat: no override, no quality
	// weight — baseline order (a first) stands.
	req.TaskProfile = decision.TaskProfile{Type: taskprofile.TaskGeneral}
	res2, err := p.Decide(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := effectiveSelected(res2); got != "a" {
		t.Fatalf("selected = %q, want a (no quality weight outside the override)", got)
	}
	// Thin evidence on both sides: baseline order stands.
	req.TaskProfile = decision.TaskProfile{Type: taskprofile.TaskCoding}
	req.Candidates = qualityCandidates(1, 0.0, 1, 1.0)
	res3, err := p.Decide(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := effectiveSelected(res3); got != "a" {
		t.Fatalf("selected = %q, want a (one-sample evidence must not flap the route)", got)
	}
}
