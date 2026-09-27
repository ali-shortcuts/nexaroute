package policy

import (
	"math"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

func makeCandidate(id string, opts ...func(*decision.Candidate)) decision.Candidate {
	c := decision.Candidate{
		ID:           id,
		ProviderID:   "p1",
		Priority:     10,
		OriginalRank: 0,
		HealthStatus: "healthy",
	}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func TestRouterBaseline_HigherLower(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.RouterScore = 0.9; c.OriginalRank = 0 }),
		makeCandidate("b", func(c *decision.Candidate) { c.RouterScore = 0.1; c.OriginalRank = 1 }),
	}
	scores := computeRouterBaseline(cands)
	if scores["a"] <= scores["b"] {
		t.Fatalf("expected a > b, got %v vs %v", scores["a"], scores["b"])
	}
	if scores["a"] != 1.0 || scores["b"] != 0.0 {
		t.Fatalf("expected normalized 1 and 0, got %v %v", scores["a"], scores["b"])
	}
}

func TestRouterBaseline_AllEqual(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.RouterScore = 0.5; c.OriginalRank = 0 }),
		makeCandidate("b", func(c *decision.Candidate) { c.RouterScore = 0.5; c.OriginalRank = 1 }),
	}
	scores := computeRouterBaseline(cands)
	if scores["a"] != 0.5 || scores["b"] != 0.5 {
		t.Fatalf("expected neutral 0.5 for equal, got %v %v", scores["a"], scores["b"])
	}
}

func TestRouterBaseline_NaNInf(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.RouterScore = math.NaN(); c.OriginalRank = 0 }),
		makeCandidate("b", func(c *decision.Candidate) { c.RouterScore = math.Inf(1); c.OriginalRank = 1 }),
		makeCandidate("c", func(c *decision.Candidate) { c.RouterScore = 0.8; c.OriginalRank = 2 }),
	}
	scores := computeRouterBaseline(cands)
	if scores["a"] != 0.5 || scores["b"] != 0.5 {
		t.Fatalf("expected neutral for NaN/Inf, got %v %v", scores["a"], scores["b"])
	}
	// c should be 0.5 when only one finite? Actually min==max for single finite => 0.5
	if scores["c"] != 0.5 {
		t.Fatalf("expected 0.5 for single finite, got %v", scores["c"])
	}
}

func TestReliability_UnknownNeutral(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) {
			c.HealthStatus = "unknown"
			c.Successes = 0
			c.Failures = 0
			c.EWMAFailureRate = 0
		}),
	}
	scores := computeReliability(cands)
	if scores["a"] != 0.5 {
		t.Fatalf("expected neutral 0.5 for no observations, got %v", scores["a"])
	}
}

func TestReliability_MeasuredSuccess(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) {
			c.HealthStatus = "healthy"
			c.Successes = 10
			c.Failures = 0
			c.EWMAFailureRate = 0
		}),
		makeCandidate("b", func(c *decision.Candidate) {
			c.HealthStatus = "healthy"
			c.Successes = 0
			c.Failures = 0
			c.EWMAFailureRate = 0
		}),
	}
	scores := computeReliability(cands)
	if scores["b"] != 0.5 {
		t.Fatalf("b should be neutral 0.5, got %v", scores["b"])
	}
	if scores["a"] <= 0.5 {
		t.Fatalf("a with success should be >0.5, got %v", scores["a"])
	}
	if scores["a"] != 1.0 {
		t.Fatalf("expected perfect 1.0 for healthy 0 failure, got %v", scores["a"])
	}
}

func TestReliability_MeasuredMixed(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) {
			c.HealthStatus = "healthy"
			c.Successes = 5
			c.Failures = 5
			c.EWMAFailureRate = 0.5
		}),
		makeCandidate("b", func(c *decision.Candidate) {
			c.HealthStatus = "degraded"
			c.Successes = 1
			c.Failures = 9
			c.EWMAFailureRate = 0.9
		}),
	}
	scores := computeReliability(cands)
	// a: health 1.0 + failure 0.5 => 0.75
	// b: health 0.2 + failure 0.1 => 0.15
	if scores["a"] <= scores["b"] {
		t.Fatalf("expected a > b, got %v vs %v", scores["a"], scores["b"])
	}
}

func TestReliability_InvalidNaNInf(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) {
			c.HealthStatus = "healthy"
			c.Successes = 1
			c.Failures = 1
			c.EWMAFailureRate = math.NaN()
		}),
	}
	scores := computeReliability(cands)
	// NaN failure rate -> frScore 0.5, health 1.0 => 0.75
	if scores["a"] != 0.75 {
		t.Fatalf("expected 0.75 for NaN failure rate, got %v", scores["a"])
	}
}

func TestReliability_DegradedUnknown(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) {
			c.HealthStatus = "degraded"
			c.Successes = 1
			c.Failures = 1
			c.EWMAFailureRate = 0
		}),
		makeCandidate("b", func(c *decision.Candidate) {
			c.HealthStatus = "unknown"
			c.Successes = 1
			c.Failures = 1
			c.EWMAFailureRate = 0
		}),
	}
	scores := computeReliability(cands)
	if scores["a"] >= scores["b"] {
		t.Fatalf("degraded should be less than unknown? degraded 0.2 vs unknown 0.5, but with failure 0: degraded (0.2+1)/2=0.6, unknown (0.5+1)/2=0.75, so unknown higher, got a=%v b=%v", scores["a"], scores["b"])
	}
}

