package nexaflow

import (
	"context"
	"math"
	"strings"
	"testing"
)

// A nil DB makes any accidental database access panic. Unsupported JSON must
// be rejected before opening a transaction or writing a fallback object.
func TestPostgresStoreRejectsUnencodableStateBeforeDatabaseAccess(t *testing.T) {
	store := &PostgresStore{}
	ctx := context.Background()
	bad := map[string]any{"bad": math.NaN()}
	def := Definition{ID: "wf", Nodes: []DefinitionNode{{ID: "n", Config: bad}}}
	checks := map[string]func() error{
		"draft":          func() error { return store.SaveDraft(ctx, def) },
		"run-definition": func() error { return store.CreateRun(ctx, Run{ID: "r", Definition: def}) },
		"run-input":      func() error { return store.CreateRun(ctx, Run{ID: "r", Input: bad}) },
		"append-node":    func() error { _, err := store.AppendNodeExecution(ctx, NodeExecution{Input: bad}); return err },
		"save-node":      func() error { _, err := store.SaveNodeExecution(ctx, NodeExecution{Output: bad}, ""); return err },
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			if err := check(); err == nil || !strings.Contains(err.Error(), "encode postgres store JSON") {
				t.Fatalf("got %v, want encoding error", err)
			}
		})
	}
}
