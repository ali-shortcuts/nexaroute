package nexaflow

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

var errLostCommitReply = errors.New("injected lost commit acknowledgement")

// This connector really commits to PostgreSQL, then simulates losing the
// acknowledgement at the driver boundary. It does not simulate a network
// partition or establish crash/failover behavior.
type lostCommitConnector struct {
	driver.Connector
	armed atomic.Bool
}

func (c *lostCommitConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &lostCommitConn{Conn: conn, parent: c}, nil
}

type lostCommitConn struct {
	driver.Conn
	parent *lostCommitConnector
}

func (c *lostCommitConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &lostCommitTx{Tx: tx, parent: c.parent}, nil
}
func (c *lostCommitConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}
func (c *lostCommitConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}
func (c *lostCommitConn) CheckNamedValue(value *driver.NamedValue) error {
	if checker, ok := c.Conn.(driver.NamedValueChecker); ok {
		return checker.CheckNamedValue(value)
	}
	return driver.ErrSkip
}

type lostCommitTx struct {
	driver.Tx
	parent *lostCommitConnector
}

func (t *lostCommitTx) Commit() error {
	if err := t.Tx.Commit(); err != nil {
		return err
	}
	if t.parent.armed.CompareAndSwap(true, false) {
		return errLostCommitReply
	}
	return nil
}

func TestPostgresLostCommitReplyRequiresReconciliation(t *testing.T) {
	dsn := os.Getenv("NEXAROUTE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("requires a disposable PostgreSQL database")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	connector := &lostCommitConnector{Connector: stdlib.GetConnector(*config)}
	db := sql.OpenDB(connector)
	defer db.Close()
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	runID := Continue95NexaflowPGID("lost-commit")
	key := Continue95NexaflowPGID("lost-key")
	defer func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM `+wfNodesTable+` WHERE run_id=$1`, runID)
		_, _ = db.ExecContext(ctx, `DELETE FROM `+wfRunsTable+` WHERE run_id=$1`, runID)
	}()
	connector.armed.Store(true)
	err = store.CreateRun(ctx, Run{ID: runID, DefinitionID: "wf", IdempotencyKey: key})
	if !errors.Is(err, ErrPostgresCommitUncertain) || !errors.Is(err, errLostCommitReply) {
		t.Fatalf("CreateRun error=%v, want uncertain commit and injected cause", err)
	}
	got, err := store.GetRunByIdempotency(ctx, key)
	if err != nil || got.ID != runID {
		t.Fatalf("reconcile committed run: %+v, %v", got, err)
	}
	if err := store.CreateRun(ctx, Run{ID: runID, DefinitionID: "wf", IdempotencyKey: key}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("same run retry = %v, want already exists", err)
	}
	connector.armed.Store(true)
	_, err = store.AppendNodeExecution(ctx, NodeExecution{RunID: runID, NodeID: "n", State: NodePending})
	if !errors.Is(err, ErrPostgresCommitUncertain) || !errors.Is(err, errLostCommitReply) {
		t.Fatalf("Append error=%v, want uncertain commit and injected cause", err)
	}
	nodes, err := store.GetNodeExecutions(ctx, runID)
	if err != nil || len(nodes) != 1 || nodes[0].Attempt != 0 {
		t.Fatalf("reconcile node attempts: %+v, %v", nodes, err)
	}
	// A blind Append retry would create another attempt. Deliberately do not
	// retry: callers must reconcile and no exactly-once claim is made.
}

func TestPostgresApprovalReplayReturnsStoredRequester(t *testing.T) {
	dsn := os.Getenv("NEXAROUTE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("requires a disposable PostgreSQL database")
	}
	ctx := context.Background()
	store, err := OpenPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := Continue95NexaflowPGID("approval-replay")
	defer store.db.ExecContext(ctx, `DELETE FROM nexaroute_workflow_approvals WHERE run_id=$1`, run)
	original := Approval{RunID: run, NodeID: "n", Requester: "requester", Approver: "reviewer", Decision: "approved"}
	first, err := store.RecordApproval(ctx, original)
	if err != nil {
		t.Fatal(err)
	}
	replay := original
	replay.Requester = "different-requester"
	got, err := store.RecordApproval(ctx, replay)
	if err != nil {
		t.Fatal(err)
	}
	if got.Requester != first.Requester || !got.DecidedAt.Equal(first.DecidedAt) {
		t.Fatalf("replay fabricated approval: got=%+v first=%+v", got, first)
	}
}

type armFirstAppendStore struct {
	Store
	connector *lostCommitConnector
	armed     bool
}

func (s *armFirstAppendStore) AppendNodeExecution(ctx context.Context, node NodeExecution) (NodeExecution, error) {
	if !s.armed {
		s.armed = true
		s.connector.armed.Store(true)
	}
	return s.Store.AppendNodeExecution(ctx, node)
}

func TestPostgresIncompleteRunReplayAndExecuteFailClosed(t *testing.T) {
	dsn := os.Getenv("NEXAROUTE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("requires a disposable PostgreSQL database")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	connector := &lostCommitConnector{Connector: stdlib.GetConnector(*config)}
	db := sql.OpenDB(connector)
	defer db.Close()
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &armFirstAppendStore{Store: store, connector: connector}
	reg := testRegistry(t, fixedSpec("test.echo", nil, nil))
	eng := mustEngine(t, wrapped, reg)
	def := chainDef(struct {
		Instance, NodeID string
		Config           map[string]any
	}{"work", "test.echo", nil})
	def.ID = Continue95NexaflowPGID("incomplete-def")
	key := Continue95NexaflowPGID("incomplete-key")
	pub := PublishedDefinition{Definition: def, Version: DefinitionVersion{DefinitionID: def.ID, Revision: 1}}
	_, err = eng.CreateRun(ctx, pub, principal(), nil, key)
	if !errors.Is(err, ErrPostgresCommitUncertain) {
		t.Fatalf("first create error: %v", err)
	}
	run, err := store.GetRunByIdempotency(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM `+wfNodesTable+` WHERE run_id=$1`, run.ID)
		_, _ = db.ExecContext(ctx, `DELETE FROM `+wfRunsTable+` WHERE run_id=$1`, run.ID)
	}()
	if _, err := eng.CreateRun(ctx, pub, principal(), nil, key); !errors.Is(err, ErrRunInitializationIncomplete) {
		t.Fatalf("replay should fail closed: %v", err)
	}
	if _, err := eng.Execute(ctx, run.ID); !errors.Is(err, ErrRunInitializationIncomplete) {
		t.Fatalf("execute should fail closed: %v", err)
	}
	nodes, err := store.GetNodeExecutions(ctx, run.ID)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("unexpected retries or loss: %+v, %v", nodes, err)
	}
	stored, err := store.GetRun(ctx, run.ID)
	if err != nil || stored.State != RunCreated {
		t.Fatalf("incomplete run advanced: %+v, %v", stored, err)
	}
}
