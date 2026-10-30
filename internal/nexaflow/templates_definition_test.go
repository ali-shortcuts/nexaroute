package nexaflow

import (
	"context"
	"strings"
	"testing"
)

func filledPlaceholders(t *testing.T, tpl Template, policyApproved bool) map[string]string {
	t.Helper()
	filled := map[string]string{}
	for _, name := range tpl.Placeholders {
		switch name {
		case "threshold_usd":
			filled[name] = "10"
		case "notify_url":
			filled[name] = "https://example.invalid/notify"
		case "prompt":
			filled[name] = "a cat"
		default:
			filled[name] = "placeholder-value"
		}
	}
	return filled
}

// Every default template instantiates into a validated Definition:
// triggers present and bijective, edges referential and acyclic, every
// non-trigger node reachable and input-covered.
func TestTemplatesInstantiateToValidDefinitions(t *testing.T) {
	for _, tpl := range DefaultTemplates() {
		t.Run(tpl.ID, func(t *testing.T) {
			def, err := tpl.InstantiateDefinition(filledPlaceholders(t, tpl, true), true)
			if err != nil {
				t.Fatalf("InstantiateDefinition: %v", err)
			}
			if def.ID != tpl.ID || def.Name != tpl.Title {
				t.Fatalf("definition identity = %q/%q, want %q/%q", def.ID, def.Name, tpl.ID, tpl.Title)
			}
			if len(def.Triggers) == 0 {
				t.Fatal("definition has no triggers: it can never run")
			}
			if len(def.Nodes) != len(tpl.Nodes) {
				t.Fatalf("node count = %d, want %d", len(def.Nodes), len(tpl.Nodes))
			}
			// ValidateDefinition already ran inside InstantiateDefinition;
			// re-running it here pins that the returned value, not just
			// an intermediate, is valid.
			if err := ValidateDefinition(def, DefaultRegistry()); err != nil {
				t.Fatalf("returned definition invalid: %v", err)
			}
		})
	}
}

func TestTemplatesInstantiateDefinitionSpotChecks(t *testing.T) {
	byID := map[string]Template{}
	for _, tpl := range DefaultTemplates() {
		byID[tpl.ID] = tpl
	}
	health, err := byID["model-health-alert"].InstantiateDefinition(map[string]string{"deployment_id": "prov/model"}, true)
	if err != nil {
		t.Fatalf("health-alert: %v", err)
	}
	if len(health.Triggers) != 1 || health.Triggers[0].Kind != TriggerManual {
		t.Fatalf("health-alert triggers = %+v, want one manual trigger", health.Triggers)
	}
	sched, err := byID["scheduled-operational-report"].InstantiateDefinition(map[string]string{"budget_key": "team", "threshold_usd": "10"}, true)
	if err != nil {
		t.Fatalf("scheduled-report: %v", err)
	}
	if len(sched.Triggers) != 1 || sched.Triggers[0].Kind != TriggerSchedule {
		t.Fatalf("scheduled-report triggers = %+v, want one schedule trigger", sched.Triggers)
	}
	for _, n := range sched.Nodes {
		if n.NodeID == "nexaroute.cost.budget_check" {
			if v, ok := n.Input["threshold_usd"].(float64); !ok || v != 10 {
				t.Fatalf("threshold_usd = %#v, want float64(10) after coercion", n.Input["threshold_usd"])
			}
		}
	}
	video := byID["video-generation-pipeline"]
	if _, err := video.InstantiateDefinition(map[string]string{"prompt": "a cat"}, false); err == nil {
		t.Fatal("policy template instantiated without approval, want error")
	}
	if _, err := video.InstantiateDefinition(map[string]string{"prompt": "a cat"}, true); err != nil {
		t.Fatalf("policy template with approval failed: %v", err)
	}
}

