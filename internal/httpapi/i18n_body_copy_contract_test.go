package httpapi

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// M2-12 body copy (#378): page body strings live in the BODY_I18N dictionary
// (EN/FA/PS) and are rendered through bt(), the body-copy twin of t().
// Static-scan contract, same style as the other web contract tests:
//
//  1. every body-copy key present in EN exists in FA and PS (and vice versa);
//  2. every bt('key') call site resolves to a defined EN key;
//  3. no hard-coded Latin-script body strings remain in the covered page
//     functions (overview, access, audit, cost, policies, playground,
//     settings + their render helpers).
func TestBodyCopyI18nContract(t *testing.T) {
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(app)

	if !strings.Contains(js, "const BODY_I18N = {") {
		t.Fatal("app.js missing BODY_I18N dictionary")
	}
	if !strings.Contains(js, "const bt = (key, vars) =>") {
		t.Fatal("app.js missing bt() body-copy lookup")
	}

	// ---- 1. locale key-set parity ---------------------------------------
	block := extractBraceBlock(t, js, "const BODY_I18N = {")
	keys := map[string]map[string]bool{}
	for _, locale := range []string{"en", "fa", "ps"} {
		sec := extractBraceBlock(t, block, locale+": {")
		keys[locale] = map[string]bool{}
		for _, m := range bodyKeyRe.FindAllStringSubmatch(sec, -1) {
			keys[locale][m[1]] = true
		}
		if len(keys[locale]) == 0 {
			t.Fatalf("no body-copy keys parsed for locale %q", locale)
		}
	}
	t.Logf("body-copy keys: en=%d fa=%d ps=%d", len(keys["en"]), len(keys["fa"]), len(keys["ps"]))
	for _, locale := range []string{"fa", "ps"} {
		for _, key := range sortedKeys(keys["en"]) {
			if !keys[locale][key] {
				t.Errorf("body-copy key %q present in EN but missing in %s", key, strings.ToUpper(locale))
			}
		}
		for _, key := range sortedKeys(keys[locale]) {
			if !keys["en"][key] {
				t.Errorf("body-copy key %q present in %s but missing in EN", key, strings.ToUpper(locale))
			}
		}
	}

	// ---- 2. every bt() call site resolves --------------------------------
	for _, m := range bodyCallRe.FindAllStringSubmatch(js, -1) {
		if !keys["en"][m[1]] {
			t.Errorf("bt(%q) has no EN entry in BODY_I18N", m[1])
		}
	}

	// ---- 3. no hard-coded Latin body strings on covered pages ------------
	covered := []string{
		"overviewView", "healthLayer", "topologyTargets", "eventList",
		"identityShownNote", "costView", "accessView", "auditView",
		"policiesView", "playgroundView", "pgExclusions", "settingsView",
		// Phase 7 provider and routing control plane (issue #399):
		"providersView", "providerCard", "routingView", "routeEligibilityPanel",
		"routeCard", "routePrivacyNotice",
		// Phase 7 remainder (issue #399): provider form, route form, model
		// discovery, status taxonomy, model inventory, decision timeline and
		// the guided first-route wizard.
		"providerForm", "openRoute", "detectModels",
		"providerStatusTaxonomy", "modelStatusTaxonomy",
		"statusTaxonomyLegend", "modelInventoryRows", "modelInventoryRow",
		"modelsView", "requestDecisionTimeline", "requestDecisionRow",
		"firstRouteCandidates", "firstRouteCostWarning", "openFirstRouteWizard",
	}
	for _, fn := range covered {
		src := extractFunctionLine(t, js, fn)
		for _, node := range textNodes(functionStaticText(src)) {
			node = strings.TrimSpace(node)
			if node == "" || bodyCopyAllow[node] {
				continue
			}
			if latinRunRe.MatchString(node) {
				t.Errorf("%s: hard-coded body string %q (route through bt())", fn, truncate(node, 80))
			}
		}
	}
}

var (
	bodyKeyRe  = regexp.MustCompile(`(?m)^\s*'([a-z0-9_]+)': '`)
	bodyCallRe = regexp.MustCompile(`\bbt\(\s*'([a-z0-9_]+)'`)
	latinRunRe = regexp.MustCompile(`[A-Za-z]{3,}`)
	textNodeRe = regexp.MustCompile(`>([^<>]*)<`)
)

