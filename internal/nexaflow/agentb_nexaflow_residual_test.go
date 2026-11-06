package nexaflow

// Residual pure-scope tests (Agent B): the InstantiateDefinition error
// paths and the normalizeJSON unmarshal failure the pure-coverage pass
// left uncovered.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentBTemplateUnknownNodeRejectedByInstantiate(t *testing.T) {
	// The short-id guards in InstantiateDefinition are unreachable
	// (Instantiate Lookups every NodeID first, and all registered
	// last segments are distinct); the reachable rejection for a bad
	// NodeID is Instantiate's Lookup error.
	for _, id := range []string{"", "foo.", "no.such.node"} {
		tpl := Template{ID: "t1", Nodes: []TemplateNode{{NodeID: id, Version: SupportedVersion}}}
		_, err := tpl.InstantiateDefinition(nil, false)
		if err == nil || !strings.Contains(err.Error(), "references unknown node") {
			t.Fatalf("InstantiateDefinition(NodeID %q) = %v, want an unknown-node error", id, err)
		}
	}
}

func TestAgentBNormalizeJSONUnmarshalError(t *testing.T) {
	// json.Number("1e400") marshals verbatim (a valid literal) but
	// cannot unmarshal into any (out of float64 range): the only
	// value shape that makes normalizeJSON's Unmarshal fail after a
	// successful Marshal.
	if _, err := normalizeJSON(map[string]any{"x": json.Number("1e400")}); err == nil {
		t.Fatal("normalizeJSON(json.Number 1e400) = nil error, want an unmarshal failure")
	}
	if _, err := normalizeJSON(map[string]any{"x": json.Number("42")}); err != nil {
		t.Fatalf("normalizeJSON(json.Number 42) = %v, want nil", err)
	}
}
