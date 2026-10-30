package scorecards

import (
	"testing"
	"time"
)

func TestIssue663CoreDimensionValidQuality(t *testing.T) {
	for _, d := range QualityDimensions {
		if !d.ValidQuality() {
			t.Errorf("quality dimension %q must report ValidQuality", d)
		}
		if !d.Valid() {
			t.Errorf("quality dimension %q must report Valid", d)
		}
	}
	for _, d := range []Dimension{DimLatencyMS, DimTTFTMS, DimThroughputTPS, DimAvailability, DimFailureRate, DimInputCostPerMTok, DimOutputCostPerMTok} {
		if d.ValidQuality() {
			t.Errorf("non-quality dimension %q must not report ValidQuality", d)
		}
		if !d.Valid() {
			t.Errorf("dimension %q must report Valid", d)
		}
	}
	if Dimension("not-a-dimension").ValidQuality() || Dimension("not-a-dimension").Valid() {
		t.Error("unknown dimension must report neither ValidQuality nor Valid")
	}
	if Dimension("").ValidQuality() || Dimension("").Valid() {
		t.Error("empty dimension must report neither ValidQuality nor Valid")
	}
}

func TestIssue663CoreProvenanceMeasured(t *testing.T) {
	measured := []Provenance{ProvenanceImported, ProvenanceEvaluation, ProvenanceProductionTelemetry}
	for _, p := range measured {
		if !p.Measured() {
			t.Errorf("provenance %q must report Measured", p)
		}
		if !p.Valid() {
			t.Errorf("provenance %q must report Valid", p)
		}
	}
	if ProvenanceOperatorConfig.Measured() {
		t.Error("operator_config is a declaration, not a measurement")
	}
	if !ProvenanceOperatorConfig.Valid() {
		t.Error("operator_config must still report Valid")
	}
	if Provenance("invented").Measured() || Provenance("invented").Valid() {
		t.Error("unknown provenance must report neither Measured nor Valid")
	}
}

func TestIssue663CoreConfidenceFromSamples(t *testing.T) {
	cases := []struct {
		n    int
		want float64
	}{
		{-5, 0},
		{0, 0},
		{1, 1.0 / float64(FullConfidenceSamples)},
		{FullConfidenceSamples / 2, 0.5},
		{FullConfidenceSamples - 1, float64(FullConfidenceSamples-1) / float64(FullConfidenceSamples)},
		{FullConfidenceSamples, 1},
		{FullConfidenceSamples + 100, 1},
	}
	for _, tc := range cases {
		if got := ConfidenceFromSamples(tc.n); got != tc.want {
			t.Errorf("ConfidenceFromSamples(%d) = %v, want %v", tc.n, got, tc.want)
		}
	}
	// Confidence is monotone: more samples never lower it.
	prev := 0.0
	for n := 0; n <= FullConfidenceSamples+5; n++ {
		if got := ConfidenceFromSamples(n); got < prev {
			t.Fatalf("confidence decreased at n=%d: %v < %v", n, got, prev)
		} else {
			prev = got
		}
	}
}

func TestIssue663CoreNewFeedbackTracker(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	// Non-positive half-life selects the default; a tracker built that
	// way still aggregates and decays like any other tracker.
	def := NewFeedbackTracker(0, 0)
	if def == nil {
		t.Fatal("default tracker must not be nil")
	}
	def.Observe("task-a", "dep-1", FeedbackOutcome{Success: true, LatencyMS: 100}, now)
	def.Observe("task-a", "dep-1", FeedbackOutcome{Success: false, LatencyMS: 200}, now)
	card, ok := def.Card("task-a", "dep-1", now)
	if !ok {
		t.Fatal("card must exist after observations")
	}
	avail, ok := card.Values[DimAvailability]
	if !ok {
		t.Fatal("card must carry an availability value")
	}
	if avail.Score != 0.5 || avail.SampleCount != 2 || avail.Provenance != ProvenanceProductionTelemetry {
		t.Fatalf("availability value wrong: %+v", avail)
	}
	if _, ok := card.Values[DimLatencyMS]; ok {
		t.Error("without a latency target the latency dimension must be omitted, not invented")
	}
	// A negative half-life also selects the default.
	neg := NewFeedbackTracker(-time.Hour, 0)
	neg.Observe("task-a", "dep-1", FeedbackOutcome{Success: true}, now)
	if _, ok := neg.Card("task-a", "dep-1", now); !ok {
		t.Fatal("negative half-life tracker must still aggregate")
	}
	// With a latency target, the latency dimension is scored from the
	// observed mean, and cost notes are recorded.
	withTarget := NewFeedbackTracker(time.Hour, 200)
	withTarget.Observe("task-a", "dep-1", FeedbackOutcome{Success: true, LatencyMS: 100, CostUSD: 0.02, HasCost: true}, now)
	card, ok = withTarget.Card("task-a", "dep-1", now)
	if !ok {
		t.Fatal("targeted card must exist")
	}
	lat, ok := card.Values[DimLatencyMS]
	if !ok {
		t.Fatal("with a latency target the latency dimension must be present")
	}
	if lat.Score != 0.5 || lat.Raw != 100 {
		t.Fatalf("latency value wrong: %+v", lat)
	}
	if len(card.Notes) != 1 {
		t.Fatalf("cost note missing: %+v", card.Notes)
	}
	// Invalid identifiers are ignored, never breaking the path.
	withTarget.Observe("", "dep-1", FeedbackOutcome{Success: true}, now)
	withTarget.Observe("task-a", "", FeedbackOutcome{Success: true}, now)
	if _, ok := withTarget.Card("", "dep-1", now); ok {
		t.Error("empty task identifier must not create a card")
	}
	// No evidence at all: no card.
	if _, ok := withTarget.Card("nope", "dep-1", now); ok {
		t.Error("unknown pair must not produce a card")
	}
	// A latency far above the target clamps the score at zero, and
	// invalid outcome fields are sanitized rather than recorded.
	slow := NewFeedbackTracker(time.Hour, 100)
	slow.Observe("task-a", "dep-1", FeedbackOutcome{Success: true, LatencyMS: 10000, CostUSD: -5, HasCost: true}, now)
	slow.Observe("task-a", "dep-1", FeedbackOutcome{Success: true, LatencyMS: -1}, now)
	card, ok = slow.Card("task-a", "dep-1", now)
	if !ok {
		t.Fatal("slow card must exist")
	}
	if lat, ok := card.Values[DimLatencyMS]; !ok || lat.Score != 0 {
		t.Fatalf("over-target latency must clamp to zero: %+v %v", lat, ok)
	}
	if len(card.Notes) != 0 {
		t.Fatalf("negative cost must not produce a cost note: %+v", card.Notes)
	}
}
