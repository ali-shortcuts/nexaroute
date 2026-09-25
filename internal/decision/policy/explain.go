package policy

import (
	"encoding/json"
	"math"
)

// PolicyScoreBreakdown is a privacy-safe explanation of policy scoring.
// Contains only bounded fields, no raw prompts or secrets.
type PolicyScoreBreakdown struct {
	CandidateID   string             `json:"candidate_id"`
	PoolID        string             `json:"pool_id,omitempty"`
	PoolOrdinal   int                `json:"pool_ordinal"`
	Priority      int                `json:"priority"`
	OriginalRank  int                `json:"original_rank"`
	Components    map[string]float64 `json:"components"`
	WeightedScore float64            `json:"weighted_score"`
	Weights       Weights            `json:"weights"`
}

// Explain returns breakdowns for scored candidates, bounded and privacy-safe.
func Explain(scored []ScoredCandidate, w Weights) []PolicyScoreBreakdown {
	if len(scored) == 0 {
		return nil
	}
	out := make([]PolicyScoreBreakdown, 0, len(scored))
	for _, sc := range scored {
		// Ensure components finite and clamped
		comps := make(map[string]float64, len(sc.Components))
		for k, v := range sc.Components {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				comps[k] = 0.5
			} else {
				if v < 0 {
					v = 0
				}
				if v > 1 {
					v = 1
				}
				comps[k] = v
			}
		}
		ws := sc.WeightedScore
		if math.IsNaN(ws) || math.IsInf(ws, 0) {
			ws = 0.5
		}
		if ws < 0 {
			ws = 0
		}
		if ws > 1 {
			ws = 1
		}
		out = append(out, PolicyScoreBreakdown{
			CandidateID:   sc.Candidate.ID,
			PoolID:        sc.Candidate.PoolID,
			PoolOrdinal:   sc.Candidate.PoolOrdinal,
			Priority:      sc.Candidate.Priority,
			OriginalRank:  sc.OriginalRank,
			Components:    comps,
			WeightedScore: ws,
			Weights:       w,
		})
	}
	return out
}

// MarshalBreakdown marshals breakdowns to JSON with bounded size (for events).
// Returns truncated JSON if too large.
func MarshalBreakdown(breakdowns []PolicyScoreBreakdown) string {
	if len(breakdowns) == 0 {
		return "[]"
	}
	// Bound to first 10 for telemetry
	if len(breakdowns) > 10 {
		breakdowns = breakdowns[:10]
	}
	b, err := json.Marshal(breakdowns)
	if err != nil {
		return "[]"
	}
	const maxLen = 4096
	if len(b) > maxLen {
		return string(b[:maxLen])
	}
	return string(b)
}
