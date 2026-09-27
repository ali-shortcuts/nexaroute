package decision

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/config"
)

// Property-based test: for any eligible set and any provider result (valid or invalid),
// the orchestrator must never return a candidate outside eligible, must preserve all eligible elements,
// and must never panic.

func TestProperty_EligibleSetPreserved(t *testing.T) {
	rnd := rand.New(rand.NewSource(42))
	reg := NewRegistry()
	randomProvider := &randomizingProvider{rnd: rnd}
	reg.Register(randomProvider)

	orch := NewOrchestrator(reg, config.DecisionConfig{Mode: "local", Provider: "random", TimeoutMS: 20}, &Metrics{})

	for iter := 0; iter < 200; iter++ {
		// Random eligible size 2-10 (avoid single optimization which skips provider)
		n := rnd.Intn(9) + 2
		eligible := make([]Candidate, n)
		for i := 0; i < n; i++ {
			eligible[i] = Candidate{ID: string(rune('A' + i)), ProviderID: "p"}
		}
		req := DecisionRequest{Candidates: eligible, Budget: Budget{Timeout: 20 * time.Millisecond, MaxProviderCalls: 1}}

		ordered, _, _ := orch.Decide(context.Background(), req)

		if len(ordered) != len(eligible) {
			t.Fatalf("iter %d: length changed: got %d expected %d", iter, len(ordered), len(eligible))
		}
		eligibleSet := make(map[string]struct{}, len(eligible))
		for _, c := range eligible {
			eligibleSet[c.ID] = struct{}{}
		}
		orderedSet := make(map[string]struct{}, len(ordered))
		for _, c := range ordered {
			if _, ok := eligibleSet[c.ID]; !ok {
				t.Fatalf("iter %d: unknown candidate %s injected", iter, c.ID)
			}
			orderedSet[c.ID] = struct{}{}
		}
		if len(orderedSet) != len(eligibleSet) {
			t.Fatalf("iter %d: set size mismatch", iter)
		}
		for id := range eligibleSet {
			if _, ok := orderedSet[id]; !ok {
				t.Fatalf("iter %d: eligible %s missing", iter, id)
			}
		}
	}
}

type randomizingProvider struct {
	rnd *rand.Rand
}

func (r *randomizingProvider) ID() string { return "random" }
func (r *randomizingProvider) Capabilities() Capabilities {
	return Capabilities{CanRank: true, CanSelect: true}
}
func (r *randomizingProvider) Health() ProviderHealth {
	return ProviderHealth{Status: HealthHealthy, CheckedAt: time.Now()}
}
func (r *randomizingProvider) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	if r.rnd.Float64() < 0.2 {
		return DecisionResult{
			Action:     ActionRank,
			RankedIDs:  []string{"EVIL", "UNKNOWN"},
			Confidence: 0.9,
		}, nil
	}
	if r.rnd.Float64() < 0.1 {
		panic("random panic")
	}
	n := len(req.Candidates)
	if n == 0 {
		return DecisionResult{Action: ActionAbstain, Abstained: true, Confidence: 0}, nil
	}
	ids := make([]string, n)
	for i, c := range req.Candidates {
		ids[i] = c.ID
	}
	r.rnd.Shuffle(n, func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	cut := r.rnd.Intn(n) + 1
	return DecisionResult{
		Action:     ActionRank,
		RankedIDs:  ids[:cut],
		Confidence: r.rnd.Float64(),
	}, nil
}
