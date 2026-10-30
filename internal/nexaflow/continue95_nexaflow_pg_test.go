package nexaflow

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Continue95Nexaflow PG tests: method-level behaviour of PostgresStore
// against the disposable cluster. They deliberately do NOT repeat the
// shared store-contract suite (TestPostgresStoreContract); each test
// below targets a distinct method path or error branch, with unique
// IDs so tests never interfere and never DROP shared tables.

var Continue95NexaflowPGCounter int64

// Continue95NexaflowPGPrefix is random per test process, so reruns
// (and sibling agents sharing the disposable database) never collide
// with rows left by an earlier process: the counter alone resets.
var Continue95NexaflowPGPrefix = func() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("%x", b)
	}
	return fmt.Sprintf("t%d", time.Now().UnixNano())
}()

func Continue95NexaflowPGID(prefix string) string {
	n := atomic.AddInt64(&Continue95NexaflowPGCounter, 1)
	return fmt.Sprintf("c95-%s-%s-%d", Continue95NexaflowPGPrefix, prefix, n)
}

func Continue95NexaflowPGDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("NEXAROUTE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("NEXAROUTE_TEST_PG_DSN not set; PostgreSQL live acceptance skipped")
	}
	return dsn
}

func Continue95NexaflowPGStore(t *testing.T) *PostgresStore {
	t.Helper()
	store, err := OpenPostgresStore(context.Background(), Continue95NexaflowPGDSN(t))
	if err != nil {
		t.Fatalf("OpenPostgresStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func Continue95NexaflowPGDef(id string) Definition {
	return Definition{
		SchemaVersion: DefinitionSchemaVersion,
		ID:            id,
		Name:          "c95-" + id,
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "work", NodeID: "test.echo", Version: SupportedVersion, Config: map[string]any{"k": "v"}},
		},
		Edges:    []DefinitionEdge{{ID: "e1", From: "start", To: "work"}},
		Triggers: []DefinitionTrigger{{Name: "start", Kind: TriggerManual, Config: map[string]any{}}},
	}
}

func Continue95NexaflowPGRun(id, defID string, rev uint64, idem string) Run {
	return Run{
		ID: id, DefinitionID: defID, Revision: rev,
		Definition: Continue95NexaflowPGDef(defID), State: RunCreated,
		IdempotencyKey: idem, Principal: "tester",
		Input: map[string]any{"x": float64(1)},
	}
}

func TestContinue95NexaflowPGOpenErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := OpenPostgresStore(ctx, "postgres://127.0.0.1:1/nope?connect_timeout=2"); err == nil {
		t.Fatal("unreachable host accepted")
	} else if !strings.Contains(err.Error(), "ping") {
		t.Fatalf("unreachable error = %v, want ping error", err)
	}
	if _, err := OpenPostgresStore(ctx, "://bad-dsn"); err == nil {
		t.Fatal("malformed DSN accepted")
	}
}

func TestContinue95NexaflowPGMigrateIdempotent(t *testing.T) {
	ctx := context.Background()
	dsn := Continue95NexaflowPGDSN(t)
	first, err := OpenPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer first.Close()
	second, err := OpenPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatalf("second open (migrate twice): %v", err)
	}
	defer second.Close()
	if _, err := second.GetDraft(ctx, "c95-never-existed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second store GetDraft missing: want ErrNotFound, got %v", err)
	}
}

func TestContinue95NexaflowPGNewPostgresStoreWrap(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("pgx", Continue95NexaflowPGDSN(t))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	store, err := NewPostgresStore(ctx, db)
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	defer store.Close()
	def := Continue95NexaflowPGDef(Continue95NexaflowPGID("wrap"))
	if err := store.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	got, err := store.GetDraft(ctx, def.ID)
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	if got.Name != def.Name || len(got.Nodes) != 2 {
		t.Fatalf("round-trip = %+v", got)
	}
}

func TestContinue95NexaflowPGDraftValidationAndNotFound(t *testing.T) {
	ctx := context.Background()
	store := Continue95NexaflowPGStore(t)
	if err := store.SaveDraft(ctx, Definition{}); err == nil {
		t.Fatal("empty definition id accepted")
	}
	if _, err := store.GetDraft(ctx, Continue95NexaflowPGID("missing")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetDraft missing: want ErrNotFound, got %v", err)
	}
}

func TestContinue95NexaflowPGDraftOverwriteAndListOrdering(t *testing.T) {
	ctx := context.Background()
	store := Continue95NexaflowPGStore(t)
	prefix := Continue95NexaflowPGID("list")
	ids := []string{prefix + "-a", prefix + "-b", prefix + "-c"}
	for _, id := range ids {
		if err := store.SaveDraft(ctx, Continue95NexaflowPGDef(id)); err != nil {
			t.Fatalf("SaveDraft %s: %v", id, err)
		}
	}
	// Overwrite the middle draft; the row must be replaced, not duplicated.
	over := Continue95NexaflowPGDef(ids[1])
	over.Name = "c95-overwritten"
	if err := store.SaveDraft(ctx, over); err != nil {
		t.Fatalf("overwrite SaveDraft: %v", err)
	}
	got, err := store.GetDraft(ctx, ids[1])
	if err != nil {
		t.Fatalf("GetDraft overwritten: %v", err)
	}
	if got.Name != "c95-overwritten" {
		t.Fatalf("overwrite name = %q", got.Name)
	}
	all, err := store.ListDrafts(ctx)
	if err != nil {
		t.Fatalf("ListDrafts: %v", err)
	}
	var mine []string
	for _, d := range all {
		if strings.HasPrefix(d.ID, prefix) {
			mine = append(mine, d.ID)
		}
	}
	if len(mine) != 3 || mine[0] != ids[0] || mine[1] != ids[1] || mine[2] != ids[2] {
		t.Fatalf("ListDrafts order = %v, want %v", mine, ids)
	}
}

