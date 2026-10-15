//go:build integration

// Real-PostgreSQL integration tests for the control-plane SQL store and
// migrator. They run only with the `integration` build tag and a DSN in
// NEXAROUTE_TEST_POSTGRES_DSN pointing at a DISPOSABLE database (CI
// provides one via a service container). Ordinary unit runs never touch
// a real server.
//
// These tests are the M4-2 evidence base for: store conformance against
// the real database, two independent connections, the concurrent
// first-write race (deterministic ErrConflict, never a leaked
// unique-violation), restart persistence, and migration idempotency,
// checksum-mismatch rejection and concurrent application.
package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func integrationDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("NEXAROUTE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("NEXAROUTE_TEST_POSTGRES_DSN not set; skipping real-PostgreSQL integration test")
	}
	return dsn
}

func integrationDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", integrationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(8)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("integration database unreachable: %v", err)
	}
	registry, err := DefaultMigrationRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, db, registry); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// uniqueNamespace isolates each test on the shared disposable database.
func uniqueNamespace(t *testing.T) string {
	t.Helper()
	ns := fmt.Sprintf("integration/%s/%d", t.Name(), time.Now().UnixNano())
	t.Cleanup(func() {
		db, err := sql.Open("pgx", integrationDSN(t))
		if err != nil {
			return
		}
		defer func() { _ = db.Close() }()
		_, _ = db.Exec(`DELETE FROM `+snapshotTable+` WHERE namespace = $1`, ns)
	})
	return ns
}

func TestPostgresIntegrationStoreConformance(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLStore(integrationDB(t))
	if err != nil {
		t.Fatal(err)
	}
	ns := uniqueNamespace(t)

	if _, err := store.Get(ctx, ns); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing: %v", err)
	}
	created, err := store.Put(ctx, ns, 0, []byte(`{"v":1}`))
	if err != nil || created.Revision != 1 {
		t.Fatalf("create: %+v %v", created, err)
	}
	got, err := store.Get(ctx, ns)
	if err != nil || got.Revision != 1 || string(got.Payload) != `{"v":1}` {
		t.Fatalf("get after create: %+v %v", got, err)
	}
	// Defensive copy: mutating the returned payload must not corrupt the
	// store's view of subsequent reads.
	got.Payload[0] = 'X'
	again, err := store.Get(ctx, ns)
	if err != nil || string(again.Payload) != `{"v":1}` {
		t.Fatalf("payload not defensively copied: %+v %v", again, err)
	}
	if _, err := store.Put(ctx, ns, 0, []byte(`{"v":x}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("re-create with revision 0: %v, want ErrConflict", err)
	}
	updated, err := store.Put(ctx, ns, 1, []byte(`{"v":2}`))
	if err != nil || updated.Revision != 2 {
		t.Fatalf("update: %+v %v", updated, err)
	}
	if _, err := store.Put(ctx, ns, 1, []byte(`{"v":stale}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update: %v, want ErrConflict", err)
	}
	if err := store.Delete(ctx, ns, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete with stale revision: %v, want ErrConflict", err)
	}
	if err := store.Delete(ctx, ns, 2); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(ctx, ns); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
}

// Two independent pools (two gateway processes) must observe each other's
// committed writes and must never silently overwrite a newer revision.
func TestPostgresIntegrationTwoInstancesAndRestart(t *testing.T) {
	ctx := context.Background()
	ns := uniqueNamespace(t)
	a, err := NewSQLStore(integrationDB(t))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewSQLStore(integrationDB(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Put(ctx, ns, 0, []byte("from-a")); err != nil {
		t.Fatal(err)
	}
	got, err := b.Get(ctx, ns)
	if err != nil || string(got.Payload) != "from-a" || got.Revision != 1 {
		t.Fatalf("second instance read: %+v %v", got, err)
	}
	if _, err := b.Put(ctx, ns, 1, []byte("from-b")); err != nil {
		t.Fatal(err)
	}
	// Instance A writing against the revision it last saw must conflict,
	// not overwrite B's newer revision.
	if _, err := a.Put(ctx, ns, 1, []byte("a-overwrite")); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-instance stale write: %v, want ErrConflict", err)
	}
	// Restart persistence: a brand-new pool sees the committed state.
	c, err := NewSQLStore(integrationDB(t))
	if err != nil {
		t.Fatal(err)
	}
	got, err = c.Get(ctx, ns)
	if err != nil || string(got.Payload) != "from-b" || got.Revision != 2 {
		t.Fatalf("after restart: %+v %v", got, err)
	}
}

// The regression for the missing-namespace race: N concurrent first
// writes to a fresh namespace must produce exactly one success and N−1
// deterministic ErrConflict results — never a leaked database error.
func TestPostgresIntegrationConcurrentFirstWriteRace(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLStore(integrationDB(t))
	if err != nil {
		t.Fatal(err)
	}
	ns := uniqueNamespace(t)
	const writers = 8
	var wg sync.WaitGroup
	results := make([]error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = store.Put(ctx, ns, 0, []byte(fmt.Sprintf("writer-%d", i)))
		}(i)
	}
	wg.Wait()
	successes, conflicts := 0, 0
	for i, err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("writer %d: unexpected error (race leaked a raw database error?): %v", i, err)
		}
	}
	if successes != 1 || conflicts != writers-1 {
		t.Fatalf("successes=%d conflicts=%d, want 1 and %d", successes, conflicts, writers-1)
	}
	final, err := store.Get(ctx, ns)
	if err != nil || final.Revision != 1 {
		t.Fatalf("final state: %+v %v", final, err)
	}
}

func TestPostgresIntegrationMigrationsIdempotentChecksumAndConcurrent(t *testing.T) {
	ctx := context.Background()
	db := integrationDB(t) // already migrated once by the helper
	registry, err := DefaultMigrationRegistry()
	if err != nil {
		t.Fatal(err)
	}
	// Idempotent re-application.
	if err := ApplyMigrations(ctx, db, registry); err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	// Concurrent application from two pools: the advisory lock must let
	// both complete without error.
	other := integrationDB(t)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, target := range []*sql.DB{db, other} {
		wg.Add(1)
		go func(i int, target *sql.DB) {
			defer wg.Done()
			errs[i] = ApplyMigrations(ctx, target, registry)
		}(i, target)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent apply %d: %v", i, err)
		}
	}
	// A tampered registry (same version, different body) must be rejected
	// by checksum, fail-closed.
	tampered, err := NewMigrationRegistry(Migration{Version: 1, Name: "create control snapshots table", Up: "SELECT 1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyMigrations(ctx, db, tampered); err == nil {
		t.Fatal("checksum mismatch must reject the tampered migration")
	}
}
