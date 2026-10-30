package nexaflow

import (
	"context"
	"strings"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

func studioTestService(t *testing.T) *Service {
	t.Helper()
	store := NewMemoryStore()
	reg := DefaultRegistry()
	engine, err := NewEngine(EngineOptions{Store: store, Registry: reg, Backend: nil, WorkerID: "studio-test"})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	svc, err := NewService(store, engine, reg, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// Instantiate every shipped template: policy-gated templates fail
// without approval and succeed with it, and every instantiated draft
// is valid, retrievable, and carries trigger declarations for its
// trigger nodes.
func TestStudioInstantiateAllTemplates(t *testing.T) {
	svc := studioTestService(t)
	ctx := context.Background()
	for _, tpl := range DefaultTemplates() {
		placeholders := make(map[string]string, len(tpl.Placeholders))
		for _, name := range tpl.Placeholders {
			placeholders[name] = "test-" + name
		}
		if tpl.RequiresPolicy {
			if _, err := svc.InstantiateTemplate(ctx, tpl.ID, placeholders, false); err == nil {
				t.Fatalf("template %q: instantiate without policy approval succeeded", tpl.ID)
			}
		}
		def, err := svc.InstantiateTemplate(ctx, tpl.ID, placeholders, true)
		if err != nil {
			t.Fatalf("template %q: instantiate: %v", tpl.ID, err)
		}
		if !strings.HasPrefix(def.ID, "tpl-"+tpl.ID+"-") {
			t.Fatalf("template %q: draft id %q lacks tpl- prefix", tpl.ID, def.ID)
		}
		if err := ValidateDefinition(&def, svc.Registry); err != nil {
			t.Fatalf("template %q: instantiated draft invalid: %v", tpl.ID, err)
		}
		got, err := svc.GetDraft(ctx, def.ID)
		if err != nil {
			t.Fatalf("template %q: GetDraft: %v", tpl.ID, err)
		}
		// Trigger-less templates gain exactly one injected manual
		// trigger (entry-point completion); templates that already
		// carry a trigger gain none.
		wantNodes := len(tpl.Nodes)
		hasTrigger := false
		for _, n := range tpl.Nodes {
			spec, err := svc.Registry.Lookup(n.NodeID, n.Version)
			if err != nil {
				t.Fatalf("template %q: lookup %q: %v", tpl.ID, n.NodeID, err)
			}
			if spec.Category == "trigger" {
				hasTrigger = true
			}
		}
		if !hasTrigger {
			wantNodes++
		}
		if len(got.Nodes) != wantNodes {
			t.Fatalf("template %q: nodes = %d, want %d", tpl.ID, len(got.Nodes), wantNodes)
		}
		// Two instantiations never collide.
		again, err := svc.InstantiateTemplate(ctx, tpl.ID, placeholders, true)
		if err != nil {
			t.Fatalf("template %q: second instantiate: %v", tpl.ID, err)
		}
		if again.ID == def.ID {
			t.Fatalf("template %q: second instantiate reused id %q", tpl.ID, def.ID)
		}
	}
}

func TestStudioInstantiateErrors(t *testing.T) {
	svc := studioTestService(t)
	ctx := context.Background()
	if _, err := svc.InstantiateTemplate(ctx, "no-such-template", nil, true); err == nil {
		t.Fatal("unknown template: expected error")
	} else if err != ErrNotFound {
		t.Fatalf("unknown template: err = %v, want ErrNotFound", err)
	}
	// A declared placeholder left unfilled fails, never a draft with
	// literal "{{...}}" text.
	if _, err := svc.InstantiateTemplate(ctx, "model-compat-evaluation", map[string]string{"deployment_id": "d1"}, true); err == nil {
		t.Fatal("missing placeholder: expected error")
	}
}

func TestStudioSecretRefs(t *testing.T) {
	reg := DefaultRegistry()
	def := &Definition{
		SchemaVersion: DefinitionSchemaVersion,
		ID:            "secrets", Name: "secrets",
		Nodes: []DefinitionNode{
			{ID: "hook", NodeID: "nexaroute.trigger.webhook", Version: SupportedVersion,
				Config: map[string]any{"secret_ref": "env:WEBHOOK_SECRET"}},
			{ID: "call", NodeID: "nexaroute.http.request", Version: SupportedVersion,
				Config: map[string]any{"auth_secret_ref": "env:API_TOKEN"}},
		},
		Edges: []DefinitionEdge{{ID: "e1", From: "hook", To: "call"}},
		Triggers: []DefinitionTrigger{
			{Name: "hook", Kind: TriggerWebhook, Config: map[string]any{"secret_ref": "env:WEBHOOK_SECRET"}},
		},
	}
	refs := DefinitionSecretRefs(def, reg)
	if len(refs) != 2 || refs[0] != "env:API_TOKEN" || refs[1] != "env:WEBHOOK_SECRET" {
		t.Fatalf("secret refs = %v, want [env:API_TOKEN env:WEBHOOK_SECRET]", refs)
	}
	// Values, not references, are never reported: a node config value
	// on a non-secretRef field is not a ref.
	def.Nodes[1].Config["url"] = "https://example.invalid/x"
	if refs := DefinitionSecretRefs(def, reg); len(refs) != 2 {
		t.Fatalf("after adding url: refs = %v, want 2", refs)
	}
	if refs := DefinitionSecretRefs(nil, reg); len(refs) != 0 {
		t.Fatalf("nil definition: refs = %v, want none", refs)
	}
}

// Distinct-approver success at the service level: the browser can only
// ever present the single admin-key identity (which the existing HTTP
// test proves is rejected as self-approval), so the positive path is
// pinned here with two principals.
func TestStudioDistinctApproverSucceeds(t *testing.T) {
	svc := studioTestService(t)
	ctx := context.Background()
	def := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "appr-distinct", Name: "appr-distinct",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "appr", NodeID: "nexaroute.control.approval", Version: SupportedVersion},
		},
		Edges:    []DefinitionEdge{{ID: "e1", From: "start", To: "appr"}},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual}},
	}
	if err := svc.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	alice := authz.Identity{Subject: "alice"}
	pub, err := svc.Publish(ctx, def.ID, alice)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	run, err := svc.StartRun(ctx, pub, alice, map[string]any{}, "distinct-1")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	run, err = svc.ExecuteRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("ExecuteRun: %v", err)
	}
	if run.State != RunWaitingForApproval {
		t.Fatalf("run state = %q, want waiting_for_approval", run.State)
	}
	if _, err := svc.ApproveRun(ctx, run.ID, "appr", alice, "approved"); err == nil {
		t.Fatal("self-approval succeeded, want rejection")
	}
	bob := authz.Identity{Subject: "bob"}
	if _, err := svc.ApproveRun(ctx, run.ID, "appr", bob, "approved"); err != nil {
		t.Fatalf("distinct approver ApproveRun: %v", err)
	}
	run, err = svc.ExecuteRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("ExecuteRun after approval: %v", err)
	}
	if run.State != RunSucceeded {
		t.Fatalf("run state after distinct approval = %q, want succeeded", run.State)
	}
}
