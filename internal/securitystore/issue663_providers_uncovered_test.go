package securitystore

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	bolt "go.etcd.io/bbolt"
)

// issue663CountingCtx reports no error for the first failAfter Err calls
// and context.Canceled afterwards, so a test can let an outer context
// check pass while failing the same check inside a transaction callback.
// It is a white-box double for the in-transaction cancellation branches.
type issue663CountingCtx struct {
	context.Context
	mu        sync.Mutex
	calls     int
	failAfter int
}

func (c *issue663CountingCtx) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls > c.failAfter {
		return context.Canceled
	}
	return nil
}

func issue663ValidHash() string { return strings.Repeat("a", 64) }

func issue663OpenStore(t *testing.T) *Store {
	t.Helper()
	// Open creates the parent directory with 0700 itself; the store
	// refuses a parent directory with wider permissions.
	path := filepath.Join(t.TempDir(), "private", "store.db")
	s, err := Open(path, 1000, 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func issue663AuditEvent() AuditEvent {
	return AuditEvent{Actor: "tester", Action: "read", Outcome: "ok", Status: 200}
}

// --- bbolt store ---

func TestIssue663ProvidersBoltOpenErrors(t *testing.T) {
	// The garbage file must live in a 0700 directory, or Open's
	// directory-permission check fires before bolt.Open sees it.
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	garbage := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(garbage, []byte("this is not a bolt database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(garbage, 1000, 30); err == nil {
		t.Fatal("Open of a non-bolt file was accepted")
	}

	// OpenReadOnly enforces the same permission rules as Open.
	s := issue663OpenStore(t)
	_ = s.Close()
	// Reopen read-only on a symlinked parent directory.
	storePath := filepath.Join(t.TempDir(), "private", "store.db")
	src, err := Open(storePath, 1000, 30)
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(t.TempDir(), "linkdir")
	if err := os.Symlink(filepath.Dir(storePath), linkDir); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(filepath.Join(linkDir, "store.db")); err == nil {
		t.Fatal("OpenReadOnly through a symlinked directory was accepted")
	}
	// The symlinked file must live in a 0700 directory, or the
	// directory-permission check fires before the symlink check.
	linkDir2 := filepath.Join(t.TempDir(), "private2")
	if err := os.MkdirAll(linkDir2, 0o700); err != nil {
		t.Fatal(err)
	}
	linkFile := filepath.Join(linkDir2, "link.db")
	if err := os.Symlink(storePath, linkFile); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(linkFile); err == nil {
		t.Fatal("OpenReadOnly of a symlinked file was accepted")
	}
	if err := os.Chmod(storePath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(storePath); err == nil {
		t.Fatal("OpenReadOnly of a 0644 file was accepted")
	}
}

// Cancellation inside transaction callbacks (outer check passes, inner
// check fails) and at the outer checks.
func TestIssue663ProvidersBoltContextBranches(t *testing.T) {
	s := issue663OpenStore(t)
	hash := issue663ValidHash()
	now := time.Now().UTC()
	sess := Session{Subject: "u", Issuer: "test", CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour)}

	// CreateSession: counting context fails inside the write transaction
	// (outer RotateSession check is call 1, transaction check is call 2).
	ctx := &issue663CountingCtx{Context: context.Background(), failAfter: 1}
	if err := s.CreateSession(ctx, hash, sess, issue663AuditEvent()); err == nil {
		t.Fatal("CreateSession with mid-transaction cancellation was accepted")
	}
	// A live create, then Session with a canceled view callback
	// (outer Session check is call 1, view check is call 2).
	if err := s.CreateSession(context.Background(), hash, sess, issue663AuditEvent()); err != nil {
		t.Fatal(err)
	}
	viewCtx := &issue663CountingCtx{Context: context.Background(), failAfter: 1}
	if _, _, err := s.Session(viewCtx, hash); err == nil {
		t.Fatal("Session with a canceled view was accepted")
	}
	// RevokeSession: invalid hash, canceled outer check, canceled inner check.
	if err := s.RevokeSession(context.Background(), "not-a-hash", issue663AuditEvent()); err == nil {
		t.Fatal("RevokeSession with an invalid hash was accepted")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.RevokeSession(canceled, hash, issue663AuditEvent()); err == nil {
		t.Fatal("RevokeSession with a canceled context was accepted")
	}
	innerCtx := &issue663CountingCtx{Context: context.Background(), failAfter: 1}
	if err := s.RevokeSession(innerCtx, hash, issue663AuditEvent()); err == nil {
		t.Fatal("RevokeSession with mid-transaction cancellation was accepted")
	}
	// AppendAudit / AuditRecords / VerifyAuditChain inner-callback checks.
	appendCtx := &issue663CountingCtx{Context: context.Background(), failAfter: 1}
	if err := s.AppendAudit(appendCtx, issue663AuditEvent()); err == nil {
		t.Fatal("AppendAudit with mid-transaction cancellation was accepted")
	}
	recordsCtx := &issue663CountingCtx{Context: context.Background(), failAfter: 1}
	if _, err := s.AuditRecords(recordsCtx, 10); err == nil {
		t.Fatal("AuditRecords with a canceled view was accepted")
	}
	if _, err := s.VerifyAuditChain(canceled); err == nil {
		t.Fatal("VerifyAuditChain with a canceled context was accepted")
	}
	verifyCtx := &issue663CountingCtx{Context: context.Background(), failAfter: 1}
	if _, err := s.VerifyAuditChain(verifyCtx); err == nil {
		t.Fatal("VerifyAuditChain with a canceled view was accepted")
	}
	// TouchSession rejects an invalid hash without touching the store.
	if _, _, err := s.TouchSession(context.Background(), "not-a-hash", now, time.Minute); err == nil {
		t.Fatal("TouchSession with an invalid hash was accepted")
	}
}

// VerifyAuditChain reports a malformed stored record instead of verifying.
func TestIssue663ProvidersBoltMalformedRecord(t *testing.T) {
	s := issue663OpenStore(t)
	if err := s.AppendAudit(context.Background(), issue663AuditEvent()); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 16)
	binary.BigEndian.PutUint64(key[:8], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint64(key[8:], 9999)
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketAudit).Put(key, []byte("{not-json"))
	}); err != nil {
		t.Fatal(err)
	}
	report, err := s.VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verified {
		t.Fatal("chain with a malformed record reported VERIFIED")
	}
	found := false
	for _, p := range report.Problems {
		if strings.Contains(p, "malformed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("malformed record problems = %v, want a malformed mention", report.Problems)
	}
}

// --- chaincore planAppend ---

func TestIssue663ProvidersPlanAppendChainedZeroWithMetadata(t *testing.T) {
	legacy := AuditEvent{Seq: 1, Actor: "tester", Action: "read", Outcome: "ok", Status: 200, Timestamp: time.Now().UTC()}
	st := chainState{
		Records:   []AuditEvent{legacy},
		Head:      chainRef{Seq: 1, Hash: recordHash(legacy, 1)},
		HeadState: refValid,
		Allocator: 1,
	}
	if _, err := planAuditAppend(st, issue663AuditEvent(), 1000, 24*time.Hour, time.Now()); err == nil {
		t.Fatal("append to a legacy-only log with chain metadata was accepted")
	}
}

func TestIssue663ProvidersPlanAppendAnchorCarry(t *testing.T) {
	anchor := chainRef{Seq: 5, Hash: "anchor-hash"}
	first := AuditEvent{Seq: 6, PrevHash: anchor.Hash, Actor: "tester", Action: "read", Outcome: "ok", Status: 200, Timestamp: time.Now().UTC()}
	first.Hash = canonicalAuditHash(first)
	st := chainState{
		Records:     []AuditEvent{first},
		Head:        chainRef{Seq: 6, Hash: recordHash(first, 6)},
		HeadState:   refValid,
		Anchor:      anchor,
		AnchorState: refValid,
		Allocator:   6,
	}
	plan, err := planAuditAppend(st, issue663AuditEvent(), 1000, 24*time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !plan.HasAnchor || plan.Anchor != anchor {
		t.Fatalf("append plan anchor = %+v (has=%v), want carried anchor %+v", plan.Anchor, plan.HasAnchor, anchor)
	}
	if plan.Event.Seq != 7 || plan.Event.PrevHash != recordHash(first, 6) {
		t.Fatalf("append plan event = %+v, want seq 7 linked to the first record", plan.Event)
	}
}

// --- Redis: go-redis wrappers over a loopback RESP server ---

// issue663RESPServer answers one RESP command per connection using the
// test-supplied responder, speaking just enough of the protocol for
// go-redis HGET/EVAL calls. No real Redis is involved.
type issue663RESPServer struct {
	t       *testing.T
	ln      net.Listener
	handler func(cmd string, args []string) string
}

func issue663StartRESPServer(t *testing.T, handler func(cmd string, args []string) string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &issue663RESPServer{t: t, ln: ln, handler: handler}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go srv.serve(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

func (s *issue663RESPServer) serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "*") {
			return
		}
		var n int
		if _, err := fmt.Sscanf(line, "*%d", &n); err != nil || n <= 0 {
			return
		}
		args := make([]string, 0, n)
		for i := 0; i < n; i++ {
			hdr, err := r.ReadString('\n')
			if err != nil || !strings.HasPrefix(hdr, "$") {
				return
			}
			var ln int
			if _, err := fmt.Sscanf(hdr, "$%d", &ln); err != nil || ln < 0 {
				return
			}
			buf := make([]byte, ln+2)
			if _, err := io.ReadFull(r, buf); err != nil {
				return
			}
			args = append(args, string(buf[:ln]))
		}
		if len(args) == 0 {
			return
		}
		// HELLO always expects a map reply (flat array in RESP2);
		// answer it in the server so handlers only deal with the
		// commands under test.
		if strings.ToUpper(args[0]) == "HELLO" {
			if _, err := conn.Write([]byte("*4\r\n$6\r\nserver\r\n$5\r\nredis\r\n$7\r\nversion\r\n$5\r\n7.0.0\r\n")); err != nil {
				return
			}
			continue
		}
		resp := s.handler(strings.ToUpper(args[0]), args[1:])
		if _, err := conn.Write([]byte(resp)); err != nil {
			return
		}
	}
}

func issue663GoRedisClient(t *testing.T, addr string) goRedisSecurityClient {
	t.Helper()
	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		MaxRetries:   0,
		Protocol:     2, // avoid the HELLO 3 negotiation; the fake speaks RESP2
		DialTimeout:  2 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	t.Cleanup(func() { _ = client.Close() })
	return goRedisSecurityClient{client: client}
}

func TestIssue663ProvidersGoRedisWrappers(t *testing.T) {
	// HGET nil -> not found, no error.
	addr := issue663StartRESPServer(t, func(cmd string, args []string) string {
		if cmd == "HGET" {
			return "$-1\r\n"
		}
		return "+OK\r\n"
	})
	c := issue663GoRedisClient(t, addr)
	if v, ok, err := c.HGet(context.Background(), "k", "f"); err != nil || ok || v != "" {
		t.Fatalf("HGET nil = (%q, %v, %v), want (\"\", false, nil)", v, ok, err)
	}
	// HGET hit returns the value.
	addr = issue663StartRESPServer(t, func(cmd string, args []string) string {
		if cmd == "HGET" {
			return "$5\r\nhello\r\n"
		}
		return "+OK\r\n"
	})
	c = issue663GoRedisClient(t, addr)
	if v, ok, err := c.HGet(context.Background(), "k", "f"); err != nil || !ok || v != "hello" {
		t.Fatalf("HGET hit = (%q, %v, %v), want (\"hello\", true, nil)", v, ok, err)
	}
	// HGET server error propagates.
	addr = issue663StartRESPServer(t, func(cmd string, args []string) string {
		if cmd == "HGET" {
			return "-ERR boom\r\n"
		}
		return "+OK\r\n"
	})
	c = issue663GoRedisClient(t, addr)
	if _, _, err := c.HGet(context.Background(), "k", "f"); err == nil {
		t.Fatal("HGET server error was swallowed")
	}
	// Snapshot Eval error propagates.
	addr = issue663StartRESPServer(t, func(cmd string, args []string) string {
		if cmd == "EVAL" {
			return "-ERR boom\r\n"
		}
		return "+OK\r\n"
	})
	c = issue663GoRedisClient(t, addr)
	if _, _, err := c.Snapshot(context.Background()); err == nil {
		t.Fatal("Snapshot Eval error was swallowed")
	}
	// Snapshot malformed shape (not a 2-element array) is rejected.
	addr = issue663StartRESPServer(t, func(cmd string, args []string) string {
		if cmd == "EVAL" {
			return "*1\r\n$3\r\nfoo\r\n"
		}
		return "+OK\r\n"
	})
	c = issue663GoRedisClient(t, addr)
	if _, _, err := c.Snapshot(context.Background()); err == nil {
		t.Fatal("Snapshot malformed shape was accepted")
	}
	// Snapshot success returns the parsed hashes.
	addr = issue663StartRESPServer(t, func(cmd string, args []string) string {
		if cmd == "EVAL" {
			return "*2\r\n*2\r\n$4\r\nhead\r\n$9\r\n{\"seq\":1}\r\n*0\r\n"
		}
		return "+OK\r\n"
	})
	c = issue663GoRedisClient(t, addr)
	meta, audit, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if meta["head"] != `{"seq":1}` || len(audit) != 0 {
		t.Fatalf("Snapshot = (%v, %v), want head map and empty audit", meta, audit)
	}
}

// --- Redis: fake/stub client paths ---

// issue663StubRedisClient implements RedisSecurityClient with scripted
// failures for the CAS and load paths.
type issue663StubRedisClient struct {
	hgetErr   error
	hgetValue string
	hgetFound bool
	eval      func() (any, error)
	cancel    context.CancelFunc
	evalCalls int
}

func (s *issue663StubRedisClient) HGetAll(context.Context, string) (map[string]string, error) {
	return map[string]string{}, nil
}
func (s *issue663StubRedisClient) HGet(context.Context, string, string) (string, bool, error) {
	if s.hgetErr != nil {
		return "", false, s.hgetErr
	}
	return s.hgetValue, s.hgetFound, nil
}
func (s *issue663StubRedisClient) Snapshot(context.Context) (map[string]string, map[string]string, error) {
	return map[string]string{}, map[string]string{}, nil
}
func (s *issue663StubRedisClient) Eval(context.Context, string, []string, ...any) (any, error) {
	s.evalCalls++
	if s.cancel != nil && s.evalCalls == 1 {
		s.cancel()
	}
	if s.eval != nil {
		return s.eval()
	}
	return int64(0), nil
}
func (s *issue663StubRedisClient) Ping(context.Context) error { return nil }
func (s *issue663StubRedisClient) Close() error               { return nil }

func TestIssue663ProvidersRedisStubPaths(t *testing.T) {
	// OpenRedis against a closed port fails closed at ping.
	if _, err := OpenRedis(context.Background(), "redis://127.0.0.1:1", 1000, 30); err == nil {
		t.Fatal("OpenRedis against a closed port was accepted")
	}
	// RotateSession rejects an invalid old hash.
	fake := newFakeRedisClient()
	store := newRedisStoreWithClient(fake, 1000, 24*time.Hour)
	now := time.Now().UTC()
	valid := Session{Subject: "u", Issuer: "test", CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour)}
	if err := store.RotateSession(context.Background(), "bad-old", issue663ValidHash(), valid, issue663AuditEvent()); err == nil {
		t.Fatal("RotateSession with an invalid old hash was accepted")
	}
	// TouchSession rejects an invalid hash.
	if _, _, err := store.TouchSession(context.Background(), "bad", time.Now(), time.Minute); err == nil {
		t.Fatal("TouchSession with an invalid hash was accepted")
	}
	// TouchSession load failure propagates.
	stub := &issue663StubRedisClient{hgetErr: errors.New("fake load failure")}
	stubStore := newRedisStoreWithClient(stub, 1000, 24*time.Hour)
	if _, _, err := stubStore.TouchSession(context.Background(), issue663ValidHash(), time.Now(), time.Minute); err == nil {
		t.Fatal("TouchSession load error was swallowed")
	}
	// CAS loops observe cancellation: the stub cancels the context on
	// its first Eval, so both the mutate and touch loops exit via ctx.
	ctx, cancel := context.WithCancel(context.Background())
	cancelStub := &issue663StubRedisClient{cancel: cancel}
	cancelStore := newRedisStoreWithClient(cancelStub, 1000, 24*time.Hour)
	if err := cancelStore.AppendAudit(ctx, issue663AuditEvent()); !errors.Is(err, context.Canceled) {
		t.Fatalf("mutate CAS cancellation = %v, want context.Canceled", err)
	}
	now = time.Now().UTC()
	encoded, err := json.Marshal(Session{Subject: "u", CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	touchStub := &issue663StubRedisClient{hgetValue: string(encoded), hgetFound: true, cancel: cancel2}
	touchStore := newRedisStoreWithClient(touchStub, 1000, 24*time.Hour)
	if _, _, err := touchStore.TouchSession(ctx2, issue663ValidHash(), now, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("touch CAS cancellation = %v, want context.Canceled", err)
	}
	// An over-retention record is pruned at append time (insert=false):
	// the event carries an explicit timestamp older than the cutoff.
	pruneStore := newRedisStoreWithClient(newFakeRedisClient(), 1000, 24*time.Hour)
	oldEvent := issue663AuditEvent()
	oldEvent.Timestamp = time.Now().Add(-48 * time.Hour)
	if err := pruneStore.AppendAudit(context.Background(), oldEvent); err != nil {
		t.Fatal(err)
	}
	records, err := pruneStore.AuditRecords(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("zero-retention append left %d records, want 0 (pruned)", len(records))
	}
	report, err := pruneStore.VerifyAuditChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.HasAnchor {
		t.Fatal("zero-retention append did not record a pruning anchor")
	}
}

// --- Postgres: fake driver paths ---

func TestIssue663ProvidersPostgresPaths(t *testing.T) {
	// OpenPostgres against a closed port fails closed at ping.
	if _, err := OpenPostgres(context.Background(), "postgres://127.0.0.1:1/db?sslmode=disable", 1000, 30); err == nil {
		t.Fatal("OpenPostgres against a closed port was accepted")
	}
	// Migration begin failure surfaces from migrate.
	db := newFakePGDB()
	db.failBegin = true
	name := fmt.Sprintf("issue663-fake-pg-%d", fakePGDriverSeq.Add(1))
	sql.Register(name, &fakePGDriver{db: db})
	raw, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	s := &PostgresStore{db: raw, maxAuditItems: 1000, retention: time.Hour}
	if err := s.migrate(context.Background()); err == nil || !strings.Contains(err.Error(), "begin migration") {
		t.Fatalf("migrate begin failure = %v, want a begin migration error", err)
	}
	// Chain metadata query failure surfaces from VerifyAuditChain.
	db2 := newFakePGDB()
	s2 := openFakePostgresOn(t, db2, 1000, time.Hour)
	db2.failQuery = "SELECT key, value"
	if _, err := s2.VerifyAuditChain(context.Background()); err == nil || !strings.Contains(err.Error(), "load chain metadata") {
		t.Fatalf("verify metadata query failure = %v, want a load chain metadata error", err)
	}
	// Malformed allocator metadata is rejected.
	db3 := newFakePGDB()
	s3 := openFakePostgresOn(t, db3, 1000, time.Hour)
	db3.meta[metaKeyAllocator] = []byte("not-json")
	if _, err := s3.VerifyAuditChain(context.Background()); err == nil || !strings.Contains(err.Error(), "allocator metadata is malformed") {
		t.Fatalf("verify allocator failure = %v, want an allocator malformed error", err)
	}
	// Verify begin failure surfaces.
	db4 := newFakePGDB()
	s4 := openFakePostgresOn(t, db4, 1000, time.Hour)
	db4.failBegin = true
	if _, err := s4.VerifyAuditChain(context.Background()); err == nil || !strings.Contains(err.Error(), "begin verify") {
		t.Fatalf("verify begin failure = %v, want a begin verify error", err)
	}
	// TouchSession: invalid hash rejected; zero time defaults to now.
	db5 := newFakePGDB()
	s5 := openFakePostgresOn(t, db5, 1000, time.Hour)
	if _, _, err := s5.TouchSession(context.Background(), "bad", time.Now(), time.Hour); err == nil {
		t.Fatal("TouchSession with an invalid hash was accepted")
	}
	hash := issue663ValidHash()
	now := time.Now().UTC()
	encoded, err := json.Marshal(Session{Subject: "u", CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	db5.sessions[hash] = encoded
	got, found, err := s5.TouchSession(context.Background(), hash, time.Time{}, time.Hour)
	if err != nil || !found {
		t.Fatalf("TouchSession with zero time = (%v, %v, %v), want a found session", got, found, err)
	}
	if got.LastSeen.IsZero() {
		t.Fatal("TouchSession with zero time did not stamp LastSeen")
	}
}
