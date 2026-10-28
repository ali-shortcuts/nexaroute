package nexaflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// cloneMap deep-copies a JSON-shaped map via marshal round-trip.
func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// Phase 8 extensions to the Phase 1 definition contract (workflow.go).
//
// The Phase 1 contract deliberately deferred three things this phase must
// deliver for the end-to-end scenarios: explicit per-node input mappings,
// a bounded loop construct, and error-handler wiring. Each extension is
// optional and backward compatible: a definition without them validates
// and executes under the pure Phase 1 rules (predecessor-merge data flow,
// trigger bijection, branch-label rule, draft/published/superseded
// lifecycle).
//
// Why input mappings are required, not just convenient: Phase 1's merge
// flow composes only when a predecessor's output keys already match the
// next node's input schema, but #675's node schemas are strict (unknown
// keys are rejected). A health check output has no "value" key, so a
// condition node can never follow it under pure merge; a branch node
// needs "condition_result" while a condition node outputs "result". The
// mapping is the explicit bridge: when a node carries Input, the mapping
// fully determines its input (it replaces, not overlays, the merge).

// MaxLoopItems is the hard cap for a ForEach expansion; a node may set a
// lower bound but never a higher one.
const MaxLoopItems = 100

// ForEachSpec declares bounded iteration over an array produced by an
// earlier node. Source is a context path such as "plan.iterations".
type ForEachSpec struct {
	Source string `json:"source"`
	// Max caps the number of iterations (1..MaxLoopItems, default MaxLoopItems).
	Max int `json:"max,omitempty"`
}

// ErrorHandlerSpec wires one nexaroute.control.error_handler node into a
// workflow. When a node fails after its retries, the handler runs with the
// sanitized error and returns retry, fallback, or fail. FallbackNodeID, when
// set, is executed on "fallback" and its output stands in for the failed
// node's output downstream; the failed step stays failed in history.
type ErrorHandlerSpec struct {
	HandlerNodeID  string `json:"handler_node_id"`
	FallbackNodeID string `json:"fallback_node_id,omitempty"`
}

// NodeByID returns the node instance with the given id, if present.
func (d *Definition) NodeByID(id string) (DefinitionNode, bool) {
	for _, n := range d.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return DefinitionNode{}, false
}

// TriggerDecl returns the trigger declaration for a node instance id.
func (d *Definition) TriggerDecl(nodeID string) (DefinitionTrigger, bool) {
	for _, t := range d.Triggers {
		if t.Name == nodeID {
			return t, true
		}
	}
	return DefinitionTrigger{}, false
}

// IsTriggerNode reports whether an instance is a trigger-category node.
func (d *Definition) IsTriggerNode(id string, reg *Registry) bool {
	n, ok := d.NodeByID(id)
	if !ok {
		return false
	}
	spec, err := reg.Lookup(n.NodeID, n.Version)
	if err != nil {
		return false
	}
	return spec.Category == "trigger"
}

// EffectiveConfig returns the config a node executes with. Trigger nodes
// execute with their declaration's Config (Phase 1: the declaration's
// Config is the trigger node's runtime config); all other nodes execute
// with their instance Config.
func (d *Definition) EffectiveConfig(node DefinitionNode) map[string]any {
	if decl, ok := d.TriggerDecl(node.ID); ok {
		return decl.Config
	}
	return node.Config
}

// Incoming returns the edges into a node, in definition order.
func (d *Definition) Incoming(nodeID string) []DefinitionEdge {
	var out []DefinitionEdge
	for _, e := range d.Edges {
		if e.To == nodeID {
			out = append(out, e)
		}
	}
	return out
}

// Outgoing returns the edges out of a node, in definition order.
func (d *Definition) Outgoing(nodeID string) []DefinitionEdge {
	var out []DefinitionEdge
	for _, e := range d.Edges {
		if e.From == nodeID {
			out = append(out, e)
		}
	}
	return out
}

