package httpapi

import (
	"strings"
	"testing"
)

// M2-4 + M2-9: the Policies page (with the local guardrail
// simulator) and the Settings route-profile privacy editor.
func TestPoliciesPageAssets(t *testing.T) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `data-page="policies"`) {
		t.Fatal("index.html has no Policies nav entry")
	}
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(app)
	for _, want := range []string{
		"policies: policiesView",
		"function policiesView()",
		"guardrailClasses",
		"pii_email",
		"secret_api_key",
		"prompt_injection",
		"decision_policies",
		"save-profile-privacy",
		"/admin/api/route-profiles/",
		"no_training",
		"Route-profile privacy",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
}
