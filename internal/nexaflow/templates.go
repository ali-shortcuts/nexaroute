package nexaflow

import (
	"fmt"
	"strings"
)

// Template is a starter workflow definition: an ordered list of node
// references with edges and placeholders. It is INERT by construction:
// it has no Run method, no execution engine, and no side effects.
// Instantiate only validates that every placeholder is filled and, for
// templates that require operator policy, that policy approval was given.
// It returns a resolved definition for a runtime to execute; it does not
// execute anything itself. Published is always false for templates
// produced here: publishing is an explicit operator act in the runtime,
// never a side effect of defining a template.
//
// Template configs contain NO real secrets and NO real notification URLs:
// notification endpoints appear only as placeholders (for example
// "{{notify_url}}") that an operator must fill deliberately.
type Template struct {
	ID             string
	Version        string
	Title          string
	Description    string
	Nodes          []TemplateNode
	Edges          [][2]string
	Placeholders   []string
	RequiresPolicy bool
	Published      bool
}

// TemplateNode is one step: a node id + version plus its config and
// its input template. Config and Input values may contain
// "{{placeholder}}" strings, resolved at Instantiate. Bindings map an
// input field to a source reference in engine syntax ("stepID.field",
// "$input.field"); they are carried into the workflow unchanged by
// Instantiate and resolved by the engine at run time. A template node
// with neither Config nor Input nor Bindings still needs its required
// inputs supplied at instantiation time (see InstantiateWorkflow).
type TemplateNode struct {
	NodeID   string
	Version  string
	Config   map[string]any
	Input    map[string]any
	Bindings map[string]string
}

// Instantiate resolves placeholders and enforces the inertness contract:
// every declared placeholder must be supplied, every referenced node
// must exist in the registry, and a policy-requiring template requires
// policyApproved. On any failure it returns an error and no definition.
// It never executes a node.
//
// Substitution replaces every "{{name}}" occurrence in a config string,
// not only whole-string matches: the previous exact-match behaviour left
// embedded placeholders (for example a URL containing "{{notify_url}}")
// unresolved, and the runtime would then act on the literal placeholder
// text. Any placeholder pattern left after substitution is an error,
// never a value to execute against.
func (t Template) Instantiate(placeholders map[string]string, policyApproved bool) (Template, error) {
	if t.RequiresPolicy && !policyApproved {
		return Template{}, fmt.Errorf("nexaflow: template %q requires explicit operator policy approval", t.ID)
	}
	for _, name := range t.Placeholders {
		if _, ok := placeholders[name]; !ok {
			return Template{}, fmt.Errorf("nexaflow: template %q placeholder %q is not filled", t.ID, name)
		}
	}
	reg := DefaultRegistry()
	resolved := t
	resolved.Nodes = make([]TemplateNode, len(t.Nodes))
	for i, node := range t.Nodes {
		if _, err := reg.Lookup(node.NodeID, node.Version); err != nil {
			return Template{}, fmt.Errorf("nexaflow: template %q references unknown node %q: %w", t.ID, node.NodeID, err)
		}
		// Merged Phase 5 + Phase 7 semantics: Phase 7's embedded
		// substitution (a whole-string-only match left embedded
		// placeholders, e.g. in URLs, unresolved) applied to both
		// Config and Input, with Phase 5's Input/Bindings preserved.
		cfg, err := resolveTemplateStringsChecked(node.Config, placeholders, t.ID)
		if err != nil {
			return Template{}, err
		}
		input, err := resolveTemplateStringsChecked(node.Input, placeholders, t.ID)
		if err != nil {
			return Template{}, err
		}
		resolved.Nodes[i] = TemplateNode{NodeID: node.NodeID, Version: node.Version, Config: cfg, Input: input, Bindings: node.Bindings}
	}
	resolved.Published = false
	return resolved, nil
}

func resolveTemplateStrings(values map[string]any, placeholders map[string]string) map[string]any {
	out, _ := resolveTemplateStringsChecked(values, placeholders, "")
	return out
}

