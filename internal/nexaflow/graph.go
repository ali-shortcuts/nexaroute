package nexaflow

// Graph helpers for workflow definitions. These operate on the Phase 1
// contract (workflow.go): nodes are instances, edges reference
// instance IDs, and there are no ports. TopologicalOrder assumes the
// definition already passed ValidateDefinition (acyclic); it returns
// node instance ids in dependency order, deterministic across engines:
// among nodes with equal readiness, ids sort ascending, so two engines
// derive the same schedule from the same definition.

// TopologicalOrder returns node instance ids in dependency order
// (sources first).
func TopologicalOrder(def Definition) []string {
	indegree := make(map[string]int, len(def.Nodes))
	adj := make(map[string][]string, len(def.Nodes))
	ids := make([]string, 0, len(def.Nodes))
	for _, n := range def.Nodes {
		ids = append(ids, n.ID)
	}
	for _, e := range def.Edges {
		adj[e.From] = append(adj[e.From], e.To)
		indegree[e.To]++
	}
	sortStrings(ids)
	ready := make([]string, 0, len(ids))
	for _, id := range ids {
		if indegree[id] == 0 {
			ready = append(ready, id)
		}
	}
	out := make([]string, 0, len(ids))
	for len(ready) > 0 {
		cur := ready[0]
		ready = ready[1:]
		out = append(out, cur)
		for _, next := range adj[cur] {
			indegree[next]--
			if indegree[next] == 0 {
				pos := len(ready)
				for pos > 0 && ready[pos-1] > next {
					pos--
				}
				ready = append(ready, "")
				copy(ready[pos+1:], ready[pos:])
				ready[pos] = next
			}
		}
	}
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func nodeDefByID(def Definition, id string) DefinitionNode {
	for _, n := range def.Nodes {
		if n.ID == id {
			return n
		}
	}
	return DefinitionNode{ID: id}
}

func incomingEdges(def Definition, nodeID string) []DefinitionEdge {
	var out []DefinitionEdge
	for _, e := range def.Edges {
		if e.To == nodeID {
			out = append(out, e)
		}
	}
	return out
}

// edgeFired reports whether an edge carries a value: unlabelled edges
// always fire; a labelled edge fires only when the source node's
// output field "selected_branch" equals the label (the branch node's
// contract; validation already restricts labels to branch nodes).
func edgeFired(e DefinitionEdge, outputs map[string]map[string]any) bool {
	if e.Branch == "" {
		return true
	}
	out, ok := outputs[e.From]
	if !ok {
		return false
	}
	sel, _ := out["selected_branch"].(string)
	return sel == e.Branch
}

// triggerConfig returns the effective runtime config for a node
// instance: for trigger-category nodes it is the trigger declaration's
// config (ADR 0008 §D2: the declaration's config IS the trigger node's
// runtime config); for every other node it is the node's own config.
func triggerConfig(def Definition, node DefinitionNode, spec *NodeSpec) map[string]any {
	if spec != nil && spec.Category == "trigger" {
		for _, tr := range def.Triggers {
			if tr.Name == node.ID {
				return deepCopyMap(tr.Config)
			}
		}
	}
	return deepCopyMap(node.Config)
}

// SortedNodeIDs returns definition node ids in topological order; it is
// a convenience for inspectors and tests.
func SortedNodeIDs(def Definition) []string { return TopologicalOrder(def) }
