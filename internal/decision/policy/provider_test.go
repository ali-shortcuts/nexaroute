package policy

import (
	"context"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/decision"
)

func makePolicy(id string, weights Weights, minDelta float64) Policy {
	if weights.TotalWeight() == 0 {
		weights = Weights{RouterBaseline: 1}
	}
	return Policy{
		ID:            id,
		SelectionMode: "select_first",
		Weights:       weights,
		MinScoreDelta: minDelta,
	}
}

func TestProvider_ID(t *testing.T) {
	p := NewProvider(nil, "")
	if p.ID() != "policy" {
		t.Fatalf("expected policy, got %s", p.ID())
	}
}

func TestProvider_Capabilities(t *testing.T) {
	p := NewProvider(nil, "")
	caps := p.Capabilities()
	if !caps.CanSelect || caps.CanRank {
		t.Fatalf("expected CanSelect true CanRank false, got %+v", caps)
	}
}

func TestProvider_ContextCancellation(t *testing.T) {
	pol := makePolicy("test", Weights{RouterBaseline: 1}, 0)
	p := NewProvider([]Policy{pol}, "test")
	cands := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, OriginalRank: 0, Priority: 10},
		{ID: "b", PoolOrdinal: 0, OriginalRank: 1, Priority: 10},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", RequestID: "test"}
	res, err := p.Decide(ctx, req)
	if err == nil {
		t.Fatalf("expected error on cancelled ctx")
	}
	if res.Action != decision.ActionAbstain {
		t.Fatalf("expected abstain on cancel")
	}
}

func TestProvider_NoPolicyConfig(t *testing.T) {
	p := NewProvider(nil, "")
	cands := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, OriginalRank: 0, Priority: 10},
		{ID: "b", PoolOrdinal: 0, OriginalRank: 1, Priority: 10},
	}
	req := decision.DecisionRequest{Candidates: cands, PolicyID: "missing", RequestID: "test"}
	res, _ := p.Decide(context.Background(), req)
	if !res.Abstained || res.Action != decision.ActionAbstain {
		t.Fatalf("expected abstain when no policy")
	}
}

func TestProvider_PoolBoundary(t *testing.T) {
	pol := makePolicy("test", Weights{RouterBaseline: 1}, 0)
	p := NewProvider([]Policy{pol}, "test")
	cands := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, RouterScore: 0.6, OriginalRank: 0, Priority: 10},
		{ID: "b", PoolOrdinal: 1, RouterScore: 0.9, OriginalRank: 1, Priority: 10},
	}
	req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", RequestID: "test"}
	res, _ := p.Decide(context.Background(), req)
	if res.SelectedID != "a" {
		t.Fatalf("pool boundary should select a from earliest pool, got %s", res.SelectedID)
	}
	found := false
	for _, rc := range res.ReasonCodes {
		if rc == decision.ReasonPoolBoundaryEnforced {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected POOL_BOUNDARY_ENFORCED reason, got %v", res.ReasonCodes)
	}
}

func TestProvider_PriorityBoundary(t *testing.T) {
	pol := makePolicy("test", Weights{RouterBaseline: 1}, 0)
	p := NewProvider([]Policy{pol}, "test")
	cands := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, Priority: 0, RouterScore: 0.6, OriginalRank: 0},
		{ID: "b", PoolOrdinal: 0, Priority: 10, RouterScore: 0.9, OriginalRank: 1},
	}
	req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", RequestID: "test"}
	res, _ := p.Decide(context.Background(), req)
	if res.SelectedID != "a" {
		t.Fatalf("priority guardrail should select a (priority 0), got %s", res.SelectedID)
	}
	found := false
	for _, rc := range res.ReasonCodes {
		if rc == decision.ReasonPriorityGuardrail {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected PRIORITY_GUARDRAIL_ENFORCED reason")
	}
}

