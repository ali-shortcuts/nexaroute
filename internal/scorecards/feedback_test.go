package scorecards

import (
	"math"
	"testing"
	"time"
)

// Phase 19 feedback loop (#510 item 5): per-(task, deployment)
// aggregation with decay, and task-scoped registry keying.

var fbT0 = time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)

func TestFeedbackTrackerDecayAndConfidence(t *testing.T) {
	tr := NewFeedbackTracker(time.Hour, 0)
	// 32 successes at t0: full confidence, perfect availability.
	for i := 0; i < 32; i++ {
		tr.Observe("coding", "prov/model", FeedbackOutcome{Success: true, LatencyMS: 100}, fbT0)
	}
	card, ok := tr.Card("coding", "prov/model", fbT0)
	if !ok {
		t.Fatal("card missing after observations")
	}
	v := card.Values[DimAvailability]
	if v.Score != 1 || v.SampleCount != 32 || v.Confidence != 1 {
		t.Fatalf("fresh card value = %+v, want score 1, 32 samples, confidence 1", v)
	}
	if v.Provenance != ProvenanceProductionTelemetry {
		t.Fatalf("provenance = %q, want production_telemetry", v.Provenance)
	}
	if card.TaskType != "coding" || card.DeploymentID != "prov/model" {
		t.Fatalf("card keyed %q/%q", card.TaskType, card.DeploymentID)
	}
	// One half-life later the glory has faded: effective samples
	// halve, confidence drops to 0.5, score (all successes) stays 1.
	card2, ok := tr.Card("coding", "prov/model", fbT0.Add(time.Hour))
	if !ok {
		t.Fatal("card missing after one half-life")
	}
	v2 := card2.Values[DimAvailability]
	if v2.SampleCount != 16 || math.Abs(v2.Confidence-0.5) > 1e-9 {
		t.Fatalf("decayed value = %+v, want 16 effective samples, confidence 0.5", v2)
	}
	// Several half-lives: evidence evaporates below one sample.
	card3, ok := tr.Card("coding", "prov/model", fbT0.Add(12*time.Hour))
	if !ok {
		t.Fatal("card should still exist with fractional evidence")
	}
	if got := card3.Values[DimAvailability].SampleCount; got != 0 {
		t.Fatalf("after 12 half-lives effective samples = %d, want 0 (thin data shown as thin)", got)
	}
	// Unknown pairs have no card; nothing is substituted.
	if _, ok := tr.Card("coding", "other/model", fbT0); ok {
		t.Fatal("unknown pair must not produce a card")
	}
}

func TestFeedbackTrackerMixedOutcomesAndLatencyTarget(t *testing.T) {
	tr := NewFeedbackTracker(time.Hour, 200) // 200ms latency target
	at := fbT0
	for i := 0; i < 3; i++ {
		tr.Observe("debugging", "p/m", FeedbackOutcome{Success: true, LatencyMS: 100, CostUSD: 0.01, HasCost: true}, at)
	}
	tr.Observe("debugging", "p/m", FeedbackOutcome{Success: false, LatencyMS: 300}, at)
	card, ok := tr.Card("debugging", "p/m", at)
	if !ok {
		t.Fatal("card missing")
	}
	if got := card.Values[DimAvailability].Score; math.Abs(got-0.75) > 1e-9 {
		t.Fatalf("availability = %v, want 0.75", got)
	}
	lat, has := card.Values[DimLatencyMS]
	if !has {
		t.Fatal("latency value must be present when a target is configured")
	}
	// Mean latency = (3*100 + 300)/4 = 150ms; score = 1 - 150/200 = 0.25.
	if math.Abs(lat.Raw-150) > 1e-9 || math.Abs(lat.Score-0.25) > 1e-9 {
		t.Fatalf("latency value = %+v, want raw 150 score 0.25", lat)
	}
	if len(card.Notes) != 1 {
		t.Fatalf("cost note missing: %v", card.Notes)
	}
	// Without a target, latency is never scored (no invented budget).
	tr2 := NewFeedbackTracker(time.Hour, 0)
	tr2.Observe("debugging", "p/m", FeedbackOutcome{Success: true, LatencyMS: 50}, at)
	card2, _ := tr2.Card("debugging", "p/m", at)
	if _, has := card2.Values[DimLatencyMS]; has {
		t.Fatal("latency must be omitted when no target is configured")
	}
	// Feedback cards validate and upsert into the registry.
	reg := NewRegistry(10, 2)
	if _, err := reg.Upsert(card); err != nil {
		t.Fatalf("feedback card must upsert: %v", err)
	}
	if _, ok := reg.QualityForTask("debugging", "p/m", DimAvailability); !ok {
		t.Fatal("registry must serve the feedback card by (task, deployment)")
	}
}

func TestRegistryTaskKeying(t *testing.T) {
	reg := NewRegistry(10, 2)
	mk := func(task string, score float64) Scorecard {
		return Scorecard{
			DeploymentID: "p/m", TaskType: task, Version: 1, GeneratedAt: fbT0,
			Values: map[Dimension]Value{DimCoding: {Score: score, Provenance: ProvenanceEvaluation, SampleCount: 10, Source: "test", SuiteVersion: "v1", EvaluatedAt: fbT0}},
		}
	}
	if _, err := reg.Upsert(mk("", 0.5)); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Upsert(mk("coding", 0.9)); err != nil {
		t.Fatal(err)
	}
	// The task card does not replace the deployment-wide card.
	if v, ok := reg.Quality("p/m", DimCoding); !ok || v.Score != 0.5 {
		t.Fatalf("deployment-wide quality = %+v ok=%v, want 0.5", v, ok)
	}
	if v, ok := reg.QualityForTask("coding", "p/m", DimCoding); !ok || v.Score != 0.9 {
		t.Fatalf("task quality = %+v ok=%v, want 0.9", v, ok)
	}
	if _, ok := reg.GetTask("debugging", "p/m"); ok {
		t.Fatal("no debugging card exists")
	}
	// Retain keeps task cards of surviving deployments only.
	reg.Retain(map[string]struct{}{"p/m": {}})
	if _, ok := reg.GetTask("coding", "p/m"); !ok {
		t.Fatal("task card must survive Retain for a live deployment")
	}
	reg.Retain(map[string]struct{}{})
	if _, ok := reg.GetTask("coding", "p/m"); ok {
		t.Fatal("task card must be dropped when its deployment is gone")
	}
	// Snapshot carries both scopes; Load accepts a task card and a
	// deployment card for the same deployment (distinct keys).
	reg2 := NewRegistry(10, 2)
	if err := reg2.Load([]Scorecard{mk("", 0.5), mk("coding", 0.9)}); err != nil {
		t.Fatalf("load with task + deployment cards: %v", err)
	}
	if len(reg2.Snapshot()) != 2 {
		t.Fatalf("snapshot = %d cards, want 2", len(reg2.Snapshot()))
	}
	// A task_type with invalid characters is rejected.
	bad := mk("coding;drop", 0.5)
	if _, err := reg.Upsert(bad); err == nil {
		t.Fatal("invalid task_type must be rejected")
	}
}
