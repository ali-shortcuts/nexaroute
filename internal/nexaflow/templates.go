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

// TemplateNode is one step: a node id + version plus its config. Config
// values may contain "{{placeholder}}" strings, resolved at Instantiate.
type TemplateNode struct {
	NodeID  string
	Version string
	Config  map[string]any
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
		cfg := make(map[string]any, len(node.Config))
		for k, v := range node.Config {
			if s, ok := v.(string); ok {
				// Substitution was previously computed into s and then
				// discarded (cfg[k] = v stored the original): templates
				// "instantiated" but no placeholder ever resolved, so a
				// runtime would have executed literal "{{...}}" text.
				for name, val := range placeholders {
					s = strings.ReplaceAll(s, "{{"+name+"}}", val)
				}
				if strings.Contains(s, "{{") && strings.Contains(s, "}}") {
					return Template{}, fmt.Errorf("nexaflow: template %q config %q leaves an unresolved placeholder", t.ID, k)
				}
				cfg[k] = s
				continue
			}
			cfg[k] = v
		}
		resolved.Nodes[i] = TemplateNode{NodeID: node.NodeID, Version: node.Version, Config: cfg}
	}
	resolved.Published = false
	return resolved, nil
}

// DefaultTemplates returns the five starter templates. All are inert
// (Published false) and all reference only nodes in DefaultRegistry.
func DefaultTemplates() []Template {
	return []Template{
		{
			ID: "model-health-alert", Version: SupportedVersion,
			Title:       "Model-health alert and approved fallback recommendation",
			Description: "Read-only: inspect provider health and compatibility, then branch on the result. It recommends a fallback; it never switches routing itself.",
			Nodes: []TemplateNode{
				{NodeID: "nexaroute.provider.health", Version: SupportedVersion, Config: map[string]any{}},
				{NodeID: "nexaroute.provider.compat", Version: SupportedVersion, Config: map[string]any{}},
				{NodeID: "nexaroute.control.branch", Version: SupportedVersion, Config: map[string]any{"true_branch": "healthy", "false_branch": "fallback"}},
			},
			Edges:        [][2]string{{"nexaroute.provider.health", "nexaroute.provider.compat"}, {"nexaroute.provider.compat", "nexaroute.control.branch"}},
			Placeholders: []string{"deployment_id"},
		},
		{
			ID: "model-compat-evaluation", Version: SupportedVersion,
			Title:       "Model compatibility evaluation",
			Description: "Read-only: compatibility inspection followed by a replay evaluation over recorded outcomes. Scores describe recorded outcomes only; no live evaluation.",
			Nodes: []TemplateNode{
				{NodeID: "nexaroute.provider.compat", Version: SupportedVersion, Config: map[string]any{}},
				{NodeID: "nexaroute.eval.run", Version: SupportedVersion, Config: map[string]any{}},
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
				{NodeID: "nexaroute.video.submit", Version: SupportedVersion, Config: map[string]any{}},
				{NodeID: "nexaroute.video.status", Version: SupportedVersion, Config: map[string]any{}},
				{NodeID: "nexaroute.video.output", Version: SupportedVersion, Config: map[string]any{}},
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
				{NodeID: "nexaroute.cost.budget_check", Version: SupportedVersion, Config: map[string]any{}},
			},
			Edges:        [][2]string{{"nexaroute.trigger.schedule", "nexaroute.cost.usage"}, {"nexaroute.cost.usage", "nexaroute.cost.budget_check"}},
			Placeholders: []string{"budget_key", "threshold_usd"},
		},
		{
			ID: "failure-notification-diagnostics", Version: SupportedVersion,
			Title:       "Failure notification and diagnostics",
			Description: "Map an error to an action, then notify over HTTP. The notification step is external and irreversible once sent, so the template requires operator policy approval; the notification URL is a placeholder only, never a real URL.",
			Nodes: []TemplateNode{
				{NodeID: "nexaroute.control.error_handler", Version: SupportedVersion, Config: map[string]any{"policy": "fallback"}},
				{NodeID: "nexaroute.http.request", Version: SupportedVersion, Config: map[string]any{}},
			},
			Edges:          [][2]string{{"nexaroute.control.error_handler", "nexaroute.http.request"}},
			Placeholders:   []string{"notify_url"},
			RequiresPolicy: true,
		},
	}
}
