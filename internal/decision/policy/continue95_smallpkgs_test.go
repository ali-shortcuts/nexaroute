package policy

import (
	"math"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

func TestContinue95ExplainClamps(t *testing.T) {
	out := Explain([]ScoredCandidate{{
		Candidate:     decision.Candidate{ID: "neg"},
		Components:    map[string]float64{"quality": -2},
		WeightedScore: -2.5,
	}}, Weights{})
	if len(out) != 1 || out[0].WeightedScore != 0 || out[0].Components["quality"] != 0 {
		t.Errorf("Explain negative clamp = %+v, want scores clamped to 0", out)
	}
	out = Explain([]ScoredCandidate{{
		Candidate:     decision.Candidate{ID: "nan"},
		Components:    map[string]float64{"quality": math.NaN()},
		WeightedScore: math.NaN(),
	}}, Weights{})
	if len(out) != 1 || out[0].WeightedScore != 0.5 || out[0].Components["quality"] != 0.5 {
		t.Errorf("Explain NaN clamp = %+v, want 0.5 neutral", out)
	}
}

func TestContinue95MarshalBreakdownError(t *testing.T) {
	got := MarshalBreakdown([]PolicyScoreBreakdown{{
		CandidateID: "x",
		Components:  map[string]float64{"a": math.NaN()},
	}})
	if got != "[]" {
		t.Errorf("MarshalBreakdown(NaN) = %q, want \"[]\"", got)
	}
}

func TestContinue95WeightedSumOverflowScore(t *testing.T) {
	// Total weight is a denormal (~1e-320) while the weighted sum is ~1e308,
	// so sum/total overflows to +Inf and must fall back to neutral 0.5.
	// (Negative weights are not validated by WeightedSum itself.)
	w := Weights{RouterBaseline: 1e308, Reliability: -1e308, Latency: 1e-320}
	components := map[string]float64{CompRouterBaseline: 1.0, CompReliability: 0, CompLatency: 0}
	if got := WeightedSum(components, w); got != 0.5 {
		t.Errorf("WeightedSum overflow score = %v, want 0.5", got)
	}
}

func TestContinue95DeriveVersionMarshalError(t *testing.T) {
	got := deriveVersion(Policy{ID: "p", Weights: Weights{RouterBaseline: math.NaN()}})
	if got != VersionPrefix+"unavailable" {
		t.Errorf("deriveVersion(NaN weight) = %q, want %q", got, VersionPrefix+"unavailable")
	}
}

func TestContinue95ScoreEmptyCandidates(t *testing.T) {
	if got := ScoreCandidatesWithObservations(nil, 0, 0); got != nil {
		t.Errorf("ScoreCandidatesWithObservations(nil) = %v, want nil", got)
	}
	if got := ScoreCandidates(nil, 0); got != nil {
		t.Errorf("ScoreCandidates(nil) = %v, want nil", got)
	}
}
