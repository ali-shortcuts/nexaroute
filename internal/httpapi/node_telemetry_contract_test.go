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
	// The popover must omit absent KPIs, never synthesize values.
	for _, forbidden := range []string{
		"fakeNodeTelemetry(",
		"syntheticNodeTelemetry(",
		"Math.random() * 100",
	} {
		if strings.Contains(string(js), forbidden) {
			t.Fatalf("app.js contains forbidden synthetic telemetry %q", forbidden)
		}
	}

	css, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), ".node-telemetry-popover") {
		t.Fatal("styles.css missing .node-telemetry-popover")
	}
}
