//go:build integration

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	"github.com/ali-shortcuts/nexaroute/internal/nexaflow"
)

// This wrapper makes a second independent PostgreSQL pool replace the draft
// between the service read and conditional publish, without scheduler timing.
type concurrentDraftStore struct {
	nexaflow.Store
	other       nexaflow.Store
	replacement nexaflow.Definition
}

func (s *concurrentDraftStore) GetDraft(ctx context.Context, id string) (nexaflow.Definition, error) {
	draft, err := s.Store.GetDraft(ctx, id)
	if err == nil {
		err = s.other.SaveDraft(ctx, s.replacement)
	}
	return draft, err
}

func (s *concurrentDraftStore) PublishIfUnchanged(ctx context.Context, id, by, hash string) (nexaflow.PublishedDefinition, error) {
	return s.Store.(nexaflow.ConditionalPublisher).PublishIfUnchanged(ctx, id, by, hash)
}

// The existing CI PostgreSQL gateway-integration step selects this prefix.
// This is a real database test, not a substitute for multi-host acceptance.
func TestConcurrentPostgresControlPlaneBootstrapConverges_NexaFlowPublishCAS(t *testing.T) {
	dsn := os.Getenv("NEXAROUTE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("NEXAROUTE_TEST_POSTGRES_DSN not set; PostgreSQL integration not run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db1, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("could not open first PostgreSQL pool")
	}
	defer db1.Close()
	db2, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("could not open second PostgreSQL pool")
	}
	defer db2.Close()
	first, err := nexaflow.NewPostgresStore(ctx, db1)
	if err != nil {
		t.Fatalf("initialize first workflow store: %v", err)
	}
	second, err := nexaflow.NewPostgresStore(ctx, db2)
	if err != nil {
		t.Fatalf("initialize second workflow store: %v", err)
	}
	id := fmt.Sprintf("cas-integration-%d", time.Now().UnixNano())
	defer func() {
		// Delete only this test's records; never drop shared tables.
		_, _ = db1.Exec(`DELETE FROM nexaroute_workflow_revisions WHERE definition_id = $1`, id)
		_, _ = db1.Exec(`DELETE FROM nexaroute_workflow_definitions WHERE definition_id = $1`, id)
	}()
	valid := nexaflow.Definition{
		SchemaVersion: nexaflow.DefinitionSchemaVersion, ID: id, Name: "valid",
		Nodes:    []nexaflow.DefinitionNode{{ID: "start", NodeID: "nexaroute.trigger.manual", Version: nexaflow.SupportedVersion}},
		Triggers: []nexaflow.DefinitionTrigger{{Name: "start", Kind: nexaflow.TriggerManual, Config: map[string]any{}}},
	}
	if err := first.SaveDraft(ctx, valid); err != nil {
		t.Fatalf("save first draft: %v", err)
	}
	replacement := valid
	replacement.Name = "" // invalid if published without the service's validation
	store := &concurrentDraftStore{Store: first, other: second, replacement: replacement}
	service := &nexaflow.Service{Store: store, Registry: nexaflow.DefaultRegistry()}
	_, err = service.Publish(ctx, id, authz.Identity{Subject: "owner", Roles: []authz.Role{authz.RoleOwner}})
	if !errors.Is(err, nexaflow.ErrStateConflict) {
		t.Fatalf("publish after concurrent draft replacement: got %v, want conflict", err)
	}
	if revisions, err := first.ListRevisions(ctx, id); err != nil || len(revisions) != 0 {
		t.Fatalf("changed draft was published: %+v, %v", revisions, err)
	}
	if err := first.SaveDraft(ctx, valid); err != nil {
		t.Fatalf("restore valid draft: %v", err)
	}
	service.Store = first
	if _, err := service.Publish(ctx, id, authz.Identity{Subject: "owner", Roles: []authz.Role{authz.RoleOwner}}); err != nil {
		t.Fatalf("matching draft must publish: %v", err)
	}
}