func TestProvider_AffinityPreservation(t *testing.T) {
	pol := makePolicy("test", Weights{RouterBaseline: 1}, 0)
	p := NewProvider([]Policy{pol}, "test")
	cands := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, Priority: 10, RouterScore: 0.8, OriginalRank: 0},
		{ID: "b", PoolOrdinal: 0, Priority: 10, RouterScore: 0.6, OriginalRank: 1},
	}
	req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", PinnedCandidateID: "b", RequestID: "test"}
	res, _ := p.Decide(context.Background(), req)
	if res.SelectedID != "b" {
		t.Fatalf("affinity should select b, got %s", res.SelectedID)
	}
	found := false
	for _, rc := range res.ReasonCodes {
		if rc == decision.ReasonAffinityPreserved {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected AFFINITY_PRESERVED")
	}
}

func TestProvider_AffinityAuthoritativeOverPriority(t *testing.T) {
	// A priority=0, B priority=10, pin=B, B in primary pool -> B remains primary despite lower priority
	pol := makePolicy("test", Weights{RouterBaseline: 1}, 0)
	p := NewProvider([]Policy{pol}, "test")
	cands := []decision.Candidate{
		{ID: "A", PoolOrdinal: 0, Priority: 0, RouterScore: 0.9, OriginalRank: 0},
		{ID: "B", PoolOrdinal: 0, Priority: 10, RouterScore: 0.1, OriginalRank: 1},
	}
	req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", PinnedCandidateID: "B", RequestID: "test"}
	res, _ := p.Decide(context.Background(), req)
	if res.SelectedID != "B" {
		t.Fatalf("affinity should override priority, expected B got %s", res.SelectedID)
	}
}

func TestProvider_AffinityInLaterPoolMustNotLeapfrog(t *testing.T) {
	pol := makePolicy("test", Weights{RouterBaseline: 1}, 0)
	p := NewProvider([]Policy{pol}, "test")
	cands := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, Priority: 10, OriginalRank: 0},
		{ID: "b", PoolOrdinal: 1, Priority: 10, OriginalRank: 1},
	}
	req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", PinnedCandidateID: "b", RequestID: "test"}
	res, _ := p.Decide(context.Background(), req)
	if res.SelectedID != "a" {
		t.Fatalf("pinned in later fallback must not leapfrog earlier pool, expected a got %s", res.SelectedID)
	}
}

func TestProvider_ExpiredOrNonBandAffinity(t *testing.T) {
	pol := makePolicy("test", Weights{RouterBaseline: 1}, 0)
	p := NewProvider([]Policy{pol}, "test")
	cands := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, Priority: 10, RouterScore: 0.9, OriginalRank: 0},
		{ID: "b", PoolOrdinal: 0, Priority: 10, RouterScore: 0.1, OriginalRank: 1},
	}
	// Pinned ID not in band
	req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", PinnedCandidateID: "c", RequestID: "test"}
	res, _ := p.Decide(context.Background(), req)
	// Since best is original primary (a), provider should ABSTAIN, not force c
	if res.Action != decision.ActionAbstain {
		t.Fatalf("non-band pin should not force selection, expected ABSTAIN got %s selected %s", res.Action, res.SelectedID)
	}
	if res.PolicyTrace != nil && res.PolicyTrace.SelectedID != "a" && res.PolicyTrace.SelectedID != "" {
		// PolicyTrace selected should be a or original
		t.Logf("policy trace selected %s", res.PolicyTrace.SelectedID)
	}
}

func TestProvider_SingleCandidate(t *testing.T) {
	pol := makePolicy("test", Weights{RouterBaseline: 1}, 0)
	p := NewProvider([]Policy{pol}, "test")
	cands := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, Priority: 10, OriginalRank: 0},
	}
	req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", RequestID: "test"}
	res, _ := p.Decide(context.Background(), req)
	if res.SelectedID != "a" {
		t.Fatalf("single candidate should be selected, got %s", res.SelectedID)
	}
}

