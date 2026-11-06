package nexaflow

// Scenario tests for PostgresStore and the Postgres delivery ledger
// over the fake database/sql driver (agentb_nexaflow_pg_fake_test.go).
// Every method's happy path runs through the fake (the canonical local
// profile has no live PostgreSQL), plus each error branch via the
// fake's injection hooks. Real disposable PostgreSQL runs are separate
// verification, not part of these tests.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
)

func agentBPGTestDef(id string) Definition {
	return Definition{
		SchemaVersion: DefinitionSchemaVersion,
		ID:            id,
		Name:          "def " + id,
		Nodes: []DefinitionNode{{
			ID:      "n1",
			NodeID:  "nexaroute.trigger.manual",
			Version: SupportedVersion,
			Config:  map[string]any{"x": float64(1)},
		}},
		Triggers: []DefinitionTrigger{{Name: "n1", Kind: TriggerManual}},
	}
}

// agentBPGRegisterFake registers the fake driver under a unique name
// and returns the name, so OpenPostgresStore can be pointed at it via
// pgxDriverName.
func agentBPGRegisterFake(t *testing.T, db *agentBPGFakeDB) string {
	t.Helper()
	name := fmt.Sprintf("agentb-fake-pg-nexaflow-%d", agentBPGDriverSeq.Add(1))
	sql.Register(name, &agentBPGFakeDriver{db: db})
	return name
}

func agentBPGOpenStore(t *testing.T, db *agentBPGFakeDB) *PostgresStore {
	t.Helper()
	sqlDB := agentBOpenFakePG(t, db)
	s, err := NewPostgresStore(context.Background(), sqlDB)
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	return s
}

func agentBPGMustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return raw
}

func agentBPGTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	v := t
	return &v
}

// agentBPGSeedRun stores a Run-shaped row; def/roles/input are
// marshaled from the Run (seed the agentBPGRun directly for corrupt
// payloads).
func agentBPGSeedRun(t *testing.T, db *agentBPGFakeDB, run Run) {
	t.Helper()
	db.seedRun(&agentBPGRun{
		id: run.ID, defID: run.DefinitionID, revision: int64(run.Revision),
		def: agentBPGMustJSON(t, run.Definition), state: string(run.State),
		idem: run.IdempotencyKey, principal: run.Principal,
		roles: agentBPGMustJSON(t, run.PrincipalRoles), input: agentBPGMustJSON(t, run.Input),
		errCode: run.ErrorCode, errMsg: run.Error,
		createdAt: run.CreatedAt, updatedAt: run.UpdatedAt,
		startedAt: agentBPGTimePtr(run.StartedAt), finishedAt: agentBPGTimePtr(run.FinishedAt),
		leaseOwner: run.LeaseOwner, leaseUntil: agentBPGTimePtr(run.LeaseUntil),
	})
}

func agentBPGSeedNode(t *testing.T, db *agentBPGFakeDB, e NodeExecution) {
	t.Helper()
	db.seedNode(&agentBPGNode{
		runID: e.RunID, nodeID: e.NodeID, attempt: int64(e.Attempt),
		payload: agentBPGMustJSON(t, e), state: string(e.State),
		leaseOwner: e.LeaseOwner, leaseUntil: agentBPGTimePtr(e.LeaseUntil),
	})
}

