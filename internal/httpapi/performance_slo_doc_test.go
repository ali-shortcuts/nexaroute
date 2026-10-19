package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #437: the SLO document must stay anchored to its measured
// evidence — the baseline artifacts, the rerun command, the
// fault-injection inventory, and the honest UNVERIFIED section.
// If the doc drifts from the artifacts it cites, this fails.
func TestPerformanceSLODocAnchoredToEvidence(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "PERFORMANCE_SLO.md"))
	if err != nil {
		t.Fatalf("read docs/PERFORMANCE_SLO.md: %v", err)
	}
	text := string(doc)
	for _, want := range []string{
		"scripts/load.sh",
		"slo-baseline-pool10.txt",
		"slo-baseline-pool100.txt",
		"slo-baseline-pool1000.txt",
		"baseline-20261018.txt",
		"6c332fa5", // the commit the fresh baseline was measured on
		"failure_injection_test.go",
		"gameday_test.go",
		"UNVERIFIED",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("PERFORMANCE_SLO.md missing evidence anchor %q", want)
		}
	}
	// Every cited fresh baseline artifact must actually exist.
	for _, name := range []string{"slo-baseline-pool10.txt", "slo-baseline-pool100.txt", "slo-baseline-pool1000.txt", "baseline-20261018.txt"} {
		if _, err := os.Stat(filepath.Join("..", "..", "benchmarks", "load", name)); err != nil {
			t.Errorf("cited baseline artifact missing: %v", err)
		}
	}
}