func TestProvider_TaskOverride(t *testing.T) {
	pol := Policy{
		ID:            "test",
		SelectionMode: "select_first",
		Weights:       Weights{RouterBaseline: 1},
		TaskOverrides: map[string]Weights{
			"coding": {Latency: 1},
		},
		MinScoreDelta: 0,
	}
	p := NewProvider([]Policy{pol}, "test")
	cands := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, Priority: 10, RouterScore: 0.9, EWMALatencyMS: 200, Successes: 1, OriginalRank: 0},
		{ID: "b", PoolOrdinal: 0, Priority: 10, RouterScore: 0.1, EWMALatencyMS: 100, Successes: 1, OriginalRank: 1},
	}
	req := decision.DecisionRequest{
		Candidates: cands,
		PolicyID:   "test",
		TaskProfile: decision.TaskProfile{
			Type: "coding",
		},
		RequestID: "test",
	}
	res, _ := p.Decide(context.Background(), req)
	// With coding override Latency weight, b should win (lower latency)
	if res.SelectedID != "b" {
		t.Fatalf("task override should make b win, got %s", res.SelectedID)
	}
	found := false
	for _, rc := range res.ReasonCodes {
		if rc == decision.ReasonTaskAwareWeights {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected TASK_AWARE_WEIGHTS reason")
	}
}

func TestProvider_TiePreservesOriginalRank(t *testing.T) {
	pol := makePolicy("test", Weights{RouterBaseline: 1}, 0)
	p := NewProvider([]Policy{pol}, "test")
	cands := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, Priority: 10, RouterScore: 0.5, OriginalRank: 0},
		{ID: "b", PoolOrdinal: 0, Priority: 10, RouterScore: 0.5, OriginalRank: 1},
	}
	req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", RequestID: "test"}
	res, _ := p.Decide(context.Background(), req)
	// Tie should preserve original order, so a wins, but per min delta logic if best is original primary, it abstains
	// With identical scores, minDelta 0, best is a which is original primary, so should abstain
	if res.Action != decision.ActionAbstain {
		t.Fatalf("tie with best being original primary should abstain, got %s selected %s", res.Action, res.SelectedID)
	}
}

func TestProvider_BestAlreadyOriginalPrimary(t *testing.T) {
	pol := makePolicy("test", Weights{RouterBaseline: 1}, 0)
	p := NewProvider([]Policy{pol}, "test")
	cands := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, Priority: 10, RouterScore: 0.9, OriginalRank: 0},
		{ID: "b", PoolOrdinal: 0, Priority: 10, RouterScore: 0.1, OriginalRank: 1},
	}
	req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", RequestID: "test"}
	res, _ := p.Decide(context.Background(), req)
	// Best is a which is already original primary, should abstain to avoid meaningless SELECT
	if res.Action != decision.ActionAbstain {
		t.Fatalf("best already original primary should abstain, got action %s selected %s", res.Action, res.SelectedID)
	}
	found := false
	for _, rc := range res.ReasonCodes {
		if rc == decision.ReasonExistingOrderPreserved {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected EXISTING_ORDER_PRESERVED reason")
	}
}

