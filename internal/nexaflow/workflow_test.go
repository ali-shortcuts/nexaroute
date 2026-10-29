package nexaflow

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// validDef returns a minimal valid definition: a manual trigger feeding
// a branch node. Configs use only values verified against the node
// schemas (manual: empty config; branch: both branch names required).
func validDef() *Definition {
	return &Definition{
		SchemaVersion: DefinitionSchemaVersion,
		ID:            "wf-1",
		Name:          "Test workflow",
		Nodes: []DefinitionNode{
			{ID: "t1", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "b1", NodeID: "nexaroute.control.branch", Version: SupportedVersion,
				Config: map[string]any{"true_branch": "yes", "false_branch": "no"}},
		},
		Edges: []DefinitionEdge{
			{ID: "e1", From: "t1", To: "b1"},
		},
		Triggers: []DefinitionTrigger{
			{Name: "t1", Kind: TriggerManual},
		},
	}
}

func TestValidateDefinitionAcceptsValid(t *testing.T) {
	if err := ValidateDefinition(validDef(), DefaultRegistry()); err != nil {
		t.Fatalf("valid definition rejected: %v", err)
	}
}

func TestValidateDefinitionDoesNotMutate(t *testing.T) {
	def := validDef()
	before, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateDefinition(def, DefaultRegistry()); err != nil {
		t.Fatalf("valid definition rejected: %v", err)
	}
	after, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("validation mutated the definition:\nbefore %s\nafter  %s", before, after)
	}
}

func TestValidateDefinitionRejectsBadIdentity(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Definition)
	}{
		{"nil registry handled by caller", func(d *Definition) {}},
		{"empty schema version", func(d *Definition) { d.SchemaVersion = "v2" }},
		{"empty id", func(d *Definition) { d.ID = "" }},
		{"empty name", func(d *Definition) { d.Name = "" }},
		{"no nodes", func(d *Definition) { d.Nodes = nil; d.Edges = nil; d.Triggers = nil }},
		{"no triggers", func(d *Definition) { d.Triggers = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := validDef()
			tc.mut(def)
			if tc.name == "nil registry handled by caller" {
				if err := ValidateDefinition(def, nil); err == nil {
					t.Fatal("nil registry accepted")
				}
				return
			}
			if err := ValidateDefinition(def, DefaultRegistry()); err == nil {
				t.Fatalf("definition accepted: %+v", def)
			}
		})
	}
	if err := ValidateDefinition(nil, DefaultRegistry()); err == nil {
		t.Fatal("nil definition accepted")
	}
}

func TestValidateDefinitionRejectsBadNodes(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Definition)
	}{
		{"duplicate instance id", func(d *Definition) {
			d.Nodes = append(d.Nodes, DefinitionNode{ID: "t1", NodeID: "nexaroute.control.branch", Version: SupportedVersion, Config: map[string]any{"true_branch": "a", "false_branch": "b"}})
		}},
		{"unknown node", func(d *Definition) { d.Nodes[1].NodeID = "nexaroute.nope" }},
		{"unknown version", func(d *Definition) { d.Nodes[1].Version = "v2" }},
		{"empty instance id", func(d *Definition) { d.Nodes[1].ID = "" }},
		{"unknown config key", func(d *Definition) {
			d.Nodes[1].Config["surprise"] = 1
		}},
		{"wrong config type", func(d *Definition) { d.Nodes[1].Config["true_branch"] = 123 }},
		{"missing required config", func(d *Definition) { d.Nodes[1].Config = map[string]any{} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := validDef()
			tc.mut(def)
			if err := ValidateDefinition(def, DefaultRegistry()); err == nil {
				t.Fatal("invalid definition accepted")
			}
		})
	}
}

