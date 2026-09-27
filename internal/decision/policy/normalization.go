package policy

import (
	"math"
)

// WeightedSum computes weighted sum / total positive weight.
// Returns 0.5 neutral if total weight is zero or not finite (should not happen due to validation).
func WeightedSum(components map[string]float64, w Weights) float64 {
	total := w.TotalWeight()
	if total <= 0 || math.IsNaN(total) || math.IsInf(total, 0) {
		return 0.5
	}
	sum := 0.0
	sum += components[CompRouterBaseline] * w.RouterBaseline
	sum += components[CompReliability] * w.Reliability
	sum += components[CompLatency] * w.Latency
	sum += components[CompTTFT] * w.TTFT
	sum += components[CompCapacity] * w.Capacity
	sum += components[CompCost] * w.Cost
	sum += components[CompContext] * w.Context

	if math.IsNaN(sum) || math.IsInf(sum, 0) {
		return 0.5
	}
	// Normalize
	score := sum / total
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return 0.5
	}
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}

// ApplyWeights computes weighted scores for all scored candidates.
func ApplyWeights(scored []ScoredCandidate, w Weights) []ScoredCandidate {
	if len(scored) == 0 {
		return nil
	}
	out := make([]ScoredCandidate, len(scored))
	for i, sc := range scored {
		ws := WeightedSum(sc.Components, w)
		out[i] = sc
		out[i].WeightedScore = ws
	}
	return out
}
