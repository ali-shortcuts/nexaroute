package httpapi

import (
	"testing"
	"time"
)

// #436 slice 2: the snapshot surfaces the manifest with effective
// (staleness-applied) states — the same evidence the docs table
// is generated from.
func TestConformanceSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	snap := conformanceSnapshot(now)
	if snap["version"] != "conformance.v1" {
		t.Fatalf("version = %v", snap["version"])
	}
	entries, _ := snap["entries"].([]map[string]any)
	if len(entries) == 0 {
		t.Fatal("no entries")
	}
	var anthropic map[string]any
	for _, e := range entries {
		if e["dialect"] == "anthropic_compatible" {
			anthropic = e
		}
		if e["evidence_source"] == "" || e["evidence_date"] == "" {
			t.Errorf("entry %v missing evidence provenance", e["dialect"])
		}
		dims, _ := e["dimensions"].([]map[string]any)
		if len(dims) == 0 {
			t.Errorf("entry %v has no dimensions", e["dialect"])
		}
	}
	if anthropic == nil {
		t.Fatal("anthropic_compatible entry missing")
	}
	if anthropic["effective_status"] != "FIXTURE-VERIFIED" {
		t.Fatalf("fresh evidence effective = %v", anthropic["effective_status"])
	}
	if anthropic["native_passthrough"] != true {
		t.Fatalf("native flag = %v", anthropic["native_passthrough"])
	}

	// A year later the same verified evidence must read STALE,
	// while UNSUPPORTED claims (absence assertions) never age.
	future := conformanceSnapshot(now.AddDate(1, 0, 0))
	fEntries, _ := future["entries"].([]map[string]any)
	for _, e := range fEntries {
		if e["dialect"] == "anthropic_compatible" && e["effective_status"] != "STALE" {
			t.Fatalf("aged evidence effective = %v, want STALE", e["effective_status"])
		}
		dims, _ := e["dimensions"].([]map[string]any)
		for _, d := range dims {
			if d["state"] == "UNSUPPORTED" && d["effective_state"] != "UNSUPPORTED" {
				t.Fatalf("UNSUPPORTED dimension aged to %v", d["effective_state"])
			}
			if d["state"] == "FIXTURE-VERIFIED" && d["effective_state"] != "STALE" && e["effective_status"] == "STALE" {
				t.Fatalf("verified dimension did not go stale with its entry: %v", d)
			}
		}
	}
}
