package httpapi

import (
	"strings"
	"testing"
)

// M2-7 + M2-5: the Evals and Observability pages are wired into the SPA.
func TestEvalsObservabilityPageAssets(t *testing.T) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []string{`data-page="evals"`, `data-page="observability"`} {
		if !strings.Contains(string(index), page) {
			t.Errorf("index.html missing %s", page)
		}
	}
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(app)
	for _, want := range []string{
		"evals: evalsView",
		"observability: observabilityView",
		"/admin/api/scorecards?limit=",
		"/admin/api/evaluation/runs?limit=",
		"/admin/api/evaluation/run",
		"nexaroute_deployment_request_latency_ms",
		"nexaroute_prom_url",
		"nexaroute_grafana_url",
		"circuit_state",
		"recent_error_rate",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	css, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{".score-bar", ".obs-table"} {
		if !strings.Contains(string(css), want) {
			t.Errorf("styles.css missing %q", want)
		}
	}
}
