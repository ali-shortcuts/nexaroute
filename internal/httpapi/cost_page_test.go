package httpapi

import (
	"strings"
	"testing"
)

// M2-3: the Budgets & Cost page is wired into the embedded SPA shell.
func TestCostPageAssets(t *testing.T) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `data-page="cost"`) {
		t.Fatal("index.html has no Budgets & Cost nav entry")
	}
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(app)
	for _, want := range []string{
		"cost: costView",
		"function costView()",
		"total_estimated_cost_usd",
		"by_deployment",
		"export-cost-csv",
		"export-cost-json",
		"budget_usd",
		"webhook alerts land with the budget backend",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	css, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), ".cost-table") {
		t.Fatal("styles.css missing .cost-table rules")
	}
}
