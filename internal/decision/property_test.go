package decision

import (
	"context"
	"math/rand"
	"testing"
	"time"
)

// adversarialProvider returns whatever selection the test dictates,
// including out-of-band and out-of-set IDs.
type adversarialProvider struct {
	id     string
	typ    string
	pick   func(req DecisionRequest) string
	conf   float64
	action Action
}

func (m *adversarialProvider) ID() string   { return m.id }
func (m *adversarialProvider) Type() string { return m.typ }
func (m *adversarialProvider) Capabilities() Capabilities {
	return Capabilities{CanSelect: true}
}
func (m *adversarialProvider) Health() Health { return Health{Available: true} }
func (m *adversarialProvider) Decide(ctx context.Context, req DecisionRequest) (DecisionResult, error) {
	if m.action == ActionAbstain {
		return DecisionResult{Action: ActionAbstain, ReasonCodes: []ReasonCode{ReasonExternalAbstained}}, nil
	}
	return DecisionResult{
		Action: ActionSelect, SelectedID: m.pick(req), Confidence: m.conf,
		ReasonCodes: []ReasonCode{ReasonExternalSelected},
	}, nil
}

// TestPropertySelectedAlwaysInPrimaryBand fuzzes the orchestrator with an
// adversarial provider: the promoted primary must ALWAYS belong to
// AllowedPrimaryIDs — never merely to the eligible set E.
func TestPropertySelectedAlwaysInPrimaryBand(t *testing.T) {
	rng := rand.New(rand.NewSource(0xF1))
	ids := []string{"A", "B", "C", "D", "E"}
	for iter := 0; iter < 2000; iter++ {
		n := 1 + rng.Intn(5)
		cands := make([]Candidate, 0, n)
		for i := 0; i < n; i++ {
			cands = append(cands, Candidate{
				ID:          ids[i],
				PoolOrdinal: rng.Intn(3),
				Priority:    []int{0, 5, 10}[rng.Intn(3)],
			})
		}
		var pin string
		if rng.Intn(4) == 0 {
			pin = ids[rng.Intn(n)] // eligible pin
		} else if rng.Intn(4) == 0 {
			pin = "STALE" // ineligible pin
		}
		pickStrategy := rng.Intn(4)
		mp := &adversarialProvider{id: "adv", typ: "jev", conf: 0.5, action: ActionSelect,
			pick: func(req DecisionRequest) string {
				switch pickStrategy {
				case 0: // random eligible (may be out of band)
					return req.Candidates[rng.Intn(len(req.Candidates))].ID
				case 1: // last eligible (often fallback pool)
					return req.Candidates[len(req.Candidates)-1].ID
				case 2: // ghost
					return "GHOST"
				default: // first allowed (valid)
					if len(req.AllowedPrimaryIDs) > 0 {
						return req.AllowedPrimaryIDs[0]
					}
					return "GHOST"
				}
			}}
		if rng.Intn(5) == 0 {
			mp.action = ActionAbstain
		}
		o := NewOrchestrator(ModeAssisted, mp, time.Second)
		out := o.Decide(context.Background(), cands, pin, RequestFeatures{}, "prop")
		// Invariants on every outcome:
		if len(out.FinalOrder) != len(cands) {
			t.Fatalf("iter %d: order length changed", iter)
		}
		seen := map[string]bool{}
		for _, id := range out.FinalOrder {
			seen[id] = true
		}
		for _, c := range cands {
			if !seen[c.ID] {
				t.Fatalf("iter %d: candidate %q lost", iter, c.ID)
			}
		}
		if out.SelectedID == "" {
			continue
		}
		allowed := ComputeConstraints(cands, pin).AllowedPrimaryIDs
		ok := false
		for _, id := range allowed {
			if id == out.SelectedID {
				ok = true
			}
		}
		if !ok {
			t.Fatalf("iter %d: selected %q outside band %v (E=%v pin=%q)",
				iter, out.SelectedID, allowed, cands, pin)
		}
	}
}
