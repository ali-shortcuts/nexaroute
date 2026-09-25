package policy

import (
	"math"
	"strings"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

// component names for explainability
const (
	CompRouterBaseline = "router_baseline"
	CompReliability    = "reliability"
	CompLatency        = "latency"
	CompTTFT           = "ttft"
	CompCapacity       = "capacity"
	CompCost           = "cost"
	CompContext        = "context"
)

// clamp01 clamps v to [0,1], returns 0.5 if not finite.
func clamp01(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0.5
	}
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func finiteOrNeutral(v float64, neutral float64) (float64, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return neutral, false
	}
	return v, true
}

// healthScore maps health status to 0..1, unknown -> 0.5
func healthScore(status string) float64 {
	s := strings.TrimSpace(strings.ToLower(status))
	switch s {
	case "healthy":
		return 1.0
	case "unknown", "":
		return 0.5
	case "half_open":
		return 0.3
	case "degraded":
		return 0.2
	case "cooldown":
		return 0.0
	case "unavailable":
		return 0.0
	default:
		return 0.5
	}
}

// ScoredCandidate holds component scores and weighted score.
type ScoredCandidate struct {
	Candidate     decision.Candidate
	Components    map[string]float64
	WeightedScore float64
	OriginalRank  int
}

// computeRouterBaseline normalizes router scores across candidates to 0..1.
// Unknown (NaN/Inf) => 0.5. If all same or only one, 0.5 for all.
func computeRouterBaseline(cands []decision.Candidate) map[string]float64 {
	out := make(map[string]float64, len(cands))
	if len(cands) == 0 {
		return out
	}
	// Collect finite scores
	finiteScores := []float64{}
	for _, c := range cands {
		if fv, ok := finiteOrNeutral(c.RouterScore, 0.5); ok {
			// RouterScore may be 0 as valid? 0 is possible but treat as finite.
			// If RouterScore is 0 and we have no other info, we treat 0 as valid only if not both 0 for all?
			// For safety, if RouterScore ==0 and original rank present, we still consider it finite but neutral handling later.
			// To avoid treating 0 as unknown, we check if c.RouterScore is exactly 0 and we have no health etc, we still treat as finite.
			// Actually we already returned finite if not NaN/Inf.
			finiteScores = append(finiteScores, fv)
		}
	}
	if len(finiteScores) == 0 {
		for _, c := range cands {
			out[c.ID] = 0.5
		}
		return out
	}
	min := finiteScores[0]
	max := finiteScores[0]
	for _, v := range finiteScores[1:] {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	delta := max - min
	if delta < 1e-9 {
		for _, c := range cands {
			if _, ok := finiteOrNeutral(c.RouterScore, 0.5); ok {
				out[c.ID] = 0.5
			} else {
				out[c.ID] = 0.5
			}
		}
		return out
	}
	for _, c := range cands {
		if fv, ok := finiteOrNeutral(c.RouterScore, 0.5); ok {
			norm := (fv - min) / delta
			out[c.ID] = clamp01(norm)
		} else {
			out[c.ID] = 0.5
		}
	}
	return out
}

func computeReliability(cands []decision.Candidate) map[string]float64 {
	out := make(map[string]float64, len(cands))
	for _, c := range cands {
		// If no reliability observations yet, treat as neutral, not perfect
		if c.Successes+c.Failures == 0 {
			out[c.ID] = 0.5
			continue
		}
		hs := healthScore(c.HealthStatus)
		// failure rate: 1 - failureRate
		frScore := 0.5
		if fv, ok := finiteOrNeutral(c.EWMAFailureRate, 0.5); ok {
			if fv < 0 {
				fv = 0
			}
			if fv > 1 {
				fv = 1
			}
			frScore = 1 - fv
		}
		// Combine health and failure rate average, but if health unknown (0.5) and failure unknown (0.5) => 0.5
		combined := (hs + frScore) / 2
		out[c.ID] = clamp01(combined)
	}
	return out
}

func computeLatency(cands []decision.Candidate, useTTFT bool) map[string]float64 {
	out := make(map[string]float64, len(cands))
	// Collect known latencies
	known := []float64{}
	for _, c := range cands {
		var lat float64
		if useTTFT {
			lat = c.EWMATTFTMS
		} else {
			lat = c.EWMALatencyMS
		}
		if fv, ok := finiteOrNeutral(lat, 0.5); ok && fv > 0 {
			known = append(known, fv)
		}
	}
	if len(known) == 0 {
		for _, c := range cands {
			out[c.ID] = 0.5
		}
		return out
	}
	min := known[0]
	max := known[0]
	for _, v := range known[1:] {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	delta := max - min
	if delta < 1e-9 {
		for _, c := range cands {
			var lat float64
			if useTTFT {
				lat = c.EWMATTFTMS
			} else {
				lat = c.EWMALatencyMS
			}
			if fv, ok := finiteOrNeutral(lat, 0.5); ok && fv > 0 {
				out[c.ID] = 0.5
			} else {
				out[c.ID] = 0.5
			}
		}
		return out
	}
	for _, c := range cands {
		var lat float64
		if useTTFT {
			lat = c.EWMATTFTMS
		} else {
			lat = c.EWMALatencyMS
		}
		if fv, ok := finiteOrNeutral(lat, 0.5); ok && fv > 0 {
			// lower latency => higher score: 1 - (lat-min)/delta
			norm := 1 - (fv-min)/delta
			out[c.ID] = clamp01(norm)
		} else {
			out[c.ID] = 0.5
		}
	}
	return out
}

func computeCapacity(cands []decision.Candidate) map[string]float64 {
	out := make(map[string]float64, len(cands))
	for _, c := range cands {
		if fv, ok := finiteOrNeutral(c.CapacityPressure, 0.5); ok {
			if fv < 0 {
				fv = 0
			}
			if fv > 4 {
				fv = 4
			}
			score := 1 - fv/4
			out[c.ID] = clamp01(score)
		} else {
			out[c.ID] = 0.5
		}
	}
	return out
}

func computeCost(cands []decision.Candidate) map[string]float64 {
	out := make(map[string]float64, len(cands))
	known := []float64{}
	for _, c := range cands {
		if c.PriceKnown {
			if fv, ok := finiteOrNeutral(c.EstimatedCostUSD, 0.5); ok && fv >= 0 {
				known = append(known, fv)
			}
		}
	}
	if len(known) == 0 {
		for _, c := range cands {
			out[c.ID] = 0.5
		}
		return out
	}
	min := known[0]
	max := known[0]
	for _, v := range known[1:] {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	delta := max - min
	if delta < 1e-9 {
		for _, c := range cands {
			if c.PriceKnown {
				out[c.ID] = 0.5
			} else {
				out[c.ID] = 0.5
			}
		}
		return out
	}
	for _, c := range cands {
		if c.PriceKnown {
			if fv, ok := finiteOrNeutral(c.EstimatedCostUSD, 0.5); ok && fv >= 0 {
				norm := 1 - (fv-min)/delta
				out[c.ID] = clamp01(norm)
			} else {
				out[c.ID] = 0.5
			}
		} else {
			// PriceKnown false != free, neutral 0.5
			out[c.ID] = 0.5
		}
	}
	return out
}

func computeContext(cands []decision.Candidate, required int) map[string]float64 {
	out := make(map[string]float64, len(cands))
	// If no meaningful request requirement, neutral for all
	if required <= 0 {
		for _, c := range cands {
			out[c.ID] = 0.5
		}
		return out
	}
	for _, c := range cands {
		// Unknown context window -> neutral
		if c.ContextWindow <= 0 {
			out[c.ID] = 0.5
			continue
		}
		// Defensive: if window < required, already should be filtered by router, do not reward
		if c.ContextWindow < required {
			out[c.ID] = 0.0
			continue
		}
		// Headroom = (window - required)/window clamped [0,1]
		headroom := float64(c.ContextWindow-required) / float64(c.ContextWindow)
		out[c.ID] = clamp01(headroom)
	}
	return out
}

// ScoreCandidates computes component scores for each candidate and returns breakdown.
// required is MinContextWindow (or EstimatedInput+MaxOutput) for request-relative headroom.
func ScoreCandidates(cands []decision.Candidate, required int) []ScoredCandidate {
	if len(cands) == 0 {
		return nil
	}
	routerScores := computeRouterBaseline(cands)
	reliabilityScores := computeReliability(cands)
	latencyScores := computeLatency(cands, false)
	ttftScores := computeLatency(cands, true)
	capacityScores := computeCapacity(cands)
	costScores := computeCost(cands)
	contextScores := computeContext(cands, required)

	out := make([]ScoredCandidate, 0, len(cands))
	for _, c := range cands {
		comp := map[string]float64{
			CompRouterBaseline: routerScores[c.ID],
			CompReliability:    reliabilityScores[c.ID],
			CompLatency:        latencyScores[c.ID],
			CompTTFT:           ttftScores[c.ID],
			CompCapacity:       capacityScores[c.ID],
			CompCost:           costScores[c.ID],
			CompContext:        contextScores[c.ID],
		}
		// Ensure all finite clamped
		for k, v := range comp {
			comp[k] = clamp01(v)
		}
		out = append(out, ScoredCandidate{
			Candidate:    c,
			Components:   comp,
			OriginalRank: c.OriginalRank,
		})
	}
	return out
}
