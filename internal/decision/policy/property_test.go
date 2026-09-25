package policy

import (
	"context"
	"math"
	"math/rand"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

func TestProperty_EligibleSetPreserved(t *testing.T) {
	pol := Policy{
		ID:            "test",
		SelectionMode: "select_first",
		Weights:       Weights{RouterBaseline: 1, Reliability: 1, Latency: 1},
		MinScoreDelta: 0,
	}
	p := NewProvider([]Policy{pol}, "test")
	for iter := 0; iter < 200; iter++ {
		n := rand.Intn(20) + 1
		cands := make([]decision.Candidate, n)
		for i := 0; i < n; i++ {
			cands[i] = decision.Candidate{
				ID:           string(rune('a'+i%26)) + string(rune('0'+i/26)),
				PoolOrdinal:  rand.Intn(3),
				Priority:     rand.Intn(5),
				RouterScore:  rand.Float64(),
				OriginalRank: i,
			}
		}
		req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", RequestID: "test"}
		res, _ := p.Decide(context.Background(), req)
		// After normalization in orchestrator, ordered set should have same IDs
		// Here we just check provider does not invent IDs
		if res.Action == decision.ActionSelect {
			found := false
			for _, c := range cands {
				if c.ID == res.SelectedID {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("selected ID %q not in eligible set", res.SelectedID)
			}
		}
	}
}

func TestProperty_PoolBoundaryEnforced(t *testing.T) {
	pol := Policy{
		ID:            "test",
		SelectionMode: "select_first",
		Weights:       Weights{RouterBaseline: 1},
		MinScoreDelta: 0,
	}
	p := NewProvider([]Policy{pol}, "test")
	for iter := 0; iter < 100; iter++ {
		cands := []decision.Candidate{
			{ID: "a", PoolOrdinal: 0, Priority: 10, OriginalRank: 0, RouterScore: rand.Float64()},
			{ID: "b", PoolOrdinal: 1, Priority: 10, OriginalRank: 1, RouterScore: rand.Float64()},
			{ID: "c", PoolOrdinal: 0, Priority: 10, OriginalRank: 2, RouterScore: rand.Float64()},
		}
		req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", RequestID: "test"}
		res, _ := p.Decide(context.Background(), req)
		if res.Action == decision.ActionSelect {
			// Must be from min ordinal 0
			for _, c := range cands {
				if c.ID == res.SelectedID && c.PoolOrdinal != 0 {
					t.Fatalf("selected from non-min pool ordinal: %v", res.SelectedID)
				}
			}
		}
	}
}

func TestProperty_PriorityBoundaryWithoutAffinity(t *testing.T) {
	pol := Policy{
		ID:            "test",
		SelectionMode: "select_first",
		Weights:       Weights{RouterBaseline: 1},
		MinScoreDelta: 0,
	}
	p := NewProvider([]Policy{pol}, "test")
	for iter := 0; iter < 100; iter++ {
		cands := []decision.Candidate{
			{ID: "a", PoolOrdinal: 0, Priority: 0, OriginalRank: 0, RouterScore: rand.Float64()},
			{ID: "b", PoolOrdinal: 0, Priority: 10, OriginalRank: 1, RouterScore: rand.Float64()},
			{ID: "c", PoolOrdinal: 0, Priority: 0, OriginalRank: 2, RouterScore: rand.Float64()},
		}
		req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", RequestID: "test"}
		res, _ := p.Decide(context.Background(), req)
		if res.Action == decision.ActionSelect {
			// Must be from min priority 0
			for _, c := range cands {
				if c.ID == res.SelectedID && c.Priority != 0 {
					t.Fatalf("without affinity, selected from non-min priority: %v", res.SelectedID)
				}
			}
		}
	}
}

func TestProperty_AffinityPreserved(t *testing.T) {
	pol := Policy{
		ID:            "test",
		SelectionMode: "select_first",
		Weights:       Weights{RouterBaseline: 1},
		MinScoreDelta: 0,
	}
	p := NewProvider([]Policy{pol}, "test")
	for iter := 0; iter < 100; iter++ {
		cands := []decision.Candidate{
			{ID: "a", PoolOrdinal: 0, Priority: 0, OriginalRank: 0, RouterScore: 0.9},
			{ID: "b", PoolOrdinal: 0, Priority: 10, OriginalRank: 1, RouterScore: 0.1},
		}
		req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", PinnedCandidateID: "b", RequestID: "test"}
		res, _ := p.Decide(context.Background(), req)
		if res.SelectedID != "b" {
			t.Fatalf("with eligible affinity, expected b, got %s", res.SelectedID)
		}
	}
}

func TestProperty_Deterministic(t *testing.T) {
	pol := Policy{
		ID:            "test",
		SelectionMode: "select_first",
		Weights:       Weights{RouterBaseline: 1, Reliability: 1, Latency: 1, Capacity: 1},
		MinScoreDelta: 0.05,
	}
	p := NewProvider([]Policy{pol}, "test")
	cands := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, Priority: 10, RouterScore: 0.5, EWMALatencyMS: 100, Successes: 5, OriginalRank: 0},
		{ID: "b", PoolOrdinal: 0, Priority: 10, RouterScore: 0.6, EWMALatencyMS: 200, Successes: 5, OriginalRank: 1},
		{ID: "c", PoolOrdinal: 0, Priority: 10, RouterScore: 0.4, EWMALatencyMS: 150, Successes: 5, OriginalRank: 2},
	}
	req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", RequestID: "test", MinContextWindow: 1000}
	res1, _ := p.Decide(context.Background(), req)
	res2, _ := p.Decide(context.Background(), req)
	if res1.SelectedID != res2.SelectedID || res1.Action != res2.Action {
		t.Fatalf("non-deterministic: %v vs %v", res1, res2)
	}
}

