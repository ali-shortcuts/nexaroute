package nexaflow

import (
	"strings"
	"testing"
)

func TestTemplatesInert(t *testing.T) {
	templates := DefaultTemplates()
	if len(templates) != 5 {
		t.Fatalf("template count = %d, want 5", len(templates))
	}
	reg := DefaultRegistry()
	for _, tpl := range templates {
		if tpl.Published {
			t.Fatalf("template %q is published at definition time", tpl.ID)
		}
		for _, node := range tpl.Nodes {
			if _, err := reg.Lookup(node.NodeID, node.Version); err != nil {
				t.Fatalf("template %q references unknown node %q: %v", tpl.ID, node.NodeID, err)
			}
			for _, v := range node.Config {
				if s, ok := v.(string); ok && strings.Contains(s, "http://") {
					t.Fatalf("template %q config contains a real URL: %q", tpl.ID, s)
				}
			}
		}
		// Without placeholders: must fail.
		if _, err := tpl.Instantiate(map[string]string{}, false); err == nil {
			t.Fatalf("template %q instantiated without placeholders", tpl.ID)
		}
		// Without policy when required: must fail even with placeholders.
		filled := map[string]string{}
		for _, name := range tpl.Placeholders {
			filled[name] = "placeholder-value"
		}
		if tpl.RequiresPolicy {
			if _, err := tpl.Instantiate(filled, false); err == nil {
				t.Fatalf("policy template %q instantiated without approval", tpl.ID)
			}
		}
		// Filled + approved (when required): succeeds, stays unpublished,
		// and still does not execute anything (no Run method exists;
		// the assertion is that Instantiate returns a definition only).
		inst, err := tpl.Instantiate(filled, true)
		if err != nil {
			t.Fatalf("template %q filled instantiation failed: %v", tpl.ID, err)
		}
		if inst.Published {
			t.Fatalf("template %q became published on instantiation", tpl.ID)
		}
		if len(inst.Nodes) != len(tpl.Nodes) {
			t.Fatalf("template %q node count changed on instantiation", tpl.ID)
		}
	}
}

func TestDirectBackends(t *testing.T) {
	// Direct health/compat/cost adapters against real in-process stores,
	// with no HTTP and no duplication of the subsystems.
	healthBackend := &DirectHealthCompatBackend{}
	if _, err := healthBackend.ProviderHealth(t.Context(), "x"); err == nil {
		t.Fatal("unwired health backend accepted")
	}
	costBackend := &DirectCostBackend{}
	if _, err := costBackend.UsageSnapshot(t.Context()); err == nil {
		t.Fatal("unwired cost backend accepted")
	}
	if _, err := costBackend.BudgetSnapshot(t.Context(), "k"); err == nil {
		t.Fatal("unwired ledger backend accepted")
	}
}