func TestTemplatesInstantiateDefinitionFailures(t *testing.T) {
	byID := map[string]Template{}
	for _, tpl := range DefaultTemplates() {
		byID[tpl.ID] = tpl
	}
	t.Run("missing placeholder", func(t *testing.T) {
		if _, err := byID["model-health-alert"].InstantiateDefinition(map[string]string{}, true); err == nil {
			t.Fatal("instantiated with a missing placeholder, want error")
		}
	})
	t.Run("unresolved embedded placeholder", func(t *testing.T) {
		tpl := Template{ID: "bad-url", Version: SupportedVersion, Title: "bad",
			Nodes: []TemplateNode{
				{NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
				{NodeID: "nexaroute.http.request", Version: SupportedVersion, Input: map[string]any{"url": "https://example.invalid/{{missing}}"}},
			},
			Edges: [][2]string{{"nexaroute.trigger.manual", "nexaroute.http.request"}},
		}
		if _, err := tpl.InstantiateDefinition(map[string]string{}, true); err == nil {
			t.Fatal("instantiated with an unresolved embedded placeholder, want error")
		}
	})
	t.Run("duplicate node", func(t *testing.T) {
		tpl := Template{ID: "dup", Version: SupportedVersion, Title: "dup",
			Nodes: []TemplateNode{
				{NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
				{NodeID: "nexaroute.cost.usage", Version: SupportedVersion},
				{NodeID: "nexaroute.cost.usage", Version: SupportedVersion},
			},
			Edges: [][2]string{{"nexaroute.trigger.manual", "nexaroute.cost.usage"}},
		}
		if _, err := tpl.InstantiateDefinition(map[string]string{}, true); err == nil || !strings.Contains(err.Error(), "duplicate node") {
			t.Fatalf("err = %v, want duplicate node error", err)
		}
	})
	t.Run("unknown node", func(t *testing.T) {
		tpl := Template{ID: "nope", Version: SupportedVersion, Title: "nope",
			Nodes: []TemplateNode{{NodeID: "nexaroute.nope", Version: SupportedVersion}},
		}
		if _, err := tpl.InstantiateDefinition(map[string]string{}, true); err == nil {
			t.Fatal("instantiated with an unknown node, want error")
		}
	})
	t.Run("missing required input", func(t *testing.T) {
		tpl := Template{ID: "missing-input", Version: SupportedVersion, Title: "missing",
			Nodes: []TemplateNode{{NodeID: "nexaroute.provider.health", Version: SupportedVersion}},
		}
		_, err := tpl.InstantiateDefinition(map[string]string{}, true)
		if err == nil || !strings.Contains(err.Error(), "deployment_id") {
			t.Fatalf("err = %v, want missing required input deployment_id", err)
		}
	})
	t.Run("uncoercible number", func(t *testing.T) {
		tpl := byID["scheduled-operational-report"]
		if _, err := tpl.InstantiateDefinition(map[string]string{"budget_key": "team", "threshold_usd": "abc"}, true); err == nil || !strings.Contains(err.Error(), "threshold_usd") {
			t.Fatalf("err = %v, want threshold_usd coercion error", err)
		}
	})
	t.Run("approval node with input mapping", func(t *testing.T) {
		tpl := Template{ID: "approval-input", Version: SupportedVersion, Title: "approval",
			Nodes: []TemplateNode{
				{NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
				{NodeID: "nexaroute.control.approval", Version: SupportedVersion, Input: map[string]any{"decision": "approved"}},
			},
			Edges: [][2]string{{"nexaroute.trigger.manual", "nexaroute.control.approval"}},
		}
		if _, err := tpl.InstantiateDefinition(map[string]string{}, true); err == nil {
			t.Fatal("approval node with an input mapping instantiated, want error")
		}
	})
	t.Run("instantiation does not mutate the template", func(t *testing.T) {
		tpl := byID["scheduled-operational-report"]
		if _, err := tpl.InstantiateDefinition(map[string]string{"budget_key": "team", "threshold_usd": "10"}, true); err != nil {
			t.Fatalf("first instantiation: %v", err)
		}
		for _, node := range tpl.Nodes {
			if node.NodeID == "nexaroute.cost.budget_check" {
				if v, ok := node.Input["threshold_usd"].(string); !ok || v != "{{threshold_usd}}" {
					t.Fatalf("template mutated: threshold_usd = %#v, want {{threshold_usd}}", node.Input["threshold_usd"])
				}
			}
		}
	})
}

// The health-alert template is the strongest instantiation proof: its
// condition node maps its whole input from the predecessor's whole
// output ({"$ref": "health"}), and the instantiated definition must
// not only validate but execute end to end through the durable engine.
func TestTemplatesHealthAlertExecutesEndToEnd(t *testing.T) {
	var tpl Template
	for _, cand := range DefaultTemplates() {
		if cand.ID == "model-health-alert" {
			tpl = cand
		}
	}
	def, err := tpl.InstantiateDefinition(map[string]string{"deployment_id": "prov/model"}, true)
	if err != nil {
		t.Fatalf("InstantiateDefinition: %v", err)
	}
	store := NewMemoryStore()
	backend := newCountingBackend()
	eng := mustEngine(t, store, DefaultRegistry(), func(o *EngineOptions) { o.Backend = backend })
	run, err := eng.CreateRun(context.Background(), publishDef(t, store, *def), reconcileOwner(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	final, err := eng.Execute(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if final.State != RunSucceeded {
		t.Fatalf("run state = %q, want succeeded", final.State)
	}
	states := nodeStates(t, store, run.ID)
	if states["condition"].Output["result"] != true {
		t.Fatalf("condition output = %+v, want result true", states["condition"].Output)
	}
	if backend.calls["ProviderHealth"] != 1 || backend.calls["Compat"] != 1 {
		t.Fatalf("backend calls = %+v, want ProviderHealth 1 and Compat 1", backend.calls)
	}
}