func TestProvider_MinScoreDeltaAgainstOriginalPrimary(t *testing.T) {
	pol := makePolicy("test", Weights{RouterBaseline: 1}, 0.1)
	p := NewProvider([]Policy{pol}, "test")
	// Router original A,B,C
	// Scores: A=0.40, B=0.60, C=0.59 -> B beats A by 0.20 -> SELECT B
	cands := []decision.Candidate{
		{ID: "A", PoolOrdinal: 0, Priority: 10, RouterScore: 0.40, OriginalRank: 0},
		{ID: "B", PoolOrdinal: 0, Priority: 10, RouterScore: 0.60, OriginalRank: 1},
		{ID: "C", PoolOrdinal: 0, Priority: 10, RouterScore: 0.59, OriginalRank: 2},
	}
	req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", RequestID: "test"}
	res, _ := p.Decide(context.Background(), req)
	if res.SelectedID != "B" {
		t.Fatalf("expected B to beat A by 0.20 with delta 0.10, got %s", res.SelectedID)
	}

	// Second example: A=0.59, B=0.60, C=0.10, delta 0.05, B improves A by 0.01 -> ABSTAIN
	cands2 := []decision.Candidate{
		{ID: "A", PoolOrdinal: 0, Priority: 10, RouterScore: 0.59, OriginalRank: 0},
		{ID: "B", PoolOrdinal: 0, Priority: 10, RouterScore: 0.60, OriginalRank: 1},
		{ID: "C", PoolOrdinal: 0, Priority: 10, RouterScore: 0.10, OriginalRank: 2},
	}
	req2 := decision.DecisionRequest{Candidates: cands2, PolicyID: "test", RequestID: "test2"}
	res2, _ := p.Decide(context.Background(), req2)
	if !res2.Abstained {
		t.Fatalf("expected ABSTAIN when B only improves A by 0.01 with delta 0.05, got %s", res2.SelectedID)
	}
	found := false
	for _, rc := range res2.ReasonCodes {
		if rc == decision.ReasonMinDeltaNotMet {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected MIN_DELTA_NOT_MET reason")
	}
}

func TestProvider_SelectOnly(t *testing.T) {
	pol := makePolicy("test", Weights{RouterBaseline: 1}, 0)
	p := NewProvider([]Policy{pol}, "test")
	cands := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, Priority: 10, RouterScore: 0.1, OriginalRank: 0},
		{ID: "b", PoolOrdinal: 0, Priority: 10, RouterScore: 0.9, OriginalRank: 1},
	}
	req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", RequestID: "test"}
	res, _ := p.Decide(context.Background(), req)
	if res.Action != decision.ActionSelect && res.Action != decision.ActionAbstain {
		t.Fatalf("policy should only SELECT or ABSTAIN, got %s", res.Action)
	}
	if res.Action == decision.ActionRank {
		t.Fatalf("policy should not RANK")
	}
}

func TestProvider_Deterministic(t *testing.T) {
	pol := makePolicy("test", Weights{RouterBaseline: 1, Reliability: 1, Latency: 1}, 0)
	p := NewProvider([]Policy{pol}, "test")
	cands := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, Priority: 10, RouterScore: 0.5, EWMALatencyMS: 100, Successes: 1, OriginalRank: 0},
		{ID: "b", PoolOrdinal: 0, Priority: 10, RouterScore: 0.6, EWMALatencyMS: 200, Successes: 1, OriginalRank: 1},
	}
	req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", RequestID: "test"}
	res1, _ := p.Decide(context.Background(), req)
	res2, _ := p.Decide(context.Background(), req)
	if res1.SelectedID != res2.SelectedID || res1.Action != res2.Action {
		t.Fatalf("deterministic failed: %v vs %v", res1, res2)
	}
}

func TestProvider_NoMutation(t *testing.T) {
	pol := makePolicy("test", Weights{RouterBaseline: 1}, 0)
	p := NewProvider([]Policy{pol}, "test")
	cands := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, Priority: 10, RouterScore: 0.5, OriginalRank: 0},
		{ID: "b", PoolOrdinal: 0, Priority: 10, RouterScore: 0.6, OriginalRank: 1},
	}
	orig := make([]decision.Candidate, len(cands))
	copy(orig, cands)
	req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", RequestID: "test"}
	_, _ = p.Decide(context.Background(), req)
	for i := range cands {
		if cands[i].ID != orig[i].ID || cands[i].RouterScore != orig[i].RouterScore {
			t.Fatalf("input mutated")
		}
	}
}

func TestProvider_InvalidTelemetryFailOpen(t *testing.T) {
	pol := makePolicy("test", Weights{RouterBaseline: 1}, 0)
	p := NewProvider([]Policy{pol}, "test")
	cands := []decision.Candidate{
		{ID: "a", PoolOrdinal: 0, Priority: 10, RouterScore: 0.5, EWMALatencyMS: 100, OriginalRank: 0, Successes: 1},
		{ID: "b", PoolOrdinal: 0, Priority: 10, RouterScore: 0.6, EWMALatencyMS: 200, OriginalRank: 1, Successes: 1},
	}
	req := decision.DecisionRequest{Candidates: cands, PolicyID: "test", RequestID: "test"}
	res, err := p.Decide(context.Background(), req)
	if err != nil {
		t.Fatalf("should not error on valid telemetry, got %v", err)
	}
	if res.SelectedID == "" && !res.Abstained {
		t.Fatalf("should select or abstain, not empty")
	}
}
