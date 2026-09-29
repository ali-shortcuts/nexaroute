package httpapi

import (
	"strings"
	"testing"
)

// Live ring v2: topology reconciled by deployment id, tilted ellipse, NEXA
// ROUTER core, SSE-driven animation only, polling fallback indicator,
// dedup, 12-anim cap with coalescing, reduced-motion honor.
func TestDashboardLiveRingV2Contracts(t *testing.T) {
	html, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"NEXA ROUTER",
		`id="ringSvg"`,
		`id="ringGuides"`,
		`id="links"`,
		`id="ringFx"`,
		`id="ringParticles"`,
		`id="ringCore"`,
		`id="ringCoalesced"`,
		`id="ringStatus"`,
		`id="coreSub"`,
		`id="routerStrategyLabel"`,
	} {
		if !strings.Contains(string(html), want) {
			t.Fatalf("index.html missing live-ring-v2 contract %q", want)
		}
	}

	js, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ringGeometry",
		"ringNodePos",
		"ringTilt",
		"ensureRingLayers",
		"layoutRingGuides",
		"startRingParticles",
		"drainRingEvent",
		"applyRingEvent",
		"trackRingInflight",
		"brightenRingLinks",
		"updateRingCoalesced",
		"updateRingStatus",
		"ringReducedMotion",
		"pollLiveEventsFallback",
		"route_attempt",
		"route_skip",
		"route_fail",
		"stream_fail",
		"provider_timeout",
		"failover",
		"route_ok",
		"candidate_exhausted",
		"ringAnim",
		"maxAnims: 12",
		"prefers-reduced-motion",
	} {
		if !strings.Contains(string(js), want) {
			t.Fatalf("app.js missing live-ring-v2 contract %q", want)
		}
	}
	// Animation must be event-driven, never fake timers generating telemetry.
	for _, forbidden := range []string{
		"setInterval(()=>drainRingEvent",
		"setInterval(function(){drainRingEvent",
		"fakeRouteEvent(",
		"syntheticRouteEvent(",
	} {
		if strings.Contains(string(js), forbidden) {
			t.Fatalf("app.js contains forbidden synthetic telemetry driver %q", forbidden)
		}
	}

	css, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"#ringSvg",
		".ring-guide",
		".ring-particle",
		".live.polling",
		".ring-coalesced",
		".ring-status",
		".ring-anim",
		"anim-attempt",
		"anim-skip",
		"anim-fail",
		"anim-failover",
		"anim-ok",
		"anim-exhausted",
		"prefers-reduced-motion",
		"link-hot",
	} {
		if !strings.Contains(string(css), want) {
			t.Fatalf("styles.css missing live-ring-v2 contract %q", want)
		}
	}
}