func TestValidateDefinitionRejectsBadEdges(t *testing.T) {
	branchCfg := map[string]any{"true_branch": "yes", "false_branch": "no"}
	cases := []struct {
		name string
		mut  func(*Definition)
	}{
		{"dangling target", func(d *Definition) { d.Edges[0].To = "ghost" }},
		{"dangling source", func(d *Definition) { d.Edges[0].From = "ghost" }},
		{"self edge", func(d *Definition) { d.Edges[0].From = "b1"; d.Edges[0].To = "b1" }},
		{"duplicate edge id", func(d *Definition) {
			d.Edges = append(d.Edges, DefinitionEdge{ID: "e1", From: "t1", To: "b1", Branch: "yes"})
		}},
		{"duplicate edge triple", func(d *Definition) {
			d.Edges = append(d.Edges, DefinitionEdge{ID: "e2", From: "t1", To: "b1"})
		}},
		{"branch label from non-branch node", func(d *Definition) { d.Edges[0].Branch = "yes" }},
		{"branch label not configured", func(d *Definition) {
			d.Nodes = append(d.Nodes, DefinitionNode{ID: "b2", NodeID: "nexaroute.control.branch", Version: SupportedVersion, Config: branchCfg})
			d.Edges = append(d.Edges, DefinitionEdge{ID: "e2", From: "b1", To: "b2", Branch: "zzz"})
		}},
		{"trigger with incoming edge", func(d *Definition) {
			d.Nodes = append(d.Nodes, DefinitionNode{ID: "t2", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion})
			d.Triggers = append(d.Triggers, DefinitionTrigger{Name: "t2", Kind: TriggerManual})
			d.Edges = append(d.Edges, DefinitionEdge{ID: "e2", From: "t1", To: "t2"})
		}},
		{"non-trigger root", func(d *Definition) {
			d.Edges = nil
		}},
		{"cycle", func(d *Definition) {
			d.Nodes = append(d.Nodes, DefinitionNode{ID: "b2", NodeID: "nexaroute.control.branch", Version: SupportedVersion, Config: branchCfg})
			d.Edges = append(d.Edges,
				DefinitionEdge{ID: "e2", From: "b1", To: "b2"},
				DefinitionEdge{ID: "e3", From: "b2", To: "b1"})
		}},
		{"unreachable subgraph", func(d *Definition) {
			d.Nodes = append(d.Nodes,
				DefinitionNode{ID: "a", NodeID: "nexaroute.control.branch", Version: SupportedVersion, Config: branchCfg},
				DefinitionNode{ID: "b", NodeID: "nexaroute.control.branch", Version: SupportedVersion, Config: branchCfg})
			d.Edges = append(d.Edges,
				DefinitionEdge{ID: "e2", From: "a", To: "b"},
				DefinitionEdge{ID: "e3", From: "b", To: "a"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := validDef()
			tc.mut(def)
			if err := ValidateDefinition(def, DefaultRegistry()); err == nil {
				t.Fatalf("invalid definition accepted: %+v", def)
			}
		})
	}
}

func TestValidateDefinitionBranchLabelAccepted(t *testing.T) {
	def := validDef()
	def.Nodes = append(def.Nodes, DefinitionNode{ID: "b2", NodeID: "nexaroute.control.branch", Version: SupportedVersion,
		Config: map[string]any{"true_branch": "yes", "false_branch": "no"}})
	def.Edges = append(def.Edges, DefinitionEdge{ID: "e2", From: "b1", To: "b2", Branch: "yes"})
	if err := ValidateDefinition(def, DefaultRegistry()); err != nil {
		t.Fatalf("labelled branch edge rejected: %v", err)
	}
}

func TestValidateDefinitionRejectsBadTriggers(t *testing.T) {
	webhookDef := func() *Definition {
		return &Definition{
			SchemaVersion: DefinitionSchemaVersion,
			ID:            "wf-webhook",
			Name:          "Webhook workflow",
			Nodes: []DefinitionNode{
				{ID: "w1", NodeID: "nexaroute.trigger.webhook", Version: SupportedVersion,
					Config: map[string]any{"secret_ref": "vault:hook-secret"}},
				{ID: "b1", NodeID: "nexaroute.control.branch", Version: SupportedVersion,
					Config: map[string]any{"true_branch": "yes", "false_branch": "no"}},
			},
			Edges: []DefinitionEdge{{ID: "e1", From: "w1", To: "b1"}},
			Triggers: []DefinitionTrigger{
				{Name: "w1", Kind: TriggerWebhook, Config: map[string]any{"secret_ref": "vault:hook-secret"}},
			},
		}
	}
	if err := ValidateDefinition(webhookDef(), DefaultRegistry()); err != nil {
		t.Fatalf("valid webhook definition rejected: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*Definition)
	}{
		{"duplicate trigger", func(d *Definition) {
			d.Triggers = append(d.Triggers, DefinitionTrigger{Name: "w1", Kind: TriggerWebhook, Config: map[string]any{"secret_ref": "vault:hook-secret"}})
		}},
		{"unknown kind", func(d *Definition) { d.Triggers[0].Kind = "cron" }},
		{"trigger name is not a node", func(d *Definition) { d.Triggers[0].Name = "ghost" }},
		{"kind does not match node", func(d *Definition) { d.Triggers[0].Kind = TriggerSchedule }},
		{"trigger config missing required secret_ref", func(d *Definition) {
			d.Triggers[0].Config = map[string]any{}
		}},
		{"node config missing required secret_ref", func(d *Definition) {
			d.Nodes[0].Config = map[string]any{}
		}},
		// Trigger node without declaration: handled specially below
		// (built from the valid manual definition, not the webhook one).
		{"trigger node without declaration", func(d *Definition) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := webhookDef()
			if tc.name == "trigger node without declaration" {
				// Build from the valid manual definition instead: two
				// trigger roots, only one declared.
				def = validDef()
				def.Nodes = append(def.Nodes, DefinitionNode{ID: "t2", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion})
			} else {
				tc.mut(def)
			}
			if err := ValidateDefinition(def, DefaultRegistry()); err == nil {
				t.Fatal("invalid definition accepted")
			}
		})
	}
}

func TestValidateDefinitionScheduleTrigger(t *testing.T) {
	def := &Definition{
		SchemaVersion: DefinitionSchemaVersion,
		ID:            "wf-sched",
		Name:          "Scheduled workflow",
		Nodes: []DefinitionNode{
			{ID: "s1", NodeID: "nexaroute.trigger.schedule", Version: SupportedVersion,
				Config: map[string]any{"daily": "09:00"}},
			{ID: "b1", NodeID: "nexaroute.control.branch", Version: SupportedVersion,
				Config: map[string]any{"true_branch": "yes", "false_branch": "no"}},
		},
		Edges:    []DefinitionEdge{{ID: "e1", From: "s1", To: "b1"}},
		Triggers: []DefinitionTrigger{{Name: "s1", Kind: TriggerSchedule, Config: map[string]any{"daily": "09:00"}}},
	}
	if err := ValidateDefinition(def, DefaultRegistry()); err != nil {
		t.Fatalf("valid schedule definition rejected: %v", err)
	}
	// Layering: interval+daily together is schema-conformant, so the
	// definition layer accepts it; the schedule node's own execution
	// validation enforces "exactly one". The definition layer must not
	// duplicate node runtime semantics.
	def.Nodes[0].Config = map[string]any{"interval": "1h", "daily": "09:00"}
	def.Triggers[0].Config = map[string]any{"interval": "1h", "daily": "09:00"}
	if err := ValidateDefinition(def, DefaultRegistry()); err != nil {
		t.Fatalf("schema-conformant schedule definition rejected at the wrong layer: %v", err)
	}
}

func TestValidateDefinitionRejectsBadSettings(t *testing.T) {
	cases := []struct {
		name string
		s    DefinitionSettings
	}{
		{"negative timeout", DefinitionSettings{RunTimeoutSeconds: -1}},
		{"negative concurrency", DefinitionSettings{MaxConcurrentRuns: -1}},
		{"retention too low", DefinitionSettings{RetentionDays: 3}},
		{"retention too high", DefinitionSettings{RetentionDays: 400}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			def := validDef()
			def.Settings = tc.s
			if err := ValidateDefinition(def, DefaultRegistry()); err == nil {
				t.Fatal("invalid settings accepted")
			}
		})
	}
	def := validDef()
	def.Settings = DefinitionSettings{RunTimeoutSeconds: 3600, MaxConcurrentRuns: 2, RetentionDays: 90}
	if err := ValidateDefinition(def, DefaultRegistry()); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
	// Zero settings mean defaults (no deadline, concurrency 1, 90 days).
	def.Settings = DefinitionSettings{}
	if err := ValidateDefinition(def, DefaultRegistry()); err != nil {
		t.Fatalf("default settings rejected: %v", err)
	}
}

func TestContentHash(t *testing.T) {
	def := validDef()
	h1, err := ContentHash(def)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := ContentHash(def)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("hash unstable: %q vs %q", h1, h2)
	}
	raw, err := json.Marshal(def)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip Definition
	if err := json.Unmarshal(raw, &roundtrip); err != nil {
		t.Fatal(err)
	}
	h3, err := ContentHash(&roundtrip)
	if err != nil {
		t.Fatal(err)
	}
	if h3 != h1 {
		t.Fatalf("JSON round-trip changed the hash: %q vs %q", h1, h3)
	}
	if !reflect.DeepEqual(def, &roundtrip) {
		t.Fatalf("JSON round-trip changed the definition:\n%+v\n%+v", def, &roundtrip)
	}
	def.Name = "Renamed workflow"
	h4, err := ContentHash(def)
	if err != nil {
		t.Fatal(err)
	}
	if h4 == h1 {
		t.Fatal("changed definition hashed identically")
	}
	if _, err := ContentHash(nil); err == nil || !strings.Contains(err.Error(), "nil") {
		t.Fatalf("nil definition hash error = %v", err)
	}
}

func TestDefinitionVersionTransitions(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		{DefinitionDraft, DefinitionPublished, true},
		{DefinitionDraft, DefinitionSuperseded, false},
		{DefinitionDraft, DefinitionDraft, false},
		{DefinitionPublished, DefinitionSuperseded, true},
		{DefinitionPublished, DefinitionPublished, false},
		{DefinitionPublished, DefinitionDraft, false},
		{DefinitionSuperseded, DefinitionPublished, false},
		{DefinitionSuperseded, DefinitionSuperseded, false},
		{"bogus", DefinitionPublished, false},
	}
	for _, tc := range cases {
		v := DefinitionVersion{Status: tc.from}
		if got := v.CanTransitionTo(tc.to); got != tc.want {
			t.Fatalf("%q -> %q = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}
