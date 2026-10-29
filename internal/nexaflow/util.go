package nexaflow

import (
	"encoding/json"
	"sort"
)

func jsonLen(v any) int {
	raw, err := json.Marshal(v)
	if err != nil {
		return 1 << 30
	}
	return len(raw)
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// deepCopyValue copies JSON-shaped values (maps, slices, scalars)
// without a JSON round trip, so Go scalar types (int, int64) survive.
func deepCopyValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = deepCopyValue(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = deepCopyValue(val)
		}
		return out
	default:
		return v
	}
}

func deepCopyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	out, _ := deepCopyValue(m).(map[string]any)
	return out
}

// cloneDefinition deep-copies a workflow definition, including every
// node config and trigger config map, so stored snapshots can never be
// mutated through a returned value.
func cloneDefinition(def Definition) Definition {
	out := def
	out.Nodes = make([]DefinitionNode, len(def.Nodes))
	for i, n := range def.Nodes {
		out.Nodes[i] = n
		out.Nodes[i].Config = deepCopyMap(n.Config)
	}
	out.Edges = make([]DefinitionEdge, len(def.Edges))
	copy(out.Edges, def.Edges)
	out.Triggers = make([]DefinitionTrigger, len(def.Triggers))
	for i, tr := range def.Triggers {
		out.Triggers[i] = tr
		out.Triggers[i].Config = deepCopyMap(tr.Config)
	}
	return out
}
