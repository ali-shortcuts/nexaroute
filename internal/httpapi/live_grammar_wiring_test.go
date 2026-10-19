package httpapi

import (
	"strings"
	"testing"
)

// TestLiveGrammarWiring (#489 item 3): the live-data visual grammar
// defined in the token fence must actually be wired into app.js —
// activity states use the dedicated pill states, the connection
// badge is a pill, the Overview live marker is the same pill, and
// cost amounts render through the basis-aware helpers so an
// unknown-basis figure is never shown as a bare dollar amount.
func TestLiveGrammarWiring(t *testing.T) {
	js := readAppJS(t)
	for _, want := range []string{
		// Connection badge: pill grammar incl. the disconnected state.
		"a.connected ? 'live' : 'disconnected'",
		// Overview topology panel: same live pill as Activity.
		"statusPill('● ' + bt('ov_live'), 'live')",
		// Cost basis helpers and their grammar classes.
		"function costAmount(d)",
		"function costTotalAmount(u, total)",
		"basis === 'recorded' ? 'cost-actual' : 'cost-estimated'",
		"basis === 'unknown'",
		"(unknown)",
		// Cost view actually routes amounts through the helpers.
		"${costAmount(d)}",
		"${costTotalAmount(u, total)}",
		// CSV export carries the basis next to the figure.
		"'estimated_cost_usd', 'cost_basis'",
		"d.cost_basis || 'estimated'",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing live-grammar wiring %q", want)
		}
	}
}
