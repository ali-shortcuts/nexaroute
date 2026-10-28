package nexaflow

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// authzPermissions is the full permission set the registry may
// reference. It mirrors internal/authz's constants (not its
// unexported role map): a node naming a permission outside this set
// fails the contract, which forces an explicit ADR update instead of
// a silently invented permission.
var authzPermissions = map[authz.Permission]bool{
	authz.ReadConfig:     true,
	authz.WriteConfig:    true,
	authz.ReadProviders:  true,
	authz.WriteProviders: true,
	authz.ReadRouting:    true,
	authz.WriteRouting:   true,
	authz.ReadUsage:      true,
	authz.RunEvaluation:  true,
	authz.ReadAudit:      true,
	authz.ManageKeys:     true,
	authz.ManageVideo:    true,
}

var validFieldTypes = map[string]bool{
	"string": true, "number": true, "boolean": true,
	"object": true, "array": true, "secretRef": true,
}

var validSideEffects = map[SideEffect]bool{
	SideEffectNone: true, SideEffectRead: true, SideEffectExternal: true,
	SideEffectBillable: true, SideEffectIrreversible: true,
}

// TestRegistryContractNodeShapes asserts the structural invariants
// every v1 node must satisfy. These are the contract the runtime,
// the definition validator, and the docs all rely on; a new node
// that breaks one fails here, not in production.
func TestRegistryContractNodeShapes(t *testing.T) {
	reg := DefaultRegistry()
	for _, spec := range reg.All() {
		if spec.Category == "" {
			t.Errorf("node %q: empty category", spec.ID)
		}
		if spec.Title == "" {
			t.Errorf("node %q: empty title", spec.ID)
		}
		if spec.Description == "" {
			t.Errorf("node %q: empty description", spec.ID)
		}
		if spec.Version != SupportedVersion {
			t.Errorf("node %q: version %q, want %q", spec.ID, spec.Version, SupportedVersion)
		}
		if spec.DefaultTimeout <= 0 {
			t.Errorf("node %q: DefaultTimeout %v, want > 0", spec.ID, spec.DefaultTimeout)
		}
		if spec.MaxTimeout < spec.DefaultTimeout {
			t.Errorf("node %q: MaxTimeout %v < DefaultTimeout %v", spec.ID, spec.MaxTimeout, spec.DefaultTimeout)
		}
		if spec.MaxInputBytes <= 0 {
			t.Errorf("node %q: MaxInputBytes %d, want > 0", spec.ID, spec.MaxInputBytes)
		}
		if spec.MaxOutputBytes <= 0 {
			t.Errorf("node %q: MaxOutputBytes %d, want > 0", spec.ID, spec.MaxOutputBytes)
		}
		if spec.Retry.MaxAttempts < 1 || spec.Retry.MaxAttempts > 3 {
			t.Errorf("node %q: Retry.MaxAttempts %d outside [1, 3]", spec.ID, spec.Retry.MaxAttempts)
		}
		for _, code := range spec.Retry.RetryableCodes {
			if code == "" {
				t.Errorf("node %q: empty retryable code", spec.ID)
			}
		}
		if !validSideEffects[spec.SideEffect] {
			t.Errorf("node %q: unknown side effect %q", spec.ID, spec.SideEffect)
		}
		if spec.SideEffect == SideEffectIrreversible {
			t.Errorf("node %q: declares irreversible in v1; no v1 node may do so (see ADR 0008)", spec.ID)
		}
		if spec.Permission != "" && !authzPermissions[spec.Permission] {
			t.Errorf("node %q: permission %q is not an authz permission", spec.ID, spec.Permission)
		}
		// Policy-gated implies side-effecting (billable or external).
		// The converse is deliberately false: mcp.call is external
		// but allowlist-gated, not policy-gated.
		if spec.RequiresOperatorPolicy && spec.SideEffect != SideEffectBillable && spec.SideEffect != SideEffectExternal {
			t.Errorf("node %q: RequiresOperatorPolicy with side effect %q, want billable or external", spec.ID, spec.SideEffect)
		}
		for name, schema := range map[string][]Field{
			"input": spec.InputSchema, "output": spec.OutputSchema, "config": spec.ConfigSchema,
		} {
			seen := map[string]bool{}
			for _, f := range schema {
				if f.Name == "" {
					t.Errorf("node %q: empty field name in %s schema", spec.ID, name)
				}
				if seen[f.Name] {
					t.Errorf("node %q: duplicate field %q in %s schema", spec.ID, f.Name, name)
				}
				seen[f.Name] = true
				if !validFieldTypes[f.Type] {
					t.Errorf("node %q: field %q has unknown type %q", spec.ID, f.Name, f.Type)
				}
			}
		}
	}
}

// TestRegistryContractPolicySurface locks the v1 policy surface: the
// only policy-gated nodes are the billable ones plus the external
// HTTP node. Adding a policy gate (or removing one) must update this
// test and ADR 0008 deliberately.
func TestRegistryContractPolicySurface(t *testing.T) {
	want := map[string]bool{
		"nexaroute.llm.chat":     true,
		"nexaroute.video.submit": true,
		"nexaroute.http.request": true,
	}
	reg := DefaultRegistry()
	for _, spec := range reg.All() {
		if spec.RequiresOperatorPolicy && !want[spec.ID] {
			t.Errorf("node %q unexpectedly requires operator policy", spec.ID)
		}
		if !spec.RequiresOperatorPolicy && want[spec.ID] {
			t.Errorf("node %q lost its operator policy requirement", spec.ID)
		}
	}
}

