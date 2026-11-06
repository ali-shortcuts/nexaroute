package scorecards

import (
	"math"
	"testing"
	"time"
)

// Agent F coverage: branches left uncovered in the 2026-11-06 baseline.

// TestAgentFCardDecaysToNothing verifies evidence fully decays away: after
// enough half-lives that the decay factor underflows to zero, no card is
// produced instead of a card built from zero evidence.
func TestAgentFCardDecaysToNothing(t *testing.T) {
	tr := NewFeedbackTracker(time.Hour, 0)
	tr.Observe("coding", "prov/model", FeedbackOutcome{Success: true, LatencyMS: 100}, fbT0)
	// 2000 half-lives: 0.5^2000 underflows to exactly 0, so no effective
	// evidence remains. (After 12 half-lives the card still exists with
	// fractional evidence; only total decay removes it.)
	if _, ok := tr.Card("coding", "prov/model", fbT0.Add(2000*time.Hour)); ok {
		t.Fatal("fully decayed evidence must not produce a card")
	}
}

// TestAgentFLatencyScoreClampedAtOne verifies the latency score never
// exceeds 1 even for a corrupt aggregate with a negative latency sum.
// Such an aggregate cannot arise through Observe (negative latencies are
// sanitized to zero there); this pins the defense-in-depth clamp in
// cardLocked itself.
func TestAgentFLatencyScoreClampedAtOne(t *testing.T) {
	tr := NewFeedbackTracker(time.Hour, 100)
	at := fbT0
	a := &feedbackAgg{
		taskType: "coding", deploymentID: "p/m",
		succ: 1, latencySum: -10, latencyN: 1, last: at,
	}
	card, ok := tr.cardLocked(a, at)
	if !ok {
		t.Fatal("card must exist for one success sample")
	}
	lat, present := card.Values[DimLatencyMS]
	if !present {
		t.Fatal("latency value must be present when a target is configured")
	}
	// Mean latency is -10ms, so the raw score 1-(-10)/100 = 1.1 clamps to 1.
	if math.Abs(lat.Score-1) > 1e-9 {
		t.Fatalf("latency score = %v, want clamped to 1", lat.Score)
	}
	if lat.Raw != -10 {
		t.Fatalf("latency raw = %v, want the recorded mean -10", lat.Raw)
	}
}
