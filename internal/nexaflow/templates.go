package nexaflow

import (
	"fmt"
	"strconv"
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
// its input mapping. Config values and Input values may contain
// "{{placeholder}}" strings, resolved at Instantiate. Input follows
// the DefinitionNode.Input grammar (literals, or references of the
// form {"$ref": "path"} / "$.path" into the run context), with
// instance ids being the short ids InstantiateDefinition assigns
// (last registry-id segment); templates are single-instance per node,
// so that mapping is injective within a template. A template node
// with neither Config nor Input still needs its required inputs
// supplied at instantiation time (see InstantiateDefinition).
type TemplateNode struct {
	NodeID  string
	Version string
	Config  map[string]any
	Input   map[string]any
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
		// Substitution was previously computed into s and then
		// discarded (cfg[k] = v stored the original): templates
		// "instantiated" but no placeholder ever resolved, so a
		// runtime would have executed literal "{{...}}" text. That
		// bug is fixed here for Config and Input alike, including
		// embedded placeholders (for example a URL containing
		// "{{notify_url}}") and nested maps/slices.
		cfg, err := substituteTemplateStrings(node.Config, placeholders, t.ID, node.NodeID, "config")
		if err != nil {
			return Template{}, err
		}
		input, err := substituteTemplateStrings(node.Input, placeholders, t.ID, node.NodeID, "input")
		if err != nil {
			return Template{}, err
		}
		resolved.Nodes[i] = TemplateNode{NodeID: node.NodeID, Version: node.Version, Config: cfg, Input: input}
	}
	resolved.Published = false
	return resolved, nil
}

// substituteTemplateStrings resolves every "{{name}}" occurrence in
// every string inside values (including nested maps and slices) and
// returns fresh maps/slices, so instantiation never mutates the
// template author's values. Any "{{...}}" pattern left after
// substitution is an error, never a value to execute against.
func substituteTemplateStrings(values map[string]any, placeholders map[string]string, templateID, nodeID, where string) (map[string]any, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(values))
	for k, v := range values {
		sub, err := substituteTemplateValue(v, placeholders, templateID, nodeID, where, k)
		if err != nil {
			return nil, err
		}
		out[k] = sub
	}
	return out, nil
}

func substituteTemplateValue(v any, placeholders map[string]string, templateID, nodeID, where, key string) (any, error) {
	switch t := v.(type) {
	case string:
		s := t
		for name, val := range placeholders {
			s = strings.ReplaceAll(s, "{{"+name+"}}", val)
		}
		if strings.Contains(s, "{{") && strings.Contains(s, "}}") {
			return nil, fmt.Errorf("nexaflow: template %q node %q %s %q leaves an unresolved placeholder", templateID, nodeID, where, key)
		}
		return s, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			sub, err := substituteTemplateValue(val, placeholders, templateID, nodeID, where, key)
			if err != nil {
				return nil, err
			}
			out[k] = sub
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			sub, err := substituteTemplateValue(val, placeholders, templateID, nodeID, where, key)
			if err != nil {
				return nil, err
			}
			out[i] = sub
		}
		return out, nil
	default:
		return v, nil
	}
}

