package usage

import (
	"fmt"
	"math"
	"testing"
	"time"
)

func TestContinue95BudgetLedgerGuards(t *testing.T) {
	now := time.Now()
	l := NewBudgetLedger()
	if id, ok := l.Reserve("k", 10, 0, now); id != 0 || !ok {
		t.Errorf("Reserve zero amount = (%d, %v), want (0, true)", id, ok)
	}
	if id, ok := l.Reserve("k", 0, 5, now); id != 0 || !ok {
		t.Errorf("Reserve zero limit = (%d, %v), want (0, true)", id, ok)
	}
	// No-op settles/releases must not panic or create rows.
	l.Settle("k", 0, 1.0)
	l.Settle("missing", 5, 1.0)
	l.Release("k", 0)
	l.Release("missing", 5)
	if rows, _, _ := l.Stats(); rows != 0 {
		t.Errorf("Stats rows after no-ops = %d, want 0", rows)
	}
	// Unknown-id release on a live row is a no-op; the hold survives.
	id, ok := l.Reserve("k", 100, 7, now)
	if !ok || id == 0 {
		t.Fatalf("Reserve = (%d, %v), want live id", id, ok)
	}
	l.Release("k", id+99)
	if _, reserved := l.Snapshot("k", now); reserved != 7 {
		t.Errorf("Snapshot reserved after unknown-id release = %v, want 7", reserved)
	}
	// Stats over live rows.
	if _, ok := l.Reserve("j", 100, 3, now); !ok {
		t.Fatal("second Reserve should succeed")
	}
	rows, reservations, reserved := l.Stats()
	if rows != 2 || reservations != 2 || reserved != 10 {
		t.Errorf("Stats = (%d, %d, %v), want (2, 2, 10)", rows, reservations, reserved)
	}
}

func Continue95FullTracker(t *testing.T) *Tracker {
	t.Helper()
	tr := New()
	for i := 0; i < maxDeployments; i++ {
		tr.byDeploy[fmt.Sprintf("d%d", i)] = &Totals{Deployment: fmt.Sprintf("d%d", i)}
	}
	return tr
}

func TestContinue95TrackerDeploymentCap(t *testing.T) {
	tr := Continue95FullTracker(t)
	tr.Record("new", 5, 5)
	if _, ok := tr.byDeploy["new"]; ok {
		t.Error("Record past maxDeployments should be dropped")
	}
	tr2 := Continue95FullTracker(t)
	tr2.RecordEx("new", Counts{Input: 1}, 1.0, "v1")
	if _, ok := tr2.byDeploy["new"]; ok {
		t.Error("RecordEx past maxDeployments should be dropped")
	}
}

func TestContinue95RecordExBranches(t *testing.T) {
	tr := New()
	// All-zero report: no deployment row is created.
	tr.RecordEx("d", Counts{}, 0, "v1")
	if len(tr.byDeploy) != 0 {
		t.Errorf("all-zero RecordEx created %d rows, want 0", len(tr.byDeploy))
	}
	// Negative and NaN costs clamp to zero (no recorded cost).
	tr.RecordEx("neg", Counts{Input: 10}, -5, "v1")
	tr.RecordEx("nan", Counts{Input: 10}, math.NaN(), "v1")
	snap := tr.Snapshot(nil)
	for _, row := range snap.ByDeployment {
		if row.Deployment == "neg" || row.Deployment == "nan" {
			if row.RecordedCostUSD != 0 || len(row.CostByVersion) != 0 {
				t.Errorf("clamped-cost row %s = cost %v versions %v, want 0/empty", row.Deployment, row.RecordedCostUSD, row.CostByVersion)
			}
			if row.PromptTokens != 10 {
				t.Errorf("clamped-cost row %s tokens = %d, want 10", row.Deployment, row.PromptTokens)
			}
		}
	}
	// Empty deployment and empty version default.
	tr.RecordEx("", Counts{Input: 5}, 2.5, "")
	snap = tr.Snapshot(nil)
	found := false
	for _, row := range snap.ByDeployment {
		if row.Deployment == "unknown" && row.CostByVersion["unpriced"] == 2.5 {
			found = true
		}
	}
	if !found {
		t.Errorf("empty deployment/version should record under unknown/unpriced; snapshot = %+v", snap.ByDeployment)
	}
}

func TestContinue95SnapshotEstimatedCostClamp(t *testing.T) {
	tr := New()
	tr.Record("d", 2000000, 0)
	snap := tr.Snapshot(map[string]Price{"d": {InputPerMTok: 1.7e308}})
	if len(snap.ByDeployment) != 1 {
		t.Fatalf("snapshot rows = %d, want 1", len(snap.ByDeployment))
	}
	row := snap.ByDeployment[0]
	if row.EstimatedCost != 0 {
		t.Errorf("EstimatedCost with overflowing fallback = %v, want 0 (clamped)", row.EstimatedCost)
	}
	if row.CostBasis != "estimated" {
		t.Errorf("CostBasis = %q, want estimated", row.CostBasis)
	}
}
