package nexaflow

// Task 6 final-acceptance tests for the #693 canonical integration.
//
// These tests prove, on #693 as-is (no sibling ports), the gates the
// Task 8 evidence left open:
//   - PostgreSQL cross-process claims (delivery ledger + scheduler
//     slot) with two SEPARATE test-binary processes, run
//     concurrently, sharing one disposable database;
//   - node-lease recovery after a SIGKILLed claimant (recoverable
//     claims, no duplicate attempt);
//   - PostgreSQL migration re-run/concurrent-open safety and a
//     genuine migration failure with transactional rollback;
//   - fail-closed handling of corrupt negative revisions (gosec
//     G115 regression: revisions are validated before narrowing);
//   - corrupt file-store failure (fail closed, file untouched).
//
// All PostgreSQL tests skip without NEXAROUTE_TEST_PG_DSN and use
// unique keys/ids per run, so concurrent or repeated runs on a
// shared disposable database cannot pollute each other (a shared-DB
// slot-pollution incident on another line is the reason for this
// discipline). Never point these tests at production data.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func task6PG(t *testing.T) *sql.DB {
	t.Helper()
	return task8PGDB(t)
}

func task6Unique(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// TestTask6CrossProcessChild is the re-exec worker for the
// cross-process tests below. It is skipped in ordinary runs; the
// parent tests spawn the test binary with TASK6_CHILD_MODE set.
func TestTask6CrossProcessChild(t *testing.T) {
	mode := os.Getenv("TASK6_CHILD_MODE")
	if mode == "" {
		t.Skip("not a cross-process child")
	}
	out := os.Getenv("TASK6_CHILD_OUT")
	if out == "" {
		t.Fatalf("TASK6_CHILD_OUT is required")
	}
	ctx := context.Background()
	db := task6PG(t)
	if err := EnsurePostgresDeliverySchema(ctx, db); err != nil {
		t.Fatalf("ensure delivery schema: %v", err)
	}
	ledger, err := NewPostgresDeliveryLedger(db)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	write := func(s string) {
		if err := os.WriteFile(out, []byte(s), 0o600); err != nil {
			t.Fatalf("write result: %v", err)
		}
	}
	switch mode {
	case "delivery":
		claimed, err := ledger.Claim(ctx, os.Getenv("TASK6_CHILD_KEY"))
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if claimed {
			write("claimed")
		} else {
			write("taken")
		}
	case "scheduler":
		sch, err := NewScheduler(ledger, func(ctx context.Context, defID, slotKey string) (string, error) {
			return "child-run", nil
		})
		if err != nil {
			t.Fatalf("scheduler: %v", err)
		}
		nowUnix := int64(0)
		fmt.Sscanf(os.Getenv("TASK6_CHILD_NOW"), "%d", &nowUnix)
		res := sch.Tick(ctx, []Schedule{{
			DefinitionID:    os.Getenv("TASK6_CHILD_DEF"),
			IntervalSeconds: 3600,
			Enabled:         true,
		}}, time.Unix(nowUnix, 0).UTC())
		if len(res) != 1 {
			t.Fatalf("tick results = %d, want 1", len(res))
		}
		if res[0].Err != nil {
			t.Fatalf("tick error: %v", res[0].Err)
		}
		if res[0].Started {
			write("started")
		} else {
			write("skipped")
		}
	case "lease-hold":
		store, err := NewPostgresStore(ctx, db)
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		_, claimed, err := store.TryClaim(ctx,
			os.Getenv("TASK6_CHILD_RUN"), os.Getenv("TASK6_CHILD_NODE"),
			"child-worker", 5*time.Second, time.Now())
		if err != nil {
			t.Fatalf("tryclaim: %v", err)
		}
		if !claimed {
			write("taken")
			return
		}
		write("claimed")
		if ready := os.Getenv("TASK6_CHILD_READY"); ready != "" {
			if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
				t.Fatalf("write ready: %v", err)
			}
		}
		// Hold the lease until the parent SIGKILLs this process.
		// The 60s bound is a backstop only; the parent kills us
		// within seconds, and the lease TTL (5s) bounds recovery.
		time.Sleep(60 * time.Second)
	case "run-limit":
		// #691-reconciliation child: admit one run against the
		// definition's durable admission cap, from a separate
		// process. The parent spawns several children against one
		// definition and requires exactly `limit` admissions.
		store, err := NewPostgresStore(ctx, db)
		if err != nil {
			t.Fatalf("store: %v", err)
		}
		limit, err := strconv.Atoi(os.Getenv("TASK6_CHILD_LIMIT"))
		if err != nil {
			t.Fatalf("limit: %v", err)
		}
		defID := os.Getenv("TASK6_CHILD_DEF")
		err = store.CreateRun(ctx, Run{
			ID:           os.Getenv("TASK6_CHILD_RUN"),
			DefinitionID: defID,
			Definition:   Definition{ID: defID, Settings: DefinitionSettings{MaxConcurrentRuns: limit}},
			State:        RunCreated,
		})
		if err == nil {
			write("admitted")
		} else if errors.Is(err, ErrRunLimit) {
			write("limited")
		} else {
			t.Fatalf("create run: %v", err)
		}
	default:
		t.Fatalf("unknown TASK6_CHILD_MODE %q", mode)
	}
}

