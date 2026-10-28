package nexaflow

import (
	"fmt"
	"strconv"
	"strings"
)

// InstantiateWorkflow turns an inert template into a validated,
// runnable Workflow — or fails with a precise error naming exactly
// what is still missing. It never returns a runnable-looking workflow
// with holes.
//
//   - placeholders fills every declared "{{name}}" (via Instantiate,
//     which also enforces operator policy approval).
//   - extraInputs supplies concrete input values keyed by step id
//     (which is the node id: each template node appears once, and a
//     duplicate node id is rejected). This is how non-string inputs
//     are supplied — for example, the eval template's outcomes array.
//   - string placeholders that land on number or boolean schema
//     fields are coerced ("10" -> 10, "true" -> true); a value that
//     cannot be coerced is an error naming the step and field.
//   - after assembly, the workflow is validated against
//     DefaultRegistry with the same policy approval the caller
//     supplied (a policy-requiring template already requires
//     approval in Instantiate, so its workflows validate cleanly;
//     a template that smuggles in a policy-requiring node without
//     approval fails here, precisely, instead of at run time).
//
// The returned Workflow stays unpublished in spirit: publishing is an
// explicit operator act in the runtime, and nothing here publishes
// anything. The engine additionally refuses any workflow that still
// contains a "{{...}}" string, so a raw template can never execute
// by accident even if InstantiateWorkflow is bypassed.
func (t Template) InstantiateWorkflow(placeholders map[string]string, extraInputs map[string]map[string]any, policyApproved bool) (*Workflow, error) {
	inst, err := t.Instantiate(placeholders, policyApproved)
	if err != nil {
		return nil, err
	}
	reg := DefaultRegistry()
	seen := make(map[string]bool, len(inst.Nodes))
	wf := &Workflow{ID: t.ID, Version: t.Version}
	for _, node := range inst.Nodes {
		if seen[node.NodeID] {
			return nil, fmt.Errorf("nexaflow: template %q has duplicate node %q; step ids must be unique", t.ID, node.NodeID)
		}
		seen[node.NodeID] = true
		spec, err := reg.Lookup(node.NodeID, node.Version)
		if err != nil {
			return nil, fmt.Errorf("nexaflow: template %q references unknown node %q: %w", t.ID, node.NodeID, err)
		}
		input := make(map[string]any, len(node.Input))
		for k, v := range node.Input {
			input[k] = v
		}
		if extra, ok := extraInputs[node.NodeID]; ok {
			for k, v := range extra {
				input[k] = v
			}
		}
		if err := coerceTypedFields(input, spec.InputSchema, t.ID, node.NodeID); err != nil {
			return nil, err
		}
		config := make(map[string]any, len(node.Config))
		for k, v := range node.Config {
			config[k] = v
		}
		if err := coerceTypedFields(config, spec.ConfigSchema, t.ID, node.NodeID); err != nil {
			return nil, err
		}
		for _, f := range spec.InputSchema {
			if !f.Required {
				continue
			}
			if _, ok := input[f.Name]; !ok {
				if _, ok := node.Bindings[f.Name]; !ok {
					return nil, fmt.Errorf("nexaflow: template %q step %q is missing required input %q (supply it via extraInputs for step %q)", t.ID, node.NodeID, f.Name, node.NodeID)
				}
			}
		}
		for k, v := range input {
			if s, ok := v.(string); ok && containsPlaceholder(s) {
				return nil, fmt.Errorf("nexaflow: template %q step %q input %q still contains an unresolved placeholder %q", t.ID, node.NodeID, k, s)
			}
		}
		wf.Steps = append(wf.Steps, Step{
			ID: node.NodeID, NodeID: node.NodeID, Version: node.Version,
			Config: config, Input: input, Bindings: node.Bindings,
		})
	}
	wf.Edges = append(wf.Edges, inst.Edges...)
	eng := &Engine{Registry: reg, PolicyApproved: policyApproved}
	if err := eng.Validate(wf); err != nil {
		return nil, fmt.Errorf("nexaflow: template %q produced an invalid workflow: %w", t.ID, err)
	}
	return wf, nil
}

// coerceTypedFields converts placeholder-resolved strings to the type
// the schema declares, so a threshold supplied as "10" becomes the
// number 10 the budget_check node requires. Non-string values and
// already-correct types pass through untouched.
func coerceTypedFields(values map[string]any, schema []Field, templateID, stepID string) error {
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
				return fmt.Errorf("nexaflow: template %q step %q field %q value %q is not a number", templateID, stepID, k, s)
			}
			values[k] = f
		case "boolean":
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "true":
				values[k] = true
			case "false":
				values[k] = false
			default:
				return fmt.Errorf("nexaflow: template %q step %q field %q value %q is not a boolean", templateID, stepID, k, s)
			}
		}
	}
	return nil
}
