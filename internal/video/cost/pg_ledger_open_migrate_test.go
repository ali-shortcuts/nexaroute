//go:build integration

// Real-PostgreSQL integration test for the durable video cost ledger's
// migration-failure path (issue #661 remainder). It runs only with the
// `integration` build tag and a DSN in NEXAROUTE_TEST_POSTGRES_DSN
// pointing at a DISPOSABLE database. Ordinary unit runs never touch a
// real server, so this path stays uncovered in the canonical unit
// profile; this test is the evidence that the path behaves fail-closed
// against a real server instead.
//
// The failure is produced without any fake: a fresh database is created,
// a login role with no CREATE privilege on its public schema is created,
// and OpenPostgresLedger is called with that role. Ping and BEGIN
// succeed, the advisory lock succeeds, and CREATE TABLE fails with a
// permission error. The constructor must then return a migration error,
// a nil ledger, no leaked credential, and no partially created table
// (the migration transaction rolls back).
package cost

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestOpenPostgresLedgerMigrateFailureFailsClosed pins the only
// coverable remainder of OpenPostgresLedger: the migrate-error branch
// (close the handle, return the migration error, create nothing).
func TestOpenPostgresLedgerMigrateFailureFailsClosed(t *testing.T) {
	superDSN := os.Getenv("NEXAROUTE_TEST_POSTGRES_DSN")
	if superDSN == "" {
		t.Skip("NEXAROUTE_TEST_POSTGRES_DSN not set; skipping real-PostgreSQL integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Fresh database: the migration must actually attempt CREATE TABLE,
	// which CREATE TABLE IF NOT EXISTS would skip if the table already
	// existed in the shared integration database.
	uniq := fmt.Sprintf("%d", time.Now().UnixNano())
	dbName := "nr661cost" + uniq
	role := "nr661migfail" + uniq
	const rolePassword = "nr661-migfail-password"

	admin, err := sql.Open("pgx", withDB(superDSN, "postgres"))
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	// Registered first, so it runs last (LIFO): the admin handle must
	// stay alive while the database/role cleanups below use it.
	t.Cleanup(func() { _ = admin.Close() })
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE "`+dbName+`"`); err != nil {
		t.Fatalf("create fresh database: %v", err)
	}
	// Cleanup order (LIFO): drop the database first, then the role.
	t.Cleanup(func() {
		// The role owns nothing, so it drops cleanly once the database
		// (and every connection to it) is gone.
		_, _ = admin.ExecContext(context.Background(), `DROP ROLE IF EXISTS "`+role+`"`)
	})
	t.Cleanup(func() {
		// Terminate any stray connection to the fresh database first so
		// the drop cannot fail on an open session.
		_, _ = admin.ExecContext(context.Background(),
			`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, dbName)
		_, _ = admin.ExecContext(context.Background(), `DROP DATABASE IF EXISTS "`+dbName+`"`)
	})
	if _, err := admin.ExecContext(ctx, `CREATE ROLE "`+role+`" LOGIN PASSWORD '`+rolePassword+`'`); err != nil {
		t.Fatalf("create restricted role: %v", err)
	}
	// The role deliberately receives no CREATE privilege on the fresh
	// database's public schema: CONNECT (default) is enough to ping and
	// BEGIN, but CREATE TABLE must fail.

	restrictedDSN := withUser(withDB(superDSN, dbName), role, rolePassword)
	ledger, err := OpenPostgresLedger(ctx, restrictedDSN)
	if err == nil {
		if ledger != nil {
			_ = ledger.Close()
		}
		t.Fatal("OpenPostgresLedger with a CREATE-less role unexpectedly succeeded")
	}
	if ledger != nil {
		_ = ledger.Close()
		t.Fatal("OpenPostgresLedger returned a non-nil ledger together with a migration error")
	}
	if !strings.Contains(err.Error(), "migration") {
		t.Fatalf("migration-failure error = %v, want an error mentioning migration", err)
	}
	if strings.Contains(err.Error(), rolePassword) {
		t.Fatalf("migration-failure error leaked the credential: %v", err)
	}

	// The failed migration must leave no partial schema behind. Check
	// through a connection to the fresh database itself: pg_tables is
	// database-scoped, so the admin connection (to "postgres") cannot
	// see the fresh database's tables.
	var tableCount int
	fresh, err := sql.Open("pgx", withDB(superDSN, dbName))
	if err != nil {
		t.Fatalf("open fresh database for verification: %v", err)
	}
	defer func() { _ = fresh.Close() }()
	if err := fresh.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename = $1`, videoCostTable).
		Scan(&tableCount); err != nil {
		t.Fatalf("count cost tables in fresh database: %v", err)
	}
	if tableCount != 0 {
		t.Fatalf("cost tables in fresh database = %d, want 0 after a rolled-back migration", tableCount)
	}
}

// withDB returns dsn with its database name replaced by name.
func withDB(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.Path = "/" + name
	return u.String()
}

// withUser returns dsn authenticated as user with password.
func withUser(dsn, user, password string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	u.User = url.UserPassword(user, password)
	return u.String()
}
