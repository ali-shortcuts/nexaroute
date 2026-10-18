package usage

import "testing"

// Cost provenance (#435): a snapshot must say what its cost figures
// ARE. The defect class pinned here: tokens recorded without
// record-time costing and without a current price were silently
// reported as an exact zero cost ("unknown" presented as free).

func rowFor(t *testing.T, s Snapshot, deployment string) Totals {
	t.Helper()
	for _, r := range s.ByDeployment {
		if r.Deployment == deployment {
			return r
		}
	}
	t.Fatalf("no row for %s in %+v", deployment, s.ByDeployment)
	return Totals{}
}

func TestCostBasisRecorded(t *testing.T) {
	tr := New()
	tr.RecordEx("dep-a", Counts{Input: 1_000_000, Output: 0}, 2.50, "v1")
	s := tr.Snapshot(nil)
	row := rowFor(t, s, "dep-a")
	if row.CostBasis != "recorded" || row.CostCompleteness != "complete" {
		t.Fatalf("basis=%q completeness=%q, want recorded/complete", row.CostBasis, row.CostCompleteness)
	}
	if row.UnpricedTokens != 0 || row.EstimatedCost != 2.50 {
		t.Fatalf("unpriced=%d cost=%v, want 0 / 2.50", row.UnpricedTokens, row.EstimatedCost)
	}
	if s.TotalCostBasis != "recorded" {
		t.Fatalf("total basis=%q, want recorded", s.TotalCostBasis)
	}
}

func TestCostBasisEstimatedAndMixed(t *testing.T) {
	prices := map[string]Price{"dep-a": {InputPerMTok: 3, OutputPerMTok: 0}}

	tr := New()
	tr.Record("dep-a", 1_000_000, 0) // legacy: no record-time cost
	s := tr.Snapshot(prices)
	row := rowFor(t, s, "dep-a")
	if row.CostBasis != "estimated" || row.CostCompleteness != "complete" {
		t.Fatalf("basis=%q completeness=%q, want estimated/complete", row.CostBasis, row.CostCompleteness)
	}
	if row.EstimatedCost != 3.0 || row.UnpricedTokens != 1_000_000 {
		t.Fatalf("cost=%v unpriced=%d, want 3 / 1000000", row.EstimatedCost, row.UnpricedTokens)
	}

	tr2 := New()
	tr2.Record("dep-a", 1_000_000, 0)
	tr2.RecordEx("dep-a", Counts{Input: 500_000}, 1.25, "v1")
	s2 := tr2.Snapshot(prices)
	row2 := rowFor(t, s2, "dep-a")
	if row2.CostBasis != "mixed" || row2.CostCompleteness != "complete" {
		t.Fatalf("basis=%q completeness=%q, want mixed/complete", row2.CostBasis, row2.CostCompleteness)
	}
	if row2.EstimatedCost != 1.25+3.0 {
		t.Fatalf("cost=%v, want recorded 1.25 + fallback 3.0", row2.EstimatedCost)
	}
}

func TestCostBasisUnknownIsNotZeroCost(t *testing.T) {
	tr := New()
	tr.Record("dep-a", 2_000_000, 1_000_000) // legacy tokens, no price anywhere
	s := tr.Snapshot(nil)
	row := rowFor(t, s, "dep-a")
	if row.CostBasis != "unknown" {
		t.Fatalf("basis=%q, want unknown (unvalued tokens must not read as exact zero cost)", row.CostBasis)
	}
	if row.CostCompleteness != "partial" {
		t.Fatalf("completeness=%q, want partial", row.CostCompleteness)
	}
	if row.UnpricedTokens != 3_000_000 {
		t.Fatalf("unpriced=%d, want 3000000", row.UnpricedTokens)
	}
	if s.TotalCostBasis != "unknown" || s.TotalUnpricedTokens != 3_000_000 {
		t.Fatalf("totals basis=%q unpriced=%d, want unknown / 3000000", s.TotalCostBasis, s.TotalUnpricedTokens)
	}
}

func TestCostBasisMixedPartialWithoutPrice(t *testing.T) {
	tr := New()
	tr.Record("dep-a", 1_000_000, 0)
	tr.RecordEx("dep-a", Counts{Input: 10}, 0.50, "v1")
	s := tr.Snapshot(nil) // no price for the legacy tokens
	row := rowFor(t, s, "dep-a")
	if row.CostBasis != "mixed" || row.CostCompleteness != "partial" {
		t.Fatalf("basis=%q completeness=%q, want mixed/partial", row.CostBasis, row.CostCompleteness)
	}
	if row.EstimatedCost != 0.50 {
		t.Fatalf("cost=%v, want only the recorded 0.50 (unknown part excluded, flagged partial)", row.EstimatedCost)
	}
}

func TestCostBasisAggregateIsWeakestRow(t *testing.T) {
	tr := New()
	tr.RecordEx("dep-a", Counts{Input: 100}, 1.0, "v1") // recorded
	tr.Record("dep-b", 100, 0)                          // estimated with price below
	s := tr.Snapshot(map[string]Price{"dep-b": {InputPerMTok: 1}})
	if s.TotalCostBasis != "estimated" {
		t.Fatalf("total basis=%q, want estimated (weakest row wins)", s.TotalCostBasis)
	}
	empty := New().Snapshot(nil)
	if empty.TotalCostBasis != "" {
		t.Fatalf("empty snapshot basis=%q, want empty (no claim)", empty.TotalCostBasis)
	}
}
