package nexaflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// ContentHashOfGraph returns the hex sha256 of a graph's JSON encoding
// (encoding/json sorts map keys, so the hash is deterministic for a
// given graph and stable across JSON round-trips).
func ContentHashOfGraph(g Graph) (string, error) {
	raw, err := json.Marshal(g)
	if err != nil {
		return "", fmt.Errorf("nexaflow: graph is not JSON-encodable: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// Graph is a workflow definition: nodes placed on a canvas plus typed
// port edges between them. It is the only graph shape the editor, the
// validator, and the runtime share: the frontend never invents its own
// schema, it edits exactly this structure (serialized with the same JSON
// field names) and the backend re-validates it on every save and run.
//
// Port semantics (v1, dataflow): an edge copies one output field of the
// source node into one input field of the target node. Execution is a
// topological dataflow pass: every node runs once, in dependency order,
// after all of its predecessors have succeeded. Condition/branch nodes
// are value producers in v1: they do not gate downstream execution.
// Conditional execution is a documented future phase, not a silent
// reinterpretation of the graph.

// MaxGraphNodes bounds one graph; MaxGraphEdges bounds its edges. Both
// keep validation, storage, and canvas rendering bounded.
const (
	MaxGraphNodes = 200
	MaxGraphEdges = 500
)

// Position is a canvas coordinate in editor pixels. The runtime ignores
// it; it exists so the editor can round-trip layout without a second
// layout store.
type Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// GraphNode is one placed node. Config holds only fields declared in
// the node's ConfigSchema; unknown keys are rejected by validation and
// again by Registry.Execute at run time. Config never holds a secret
// value: secretRef fields hold references only, resolved by the backend.
type GraphNode struct {
	ID     string         `json:"id"`
	Type   string         `json:"type"`
	Label  string         `json:"label,omitempty"`
	Pos    Position       `json:"position"`
	Config map[string]any `json:"config,omitempty"`
}

// GraphEdge connects one output field to one input field.
type GraphEdge struct {
	ID       string `json:"id"`
	From     string `json:"from"`
	FromPort string `json:"from_port"`
	To       string `json:"to"`
	ToPort   string `json:"to_port"`
}

// Graph is the ordered node/edge definition of one workflow version.
//
// Inputs declares the run-input parameter names the workflow accepts
// (e.g. "operations" for a JSON-transform node). A required node input
// is satisfied by an incoming edge or by a declared input name; the
// runtime fills it from the run input. Trigger nodes are exempt: their
// inputs arrive with the run itself (webhook deliveries, schedule
// state, the manual payload).
type Graph struct {
	Nodes  []GraphNode `json:"nodes"`
	Edges  []GraphEdge `json:"edges"`
	Inputs []string    `json:"inputs,omitempty"`
}

// ValidationIssue is one validation finding. Level is "error" (the
// graph must not run) or "warning" (it may run but the operator should
// check: e.g. an LLM node whose prompt can only come from run input).
type ValidationIssue struct {
	Level   string `json:"level"`
	Code    string `json:"code"`
	NodeID  string `json:"node_id,omitempty"`
	Message string `json:"message"`
}

func errIssue(code, nodeID, format string, args ...any) ValidationIssue {
	return ValidationIssue{Level: "error", Code: code, NodeID: nodeID, Message: fmt.Sprintf(format, args...)}
}

func warnIssue(code, nodeID, format string, args ...any) ValidationIssue {
	return ValidationIssue{Level: "warning", Code: code, NodeID: nodeID, Message: fmt.Sprintf(format, args...)}
}

// HasErrors reports whether any issue blocks execution.
func HasErrors(issues []ValidationIssue) bool {
	for _, iss := range issues {
		if iss.Level == "error" {
			return true
		}
	}
	return false
}

func fieldNames(fields []Field) map[string]bool {
	out := make(map[string]bool, len(fields))
	for _, f := range fields {
		out[f.Name] = true
	}
	return out
}

// ValidateGraph checks a graph against the registry without executing
// anything. It never mutates the graph.
func ValidateGraph(reg *Registry, g Graph) []ValidationIssue {
	var issues []ValidationIssue
	if reg == nil {
		return []ValidationIssue{errIssue("internal_error", "", "no node registry")}
	}
	if len(g.Nodes) == 0 {
		issues = append(issues, errIssue("empty_graph", "", "graph has no nodes"))
		return issues
	}
	if len(g.Nodes) > MaxGraphNodes {
		issues = append(issues, errIssue("graph_too_large", "", "graph has %d nodes, maximum is %d", len(g.Nodes), MaxGraphNodes))
	}
	if len(g.Edges) > MaxGraphEdges {
		issues = append(issues, errIssue("graph_too_large", "", "graph has %d edges, maximum is %d", len(g.Edges), MaxGraphEdges))
	}
	nodes := make(map[string]*GraphNode, len(g.Nodes))
	specs := make(map[string]*NodeSpec, len(g.Nodes))
	hasTrigger := false
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if strings.TrimSpace(n.ID) == "" {
			issues = append(issues, errIssue("invalid_node_id", "", "node %d has an empty id", i))
			continue
		}
		if _, dup := nodes[n.ID]; dup {
			issues = append(issues, errIssue("duplicate_node_id", n.ID, "duplicate node id %q", n.ID))
			continue
		}
		nodes[n.ID] = n
		if math.IsNaN(n.Pos.X) || math.IsNaN(n.Pos.Y) || math.IsInf(n.Pos.X, 0) || math.IsInf(n.Pos.Y, 0) {
			issues = append(issues, errIssue("invalid_position", n.ID, "node %q has a non-finite position", n.ID))
		}
		spec, err := reg.Lookup(n.Type, SupportedVersion)
		if err != nil {
			issues = append(issues, errIssue("node_not_found", n.ID, "node %q has unknown type %q", n.ID, n.Type))
			continue
		}
		specs[n.ID] = spec
		if spec.Category == "trigger" {
			hasTrigger = true
		}
		if cfg := n.Config; cfg != nil {
			if err := reg.ValidateConfig(spec, cfg); err != nil {
				issues = append(issues, errIssue("invalid_config", n.ID, "node %q config invalid: %s", n.ID, RedactError(err)))
			}
		}
	}
	if !hasTrigger {
		issues = append(issues, errIssue("no_trigger", "", "graph has no trigger node; add a manual, webhook, or schedule trigger"))
	}
	// Edges: endpoints and ports must exist; duplicates are rejected.
	seenEdge := make(map[string]bool, len(g.Edges))
	incoming := make(map[string]map[string]bool, len(g.Nodes)) // node id -> input field fed by an edge
	for i := range g.Edges {
		e := &g.Edges[i]
		key := e.From + "\x00" + e.FromPort + "\x00" + e.To + "\x00" + e.ToPort
		if seenEdge[key] {
			issues = append(issues, errIssue("duplicate_edge", e.To, "duplicate edge %s.%s -> %s.%s", e.From, e.FromPort, e.To, e.ToPort))
			continue
		}
		seenEdge[key] = true
		fromSpec, ok := specs[e.From]
		if !ok {
			issues = append(issues, errIssue("unknown_edge_node", e.To, "edge source node %q does not exist", e.From))
			continue
		}
		toSpec, ok := specs[e.To]
		if !ok {
			issues = append(issues, errIssue("unknown_edge_node", e.To, "edge target node %q does not exist", e.To))
			continue
		}
		if !fieldNames(fromSpec.OutputSchema)[e.FromPort] {
			issues = append(issues, errIssue("unknown_port", e.From, "node %q has no output port %q", e.From, e.FromPort))
			continue
		}
		if !fieldNames(toSpec.InputSchema)[e.ToPort] {
			issues = append(issues, errIssue("unknown_port", e.To, "node %q has no input port %q", e.To, e.ToPort))
			continue
		}
		if incoming[e.To] == nil {
			incoming[e.To] = make(map[string]bool)
		}
		incoming[e.To][e.ToPort] = true
	}
	// Declared run inputs must be unique non-empty names.
	declaredInputs := make(map[string]bool, len(g.Inputs))
	for _, name := range g.Inputs {
		if strings.TrimSpace(name) == "" {
			issues = append(issues, errIssue("invalid_input", "", "run input names must not be empty"))
			continue
		}
		if declaredInputs[name] {
			issues = append(issues, errIssue("duplicate_input", "", "duplicate run input %q", name))
			continue
		}
		declaredInputs[name] = true
	}
	// Required inputs must be fed by an edge or declared as a run
	// input, except on trigger nodes whose inputs arrive with the run
	// itself.
	for id, spec := range specs {
		if spec.Category == "trigger" {
			continue
		}
		for _, f := range spec.InputSchema {
			if f.Required && !incoming[id][f.Name] && !declaredInputs[f.Name] {
				issues = append(issues, errIssue("missing_input", id, "node %q requires input %q but no edge feeds it and it is not a declared run input", id, f.Name))
			}
		}
		// LLM nodes need a prompt or messages from somewhere: an edge,
		// or run input. Without an edge it is only a warning, because
		// run input may supply it.
		if spec.ID == "nexaroute.llm.chat" && !incoming[id]["prompt"] && !incoming[id]["messages"] {
			issues = append(issues, warnIssue("llm_input_from_run", id, "LLM node %q has no prompt/messages edge; the prompt must come from run input", id))
		}
	}
	// Cycle detection (Kahn). A cycle is an error even if every other
	// check passes, because the runtime cannot order it.
	if _, err := TopologicalOrder(g); err != nil {
		issues = append(issues, errIssue("cycle", "", "graph contains a cycle: %s", RedactError(err)))
	}
	return issues
}

// TopologicalOrder returns node ids in dependency order (stable: ties
// break by graph order). A cyclic graph returns an error naming no
// node output, only the fact of the cycle.
func TopologicalOrder(g Graph) ([]string, error) {
	inDegree := make(map[string]int, len(g.Nodes))
	adj := make(map[string][]string, len(g.Nodes))
	seen := make(map[string]bool, len(g.Nodes))
	order := make([]string, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		if seen[n.ID] {
			continue
		}
		seen[n.ID] = true
		inDegree[n.ID] = 0
	}
	for _, e := range g.Edges {
		if !seen[e.From] || !seen[e.To] {
			continue
		}
		adj[e.From] = append(adj[e.From], e.To)
		inDegree[e.To]++
	}
	// Kahn with a queue seeded in graph order for determinism.
	var queue []string
	for _, n := range g.Nodes {
		if inDegree[n.ID] == 0 {
			queue = append(queue, n.ID)
		}
	}
	queued := make(map[string]bool, len(queue))
	for _, id := range queue {
		queued[id] = true
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		order = append(order, id)
		for _, next := range adj[id] {
			inDegree[next]--
			if inDegree[next] == 0 && !queued[next] {
				queued[next] = true
				queue = append(queue, next)
			}
		}
	}
	if len(order) != len(seen) {
		return nil, fmt.Errorf("graph contains a cycle")
	}
	return order, nil
}

// CloneGraph deep-copies a graph (config maps included) so a published
// snapshot can never be mutated by later draft edits.
func CloneGraph(g Graph) Graph {
	out := Graph{
		Nodes:  make([]GraphNode, len(g.Nodes)),
		Edges:  make([]GraphEdge, len(g.Edges)),
		Inputs: append([]string(nil), g.Inputs...),
	}
	copy(out.Edges, g.Edges)
	for i, n := range g.Nodes {
		out.Nodes[i] = n
		if n.Config != nil {
			cfg := make(map[string]any, len(n.Config))
			for k, v := range n.Config {
				cfg[k] = deepCopyValue(v)
			}
			out.Nodes[i].Config = cfg
		}
	}
	return out
}

func deepCopyValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, child := range val {
			out[k] = deepCopyValue(child)
		}
		return out
	case []any:
		out := make([]any, len(val))
		for i, child := range val {
			out[i] = deepCopyValue(child)
		}
		return out
	default:
		return v
	}
}
