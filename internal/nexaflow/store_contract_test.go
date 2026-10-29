package nexaflow

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/authz"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// runStoreContract exercises the Store interface identically for every
// implementation. pg=true selects the PostgreSQL-only paths (server
// clock for leases, real sleeps instead of injected time).
func runStoreContract(t *testing.T, store Store, pg bool) {
	t.Helper()
	ctx := context.Background()

	def := Definition{
		SchemaVersion: DefinitionSchemaVersion,
		ID:            "wf-contract",
		Name:          "contract",
		Nodes: []DefinitionNode{
			{ID: "start", NodeID: "nexaroute.trigger.manual", Version: SupportedVersion},
			{ID: "work", NodeID: "test.echo", Version: SupportedVersion, Config: map[string]any{"k": "v"}},
		},
		Edges: []DefinitionEdge{{ID: "e1", From: "start", To: "work"}},
		Triggers: []DefinitionTrigger{
			{Name: "start", Kind: TriggerManual, Config: map[string]any{}},
		},
	}

	// Draft round-trip + mutation isolation.
	if err := store.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	got, err := store.GetDraft(ctx, "wf-contract")
	if err != nil {
		t.Fatalf("GetDraft: %v", err)
	}
	got.Nodes[0].Config = map[string]any{"mutated": true}
	got2, err := store.GetDraft(ctx, "wf-contract")
	if err != nil {
		t.Fatalf("GetDraft again: %v", err)
	}
	if got2.Nodes[1].Config["k"] != "v" {
		t.Fatalf("draft mutated through returned value: %+v", got2.Nodes[1].Config)
	}
	if _, err := store.GetDraft(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetDraft missing: want ErrNotFound, got %v", err)
	}

	// Publish: idempotent, then supersede on new content.
	rev1, err := store.Publish(ctx, "wf-contract", "tester")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if rev1.Version.Revision != 1 || rev1.Version.Status != DefinitionPublished {
		t.Fatalf("rev1 = %+v", rev1.Version)
	}
	rev1b, err := store.Publish(ctx, "wf-contract", "tester")
	if err != nil {
		t.Fatalf("Publish again: %v", err)
	}
	if rev1b.Version.Revision != 1 {
		t.Fatalf("idempotent publish created revision %d", rev1b.Version.Revision)
	}
	def.Name = "contract-v2"
	if err := store.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft v2: %v", err)
	}
	rev2, err := store.Publish(ctx, "wf-contract", "tester")
	if err != nil {
		t.Fatalf("Publish v2: %v", err)
	}
	if rev2.Version.Revision != 2 {
		t.Fatalf("rev2 = %d", rev2.Version.Revision)
	}
	old, err := store.GetRevision(ctx, "wf-contract", 1)
	if err != nil {
		t.Fatalf("GetRevision 1: %v", err)
	}
	if old.Version.Status != DefinitionSuperseded {
		t.Fatalf("rev1 status = %q, want superseded", old.Version.Status)
	}
	if old.Definition.Name != "contract" {
		t.Fatalf("rev1 snapshot changed: %q", old.Definition.Name)
	}
	list, err := store.ListRevisions(ctx, "wf-contract")
	if err != nil {
		t.Fatalf("ListRevisions: %v", err)
	}
	if len(list) != 2 || list[0].Version.Revision != 1 || list[1].Version.Revision != 2 {
		t.Fatalf("ListRevisions = %d entries", len(list))
	}
	sup, err := store.SupersedeRevision(ctx, "wf-contract", 2)
	if err != nil {
		t.Fatalf("SupersedeRevision: %v", err)
	}
	if sup.Version.Status != DefinitionSuperseded {
		t.Fatalf("superseded status = %q", sup.Version.Status)
	}
	if _, err := store.SupersedeRevision(ctx, "wf-contract", 2); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("double supersede: want ErrStateConflict, got %v", err)
	}
	if _, err := store.SupersedeRevision(ctx, "wf-contract", 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("supersede missing: want ErrNotFound, got %v", err)
	}
	if _, err := store.Publish(ctx, "missing-def", "tester"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("publish missing draft: want ErrNotFound, got %v", err)
	}

	// Runs: create, idempotency, transitions, filters, prune.
	run := Run{
		ID: "run-contract-1", DefinitionID: "wf-contract", Revision: 1,
		Definition: rev1.Definition, State: RunCreated, IdempotencyKey: "idem-1",
		Principal: "alice", PrincipalRoles: []authz.Role{"owner"},
		Input: map[string]any{"payload": map[string]any{"x": 1}},
	}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := store.CreateRun(ctx, run); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate run: want ErrAlreadyExists, got %v", err)
	}
	dup := run
	dup.ID = "run-contract-2"
	if err := store.CreateRun(ctx, dup); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("duplicate idempotency key: want ErrIdempotencyConflict, got %v", err)
	}
	byIdem, err := store.GetRunByIdempotency(ctx, "idem-1")
	if err != nil {
		t.Fatalf("GetRunByIdempotency: %v", err)
	}
	if byIdem.ID != run.ID || byIdem.PrincipalRoles[0] != "owner" {
		t.Fatalf("GetRunByIdempotency = %+v", byIdem)
	}
	if _, err := store.UpdateRunState(ctx, run.ID, RunSucceeded, RunRunning, "", ""); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("wrong expected state: want ErrStateConflict, got %v", err)
	}
	moved, err := store.UpdateRunState(ctx, run.ID, RunCreated, RunRunning, "", "")
	if err != nil {
		t.Fatalf("created->running: %v", err)
	}
	if moved.StartedAt.IsZero() {
		t.Fatal("StartedAt not set on running")
	}
	same, err := store.UpdateRunState(ctx, run.ID, "", RunRunning, "", "")
	if err != nil || same.State != RunRunning {
		t.Fatalf("same-state run transition: %v %v", same.State, err)
	}
	finished, err := store.UpdateRunState(ctx, run.ID, "", RunSucceeded, "", "")
	if err != nil {
		t.Fatalf("running->succeeded: %v", err)
	}
	if finished.FinishedAt.IsZero() {
		t.Fatal("FinishedAt not set on terminal")
	}
	if _, err := store.UpdateRunState(ctx, run.ID, "", RunFailed, "", ""); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("terminal move: want ErrTerminalState, got %v", err)
	}
	runs, err := store.ListRuns(ctx, RunFilter{DefinitionID: "wf-contract", State: RunSucceeded})
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns filtered = %d, %v", len(runs), err)
	}
	// Prune: backdate the finished run by moving it through a fresh
	// terminal run finished long ago is not possible via the API, so
	// prune with a future cutoff and check the non-terminal run stays.
	run2 := Run{ID: "run-contract-3", DefinitionID: "wf-contract", Revision: 1, Definition: rev1.Definition, State: RunCreated}
	if err := store.CreateRun(ctx, run2); err != nil {
		t.Fatalf("CreateRun 2: %v", err)
	}
	removed, err := store.Prune(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if removed != 1 {
		t.Fatalf("Prune removed %d, want 1 (only the terminal run)", removed)
	}
	if _, err := store.GetRun(ctx, run2.ID); err != nil {
		t.Fatalf("non-terminal run pruned: %v", err)
	}

	// Node executions: attempts, claims, transitions, approvals.
	n1, err := store.AppendNodeExecution(ctx, NodeExecution{RunID: run2.ID, NodeID: "work", State: NodePending})
	if err != nil {
		t.Fatalf("AppendNodeExecution: %v", err)
	}
	if n1.Attempt != 0 || n1.IdempotencyKey != NodeIDKey(run2.ID, "work", 0) {
		t.Fatalf("attempt 0 = %+v", n1)
	}
	n2, err := store.AppendNodeExecution(ctx, NodeExecution{RunID: run2.ID, NodeID: "work", State: NodePending})
	if err != nil {
		t.Fatalf("AppendNodeExecution 2: %v", err)
	}
	if n2.Attempt != 1 {
		t.Fatalf("attempt 1 = %d", n2.Attempt)
	}
	// The current attempt is 1; claim it.
	now := time.Now()
	claimed, ok, err := store.TryClaim(ctx, run2.ID, "work", "w1", time.Minute, now)
	if err != nil || !ok {
		t.Fatalf("TryClaim: %v %v", claimed, err)
	}
	if claimed.Attempt != 1 || claimed.LeaseOwner != "w1" {
		t.Fatalf("claimed = %+v", claimed)
	}
	if _, ok, err := store.TryClaim(ctx, run2.ID, "work", "w2", time.Minute, now); err != nil || ok {
		t.Fatalf("second claim while leased: ok=%v err=%v", ok, err)
	}
	if _, ok, err := store.TryClaim(ctx, run2.ID, "work", "w1", time.Minute, now); err != nil || !ok {
		t.Fatalf("same-owner reclaim: ok=%v err=%v", ok, err)
	}
	if err := store.ReleaseLease(ctx, run2.ID, "work", "w2"); err != nil {
		t.Fatalf("foreign release: %v", err)
	}
	if _, ok, err := store.TryClaim(ctx, run2.ID, "work", "w2", time.Minute, now); err != nil || ok {
		t.Fatalf("claim after foreign release: ok=%v err=%v", ok, err)
	}
	if err := store.ReleaseLease(ctx, run2.ID, "work", "w1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	// Expiry: memory/file use the passed clock; PG uses the server clock.
	if pg {
		if _, ok, err := store.TryClaim(ctx, run2.ID, "work", "w1", time.Second, now); err != nil || !ok {
			t.Fatalf("short claim: ok=%v err=%v", ok, err)
		}
		time.Sleep(1100 * time.Millisecond)
		if _, ok, err := store.TryClaim(ctx, run2.ID, "work", "w2", time.Minute, now); err != nil || !ok {
			t.Fatalf("claim after server expiry: ok=%v err=%v", ok, err)
		}
	} else {
		if _, ok, err := store.TryClaim(ctx, run2.ID, "work", "w1", time.Second, now); err != nil || !ok {
			t.Fatalf("short claim: ok=%v err=%v", ok, err)
		}
		if _, ok, err := store.TryClaim(ctx, run2.ID, "work", "w2", time.Minute, now.Add(2*time.Second)); err != nil || !ok {
			t.Fatalf("claim after expiry: ok=%v err=%v", ok, err)
		}
	}
	if err := store.ReleaseLease(ctx, run2.ID, "work", "w2"); err != nil {
		t.Fatalf("release 2: %v", err)
	}
	claimed, ok, err = store.TryClaim(ctx, run2.ID, "work", "w2", time.Minute, now)
	if err != nil || !ok {
		t.Fatalf("reclaim: %v %v", claimed, err)
	}
	running := claimed
	running.State = NodeRunning
	if _, err := store.SaveNodeExecution(ctx, running, NodeClaimed); err != nil {
		t.Fatalf("claimed->running: %v", err)
	}
	bad := running
	bad.State = NodeSucceeded
	if _, err := store.SaveNodeExecution(ctx, bad, NodeClaimed); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("wrong expected node state: want ErrStateConflict, got %v", err)
	}
	done := running
	done.State = NodeSucceeded
	done.Output = map[string]any{"ok": true}
	if _, err := store.SaveNodeExecution(ctx, done, NodeRunning); err != nil {
		t.Fatalf("running->succeeded: %v", err)
	}
	if _, err := store.SaveNodeExecution(ctx, done, NodeSucceeded); err != nil {
		t.Fatalf("same-state node save should be idempotent: %v", err)
	}
	moved2 := done
	moved2.State = NodeFailed
	if _, err := store.SaveNodeExecution(ctx, moved2, NodeSucceeded); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("terminal node move: want ErrTerminalState, got %v", err)
	}
	execs, err := store.GetNodeExecutions(ctx, run2.ID)
	if err != nil || len(execs) != 2 {
		t.Fatalf("GetNodeExecutions = %d, %v", len(execs), err)
	}

	// Approvals.
	if _, err := store.GetApproval(ctx, run2.ID, "work"); !errors.Is(err, ErrNoApprovalRecord) {
		t.Fatalf("missing approval: want ErrNoApprovalRecord, got %v", err)
	}
	appr, err := store.RecordApproval(ctx, Approval{RunID: run2.ID, NodeID: "work", Requester: "alice", Approver: "op1", Decision: "approved"})
	if err != nil {
		t.Fatalf("RecordApproval: %v", err)
	}
	if appr.DecidedAt.IsZero() {
		t.Fatal("DecidedAt not set")
	}
	sameAppr, err := store.RecordApproval(ctx, Approval{RunID: run2.ID, NodeID: "work", Requester: "alice", Approver: "op1", Decision: "approved"})
	if err != nil || sameAppr.Decision != "approved" {
		t.Fatalf("idempotent approval repeat: %v %v", sameAppr, err)
	}
	if _, err := store.RecordApproval(ctx, Approval{RunID: run2.ID, NodeID: "work", Requester: "alice", Approver: "op1", Decision: "rejected"}); !errors.Is(err, ErrApprovalConflict) {
		t.Fatalf("conflicting approval: want ErrApprovalConflict, got %v", err)
	}
	if _, err := store.RecordApproval(ctx, Approval{RunID: run2.ID, NodeID: "other", Requester: "alice", Approver: "alice", Decision: "approved"}); err == nil {
		t.Fatal("self-approval accepted")
	}
	if _, err := store.RecordApproval(ctx, Approval{RunID: run2.ID, NodeID: "other", Decision: "maybe"}); err == nil {
		t.Fatal("invalid decision accepted")
	}
}

