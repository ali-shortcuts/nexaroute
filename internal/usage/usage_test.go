package usage

import "testing"

func TestTrackerIgnoresEmptyRecords(t *testing.T) {
	tr := New()
	tr.Record("d1", 0, 0)
	s := tr.Snapshot(nil)
	if s.TotalRequests != 0 || s.TotalPromptTokens != 0 || len(s.ByDeployment) != 0 {
		t.Fatalf("zero-token record must be ignored: %+v", s)
	}
}

func TestTrackerAccumulatesPerDeployment(t *testing.T) {
	tr := New()
	tr.Record("d1", 100, 10)
	tr.Record("d1", 50, 5)
	tr.Record("d2", 1000, 100)
	s := tr.Snapshot(nil)
	if s.TotalPromptTokens != 1150 || s.TotalCompletionTokens != 115 || s.TotalRequests != 3 {
		t.Fatalf("unexpected totals: %+v", s)
	}
	if len(s.ByDeployment) != 2 {
		t.Fatalf("expected 2 deployments, got %d", len(s.ByDeployment))
	}
	if s.ByDeployment[0].Deployment != "d1" || s.ByDeployment[1].Deployment != "d2" {
		t.Fatalf("snapshot must be sorted by deployment id: %+v", s.ByDeployment)
	}
	if s.ByDeployment[0].PromptTokens != 150 || s.ByDeployment[0].CompletionTok != 15 {
		t.Fatalf("unexpected d1 totals: %+v", s.ByDeployment[0])
	}
}

func TestTrackerCostEstimation(t *testing.T) {
	tr := New()
	tr.Record("d1", 1_000_000, 2_000_000)
	tr.Record("d2", 1_000_000, 0)
	prices := map[string]Price{
		"d1": {InputPerMTok: 3, OutputPerMTok: 15},
		"d2": {InputPerMTok: 0.5, OutputPerMTok: 2},
	}
	s := tr.Snapshot(prices)
	if s.TotalEstimatedCostUSD < 33.499 || s.TotalEstimatedCostUSD > 33.501 {
		t.Fatalf("expected ~33.5 USD, got %f", s.TotalEstimatedCostUSD)
	}
	// Deployment without pricing contributes zero cost.
	noPrice := tr.Snapshot(map[string]Price{})
	if noPrice.TotalEstimatedCostUSD != 0 {
		t.Fatalf("missing price table must yield zero cost, got %f", noPrice.TotalEstimatedCostUSD)
	}
}

func TestTrackerRetainDropsRemovedDeployments(t *testing.T) {
	tr := New()
	tr.Record("keep", 10, 10)
	tr.Record("gone", 10, 10)
	tr.Retain(map[string]struct{}{"keep": {}})
	s := tr.Snapshot(nil)
	if len(s.ByDeployment) != 1 || s.ByDeployment[0].Deployment != "keep" {
		t.Fatalf("retain must drop removed deployments: %+v", s.ByDeployment)
	}
	// Global counters are cumulative history for surviving deployments only
	// in the sense that per-deployment rows are trimmed; totals follow rows.
	if s.TotalPromptTokens != 10 {
		t.Fatalf("expected retained totals to match rows, got %d", s.TotalPromptTokens)
	}
}

func TestTrackerReset(t *testing.T) {
	tr := New()
	tr.Record("d", 10, 10)
	tr.Reset()
	s := tr.Snapshot(nil)
	if s.TotalRequests != 0 || len(s.ByDeployment) != 0 {
		t.Fatalf("reset must clear everything: %+v", s)
	}
}

func TestTrackerUnknownDeploymentBucketed(t *testing.T) {
	tr := New()
	tr.Record("", 5, 5)
	s := tr.Snapshot(nil)
	if len(s.ByDeployment) != 1 || s.ByDeployment[0].Deployment != "unknown" {
		t.Fatalf("empty deployment id must map to unknown bucket: %+v", s.ByDeployment)
	}
}
