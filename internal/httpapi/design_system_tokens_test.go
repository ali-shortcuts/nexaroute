package httpapi

import (
	"regexp"
	"strings"
	"testing"
)

// Phase 16 takeover subset (issue #416): the design-token layer is the single
// source of truth for the control-plane visual language. This contract pins
// the fence, the required token set, and dark/light parity, so future UI work
// cannot silently drift back to ad-hoc values.
func TestDesignTokenLayerContract(t *testing.T) {
	css, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	s := string(css)

	begin := strings.Index(s, "[tokens:begin]")
	end := strings.Index(s, "[tokens:end]")
	if begin < 0 || end < 0 || end < begin {
		t.Fatal("styles.css is missing the [tokens:begin]/[tokens:end] fence")
	}
	// The fence must be the first block in the file: no CSS rule precedes it.
	if firstBrace := strings.Index(s, "{"); firstBrace >= 0 && firstBrace < begin {
		t.Errorf("token fence is not the first block in styles.css")
	}
	fence := s[begin:end]

	required := []string{
		// Semantic status tokens.
		"--ok", "--degraded", "--down", "--live", "--stale", "--estimated", "--actual",
		// Type scale.
		"--fs-2xs", "--fs-xs", "--fs-sm", "--fs-md", "--fs-lg", "--fs-xl", "--fs-2xl",
		// Spacing scale.
		"--sp-1", "--sp-2", "--sp-3", "--sp-4", "--sp-5", "--sp-6",
		// Radius scale.
		"--r-sm", "--r-md", "--r-lg", "--r-xl", "--r-pill",
		// Elevation.
		"--shadow-1", "--shadow-2", "--shadow-3",
		// Motion.
		"--dur-fast", "--dur-base", "--dur-slow", "--ease-out",
	}

	rootBlock := cssRuleBlock(fence, ":root{")
	lightBlock := cssRuleBlock(fence, "html[data-theme=light]{")
	if rootBlock == "" {
		t.Fatal("token fence is missing the dark :root block")
	}
	if lightBlock == "" {
		t.Fatal("token fence is missing the html[data-theme=light] block")
	}
	for _, tok := range required {
		if !strings.Contains(rootBlock, tok+":") {
			t.Errorf("dark :root is missing token %s", tok)
		}
		if !strings.Contains(lightBlock, tok+":") {
			t.Errorf("light theme is missing token %s", tok)
		}
	}

	// Theme parity: every custom property defined for dark must also be
	// defined for light (values may differ; names must not).
	propRe := regexp.MustCompile(`--[a-z0-9-]+:`)
	for _, m := range propRe.FindAllString(rootBlock, -1) {
		name := strings.TrimSuffix(m, ":")
		if !strings.Contains(lightBlock, name+":") {
			t.Errorf("token %s is defined for dark but missing in the light theme", name)
		}
	}

	// The additive live-state grammar classes must consume tokens, never
	// raw color literals.
	hexRe := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b`)
	for _, cls := range []string{".status.live{", ".status.stale{", ".cost-estimated{", ".cost-actual{"} {
		b := cssRuleBlock(fence, cls)
		if b == "" {
			t.Errorf("token fence is missing additive class %s", cls)
			continue
		}
		if !strings.Contains(b, "var(--") {
			t.Errorf("class %s does not consume design tokens", cls)
		}
		if hexRe.MatchString(b) {
			t.Errorf("class %s embeds a raw hex literal; use tokens", cls)
		}
	}
}

// cssRuleBlock extracts the first "selector{...}" rule for a flat
// (brace-free declaration) rule. It is intentionally strict: nested rules
// are out of scope for the token contract.
func cssRuleBlock(s, selector string) string {
	i := strings.Index(s, selector)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], "}")
	if j < 0 {
		return ""
	}
	return s[i : i+j+1]
}
