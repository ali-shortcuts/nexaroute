package httpapi

import (
	"strings"
	"testing"
)

// Phase 15 (#415): the Routing page renders the coding-agent
// profiles panel from the snapshot, with localized headings.
func TestCodingProfilesPanelWiring(t *testing.T) {
	appBytes, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(appBytes)
	for _, want := range []string{
		"codingProfilesPanel()",
		"state.snap.coding_profiles",
		"bt('cp_title')",
		"bt('cp_d')",
		"'cp_title': 'Coding agent profiles'",
		"'cp_title': 'پروفایل\u200cهای ایجنت کدنویسی'",
		"capability partial",
		"No route profile maps ",
		"data-action=\"cp-test\"", // #415 item 6: one-click connection test per mapping
		"/admin/api/connection-test",
		"state.cpTest", // the bounded test's report line renders in the panel
		"bt('cp_test')",
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js missing coding profiles wiring %q", want)
		}
	}
}

// #415 item 5 remainder: every route-profile mapping row links to
// the exact request history for that profile — the action opens
// Activity's live view pre-filtered by the route profile ID,
// which lifecycle records and events carry as route_profile.
func TestCodingProfilesHistoryLinkWiring(t *testing.T) {
	appBytes, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(appBytes)
	for _, want := range []string{
		`data-action="cp-history"`,
		"data-profile",
		"'cp-history': e =>",
		"state.activityQuery = p",
		"state.activityTab = 'live'",
		"bt('cp_history')",
		"'cp_history': 'History'",
		"'cp_history': 'تاریخچه'",
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js missing profile history-link wiring %q", want)
		}
	}
}