func TestMemoryStoreContract(t *testing.T) {
	runStoreContract(t, NewMemoryStore(), false)
}

func TestFileStoreContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	fs, err := OpenFileStore(path)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	runStoreContract(t, fs, false)
}

func TestFileStoreRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	fs, err := OpenFileStore(path)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	ctx := context.Background()
	def := Definition{SchemaVersion: DefinitionSchemaVersion, ID: "wf-restart", Name: "r"}
	if err := fs.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if _, err := fs.Publish(ctx, "wf-restart", "tester"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fs2, err := OpenFileStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer fs2.Close()
	rev, err := fs2.GetRevision(ctx, "wf-restart", 1)
	if err != nil {
		t.Fatalf("GetRevision after restart: %v", err)
	}
	if rev.Definition.Name != "r" || rev.Version.Status != DefinitionPublished {
		t.Fatalf("restarted state = %+v", rev)
	}
}

func TestFileStorePersistFailureKeepsMemory(t *testing.T) {
	// Clone-apply-persist-swap: replace the data file with a directory
	// after a good write, so the next persist fails at rename time.
	// The failed mutation must not leak into memory.
	path := filepath.Join(t.TempDir(), "store.json")
	fs, err := OpenFileStore(path)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	defer fs.Close()
	ctx := context.Background()
	if err := fs.SaveDraft(ctx, Definition{ID: "wf-good"}); err != nil {
		t.Fatalf("SaveDraft good: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := fs.SaveDraft(ctx, Definition{ID: "wf-bad"}); err == nil {
		t.Fatal("SaveDraft onto a directory succeeded")
	}
	if _, err := fs.GetDraft(ctx, "wf-good"); err != nil {
		t.Fatalf("good draft lost after failed persist: %v", err)
	}
	if _, err := fs.GetDraft(ctx, "wf-bad"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed mutate leaked into memory: %v", err)
	}
}

func TestPostgresStoreContract(t *testing.T) {
	dsn := os.Getenv("NEXAROUTE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("NEXAROUTE_TEST_PG_DSN not set; PostgreSQL live acceptance skipped")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("pg ping: %v", err)
	}
	// Fresh tables on every run, so the fixed contract IDs do not
	// collide with a previous run against the same database.
	for _, tbl := range []string{
		"nexaroute_workflow_approvals", "nexaroute_workflow_nodes",
		"nexaroute_workflow_runs", "nexaroute_workflow_revisions",
		"nexaroute_workflow_definitions",
	} {
		if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS `+tbl+` CASCADE`); err != nil {
			t.Fatalf("drop %s: %v", tbl, err)
		}
	}
	store, err := NewPostgresStore(ctx, db)
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	defer store.Close()
	runStoreContract(t, store, true)
}
