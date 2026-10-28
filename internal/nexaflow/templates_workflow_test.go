package nexaflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/eval"
)

func templateByID(t *testing.T, id string) Template {
	t.Helper()
	for _, tpl := range DefaultTemplates() {
		if tpl.ID == id {
			return tpl
		}
	}
	t.Fatalf("template %q not found", id)
	return Template{}
}

func evalOutcomes(t *testing.T) []any {
	t.Helper()
	suite, ok := eval.LookupSuite("coding")
	if !ok || len(suite.Cases) == 0 {
		t.Fatal("coding suite has no cases")
	}
	outcomes := []eval.Outcome{{CaseID: suite.Cases[0].ID, Status: eval.OutcomeOK, LatencyMS: 100}}
	raw, err := json.Marshal(outcomes)
	if err != nil {
		t.Fatalf("marshal outcomes: %v", err)
	}
	var decoded []any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode outcomes: %v", err)
	}
	return decoded
}

// Every template instantiates to a workflow the engine accepts, with
// every node present in the registry and every required input filled.
func TestTemplatesInstantiateToValidWorkflows(t *testing.T) {
	eng := &Engine{Registry: DefaultRegistry(), PolicyApproved: true}
	cases := []struct {
		id           string
		placeholders map[string]string
		extra        map[string]map[string]any
	}{
		{"model-health-alert", map[string]string{"deployment_id": "prov/model"}, nil},
		{"model-compat-evaluation", map[string]string{"deployment_id": "prov/model", "suite_id": "coding"}, map[string]map[string]any{"nexaroute.eval.run": {"outcomes": evalOutcomes(t)}}},
		{"video-generation-pipeline", map[string]string{"prompt": "a cat"}, nil},
		{"scheduled-operational-report", map[string]string{"budget_key": "team", "threshold_usd": "10"}, nil},
		{"failure-notification-diagnostics", map[string]string{"notify_url": "http://127.0.0.1:9/notify"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			tpl := templateByID(t, tc.id)
			wf, err := tpl.InstantiateWorkflow(tc.placeholders, tc.extra, true)
			if err != nil {
				t.Fatalf("InstantiateWorkflow: %v", err)
			}
			if wf.ID != tc.id {
				t.Fatalf("workflow id = %q, want %q", wf.ID, tc.id)
			}
			if err := eng.Validate(wf); err != nil {
				t.Fatalf("engine validation of instantiated workflow: %v", err)
			}
			// No placeholder text may survive instantiation.
			raw, _ := json.Marshal(wf)
			if strings.Contains(string(raw), "{{") {
				t.Fatalf("instantiated workflow still contains a placeholder: %s", raw)
			}
		})
	}
}

// Missing values fail precisely: the error names the missing input.
func TestTemplatesInstantiateMissingValues(t *testing.T) {
	tpl := templateByID(t, "model-compat-evaluation")
	if _, err := tpl.InstantiateWorkflow(map[string]string{"deployment_id": "prov/model", "suite_id": "coding"}, nil, true); err == nil || !strings.Contains(err.Error(), "outcomes") {
		t.Fatalf("missing-outcomes error = %v, want an error naming outcomes", err)
	}
	tpl2 := templateByID(t, "scheduled-operational-report")
	if _, err := tpl2.InstantiateWorkflow(map[string]string{"budget_key": "team", "threshold_usd": "not-a-number"}, nil, true); err == nil || !strings.Contains(err.Error(), "threshold_usd") {
		t.Fatalf("bad-threshold error = %v, want an error naming threshold_usd", err)
	}
	tpl3 := templateByID(t, "video-generation-pipeline")
	if _, err := tpl3.InstantiateWorkflow(map[string]string{"prompt": "a cat"}, nil, false); err == nil {
		t.Fatalf("policy template instantiated without approval")
	}
}

// Instantiated templates actually run end to end on the fake backend.
func TestTemplatesExecuteEndToEnd(t *testing.T) {
	fake := newFakeBackend()
	eng := testEngine(fake, true)

	healthTpl := templateByID(t, "model-health-alert")
	healthWf, err := healthTpl.InstantiateWorkflow(map[string]string{"deployment_id": "prov/model"}, nil, true)
	if err != nil {
		t.Fatalf("health instantiate: %v", err)
	}
	run := mustRun(t, eng, healthWf, nil)
	if run.Status != RunCompleted {
		t.Fatalf("health run = %+v", run)
	}
	if run.Outputs["nexaroute.control.branch"]["selected_branch"] != "healthy" {
		t.Fatalf("health branch = %#v, want healthy", run.Outputs["nexaroute.control.branch"])
	}

	videoTpl := templateByID(t, "video-generation-pipeline")
	videoWf, err := videoTpl.InstantiateWorkflow(map[string]string{"prompt": "a cat"}, nil, true)
	if err != nil {
		t.Fatalf("video instantiate: %v", err)
	}
	run = mustRun(t, eng, videoWf, nil)
	if run.Status != RunCompleted {
		t.Fatalf("video run = %+v", run)
	}
	if run.Outputs["nexaroute.video.status"]["state"] != "completed" {
		t.Fatalf("video status = %#v, want completed", run.Outputs["nexaroute.video.status"])
	}

	reportTpl := templateByID(t, "scheduled-operational-report")
	reportWf, err := reportTpl.InstantiateWorkflow(map[string]string{"budget_key": "team", "threshold_usd": "10"}, nil, true)
	if err != nil {
		t.Fatalf("report instantiate: %v", err)
	}
	run = mustRun(t, eng, reportWf, nil)
	if run.Status != RunCompleted {
		t.Fatalf("report run = %+v", run)
	}
	if run.Outputs["nexaroute.cost.budget_check"]["over_threshold"] != false {
		t.Fatalf("budget check = %#v, want over_threshold=false (5.0 < 10)", run.Outputs["nexaroute.cost.budget_check"])
	}

	evalTpl := templateByID(t, "model-compat-evaluation")
	evalWf, err := evalTpl.InstantiateWorkflow(
		map[string]string{"deployment_id": "prov/model", "suite_id": "coding"},
		map[string]map[string]any{"nexaroute.eval.run": {"outcomes": evalOutcomes(t)}}, true)
	if err != nil {
		t.Fatalf("eval instantiate: %v", err)
	}
	run = mustRun(t, eng, evalWf, nil)
	if run.Status != RunCompleted {
		t.Fatalf("eval run = %+v", run)
	}
	if run.Outputs["nexaroute.eval.run"]["mode"] != "replay" {
		t.Fatalf("eval mode = %#v, want replay (live evaluation is unsupported)", run.Outputs["nexaroute.eval.run"])
	}

	notifyTpl := templateByID(t, "failure-notification-diagnostics")
	notifyWf, err := notifyTpl.InstantiateWorkflow(map[string]string{"notify_url": "http://127.0.0.1:9/notify"}, nil, true)
	if err != nil {
		t.Fatalf("notify instantiate: %v", err)
	}
	run = mustRun(t, eng, notifyWf, map[string]any{"error_code": "backend_error", "message": "boom"})
	if run.Status != RunCompleted {
		t.Fatalf("notify run = %+v", run)
	}
	if run.Outputs["nexaroute.control.error_handler"]["action"] != "fallback" {
		t.Fatalf("error action = %#v, want fallback", run.Outputs["nexaroute.control.error_handler"])
	}
}
