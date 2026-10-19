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
		"--ok", "--degraded", "--down", "--live", "--stale", "--estimated", "--actual", "--bad-ink",
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

// TestDesignTokenSingleSourceOfTruth (#489 item 1): after the full
// literal migration, NO raw color literal may exist in styles.css
// outside the token fence — the fence is the single source of truth.
// Comments are stripped first: issue references like "#399" in prose
// are not colors. Alpha variants of palette colors must be expressed
// as color-mix over the themed token, one-off values as named tokens
// defined inside the fence in both themes.
func TestDesignTokenSingleSourceOfTruth(t *testing.T) {
	css, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	s := string(css)
	end := strings.Index(s, "[tokens:end]")
	if end < 0 {
		t.Fatal("styles.css is missing the [tokens:end] fence marker")
	}
	rest := s[end:]
	commentRe := regexp.MustCompile(`(?s)/\*.*?\*/`)
	rest = commentRe.ReplaceAllString(rest, "")
	litRe := regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|rgba?\([^)]*\)|hsla?\([^)]*\)`)
	matches := litRe.FindAllString(rest, -1)
	if len(matches) > 0 {
		seen := map[string]int{}
		for _, m := range matches {
			seen[m]++
		}
		t.Errorf("styles.css has %d raw color literals outside the token fence (%d distinct), e.g. %v — migrate them into the fence as tokens or color-mix over tokens",
			len(matches), len(seen), matches[:min(5, len(matches))])
	}

	// Full-fence parity, both directions: every token defined in the
	// dark :root block must exist in the light block and vice versa.
	fence := s[strings.Index(s, "[tokens:begin]"):end]
	rootBlock := cssRuleBlock(fence, ":root{")
	lightBlock := cssRuleBlock(fence, "html[data-theme=light]{")
	propRe := regexp.MustCompile(`--[a-z0-9-]+:`)
	names := func(block string) map[string]bool {
		out := map[string]bool{}
		for _, m := range propRe.FindAllString(block, -1) {
			out[strings.TrimSuffix(m, ":")] = true
		}
		return out
	}
	rootNames, lightNames := names(rootBlock), names(lightBlock)
	for n := range rootNames {
		if !lightNames[n] {
			t.Errorf("token %s defined in dark :root but missing in light theme", n)
		}
	}
	for n := range lightNames {
		if !rootNames[n] {
			t.Errorf("token %s defined in light theme but missing in dark :root", n)
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

func TestLightThemeSidebarAndErrorTextUseContrastTokens(t *testing.T) {
	css, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	s := string(css)
	required := []string{
		"html[data-theme=light] .notice.error { color: var(--bad-ink); }",
		"@media (min-width: 701px)",
		"html[data-theme=light] .sidebar .brand strong { color: var(--slate-50); }",
		"html[data-theme=light] .sidebar .brand small { color: var(--slate-300); }",
		"html[data-theme=light] .sidebar .brand-mark { color: var(--tint-blue-2); }",
		"html[data-theme=light] .sidebar .nav-group { color: var(--slate-300); }",
		"html[data-theme=light] .sidebar .primary-nav button:not(.active) { color: var(--slate-300); }",
		"html[data-theme=light] .sidebar .primary-nav button:not(.active) span { color: var(--tint-blue-2); }",
		"html[data-theme=light] .sidebar .sidebar-foot #version { color: var(--slate-300); }",
	}
	for _, rule := range required {
		if !strings.Contains(s, rule) {
			t.Errorf("missing contrast rule %q", rule)
		}
	}
}