func agentBPGExpectErr(t *testing.T, err error, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error, got nil")
	}
	if want != nil && !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestAgentBPGOpen(t *testing.T) {
	ctx := context.Background()

	t.Run("unknown driver", func(t *testing.T) {
		old := pgxDriverName
		pgxDriverName = "agentb-no-such-driver"
		defer func() { pgxDriverName = old }()
		_, err := OpenPostgresStore(ctx, "postgres://localhost/db")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("ping fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		db.failPing = true
		old := pgxDriverName
		pgxDriverName = agentBPGRegisterFake(t, db)
		defer func() { pgxDriverName = old }()
		_, err := OpenPostgresStore(ctx, "")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("migrate begin fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		db.failBegin = true
		old := pgxDriverName
		pgxDriverName = agentBPGRegisterFake(t, db)
		defer func() { pgxDriverName = old }()
		_, err := OpenPostgresStore(ctx, "")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("success", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		old := pgxDriverName
		pgxDriverName = agentBPGRegisterFake(t, db)
		defer func() { pgxDriverName = old }()
		s, err := OpenPostgresStore(ctx, "")
		if err != nil {
			t.Fatalf("OpenPostgresStore: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

func TestAgentBPGNewStore(t *testing.T) {
	ctx := context.Background()

	t.Run("begin fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		db.failBegin = true
		sqlDB := agentBOpenFakePG(t, db)
		_, err := NewPostgresStore(ctx, sqlDB)
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("success", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

func TestAgentBPGMigrateErrors(t *testing.T) {
	ctx := context.Background()

	t.Run("advisory lock fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		db.failExec = "pg_advisory_xact_lock"
		sqlDB := agentBOpenFakePG(t, db)
		_, err := NewPostgresStore(ctx, sqlDB)
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("create table fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		db.failExec = "CREATE TABLE"
		sqlDB := agentBOpenFakePG(t, db)
		_, err := NewPostgresStore(ctx, sqlDB)
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("commit fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		db.failCommit = true
		sqlDB := agentBOpenFakePG(t, db)
		_, err := NewPostgresStore(ctx, sqlDB)
		agentBPGExpectErr(t, err, nil)
	})
}

func TestAgentBPGSaveGetDraft(t *testing.T) {
	ctx := context.Background()

	t.Run("empty id", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		agentBPGExpectErr(t, s.SaveDraft(ctx, Definition{}), nil)
	})

	t.Run("NaN config encode fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		def := agentBPGTestDef("d1")
		def.Nodes[0].Config = map[string]any{"x": math.NaN()}
		agentBPGExpectErr(t, s.SaveDraft(ctx, def), nil)
	})

	t.Run("round trip and upsert", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		def := agentBPGTestDef("d1")
		if err := s.SaveDraft(ctx, def); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		got, err := s.GetDraft(ctx, "d1")
		if err != nil {
			t.Fatalf("GetDraft: %v", err)
		}
		if got.ID != "d1" || got.Name != def.Name || len(got.Nodes) != 1 || got.Nodes[0].NodeID != "nexaroute.trigger.manual" {
			t.Fatalf("GetDraft = %+v, want id d1 with one manual trigger node", got)
		}
		def.Name = "renamed"
		if err := s.SaveDraft(ctx, def); err != nil {
			t.Fatalf("SaveDraft upsert: %v", err)
		}
		got, err = s.GetDraft(ctx, "d1")
		if err != nil {
			t.Fatalf("GetDraft after upsert: %v", err)
		}
		if got.Name != "renamed" {
			t.Fatalf("GetDraft after upsert name = %q, want renamed", got.Name)
		}
	})

	t.Run("missing draft", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		_, err := s.GetDraft(ctx, "nope")
		agentBPGExpectErr(t, err, ErrNotFound)
	})

	t.Run("query fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failQuery = "SELECT draft"
		_, err := s.GetDraft(ctx, "d1")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("garbage draft", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		db.seedDraft("d1", []byte("{not json"))
		s := agentBPGOpenStore(t, db)
		_, err := s.GetDraft(ctx, "d1")
		agentBPGExpectErr(t, err, nil)
	})
}

func TestAgentBPGListDrafts(t *testing.T) {
	ctx := context.Background()

	t.Run("empty", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		got, err := s.ListDrafts(ctx)
		if err != nil {
			t.Fatalf("ListDrafts: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("ListDrafts = %d drafts, want 0", len(got))
		}
	})

	t.Run("sorted", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		for _, id := range []string{"d2", "d1"} {
			if err := s.SaveDraft(ctx, agentBPGTestDef(id)); err != nil {
				t.Fatalf("SaveDraft %s: %v", id, err)
			}
		}
		got, err := s.ListDrafts(ctx)
		if err != nil {
			t.Fatalf("ListDrafts: %v", err)
		}
		if len(got) != 2 || got[0].ID != "d1" || got[1].ID != "d2" {
			t.Fatalf("ListDrafts ids = %v, want [d1 d2]", got)
		}
	})

	t.Run("query fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failQuery = "SELECT draft"
		_, err := s.ListDrafts(ctx)
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("garbage draft", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		db.seedDraft("d1", []byte("{not json"))
		s := agentBPGOpenStore(t, db)
		_, err := s.ListDrafts(ctx)
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("scan fails on column count", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		if err := s.SaveDraft(ctx, agentBPGTestDef("d1")); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		db.badListDraftsCols = true
		_, err := s.ListDrafts(ctx)
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("rows error", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		if err := s.SaveDraft(ctx, agentBPGTestDef("d1")); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		db.rowsErrQuery = "SELECT draft"
		_, err := s.ListDrafts(ctx)
		agentBPGExpectErr(t, err, nil)
	})
}

func TestAgentBPGPublish(t *testing.T) {
	ctx := context.Background()

	t.Run("empty expected hash conflicts", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		_, err := s.PublishIfUnchanged(ctx, "d1", "op", "")
		agentBPGExpectErr(t, err, ErrStateConflict)
	})

	t.Run("missing draft", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		_, err := s.Publish(ctx, "nope", "op")
		agentBPGExpectErr(t, err, ErrNotFound)
	})

	t.Run("begin fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failBegin = true
		_, err := s.Publish(ctx, "d1", "op")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("draft query fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		if err := s.SaveDraft(ctx, agentBPGTestDef("d1")); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		db.failQuery = "FOR UPDATE"
		_, err := s.Publish(ctx, "d1", "op")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("garbage draft", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		db.seedDraft("d1", []byte("{not json"))
		s := agentBPGOpenStore(t, db)
		_, err := s.Publish(ctx, "d1", "op")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("wrong hash conflicts", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		if err := s.SaveDraft(ctx, agentBPGTestDef("d1")); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		_, err := s.PublishIfUnchanged(ctx, "d1", "op", "deadbeef")
		agentBPGExpectErr(t, err, ErrStateConflict)
	})

	t.Run("last revision query fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		if err := s.SaveDraft(ctx, agentBPGTestDef("d1")); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		db.failQuery = "ORDER BY revision DESC LIMIT 1"
		_, err := s.Publish(ctx, "d1", "op")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("negative stored revision fails closed", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		def := agentBPGTestDef("d1")
		if err := s.SaveDraft(ctx, def); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		hash, _ := ContentHash(&def)
		db.seedRevision("d1", -1, hash, string(DefinitionPublished), "op", agentBPGMustJSON(t, def), time.Now())
		_, err := s.Publish(ctx, "d1", "op")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("idempotent path garbage revision", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		def := agentBPGTestDef("d1")
		if err := s.SaveDraft(ctx, def); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		hash, _ := ContentHash(&def)
		db.seedRevision("d1", 1, hash, string(DefinitionPublished), "op", []byte("{not json"), time.Now())
		_, err := s.Publish(ctx, "d1", "op")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("idempotent republish", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		if err := s.SaveDraft(ctx, agentBPGTestDef("d1")); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		first, err := s.Publish(ctx, "d1", "op")
		if err != nil {
			t.Fatalf("Publish: %v", err)
		}
		if first.Version.Revision != 1 {
			t.Fatalf("first publish revision = %d, want 1", first.Version.Revision)
		}
		again, err := s.Publish(ctx, "d1", "op")
		if err != nil {
			t.Fatalf("republish: %v", err)
		}
		if again.Version.Revision != 1 || again.Version.ContentHash != first.Version.ContentHash {
			t.Fatalf("republish = rev %d hash %q, want rev 1 hash %q", again.Version.Revision, again.Version.ContentHash, first.Version.ContentHash)
		}
	})

	t.Run("idempotent republish commit fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		if err := s.SaveDraft(ctx, agentBPGTestDef("d1")); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		if _, err := s.Publish(ctx, "d1", "op"); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		db.failCommit = true
		_, err := s.Publish(ctx, "d1", "op")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("supersede update fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		if err := s.SaveDraft(ctx, agentBPGTestDef("d1")); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		db.failExec = "WHERE definition_id = $2 AND status = $3"
		_, err := s.Publish(ctx, "d1", "op")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("revision insert fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		if err := s.SaveDraft(ctx, agentBPGTestDef("d1")); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		db.failExec = "INSERT INTO nexaroute_workflow_revisions"
		_, err := s.Publish(ctx, "d1", "op")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("commit fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		if err := s.SaveDraft(ctx, agentBPGTestDef("d1")); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		db.failCommit = true
		_, err := s.Publish(ctx, "d1", "op")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("second publish supersedes first", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		def := agentBPGTestDef("d1")
		if err := s.SaveDraft(ctx, def); err != nil {
			t.Fatalf("SaveDraft: %v", err)
		}
		if _, err := s.Publish(ctx, "d1", "op"); err != nil {
			t.Fatalf("Publish 1: %v", err)
		}
		def.Name = "v2"
		if err := s.SaveDraft(ctx, def); err != nil {
			t.Fatalf("SaveDraft v2: %v", err)
		}
		second, err := s.Publish(ctx, "d1", "op")
		if err != nil {
			t.Fatalf("Publish 2: %v", err)
		}
		if second.Version.Revision != 2 || second.Definition.Name != "v2" {
			t.Fatalf("second publish = rev %d name %q, want rev 2 name v2", second.Version.Revision, second.Definition.Name)
		}
		first, err := s.GetRevision(ctx, "d1", 1)
		if err != nil {
			t.Fatalf("GetRevision 1: %v", err)
		}
		if first.Version.Status != DefinitionSuperseded {
			t.Fatalf("revision 1 status = %q, want superseded", first.Version.Status)
		}
	})
}

func TestAgentBPGSupersedeRevision(t *testing.T) {
	ctx := context.Background()
	seedPublished := func(t *testing.T, db *agentBPGFakeDB) {
		t.Helper()
		def := agentBPGTestDef("d1")
		db.seedRevision("d1", 1, "hash1", string(DefinitionPublished), "op", agentBPGMustJSON(t, def), time.Now())
	}

	t.Run("begin fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failBegin = true
		_, err := s.SupersedeRevision(ctx, "d1", 1)
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("update fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		seedPublished(t, db)
		db.failExec = "AND revision = $3 AND status = $4"
		_, err := s.SupersedeRevision(ctx, "d1", 1)
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("rows affected fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		seedPublished(t, db)
		db.failRowsAffected = true
		_, err := s.SupersedeRevision(ctx, "d1", 1)
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("missing revision", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		_, err := s.SupersedeRevision(ctx, "d1", 9)
		agentBPGExpectErr(t, err, ErrNotFound)
	})

	t.Run("already superseded conflicts", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		def := agentBPGTestDef("d1")
		db.seedRevision("d1", 1, "hash1", string(DefinitionSuperseded), "op", agentBPGMustJSON(t, def), time.Now())
		_, err := s.SupersedeRevision(ctx, "d1", 1)
		agentBPGExpectErr(t, err, ErrStateConflict)
	})

	t.Run("exists query fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failQuery = "SELECT EXISTS"
		_, err := s.SupersedeRevision(ctx, "d1", 9)
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("commit fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		seedPublished(t, db)
		db.failCommit = true
		_, err := s.SupersedeRevision(ctx, "d1", 1)
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("success", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		seedPublished(t, db)
		got, err := s.SupersedeRevision(ctx, "d1", 1)
		if err != nil {
			t.Fatalf("SupersedeRevision: %v", err)
		}
		if got.Version.Status != DefinitionSuperseded || got.Version.Revision != 1 {
			t.Fatalf("SupersedeRevision = rev %d status %q, want rev 1 superseded", got.Version.Revision, got.Version.Status)
		}
	})
}

func TestAgentBPGGetRevision(t *testing.T) {
	ctx := context.Background()

	t.Run("missing", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		_, err := s.GetRevision(ctx, "d1", 1)
		agentBPGExpectErr(t, err, ErrNotFound)
	})

	t.Run("query fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failQuery = "SELECT content_hash"
		_, err := s.GetRevision(ctx, "d1", 1)
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("garbage definition", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		db.seedRevision("d1", 1, "h", string(DefinitionPublished), "op", []byte("{not json"), time.Now())
		s := agentBPGOpenStore(t, db)
		_, err := s.GetRevision(ctx, "d1", 1)
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("success", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		def := agentBPGTestDef("d1")
		db.seedRevision("d1", 3, "h3", string(DefinitionPublished), "op", agentBPGMustJSON(t, def), time.Now())
		got, err := s.GetRevision(ctx, "d1", 3)
		if err != nil {
			t.Fatalf("GetRevision: %v", err)
		}
		if got.Version.Revision != 3 || got.Definition.ID != "d1" || got.PublishedBy != "op" {
			t.Fatalf("GetRevision = %+v, want rev 3 def d1 by op", got)
		}
	})
}

func TestAgentBPGListRevisions(t *testing.T) {
	ctx := context.Background()

	t.Run("query fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failQuery = "ORDER BY revision"
		_, err := s.ListRevisions(ctx, "d1")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("scan fails on bad revision", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		def := agentBPGTestDef("d1")
		db.seedRevision("d1", 1, "h", string(DefinitionPublished), "op", agentBPGMustJSON(t, def), time.Now())
		db.badRevRevision["d1"] = true
		_, err := s.ListRevisions(ctx, "d1")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("negative revision fails closed", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		def := agentBPGTestDef("d1")
		db.seedRevision("d1", -1, "h", string(DefinitionPublished), "op", agentBPGMustJSON(t, def), time.Now())
		_, err := s.ListRevisions(ctx, "d1")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("garbage definition", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.seedRevision("d1", 1, "h", string(DefinitionPublished), "op", []byte("{not json"), time.Now())
		_, err := s.ListRevisions(ctx, "d1")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("rows error", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		def := agentBPGTestDef("d1")
		db.seedRevision("d1", 1, "h", string(DefinitionPublished), "op", agentBPGMustJSON(t, def), time.Now())
		db.rowsErrQuery = "ORDER BY revision"
		_, err := s.ListRevisions(ctx, "d1")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("success ordered", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		def := agentBPGTestDef("d1")
		db.seedRevision("d1", 2, "h2", string(DefinitionPublished), "op", agentBPGMustJSON(t, def), time.Now())
		db.seedRevision("d1", 1, "h1", string(DefinitionSuperseded), "op", agentBPGMustJSON(t, def), time.Now())
		got, err := s.ListRevisions(ctx, "d1")
		if err != nil {
			t.Fatalf("ListRevisions: %v", err)
		}
		if len(got) != 2 || got[0].Version.Revision != 1 || got[1].Version.Revision != 2 {
			t.Fatalf("ListRevisions revisions = %v, want [1 2]", got)
		}
	})
}

func TestAgentBPGCreateRun(t *testing.T) {
	ctx := context.Background()
	newRun := func(id string) Run {
		return Run{
			ID: "run-" + id, DefinitionID: "d1", Revision: 1,
			Definition: agentBPGTestDef("d1"), State: RunCreated,
			Principal: "alice", PrincipalRoles: []authz.Role{authz.RoleOwner},
			Input: map[string]any{"k": "v"},
		}
	}

	t.Run("empty id", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		agentBPGExpectErr(t, s.CreateRun(ctx, Run{}), nil)
	})

	t.Run("NaN definition encode fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		run := newRun("1")
		run.Definition.Nodes[0].Config = map[string]any{"x": math.NaN()}
		agentBPGExpectErr(t, s.CreateRun(ctx, run), nil)
	})

	t.Run("NaN input encode fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		run := newRun("1")
		run.Input = map[string]any{"x": math.NaN()}
		agentBPGExpectErr(t, s.CreateRun(ctx, run), nil)
	})

	t.Run("begin fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failBegin = true
		agentBPGExpectErr(t, s.CreateRun(ctx, newRun("1")), nil)
	})

	t.Run("cap lock fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		run := newRun("1")
		run.Definition.Settings.MaxConcurrentRuns = 1
		db.failExec = "pg_advisory_xact_lock(hashtext"
		agentBPGExpectErr(t, s.CreateRun(ctx, run), nil)
	})

	t.Run("cap count fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		run := newRun("1")
		run.Definition.Settings.MaxConcurrentRuns = 1
		db.failQuery = "SELECT COUNT(*)"
		agentBPGExpectErr(t, s.CreateRun(ctx, run), nil)
	})

	t.Run("cap reached", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		active := newRun("0")
		active.Definition.Settings.MaxConcurrentRuns = 1
		agentBPGSeedRun(t, db, active)
		run := newRun("1")
		run.Definition.Settings.MaxConcurrentRuns = 1
		agentBPGExpectErr(t, s.CreateRun(ctx, run), ErrRunLimit)
	})

	t.Run("insert fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failExec = "INSERT INTO nexaroute_workflow_runs"
		agentBPGExpectErr(t, s.CreateRun(ctx, newRun("1")), nil)
	})

	t.Run("duplicate idempotency key", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		first := newRun("1")
		first.IdempotencyKey = "idem-1"
		if err := s.CreateRun(ctx, first); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		second := newRun("2")
		second.IdempotencyKey = "idem-1"
		agentBPGExpectErr(t, s.CreateRun(ctx, second), ErrIdempotencyConflict)
	})

	t.Run("duplicate run id", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		if err := s.CreateRun(ctx, newRun("1")); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		agentBPGExpectErr(t, s.CreateRun(ctx, newRun("1")), ErrAlreadyExists)
	})

	t.Run("commit fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failCommit = true
		agentBPGExpectErr(t, s.CreateRun(ctx, newRun("1")), nil)
	})

	t.Run("success with optional times", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		now := time.Now()
		run := newRun("1")
		run.StartedAt = now.Add(-time.Minute)
		run.FinishedAt = now
		run.LeaseUntil = now.Add(time.Hour)
		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		got, err := s.GetRun(ctx, "run-1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.StartedAt.IsZero() || got.FinishedAt.IsZero() || got.LeaseUntil.IsZero() {
			t.Fatalf("GetRun times = started %v finished %v lease %v, want all set", got.StartedAt, got.FinishedAt, got.LeaseUntil)
		}
		if got.Principal != "alice" || len(got.PrincipalRoles) != 1 || got.PrincipalRoles[0] != authz.RoleOwner {
			t.Fatalf("GetRun principal = %q roles %v, want alice [owner]", got.Principal, got.PrincipalRoles)
		}
	})
}

func TestAgentBPGGetRun(t *testing.T) {
	ctx := context.Background()

	t.Run("missing", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		_, err := s.GetRun(ctx, "nope")
		agentBPGExpectErr(t, err, ErrNotFound)
	})

	t.Run("scan fails on bad revision", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		run := Run{ID: "r1", DefinitionID: "d1", Revision: 1, Definition: agentBPGTestDef("d1"), State: RunCreated, Input: map[string]any{}}
		agentBPGSeedRun(t, db, run)
		s := agentBPGOpenStore(t, db)
		db.badRunRevision["r1"] = true
		_, err := s.GetRun(ctx, "r1")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("garbage definition", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		db.seedRun(&agentBPGRun{id: "r1", defID: "d1", revision: 1, def: []byte("{bad"), state: "created", roles: []byte("[]"), input: []byte("{}")})
		s := agentBPGOpenStore(t, db)
		_, err := s.GetRun(ctx, "r1")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("garbage roles", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		db.seedRun(&agentBPGRun{id: "r1", defID: "d1", revision: 1, def: agentBPGMustJSON(t, agentBPGTestDef("d1")), state: "created", roles: []byte("{bad"), input: []byte("{}")})
		s := agentBPGOpenStore(t, db)
		_, err := s.GetRun(ctx, "r1")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("garbage input", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		db.seedRun(&agentBPGRun{id: "r1", defID: "d1", revision: 1, def: agentBPGMustJSON(t, agentBPGTestDef("d1")), state: "created", roles: []byte("[]"), input: []byte("{bad")})
		s := agentBPGOpenStore(t, db)
		_, err := s.GetRun(ctx, "r1")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("success without optional times", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		now := time.Now().Truncate(time.Microsecond)
		run := Run{ID: "r1", DefinitionID: "d1", Revision: 2, Definition: agentBPGTestDef("d1"), State: RunCreated, Input: map[string]any{"a": float64(1)}, CreatedAt: now, UpdatedAt: now}
		agentBPGSeedRun(t, db, run)
		s := agentBPGOpenStore(t, db)
		got, err := s.GetRun(ctx, "r1")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.ID != "r1" || got.Revision != 2 || got.Input["a"] != float64(1) {
			t.Fatalf("GetRun = %+v, want r1 rev 2 input a=1", got)
		}
		if !got.StartedAt.IsZero() || !got.FinishedAt.IsZero() || !got.LeaseUntil.IsZero() {
			t.Fatalf("GetRun optional times = %v %v %v, want all zero", got.StartedAt, got.FinishedAt, got.LeaseUntil)
		}
	})

	t.Run("by idempotency key", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		_, err := s.GetRunByIdempotency(ctx, "")
		agentBPGExpectErr(t, err, ErrNotFound)
		_, err = s.GetRunByIdempotency(ctx, "nope")
		agentBPGExpectErr(t, err, ErrNotFound)
		run := Run{ID: "r1", DefinitionID: "d1", Revision: 1, Definition: agentBPGTestDef("d1"), State: RunCreated, IdempotencyKey: "idem-9", Input: map[string]any{}}
		if err := s.CreateRun(ctx, run); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		got, err := s.GetRunByIdempotency(ctx, "idem-9")
		if err != nil {
			t.Fatalf("GetRunByIdempotency: %v", err)
		}
		if got.ID != "r1" {
			t.Fatalf("GetRunByIdempotency id = %q, want r1", got.ID)
		}
	})
}

func TestAgentBPGUpdateRunState(t *testing.T) {
	ctx := context.Background()
	seedCreated := func(t *testing.T, db *agentBPGFakeDB) {
		t.Helper()
		now := time.Now()
		agentBPGSeedRun(t, db, Run{ID: "r1", DefinitionID: "d1", Revision: 1, Definition: agentBPGTestDef("d1"), State: RunCreated, Input: map[string]any{}, CreatedAt: now, UpdatedAt: now})
	}

	t.Run("begin fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failBegin = true
		_, err := s.UpdateRunState(ctx, "r1", "", RunRunning, "", "")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("missing run", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		_, err := s.UpdateRunState(ctx, "nope", "", RunRunning, "", "")
		agentBPGExpectErr(t, err, ErrNotFound)
	})

	t.Run("lock query fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedCreated(t, db)
		s := agentBPGOpenStore(t, db)
		db.failQuery = "FOR UPDATE"
		_, err := s.UpdateRunState(ctx, "r1", "", RunRunning, "", "")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("scan fails on bad revision", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedCreated(t, db)
		s := agentBPGOpenStore(t, db)
		db.badRunRevision["r1"] = true
		_, err := s.UpdateRunState(ctx, "r1", "", RunRunning, "", "")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("wrong expected state", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedCreated(t, db)
		s := agentBPGOpenStore(t, db)
		_, err := s.UpdateRunState(ctx, "r1", RunRunning, RunRunning, "", "")
		agentBPGExpectErr(t, err, ErrStateConflict)
	})

	t.Run("terminal run", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		now := time.Now()
		agentBPGSeedRun(t, db, Run{ID: "r1", DefinitionID: "d1", Revision: 1, Definition: agentBPGTestDef("d1"), State: RunSucceeded, Input: map[string]any{}, CreatedAt: now, UpdatedAt: now, FinishedAt: now})
		s := agentBPGOpenStore(t, db)
		_, err := s.UpdateRunState(ctx, "r1", "", RunRunning, "", "")
		agentBPGExpectErr(t, err, ErrTerminalState)
	})

	t.Run("invalid transition", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedCreated(t, db)
		s := agentBPGOpenStore(t, db)
		_, err := s.UpdateRunState(ctx, "r1", "", RunSucceeded, "", "")
		agentBPGExpectErr(t, err, ErrStateConflict)
	})

	t.Run("update fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedCreated(t, db)
		s := agentBPGOpenStore(t, db)
		db.failExec = "UPDATE nexaroute_workflow_runs SET state"
		_, err := s.UpdateRunState(ctx, "r1", "", RunRunning, "", "")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("commit fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedCreated(t, db)
		s := agentBPGOpenStore(t, db)
		db.failCommit = true
		_, err := s.UpdateRunState(ctx, "r1", "", RunRunning, "", "")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("created to running stamps started", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedCreated(t, db)
		s := agentBPGOpenStore(t, db)
		got, err := s.UpdateRunState(ctx, "r1", RunCreated, RunRunning, "", "")
		if err != nil {
			t.Fatalf("UpdateRunState: %v", err)
		}
		if got.State != RunRunning || got.StartedAt.IsZero() || !got.FinishedAt.IsZero() {
			t.Fatalf("UpdateRunState = state %q started %v finished %v, want running with started set", got.State, got.StartedAt, got.FinishedAt)
		}
	})

	t.Run("running to succeeded stamps finished", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		now := time.Now()
		agentBPGSeedRun(t, db, Run{ID: "r1", DefinitionID: "d1", Revision: 1, Definition: agentBPGTestDef("d1"), State: RunRunning, Input: map[string]any{}, CreatedAt: now, UpdatedAt: now, StartedAt: now})
		s := agentBPGOpenStore(t, db)
		got, err := s.UpdateRunState(ctx, "r1", RunRunning, RunSucceeded, "ok", "done")
		if err != nil {
			t.Fatalf("UpdateRunState: %v", err)
		}
		if got.State != RunSucceeded || got.FinishedAt.IsZero() || got.ErrorCode != "ok" || got.Error != "done" {
			t.Fatalf("UpdateRunState = %+v, want succeeded with finished set and error ok/done", got)
		}
		if !got.StartedAt.Equal(now) {
			t.Fatalf("StartedAt = %v, want preserved %v", got.StartedAt, now)
		}
	})
}

func TestAgentBPGListRuns(t *testing.T) {
	ctx := context.Background()
	seedThree := func(t *testing.T, db *agentBPGFakeDB) {
		t.Helper()
		now := time.Now()
		agentBPGSeedRun(t, db, Run{ID: "r1", DefinitionID: "d1", Revision: 1, Definition: agentBPGTestDef("d1"), State: RunCreated, Input: map[string]any{}, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now})
		agentBPGSeedRun(t, db, Run{ID: "r2", DefinitionID: "d1", Revision: 1, Definition: agentBPGTestDef("d1"), State: RunSucceeded, Input: map[string]any{}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now, FinishedAt: now})
		agentBPGSeedRun(t, db, Run{ID: "r3", DefinitionID: "d2", Revision: 1, Definition: agentBPGTestDef("d2"), State: RunCreated, Input: map[string]any{}, CreatedAt: now, UpdatedAt: now})
	}

	t.Run("query fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failQuery = "WHERE 1=1"
		_, err := s.ListRuns(ctx, RunFilter{})
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("scan fails on bad revision", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedThree(t, db)
		s := agentBPGOpenStore(t, db)
		db.badRunRevision["r1"] = true
		_, err := s.ListRuns(ctx, RunFilter{})
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("rows error", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedThree(t, db)
		s := agentBPGOpenStore(t, db)
		db.rowsErrQuery = "WHERE 1=1"
		_, err := s.ListRuns(ctx, RunFilter{})
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("filters", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedThree(t, db)
		s := agentBPGOpenStore(t, db)
		byDef, err := s.ListRuns(ctx, RunFilter{DefinitionID: "d1"})
		if err != nil {
			t.Fatalf("ListRuns by def: %v", err)
		}
		if len(byDef) != 2 || byDef[0].ID != "r1" || byDef[1].ID != "r2" {
			t.Fatalf("ListRuns by def = %v, want [r1 r2]", byDef)
		}
		byState, err := s.ListRuns(ctx, RunFilter{State: RunSucceeded})
		if err != nil {
			t.Fatalf("ListRuns by state: %v", err)
		}
		if len(byState) != 1 || byState[0].ID != "r2" {
			t.Fatalf("ListRuns by state = %v, want [r2]", byState)
		}
		limited, err := s.ListRuns(ctx, RunFilter{Limit: 1})
		if err != nil {
			t.Fatalf("ListRuns limited: %v", err)
		}
		if len(limited) != 1 || limited[0].ID != "r1" {
			t.Fatalf("ListRuns limited = %v, want [r1]", limited)
		}
		all, err := s.ListRuns(ctx, RunFilter{})
		if err != nil {
			t.Fatalf("ListRuns all: %v", err)
		}
		if len(all) != 3 || all[0].ID != "r1" || all[1].ID != "r2" || all[2].ID != "r3" {
			t.Fatalf("ListRuns all = %v, want [r1 r2 r3]", all)
		}
	})
}

func TestAgentBPGPrune(t *testing.T) {
	ctx := context.Background()
	seedPrunable := func(t *testing.T, db *agentBPGFakeDB, now time.Time) {
		t.Helper()
		old := Run{ID: "old", DefinitionID: "d1", Revision: 1, Definition: agentBPGTestDef("d1"), State: RunSucceeded, Input: map[string]any{}, CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour), FinishedAt: now.Add(-2 * time.Hour)}
		agentBPGSeedRun(t, db, old)
		agentBPGSeedNode(t, db, NodeExecution{RunID: "old", NodeID: "n1", Attempt: 0, State: NodeSucceeded})
		db.tables.approvals["old"] = map[string]*agentBPGApproval{"n1": {runID: "old", nodeID: "n1", requester: "alice", approver: "bob", decision: "approved", at: now}}
	}

	t.Run("begin fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failBegin = true
		_, err := s.Prune(ctx, time.Now())
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("select fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failQuery = "SELECT run_id FROM"
		_, err := s.Prune(ctx, time.Now())
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("scan fails on column count", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		now := time.Now()
		seedPrunable(t, db, now)
		s := agentBPGOpenStore(t, db)
		db.badPruneCols = true
		_, err := s.Prune(ctx, now.Add(-time.Hour))
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("rows error", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		now := time.Now()
		seedPrunable(t, db, now)
		s := agentBPGOpenStore(t, db)
		db.rowsErrQuery = "SELECT run_id FROM"
		_, err := s.Prune(ctx, now.Add(-time.Hour))
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("close error surfaces via rows error", func(t *testing.T) {
		// database/sql auto-closes Rows when Next() exhausts the set,
		// so a driver Close failure lands in rows.Err(), not in the
		// (removed) explicit Close check.
		db := newAgentBPGFakeDB()
		now := time.Now()
		seedPrunable(t, db, now)
		s := agentBPGOpenStore(t, db)
		db.closeErrQuery = "SELECT run_id FROM"
		_, err := s.Prune(ctx, now.Add(-time.Hour))
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("delete nodes fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		now := time.Now()
		seedPrunable(t, db, now)
		s := agentBPGOpenStore(t, db)
		db.failExec = "DELETE FROM nexaroute_workflow_nodes"
		_, err := s.Prune(ctx, now.Add(-time.Hour))
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("delete approvals fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		now := time.Now()
		seedPrunable(t, db, now)
		s := agentBPGOpenStore(t, db)
		db.failExec = "DELETE FROM nexaroute_workflow_approvals"
		_, err := s.Prune(ctx, now.Add(-time.Hour))
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("delete runs fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		now := time.Now()
		seedPrunable(t, db, now)
		s := agentBPGOpenStore(t, db)
		db.failExec = "DELETE FROM nexaroute_workflow_runs"
		_, err := s.Prune(ctx, now.Add(-time.Hour))
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("rows affected fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		now := time.Now()
		seedPrunable(t, db, now)
		s := agentBPGOpenStore(t, db)
		db.failRowsAffected = true
		_, err := s.Prune(ctx, now.Add(-time.Hour))
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("commit fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		now := time.Now()
		seedPrunable(t, db, now)
		s := agentBPGOpenStore(t, db)
		db.failCommit = true
		_, err := s.Prune(ctx, now.Add(-time.Hour))
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("empty set", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		n, err := s.Prune(ctx, time.Now())
		if err != nil {
			t.Fatalf("Prune: %v", err)
		}
		if n != 0 {
			t.Fatalf("Prune = %d, want 0", n)
		}
	})

	t.Run("success prunes only eligible", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		now := time.Now()
		seedPrunable(t, db, now)
		recent := Run{ID: "recent", DefinitionID: "d1", Revision: 1, Definition: agentBPGTestDef("d1"), State: RunFailed, Input: map[string]any{}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-30 * time.Minute), FinishedAt: now.Add(-30 * time.Minute)}
		agentBPGSeedRun(t, db, recent)
		active := Run{ID: "active", DefinitionID: "d1", Revision: 1, Definition: agentBPGTestDef("d1"), State: RunCreated, Input: map[string]any{}, CreatedAt: now, UpdatedAt: now}
		agentBPGSeedRun(t, db, active)
		s := agentBPGOpenStore(t, db)
		n, err := s.Prune(ctx, now.Add(-time.Hour))
		if err != nil {
			t.Fatalf("Prune: %v", err)
		}
		if n != 1 {
			t.Fatalf("Prune = %d, want 1", n)
		}
		if _, err := s.GetRun(ctx, "old"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetRun old = %v, want ErrNotFound", err)
		}
		if _, err := s.GetNodeExecutions(ctx, "old"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("GetNodeExecutions old = %v, want ErrNotFound", err)
		}
		if _, err := s.GetApproval(ctx, "old", "n1"); !errors.Is(err, ErrNoApprovalRecord) {
			t.Fatalf("GetApproval old = %v, want ErrNoApprovalRecord", err)
		}
		if _, err := s.GetRun(ctx, "recent"); err != nil {
			t.Fatalf("GetRun recent: %v", err)
		}
		if _, err := s.GetRun(ctx, "active"); err != nil {
			t.Fatalf("GetRun active: %v", err)
		}
	})
}

func TestAgentBPGAppendNodeExecution(t *testing.T) {
	ctx := context.Background()
	seedRun := func(t *testing.T, db *agentBPGFakeDB) {
		t.Helper()
		now := time.Now()
		agentBPGSeedRun(t, db, Run{ID: "r1", DefinitionID: "d1", Revision: 1, Definition: agentBPGTestDef("d1"), State: RunRunning, Input: map[string]any{}, CreatedAt: now, UpdatedAt: now, StartedAt: now})
	}
	newExec := func(op string) NodeExecution {
		return NodeExecution{RunID: "r1", NodeID: "n1", AppendOperationID: op, Input: map[string]any{"in": "x"}}
	}

	t.Run("NaN input encode fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		exec := newExec("")
		exec.Input = map[string]any{"x": math.NaN()}
		_, err := s.AppendNodeExecution(ctx, exec)
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("begin fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failBegin = true
		_, err := s.AppendNodeExecution(ctx, newExec(""))
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("missing run", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		_, err := s.AppendNodeExecution(ctx, newExec(""))
		agentBPGExpectErr(t, err, ErrNotFound)
	})

	t.Run("run lock query fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedRun(t, db)
		s := agentBPGOpenStore(t, db)
		db.failQuery = "FOR UPDATE"
		_, err := s.AppendNodeExecution(ctx, newExec(""))
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("operation lookup returns existing", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedRun(t, db)
		s := agentBPGOpenStore(t, db)
		first, err := s.AppendNodeExecution(ctx, newExec("op-1"))
		if err != nil {
			t.Fatalf("Append 1: %v", err)
		}
		if first.Attempt != 0 || first.IdempotencyKey == "" {
			t.Fatalf("Append 1 = attempt %d key %q, want attempt 0 with a key", first.Attempt, first.IdempotencyKey)
		}
		again, err := s.AppendNodeExecution(ctx, newExec("op-1"))
		if err != nil {
			t.Fatalf("Append replay: %v", err)
		}
		if again.Attempt != 0 || again.IdempotencyKey != first.IdempotencyKey {
			t.Fatalf("Append replay = attempt %d key %q, want attempt 0 key %q", again.Attempt, again.IdempotencyKey, first.IdempotencyKey)
		}
	})

	t.Run("operation lookup scan fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedRun(t, db)
		agentBPGSeedNode(t, db, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 0, State: NodePending, AppendOperationID: "op-1"})
		db.badNodeAttempt["r1\x00n1\x000"] = true
		s := agentBPGOpenStore(t, db)
		_, err := s.AppendNodeExecution(ctx, newExec("op-1"))
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("max attempt query fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedRun(t, db)
		s := agentBPGOpenStore(t, db)
		db.failQuery = "SELECT MAX(attempt)"
		_, err := s.AppendNodeExecution(ctx, newExec(""))
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("insert fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedRun(t, db)
		s := agentBPGOpenStore(t, db)
		db.failExec = "INSERT INTO nexaroute_workflow_nodes"
		_, err := s.AppendNodeExecution(ctx, newExec(""))
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("commit fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedRun(t, db)
		s := agentBPGOpenStore(t, db)
		db.failCommit = true
		_, err := s.AppendNodeExecution(ctx, newExec(""))
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("success attempts increment", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedRun(t, db)
		s := agentBPGOpenStore(t, db)
		first, err := s.AppendNodeExecution(ctx, newExec("op-1"))
		if err != nil {
			t.Fatalf("Append 1: %v", err)
		}
		second, err := s.AppendNodeExecution(ctx, newExec("op-2"))
		if err != nil {
			t.Fatalf("Append 2: %v", err)
		}
		if first.Attempt != 0 || second.Attempt != 1 {
			t.Fatalf("attempts = %d, %d, want 0, 1", first.Attempt, second.Attempt)
		}
		if first.State != NodePending || first.CreatedAt.IsZero() {
			t.Fatalf("first = state %q created %v, want pending with created set", first.State, first.CreatedAt)
		}
	})
}

func TestAgentBPGSavenodeExecution(t *testing.T) {
	ctx := context.Background()
	appendTwo := func(t *testing.T, s *PostgresStore) {
		t.Helper()
		for _, op := range []string{"op-1", "op-2"} {
			if _, err := s.AppendNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n1", AppendOperationID: op}); err != nil {
				t.Fatalf("Append %s: %v", op, err)
			}
		}
	}
	seedRun := func(t *testing.T, db *agentBPGFakeDB) {
		t.Helper()
		now := time.Now()
		agentBPGSeedRun(t, db, Run{ID: "r1", DefinitionID: "d1", Revision: 1, Definition: agentBPGTestDef("d1"), State: RunRunning, Input: map[string]any{}, CreatedAt: now, UpdatedAt: now, StartedAt: now})
	}

	t.Run("NaN encode fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		exec := NodeExecution{RunID: "r1", NodeID: "n1", Input: map[string]any{"x": math.NaN()}}
		_, err := s.SaveNodeExecution(ctx, exec, "")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("begin fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failBegin = true
		_, err := s.SaveNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n1"}, "")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("exact attempt hit", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedRun(t, db)
		s := agentBPGOpenStore(t, db)
		appendTwo(t, s)
		got, err := s.SaveNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 0, State: NodeClaimed}, NodePending)
		if err != nil {
			t.Fatalf("SaveNodeExecution: %v", err)
		}
		if got.Attempt != 0 || got.State != NodeClaimed {
			t.Fatalf("Save = attempt %d state %q, want attempt 0 claimed", got.Attempt, got.State)
		}
	})

	t.Run("fallback resolves latest attempt", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedRun(t, db)
		s := agentBPGOpenStore(t, db)
		appendTwo(t, s)
		got, err := s.SaveNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 99, State: NodeClaimed}, "")
		if err != nil {
			t.Fatalf("SaveNodeExecution: %v", err)
		}
		if got.Attempt != 1 {
			t.Fatalf("Save attempt = %d, want 1 (latest)", got.Attempt)
		}
	})

	t.Run("missing node", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedRun(t, db)
		s := agentBPGOpenStore(t, db)
		_, err := s.SaveNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 99, State: NodeClaimed}, "")
		agentBPGExpectErr(t, err, ErrNotFound)
	})

	t.Run("fallback query fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedRun(t, db)
		s := agentBPGOpenStore(t, db)
		db.failQuery = "ORDER BY attempt DESC LIMIT 1 FOR UPDATE"
		_, err := s.SaveNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 99, State: NodeClaimed}, "")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("first scan fails on bad attempt", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedRun(t, db)
		agentBPGSeedNode(t, db, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 0, State: NodePending})
		db.badNodeAttempt["r1\x00n1\x000"] = true
		s := agentBPGOpenStore(t, db)
		_, err := s.SaveNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 0, State: NodeClaimed}, "")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("wrong expected state", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedRun(t, db)
		s := agentBPGOpenStore(t, db)
		appendTwo(t, s)
		_, err := s.SaveNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 0, State: NodeClaimed}, NodeRunning)
		agentBPGExpectErr(t, err, ErrStateConflict)
	})

	t.Run("terminal node", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedRun(t, db)
		agentBPGSeedNode(t, db, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 0, State: NodeSucceeded})
		s := agentBPGOpenStore(t, db)
		_, err := s.SaveNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 0, State: NodeRunning}, "")
		agentBPGExpectErr(t, err, ErrTerminalState)
	})

	t.Run("invalid transition", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedRun(t, db)
		s := agentBPGOpenStore(t, db)
		appendTwo(t, s)
		_, err := s.SaveNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 0, State: NodeSucceeded}, "")
		agentBPGExpectErr(t, err, ErrStateConflict)
	})

	t.Run("update fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedRun(t, db)
		s := agentBPGOpenStore(t, db)
		appendTwo(t, s)
		db.failExec = "UPDATE nexaroute_workflow_nodes SET payload"
		_, err := s.SaveNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 0, State: NodeClaimed}, "")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("commit fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedRun(t, db)
		s := agentBPGOpenStore(t, db)
		appendTwo(t, s)
		db.failCommit = true
		_, err := s.SaveNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 0, State: NodeClaimed}, "")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("running then succeeded stamps times and clears lease", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedRun(t, db)
		s := agentBPGOpenStore(t, db)
		appendTwo(t, s)
		claimed, err := s.SaveNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 1, State: NodeClaimed, LeaseOwner: "w1"}, NodePending)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if claimed.LeaseOwner != "w1" {
			t.Fatalf("claimed lease owner = %q, want w1", claimed.LeaseOwner)
		}
		running, err := s.SaveNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 1, State: NodeRunning, LeaseOwner: "w1"}, NodeClaimed)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if running.StartedAt.IsZero() {
			t.Fatalf("running StartedAt is zero, want stamped")
		}
		done, err := s.SaveNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 1, State: NodeSucceeded, LeaseOwner: "w1"}, NodeRunning)
		if err != nil {
			t.Fatalf("succeed: %v", err)
		}
		if done.FinishedAt.IsZero() || done.LeaseOwner != "" || !done.LeaseUntil.IsZero() {
			t.Fatalf("done = finished %v lease %q until %v, want finished set and lease cleared", done.FinishedAt, done.LeaseOwner, done.LeaseUntil)
		}
	})
}