func task6RunChild(t *testing.T, env map[string]string) error {
	t.Helper()
	args := []string{"-test.run=^TestTask6CrossProcessChild$", "-test.count=1"}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("child: %v\n%s", err, out)
	}
	return nil
}

func task6RunChildrenConcurrently(t *testing.T, envs []map[string]string) []error {
	t.Helper()
	errs := make([]error, len(envs))
	var wg sync.WaitGroup
	for i, env := range envs {
		wg.Add(1)
		go func(i int, env map[string]string) {
			defer wg.Done()
			errs[i] = task6RunChild(t, env)
		}(i, env)
	}
	wg.Wait()
	return errs
}

func task6ReadResult(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read result %s: %v", path, err)
	}
	return strings.TrimSpace(string(raw))
}

func TestTask6PGCrossProcessDelivery(t *testing.T) {
	db := task6PG(t)
	key := task6Unique("task6-delivery")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM nexaroute_workflow_deliveries WHERE delivery_key = $1`, key)
	})
	dir := t.TempDir()
	envs := make([]map[string]string, 2)
	outs := make([]string, 2)
	for i := 0; i < 2; i++ {
		outs[i] = filepath.Join(dir, fmt.Sprintf("r%d.txt", i))
		envs[i] = map[string]string{
			"TASK6_CHILD_MODE": "delivery",
			"TASK6_CHILD_KEY":  key,
			"TASK6_CHILD_OUT":  outs[i],
		}
	}
	for _, err := range task6RunChildrenConcurrently(t, envs) {
		if err != nil {
			t.Fatal(err)
		}
	}
	claimed := 0
	for _, out := range outs {
		if task6ReadResult(t, out) == "claimed" {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("concurrent PG delivery claims = %d, want exactly 1", claimed)
	}
}

func TestTask6PGCrossProcessSchedulerSlot(t *testing.T) {
	db := task6PG(t)
	ctx := context.Background()
	defID := task6Unique("task6-sched-def")
	// Fixed slot for both children: the hour bucket containing now.
	now := time.Now().UTC().Truncate(time.Hour)
	slotKey := fmt.Sprintf("sched:%s:%d", defID, now.Unix())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM nexaroute_workflow_deliveries WHERE delivery_key = $1`, slotKey)
	})
	dir := t.TempDir()
	envs := make([]map[string]string, 2)
	outs := make([]string, 2)
	for i := 0; i < 2; i++ {
		outs[i] = filepath.Join(dir, fmt.Sprintf("r%d.txt", i))
		envs[i] = map[string]string{
			"TASK6_CHILD_MODE": "scheduler",
			"TASK6_CHILD_DEF":  defID,
			"TASK6_CHILD_NOW":  fmt.Sprintf("%d", now.Unix()),
			"TASK6_CHILD_OUT":  outs[i],
		}
	}
	for _, err := range task6RunChildrenConcurrently(t, envs) {
		if err != nil {
			t.Fatal(err)
		}
	}
	started := 0
	for _, out := range outs {
		if task6ReadResult(t, out) == "started" {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("concurrent PG scheduler starts = %d, want exactly 1", started)
	}
	_ = ctx
}

