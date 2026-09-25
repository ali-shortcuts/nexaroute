package decision

import (
	"reflect"
	"testing"
)

func cand(id string, pool, priority int) Candidate {
	return Candidate{ID: id, PoolOrdinal: pool, Priority: priority}
}

func TestComputeConstraintsEarliestPoolMandatory(t *testing.T) {
	// Primary pool A,B (pool 0); fallback pool C (pool 1).
	in := []Candidate{cand("A", 0, 0), cand("B", 0, 0), cand("C", 1, 0)}
	got := ComputeConstraints(in, "")
	if !reflect.DeepEqual(got.AllowedPrimaryIDs, []string{"A", "B"}) {
		t.Fatalf("allowed=%v, want [A B]", got.AllowedPrimaryIDs)
	}
	if got.ForcedPrimaryID != "" {
		t.Fatalf("forced=%q, want empty", got.ForcedPrimaryID)
	}
	// Full candidate list untouched.
	if len(in) != 3 {
		t.Fatalf("input mutated: %v", in)
	}
}

func TestComputeConstraintsMinimumPriorityTier(t *testing.T) {
	in := []Candidate{cand("A", 0, 0), cand("B", 0, 10)}
	got := ComputeConstraints(in, "")
	if !reflect.DeepEqual(got.AllowedPrimaryIDs, []string{"A"}) {
		t.Fatalf("allowed=%v, want [A]", got.AllowedPrimaryIDs)
	}
}

func TestComputeConstraintsPoolBeatsPriority(t *testing.T) {
	// B has a better priority number but sits in a later pool: A wins.
	in := []Candidate{cand("A", 0, 5), cand("B", 1, 0)}
	got := ComputeConstraints(in, "")
	if !reflect.DeepEqual(got.AllowedPrimaryIDs, []string{"A"}) {
		t.Fatalf("allowed=%v, want [A]", got.AllowedPrimaryIDs)
	}
}

func TestComputeConstraintsAffinityPinAuthoritative(t *testing.T) {
	in := []Candidate{cand("A", 0, 0), cand("B", 0, 10)}
	got := ComputeConstraints(in, "B")
	if got.ForcedPrimaryID != "B" {
		t.Fatalf("forced=%q, want B", got.ForcedPrimaryID)
	}
	if !reflect.DeepEqual(got.AllowedPrimaryIDs, []string{"B"}) {
		t.Fatalf("allowed=%v, want [B]", got.AllowedPrimaryIDs)
	}
	if !reflect.DeepEqual(got.ReasonCodes, []ReasonCode{ReasonAffinityPreserved}) {
		t.Fatalf("reasons=%v", got.ReasonCodes)
	}
}

func TestComputeConstraintsAffinityPinOutsideEarliestPoolStillForced(t *testing.T) {
	// Conservative extension: any eligible pin is authoritative, even outside
	// the earliest pool. Skipping remote intelligence is always safe.
	in := []Candidate{cand("A", 0, 0), cand("C", 1, 0)}
	got := ComputeConstraints(in, "C")
	if got.ForcedPrimaryID != "C" {
		t.Fatalf("forced=%q, want C", got.ForcedPrimaryID)
	}
}

func TestComputeConstraintsStalePinIgnored(t *testing.T) {
	in := []Candidate{cand("A", 0, 0), cand("B", 0, 0)}
	got := ComputeConstraints(in, "GONE")
	if got.ForcedPrimaryID != "" {
		t.Fatalf("forced=%q, want empty", got.ForcedPrimaryID)
	}
	if !reflect.DeepEqual(got.AllowedPrimaryIDs, []string{"A", "B"}) {
		t.Fatalf("allowed=%v, want [A B]", got.AllowedPrimaryIDs)
	}
}

func TestComputeConstraintsEmpty(t *testing.T) {
	got := ComputeConstraints(nil, "")
	if len(got.AllowedPrimaryIDs) != 0 || got.ForcedPrimaryID != "" {
		t.Fatalf("empty input must yield empty constraints: %+v", got)
	}
}

func TestComputeConstraintsPreservesOrder(t *testing.T) {
	in := []Candidate{cand("B", 0, 0), cand("A", 0, 0), cand("C", 0, 0)}
	got := ComputeConstraints(in, "")
	if !reflect.DeepEqual(got.AllowedPrimaryIDs, []string{"B", "A", "C"}) {
		t.Fatalf("allowed=%v, want input order", got.AllowedPrimaryIDs)
	}
}