func TestAgentBPGGetNodeExecutions(t *testing.T) {
	ctx := context.Background()

	t.Run("exists query fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failQuery = "SELECT EXISTS"
		_, err := s.GetNodeExecutions(ctx, "r1")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("missing run", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		_, err := s.GetNodeExecutions(ctx, "nope")
		agentBPGExpectErr(t, err, ErrNotFound)
	})

	t.Run("list query fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		now := time.Now()
		agentBPGSeedRun(t, db, Run{ID: "r1", DefinitionID: "d1", Revision: 1, Definition: agentBPGTestDef("d1"), State: RunRunning, Input: map[string]any{}, CreatedAt: now, UpdatedAt: now})
		s := agentBPGOpenStore(t, db)
		db.failQuery = "ORDER BY node_id, attempt"
		_, err := s.GetNodeExecutions(ctx, "r1")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("scan fails on bad attempt", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		now := time.Now()
		agentBPGSeedRun(t, db, Run{ID: "r1", DefinitionID: "d1", Revision: 1, Definition: agentBPGTestDef("d1"), State: RunRunning, Input: map[string]any{}, CreatedAt: now, UpdatedAt: now})
		agentBPGSeedNode(t, db, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 0, State: NodePending})
		db.badNodeAttempt["r1\x00n1\x000"] = true
		s := agentBPGOpenStore(t, db)
		_, err := s.GetNodeExecutions(ctx, "r1")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("garbage payload", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		now := time.Now()
		agentBPGSeedRun(t, db, Run{ID: "r1", DefinitionID: "d1", Revision: 1, Definition: agentBPGTestDef("d1"), State: RunRunning, Input: map[string]any{}, CreatedAt: now, UpdatedAt: now})
		db.seedNode(&agentBPGNode{runID: "r1", nodeID: "n1", attempt: 0, payload: []byte("{bad"), state: "pending"})
		s := agentBPGOpenStore(t, db)
		_, err := s.GetNodeExecutions(ctx, "r1")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("rows error", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		now := time.Now()
		agentBPGSeedRun(t, db, Run{ID: "r1", DefinitionID: "d1", Revision: 1, Definition: agentBPGTestDef("d1"), State: RunRunning, Input: map[string]any{}, CreatedAt: now, UpdatedAt: now})
		agentBPGSeedNode(t, db, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 0, State: NodePending})
		s := agentBPGOpenStore(t, db)
		db.rowsErrQuery = "ORDER BY node_id, attempt"
		_, err := s.GetNodeExecutions(ctx, "r1")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("success ordered", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		now := time.Now()
		agentBPGSeedRun(t, db, Run{ID: "r1", DefinitionID: "d1", Revision: 1, Definition: agentBPGTestDef("d1"), State: RunRunning, Input: map[string]any{}, CreatedAt: now, UpdatedAt: now})
		agentBPGSeedNode(t, db, NodeExecution{RunID: "r1", NodeID: "n2", Attempt: 0, State: NodePending})
		agentBPGSeedNode(t, db, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 1, State: NodePending})
		agentBPGSeedNode(t, db, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 0, State: NodePending})
		s := agentBPGOpenStore(t, db)
		got, err := s.GetNodeExecutions(ctx, "r1")
		if err != nil {
			t.Fatalf("GetNodeExecutions: %v", err)
		}
		if len(got) != 3 || got[0].NodeID != "n1" || got[0].Attempt != 0 || got[1].Attempt != 1 || got[2].NodeID != "n2" {
			t.Fatalf("GetNodeExecutions = %v, want n1/0 n1/1 n2/0", got)
		}
	})
}

