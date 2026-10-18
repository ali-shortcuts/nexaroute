package httpapi

import (
	"strings"
	"testing"
)

// TestRTLMirroringContract (#489 item 4): direction-carrying
// chains must mirror in RTL — the topology connector gradient, the
// waterfall/request-step accent edge (logical properties), and
// directional arrow glyphs (flipped .dir-arrow wrappers).
func TestRTLMirroringContract(t *testing.T) {
	cssBytes, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBytes)
	for _, want := range []string{
		"[dir=rtl] .topology-line{background:linear-gradient(270deg,transparent,var(--accent),transparent)}",
		"[dir=rtl] .topology-line{background:linear-gradient(180deg,transparent,var(--accent),transparent)}",
		".waterfall-step{",
		"border-inline-start-width:3px",
		".waterfall-step.good{border-inline-start-color:var(--good)}",
		".waterfall-step.bad{border-inline-start-color:var(--bad)}",
		".waterfall-step.warn{border-inline-start-color:var(--warn)}",
		".waterfall-step.neutral{border-inline-start-color:var(--accent)}",
		"border-inline-start:3px solid var(--good)",
		".request-steps li.retry{border-inline-start-color:var(--warn)}",
		".dir-arrow{display:inline-block}",
		"[dir=rtl] .dir-arrow{transform:scaleX(-1)}",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("styles.css missing RTL mirroring rule %q", want)
		}
	}
	for _, gone := range []string{
		".waterfall-step.good{border-left-color",
		".waterfall-step.bad{border-left-color",
		".waterfall-step.warn{border-left-color",
		".waterfall-step.neutral{border-left-color",
		".request-steps li.retry{border-left-color",
	} {
		if strings.Contains(css, gone) {
			t.Errorf("styles.css still carries physical accent edge %q", gone)
		}
	}

	js := readAppJS(t)
	if n := strings.Count(js, `<span class="dir-arrow">→</span>`); n != 3 {
		t.Errorf("setup-step arrows wrapped in .dir-arrow: got %d, want 3", n)
	}
	if !strings.Contains(js, `<span class="dir-arrow">↗</span>`) {
		t.Error("activity metric arrow is not wrapped in .dir-arrow")
	}
	if strings.Contains(js, "'✓' : '→'") {
		t.Error("a bare (unflippable) setup-step arrow remains in app.js")
	}
}
