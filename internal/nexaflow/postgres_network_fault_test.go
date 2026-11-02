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
	_, err = store.AppendNodeExecution(ctx, NodeExecution{RunID: runID, NodeID: "n", State: NodePending})
	if !p.dropped.Load() || !errors.Is(err, ErrPostgresCommitUncertain) {
		t.Fatalf("append network fault not surfaced as uncertain: dropped=%v err=%v", p.dropped.Load(), err)
	}
	nodes, err := store.GetNodeExecutions(ctx, runID)
	if err != nil || len(nodes) != 1 || nodes[0].Attempt != 0 {
		t.Fatalf("reconnect and reconcile attempt: %+v %v", nodes, err)
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
	if err := store.SaveDraft(ctx, def); err != nil {
		t.Fatal(err)
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
}
