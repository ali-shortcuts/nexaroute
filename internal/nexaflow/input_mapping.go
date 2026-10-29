package nexaflow

import (
	"fmt"
	"strings"
)

// Input mappings (Task 8 canonical fix for the two cross-engine findings).
//
// Problem, proven first-hand on the #689 engine: nodeInput merged direct
// predecessors' outputs flat, last-wins, with no projection onto the
// node's InputSchema — and Registry.Execute rejects unknown input fields
// fail-closed. A condition node outputs {"result", "field"} while the
// branch node requires {"condition_result"}: the unmapped chain fails
// validation on every run, so branch composition was unreachable, and
// any schema-inexact chain failed the same way. Nothing executed
// wrongly; it failed closed — but it could not succeed either.
//
// Fix, adopting the #691 DefinitionNode.Input contract (same JSON shape,
// so #691 drafts migrate losslessly): a node may declare Input, a JSON
// object whose values are literals or references into the run context:
//   - {"$ref": "path"} or "$.path" strings resolve a dotted path
//     through {"input": <run input>, <node instance id>: <output>}
//     (maps and array indices; a missing path fails the node with
//     code "input_resolution", never a silent zero value);
//   - every other value is a literal (maps and arrays resolve
//     recursively).
// When Input is present it REPLACES the predecessor merge for that node,
// so the node's input is exactly the mapped keys: that is the
// projection. Unmapped nodes keep the legacy merge (documented
// limitation: unmapped chains must be schema-exact or validation fails
// them closed). Trigger-category nodes may not declare Input: triggers
// receive the run input verbatim by contract.
// ForEach, OnError and PolicyApproved from #691 are NOT adopted here;
// definitions using them are rejected loudly by migration, never
// silently dropped (see task8_migrate.go).
//
// Approval nodes are special (Task 8 fix F-2): their entire input
// contract is the decision triple, and that triple must come from the
// durable approval record (DecideApproval), never from predecessor
// outputs or from a mapping. An unmapped approval node therefore
// starts from an empty input instead of the predecessor merge (a merge
// could only ever fail validation, or worse, smuggle a decision past
// the approval record through a transform literal). A mapping on an
// approval node is rejected at validation time for the same reason.

// resolveMappingValue resolves one mapping value against ctx.
func resolveMappingValue(v any, ctx map[string]any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 1 {
			if ref, ok := t["$ref"].(string); ok {
				return resolveMappingPath(ctx, ref)
			}
		}
		out := make(map[string]any, len(t))
		for k, val := range t {
			resolved, err := resolveMappingValue(val, ctx)
			if err != nil {
				return nil, err
			}
			out[k] = resolved
		}
		return out, nil
	case []any:
		out := make([]any, 0, len(t))
		for _, val := range t {
			resolved, err := resolveMappingValue(val, ctx)
			if err != nil {
				return nil, err
			}
			out = append(out, resolved)
		}
		return out, nil
	case string:
		if strings.HasPrefix(t, "$.") {
			return resolveMappingPath(ctx, strings.TrimPrefix(t, "$."))
		}
		return t, nil
	default:
		return t, nil
	}
}

// resolveMappingPath walks a dotted path through maps and arrays.
func resolveMappingPath(ctx map[string]any, path string) (any, error) {
	if path == "" {
		return nil, fmt.Errorf("nexaflow: empty input-mapping path")
	}
	parts := strings.Split(path, ".")
	cur := any(ctx)
	for _, part := range parts {
		switch c := cur.(type) {
		case map[string]any:
			v, ok := c[part]
			if !ok {
				return nil, fmt.Errorf("nexaflow: input-mapping path %q not found", path)
			}
			cur = v
		case []any:
			var idx int
			if _, err := fmt.Sscanf(part, "%d", &idx); err != nil || idx < 0 || idx >= len(c) {
				return nil, fmt.Errorf("nexaflow: input-mapping path %q not found", path)
			}
			cur = c[idx]
		default:
			return nil, fmt.Errorf("nexaflow: input-mapping path %q not found", path)
		}
	}
	return cur, nil
}

// collectMappingRefs appends every reference path in a mapping value.
func collectMappingRefs(v any, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 1 {
			if ref, ok := t["$ref"].(string); ok {
				*out = append(*out, ref)
				return
			}
		}
		for _, val := range t {
			collectMappingRefs(val, out)
		}
	case []any:
		for _, val := range t {
			collectMappingRefs(val, out)
		}
	case string:
		if strings.HasPrefix(t, "$.") {
			*out = append(*out, strings.TrimPrefix(t, "$."))
		}
	}
}

// approvalNodeID is the only node whose input is a decision record.
const approvalNodeID = "nexaroute.control.approval"

// validateInputMappings checks every declared Input mapping at
// authoring/validation time: the mapping target keys must be declared
// InputSchema fields of the node's spec, trigger nodes must not declare
// a mapping, approval nodes must not declare a mapping (their decision
// comes only from the approval record), and every reference root must
// be "input" or a node instance id in this definition. Deeper path
// segments are runtime checks (missing path => input_resolution,
// fail-closed).
func validateInputMappings(def *Definition, specs map[string]*NodeSpec, instanceIsTrigger map[string]bool) error {
	for i := range def.Nodes {
		n := &def.Nodes[i]
		if n.Input == nil {
			continue
		}
		if instanceIsTrigger[n.ID] {
			return fmt.Errorf("nexaflow: definition %q: trigger node instance %q must not declare an input mapping (triggers receive run input verbatim)", def.ID, n.ID)
		}
		if specs[n.ID] != nil && specs[n.ID].ID == approvalNodeID {
			return fmt.Errorf("nexaflow: definition %q: approval node instance %q must not declare an input mapping (its decision comes only from the approval record)", def.ID, n.ID)
		}
		spec := specs[n.ID]
		known := make(map[string]bool, len(spec.InputSchema))
		for _, f := range spec.InputSchema {
			known[f.Name] = true
		}
		for key := range n.Input {
			if !known[key] {
				return fmt.Errorf("nexaflow: definition %q: node instance %q: input mapping key %q is not a declared input field of node %q", def.ID, n.ID, key, n.NodeID)
			}
		}
		var refs []string
		collectMappingRefs(n.Input, &refs)
		for _, ref := range refs {
			root := ref
			if idx := strings.Index(ref, "."); idx >= 0 {
				root = ref[:idx]
			}
			if root != "input" {
				if _, ok := specs[root]; !ok {
					return fmt.Errorf("nexaflow: definition %q: node instance %q: input mapping references unknown node instance %q", def.ID, n.ID, root)
				}
			}
		}
	}
	return nil
}

// mappedNodeInput builds a mapped node's input from the run context.
// It returns an error (never a partial map) when any reference cannot
// be resolved, so the engine fails the node with input_resolution
// instead of dispatching it with a guessed input.
func mappedNodeInput(run Run, node DefinitionNode, outputs map[string]map[string]any) (map[string]any, error) {
	ctx := map[string]any{"input": deepCopyMap(run.Input)}
	for id, out := range outputs {
		ctx[id] = deepCopyMap(out)
	}
	resolved, err := resolveMappingValue(node.Input, ctx)
	if err != nil {
		return nil, err
	}
	m, ok := resolved.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("nexaflow: input mapping for node %q must resolve to an object", node.ID)
	}
	return m, nil
}
