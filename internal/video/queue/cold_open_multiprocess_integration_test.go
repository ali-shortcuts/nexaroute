//go:build integration

package queue

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

	"github.com/ali-shortcuts/nexaroute/internal/video"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestHelperColdOpenProcesses is a subprocess worker for
// TestPostgresVideoStoreConcurrentColdOpen. It runs only when
// NEXAROUTE_HA652_HELPER_DSN is set. Opening the store runs the
// migration (advisory-xact-lock serialized); two independent processes
// opening against the same empty schema must both succeed.
func TestHelperColdOpenProcesses(t *testing.T) {
	dsn := os.Getenv("NEXAROUTE_HA652_HELPER_DSN")
	if dsn == "" {
		t.Skip("helper only: NEXAROUTE_HA652_HELPER_DSN not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := OpenPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatalf("helper cold open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("helper cold close: %v", err)
	}
}

// TestPostgresVideoStoreConcurrentColdOpen proves the video job-store
// migration serializes across processes: two separate test processes
// open the Postgres store against a schema whose tables do not exist
// yet. Both must succeed, and a job must then round-trip through a
// third open.
func TestPostgresVideoStoreConcurrentColdOpen(t *testing.T) {
	base := videoPGDSN(t)
	const schema = "ha652_vjob_mp"
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

	s, err := OpenPostgresStore(ctx, schemaDSN)
	if err != nil {
		t.Fatalf("open after multi-process cold open: %v", err)
	}
	defer s.Close()
	job := video.VideoJob{JobID: "mp-job", State: video.StateQueued, CreatedAt: time.Now().UTC().Truncate(time.Millisecond)}
	if err := s.Create(ctx, job); err != nil {
		t.Fatalf("job create after multi-process cold open: %v", err)
	}
	got, err := s.Get(ctx, "mp-job")
	if err != nil || got.JobID != "mp-job" {
		t.Fatalf("job read after multi-process cold open: got=%+v err=%v", got, err)
	}
}
