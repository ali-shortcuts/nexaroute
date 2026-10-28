//go:build integration

package securitystore

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

// TestHelperColdOpenProcesses is a subprocess worker for
// TestPostgresSecurityStoreIntegrationConcurrentColdOpen. It runs only
// when NEXAROUTE_HA652_HELPER_DSN is set. Opening the backend runs the
// schema migration (advisory-xact-lock serialized); two independent
// processes opening against the same empty schema must both succeed.
func TestHelperColdOpenProcesses(t *testing.T) {
	dsn := os.Getenv("NEXAROUTE_HA652_HELPER_DSN")
	if dsn == "" {
		t.Skip("helper only: NEXAROUTE_HA652_HELPER_DSN not set")
	}
	b, err := OpenBackend(BackendPostgres, dsn, "", 1000, 365)
	if err != nil {
		t.Fatalf("helper cold open: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("helper cold close: %v", err)
	}
}

// TestPostgresSecurityStoreIntegrationConcurrentColdOpen proves the
// security-store migration serializes across processes: two separate
// test processes open the Postgres backend against a schema whose tables
// do not exist yet. Both must succeed, and the migrated schema must then
// be usable (a session round-trips through a third open).
func TestPostgresSecurityStoreIntegrationConcurrentColdOpen(t *testing.T) {
	base := securityPGDSN(t)
	const schema = "ha652_sec_mp"
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
			"-test.run=TestHelperColdOpenProcesses", "-test.count=1")
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

	b, err := OpenBackend(BackendPostgres, schemaDSN, "", 1000, 365)
	if err != nil {
		t.Fatalf("open after multi-process cold open: %v", err)
	}
	defer b.Close()
	sess := Session{Subject: "mp-check", Issuer: "mp-test", CreatedAt: time.Now().UTC(), LastSeen: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	hash := strings.Repeat("cc", 32)
	if err := b.CreateSession(ctx, hash, sess, AuditEvent{Timestamp: time.Now().UTC(), Actor: "mp-check", Action: "auth.login", Outcome: "success", Status: 200}); err != nil {
		t.Fatalf("session create after multi-process cold open: %v", err)
	}
	got, ok, err := b.Session(ctx, hash)
	if err != nil || !ok || got.Subject != "mp-check" {
		t.Fatalf("session read after multi-process cold open: got=%+v ok=%v err=%v", got, ok, err)
	}
}
