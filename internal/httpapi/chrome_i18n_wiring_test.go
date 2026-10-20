package httpapi

import (
	"strings"
	"testing"
)

// Regression: the chrome (I18N) dictionary carried translations for the
// auth-control and connection-status strings in all three languages, but
// renderAuthControl and setConnection hardcoded the English literals, so
// under fa/ps those shell strings stayed English and the translations
// were dead. Every chrome key must be consumed through t(), and the
// hardcoded literals must survive only as the en dictionary values.
func TestChromeStringsGoThroughI18N(t *testing.T) {
	appBytes, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	app := string(appBytes)
	for _, key := range []string{"signInSSO", "emergency", "signOut", "signedIn", "connected", "unavailable", "connecting", "live", "offline"} {
		if !strings.Contains(app, "t('"+key+"')") {
			t.Errorf("app.js never consumes chrome i18n key %q via t() — the string is hardcoded somewhere", key)
		}
	}
	for _, literal := range []string{"'Sign in with SSO'", "'Emergency access'"} {
		if n := strings.Count(app, literal); n != 1 {
			t.Errorf("literal %s occurs %d times in app.js, want exactly 1 (the en dictionary definition); a hardcoded copy bypasses i18n", literal, n)
		}
	}
	if strings.Contains(app, "· Sign out") {
		t.Error("app.js still hardcodes the '· Sign out' suffix instead of t('signOut')")
	}
	if !strings.Contains(app, "state.connectionStatus = { ok: !!ok, bad: !!bad }") {
		t.Error("setConnection must retain its state so the labels can be refreshed after a language change")
	}
	if !strings.Contains(app, "setConnection(state.connectionStatus.ok, state.connectionStatus.bad)") {
		t.Error("applyLang must refresh connection-status labels immediately when the language changes")
	}
}