func TestProperty_ScoreComponentsFinite(t *testing.T) {
	for iter := 0; iter < 200; iter++ {
		cands := make([]decision.Candidate, 10)
		for i := 0; i < 10; i++ {
			// Random telemetry including NaN, Inf, negative, huge
			var rs, lat, ttft, pressure, cost, failure float64
			switch rand.Intn(7) {
			case 0:
				rs = math.NaN()
			case 1:
				rs = math.Inf(1)
			case 2:
				rs = math.Inf(-1)
			case 3:
				rs = -100
			case 4:
				rs = 1e9
			default:
				rs = rand.Float64()*2 - 0.5
			}
			switch rand.Intn(5) {
			case 0:
				lat = math.NaN()
			case 1:
				lat = math.Inf(1)
			case 2:
				lat = -10
			default:
				lat = rand.Float64() * 1000
			}
			ttft = lat
			pressure = rand.Float64()*10 - 2
			cost = rand.Float64()*0.1 - 0.01
			failure = rand.Float64()*2 - 0.2
			cands[i] = decision.Candidate{
				ID:               string(rune('a' + i)),
				PoolOrdinal:      rand.Intn(2),
				Priority:         rand.Intn(3),
				RouterScore:      rs,
				HealthStatus:     []string{"healthy", "unknown", "degraded", "half_open", "cooldown"}[rand.Intn(5)],
				EWMALatencyMS:    lat,
				EWMATTFTMS:       ttft,
				EWMAFailureRate:  failure,
				Successes:        int64(rand.Intn(10)),
				Failures:         int64(rand.Intn(10)),
				CapacityPressure: pressure,
				EstimatedCostUSD: cost,
				PriceKnown:       rand.Intn(2) == 0,
				ContextWindow:    rand.Intn(200000) - 1000,
				OriginalRank:     i,
			}
		}
		required := rand.Intn(20000) - 1000
		scored := ScoreCandidates(cands, required)
		for _, sc := range scored {
			for k, v := range sc.Components {
				if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
					t.Fatalf("iter %d component %s not finite [0,1]: %v candidate %v", iter, k, v, sc.Candidate)
				}
			}
		}
	}
}

func TestProperty_NoPanicRandom(t *testing.T) {
	pol := Policy{
		ID:            "test",
		SelectionMode: "select_first",
		Weights:       Weights{RouterBaseline: 1, Reliability: 1, Latency: 1, TTFT: 1, Capacity: 1, Cost: 1, Context: 1},
		MinScoreDelta: 0.1,
	}
	p := NewProvider([]Policy{pol}, "test")
	for iter := 0; iter < 200; iter++ {
		n := rand.Intn(20) + 1
		cands := make([]decision.Candidate, n)
		for i := 0; i < n; i++ {
			cands[i] = decision.Candidate{
				ID:               string(rune('a'+i%26)) + string(rune('0'+i/26)),
				PoolOrdinal:      rand.Intn(3) - 1, // include negative invalid
				Priority:         rand.Intn(5) - 1,
				RouterScore:      rand.NormFloat64() * 100,
				HealthStatus:     []string{"healthy", "unknown", "", "degraded", "cooldown", "half_open", "invalid"}[rand.Intn(7)],
				EWMALatencyMS:    rand.NormFloat64() * 1000,
				EWMATTFTMS:       rand.NormFloat64() * 1000,
				EWMAFailureRate:  rand.NormFloat64(),
				Successes:        int64(rand.Intn(20) - 5),
				Failures:         int64(rand.Intn(20) - 5),
				CapacityPressure: rand.NormFloat64() * 10,
				EstimatedCostUSD: rand.NormFloat64(),
				PriceKnown:       rand.Intn(2) == 0,
				ContextWindow:    rand.Intn(300000) - 50000,
				OriginalRank:     i,
			}
			if rand.Intn(10) == 0 {
				cands[i].RouterScore = math.NaN()
			}
			if rand.Intn(10) == 0 {
				cands[i].EWMALatencyMS = math.Inf(1)
			}
		}
		req := decision.DecisionRequest{
			Candidates:           cands,
			PolicyID:             "test",
			MinContextWindow:     rand.Intn(20000) - 1000,
			EstimatedInputTokens: rand.Intn(10000),
			MaxOutputTokens:      rand.Intn(4000),
			RequestID:            "test",
		}
		if rand.Intn(5) == 0 {
			req.PinnedCandidateID = cands[rand.Intn(len(cands))].ID
		}
		// Should not panic
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Fatalf("panic on iter %d: %v", iter, rec)
				}
			}()
			_, _ = p.Decide(context.Background(), req)
		}()
	}
}