func TestContinue95NexaflowPGPublishIdempotentAndSupersede(t *testing.T) {
	ctx := context.Background()
	store := Continue95NexaflowPGStore(t)
	def := Continue95NexaflowPGDef(Continue95NexaflowPGID("pub"))
	if err := store.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	rev1, err := store.Publish(ctx, def.ID, "tester")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if rev1.Version.Revision != 1 || rev1.Version.Status != DefinitionPublished || rev1.PublishedBy != "tester" {
		t.Fatalf("rev1 = %+v", rev1.Version)
	}
	again, err := store.Publish(ctx, def.ID, "tester")
	if err != nil {
		t.Fatalf("idempotent Publish: %v", err)
	}
	if again.Version.Revision != 1 {
		t.Fatalf("idempotent publish created revision %d", again.Version.Revision)
	}
	def.Name = "c95-pub-v2"
	if err := store.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft v2: %v", err)
	}
	rev2, err := store.Publish(ctx, def.ID, "tester2")
	if err != nil {
		t.Fatalf("Publish v2: %v", err)
	}
	if rev2.Version.Revision != 2 {
		t.Fatalf("rev2 = %d", rev2.Version.Revision)
	}
	old, err := store.GetRevision(ctx, def.ID, 1)
	if err != nil {
		t.Fatalf("GetRevision 1: %v", err)
	}
	if old.Version.Status != DefinitionSuperseded {
		t.Fatalf("rev1 status = %q, want superseded", old.Version.Status)
	}
	revs, err := store.ListRevisions(ctx, def.ID)
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(revs) != 2 || revs[0].Version.Revision != 1 || revs[1].Version.Revision != 2 {
		t.Fatalf("ListRevisions = %d entries", len(revs))
	}
	sup, err := store.SupersedeRevision(ctx, def.ID, 2)
	if err != nil {
		t.Fatalf("SupersedeRevision: %v", err)
	}
	if sup.Version.Status != DefinitionSuperseded {
		t.Fatalf("superseded status = %q", sup.Version.Status)
	}
}

func TestContinue95NexaflowPGPublishAndRevisionErrors(t *testing.T) {
	ctx := context.Background()
	store := Continue95NexaflowPGStore(t)
	missing := Continue95NexaflowPGID("missing")
	if _, err := store.Publish(ctx, missing, "tester"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Publish missing: want ErrNotFound, got %v", err)
	}
	if _, err := store.SupersedeRevision(ctx, missing, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SupersedeRevision missing: want ErrNotFound, got %v", err)
	}
	if _, err := store.GetRevision(ctx, missing, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRevision missing: want ErrNotFound, got %v", err)
	}
	revs, err := store.ListRevisions(ctx, missing)
	if err != nil || len(revs) != 0 {
		t.Fatalf("ListRevisions missing = %v, err %v", revs, err)
	}
	// Double supersede: the second attempt finds the row but it is no
	// longer published, so it is a state conflict, not a not-found.
	def := Continue95NexaflowPGDef(Continue95NexaflowPGID("dblsup"))
	if err := store.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if _, err := store.Publish(ctx, def.ID, "tester"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := store.SupersedeRevision(ctx, def.ID, 1); err != nil {
		t.Fatalf("first supersede: %v", err)
	}
	if _, err := store.SupersedeRevision(ctx, def.ID, 1); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("double supersede: want ErrStateConflict, got %v", err)
	}
}