func TestLatency_LowerBetter(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.EWMALatencyMS = 100; c.Successes = 1 }),
		makeCandidate("b", func(c *decision.Candidate) { c.EWMALatencyMS = 200; c.Successes = 1 }),
	}
	scores := computeLatency(cands, false)
	if scores["a"] <= scores["b"] {
		t.Fatalf("lower latency should score higher, got a=%v b=%v", scores["a"], scores["b"])
	}
	if scores["a"] != 1.0 || scores["b"] != 0.0 {
		t.Fatalf("expected 1 and 0, got %v %v", scores["a"], scores["b"])
	}
}

func TestLatency_UnmeasuredNeutral(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.EWMALatencyMS = 0 }),
		makeCandidate("b", func(c *decision.Candidate) { c.EWMALatencyMS = 0 }),
	}
	scores := computeLatency(cands, false)
	if scores["a"] != 0.5 || scores["b"] != 0.5 {
		t.Fatalf("expected neutral 0.5 for unmeasured, got %v %v", scores["a"], scores["b"])
	}
}

func TestLatency_EqualKnown(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.EWMALatencyMS = 100; c.Successes = 1 }),
		makeCandidate("b", func(c *decision.Candidate) { c.EWMALatencyMS = 100; c.Successes = 1 }),
	}
	scores := computeLatency(cands, false)
	if scores["a"] != 0.5 || scores["b"] != 0.5 {
		t.Fatalf("equal known should be neutral 0.5, got %v %v", scores["a"], scores["b"])
	}
}

func TestTTFT_SameCases(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.EWMATTFTMS = 50; c.Successes = 1 }),
		makeCandidate("b", func(c *decision.Candidate) { c.EWMATTFTMS = 100; c.Successes = 1 }),
	}
	scores := computeLatency(cands, true)
	if scores["a"] <= scores["b"] {
		t.Fatalf("lower TTFT should score higher")
	}
}

func TestCapacity_Pressure(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.CapacityPressure = 0 }),
		makeCandidate("b", func(c *decision.Candidate) { c.CapacityPressure = 2 }),
		makeCandidate("c", func(c *decision.Candidate) { c.CapacityPressure = 4 }),
	}
	scores := computeCapacity(cands)
	if scores["a"] != 1.0 {
		t.Fatalf("0 pressure should be 1.0, got %v", scores["a"])
	}
	if scores["b"] != 0.5 {
		t.Fatalf("2 pressure should be 0.5, got %v", scores["b"])
	}
	if scores["c"] != 0.0 {
		t.Fatalf("4 pressure should be 0.0, got %v", scores["c"])
	}
}

func TestCapacity_Invalid(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.CapacityPressure = math.NaN() }),
	}
	scores := computeCapacity(cands)
	if scores["a"] != 0.5 {
		t.Fatalf("NaN pressure should be neutral 0.5, got %v", scores["a"])
	}
}

func TestCost_PriceKnownFalseNeutral(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.PriceKnown = false; c.EstimatedCostUSD = 0 }),
		makeCandidate("b", func(c *decision.Candidate) { c.PriceKnown = true; c.EstimatedCostUSD = 0.001 }),
	}
	scores := computeCost(cands)
	if scores["a"] != 0.5 {
		t.Fatalf("PriceKnown false should be neutral 0.5, got %v", scores["a"])
	}
}

func TestCost_CheaperWins(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.PriceKnown = true; c.EstimatedCostUSD = 0.001 }),
		makeCandidate("b", func(c *decision.Candidate) { c.PriceKnown = true; c.EstimatedCostUSD = 0.002 }),
	}
	scores := computeCost(cands)
	if scores["a"] <= scores["b"] {
		t.Fatalf("cheaper should win, got a=%v b=%v", scores["a"], scores["b"])
	}
}

func TestCost_EqualKnown(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.PriceKnown = true; c.EstimatedCostUSD = 0.001 }),
		makeCandidate("b", func(c *decision.Candidate) { c.PriceKnown = true; c.EstimatedCostUSD = 0.001 }),
	}
	scores := computeCost(cands)
	if scores["a"] != 0.5 || scores["b"] != 0.5 {
		t.Fatalf("equal known cost should be 0.5, got %v %v", scores["a"], scores["b"])
	}
}

func TestCost_InvalidNeutral(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.PriceKnown = true; c.EstimatedCostUSD = math.NaN() }),
	}
	scores := computeCost(cands)
	if scores["a"] != 0.5 {
		t.Fatalf("invalid cost should be neutral 0.5, got %v", scores["a"])
	}
}

