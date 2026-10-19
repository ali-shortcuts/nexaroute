package httpapi

import (
	"regexp"
	"strings"
	"testing"
)

var (
	keyframesRe = regexp.MustCompile(`@keyframes ([a-z0-9-]+)\{((?:[^{}]*\{[^{}]*\})+)\}`)
	animRefRe   = regexp.MustCompile(`animation:([a-z0-9-]+)`)
	layoutProp  = regexp.MustCompile(`(?:^|[;{])\s*(width|height|top|left|right|bottom|margin|padding)\s*:`)
)

// TestMotionContract (#489 item 5): motion stays functional and
// compositor-only — keyframes never animate layout properties,
// transitions name their properties explicitly, every animation
// reference resolves, the keyframe set is exactly the audited
// inventory in docs/COMPONENTS.md, and reduced-motion coverage is
// present.
func TestMotionContract(t *testing.T) {
	cssBytes, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBytes)

	audited := map[string]bool{"state-pulse": true, "visual-pulse": true, "visual-walk": true}
	defined := map[string]bool{}
	for _, m := range keyframesRe.FindAllStringSubmatch(css, -1) {
		name, body := m[1], m[2]
		defined[name] = true
		if !audited[name] {
			t.Errorf("@keyframes %s is not in the audited motion inventory (docs/COMPONENTS.md)", name)
		}
		if lp := layoutProp.FindStringSubmatch(body); lp != nil {
			t.Errorf("@keyframes %s animates layout property %s — forbidden", name, lp[1])
		}
	}
	for name := range audited {
		if !defined[name] {
			t.Errorf("audited @keyframes %s is missing from styles.css", name)
		}
	}
	for _, m := range animRefRe.FindAllStringSubmatch(css, -1) {
		if m[1] == "none" { // reduced-motion / state overrides
			continue
		}
		if !defined[m[1]] {
			t.Errorf("animation references undefined @keyframes %s", m[1])
		}
	}

	if strings.Contains(css, "transition:.18s ease") || strings.Contains(css, "transition:all") {
		t.Error("bare/all-property transition shorthand present — transitions must name their properties")
	}
	if n := strings.Count(css, "transition:transform .18s ease,border-color .18s ease,box-shadow .18s ease"); n != 3 {
		t.Errorf("scoped card transitions: got %d, want 3", n)
	}

	if !strings.Contains(css, "@media(prefers-reduced-motion:reduce)") {
		t.Error("prefers-reduced-motion coverage missing")
	}
}
