package httpapi

import (
	"strings"
	"testing"
)

// M2-8: the Playground page is wired into the embedded SPA shell.
func TestPlaygroundPageAssets(t *testing.T) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `data-page="playground"`) {
		t.Fatal("index.html has no Playground nav entry")
	}
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(app)
	for _, want := range []string{
		"playground: playgroundView",
		"function pgExclusions()",
		"function pgSend()",
		"x-gateway-deployment",
		"x-gateway-route-profile",
		"include_usage: true",
		"nexaroute_pg_key",
		"input_cost_per_mtok",
		"trains on data — excluded by the no-training profile",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
}
