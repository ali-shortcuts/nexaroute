package nexaflow

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

// The proxy forwards the PostgreSQL protocol unchanged, except that once
// armed it drops the server's CommandComplete(COMMIT) message and closes both
// sockets. Seeing that message proves the actual server executed COMMIT; the
// client never receives the acknowledgement. It is a local network fault, not
// a server crash, power-loss, failover, or remote-network acceptance test.
type commitDropProxy struct {
	listener                         net.Listener
	upstreamNetwork, upstreamAddress string
	armed                            atomic.Bool
	dropped                          atomic.Bool
	dropAutoTag                      atomic.Pointer[string]
}

func (p *commitDropProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		go p.forward(client)
	}
}
func (p *commitDropProxy) forward(client net.Conn) {
	defer client.Close()
	server, err := net.Dial(p.upstreamNetwork, p.upstreamAddress)
	if err != nil {
		return
	}
	defer server.Close()
	go func() { _, _ = io.Copy(server, client); _ = server.Close(); _ = client.Close() }()
	pendingAutoCommit := false
	for {
		var header [5]byte
		if _, err := io.ReadFull(server, header[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(header[1:])
		if n < 4 || n > 16<<20 {
			return
		}
		body := make([]byte, int(n)-4)
		if _, err := io.ReadFull(server, body); err != nil {
			return
		}
		if header[0] == 'C' {
			if tag := p.dropAutoTag.Load(); tag != nil && string(body) == *tag+"\x00" && p.dropAutoTag.CompareAndSwap(tag, nil) {
				pendingAutoCommit = true
			}
		}
		// ReadyForQuery follows completion of the implicit transaction.
		if header[0] == 'Z' && pendingAutoCommit {
			p.dropped.Store(true)
			return
		}
		if header[0] == 'C' && string(body) == "COMMIT\x00" && p.armed.CompareAndSwap(true, false) {
			p.dropped.Store(true)
			return
		}
		if _, err := client.Write(header[:]); err != nil {
			return
		}
		if _, err := client.Write(body); err != nil {
			return
		}
	}
}

func TestPostgresNetworkDropsCommitAcknowledgement(t *testing.T) {
	dsn := os.Getenv("NEXAROUTE_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("requires disposable PostgreSQL")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	// Do not intercept or downgrade TLS for remote/shared servers. This
	// test is expressly restricted to a local, plaintext disposable DB.
	if config.TLSConfig != nil || len(config.Fallbacks) != 0 {
		t.Skip("requires sslmode=disable and no fallback hosts")
	}
	p := &commitDropProxy{}
	if strings.HasPrefix(config.Host, "/") {
		p.upstreamNetwork = "unix"
		p.upstreamAddress = filepath.Join(config.Host, fmt.Sprintf(".s.PGSQL.%d", config.Port))
	} else if ip := net.ParseIP(config.Host); ip != nil && ip.IsLoopback() {
		p.upstreamNetwork = "tcp"
		p.upstreamAddress = net.JoinHostPort(config.Host, fmt.Sprint(config.Port))
	} else {
		t.Skip("requires local Unix socket or loopback PostgreSQL")
	}
	p.listener, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer p.listener.Close()
	go p.serve()
	config.Host = "127.0.0.1"
	config.Port = uint16(p.listener.Addr().(*net.TCPAddr).Port)
	db := sql.OpenDB(stdlib.GetConnector(*config))
	defer db.Close()
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	runID := Continue95NexaflowPGID("network-commit")
	key := Continue95NexaflowPGID("network-key")
	defer func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM `+wfNodesTable+` WHERE run_id=$1`, runID)
		_, _ = db.ExecContext(ctx, `DELETE FROM `+wfApprovalsTable+` WHERE run_id=$1`, runID)
		_, _ = db.ExecContext(ctx, `DELETE FROM `+wfRunsTable+` WHERE run_id=$1`, runID)
	}()
	p.armed.Store(true)
	err = store.CreateRun(ctx, Run{ID: runID, DefinitionID: "wf", IdempotencyKey: key})
	if !p.dropped.Load() || !errors.Is(err, ErrPostgresCommitUncertain) {
		t.Fatalf("network fault not surfaced as uncertain: dropped=%v err=%v", p.dropped.Load(), err)
	}
	got, err := store.GetRunByIdempotency(ctx, key)
	if err != nil || got.ID != runID {
		t.Fatalf("reconnect and reconcile run: %+v %v", got, err)
	}
	p.dropped.Store(false)
	p.armed.Store(true)
	_, err = store.AppendNodeExecution(ctx, NodeExecution{RunID: runID, NodeID: "n", State: NodePending, AppendOperationID: "initial"})
	if !p.dropped.Load() || !errors.Is(err, ErrPostgresCommitUncertain) {
		t.Fatalf("append network fault not surfaced as uncertain: dropped=%v err=%v", p.dropped.Load(), err)
	}
	nodes, err := store.GetNodeExecutions(ctx, runID)
	if err != nil || len(nodes) != 1 || nodes[0].Attempt != 0 {
		t.Fatalf("reconnect and reconcile attempt: %+v %v", nodes, err)
	}
	replayed, err := store.AppendNodeExecution(ctx, NodeExecution{RunID: runID, NodeID: "n", State: NodePending, AppendOperationID: "initial"})
	if err != nil || replayed.Attempt != 0 {
		t.Fatalf("append replay: %+v %v", replayed, err)
	}
	nodes, err = store.GetNodeExecutions(ctx, runID)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("append replay duplicated: %+v %v", nodes, err)
	}
	arm := func() { p.dropped.Store(false); p.armed.Store(true) }
	check := func(operation string, err error) {
		t.Helper()
		if !p.dropped.Load() || !errors.Is(err, ErrPostgresCommitUncertain) {
			t.Fatalf("%s: dropped=%v err=%v", operation, p.dropped.Load(), err)
		}
	}
	arm()
	_, _, err = store.TryClaim(ctx, runID, "n", "owner", time.Minute, time.Now())
	check("TryClaim", err)
	nodes, err = store.GetNodeExecutions(ctx, runID)
	if err != nil || len(nodes) != 1 || nodes[0].LeaseOwner != "owner" || nodes[0].State != NodeClaimed {
		t.Fatalf("claim reconciliation: %+v %v", nodes, err)
	}
	// ReleaseLease uses an autocommit UPDATE; inspect after the server's
	// ReadyForQuery is dropped rather than assuming a retry is safe.
	autoRelease := "UPDATE 1"
	p.dropped.Store(false)
	p.dropAutoTag.Store(&autoRelease)
	err = store.ReleaseLease(ctx, runID, "n", "owner")
	if !p.dropped.Load() || err == nil {
		t.Fatalf("ReleaseLease ack loss dropped=%v err=%v", p.dropped.Load(), err)
	}
	nodes, err = store.GetNodeExecutions(ctx, runID)
	if err != nil || len(nodes) != 1 || nodes[0].LeaseOwner != "" {
		t.Fatalf("release reconciliation: %+v %v", nodes, err)
	}
	// Reclaim so the following state transition remains representative.
	if _, ok, err := store.TryClaim(ctx, runID, "n", "owner", time.Minute, time.Now()); err != nil || !ok {
		t.Fatalf("reclaim: ok=%v err=%v", ok, err)
	}
	nodes, err = store.GetNodeExecutions(ctx, runID)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("reclaim read: %+v %v", nodes, err)
	}
	node := nodes[0]
	node.State = NodeRunning
	arm()
	_, err = store.SaveNodeExecution(ctx, node, NodeClaimed)
	check("SaveNodeExecution", err)
	nodes, err = store.GetNodeExecutions(ctx, runID)
	if err != nil || len(nodes) != 1 || nodes[0].State != NodeRunning {
		t.Fatalf("save reconciliation: %+v %v", nodes, err)
	}
	arm()
	_, err = store.UpdateRunState(ctx, runID, RunCreated, RunRunning, "", "")
	check("UpdateRunState", err)
	got, err = store.GetRun(ctx, runID)
	if err != nil || got.State != RunRunning {
		t.Fatalf("state reconciliation: %+v %v", got, err)
	}
	arm()
	_, err = store.RecordApproval(ctx, Approval{RunID: runID, NodeID: "n", Requester: "requester", Approver: "reviewer", Decision: "approved"})
	check("RecordApproval", err)
	approval, err := store.GetApproval(ctx, runID, "n")
	if err != nil || approval.Decision != "approved" {
		t.Fatalf("approval reconciliation: %+v %v", approval, err)
	}
	def := Continue95NexaflowPGDef(Continue95NexaflowPGID("network-publish"))
	defer func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM `+wfRevsTable+` WHERE definition_id=$1`, def.ID)
		_, _ = db.ExecContext(ctx, `DELETE FROM `+wfDefsTable+` WHERE definition_id=$1`, def.ID)
	}()
	// SaveDraft is autocommit, so drop ReadyForQuery only after seeing the
	// INSERT completion: the server has committed before that ready message.
	autoTag := "INSERT 0 1"
	p.dropped.Store(false)
	p.dropAutoTag.Store(&autoTag)
	err = store.SaveDraft(ctx, def)
	if !p.dropped.Load() || err == nil {
		t.Fatalf("SaveDraft ack loss dropped=%v err=%v", p.dropped.Load(), err)
	}
	draft, err := store.GetDraft(ctx, def.ID)
	if err != nil || draft.ID != def.ID {
		t.Fatalf("SaveDraft reconciliation: %+v %v", draft, err)
	}
	arm()
	_, err = store.Publish(ctx, def.ID, "tester")
	check("Publish", err)
	pub, err := store.Publish(ctx, def.ID, "tester")
	if err != nil || pub.Version.Revision != 1 {
		t.Fatalf("publish replay: %+v %v", pub, err)
	}
	def.Name = "changed"
	if err := store.SaveDraft(ctx, def); err != nil {
		t.Fatal(err)
	}
	hash, err := ContentHash(&def)
	if err != nil {
		t.Fatal(err)
	}
	arm()
	_, err = store.PublishIfUnchanged(ctx, def.ID, "tester", hash)
	check("PublishIfUnchanged", err)
	pub, err = store.PublishIfUnchanged(ctx, def.ID, "tester", hash)
	if err != nil || pub.Version.Revision != 2 {
		t.Fatalf("conditional publish replay: %+v %v", pub, err)
	}
	// Prune is global to a store, so isolate it in a fresh disposable schema
	// rather than deleting unrelated test rows in the service database.
	schema := fmt.Sprintf("nf_fault_%x", time.Now().UnixNano())
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = db.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE") }()
	isolatedConfig := config.Copy()
	isolatedConfig.RuntimeParams["search_path"] = schema
	isolatedDB := sql.OpenDB(stdlib.GetConnector(*isolatedConfig))
	defer isolatedDB.Close()
	isolated, err := NewPostgresStore(ctx, isolatedDB)
	if err != nil {
		t.Fatal(err)
	}
	// Drop the response to the INSERT while still inside Append's explicit
	// transaction. Closing the connection before COMMIT rolls it back.
	rollbackID := Continue95NexaflowPGID("before-commit")
	if err := isolated.CreateRun(ctx, Run{ID: rollbackID, DefinitionID: "wf"}); err != nil {
		t.Fatal(err)
	}
	beforeTag := "INSERT 0 1"
	p.dropped.Store(false)
	p.dropAutoTag.Store(&beforeTag)
	_, err = isolated.AppendNodeExecution(ctx, NodeExecution{RunID: rollbackID, NodeID: "n", AppendOperationID: "initial"})
	if !p.dropped.Load() || err == nil || errors.Is(err, ErrPostgresCommitUncertain) {
		t.Fatalf("before commit loss dropped=%v err=%v", p.dropped.Load(), err)
	}
	rolledBack, err := isolated.GetNodeExecutions(ctx, rollbackID)
	if err != nil || len(rolledBack) != 0 {
		t.Fatalf("uncommitted append visible: %+v %v", rolledBack, err)
	}
	retry, err := isolated.AppendNodeExecution(ctx, NodeExecution{RunID: rollbackID, NodeID: "n", AppendOperationID: "initial"})
	if err != nil || retry.Attempt != 0 {
		t.Fatalf("retry after rollback: %+v %v", retry, err)
	}

	// Real PostgreSQL SERIALIZABLE conflict and rollback, not a claim that
	// workflow stores currently use serializable isolation.
	if _, err := isolatedDB.ExecContext(ctx, `CREATE TABLE serialization_probe (id INT PRIMARY KEY, n INT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := isolatedDB.ExecContext(ctx, `INSERT INTO serialization_probe VALUES (1,0)`); err != nil {
		t.Fatal(err)
	}
	tx1, err := isolatedDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		t.Fatal(err)
	}
	defer tx1.Rollback()
	tx2, err := isolatedDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback()
	var n1, n2 int
	if err := tx1.QueryRowContext(ctx, `SELECT n FROM serialization_probe WHERE id=1`).Scan(&n1); err != nil {
		t.Fatal(err)
	}
	if err := tx2.QueryRowContext(ctx, `SELECT n FROM serialization_probe WHERE id=1`).Scan(&n2); err != nil {
		t.Fatal(err)
	}
	if _, err := tx1.ExecContext(ctx, `UPDATE serialization_probe SET n=n+1 WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if err := tx1.Commit(); err != nil {
		t.Fatal(err)
	}
	_, err = tx2.ExecContext(ctx, `UPDATE serialization_probe SET n=n+1 WHERE id=1`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "40001" {
		t.Fatalf("expected serialization failure, got %v", err)
	}
	_ = tx2.Rollback()
	if err := isolatedDB.QueryRowContext(ctx, `SELECT n FROM serialization_probe WHERE id=1`).Scan(&n1); err != nil || n1 != 1 {
		t.Fatalf("rollback state n=%d err=%v", n1, err)
	}

	pruneID := Continue95NexaflowPGID("network-prune")
	if err := isolated.CreateRun(ctx, Run{ID: pruneID, DefinitionID: "wf", State: RunSucceeded, FinishedAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := isolated.AppendNodeExecution(ctx, NodeExecution{RunID: pruneID, NodeID: "n", AppendOperationID: "initial"}); err != nil {
		t.Fatal(err)
	}
	if _, err := isolated.RecordApproval(ctx, Approval{RunID: pruneID, NodeID: "n", Approver: "reviewer", Decision: "approved"}); err != nil {
		t.Fatal(err)
	}
	arm()
	_, err = isolated.Prune(ctx, time.Now())
	check("Prune", err)
	if _, err := isolated.GetRun(ctx, pruneID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("prune reconciliation: %v", err)
	}
	if _, err := isolated.GetApproval(ctx, pruneID, "n"); !errors.Is(err, ErrNoApprovalRecord) {
		t.Fatalf("prune approval retained: %v", err)
	}
	if n, err := isolated.Prune(ctx, time.Now()); err != nil || n != 0 {
		t.Fatalf("prune replay: count=%d err=%v", n, err)
	}
}
