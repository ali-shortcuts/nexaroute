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

// sameJSONValue reports whether two JSON-shaped values are equal
// after a JSON round trip, so Go scalar spellings of the same JSON
// number (1 vs 1.0) do not count as different inputs. A value that
// cannot be marshaled is never equal to anything (fail closed).
func sameJSONValue(a, b any) bool {
	na, err := normalizeJSON(a)
	if err != nil {
		return false
	}
	nb, err := normalizeJSON(b)
	if err != nil {
		return false
	}
	return string(na) == string(nb)
}

func normalizeJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
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
		if n.Input != nil {
			out.Nodes[i].Input = deepCopyMap(n.Input)
		}
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
