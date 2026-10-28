package httpapi

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// M2-13: the admin UI ships embedded assets only, inside a size
// budget. Budgets sit ~25% above current sizes so ordinary page
// work fits; raising one is a deliberate, reviewed act.
var webAssetBudgets = map[string]int{
	"web/index.html": 8 << 10,
	// Issue #393 adds full redacted provider-edit metadata and explicit
	// credential-pool controls; keep the aggregate asset ceiling unchanged.
	// Issue #411 groups the primary nav, discloses rare controls behind
	// Advanced, and splits Settings into tabs, adding new EN/FA/PS copy.
	// Keep the aggregate asset ceiling unchanged.
	// Issue #399 (Phase 7) routes the provider and routing control-plane
	// surfaces through the EN/FA/PS body-copy dictionary and adds the
	// routing eligibility/decision view, so app.js grows by translated copy.
	// Issue #399 remainder adds the unified model inventory, status taxonomy,
	// per-request decision timeline, bounded list rendering and the guided
	// first-route wizard, each expanding the three-locale copy dictionary.
	// Supervisor 2026-10-18: raised 272->336 KB after review. The Phase 7
	// remainder merge also absorbed the Phase 10 (#410) live-activity UI
	// (refresh/polling/detail drawer + EN/FA/PS copy), landing app.js at
	// ~269 KB; 336 KB restores the ~25% headroom convention.
	// Phase 6 (2026-10-28): the NexaFlow visual studio (workflow list,
	// palette, canvas, schema config panel, validation, runs) lands
	// app.js at ~366 KB; 460 KB restores the ~25% headroom convention.
	"web/app.js":          460 << 10,
	"web/styles.css":      96 << 10,
	"web/visual-agent.js": 16 << 10,
}

// Supervisor 2026-10-18: raised 320->410 KB after review. The aggregate
// ceiling was set before the Phase 7 remainder (#399) and Phase 10 (#410)
// UI growth landed together; 410 KB restores the ~25% headroom convention
// over the current 336303-byte total.
// Phase 6 (2026-10-28): studio growth lands the total at ~452 KB;
// 552 KB restores the ~25% headroom convention.
const webAssetTotalBudget = 552 << 10

func TestWebAssetSizeBudget(t *testing.T) {
	total := 0
	for path, budget := range webAssetBudgets {
		b, err := webFS.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		total += len(b)
		if len(b) > budget {
			t.Errorf("%s is %d bytes, over its %d-byte budget", path, len(b), budget)
		}
	}
	if total > webAssetTotalBudget {
		t.Errorf("total web assets %d bytes, over the %d-byte budget", total, webAssetTotalBudget)
	}
}

func TestWebAssetsEmbeddedOnly(t *testing.T) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{`src="/visual-agent.js"`, `src="/app.js"`, `href="/styles.css"`} {
		if !strings.Contains(string(index), ref) {
			t.Errorf("index.html missing local asset reference %s", ref)
		}
	}
	if strings.Contains(string(index), `src="http`) || strings.Contains(string(index), `href="http`) {
		t.Error("index.html references an external asset")
	}
	css, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(css), "@import") || strings.Contains(string(css), "url(") {
		t.Error("styles.css must not import or reference external resources")
	}
}

// M2-11: every text-bearing foreground/background pair in both
// themes must meet the WCAG 4.5:1 contrast ratio for normal text.
var cssVarPairRe = regexp.MustCompile(`--(bg|surface|surface-2|surface-3|text|muted|subtle|accent|violet|good|warn|bad):(#[0-9a-fA-F]{6}|#[0-9a-fA-F]{3})\b`)

func parseThemeVars(t *testing.T, css, marker string) map[string]string {
	t.Helper()
	i := strings.Index(css, marker)
	if i < 0 {
		t.Fatalf("theme block %q not found", marker)
	}
	seg := css[i:]
	if j := strings.Index(seg, "}"); j >= 0 {
		seg = seg[:j]
	}
	out := map[string]string{}
	for _, m := range cssVarPairRe.FindAllStringSubmatch(seg, -1) {
		out[m[1]] = m[2]
	}
	return out
}

func relLuminance(hex string) float64 {
	if len(hex) == 4 { // #rgb shorthand
		hex = "#" + string([]byte{hex[1], hex[1], hex[2], hex[2], hex[3], hex[3]})
	}
	f := func(i int) float64 {
		v, _ := strconv.ParseInt(hex[i:i+2], 16, 0)
		s := float64(v) / 255
		if s <= 0.03928 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*f(1) + 0.7152*f(3) + 0.0722*f(5)
}

func contrastRatio(a, b string) float64 {
	la, lb := relLuminance(a), relLuminance(b)
	hi, lo := math.Max(la, lb), math.Min(la, lb)
	return (hi + 0.05) / (lo + 0.05)
}

func TestWebThemeContrastWCAG(t *testing.T) {
	cssBytes, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBytes)
	themes := map[string]map[string]string{
		"dark":  parseThemeVars(t, css, ":root{"),
		"light": parseThemeVars(t, css, "html[data-theme=light]{"),
	}
	foregrounds := []string{"text", "muted", "subtle", "accent", "violet", "good", "warn", "bad"}
	backgrounds := []string{"bg", "surface", "surface-2", "surface-3"}
	for name, vars := range themes {
		for _, fg := range foregrounds {
			for _, bg := range backgrounds {
				if vars[fg] == "" || vars[bg] == "" {
					t.Fatalf("%s theme: missing --%s or --%s", name, fg, bg)
				}
				if r := contrastRatio(vars[fg], vars[bg]); r < 4.5 {
					t.Errorf("%s theme: --%s (%s) on --%s (%s) = %.2f:1, below WCAG 4.5:1", name, fg, vars[fg], bg, vars[bg], r)
				}
			}
		}
	}
}