func TestAgentBPGTryClaim(t *testing.T) {
	ctx := context.Background()
	seedPending := func(t *testing.T, db *agentBPGFakeDB) {
		t.Helper()
		agentBPGSeedNode(t, db, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 0, State: NodePending})
	}

	t.Run("begin fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failBegin = true
		_, _, err := s.TryClaim(ctx, "r1", "n1", "w1", 30*time.Second, time.Now())
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("missing node", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		_, _, err := s.TryClaim(ctx, "r1", "n1", "w1", 30*time.Second, time.Now())
		agentBPGExpectErr(t, err, ErrNotFound)
	})

	t.Run("resolve query fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedPending(t, db)
		s := agentBPGOpenStore(t, db)
		db.failQuery = "SELECT attempt FROM"
		_, _, err := s.TryClaim(ctx, "r1", "n1", "w1", 30*time.Second, time.Now())
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("live foreign lease loses", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		future := time.Now().Add(time.Hour)
		agentBPGSeedNode(t, db, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 0, State: NodeClaimed, LeaseOwner: "other", LeaseUntil: future})
		s := agentBPGOpenStore(t, db)
		_, ok, err := s.TryClaim(ctx, "r1", "n1", "w1", 30*time.Second, time.Now())
		if err != nil {
			t.Fatalf("TryClaim: %v", err)
		}
		if ok {
			t.Fatalf("TryClaim = claimed, want lose against live foreign lease")
		}
	})

	t.Run("returning query fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedPending(t, db)
		s := agentBPGOpenStore(t, db)
		db.failQuery = "RETURNING payload"
		_, _, err := s.TryClaim(ctx, "r1", "n1", "w1", 30*time.Second, time.Now())
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("garbage payload", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		db.seedNode(&agentBPGNode{runID: "r1", nodeID: "n1", attempt: 0, payload: []byte("{bad"), state: "pending"})
		s := agentBPGOpenStore(t, db)
		_, _, err := s.TryClaim(ctx, "r1", "n1", "w1", 30*time.Second, time.Now())
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("commit fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedPending(t, db)
		s := agentBPGOpenStore(t, db)
		db.failCommit = true
		_, _, err := s.TryClaim(ctx, "r1", "n1", "w1", 30*time.Second, time.Now())
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("success claims with lease", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		seedPending(t, db)
		s := agentBPGOpenStore(t, db)
		before := time.Now()
		got, ok, err := s.TryClaim(ctx, "r1", "n1", "w1", 30*time.Second, time.Now())
		if err != nil {
			t.Fatalf("TryClaim: %v", err)
		}
		if !ok || got.State != NodeClaimed || got.LeaseOwner != "w1" {
			t.Fatalf("TryClaim = ok %v state %q owner %q, want claimed by w1", ok, got.State, got.LeaseOwner)
		}
		if got.LeaseUntil.Before(before.Add(29*time.Second)) || got.LeaseUntil.After(before.Add(31*time.Second)) {
			t.Fatalf("TryClaim lease until = %v, want now+30s", got.LeaseUntil)
		}
		// Same owner can re-claim; a different owner loses.
		if _, ok, err := s.TryClaim(ctx, "r1", "n1", "w1", 30*time.Second, time.Now()); err != nil || !ok {
			t.Fatalf("re-claim by same owner = ok %v err %v, want ok", ok, err)
		}
		if _, ok, err := s.TryClaim(ctx, "r1", "n1", "w2", 30*time.Second, time.Now()); err != nil || ok {
			t.Fatalf("claim by other owner = ok %v err %v, want lose", ok, err)
		}
	})
}

func TestAgentBPGReleaseLease(t *testing.T) {
	ctx := context.Background()

	t.Run("owner match clears, wrong owner no-op", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		now := time.Now()
		agentBPGSeedRun(t, db, Run{ID: "r1", DefinitionID: "d1", Revision: 1, Definition: agentBPGTestDef("d1"), State: RunRunning, Input: map[string]any{}, CreatedAt: now, UpdatedAt: now, StartedAt: now})
		if _, err := s.AppendNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n1"}); err != nil {
			t.Fatalf("Append: %v", err)
		}
		// Save a lease through the production path: payload and
		// columns both carry it. ReleaseLease clears the columns
		// only, so the read-back must not resurrect the payload's
		// stale lease_until (scanNode overlay defect, fixed).
		future := time.Now().Add(time.Hour)
		if _, err := s.SaveNodeExecution(ctx, NodeExecution{RunID: "r1", NodeID: "n1", Attempt: 0, State: NodeClaimed, LeaseOwner: "w1", LeaseUntil: future}, NodePending); err != nil {
			t.Fatalf("Save leased: %v", err)
		}
		if err := s.ReleaseLease(ctx, "r1", "n1", "w2"); err != nil {
			t.Fatalf("ReleaseLease wrong owner: %v", err)
		}
		got, err := s.GetNodeExecutions(ctx, "r1")
		if err != nil {
			t.Fatalf("GetNodeExecutions: %v", err)
		}
		if got[0].LeaseOwner != "w1" || !got[0].LeaseUntil.Equal(future) {
			t.Fatalf("lease after wrong-owner release = owner %q until %v, want w1 %v", got[0].LeaseOwner, got[0].LeaseUntil, future)
		}
		if err := s.ReleaseLease(ctx, "r1", "n1", "w1"); err != nil {
			t.Fatalf("ReleaseLease: %v", err)
		}
		got, err = s.GetNodeExecutions(ctx, "r1")
		if err != nil {
			t.Fatalf("GetNodeExecutions: %v", err)
		}
		if got[0].LeaseOwner != "" || !got[0].LeaseUntil.IsZero() {
			t.Fatalf("lease after release = owner %q until %v, want cleared", got[0].LeaseOwner, got[0].LeaseUntil)
		}
	})

	t.Run("exec fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failExec = "SET lease_owner = '', lease_until = NULL"
		agentBPGExpectErr(t, s.ReleaseLease(ctx, "r1", "n1", "w1"), nil)
	})
}

