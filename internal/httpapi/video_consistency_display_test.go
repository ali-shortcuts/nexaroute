package httpapi

import (
	"strings"
	"testing"
)

// #420 item 3: the Studio job card states which consistency
// mechanisms are in play — and says "none" when there are
// none, instead of implying consistency.
func TestVideoConsistencyDisplayWiring(t *testing.T) {
	appBytes, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	appJS := string(appBytes)
	for _, want := range []string{
		"Array.isArray(j.consistency)",
		"j.consistency.map(c => esc(c.detail)).join(' · ')",
		"none — no consistency mechanism in play",
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js missing consistency display wiring %q", want)
		}
	}
}
