package httpapi

import (
	"strings"
	"testing"
)

// Phase 17 slice 1 (#417): the Playground is now WebChat — a
// conversation whose every assistant reply carries a route
// transparency badge built from the gateway's real response
// headers and measured timing/usage; advanced numeric controls
// live behind one Advanced toggle; streaming can be stopped.
func TestWebChatBadgeWiring(t *testing.T) {
	appBytes, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(appBytes)
	for _, want := range []string{
		"playground: ['WebChat'",
		"state.pgMessages",        // conversation model
		"function pgBadgeHTML(m)", // per-message badge
		"function pgHistory()",    // multi-turn history
		"x-gateway-public-model",  // requested alias from the real response
		"'connecting…'",           // attempt state before headers
		"'streaming…'",            // attempt state while streaming
		"'stopped'",               // honest terminal state for aborts
		"AbortController",         // stop control
		`id="pg-stop"`,            // stop button
		"pg-advanced",             // one Advanced toggle for numeric controls
		"bt('pg_advanced')",
		"bt('pg_local_only')", // privacy line: tab-memory only
		"'pg_local_only': 'This conversation is kept only in this tab’s memory",
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js missing WebChat wiring %q", want)
		}
	}
	// Advanced parameters must be inside the toggle, not beside
	// the composer: the temperature field renders after the
	// Advanced summary in the view markup.
	adv := strings.Index(appJS, "pg-advanced")
	temp := strings.Index(appJS, `id="pg-temp"`)
	if adv < 0 || temp < 0 || temp < adv {
		t.Error("temperature control is not behind the Advanced toggle")
	}
	indexBytes, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(indexBytes), ">WebChat</button>") {
		t.Error("index.html nav entry not renamed to WebChat")
	}
	cssBytes, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cssBytes), ".pg-badge") {
		t.Error("styles.css missing badge styles")
	}
}