func TestTask6PGCrossProcessLeaseRecovery(t *testing.T) {
	db := task6PG(t)
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, db)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	def := task8BranchDef()
	def.ID = task6Unique("task6-lease-def")
	runID := task6Unique("task6-lease-run")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM nexaroute_workflow_nodes WHERE run_id = $1`, runID)
		_, _ = db.Exec(`DELETE FROM nexaroute_workflow_runs WHERE run_id = $1`, runID)
	})
	run := Run{ID: runID, DefinitionID: def.ID, Revision: 1, Definition: def, State: RunCreated}
	if err := store.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := store.AppendNodeExecution(ctx, NodeExecution{RunID: runID, NodeID: "n1", State: NodePending}); err != nil {
		t.Fatalf("AppendNodeExecution: %v", err)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "lease.txt")
	ready := filepath.Join(dir, "ready.txt")
	cmd := exec.Command(os.Args[0], "-test.run=^TestTask6CrossProcessChild$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"TASK6_CHILD_MODE=lease-hold",
		"TASK6_CHILD_OUT="+out,
		"TASK6_CHILD_READY="+ready,
		"TASK6_CHILD_RUN="+runID,
		"TASK6_CHILD_NODE=n1",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	// Wait for the child to hold the lease, then SIGKILL it: the
	// lease must survive the process death until its TTL expires.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatal("child never claimed the lease")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := task6ReadResult(t, out); got != "claimed" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("child lease result = %q, want claimed", got)
	}
	// A live foreign lease blocks a new claim even after the holder dies.
	if _, claimed, err := store.TryClaim(ctx, runID, "n1", "parent", 5*time.Second, time.Now()); err != nil {
		t.Fatalf("TryClaim while lease live: %v", err)
	} else if claimed {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("claim succeeded while a live foreign lease existed")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed child exited cleanly, want signal termination")
	}
	// Past the 5s TTL the lease is recoverable, and recovery leases
	// the same attempt rather than appending a duplicate.
	time.Sleep(6 * time.Second)
	if _, claimed, err := store.TryClaim(ctx, runID, "n1", "parent", 5*time.Second, time.Now()); err != nil {
		t.Fatalf("TryClaim after TTL: %v", err)
	} else if !claimed {
		t.Fatal("claim after lease TTL expiry failed")
	}
	execs, err := store.GetNodeExecutions(ctx, runID)
	if err != nil {
		t.Fatalf("GetNodeExecutions: %v", err)
	}
	attempts := 0
	for _, e := range execs {
		if e.NodeID == "n1" {
			attempts++
		}
	}
	if attempts != 1 {
		t.Fatalf("node n1 attempts = %d, want 1 (recovery must not duplicate the attempt)", attempts)
	}
}

func TestTask6PostgresNegativeRevisionFailsClosed(t *testing.T) {
	db := task6PG(t)
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, db)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	def := task8BranchDef()
	def.ID = task6Unique("task6-negrev-def")
	raw, _ := json.Marshal(def)
	// A negative stored revision is corrupt data: both read paths
	// must fail closed instead of wrapping it through uint64.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO nexaroute_workflow_revisions (definition_id, revision, content_hash, definition, status, published_by, published_at) VALUES ($1, -3, 'x', $2, 'published', 'tester', now())`,
		def.ID, raw); err != nil {
		t.Fatalf("insert negative revision: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM nexaroute_workflow_revisions WHERE definition_id = $1`, def.ID)
		_, _ = db.Exec(`DELETE FROM nexaroute_workflow_definitions WHERE definition_id = $1`, def.ID)
	})
	if _, err := store.ListRevisions(ctx, def.ID); err == nil || !strings.Contains(err.Error(), "corrupt negative revision") {
		t.Fatalf("ListRevisions negative revision err = %v, want corrupt negative revision", err)
	}
	if err := store.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if _, err := store.Publish(ctx, def.ID, "tester"); err == nil || !strings.Contains(err.Error(), "corrupt negative revision") {
		t.Fatalf("Publish negative revision err = %v, want corrupt negative revision", err)
	}
}

