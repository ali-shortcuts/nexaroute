package httpapi

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// M2-3: the Budgets & Cost page is wired into the embedded SPA shell.
func TestCostPageAssets(t *testing.T) {
	index, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `data-page="cost"`) {
		t.Fatal("index.html has no Budgets & Cost nav entry")
	}
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(app)
	for _, want := range []string{
		"cost: costView",
		"function costView()",
		"total_estimated_cost_usd",
		"by_deployment",
		"export-cost-csv",
		"export-cost-json",
		"budget_usd",
		// The Budgets panel distinguishes its states explicitly; an
		// empty list must never masquerade as "no budgets exist".
		"not visible to your role",
		"budget data unavailable",
		"no budgets configured",
		"budgetPanelState",
		"identityShownNote",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing %q", want)
		}
	}
	css, err := webFS.ReadFile("web/styles.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), ".cost-table") {
		t.Fatal("styles.css missing .cost-table rules")
	}
}

// E4 behavioral test: the Budgets panel's classification logic and
// the identity-table cap note are EXECUTED (extracted verbatim from
// app.js and run under node), not merely string-matched. Skipped
// when node is unavailable (verify.sh requires node for app.js
// syntax checks, so CI exercises this).
func TestCostPageBudgetPanelBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	app, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(app)
	start := strings.Index(src, "/* cost-page-logic:start")
	end := strings.Index(src, "/* cost-page-logic:end */")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("cost-page-logic block not found in app.js")
	}
	// Extract the REAL authorization helpers verbatim so the role
	// scenarios below execute production code, not a copy.
	extractFunc := func(name string) string {
		i := strings.Index(src, "function "+name+"(")
		if i < 0 {
			t.Fatalf("function %s not found in app.js", name)
		}
		open := strings.Index(src[i:], "{") + i
		depth := 0
		for j := open; j < len(src); j++ {
			switch src[j] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return src[i : j+1]
				}
			}
		}
		t.Fatalf("function %s: unbalanced braces", name)
		return ""
	}
	// #378: the cost-page logic now renders body copy through the BODY_I18N
	// dictionary seam (bt). Extract the REAL dictionary and lookup verbatim
	// so the scenarios below still execute production code, not a copy.
	// String-aware: dictionary values contain '{n}'-style placeholders.
	extractJSBlock := func(marker string) string {
		i := strings.Index(src, marker)
		if i < 0 {
			t.Fatalf("marker %q not found in app.js", marker)
		}
		open := strings.Index(src[i:], "{") + i
		depth := 0
		var quote byte
		for j := open; j < len(src); j++ {
			c := src[j]
			if quote != 0 {
				if c == '\\' {
					j++
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
					return src[i : j+1]
				}
			}
		}
		t.Fatalf("marker %q: unbalanced braces", marker)
		return ""
	}
	script := "var state = { lang: 'en', auth: { roles: [], emergency_access_enabled: false, oidc_enabled: false, api_key_configured: true }, adminKey: '' };\n" +
		extractJSBlock("const BODY_I18N = {") + "\n" + extractJSBlock("const bt = (key, vars) =>") + "\n" +
		extractFunc("hasRole") + "\n" + extractFunc("isEmergencyAdmin") + "\n" + extractFunc("canManageKeys") + "\n" +
		src[start:end] + `
function eq(got, want, msg) { if (got !== want) { console.error('FAIL ' + msg + ': got ' + JSON.stringify(got) + ', want ' + JSON.stringify(want)); process.exit(1); } }
eq(budgetPanelState('forbidden', [{ budget_usd: 10 }]).kind, 'forbidden', 'a role that cannot see keys must not read budgets even from a stale list');
eq(budgetPanelState('forbidden', []).budgeted.length, 0, 'forbidden lists nothing');
eq(budgetPanelState('error', []).kind, 'unavailable', 'a failed key fetch is unavailable, not empty');
eq(budgetPanelState('unknown', []).kind, 'loading', 'before the first fetch the panel is loading');
eq(budgetPanelState('ok', []).kind, 'none', 'no keys at all means no configured budgets');
const some = budgetPanelState('ok', [{ id: 'a', budget_usd: 0 }, { id: 'b', budget_usd: 25 }]);
eq(some.kind, 'some', 'one budgeted key is some');
eq(some.budgeted.length, 1, 'exactly the budgeted keys are listed');
eq(some.budgeted[0].id, 'b', 'the listed key is the budgeted one');
eq(budgetPanelState('ok', [{ id: 'a', budget_usd: 0 }]).kind, 'none', 'budget field present but zero on every key is none');
eq(budgetPanelState('ok', [{ id: 'a' }]).kind, 'legacy', 'budget field absent on every key means the backend does not expose budgets');
eq(identityShownNote(100), '', 'no cap note at exactly 100 identities');
const note = identityShownNote(250);
if (!note.includes('100') || !note.includes('250')) { console.error('FAIL cap note must disclose shown and total counts, got: ' + note); process.exit(1); }
// Role predicate (E4 sign-off Finding 1): the server grants
// keys.manage to owner AND admin (internal/authz); the frontend
// predicate must agree, and the panel outcome must follow it.
function panelKindFor(roles, keys) { state.auth.roles = roles; state.auth.emergency_access_enabled = false; return budgetPanelState(canManageKeys() ? 'ok' : 'forbidden', keys).kind; }
const budgetedKeys = [{ id: 'k1', budget_usd: 100 }];
eq(panelKindFor(['owner'], budgetedKeys), 'some', 'owner fetches key records and sees configured budgets');
eq(panelKindFor(['admin'], budgetedKeys), 'some', 'admin still sees configured budgets');
eq(panelKindFor(['operator'], budgetedKeys), 'forbidden', 'operator without keys.manage gets the forbidden state, not an empty list');
eq(panelKindFor(['viewer'], budgetedKeys), 'forbidden', 'viewer gets the forbidden state');
state.auth.roles = []; state.auth.emergency_access_enabled = true; state.auth.api_key_configured = false;
eq(canManageKeys(), true, 'emergency access still manages keys');
console.log('cost page behavior OK');
`
	path := filepath.Join(t.TempDir(), "cost_page_behavior_test.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("node behavior test failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "cost page behavior OK") {
		t.Fatalf("unexpected node output: %s", out)
	}
}