// bodyCopyAllow lists exact rendered text nodes that are intentionally not
// translated: product/technical proper nouns and protocol identifiers kept in
// Latin script per the chrome dictionary precedent.
var bodyCopyAllow = map[string]bool{
	"NR": true, "LIVE": true, "NexaRoute": true,
	"OpenAI · /v1/chat/completions": true, "Anthropic · /v1/messages": true,
	"nexaroute audit verify": true, "nexaroute policy": true,
	"ready_mesh": true, "ready_queue": true, "adaptive": true,
	"no_training": true,
	"Bearer":      true, "x-api-key": true, "x-goog-api-key": true, "api-key": true,
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// extractBraceBlock returns the {...} block starting at the first occurrence
// of marker (marker includes the opening brace), honouring JS string escapes.
func extractBraceBlock(t *testing.T, js, marker string) string {
	t.Helper()
	start := strings.Index(js, marker)
	if start < 0 {
		t.Fatalf("marker %q not found", marker)
	}
	i := start + len(marker) - 1 // at the opening brace
	depth := 0
	var quote byte
	for ; i < len(js); i++ {
		c := js[i]
		if quote != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return js[start:i]
			}
		}
	}
	t.Fatalf("unterminated block after %q", marker)
	return ""
}

// extractFunctionLine returns the source of a one-line function declaration.
func extractFunctionLine(t *testing.T, js, name string) string {
	t.Helper()
	start := strings.Index(js, "function "+name+"(")
	if start < 0 {
		t.Fatalf("function %s not found", name)
	}
	end := strings.Index(js[start:], "\n")
	if end < 0 {
		end = len(js) - start
	}
	src := js[start : start+end]
	if strings.Count(src, "{") != strings.Count(src, "}") {
		t.Fatalf("function %s is not a single balanced line; contract scanner needs updating", name)
	}
	return src
}

// functionStaticText concatenates the static text of every template literal
// in fn, with ${...} interpolations removed but nested template literals'
// static text preserved. Single-quoted literals containing markup are
// included as well.
func functionStaticText(fn string) string {
	var sb strings.Builder
	i := 0
	for i < len(fn) {
		switch fn[i] {
		case '`':
			sb.WriteString(templateStaticText(fn, &i))
			sb.WriteByte('\n')
		case '\'':
			sb.WriteString(quotedStaticText(fn, &i, '\''))
			sb.WriteByte('\n')
		case '\\':
			i += 2
		default:
			i++
		}
	}
	return sb.String()
}

// templateStaticText consumes a template literal starting at *i (the opening
// backtick) and returns its static text.
func templateStaticText(s string, i *int) string {
	var sb strings.Builder
	*i++ // opening backtick
	for *i < len(s) {
		c := s[*i]
		switch {
		case c == '\\' && *i+1 < len(s):
			sb.WriteByte(s[*i+1])
			*i += 2
		case c == '`':
			*i++
			return sb.String()
		case c == '$' && *i+1 < len(s) && s[*i+1] == '{':
			*i += 2
			skipInterpolation(s, i, &sb)
		default:
			sb.WriteByte(c)
			*i++
		}
	}
	return sb.String()
}

// skipInterpolation consumes through the matching close brace of a ${...}
// interpolation; static text of nested template literals is kept.
func skipInterpolation(s string, i *int, sb *strings.Builder) {
	depth := 1
	var quote byte
	for *i < len(s) && depth > 0 {
		c := s[*i]
		if quote != 0 {
			if c == '\\' {
				*i += 2
				continue
			}
			if c == quote {
				quote = 0
			}
			*i++
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			*i++
		case '`':
			sb.WriteString(templateStaticText(s, i))
		case '{':
			depth++
			*i++
		case '}':
			depth--
			*i++
		default:
			*i++
		}
	}
}

// quotedStaticText consumes a quoted literal for single-quote strings that
// embed markup (e.g. '<p class="muted">…</p>').
func quotedStaticText(s string, i *int, q byte) string {
	var sb strings.Builder
	*i++ // opening quote
	for *i < len(s) {
		c := s[*i]
		if c == '\\' && *i+1 < len(s) {
			sb.WriteByte(s[*i+1])
			*i += 2
			continue
		}
		if c == q {
			*i++
			break
		}
		sb.WriteByte(c)
		*i++
	}
	out := sb.String()
	if !strings.Contains(out, "<") {
		return ""
	}
	return out
}

// textNodes returns raw HTML text-node contents from static markup.
func textNodes(static string) []string {
	var out []string
	for _, m := range textNodeRe.FindAllStringSubmatch(static, -1) {
		out = append(out, m[1])
	}
	return out
}
