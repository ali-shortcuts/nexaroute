package httpapi

import (
	"strings"
	"testing"
)

// #435: the Cost page Budgets panel renders the live budget state
// (settled / reserved / remaining) the client-keys API now
// carries, with the per-instance scope note — and renders nothing
// numeric for keys whose response lacks the state fields.
func TestCostBudgetStateWiring(t *testing.T) {
	appBytes, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(appBytes)
	for _, want := range []string{
		"budget_remaining_usd",
		"budget_committed_usd",
		"budget_reserved_usd",
		"bt('cb_settled')",
		"bt('cb_reserved')",
		"bt('cb_remaining')",
		"bt('cb_scope')",
		"'cb_remaining': 'remaining'",
		"'cb_remaining': 'باقی‌مانده'",
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js missing budget-state wiring %q", want)
		}
	}
}