// InstantiateDefinition turns an inert template into a validated
// Definition — or fails with a precise error naming exactly what is
// still missing. It never returns a runnable-looking definition with
// holes. This is the template-instantiation path ported from PR #688
// (its InstantiateWorkflow built a Workflow for the retired in-memory
// engine); here it builds a Definition for the durable engine, in
// this branch's own grammar:
//
//   - placeholders fill every declared "{{name}}" (via Instantiate,
//     which also enforces operator policy approval);
//   - instance ids are the short ids (last registry-id segment);
//     templates are single-instance per node, so the mapping is
//     injective, and a collision is a template authoring error;
//   - Input mappings use short-id references ("health.status",
//     "input.outcomes"), which validateInputMappings accepts; a
//     reference to an unknown instance fails here, not at run time;
//   - string values that land on number or boolean schema fields are
//     coerced ("10" -> 10, "true" -> true); a value that cannot be
//     coerced is an error naming the template, step, and field;
//   - every required input field of every non-trigger node must be
//     covered by its Input mapping or by an incoming edge (whose
//     predecessor merge may supply it); anything else is an error
//     naming the missing field, so a template can never instantiate
//     into a definition that fails its first node on input
//     validation. Trigger nodes are exempt: they receive the run
//     input (manual) or the delivery (webhook) verbatim, not a
//     mapping, and their inputs come from the trigger mechanism;
//   - after assembly, the definition is validated with
//     ValidateDefinition (triggers present and bijective, edges
//     referential and acyclic, every non-trigger node reachable).
//     A policy-requiring template already requires approval in
//     Instantiate, so its definitions validate cleanly; a template
//     that smuggles in a policy-requiring node without approval
//     fails in Instantiate, precisely, instead of at run time.
//
// The returned Definition stays unpublished in spirit: publishing is
// an explicit operator act in the runtime, and nothing here publishes
// anything. The engine additionally refuses any definition whose
// input mapping cannot resolve, so a raw template can never execute
// by accident even if InstantiateDefinition is bypassed.
func (t Template) InstantiateDefinition(placeholders map[string]string, policyApproved bool) (*Definition, error) {
	inst, err := t.Instantiate(placeholders, policyApproved)
	if err != nil {
		return nil, err
	}
	reg := DefaultRegistry()
	shortID := make(map[string]string, len(inst.Nodes))
	seen := make(map[string]bool, len(inst.Nodes))
	for _, node := range inst.Nodes {
		if node.NodeID == "" {
			return nil, fmt.Errorf("nexaflow: template %q has a node with no node id", t.ID)
		}
		if seen[node.NodeID] {
			return nil, fmt.Errorf("nexaflow: template %q has duplicate node %q (templates are single-instance per node)", t.ID, node.NodeID)
		}
		seen[node.NodeID] = true
		short := node.NodeID
		if idx := strings.LastIndex(short, "."); idx >= 0 {
			short = short[idx+1:]
		}
		if short == "" {
			return nil, fmt.Errorf("nexaflow: template %q node %q has an empty instance id", t.ID, node.NodeID)
		}
		for other, s := range shortID {
			if s == short {
				return nil, fmt.Errorf("nexaflow: template %q nodes %q and %q collide on instance id %q", t.ID, other, node.NodeID, short)
			}
		}
		shortID[node.NodeID] = short
	}
	def := &Definition{
		SchemaVersion: DefinitionSchemaVersion,
		ID:            t.ID,
		Name:          t.Title,
		Description:   t.Description,
	}
	hasIncoming := make(map[string]bool, len(inst.Nodes))
	for i, edge := range inst.Edges {
		from, ok := shortID[edge[0]]
		if !ok {
			return nil, fmt.Errorf("nexaflow: template %q edge %d references node %q outside the template", t.ID, i, edge[0])
		}
		to, ok := shortID[edge[1]]
		if !ok {
			return nil, fmt.Errorf("nexaflow: template %q edge %d references node %q outside the template", t.ID, i, edge[1])
		}
		hasIncoming[to] = true
		def.Edges = append(def.Edges, DefinitionEdge{ID: fmt.Sprintf("e%d", i+1), From: from, To: to})
	}
	for _, node := range inst.Nodes {
		spec, err := reg.Lookup(node.NodeID, node.Version)
		if err != nil {
			return nil, fmt.Errorf("nexaflow: template %q node %q is unknown: %w", t.ID, node.NodeID, err)
		}
		if spec.ID == approvalNodeID && node.Input != nil {
			return nil, fmt.Errorf("nexaflow: template %q node %q is an approval node and must not declare an input mapping (its decision comes only from the approval record)", t.ID, node.NodeID)
		}
		if err := coerceTemplateFields(node.Config, spec.ConfigSchema, t.ID, node.NodeID); err != nil {
			return nil, err
		}
		if err := coerceTemplateFields(node.Input, spec.InputSchema, t.ID, node.NodeID); err != nil {
			return nil, err
		}
		if spec.Category != "trigger" {
			for _, f := range spec.InputSchema {
				if !f.Required {
					continue
				}
				if node.Input != nil {
					if _, ok := node.Input[f.Name]; ok {
						continue
					}
				}
				if hasIncoming[shortID[node.NodeID]] {
					continue
				}
				return nil, fmt.Errorf("nexaflow: template %q node %q is missing required input %q (supply it via Input for node %q, or give the node an incoming edge)", t.ID, node.NodeID, f.Name, node.NodeID)
			}
		}
		dn := DefinitionNode{ID: shortID[node.NodeID], NodeID: node.NodeID, Version: node.Version, Config: node.Config, Input: node.Input}
		def.Nodes = append(def.Nodes, dn)
		if spec.Category == "trigger" {
			kind, ok := triggerKindForNode(node.NodeID)
			if !ok {
				return nil, fmt.Errorf("nexaflow: template %q trigger node %q has no trigger kind", t.ID, node.NodeID)
			}
			trigCfg := make(map[string]any, len(node.Config))
			for k, v := range node.Config {
				trigCfg[k] = v
			}
			def.Triggers = append(def.Triggers, DefinitionTrigger{Name: shortID[node.NodeID], Kind: kind, Config: trigCfg})
		}
	}
	if err := ValidateDefinition(def, reg); err != nil {
		return nil, fmt.Errorf("nexaflow: template %q produced an invalid definition: %w", t.ID, err)
	}
	return def, nil
}

