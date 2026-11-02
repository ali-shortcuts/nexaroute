package nexaflow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Prune must use one stable set of run IDs, even if another run becomes
// eligible while child rows are being deleted. This uses real row locks to
// deterministically force that ordering on disposable PostgreSQL.
func TestPostgresPruneConcurrentTerminalTransition(t *testing.T) {
	dsn := os.Getenv("NEXAROUTE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin := sql.OpenDB(stdlib.GetConnector(*cfg))
	defer admin.Close()
	schema := fmt.Sprintf("prune_race_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
	cfg.RuntimeParams["search_path"] = schema
	cfg.RuntimeParams["application_name"] = schema
	db := sql.OpenDB(stdlib.GetConnector(*cfg))
	defer db.Close()
	store, err := NewPostgresStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []Run{
		{ID: "old", DefinitionID: "wf", State: RunSucceeded, FinishedAt: time.Now().Add(-time.Hour)},
		{ID: "transition", DefinitionID: "wf", State: RunRunning},
	} {
		if err := store.CreateRun(ctx, run); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AppendNodeExecution(ctx, NodeExecution{RunID: run.ID, NodeID: "node", AppendOperationID: "initial"}); err != nil {
			t.Fatal(err)
		}
	}
	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	var locked string
	if err := blocker.QueryRowContext(ctx, `SELECT run_id FROM `+wfNodesTable+` WHERE run_id='old' FOR UPDATE`).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	type result struct {
		count int
		err   error
	}
	done := make(chan result, 1)
	go func() { count, err := store.Prune(ctx, time.Now().Add(time.Hour)); done <- result{count, err} }()
	// Observe PostgreSQL's lock wait rather than relying on scheduling sleeps.
	for {
		var waiting bool
		if err := admin.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock' AND query LIKE 'DELETE FROM nexaroute_workflow_nodes%')`, schema).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case r := <-done:
			t.Fatalf("prune finished before lock wait: %+v", r)
		case <-ctx.Done():
			t.Fatal("prune did not reach expected lock wait")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := store.UpdateRunState(ctx, "transition", RunRunning, RunSucceeded, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.count != 1 {
		t.Fatalf("prune returned %+v, want exactly the originally eligible run", r)
	}
	if _, err := store.GetRun(ctx, "old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old run: %v", err)
	}
	if _, err := store.GetRun(ctx, "transition"); err != nil {
		t.Fatalf("newly eligible run must remain: %v", err)
	}
	nodes, err := store.GetNodeExecutions(ctx, "transition")
	if err != nil || len(nodes) != 1 {
		t.Fatalf("transition node rows: %+v %v", nodes, err)
	}
}
