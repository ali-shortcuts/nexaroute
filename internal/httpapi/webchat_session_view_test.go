package httpapi

import (
	"strings"
	"testing"
)

// #421 item 6: the WebChat shows a session view fed by real
// evidence — and it actually joins a session: a stable
// x-session-id per conversation (a header the gateway's
// session-key derivation already honors) lets router affinity
// pin the conversation, and the strip renders the current pin,
// cache state and context trend from the per-turn response
// badges, never from estimates.
func TestWebChatSessionViewWiring(t *testing.T) {
	appBytes, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(appBytes)
	for _, want := range []string{
		"function pgSessionID()",
		"state.pgSession",
		"'x-session-id': pgSessionID()", // the conversation joins one affinity session
		"function pgSessionHTML()",
		"function pgRenderSession()",
		`id="pg-session"`,
		"last.badge.deployment", // pin from the last turn's real identity headers
		"last.badge.cache",      // cache state from the real response header
		"m.badge.inTok",         // context trend from real per-turn usage
		"bt('pg_session')",
		"'pg_session': 'Session'",
		"'pg_session': 'نشست'",
		"state.pgSession = ''", // clearing starts a new session
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js missing session-view wiring %q", want)
		}
	}
}