func TestTask6PostgresMigrationRerun(t *testing.T) {
	db := task6PG(t)
	ctx := context.Background()
	// Sequential re-open and concurrent re-open must both succeed
	// and preserve data: the migration is CREATE TABLE IF NOT
	// EXISTS under one advisory transaction lock.
	s1, err := NewPostgresStore(ctx, db)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	def := task8BranchDef()
	def.ID = task6Unique("task6-mig-def")
	runID := task6Unique("task6-mig-run")
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM nexaroute_workflow_runs WHERE run_id = $1`, runID)
		_, _ = db.Exec(`DELETE FROM nexaroute_workflow_definitions WHERE definition_id = $1`, def.ID)
	})
	if err := s1.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	if err := s1.CreateRun(ctx, Run{ID: runID, DefinitionID: def.ID, Revision: 1, Definition: def, State: RunCreated}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := NewPostgresStore(ctx, db); err != nil {
		t.Fatalf("re-open: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = NewPostgresStore(ctx, db)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent re-open: %v", err)
		}
	}
	got, err := s1.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun after re-opens: %v", err)
	}
	if got.ID != runID || got.DefinitionID != def.ID {
		t.Fatalf("run after re-opens = %+v, want id %s", got, runID)
	}
	if _, err := s1.GetDraft(ctx, def.ID); err != nil {
		t.Fatalf("GetDraft after re-opens: %v", err)
	}
}

func TestTask6PostgresMigrationFailureRollback(t *testing.T) {
	// A genuinely failing migration must surface its error and
	// leave no partial workflow schema behind; fixing the cause
	// must let a later open succeed. The failure is injected with
	// a login role that can connect but cannot CREATE (PG 16
	// withholds CREATE on the public schema from fresh roles), in
	// an isolated fresh database so the rollback assertion is
	// meaningful (the shared disposable DB already has tables).
	adminDSN := os.Getenv("NEXAROUTE_TEST_PG_DSN")
	if adminDSN == "" {
		t.Skip("NEXAROUTE_TEST_PG_DSN not set; PostgreSQL live acceptance skipped")
	}
	admin, err := sql.Open("pgx", "postgres://root@/postgres?host=/var/run/postgresql")
	if err != nil {
		t.Fatalf("admin open: %v", err)
	}
	defer admin.Close()
	ctx := context.Background()
	if err := admin.PingContext(ctx); err != nil {
		t.Fatalf("admin ping: %v (this test needs socket peer auth as root)", err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	testDB := "task6_mig_" + suffix
	role := "task6_mig_" + suffix
	testPass := "task6-disposable-password"
	for _, stmt := range []string{
		`CREATE DATABASE ` + testDB,
		`CREATE ROLE ` + role + ` LOGIN PASSWORD '` + testPass + `'`,
	} {
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS `+testDB)
		_, _ = admin.ExecContext(context.Background(), `DROP ROLE IF EXISTS `+role)
	})
	// The role can connect but cannot create tables.
	restrictedDSN := fmt.Sprintf("postgres://%s:%s@127.0.0.1:5432/%s", role, testPass, testDB)
	if _, err := OpenPostgresStore(ctx, restrictedDSN); err == nil {
		t.Fatal("migration as CREATE-less role succeeded, want permission error")
	} else if !strings.Contains(err.Error(), "permission denied") && !strings.Contains(err.Error(), "denied") {
		t.Fatalf("migration error = %v, want a permission error", err)
	}
	// information_schema only describes the current database, so
	// count from a root connection opened against the test DB
	// itself (the restricted role's failed attempt is over: its
	// pool is closed by OpenPostgresStore on migration error).
	rootTestDB, err := sql.Open("pgx", "postgres://root@/"+testDB+"?host=/var/run/postgresql")
	if err != nil {
		t.Fatalf("root testdb open: %v", err)
	}
	defer rootTestDB.Close()
	var tables int
	if err := rootTestDB.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name LIKE 'nexaroute_workflow_%'`).Scan(&tables); err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if tables != 0 {
		t.Fatalf("failed migration left %d workflow tables behind, want 0 (rollback)", tables)
	}
	// Grant CREATE in the TEST database (a grant issued while
	// connected to postgres would land in the wrong database) and
	// the same open must now succeed end to end.
	if _, err := rootTestDB.ExecContext(ctx, `GRANT CREATE ON SCHEMA public TO `+role); err != nil {
		t.Fatalf("grant create: %v", err)
	}
	s, err := OpenPostgresStore(ctx, restrictedDSN)
	if err != nil {
		t.Fatalf("open after grant: %v", err)
	}
	defer s.Close()
	def := task8BranchDef()
	def.ID = task6Unique("task6-migfail-def")
	if err := s.SaveDraft(ctx, def); err != nil {
		t.Fatalf("SaveDraft after recovery: %v", err)
	}
}

func TestTask6FileStoreCorruptFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	if _, err := OpenFileStore(path); err == nil {
		t.Fatal("corrupt file store opened, want an error")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(raw) != "{not json" {
		t.Fatalf("corrupt file was modified on failed open: %q", raw)
	}
	// Fixing the file lets a later open succeed (recovery, not loss).
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write fixed: %v", err)
	}
	fs, err := OpenFileStore(path)
	if err != nil {
		t.Fatalf("open after fix: %v", err)
	}
	_ = fs.Close()
}
