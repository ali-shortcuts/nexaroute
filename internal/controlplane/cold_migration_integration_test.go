//go:build integration

package controlplane

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestPostgresIntegrationColdDatabaseConcurrentMigrations reproduces the
// 2026-10-27 HA-acceptance cold start: two gateway replicas starting at
// the same time against a database whose migration tables do not exist
// yet. Before the lock-first fix, the loser failed with a
// pg_type_typname_nsp_index duplicate-key error from its CREATE TABLE IF
// NOT EXISTS, which ran before the advisory lock was taken.
//
// The test runs in its own schema (via search_path) so dropping its
// tables can never disturb other packages sharing the disposable test
// database.
func TestPostgresIntegrationColdDatabaseConcurrentMigrations(t *testing.T) {
	base := integrationDSN(t)
	const schema = "ha652_cold_mig"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
	registry, err := DefaultMigrationRegistry()
	if err != nil {
		t.Fatal(err)
	}
	open := func() *sql.DB {
		db, err := sql.Open("pgx", schemaDSN)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(4)
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	pools := []*sql.DB{open(), open()}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, len(pools))
	for i, db := range pools {
		wg.Add(1)
		go func(i int, db *sql.DB) {
			defer wg.Done()
			<-start
			errs[i] = ApplyMigrations(ctx, db, registry)
		}(i, db)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent cold apply %d: %v", i, err)
		}
	}
	// A third, sequential application must be idempotent, and exactly
	// one migration row must exist in the cold schema.
	if err := ApplyMigrations(ctx, pools[0], registry); err != nil {
		t.Fatalf("idempotent re-apply after cold concurrent apply: %v", err)
	}
	var count int
	if err := pools[0].QueryRowContext(ctx, `SELECT count(*) FROM nexaroute_control_migrations`).Scan(&count); err != nil {
		t.Fatalf("count migrations in cold schema: %v", err)
	}
	if count != 1 {
		t.Fatalf("migration rows in cold schema = %d, want 1", count)
	}
}