func TestContext_RequestRelativeHeadroom(t *testing.T) {
	required := 12000
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.ContextWindow = 16000 }),
		makeCandidate("b", func(c *decision.Candidate) { c.ContextWindow = 128000 }),
	}
	scores := computeContext(cands, required)
	// a: (16000-12000)/16000=0.25
	// b: (128000-12000)/128000=0.90625
	if scores["a"] >= scores["b"] {
		t.Fatalf("128k should have higher headroom than 16k for 12k req, got a=%v b=%v", scores["a"], scores["b"])
	}
	if math.Abs(scores["a"]-0.25) > 1e-6 {
		t.Fatalf("expected 0.25 for a, got %v", scores["a"])
	}
}

func TestContext_TinyRequest(t *testing.T) {
	required := 100
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.ContextWindow = 16000 }),
		makeCandidate("b", func(c *decision.Candidate) { c.ContextWindow = 128000 }),
	}
	scores := computeContext(cands, required)
	// a: (16000-100)/16000=0.99375
	// b: (128000-100)/128000=0.999218...
	// Both close to 1, but b slightly higher
	if scores["a"] <= 0.9 || scores["b"] <= 0.9 {
		t.Fatalf("tiny request should give high headroom close to 1, got a=%v b=%v", scores["a"], scores["b"])
	}
}

func TestContext_UnknownNeutral(t *testing.T) {
	required := 12000
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.ContextWindow = 0 }),
	}
	scores := computeContext(cands, required)
	if scores["a"] != 0.5 {
		t.Fatalf("unknown context window should be neutral 0.5, got %v", scores["a"])
	}
}

func TestContext_RequiredZeroNeutral(t *testing.T) {
	required := 0
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.ContextWindow = 16000 }),
		makeCandidate("b", func(c *decision.Candidate) { c.ContextWindow = 128000 }),
	}
	scores := computeContext(cands, required)
	if scores["a"] != 0.5 || scores["b"] != 0.5 {
		t.Fatalf("required <=0 should be neutral 0.5, got %v %v", scores["a"], scores["b"])
	}
}

func TestContext_TooSmallDefensive(t *testing.T) {
	required := 12000
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.ContextWindow = 8000 }),
		makeCandidate("b", func(c *decision.Candidate) { c.ContextWindow = 16000 }),
	}
	scores := computeContext(cands, required)
	if scores["a"] != 0.0 {
		t.Fatalf("too-small window should be 0.0 defensive, got %v", scores["a"])
	}
	if scores["b"] <= 0 {
		t.Fatalf("valid window should be >0, got %v", scores["b"])
	}
}

func TestContext_InvalidNegative(t *testing.T) {
	required := 12000
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.ContextWindow = -1 }),
	}
	scores := computeContext(cands, required)
	if scores["a"] != 0.5 {
		t.Fatalf("negative context should be neutral 0.5, got %v", scores["a"])
	}
}

func TestScoreCandidates_Finite(t *testing.T) {
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) {
			c.RouterScore = math.NaN()
			c.HealthStatus = "healthy"
			c.Successes = 0
			c.Failures = 0
			c.EWMALatencyMS = math.Inf(1)
			c.EWMATTFTMS = -1
			c.CapacityPressure = math.NaN()
			c.EstimatedCostUSD = math.Inf(1)
			c.PriceKnown = false
			c.ContextWindow = 0
			c.OriginalRank = 0
		}),
		makeCandidate("b", func(c *decision.Candidate) {
			c.RouterScore = 0.5
			c.HealthStatus = "unknown"
			c.Successes = 0
			c.Failures = 0
			c.EWMALatencyMS = 0
			c.EWMATTFTMS = 0
			c.CapacityPressure = 0
			c.EstimatedCostUSD = 0
			c.PriceKnown = false
			c.ContextWindow = 0
			c.OriginalRank = 1
		}),
	}
	scored := ScoreCandidates(cands, 0)
	for _, sc := range scored {
		for k, v := range sc.Components {
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
				t.Fatalf("component %s not finite [0,1]: %v", k, v)
			}
		}
		if math.IsNaN(sc.WeightedScore) || math.IsInf(sc.WeightedScore, 0) {
			t.Fatalf("weighted score not finite: %v", sc.WeightedScore)
		}
	}
}

func TestScoreCandidates_ContextHeadroomIntegration(t *testing.T) {
	// 16k vs 128k with 12k requirement
	cands := []decision.Candidate{
		makeCandidate("a", func(c *decision.Candidate) { c.ContextWindow = 16000; c.OriginalRank = 0 }),
		makeCandidate("b", func(c *decision.Candidate) { c.ContextWindow = 128000; c.OriginalRank = 1 }),
	}
	scored := ScoreCandidates(cands, 12000)
	if len(scored) != 2 {
		t.Fatalf("expected 2 scored")
	}
	// Find scores
	var scoreA, scoreB float64
	for _, sc := range scored {
		if sc.Candidate.ID == "a" {
			scoreA = sc.Components[CompContext]
		}
		if sc.Candidate.ID == "b" {
			scoreB = sc.Components[CompContext]
		}
	}
	if scoreB <= scoreA {
		t.Fatalf("128k should beat 16k for 12k req, got a=%v b=%v", scoreA, scoreB)
	}
}