// resolveTemplateStringsChecked substitutes every "{{name}}" occurrence
// (not only whole-string matches) and fails on any leftover pattern,
// so a runtime never executes literal placeholder text.
func resolveTemplateStringsChecked(values map[string]any, placeholders map[string]string, templateID string) (map[string]any, error) {
	out := make(map[string]any, len(values))
	for k, v := range values {
		if s, ok := v.(string); ok {
			for name, val := range placeholders {
				s = strings.ReplaceAll(s, "{{"+name+"}}", val)
			}
			if strings.Contains(s, "{{") && strings.Contains(s, "}}") {
				return nil, fmt.Errorf("nexaflow: template %q leaves an unresolved placeholder in %q", templateID, k)
			}
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out, nil
}

// DefaultTemplates returns the five starter templates. All are inert
// (Published false) and all reference only nodes in DefaultRegistry.
func DefaultTemplates() []Template {
	return []Template{
		{
			ID: "model-health-alert", Version: SupportedVersion,
			Title:       "Model-health alert and approved fallback recommendation",
			Description: "Read-only: inspect provider health and compatibility, compare health status to healthy, then branch on the result. It recommends a fallback; it never switches routing itself.",
			Nodes: []TemplateNode{
				{NodeID: "nexaroute.provider.health", Version: SupportedVersion, Input: map[string]any{"deployment_id": "{{deployment_id}}"}},
				{NodeID: "nexaroute.provider.compat", Version: SupportedVersion, Input: map[string]any{"deployment_id": "{{deployment_id}}"}},
				{NodeID: "nexaroute.control.condition", Version: SupportedVersion, Config: map[string]any{"field": "status", "operator": "eq", "expected": "healthy"}, Bindings: map[string]string{"value": "nexaroute.provider.health"}},
				{NodeID: "nexaroute.control.branch", Version: SupportedVersion, Config: map[string]any{"true_branch": "healthy", "false_branch": "fallback"}, Bindings: map[string]string{"condition_result": "nexaroute.control.condition.result"}},
			},
			Edges:        [][2]string{{"nexaroute.provider.health", "nexaroute.control.condition"}, {"nexaroute.control.condition", "nexaroute.control.branch"}, {"nexaroute.provider.compat", "nexaroute.control.branch"}},
			Placeholders: []string{"deployment_id"},
		},
		{
			ID: "model-compat-evaluation", Version: SupportedVersion,
			Title:       "Model compatibility evaluation",
			Description: "Read-only: compatibility inspection followed by a replay evaluation over recorded outcomes. Scores describe recorded outcomes only; no live evaluation. Outcomes are supplied at instantiation time via extraInputs, never invented.",
			Nodes: []TemplateNode{
				{NodeID: "nexaroute.provider.compat", Version: SupportedVersion, Input: map[string]any{"deployment_id": "{{deployment_id}}"}},
				{NodeID: "nexaroute.eval.run", Version: SupportedVersion, Input: map[string]any{"suite_id": "{{suite_id}}", "deployment_id": "{{deployment_id}}"}},
			},
			Edges:          [][2]string{{"nexaroute.provider.compat", "nexaroute.eval.run"}},
			Placeholders:   []string{"deployment_id", "suite_id"},
			RequiresPolicy: false,
		},
		{
			ID: "video-generation-pipeline", Version: SupportedVersion,
			Title:       "Video-generation pipeline",
			Description: "Submit a video job, poll its status, and fetch its output. Billable; requires operator policy approval before instantiation.",
			Nodes: []TemplateNode{
				{NodeID: "nexaroute.video.submit", Version: SupportedVersion, Input: map[string]any{"prompt": "{{prompt}}", "duration_seconds": 5.0, "mode": "text_to_video", "aspect_ratio": "16:9"}},
				{NodeID: "nexaroute.video.status", Version: SupportedVersion, Bindings: map[string]string{"job_id": "nexaroute.video.submit.job_id"}},
				{NodeID: "nexaroute.video.output", Version: SupportedVersion, Bindings: map[string]string{"job_id": "nexaroute.video.submit.job_id"}},
			},
			Edges:          [][2]string{{"nexaroute.video.submit", "nexaroute.video.status"}, {"nexaroute.video.status", "nexaroute.video.output"}},
			Placeholders:   []string{"prompt"},
			RequiresPolicy: true,
		},
		{
			ID: "scheduled-operational-report", Version: SupportedVersion,
			Title:       "Scheduled operational report",
			Description: "Read-only: compute the next scheduled fire time, then inspect usage and check the budget threshold. Reporting only; the budget check does not enforce anything.",
			Nodes: []TemplateNode{
				{NodeID: "nexaroute.trigger.schedule", Version: SupportedVersion, Config: map[string]any{"daily": "09:00"}},
				{NodeID: "nexaroute.cost.usage", Version: SupportedVersion, Config: map[string]any{}},
				{NodeID: "nexaroute.cost.budget_check", Version: SupportedVersion, Input: map[string]any{"key": "{{budget_key}}", "threshold_usd": "{{threshold_usd}}"}},
			},
			Edges:        [][2]string{{"nexaroute.trigger.schedule", "nexaroute.cost.usage"}, {"nexaroute.cost.usage", "nexaroute.cost.budget_check"}},
			Placeholders: []string{"budget_key", "threshold_usd"},
		},
		{
			ID: "failure-notification-diagnostics", Version: SupportedVersion,
			Title:       "Failure notification and diagnostics",
			Description: "Map an error to an action, then notify over HTTP. The notification step is external and irreversible once sent, so the template requires operator policy approval; the notification URL is a placeholder only, never a real URL. The error itself comes from the workflow input at run time.",
			Nodes: []TemplateNode{
				{NodeID: "nexaroute.control.error_handler", Version: SupportedVersion, Config: map[string]any{"policy": "fallback"}, Bindings: map[string]string{"error_code": "$input.error_code", "message": "$input.message"}},
				{NodeID: "nexaroute.http.request", Version: SupportedVersion, Input: map[string]any{"url": "{{notify_url}}", "method": "POST"}},
			},
			Edges:          [][2]string{{"nexaroute.control.error_handler", "nexaroute.http.request"}},
			Placeholders:   []string{"notify_url"},
			RequiresPolicy: true,
		},
	}
}
