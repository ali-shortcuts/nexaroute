package decision

import "testing"

func TestPrimaryConstraints_PoolBoundary(t *testing.T) {
	cands := []Candidate{
		{ID: "p1/m1", PoolOrdinal: 0, Priority: 0, OriginalRank: 0},
		{ID: "p2/m2", PoolOrdinal: 0, Priority: 0, OriginalRank: 1},
		{ID: "p3/m3", PoolOrdinal: 1, Priority: 0, OriginalRank: 2},
	}
	pc := ComputePrimaryConstraints(cands, "")
	if len(pc.AllowedPrimaryIDs) != 2 {
		t.Fatalf("expected 2 allowed, got %v", pc.AllowedPrimaryIDs)
	}
	if !pc.IsAllowedPrimary("p1/m1") || !pc.IsAllowedPrimary("p2/m2") {
		t.Fatalf("allowed should be primary pool")
	}
	if pc.IsAllowedPrimary("p3/m3") {
		t.Fatalf("fallback should not be allowed")
	}
	found := false
	for _, rc := range pc.ReasonCodes {
		if rc == ReasonPoolBoundaryEnforced {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected POOL_BOUNDARY_ENFORCED")
	}
}

func TestPrimaryConstraints_PriorityBoundary(t *testing.T) {
	cands := []Candidate{
		{ID: "p1/m1", PoolOrdinal: 0, Priority: 0, OriginalRank: 0},
		{ID: "p2/m2", PoolOrdinal: 0, Priority: 10, OriginalRank: 1},
		{ID: "p3/m3", PoolOrdinal: 0, Priority: 10, OriginalRank: 2},
	}
	pc := ComputePrimaryConstraints(cands, "")
	if len(pc.AllowedPrimaryIDs) != 1 || pc.AllowedPrimaryIDs[0] != "p1/m1" {
		t.Fatalf("expected only p1/m1 allowed, got %v", pc.AllowedPrimaryIDs)
	}
	found := false
	for _, rc := range pc.ReasonCodes {
		if rc == ReasonPriorityGuardrail {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected PRIORITY_GUARDRAIL_ENFORCED")
	}
}

func TestPrimaryConstraints_AffinityPreserved(t *testing.T) {
	cands := []Candidate{
		{ID: "p1/m1", PoolOrdinal: 0, Priority: 0, OriginalRank: 0},
		{ID: "p2/m2", PoolOrdinal: 0, Priority: 10, OriginalRank: 1},
	}
	pc := ComputePrimaryConstraints(cands, "p2/m2")
	if pc.ForcedPrimaryID != "p2/m2" {
		t.Fatalf("expected forced p2/m2, got %s", pc.ForcedPrimaryID)
	}
	if len(pc.AllowedPrimaryIDs) != 1 || pc.AllowedPrimaryIDs[0] != "p2/m2" {
		t.Fatalf("allowed should be only forced, got %v", pc.AllowedPrimaryIDs)
	}
	found := false
	for _, rc := range pc.ReasonCodes {
		if rc == ReasonAffinityPreserved {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected AFFINITY_PRESERVED")
	}
}

func TestPrimaryConstraints_AffinityInLaterPoolMustNotLeapfrog(t *testing.T) {
	cands := []Candidate{
		{ID: "p1/m1", PoolOrdinal: 0, Priority: 0, OriginalRank: 0},
		{ID: "p3/m3", PoolOrdinal: 1, Priority: 0, OriginalRank: 1},
	}
	pc := ComputePrimaryConstraints(cands, "p3/m3")
	if pc.ForcedPrimaryID != "" {
		t.Fatalf("affinity in later pool should not be forced, got %s", pc.ForcedPrimaryID)
	}
	if !pc.IsAllowedPrimary("p1/m1") {
		t.Fatalf("primary should be allowed")
	}
	if pc.IsAllowedPrimary("p3/m3") {
		t.Fatalf("fallback should not be allowed even with affinity")
	}
}

func TestPrimaryConstraints_Deterministic(t *testing.T) {
	cands := []Candidate{
		{ID: "p1/m1", PoolOrdinal: 0, Priority: 0, OriginalRank: 0},
		{ID: "p2/m2", PoolOrdinal: 0, Priority: 0, OriginalRank: 1},
	}
	pc1 := ComputePrimaryConstraints(cands, "")
	pc2 := ComputePrimaryConstraints(cands, "")
	if len(pc1.AllowedPrimaryIDs) != len(pc2.AllowedPrimaryIDs) {
		t.Fatalf("deterministic failed")
	}
	for i := range pc1.AllowedPrimaryIDs {
		if pc1.AllowedPrimaryIDs[i] != pc2.AllowedPrimaryIDs[i] {
			t.Fatalf("deterministic mismatch")
		}
	}
}
