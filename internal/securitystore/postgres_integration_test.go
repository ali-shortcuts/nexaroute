//go:build integration

// Real-PostgreSQL integration tests for the security store adapter
// (M4-2a). They run only with the `integration` build tag and a DSN in
// NEXAROUTE_TEST_POSTGRES_DSN pointing at a DISPOSABLE database (CI
// provides one via a service container). Ordinary unit runs never
// touch a real server.
//
// Evidence base: the shared Backend contract suite against the real
// service, cross-backend audit hash parity with the bbolt store, two
// independent store instances appending concurrently (the chain must
// stay verifiable with unique sequences), and persistence across a
// close/reopen.
package securitystore

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func securityPGDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("NEXAROUTE_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("NEXAROUTE_TEST_POSTGRES_DSN not set; skipping real-PostgreSQL integration test")
	}
	return dsn
}

func resetSecurityTables(t *testing.T) {
	t.Helper()
	db, err := sql.Open("pgx", securityPGDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, table := range []string{securitySessionsTable, securityAuditTable, securityMetaTable} {
		if _, err := db.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
			t.Fatalf("reset %s: %v", table, err)
		}
	}
}

func openPGBackend(t *testing.T) Backend {
	t.Helper()
	b, err := OpenBackend(BackendPostgres, securityPGDSN(t), "", 1000, 365)
	if err != nil {
		t.Fatalf("OpenBackend postgres: %v", err)
	}
	return b
}

func TestPostgresSecurityStoreIntegration(t *testing.T) {
	resetSecurityTables(t)
	t.Run("contract", func(t *testing.T) {
		runBackendContract(t, func(t *testing.T) Backend {
			resetSecurityTables(t)
			return openPGBackend(t)
		})
	})

	t.Run("hash parity with bbolt", func(t *testing.T) {
		resetSecurityTables(t)
		pg := openPGBackend(t)
		defer func() { _ = pg.Close() }()
		bb, err := OpenBackend("", "", contractStorePath(t, "parity.db"), 1000, 365)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = bb.Close() }()

		ctx := context.Background()
		base := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
		hashA := strings.Repeat("aa", 32)
		sess := Session{Subject: "alice", Issuer: "local", Roles: []string{"admin"}, CreatedAt: base, LastSeen: base, ExpiresAt: base.Add(time.Hour)}
		login := AuditEvent{Timestamp: base, Actor: "alice", Action: "auth.login", Outcome: "success", Status: 200}
		for i, b := range []Backend{pg, bb} {
			if err := b.CreateSession(ctx, hashA, sess, login); err != nil {
				t.Fatalf("backend %d CreateSession: %v", i, err)
			}
			for j, action := range []string{"contract.one", "contract.two", "contract.three"} {
				ev := AuditEvent{Timestamp: base.Add(time.Duration(j+1) * time.Second), Actor: "contract", Action: action, Outcome: "success", Status: 200}
				if err := b.AppendAudit(ctx, ev); err != nil {
					t.Fatalf("backend %d AppendAudit: %v", i, err)
				}
			}
			if err := b.RevokeSession(ctx, hashA, AuditEvent{Timestamp: base.Add(4 * time.Second), Actor: "alice", Action: "auth.logout", Outcome: "success", Status: 200}); err != nil {
				t.Fatalf("backend %d RevokeSession: %v", i, err)
			}
		}
		pgRecords, err := pg.AuditRecords(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		bbRecords, err := bb.AuditRecords(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(pgRecords) != len(bbRecords) || len(pgRecords) != 5 {
			t.Fatalf("record counts: pg=%d bbolt=%d, want 5/5", len(pgRecords), len(bbRecords))
		}
		for i := range pgRecords {
			if pgRecords[i] != bbRecords[i] {
				t.Fatalf("record %d differs across backends:\npg:    %+v\nbbolt: %+v", i, pgRecords[i], bbRecords[i])
			}
		}
	})

	t.Run("two writers one chain", func(t *testing.T) {
		resetSecurityTables(t)
		a := openPGBackend(t)
		defer func() { _ = a.Close() }()
		b := openPGBackend(t)
		defer func() { _ = b.Close() }()
		ctx := context.Background()
		var wg sync.WaitGroup
		errs := make(chan error, 50)
		for w, store := range []Backend{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 25; i++ {
					ev := AuditEvent{Actor: fmt.Sprintf("writer-%d", w), Action: "contract.concurrent", Outcome: "success", Status: 200}
					if err := store.AppendAudit(ctx, ev); err != nil {
						errs <- err
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatalf("concurrent append: %v", err)
		}
		report, err := a.VerifyAuditChain(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !report.Verified || report.ChainedRecords != 50 {
			t.Fatalf("chain after concurrent appends: %+v", report)
		}
		records, err := a.AuditRecords(ctx, 100)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[uint64]bool{}
		for _, r := range records {
			if seen[r.Seq] {
				t.Fatalf("duplicate sequence %d", r.Seq)
			}
			seen[r.Seq] = true
		}
	})

	t.Run("reopen persists", func(t *testing.T) {
		resetSecurityTables(t)
		ctx := context.Background()
		hash := strings.Repeat("ee", 32)
		now := time.Now().UTC().Truncate(time.Second)
		first := openPGBackend(t)
		sess := Session{Subject: "carol", Issuer: "local", Roles: []string{"viewer"}, CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour)}
		if err := first.CreateSession(ctx, hash, sess, AuditEvent{Timestamp: now, Actor: "carol", Action: "auth.login", Outcome: "success", Status: 200}); err != nil {
			t.Fatal(err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		second := openPGBackend(t)
		defer func() { _ = second.Close() }()
		got, ok, err := second.Session(ctx, hash)
		if err != nil || !ok || got.Subject != "carol" {
			t.Fatalf("session after reopen: ok=%v err=%v subject=%q", ok, err, got.Subject)
		}
		report, err := second.VerifyAuditChain(ctx)
		if err != nil || !report.Verified {
			t.Fatalf("chain after reopen: err=%v report=%+v", err, report)
		}
	})
}