func TestContinue95NexaflowPGCorruptNegativeRevision(t *testing.T) {
	ctx := context.Background()
	store := Continue95NexaflowPGStore(t)
	def := Continue95NexaflowPGDef(Continue95NexaflowPGID("negrev"))
	if err := store.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	db, err := sql.Open("pgx", Continue95NexaflowPGDSN(t))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO nexaroute_workflow_revisions (definition_id, revision, content_hash, definition, status, published_by) VALUES ($1, -1, 'x', $2, 'published', 'tester')`,
		def.ID, mustJSON(def)); err != nil {
		t.Fatalf("insert negative revision: %v", err)
	}
	if _, err := store.Publish(ctx, def.ID, "tester"); err == nil || !strings.Contains(err.Error(), "corrupt negative revision") {
		t.Fatalf("Publish negative revision: err = %v", err)
	}
	if _, err := store.ListRevisions(ctx, def.ID); err == nil || !strings.Contains(err.Error(), "corrupt negative revision") {
		t.Fatalf("ListRevisions negative revision: err = %v", err)
	}
}

func TestContinue95NexaflowPGRunCreateDuplicatesAndLookup(t *testing.T) {
	ctx := context.Background()
	store := Continue95NexaflowPGStore(t)
	defID := Continue95NexaflowPGID("rundef")
	runID := Continue95NexaflowPGID("run")
	idem := Continue95NexaflowPGID("idem")
	if err := store.CreateRun(ctx, Continue95NexaflowPGRun(runID, defID, 1, idem)); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := store.CreateRun(ctx, Continue95NexaflowPGRun(runID, defID, 1, Continue95NexaflowPGID("idem2"))); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate run id: want ErrAlreadyExists, got %v", err)
	}
	if err := store.CreateRun(ctx, Continue95NexaflowPGRun(Continue95NexaflowPGID("run"), defID, 1, idem)); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("duplicate idempotency key: want ErrIdempotencyConflict, got %v", err)
	}
	if err := store.CreateRun(ctx, Run{DefinitionID: defID}); err == nil {
		t.Fatal("empty run id accepted")
	}
	got, err := store.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.DefinitionID != defID || got.Principal != "tester" || got.Input["x"] != float64(1) {
		t.Fatalf("GetRun = %+v", got)
	}
	byIdem, err := store.GetRunByIdempotency(ctx, idem)
	if err != nil || byIdem.ID != runID {
		t.Fatalf("GetRunByIdempotency = %+v, err %v", byIdem, err)
	}
	if _, err := store.GetRun(ctx, Continue95NexaflowPGID("missing")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRun missing: want ErrNotFound, got %v", err)
	}
	if _, err := store.GetRunByIdempotency(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRunByIdempotency empty: want ErrNotFound, got %v", err)
	}
	if _, err := store.GetRunByIdempotency(ctx, Continue95NexaflowPGID("missing")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetRunByIdempotency missing: want ErrNotFound, got %v", err)
	}
	// A run created with no explicit state defaults to created.
	blank := Continue95NexaflowPGRun(Continue95NexaflowPGID("run"), defID, 1, Continue95NexaflowPGID("idem"))
	blank.State = ""
	if err := store.CreateRun(ctx, blank); err != nil {
		t.Fatalf("CreateRun blank state: %v", err)
	}
	blankGot, err := store.GetRun(ctx, blank.ID)
	if err != nil || blankGot.State != RunCreated {
		t.Fatalf("blank state run = %+v, err %v", blankGot, err)
	}
}

func TestContinue95NexaflowPGRunStateTransitions(t *testing.T) {
	ctx := context.Background()
	store := Continue95NexaflowPGStore(t)
	defID := Continue95NexaflowPGID("statedef")
	runID := Continue95NexaflowPGID("run")
	if err := store.CreateRun(ctx, Continue95NexaflowPGRun(runID, defID, 1, Continue95NexaflowPGID("idem"))); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := store.UpdateRunState(ctx, runID, RunRunning, RunRunning, "", ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("expected-state mismatch: want ErrStateConflict, got %v", err)
	}
	if _, err := store.UpdateRunState(ctx, runID, "", RunSucceeded, "", ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("created->succeeded: want ErrStateConflict, got %v", err)
	}
	running, err := store.UpdateRunState(ctx, runID, RunCreated, RunRunning, "", "")
	if err != nil {
		t.Fatalf("created->running: %v", err)
	}
	if running.State != RunRunning || running.StartedAt.IsZero() {
		t.Fatalf("running run = %+v", running)
	}
	same, err := store.UpdateRunState(ctx, runID, RunRunning, RunRunning, "", "")
	if err != nil || same.State != RunRunning {
		t.Fatalf("same-state running update = %+v, err %v", same, err)
	}
	done, err := store.UpdateRunState(ctx, runID, RunRunning, RunSucceeded, "", "")
	if err != nil {
		t.Fatalf("running->succeeded: %v", err)
	}
	if done.State != RunSucceeded || done.FinishedAt.IsZero() {
		t.Fatalf("succeeded run = %+v", done)
	}
	if _, err := store.UpdateRunState(ctx, runID, "", RunFailed, "x", "y"); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("terminal->failed: want ErrTerminalState, got %v", err)
	}
	if _, err := store.UpdateRunState(ctx, runID, "", RunSucceeded, "", ""); err != nil {
		t.Fatalf("same-state terminal update must be idempotent: %v", err)
	}
	if _, err := store.UpdateRunState(ctx, Continue95NexaflowPGID("missing"), "", RunRunning, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateRunState missing: want ErrNotFound, got %v", err)
	}
}

func TestContinue95NexaflowPGListRunsFilters(t *testing.T) {
	ctx := context.Background()
	store := Continue95NexaflowPGStore(t)
	defA := Continue95NexaflowPGID("defa")
	defB := Continue95NexaflowPGID("defb")
	ra := Continue95NexaflowPGID("run")
	rb := Continue95NexaflowPGID("run")
	rc := Continue95NexaflowPGID("run")
	for _, r := range []Run{
		Continue95NexaflowPGRun(ra, defA, 1, Continue95NexaflowPGID("idem")),
		Continue95NexaflowPGRun(rb, defA, 1, Continue95NexaflowPGID("idem")),
		Continue95NexaflowPGRun(rc, defB, 1, Continue95NexaflowPGID("idem")),
	} {
		if err := store.CreateRun(ctx, r); err != nil {
			t.Fatalf("CreateRun %s: %v", r.ID, err)
		}
	}
	byDef, err := store.ListRuns(ctx, RunFilter{DefinitionID: defA})
	if err != nil {
		t.Fatalf("ListRuns by definition: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range byDef {
		seen[r.ID] = true
	}
	if !seen[ra] || !seen[rb] || seen[rc] {
		t.Fatalf("ListRuns by definition = %v", seen)
	}
	byState, err := store.ListRuns(ctx, RunFilter{DefinitionID: defA, State: RunCreated})
	if err != nil || len(byState) != 2 {
		t.Fatalf("ListRuns by state = %d, err %v", len(byState), err)
	}
	limited, err := store.ListRuns(ctx, RunFilter{DefinitionID: defA, Limit: 1})
	if err != nil || len(limited) != 1 {
		t.Fatalf("ListRuns limit = %d, err %v", len(limited), err)
	}
	none, err := store.ListRuns(ctx, RunFilter{DefinitionID: Continue95NexaflowPGID("missing")})
	if err != nil || len(none) != 0 {
		t.Fatalf("ListRuns missing definition = %v, err %v", none, err)
	}
}

func TestContinue95NexaflowPGNodeExecutionLifecycle(t *testing.T) {
	ctx := context.Background()
	store := Continue95NexaflowPGStore(t)
	defID := Continue95NexaflowPGID("nodedef")
	runID := Continue95NexaflowPGID("run")
	if err := store.CreateRun(ctx, Continue95NexaflowPGRun(runID, defID, 1, Continue95NexaflowPGID("idem"))); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := store.AppendNodeExecution(ctx, NodeExecution{RunID: Continue95NexaflowPGID("missing"), NodeID: "work"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Append missing run: want ErrNotFound, got %v", err)
	}
	first, err := store.AppendNodeExecution(ctx, NodeExecution{RunID: runID, NodeID: "work", Input: map[string]any{"a": float64(1)}})
	if err != nil {
		t.Fatalf("Append first: %v", err)
	}
	if first.Attempt != 0 || first.State != NodePending || first.IdempotencyKey == "" {
		t.Fatalf("first attempt = %+v", first)
	}
	second, err := store.AppendNodeExecution(ctx, NodeExecution{RunID: runID, NodeID: "work"})
	if err != nil {
		t.Fatalf("Append second: %v", err)
	}
	if second.Attempt != 1 {
		t.Fatalf("second attempt = %d", second.Attempt)
	}
	if _, err := store.GetNodeExecutions(ctx, Continue95NexaflowPGID("missing")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetNodeExecutions missing: want ErrNotFound, got %v", err)
	}
	execs, err := store.GetNodeExecutions(ctx, runID)
	if err != nil {
		t.Fatalf("GetNodeExecutions: %v", err)
	}
	if len(execs) != 2 || execs[0].Attempt != 0 || execs[1].Attempt != 1 {
		t.Fatalf("executions = %+v", execs)
	}
	// Expected-state mismatch against the current (second) attempt.
	bad := second
	bad.State = NodeClaimed
	if _, err := store.SaveNodeExecution(ctx, bad, NodeClaimed); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("SaveNodeExecution mismatch: want ErrStateConflict, got %v", err)
	}
	// Attempt-99 resolution: the store resolves the current attempt.
	resolved := second
	resolved.Attempt = 99
	resolved.State = NodeClaimed
	saved, err := store.SaveNodeExecution(ctx, resolved, NodePending)
	if err != nil {
		t.Fatalf("SaveNodeExecution resolved: %v", err)
	}
	if saved.Attempt != 1 || saved.State != NodeClaimed {
		t.Fatalf("resolved save = %+v", saved)
	}
	running := saved
	running.State = NodeRunning
	if _, err := store.SaveNodeExecution(ctx, running, NodeClaimed); err != nil {
		t.Fatalf("claimed->running: %v", err)
	}
	done := running
	done.State = NodeSucceeded
	done.Output = map[string]any{"ok": true}
	if _, err := store.SaveNodeExecution(ctx, done, NodeRunning); err != nil {
		t.Fatalf("running->succeeded: %v", err)
	}
	failed := done
	failed.State = NodeFailed
	if _, err := store.SaveNodeExecution(ctx, failed, NodeSucceeded); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("terminal node update: want ErrTerminalState, got %v", err)
	}
	if _, err := store.SaveNodeExecution(ctx, NodeExecution{RunID: runID, NodeID: "nope", Attempt: 0, State: NodeClaimed}, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SaveNodeExecution missing node: want ErrNotFound, got %v", err)
	}
}

func TestContinue95NexaflowPGClaimAndLease(t *testing.T) {
	ctx := context.Background()
	store := Continue95NexaflowPGStore(t)
	defID := Continue95NexaflowPGID("claimdef")
	runID := Continue95NexaflowPGID("run")
	if err := store.CreateRun(ctx, Continue95NexaflowPGRun(runID, defID, 1, Continue95NexaflowPGID("idem"))); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, _, err := store.TryClaim(ctx, runID, "nope", "owner1", time.Hour, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("TryClaim missing: want ErrNotFound, got %v", err)
	}
	if _, err := store.AppendNodeExecution(ctx, NodeExecution{RunID: runID, NodeID: "work"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	claimed, ok, err := store.TryClaim(ctx, runID, "work", "owner1", time.Hour, time.Now())
	if err != nil || !ok {
		t.Fatalf("first claim = ok %v, err %v", ok, err)
	}
	if claimed.State != NodeClaimed || claimed.LeaseOwner != "owner1" || claimed.LeaseUntil.IsZero() {
		t.Fatalf("claimed = %+v", claimed)
	}
	if _, ok, err := store.TryClaim(ctx, runID, "work", "owner2", time.Hour, time.Now()); err != nil || ok {
		t.Fatalf("foreign live lease claim = ok %v, err %v", ok, err)
	}
	// Wrong-owner release is a no-op and must not clear owner1's lease.
	if err := store.ReleaseLease(ctx, runID, "work", "owner2"); err != nil {
		t.Fatalf("wrong-owner ReleaseLease: %v", err)
	}
	if _, ok, err := store.TryClaim(ctx, runID, "work", "owner2", time.Hour, time.Now()); err != nil || ok {
		t.Fatalf("claim after wrong-owner release = ok %v, err %v", ok, err)
	}
	if err := store.ReleaseLease(ctx, runID, "work", "owner1"); err != nil {
		t.Fatalf("owner ReleaseLease: %v", err)
	}
	if _, ok, err := store.TryClaim(ctx, runID, "work", "owner2", time.Hour, time.Now()); err != nil || !ok {
		t.Fatalf("claim after release = ok %v, err %v", ok, err)
	}
	// A terminal node is not claimable: false, no error.
	execs, err := store.GetNodeExecutions(ctx, runID)
	if err != nil {
		t.Fatalf("GetNodeExecutions: %v", err)
	}
	cur := execs[len(execs)-1]
	cur.State = NodeRunning
	if _, err := store.SaveNodeExecution(ctx, cur, NodeClaimed); err != nil {
		t.Fatalf("claimed->running: %v", err)
	}
	cur.State = NodeSucceeded
	if _, err := store.SaveNodeExecution(ctx, cur, NodeRunning); err != nil {
		t.Fatalf("running->succeeded: %v", err)
	}
	if _, ok, err := store.TryClaim(ctx, runID, "work", "owner3", time.Hour, time.Now()); err != nil || ok {
		t.Fatalf("claim terminal node = ok %v, err %v", ok, err)
	}
}

func TestContinue95NexaflowPGConcurrentClaim(t *testing.T) {
	ctx := context.Background()
	store := Continue95NexaflowPGStore(t)
	defID := Continue95NexaflowPGID("raceclaim")
	runID := Continue95NexaflowPGID("run")
	if err := store.CreateRun(ctx, Continue95NexaflowPGRun(runID, defID, 1, Continue95NexaflowPGID("idem"))); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := store.AppendNodeExecution(ctx, NodeExecution{RunID: runID, NodeID: "work"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	var wins int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, ok, err := store.TryClaim(ctx, runID, "work", fmt.Sprintf("owner-%d", i), time.Hour, time.Now())
			if err != nil {
				t.Errorf("TryClaim: %v", err)
				return
			}
			if ok {
				atomic.AddInt64(&wins, 1)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("concurrent claims: %d winners, want exactly 1", wins)
	}
}

func TestContinue95NexaflowPGApprovals(t *testing.T) {
	ctx := context.Background()
	store := Continue95NexaflowPGStore(t)
	runID := Continue95NexaflowPGID("run")
	nodeID := "approve-node"
	if _, err := store.RecordApproval(ctx, Approval{RunID: runID, NodeID: nodeID, Decision: "maybe"}); err == nil {
		t.Fatal("invalid decision accepted")
	}
	if _, err := store.RecordApproval(ctx, Approval{RunID: runID, NodeID: nodeID, Requester: "alice", Approver: "alice", Decision: "approved"}); err == nil {
		t.Fatal("self-approval accepted")
	}
	first, err := store.RecordApproval(ctx, Approval{RunID: runID, NodeID: nodeID, Requester: "alice", Approver: "bob", Decision: "approved"})
	if err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}
	if first.DecidedAt.IsZero() {
		t.Fatal("DecidedAt not set")
	}
	again, err := store.RecordApproval(ctx, Approval{RunID: runID, NodeID: nodeID, Requester: "alice", Approver: "bob", Decision: "approved"})
	if err != nil {
		t.Fatalf("identical re-record: %v", err)
	}
	// TIMESTAMPTZ stores microseconds, so compare at that precision.
	if !again.DecidedAt.Equal(first.DecidedAt.Truncate(time.Microsecond)) {
		t.Fatalf("idempotent re-record changed DecidedAt: %v vs %v", again.DecidedAt, first.DecidedAt)
	}
	if _, err := store.RecordApproval(ctx, Approval{RunID: runID, NodeID: nodeID, Requester: "alice", Approver: "bob", Decision: "rejected"}); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("conflicting decision: want ErrApprovalConflict, got %v", err)
	}
	got, err := store.GetApproval(ctx, runID, nodeID)
	if err != nil {
		t.Fatalf("GetApproval: %v", err)
	}
	if got.Decision != "approved" || got.Approver != "bob" || got.Requester != "alice" {
		t.Fatalf("GetApproval = %+v", got)
	}
	if _, err := store.GetApproval(ctx, runID, "nope"); !errors.Is(err, ErrNoApprovalRecord) {
		t.Fatalf("GetApproval missing: want ErrNoApprovalRecord, got %v", err)
	}
}

func TestContinue95NexaflowPGPrune(t *testing.T) {
	ctx := context.Background()
	store := Continue95NexaflowPGStore(t)
	defID := Continue95NexaflowPGID("prunedef")
	oldID := Continue95NexaflowPGID("run")
	recentID := Continue95NexaflowPGID("run")
	liveID := Continue95NexaflowPGID("run")
	old := Continue95NexaflowPGRun(oldID, defID, 1, Continue95NexaflowPGID("idem"))
	old.State = RunSucceeded
	old.FinishedAt = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := store.CreateRun(ctx, old); err != nil {
		t.Fatalf("CreateRun old: %v", err)
	}
	if _, err := store.AppendNodeExecution(ctx, NodeExecution{RunID: oldID, NodeID: "work", State: NodeSucceeded}); err != nil {
		t.Fatalf("Append old node: %v", err)
	}
	recent := Continue95NexaflowPGRun(recentID, defID, 1, Continue95NexaflowPGID("idem"))
	recent.State = RunSucceeded
	recent.FinishedAt = time.Now()
	if err := store.CreateRun(ctx, recent); err != nil {
		t.Fatalf("CreateRun recent: %v", err)
	}
	live := Continue95NexaflowPGRun(liveID, defID, 1, Continue95NexaflowPGID("idem"))
	live.State = RunRunning
	live.FinishedAt = time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := store.CreateRun(ctx, live); err != nil {
		t.Fatalf("CreateRun live: %v", err)
	}
	n, err := store.Prune(ctx, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("Prune = %d, want 1 (only the old terminal run)", n)
	}
	if _, err := store.GetRun(ctx, oldID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pruned run still present: %v", err)
	}
	if _, err := store.GetRun(ctx, recentID); err != nil {
		t.Fatalf("recent terminal run pruned: %v", err)
	}
	if _, err := store.GetRun(ctx, liveID); err != nil {
		t.Fatalf("non-terminal run pruned: %v", err)
	}
	n, err = store.Prune(ctx, time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || n != 0 {
		t.Fatalf("second Prune = %d, err %v", n, err)
	}
}

func TestContinue95NexaflowPGClosedPool(t *testing.T) {
	ctx := context.Background()
	dsn := Continue95NexaflowPGDSN(t)
	store, err := OpenPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatalf("OpenPostgresStore: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Every method must surface the closed-pool error, not panic.
	id := Continue95NexaflowPGID("closed")
	calls := []struct {
		name string
		call func() error
	}{
		{"GetDraft", func() error { _, err := store.GetDraft(ctx, id); return err }},
		{"ListDrafts", func() error { _, err := store.ListDrafts(ctx); return err }},
		{"Publish", func() error { _, err := store.Publish(ctx, id, "t"); return err }},
		{"SupersedeRevision", func() error { _, err := store.SupersedeRevision(ctx, id, 1); return err }},
		{"GetRevision", func() error { _, err := store.GetRevision(ctx, id, 1); return err }},
		{"ListRevisions", func() error { _, err := store.ListRevisions(ctx, id); return err }},
		{"CreateRun", func() error { return store.CreateRun(ctx, Continue95NexaflowPGRun(id, id, 1, "")) }},
		{"GetRun", func() error { _, err := store.GetRun(ctx, id); return err }},
		{"GetRunByIdempotency", func() error { _, err := store.GetRunByIdempotency(ctx, id); return err }},
		{"UpdateRunState", func() error { _, err := store.UpdateRunState(ctx, id, "", RunRunning, "", ""); return err }},
		{"ListRuns", func() error { _, err := store.ListRuns(ctx, RunFilter{}); return err }},
		{"Prune", func() error { _, err := store.Prune(ctx, time.Now()); return err }},
		{"AppendNodeExecution", func() error {
			_, err := store.AppendNodeExecution(ctx, NodeExecution{RunID: id, NodeID: "n"})
			return err
		}},
		{"SaveNodeExecution", func() error {
			_, err := store.SaveNodeExecution(ctx, NodeExecution{RunID: id, NodeID: "n", State: NodePending}, "")
			return err
		}},
		{"GetNodeExecutions", func() error { _, err := store.GetNodeExecutions(ctx, id); return err }},
		{"TryClaim", func() error { _, _, err := store.TryClaim(ctx, id, "n", "w", time.Minute, time.Now()); return err }},
		{"ReleaseLease", func() error { return store.ReleaseLease(ctx, id, "n", "w") }},
		{"RecordApproval", func() error {
			_, err := store.RecordApproval(ctx, Approval{RunID: id, NodeID: "n", Requester: "a", Approver: "b", Decision: "approved"})
			return err
		}},
		{"GetApproval", func() error { _, err := store.GetApproval(ctx, id, "n"); return err }},
	}
	for _, tc := range calls {
		if err := tc.call(); err == nil {
			t.Fatalf("closed pool %s: no error", tc.name)
		}
	}
	// A cancelled context fails the open before any migration runs.
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := OpenPostgresStore(cancelCtx, dsn); err == nil {
		t.Fatal("OpenPostgresStore cancelled context accepted")
	}
}

func TestContinue95NexaflowPGCorruptRows(t *testing.T) {
	ctx := context.Background()
	dsn := Continue95NexaflowPGDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	// Cleanup uses a separate handle: t.Cleanup runs after the deferred
	// db.Close above, so deleting through db itself would silently fail
	// and leak corrupt rows into later tests' ListDrafts.
	cleanDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open cleanup: %v", err)
	}
	var draftID, revID, nodeRun string
	var runIDs []string
	t.Cleanup(func() {
		if draftID != "" {
			_, _ = cleanDB.Exec(`DELETE FROM nexaroute_workflow_definitions WHERE definition_id = $1`, draftID)
		}
		if revID != "" {
			_, _ = cleanDB.Exec(`DELETE FROM nexaroute_workflow_revisions WHERE definition_id = $1`, revID)
		}
		for _, id := range runIDs {
			_, _ = cleanDB.Exec(`DELETE FROM nexaroute_workflow_runs WHERE run_id = $1`, id)
		}
		if nodeRun != "" {
			_, _ = cleanDB.Exec(`DELETE FROM nexaroute_workflow_nodes WHERE run_id = $1`, nodeRun)
		}
		_ = cleanDB.Close()
	})
	store := Continue95NexaflowPGStore(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("direct SQL %q: %v", q, err)
		}
	}
	// Corrupt draft: GetDraft and Publish fail to unmarshal, and
	// ListDrafts fails when it reaches the corrupt row.
	draftID = Continue95NexaflowPGID("corruptdraft")
	exec(`INSERT INTO nexaroute_workflow_definitions (definition_id, draft) VALUES ($1, CAST($2 AS jsonb))`, draftID, `"x"`)
	if _, err := store.GetDraft(ctx, draftID); err == nil {
		t.Fatal("GetDraft corrupt draft accepted")
	}
	if _, err := store.Publish(ctx, draftID, "t"); err == nil {
		t.Fatal("Publish corrupt draft accepted")
	}
	if _, err := store.ListDrafts(ctx); err == nil {
		t.Fatal("ListDrafts corrupt draft accepted")
	}
	// Corrupt revision definition: GetRevision and ListRevisions fail.
	revID = Continue95NexaflowPGID("corruptrev")
	exec(`INSERT INTO nexaroute_workflow_revisions (definition_id, revision, content_hash, definition, status) VALUES ($1, 1, 'x', CAST($2 AS jsonb), 'published')`, revID, `"x"`)
	if _, err := store.GetRevision(ctx, revID, 1); err == nil {
		t.Fatal("GetRevision corrupt definition accepted")
	}
	if _, err := store.ListRevisions(ctx, revID); err == nil {
		t.Fatal("ListRevisions corrupt definition accepted")
	}
	// Corrupt run fields, one per row: definition, roles, input.
	for _, tc := range []struct {
		name, def, roles, input string
	}{
		{"def", `"x"`, `[]`, `{}`},
		{"roles", `{"id":"d"}`, `"x"`, `{}`},
		{"input", `{"id":"d"}`, `[]`, `"x"`},
	} {
		runID := Continue95NexaflowPGID("corruptrun-" + tc.name)
		runIDs = append(runIDs, runID)
		exec(`INSERT INTO nexaroute_workflow_runs (run_id, definition_id, revision, definition, state, principal_roles, input) VALUES ($1, $2, 1, CAST($3 AS jsonb), 'created', CAST($4 AS jsonb), CAST($5 AS jsonb))`,
			runID, "d", tc.def, tc.roles, tc.input)
		if _, err := store.GetRun(ctx, runID); err == nil {
			t.Fatalf("GetRun corrupt %s accepted", tc.name)
		}
	}
	// Corrupt node payload: GetNodeExecutions, TryClaim, and
	// SaveNodeExecution all fail to unmarshal it.
	nodeRun = Continue95NexaflowPGID("corruptnode")
	exec(`INSERT INTO nexaroute_workflow_nodes (run_id, node_id, attempt, payload, state) VALUES ($1, 'n', 0, CAST($2 AS jsonb), 'pending')`, nodeRun, `"x"`)
	if _, err := store.GetNodeExecutions(ctx, nodeRun); err == nil {
		t.Fatal("GetNodeExecutions corrupt payload accepted")
	}
	if _, _, err := store.TryClaim(ctx, nodeRun, "n", "w", time.Minute, time.Now()); err == nil {
		t.Fatal("TryClaim corrupt payload accepted")
	}
	if _, err := store.SaveNodeExecution(ctx, NodeExecution{RunID: nodeRun, NodeID: "n", Attempt: 0, State: NodePending}, ""); err == nil {
		t.Fatal("SaveNodeExecution corrupt payload accepted")
	}
}

func TestContinue95NexaflowPGMustJSONFallback(t *testing.T) {
	ctx := context.Background()
	store := Continue95NexaflowPGStore(t)
	// A func-valued config cannot marshal: the whole draft falls back
	// to "{}", so the save succeeds and the draft reads back empty.
	defID := Continue95NexaflowPGID("funcdef")
	def := Continue95NexaflowPGDef(defID)
	def.Nodes[1].Config = map[string]any{"f": func() {}}
	if err := store.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft func config: %v", err)
	}
	got, err := store.GetDraft(ctx, defID)
	if err != nil {
		t.Fatalf("GetDraft func config: %v", err)
	}
	if got.ID != "" {
		t.Fatalf("GetDraft func config ID = %q, want empty (fallback {})", got.ID)
	}
	// Same fallback for a func-valued run input.
	runID := Continue95NexaflowPGID("funcrun")
	run := Continue95NexaflowPGRun(runID, defID, 1, "")
	run.Input = map[string]any{"f": func() {}}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun func input: %v", err)
	}
	gotRun, err := store.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun func input: %v", err)
	}
	if len(gotRun.Input) != 0 {
		t.Fatalf("GetRun func input = %v, want empty (fallback {})", gotRun.Input)
	}
}

func TestContinue95NexaflowPGDeliveryLedger(t *testing.T) {
	ctx := context.Background()
	dsn := Continue95NexaflowPGDSN(t)
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	cleanDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open cleanup: %v", err)
	}
	var deliveryKey string
	t.Cleanup(func() {
		if deliveryKey != "" {
			_, _ = cleanDB.Exec(`DELETE FROM nexaroute_workflow_deliveries WHERE delivery_key = $1`, deliveryKey)
		}
		_ = cleanDB.Close()
	})
	if err := EnsurePostgresDeliverySchema(ctx, db); err != nil {
		t.Fatalf("EnsurePostgresDeliverySchema: %v", err)
	}
	if err := EnsurePostgresDeliverySchema(ctx, db); err != nil {
		t.Fatalf("EnsurePostgresDeliverySchema idempotent: %v", err)
	}
	ledger, err := NewPostgresDeliveryLedger(db)
	if err != nil {
		t.Fatalf("NewPostgresDeliveryLedger: %v", err)
	}
	if _, err := ledger.Claim(ctx, ""); err == nil {
		t.Fatal("delivery Claim empty key accepted")
	}
	key := Continue95NexaflowPGID("delivery")
	deliveryKey = key
	if ok, err := ledger.Claim(ctx, key); err != nil || !ok {
		t.Fatalf("delivery Claim first = %v, %v", ok, err)
	}
	if ok, err := ledger.Claim(ctx, key); err != nil || ok {
		t.Fatalf("delivery Claim duplicate = %v, %v", ok, err)
	}
}

func TestContinue95NexaflowPGEngineRetry(t *testing.T) {
	ctx := context.Background()
	store := Continue95NexaflowPGStore(t)
	calls := 0
	flaky := testSpec("test.flaky", []Field{{Name: "payload", Type: "object"}}, func(context.Context, *RunContext) (map[string]any, error) {
		calls++
		if calls == 1 {
			return nil, NewNodeError("boom", true, "boom")
		}
		return map[string]any{"v": "ok"}, nil
	})
	flaky.Retry = RetryPolicy{MaxAttempts: 2, RetryableCodes: []string{"boom"}}
	reg := testRegistry(t, flaky)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"a", "test.flaky", nil})
	eng := mustEngine(t, store, reg)
	pub := publishDef(t, store, def)
	run, err := eng.CreateRun(ctx, pub, principal(), runInput(), "")
	if err != nil {
		t.Fatalf("CreateRun pg retry: %v", err)
	}
	final, err := eng.Execute(ctx, run.ID)
	if err != nil || final.State != RunSucceeded {
		t.Fatalf("Execute pg retry = %q, err = %v", final.State, err)
	}
	execs, err := store.GetNodeExecutions(ctx, run.ID)
	if err != nil {
		t.Fatalf("GetNodeExecutions pg retry: %v", err)
	}
	var failed, succeeded bool
	for _, e := range execs {
		if e.NodeID != "a" {
			continue
		}
		if e.Attempt == 0 && e.State == NodeFailed {
			failed = true
		}
		if e.Attempt == 1 && e.State == NodeSucceeded {
			succeeded = true
		}
	}
	if !failed || !succeeded {
		t.Fatalf("pg retry attempts failed=%v succeeded=%v: %+v", failed, succeeded, execs)
	}
}