func TestAgentBPGRecordApproval(t *testing.T) {
	ctx := context.Background()
	newAppr := func() Approval {
		return Approval{RunID: "r1", NodeID: "n1", Requester: "alice", Approver: "bob", Decision: "approved"}
	}

	t.Run("invalid decision", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		appr := newAppr()
		appr.Decision = "maybe"
		_, err := s.RecordApproval(ctx, appr)
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("approver is requester", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		appr := newAppr()
		appr.Approver = "alice"
		_, err := s.RecordApproval(ctx, appr)
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("begin fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failBegin = true
		_, err := s.RecordApproval(ctx, newAppr())
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("insert fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failExec = "INSERT INTO nexaroute_workflow_approvals"
		_, err := s.RecordApproval(ctx, newAppr())
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("rows affected fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failRowsAffected = true
		_, err := s.RecordApproval(ctx, newAppr())
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("commit fails on fresh insert", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failCommit = true
		_, err := s.RecordApproval(ctx, newAppr())
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("fresh insert and idempotent replay", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		got, err := s.RecordApproval(ctx, newAppr())
		if err != nil {
			t.Fatalf("RecordApproval: %v", err)
		}
		if got.DecidedAt.IsZero() || got.Decision != "approved" {
			t.Fatalf("RecordApproval = %+v, want approved with decided_at set", got)
		}
		replay := newAppr()
		replay.Requester = "mallory"
		again, err := s.RecordApproval(ctx, replay)
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if again.Requester != "alice" || !again.DecidedAt.Equal(got.DecidedAt) {
			t.Fatalf("replay = requester %q decided %v, want alice %v (stored record)", again.Requester, again.DecidedAt, got.DecidedAt)
		}
	})

	t.Run("read-back query fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		if _, err := s.RecordApproval(ctx, newAppr()); err != nil {
			t.Fatalf("RecordApproval: %v", err)
		}
		db.failQuery = "SELECT decision, approver, requester, decided_at"
		_, err := s.RecordApproval(ctx, newAppr())
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("different decision conflicts", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		if _, err := s.RecordApproval(ctx, newAppr()); err != nil {
			t.Fatalf("RecordApproval: %v", err)
		}
		other := newAppr()
		other.Decision = "rejected"
		_, err := s.RecordApproval(ctx, other)
		agentBPGExpectErr(t, err, ErrApprovalConflict)
	})
}

func TestAgentBPGGetApproval(t *testing.T) {
	ctx := context.Background()

	t.Run("missing", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		_, err := s.GetApproval(ctx, "r1", "n1")
		agentBPGExpectErr(t, err, ErrNoApprovalRecord)
	})

	t.Run("query fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		db.failQuery = "SELECT run_id, node_id, requester"
		_, err := s.GetApproval(ctx, "r1", "n1")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("success", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		s := agentBPGOpenStore(t, db)
		if _, err := s.RecordApproval(ctx, Approval{RunID: "r1", NodeID: "n1", Requester: "alice", Approver: "bob", Decision: "rejected"}); err != nil {
			t.Fatalf("RecordApproval: %v", err)
		}
		got, err := s.GetApproval(ctx, "r1", "n1")
		if err != nil {
			t.Fatalf("GetApproval: %v", err)
		}
		if got.Decision != "rejected" || got.Approver != "bob" || got.Requester != "alice" {
			t.Fatalf("GetApproval = %+v, want rejected by bob for alice", got)
		}
	})
}

func TestAgentBPGDeliveryLedger(t *testing.T) {
	ctx := context.Background()

	t.Run("nil db", func(t *testing.T) {
		_, err := NewPostgresDeliveryLedger(nil)
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("ensure schema success and failure", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		sqlDB := agentBOpenFakePG(t, db)
		if err := EnsurePostgresDeliverySchema(ctx, sqlDB); err != nil {
			t.Fatalf("EnsurePostgresDeliverySchema: %v", err)
		}
		if err := EnsurePostgresDeliverySchema(ctx, sqlDB); err != nil {
			t.Fatalf("EnsurePostgresDeliverySchema again: %v", err)
		}
		db.failExec = "CREATE TABLE IF NOT EXISTS nexaroute_workflow_deliveries"
		agentBPGExpectErr(t, EnsurePostgresDeliverySchema(ctx, sqlDB), nil)
	})

	t.Run("claim empty key", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		sqlDB := agentBOpenFakePG(t, db)
		ledger, err := NewPostgresDeliveryLedger(sqlDB)
		if err != nil {
			t.Fatalf("NewPostgresDeliveryLedger: %v", err)
		}
		_, err = ledger.Claim(ctx, "")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("claim first true second false", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		sqlDB := agentBOpenFakePG(t, db)
		ledger, err := NewPostgresDeliveryLedger(sqlDB)
		if err != nil {
			t.Fatalf("NewPostgresDeliveryLedger: %v", err)
		}
		first, err := ledger.Claim(ctx, "k1")
		if err != nil || !first {
			t.Fatalf("Claim 1 = %v, %v, want true, nil", first, err)
		}
		second, err := ledger.Claim(ctx, "k1")
		if err != nil || second {
			t.Fatalf("Claim 2 = %v, %v, want false, nil", second, err)
		}
	})

	t.Run("claim exec fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		sqlDB := agentBOpenFakePG(t, db)
		ledger, err := NewPostgresDeliveryLedger(sqlDB)
		if err != nil {
			t.Fatalf("NewPostgresDeliveryLedger: %v", err)
		}
		db.failExec = "INSERT INTO nexaroute_workflow_deliveries"
		_, err = ledger.Claim(ctx, "k1")
		agentBPGExpectErr(t, err, nil)
	})

	t.Run("claim rows affected fails", func(t *testing.T) {
		db := newAgentBPGFakeDB()
		sqlDB := agentBOpenFakePG(t, db)
		ledger, err := NewPostgresDeliveryLedger(sqlDB)
		if err != nil {
			t.Fatalf("NewPostgresDeliveryLedger: %v", err)
		}
		db.failRowsAffected = true
		_, err = ledger.Claim(ctx, "k1")
		agentBPGExpectErr(t, err, nil)
	})
}
