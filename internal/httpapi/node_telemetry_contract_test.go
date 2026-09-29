package httpapi

import (
	"strings"
	"testing"
)

// B3 contract: additive node telemetry view-model helpers consume only the
// real snap.node_telemetry rows, show source+freshness per KPI, omit absent
// data, and never fabricate metrics or touch routing decisions.
func TestNodeTelemetryPopoverContracts(t *testing.T) {
	html, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `id="nodeTelemetryPopover"`) {
		t.Fatal("index.html missing additive B3 nodeTelemetryPopover element")
	}

	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"node_telemetry",
		"nodeTelemetryById",
		"nodeTelemetryRowHTML",
		"showNodeTelemetryPopover",
		"hideNodeTelemetryPopover",
		"nodeTelemetryAgeText",
		"nodeTelemetryKpiRow",
		"src: ",
	} {
		if !strings.Contains(string(js), want) {
			t.Fatalf("app.js missing B3 node-telemetry contract %q", want)
		}
	}
	// The popover must omit absent KPIs, never synthesize values, and never
	// render raw upstream error text (LastError) in the new surface.
	for _, forbidden := range []string{
		"fakeNodeTelemetry(",
		"syntheticNodeTelemetry(",
		"Math.random() * 100",
		"last_failure.detail",
		"lastFailure.detail",
		"nt-err",
	} {
		if strings.Contains(string(js), forbidden) {
			t.Fatalf("app.js contains forbidden synthetic telemetry %q", forbidden)
		}
	}
	// Scope the renderer check to the B3 popover so unrelated UI detail
	// strings cannot cause false positives/negatives.
	if start := strings.Index(string(js), "function nodeTelemetryRowHTML"); start >= 0 {
		end := strings.Index(string(js)[start:], "\nfunction showNodeTelemetryPopover")
		if end > 0 {
			renderer := string(js)[start : start+end]
			if strings.Contains(renderer, ".detail") {
				t.Fatalf("nodeTelemetryRowHTML must not render raw error detail text")
			}
		}
	} else {
		t.Fatal("app.js missing nodeTelemetryRowHTML for detail guard")
	}

	css, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), ".node-telemetry-popover") {
		t.Fatal("styles.css missing .node-telemetry-popover")
	}
}
