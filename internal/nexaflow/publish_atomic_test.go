package nexaflow

import (
	"context"
	"errors"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

// Force a draft replacement exactly after the service reads it and before it
// tries to publish. The replacement deliberately bypasses the service's save
// validation, simulating a concurrent store writer or another registry.
type replaceDraftAfterRead struct {
	Store
	replacement Definition
}

func (s *replaceDraftAfterRead) GetDraft(ctx context.Context, id string) (Definition, error) {
	draft, err := s.Store.GetDraft(ctx, id)
	if err == nil {
		err = s.Store.SaveDraft(ctx, s.replacement)
	}
	return draft, err
}

func (s *replaceDraftAfterRead) PublishIfUnchanged(ctx context.Context, id, by, hash string) (PublishedDefinition, error) {
	return s.Store.(ConditionalPublisher).PublishIfUnchanged(ctx, id, by, hash)
}

func TestServicePublishRejectsDraftChangedAfterValidation(t *testing.T) {
	ctx := context.Background()
	base := NewMemoryStore()
	valid := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "atomic-publish", Name: "valid",
		Nodes:    []DefinitionNode{{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion}},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	if err := base.SaveDraft(ctx, valid); err != nil {
		t.Fatal(err)
	}
	invalid := valid
	invalid.Name = "" // invalid replacement must not be published
	store := &replaceDraftAfterRead{Store: base, replacement: invalid}
	service := &Service{Store: store, Registry: DefaultRegistry()}
	_, err := service.Publish(ctx, valid.ID, authz.Identity{Subject: "owner", Roles: []authz.Role{authz.RoleOwner}})
	if !errors.Is(err, ErrStateConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	if revisions, err := base.ListRevisions(ctx, valid.ID); err != nil || len(revisions) != 0 {
		t.Fatalf("changed draft was published: %+v, %v", revisions, err)
	}
}

// Embedding the old Store interface intentionally does not expose the new
// conditional capability. Service must refuse rather than publish unchecked.
type uncheckedPublishStore struct{ Store }

func TestServicePublishRequiresAtomicStore(t *testing.T) {
	ctx := context.Background()
	base := NewMemoryStore()
	valid := Definition{
		SchemaVersion: DefinitionSchemaVersion, ID: "unchecked", Name: "valid",
		Nodes:    []DefinitionNode{{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion}},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
	if err := base.SaveDraft(ctx, valid); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: uncheckedPublishStore{base}, Registry: DefaultRegistry()}
	_, err := service.Publish(ctx, valid.ID, authz.Identity{Subject: "owner", Roles: []authz.Role{authz.RoleOwner}})
	if err == nil {
		t.Fatal("unchecked store was allowed to publish")
	}
	if revisions, err := base.ListRevisions(ctx, valid.ID); err != nil || len(revisions) != 0 {
		t.Fatalf("unchecked store published: %+v, %v", revisions, err)
	}
}