// TestRegistryDeterministic asserts DefaultRegistry returns the same
// nodes in the same order on every call: definition validation and
// docs generation must not depend on map iteration order.
func TestRegistryDeterministic(t *testing.T) {
	first := DefaultRegistry().All()
	second := DefaultRegistry().All()
	if len(first) != len(second) {
		t.Fatalf("registry size changed between calls: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("registry order changed at %d: %q vs %q", i, first[i].ID, second[i].ID)
		}
	}
}

func validTestSpec() *NodeSpec {
	return &NodeSpec{
		ID: "test.node", Version: "v1",
		Retry:          RetryPolicy{MaxAttempts: 1},
		DefaultTimeout: time.Second, MaxTimeout: time.Second,
		MaxInputBytes: 1024, MaxOutputBytes: 1024,
		Run: func(ctx context.Context, rc *RunContext) (map[string]any, error) {
			return map[string]any{}, nil
		},
	}
}

// TestNewRegistryRejections pins the fail-closed registration rules.
// Note the wording: out-of-range values are REJECTED, not clamped.
func TestNewRegistryRejections(t *testing.T) {
	cases := []struct {
		name string
		mut  func([]*NodeSpec) []*NodeSpec
	}{
		{"nil spec", func(s []*NodeSpec) []*NodeSpec { return []*NodeSpec{nil} }},
		{"empty id", func(s []*NodeSpec) []*NodeSpec { s[0].ID = ""; return s }},
		{"bad version", func(s []*NodeSpec) []*NodeSpec { s[0].Version = "v2"; return s }},
		{"nil run", func(s []*NodeSpec) []*NodeSpec { s[0].Run = nil; return s }},
		{"attempts zero", func(s []*NodeSpec) []*NodeSpec { s[0].Retry.MaxAttempts = 0; return s }},
		{"attempts four", func(s []*NodeSpec) []*NodeSpec { s[0].Retry.MaxAttempts = 4; return s }},
		{"default timeout zero", func(s []*NodeSpec) []*NodeSpec { s[0].DefaultTimeout = 0; return s }},
		{"max below default", func(s []*NodeSpec) []*NodeSpec { s[0].MaxTimeout = time.Millisecond; return s }},
		{"input bound zero", func(s []*NodeSpec) []*NodeSpec { s[0].MaxInputBytes = 0; return s }},
		{"output bound zero", func(s []*NodeSpec) []*NodeSpec { s[0].MaxOutputBytes = 0; return s }},
		{"duplicate id", func(s []*NodeSpec) []*NodeSpec { return append(s, validTestSpec()) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRegistry(tc.mut([]*NodeSpec{validTestSpec()})); err == nil {
				t.Fatal("invalid registry accepted")
			}
		})
	}
	if _, err := NewRegistry([]*NodeSpec{validTestSpec()}); err != nil {
		t.Fatalf("valid registry rejected: %v", err)
	}
}

// TestTemplatesReferenceContract covers what TestTemplatesInert does
// not: template node configs must validate against the node schemas,
// and template edges must reference nodes of the same template.
// Templates edge by registry node ID (not instance ID) because a
// template never contains two instances of one node; definitions
// edge by instance ID instead. That difference is part of the
// contract, documented in ADR 0008.
func TestTemplatesReferenceContract(t *testing.T) {
	reg := DefaultRegistry()
	for _, tpl := range DefaultTemplates() {
		if tpl.Version != SupportedVersion {
			t.Errorf("template %q: version %q, want %q", tpl.ID, tpl.Version, SupportedVersion)
		}
		nodeIDs := map[string]bool{}
		for _, node := range tpl.Nodes {
			if nodeIDs[node.NodeID] {
				t.Errorf("template %q: duplicate node %q (templates are single-instance)", tpl.ID, node.NodeID)
			}
			nodeIDs[node.NodeID] = true
			spec, err := reg.Lookup(node.NodeID, node.Version)
			if err != nil {
				t.Errorf("template %q: %v", tpl.ID, err)
				continue
			}
			// Placeholders are resolved at Instantiate; validate the
			// raw config only when it contains no placeholders, so
			// "{{name}}" strings do not fail type checks here.
			hasPlaceholder := false
			for _, v := range node.Config {
				if s, ok := v.(string); ok && len(s) > 4 && s[:2] == "{{" {
					hasPlaceholder = true
				}
			}
			if !hasPlaceholder {
				if err := reg.ValidateConfig(spec, node.Config); err != nil {
					t.Errorf("template %q node %q: config invalid: %v", tpl.ID, node.NodeID, err)
				}
			}
		}
		for _, edge := range tpl.Edges {
			if !nodeIDs[edge[0]] || !nodeIDs[edge[1]] {
				t.Errorf("template %q: edge %q -> %q references a node outside the template", tpl.ID, edge[0], edge[1])
			}
		}
	}
	// Sanity: reflect confirms the template edge type is a node-ID
	// pair, not an instance-ID pair, so the distinction above is real.
	if reflect.TypeOf(DefaultTemplates()[0].Edges).String() != "[][2]string" {
		t.Fatal("template edge type changed; update the contract documentation")
	}
}
