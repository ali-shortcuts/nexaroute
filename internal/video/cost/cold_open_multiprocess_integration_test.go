//go:build integration

package cost

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
// TestPostgresLedgerConcurrentColdOpen. It runs only when
// NEXAROUTE_HA652_HELPER_DSN is set. Opening the ledger runs the
// migration (advisory-xact-lock serialized); two independent processes
// opening against the same empty schema must both succeed.
func TestHelperColdOpenProcesses(t *testing.T) {
	dsn := os.Getenv("NEXAROUTE_HA652_HELPER_DSN")
	if dsn == "" {
		t.Skip("helper only: NEXAROUTE_HA652_HELPER_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	l, err := OpenPostgresLedger(ctx, dsn)
	if err != nil {
		t.Fatalf("helper cold open: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("helper cold close: %v", err)
	}
}

// TestPostgresLedgerConcurrentColdOpen proves the video cost-ledger
// migration serializes across processes: two separate test processes
// open the Postgres ledger against a schema whose tables do not exist
// yet. Both must succeed, and the ledger must then be usable through a
// third open.
func TestPostgresLedgerConcurrentColdOpen(t *testing.T) {
	base := os.Getenv("NEXAROUTE_TEST_POSTGRES_DSN")
	if base == "" {
		t.Skip("NEXAROUTE_TEST_POSTGRES_DSN not set; skipping real-PostgreSQL integration test")
	}
	const schema = "ha652_vcost_mp"
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

	l, err := OpenPostgresLedger(ctx, schemaDSN)
	if err != nil {
		t.Fatalf("open after multi-process cold open: %v", err)
	}
	defer l.Close()
	if got := l.Entries(); len(got) != 0 {
		t.Fatalf("ledger entries after multi-process cold open = %d, want 0", len(got))
	}
}
