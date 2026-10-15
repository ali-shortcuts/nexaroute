package httpapi

import (
	"regexp"
	"strings"
	"testing"
)

// Regression (issue #357 audit, confirmed on main @ 0263c0a): the
// 'save-settings' action built its routing payload as
//
//	const r = { ...(state.snap.config?.routing || {}), ...,
//		max_attempts: Number(r.max_attempts || 1) }
//
// `r` is referenced inside its own const initializer, so every click on
// "Save changes" threw a TDZ ReferenceError before the PUT was sent and
// settings could never be saved. The fix must read max_attempts from a
// snapshot binding taken before the payload object is constructed.
func TestSaveSettingsHandlerHasNoSelfReference(t *testing.T) {
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(app)
	start := strings.Index(js, "'save-settings':")
	if start < 0 {
		t.Fatal("app.js: 'save-settings' action not found")
	}
	handler := js[start:]
	if end := strings.Index(handler, "'save-profile-privacy'"); end > 0 {
		// The actions table continues past save-settings; bound the scan to
		// a window comfortably larger than the handler body.
		_ = end
	}
	if len(handler) > 1200 {
		handler = handler[:1200]
	}
	// Anti-pattern: `const <name> = { ... Number(<name>.` — a binding used
	// inside its own initializer. (Go's RE2 has no backreferences, so match
	// each const-object binding and check its initializer window manually.)
	bindings := regexp.MustCompile(`const\s+([A-Za-z_$][\w$]*)\s*=\s*\{`).FindAllStringSubmatchIndex(handler, -1)
	for _, b := range bindings {
		name := handler[b[2]:b[3]]
		end := b[1] + 500
		if end > len(handler) {
			end = len(handler)
		}
		window := handler[b[0]:end]
		if strings.Contains(window, "Number("+name+".") {
			t.Fatalf("save-settings handler references %q inside its own const initializer (TDZ ReferenceError at runtime): %q", name, window)
		}
	}
	// The handler must still send a numeric max_attempts derived from the
	// previous routing snapshot, and still PUT to the settings endpoint.
	if !strings.Contains(handler, "max_attempts: Number(") {
		t.Error("save-settings handler no longer sends a numeric max_attempts")
	}
	if !strings.Contains(handler, "state.snap.config?.routing") {
		t.Error("save-settings handler no longer derives routing from the config snapshot")
	}
	if !strings.Contains(handler, "/admin/api/settings") {
		t.Error("save-settings handler no longer PUTs to /admin/api/settings")
	}
}
