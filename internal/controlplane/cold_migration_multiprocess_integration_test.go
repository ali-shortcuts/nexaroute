//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestHelperColdMigrateProcesses is a subprocess worker for
// TestPostgresIntegrationColdDatabaseConcurrentMigrationsMultiProcess.
// It runs only when NEXAROUTE_HA652_HELPER_DSN is set; ordinary test runs
// skip it. The parent test launches two independent processes running
// this helper against the same empty schema: two pools inside one test
// process (the pre-existing cold test) cannot prove that two gateway
// processes serialize, because they share one pool's connection setup.
func TestHelperColdMigrateProcesses(t *testing.T) {
	dsn := os.Getenv("NEXAROUTE_HA652_HELPER_DSN")
	if dsn == "" {
		t.Skip("helper only: NEXAROUTE_HA652_HELPER_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	registry, err := DefaultMigrationRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, db, registry); err != nil {
		t.Fatalf("helper cold apply: %v", err)
	}
}

// TestPostgresIntegrationColdDatabaseConcurrentMigrationsMultiProcess
// is the independent-process form of the 2026-10-27 cold-start defect:
// two separate processes (not two pools in one process) start against a
// schema whose migration tables do not exist yet. Before the lock-first
// fix, the loser failed with a pg_type_typname_nsp_index duplicate-key
// error. Both processes must exit 0, a sequential re-apply must stay
// idempotent, and exactly one migration row must exist.
func TestPostgresIntegrationColdDatabaseConcurrentMigrationsMultiProcess(t *testing.T) {
	base := integrationDSN(t)
	const schema = "ha652_cold_mig_mp"
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if _, err := admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
		t.Fatalf("drop cold schema: %v", err)
	}
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create cold schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cleanupCancel()
		_, _ = admin.ExecContext(cleanupCtx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	})

	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	schemaDSN := base + sep + "search_path=" + schema
	run := func() error {
		runCtx, runCancel := context.WithTimeout(ctx, 90*time.Second)
		defer runCancel()
		cmd := exec.CommandContext(runCtx, os.Args[0],
			"-test.run=TestHelperColdMigrateProcesses", "-test.count=1")
		cmd.Env = append(os.Environ(), "NEXAROUTE_HA652_HELPER_DSN="+schemaDSN)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("helper process: %w\n%s", err, out)
		}
		return nil
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = run()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent cold helper %d: %v", i, err)
		}
	}

	pool, err := sql.Open("pgx", schemaDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	registry, err := DefaultMigrationRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, pool, registry); err != nil {
		t.Fatalf("idempotent re-apply after multi-process cold apply: %v", err)
	}
	var count int
	if err := pool.QueryRowContext(ctx, `SELECT count(*) FROM nexaroute_control_migrations`).Scan(&count); err != nil {
		t.Fatalf("count migrations in cold schema: %v", err)
	}
	if count != 1 {
		t.Fatalf("migration rows in cold schema = %d, want 1", count)
	}
}