// ReachableFrom returns the set of node ids reachable from start,
// following outgoing edges (branch labels ignored: reachability is
// structural, selection happens at execution time).
func (d *Definition) ReachableFrom(start string) map[string]bool {
	seen := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, e := range d.Outgoing(id) {
			if !seen[e.To] {
				seen[e.To] = true
				queue = append(queue, e.To)
			}
		}
	}
	return seen
}

// ValidateDefinitionExtended validates a definition under the Phase 1
// contract plus the Phase 8 extensions. Phase 1 validation runs first
// (first error); extension problems are collected and joined.
func ValidateDefinitionExtended(def *Definition, reg *Registry) error {
	if err := ValidateDefinition(def, reg); err != nil {
		return err
	}
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}
	byID := make(map[string]DefinitionNode, len(def.Nodes))
	for _, n := range def.Nodes {
		byID[n.ID] = n
	}
	for _, n := range def.Nodes {
		for _, p := range collectRefPaths(n.Input) {
			root := strings.SplitN(p, ".", 2)[0]
			if root == "input" || root == "loop" {
				continue
			}
			if _, ok := byID[root]; !ok {
				add("node %q input references unknown node %q", n.ID, root)
			}
		}
		if n.ForEach != nil {
			if strings.TrimSpace(n.ForEach.Source) == "" {
				add("node %q for_each source must not be empty", n.ID)
			} else {
				root := strings.SplitN(n.ForEach.Source, ".", 2)[0]
				if root != "input" && root != "loop" {
					if _, ok := byID[root]; !ok {
						add("node %q for_each source references unknown node %q", n.ID, root)
					}
				}
				if root == n.ID {
					add("node %q for_each source must not reference itself", n.ID)
				}
			}
			max := n.ForEach.Max
			if max == 0 {
				max = MaxLoopItems
			}
			if max < 1 || max > MaxLoopItems {
				add("node %q for_each max must be between 1 and %d", n.ID, MaxLoopItems)
			}
		}
	}
	for _, t := range def.Triggers {
		if t.Kind != "schedule" {
			continue
		}
		interval, _ := t.Config["interval"].(string)
		daily, _ := t.Config["daily"].(string)
		if (interval == "") == (daily == "") {
			add("schedule trigger %q must set exactly one of interval and daily", t.Name)
			continue
		}
		if interval != "" {
			d, err := time.ParseDuration(interval)
			if err != nil || d <= 0 {
				add("schedule trigger %q interval %q must be a positive duration", t.Name, interval)
			}
		}
		if daily != "" {
			if _, err := time.Parse("15:04", daily); err != nil {
				add("schedule trigger %q daily %q must be HH:MM in UTC", t.Name, daily)
			}
		}
	}
	if def.OnError != nil {
		h, ok := byID[def.OnError.HandlerNodeID]
		if !ok {
			add("on_error handler references unknown node %q", def.OnError.HandlerNodeID)
		} else if h.NodeID != "nexaroute.control.error_handler" {
			add("on_error handler node %q must be a nexaroute.control.error_handler node", def.OnError.HandlerNodeID)
		}
		if def.OnError.FallbackNodeID != "" {
			if _, ok := byID[def.OnError.FallbackNodeID]; !ok {
				add("on_error fallback references unknown node %q", def.OnError.FallbackNodeID)
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("nexaflow: definition %q extension validation failed: %s", def.ID, strings.Join(problems, "; "))
	}
	return nil
}

// collectRefPaths walks an input mapping and returns every $ref path and
// every "$." shorthand path it contains.
func collectRefPaths(v any) []string {
	var out []string
	var walk func(x any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			if len(t) == 1 {
				if ref, ok := t["$ref"].(string); ok {
					out = append(out, ref)
					return
				}
			}
			for _, val := range t {
				walk(val)
			}
		case []any:
			for _, val := range t {
				walk(val)
			}
		case string:
			if strings.HasPrefix(t, "$.") {
				out = append(out, strings.TrimPrefix(t, "$."))
			}
		}
	}
	walk(v)
	return out
}
