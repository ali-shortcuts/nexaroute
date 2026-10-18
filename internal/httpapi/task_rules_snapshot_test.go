package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/taskprofile"
)

// Phase 19 UI surfacing (#510 item 7): the admin snapshot carries
// the classifier rule table (version + readable rows), and app.js
// renders it plus the per-route task-intelligence sentence and the
// task_classified trace line.
func TestTaskRulesSnapshot(t *testing.T) {
	snap := taskRuleTableSnapshot()
	if snap["version"] != taskprofile.RuleVersion {
		t.Fatalf("version=%v want %q", snap["version"], taskprofile.RuleVersion)
	}
	rows, ok := snap["rules"].([]map[string]any)
	if !ok || len(rows) != len(taskprofile.RuleTable()) {
		t.Fatalf("rules rows=%v, want %d projected rows", snap["rules"], len(taskprofile.RuleTable()))
	}
	for _, row := range rows {
		for _, k := range []string{"name", "when", "signals", "produces"} {
			if _, ok := row[k]; !ok {
				t.Fatalf("rule row missing %q: %v", k, row)
			}
		}
		if _, hasProbe := row["probe"]; hasProbe {
			t.Fatal("snapshot must not carry executable probe fixtures")
		}
	}
}

func TestSnapshotEndpointCarriesTaskRules(t *testing.T) {
	cfg := taskIntelTestConfig("http://127.0.0.1:1", "http://127.0.0.1:2", "apply", "")
	cfg.Admin.APIKey = "task-rules-test-key"
	cfg.Admin.EmergencyAccessEnabled = true
	srv := testGateway(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "http://gateway/admin/api/snapshot", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Admin-Key", "task-rules-test-key")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", rr.Code, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	tr, ok := payload["task_rules"].(map[string]any)
	if !ok {
		t.Fatalf("snapshot missing task_rules: keys %v", payload["version"])
	}
	if tr["version"] != taskprofile.RuleVersion {
		t.Fatalf("task_rules.version=%v want %q", tr["version"], taskprofile.RuleVersion)
	}
	rules, ok := tr["rules"].([]any)
	if !ok || len(rules) != len(taskprofile.RuleTable()) {
		t.Fatalf("task_rules.rules=%v want %d rows", tr["rules"], len(taskprofile.RuleTable()))
	}
}

func TestTaskUISurfacingWiring(t *testing.T) {
	js := readAppJS(t)
	for _, want := range []string{
		"function taskIntelSentence(p)",
		"function taskRulesPanel()",
		"state.snap.task_rules",
		"'route_rules_title'",
		"task: ${esc(e.task_type || 'unknown')}",
		"e.task_intel_outcome",
		"'eval_gated'",
		"${taskRulesPanel()}",
		"${tiHtml}",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js missing task UI surfacing %q", want)
		}
	}
}