func triggerKindForNode(nodeID string) (string, bool) {
	for kind, id := range triggerNodeID {
		if id == nodeID {
			return kind, true
		}
	}
	return "", false
}

// coerceTemplateFields converts placeholder-resolved strings to the
// type the schema declares, so a threshold supplied as "10" becomes
// the number 10 the budget_check node requires. Non-string values and
// already-correct types pass through untouched. Ported from PR #688's
// coerceTypedFields, adapted to Definition grammar.
func coerceTemplateFields(values map[string]any, schema []Field, templateID, nodeID string) error {
	if len(values) == 0 {
		return nil
	}
	types := make(map[string]string, len(schema))
	for _, f := range schema {
		types[f.Name] = f.Type
	}
	for k, v := range values {
		s, ok := v.(string)
		if !ok {
			continue
		}
		switch types[k] {
		case "number":
			f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
			if err != nil {
				return fmt.Errorf("nexaflow: template %q node %q field %q value %q is not a number", templateID, nodeID, k, s)
			}
			values[k] = f
		case "boolean":
			switch strings.TrimSpace(s) {
			case "true":
				values[k] = true
			case "false":
				values[k] = false
			default:
				return fmt.Errorf("nexaflow: template %q node %q field %q value %q is not a boolean", templateID, nodeID, k, s)
			}
		}
	}
	return nil
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
				{NodeID: "nexaroute.trigger.manual", Version: SupportedVersion, Config: map[string]any{}},
				{NodeID: "nexaroute.provider.health", Version: SupportedVersion, Input: map[string]any{"deployment_id": "{{deployment_id}}"}},
				{NodeID: "nexaroute.control.condition", Version: SupportedVersion, Config: map[string]any{"field": "status", "operator": "eq", "expected": "healthy"}, Input: map[string]any{"value": map[string]any{"$ref": "health"}}},
				{NodeID: "nexaroute.provider.compat", Version: SupportedVersion, Input: map[string]any{"deployment_id": map[string]any{"$ref": "health.deployment_id"}}},
				{NodeID: "nexaroute.control.branch", Version: SupportedVersion, Config: map[string]any{"true_branch": "healthy", "false_branch": "fallback"}, Input: map[string]any{"condition_result": map[string]any{"$ref": "condition.result"}}},
			},
			Edges:        [][2]string{{"nexaroute.trigger.manual", "nexaroute.provider.health"}, {"nexaroute.provider.health", "nexaroute.control.condition"}, {"nexaroute.provider.health", "nexaroute.provider.compat"}, {"nexaroute.control.condition", "nexaroute.control.branch"}},
			Placeholders: []string{"deployment_id"},
		},
		{
			ID: "model-compat-evaluation", Version: SupportedVersion,
			Title:       "Model compatibility evaluation",
			Description: "Read-only: compatibility inspection followed by a replay evaluation over recorded outcomes. Scores describe recorded outcomes only; no live evaluation.",
			Nodes: []TemplateNode{
				{NodeID: "nexaroute.trigger.manual", Version: SupportedVersion, Config: map[string]any{}},
				{NodeID: "nexaroute.provider.compat", Version: SupportedVersion, Input: map[string]any{"deployment_id": "{{deployment_id}}"}},
				{NodeID: "nexaroute.eval.run", Version: SupportedVersion, Input: map[string]any{"suite_id": "{{suite_id}}", "deployment_id": map[string]any{"$ref": "compat.deployment_id"}, "outcomes": map[string]any{"$ref": "input.outcomes"}}},
			},
			Edges:          [][2]string{{"nexaroute.trigger.manual", "nexaroute.provider.compat"}, {"nexaroute.provider.compat", "nexaroute.eval.run"}},
			Placeholders:   []string{"deployment_id", "suite_id"},
			RequiresPolicy: false,
		},
		{
			ID: "video-generation-pipeline", Version: SupportedVersion,
			Title:       "Video-generation pipeline",
			Description: "Submit a video job, poll its status, and fetch its output. Billable; requires operator policy approval before instantiation.",
			Nodes: []TemplateNode{
				{NodeID: "nexaroute.trigger.manual", Version: SupportedVersion, Config: map[string]any{}},
				{NodeID: "nexaroute.video.submit", Version: SupportedVersion, Input: map[string]any{"prompt": "{{prompt}}", "mode": "text_to_video", "duration_seconds": 5.0, "aspect_ratio": "16:9"}},
				{NodeID: "nexaroute.video.status", Version: SupportedVersion, Input: map[string]any{"job_id": map[string]any{"$ref": "submit.job_id"}}},
				{NodeID: "nexaroute.video.output", Version: SupportedVersion, Input: map[string]any{"job_id": map[string]any{"$ref": "status.job_id"}}},
			},
			Edges:          [][2]string{{"nexaroute.trigger.manual", "nexaroute.video.submit"}, {"nexaroute.video.submit", "nexaroute.video.status"}, {"nexaroute.video.status", "nexaroute.video.output"}},
			Placeholders:   []string{"prompt"},
			RequiresPolicy: true,
		},
		{
			ID: "scheduled-operational-report", Version: SupportedVersion,
			Title:       "Scheduled operational report",
			Description: "Read-only: compute the next scheduled fire time, then inspect usage and check the budget threshold. Reporting only; the budget check does not enforce anything.",
			Nodes: []TemplateNode{
				{NodeID: "nexaroute.trigger.schedule", Version: SupportedVersion, Config: map[string]any{"daily": "09:00"}},
				{NodeID: "nexaroute.cost.usage", Version: SupportedVersion},
				{NodeID: "nexaroute.cost.budget_check", Version: SupportedVersion, Input: map[string]any{"key": "{{budget_key}}", "threshold_usd": "{{threshold_usd}}"}},
			},
			Edges:        [][2]string{{"nexaroute.trigger.schedule", "nexaroute.cost.usage"}, {"nexaroute.cost.usage", "nexaroute.cost.budget_check"}},
			Placeholders: []string{"budget_key", "threshold_usd"},
		},
		{
			ID: "failure-notification-diagnostics", Version: SupportedVersion,
			Title:       "Failure notification and diagnostics",
			Description: "Map an error to an action, then notify over HTTP. The notification step is external and irreversible once sent, so the template requires operator policy approval; the notification URL is a placeholder only, never a real URL.",
			Nodes: []TemplateNode{
				{NodeID: "nexaroute.trigger.manual", Version: SupportedVersion, Config: map[string]any{}},
				{NodeID: "nexaroute.control.error_handler", Version: SupportedVersion, Config: map[string]any{"policy": "fallback"}, Input: map[string]any{"error_code": map[string]any{"$ref": "input.error_code"}}},
				{NodeID: "nexaroute.http.request", Version: SupportedVersion, Input: map[string]any{"url": "{{notify_url}}", "method": "POST", "body": map[string]any{"$ref": "error_handler.message"}}},
			},
			Edges:          [][2]string{{"nexaroute.trigger.manual", "nexaroute.control.error_handler"}, {"nexaroute.control.error_handler", "nexaroute.http.request"}},
			Placeholders:   []string{"notify_url"},
			RequiresPolicy: true,
		},
	}
}
