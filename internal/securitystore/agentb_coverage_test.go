package securitystore

// Agent B coverage tests for the 100% mission: every remaining
// statement in this package that a deterministic test can reach.
// The provably unreachable branches (json.Marshal on plain structs,
// prune-loop checks already enforced by verifyChainTx, bbolt
// Delete/Put on keys Get just proved are plain values) were removed
// from production code with proof comments instead of being faked.

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	agentBHashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	agentBHashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	agentBHashC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func agentBValidEvent(ts time.Time) AuditEvent {
	return AuditEvent{Timestamp: ts, Actor: "alice", Action: "auth.login", Outcome: "success", Status: 200}
}

func agentBValidSession(expiresAt time.Time) Session {
	return Session{Subject: "alice", Issuer: "issuer", ExpiresAt: expiresAt}
}

// agentBStorePath returns a store path under a "private" subdirectory:
// t.TempDir() itself is 0770 under this environment's umask, while
// Open requires the parent directory to be 0700; the "private"
// subdirectory is created by Open's MkdirAll with 0700.
func agentBStorePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "private", "store.db")
}

func agentBOpenStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path, 100, 30)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.db.Close() })
	return s
}

func agentBKey(t *testing.T, hash string) []byte {
	t.Helper()
	key, err := validSessionHash(hash)
	if err != nil {
		t.Fatalf("validSessionHash(%q): %v", hash, err)
	}
	return key
}

func agentBAuditKey(ts time.Time, seq uint64) []byte {
	key := make([]byte, 16)
	binary.BigEndian.PutUint64(key[:8], uint64(ts.UnixNano()))
	binary.BigEndian.PutUint64(key[8:], seq)
	return key
}

// agentBPlantBolt creates the three buckets in a bbolt file and runs
// plant against it, then closes the file so Open can be called on the
// planted state.
func agentBPlantBolt(t *testing.T, path string, plant func(tx *bolt.Tx) error) {
	t.Helper()
	// bolt.Open does not create parent directories.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("plant mkdir: %v", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("plant bolt open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketSessions, bucketAudit, bucketMeta} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return plant(tx)
	}); err != nil {
		t.Fatalf("plant bolt: %v", err)
	}
}

func agentBRegisterFakePGDriver(t *testing.T, db *fakePGDB) string {
	t.Helper()
	name := fmt.Sprintf("agentb-fake-pg-%d", fakePGDriverSeq.Add(1))
	sql.Register(name, &fakePGDriver{db: db})
	return name
}

func agentBSetPostgresDriver(t *testing.T, name string) {
	t.Helper()
	old := postgresDriverName
	postgresDriverName = name
	t.Cleanup(func() { postgresDriverName = old })
}

func agentBSeededFakePostgres(t *testing.T) (*PostgresStore, *fakePGDB) {
	t.Helper()
	db := newFakePGDB()
	s := openFakePostgresOn(t, db, 1000, 365*24*time.Hour)
	if err := s.AppendAudit(context.Background(), agentBValidEvent(time.Now())); err != nil {
		t.Fatalf("seed fake audit: %v", err)
	}
	return s, db
}

// --- OpenPostgres ---

func TestAgentBOpenPostgresUnknownDriver(t *testing.T) {
	// sql.Open fails only for an unregistered driver name: pgx defers
	// DSN parsing to connect time, so a malformed DSN surfaces as a
	// Ping failure instead.
	agentBSetPostgresDriver(t, "agentb-no-such-driver")
	_, err := OpenPostgres(context.Background(), "postgres://127.0.0.1:5432/db", 100, 30)
	if err == nil || !strings.Contains(err.Error(), "invalid postgres configuration") {
		t.Fatalf("OpenPostgres with unknown driver: err=%v", err)
	}
}

func TestAgentBOpenPostgresMigrateFailure(t *testing.T) {
	db := newFakePGDB()
	db.failBegin = true
	agentBSetPostgresDriver(t, agentBRegisterFakePGDriver(t, db))
	_, err := OpenPostgres(context.Background(), "ignored-dsn", 100, 30)
	if err == nil || !strings.Contains(err.Error(), "begin migration") {
		t.Fatalf("OpenPostgres with failing migrate: err=%v", err)
	}
}

func TestAgentBOpenPostgresSuccessOverFake(t *testing.T) {
	db := newFakePGDB()
	agentBSetPostgresDriver(t, agentBRegisterFakePGDriver(t, db))
	s, err := OpenPostgres(context.Background(), "ignored-dsn", 100, 30)
	if err != nil {
		t.Fatalf("OpenPostgres over fake driver: %v", err)
	}
	_ = s.db.Close()
}

// --- loadChainTx ---

func TestAgentBPostgresLoadChainScanErrors(t *testing.T) {
	ctx := context.Background()
	t.Run("audit scan", func(t *testing.T) {
		s, db := agentBSeededFakePostgres(t)
		db.badAuditSeq = true
		err := s.AppendAudit(ctx, agentBValidEvent(time.Now()))
		if err == nil || !strings.Contains(err.Error(), "scan audit record") {
			t.Fatalf("bad audit seq: err=%v", err)
		}
	})
	t.Run("audit rows error", func(t *testing.T) {
		s, db := agentBSeededFakePostgres(t)
		db.auditRowsErr = true
		err := s.AppendAudit(ctx, agentBValidEvent(time.Now()))
		if err == nil || !strings.Contains(err.Error(), "load audit records") {
			t.Fatalf("audit rows error: err=%v", err)
		}
	})
	t.Run("meta scan", func(t *testing.T) {
		s, db := agentBSeededFakePostgres(t)
		db.badMetaKey = true
		err := s.AppendAudit(ctx, agentBValidEvent(time.Now()))
		if err == nil || !strings.Contains(err.Error(), "scan chain metadata") {
			t.Fatalf("bad meta key: err=%v", err)
		}
	})
	t.Run("meta rows error", func(t *testing.T) {
		s, db := agentBSeededFakePostgres(t)
		db.metaRowsErr = true
		err := s.AppendAudit(ctx, agentBValidEvent(time.Now()))
		if err == nil || !strings.Contains(err.Error(), "load chain metadata") {
			t.Fatalf("meta rows error: err=%v", err)
		}
	})
}

// --- applyPlanTx ---

func TestAgentBPostgresApplyPlanMarshalEvent(t *testing.T) {
	s, _ := agentBSeededFakePostgres(t)
	far := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	err := s.AppendAudit(context.Background(), agentBValidEvent(far))
	if err == nil || !strings.Contains(err.Error(), "year outside of range") {
		t.Fatalf("year-10000 event append: err=%v", err)
	}
}

func TestAgentBPostgresApplyPlanMetaFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("head write", func(t *testing.T) {
		s, db := agentBSeededFakePostgres(t)
		db.failMetaKey = metaKeyHead
		err := s.AppendAudit(ctx, agentBValidEvent(time.Now()))
		if err == nil || !strings.Contains(err.Error(), "store chain metadata") {
			t.Fatalf("failing head write: err=%v", err)
		}
	})
	t.Run("anchor write", func(t *testing.T) {
		db := newFakePGDB()
		s := openFakePostgresOn(t, db, 100, 24*time.Hour)
		old := time.Now().Add(-40 * 24 * time.Hour)
		if err := s.AppendAudit(ctx, agentBValidEvent(old)); err != nil {
			t.Fatalf("seed old audit: %v", err)
		}
		// The next append prunes everything (both records are over
		// the 1-day retention), so the plan carries an anchor.
		db.failMetaKey = metaKeyAnchor
		err := s.AppendAudit(ctx, agentBValidEvent(old))
		if err == nil || !strings.Contains(err.Error(), "store chain metadata") {
			t.Fatalf("failing anchor write: err=%v", err)
		}
	})
	t.Run("allocator write", func(t *testing.T) {
		s, db := agentBSeededFakePostgres(t)
		db.failMetaKey = metaKeyAllocator
		err := s.AppendAudit(ctx, agentBValidEvent(time.Now()))
		if err == nil || !strings.Contains(err.Error(), "store allocator") {
			t.Fatalf("failing allocator write: err=%v", err)
		}
	})
}

// --- AuditRecords ---

func TestAgentBPostgresAuditRecordsScanError(t *testing.T) {
	s, db := agentBSeededFakePostgres(t)
	db.auditRecordExtraCol = true
	_, err := s.AuditRecords(context.Background(), 10)
	if err == nil || !strings.Contains(err.Error(), "scan audit record") {
		t.Fatalf("AuditRecords with extra column: err=%v", err)
	}
}

// --- OpenRedis over a fake RESP server ---

// agentBFakeRESPServer answers HELLO with a minimal RESP3 server-info
// map (go-redis negotiates RESP3 first and rejects a simple-string
// reply to HELLO) and every other command with +PONG: go-redis only
// needs a non-error reply for its Ping (and the SETINFO /
// MAINT_NOTIFICATIONS preamble), so no real Redis is needed. It parses
// one RESP array per command and replies once per command: go-redis
// pipelines the two SETINFO commands in a single write, and a single
// reply per Read chunk would desynchronize the stream.
func agentBFakeRESPServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen fake RESP: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
					name, err := agentBReadRESPCommand(r)
					if err != nil {
						return
					}
					if strings.EqualFold(name, "hello") {
						_, _ = c.Write([]byte("%7\r\n$6\r\nserver\r\n$5\r\nredis\r\n$7\r\nversion\r\n$5\r\n7.0.0\r\n$5\r\nproto\r\n:3\r\n$2\r\nid\r\n:1\r\n$4\r\nmode\r\n$10\r\nstandalone\r\n$4\r\nrole\r\n$6\r\nmaster\r\n$7\r\nmodules\r\n*0\r\n"))
					} else {
						_, _ = c.Write([]byte("+PONG\r\n"))
					}
				}
			}(conn)
		}
	}()
	return ln.Addr().String()
}

// agentBReadRESPCommand reads one RESP array command and returns its
// first element (the command name), skipping the remaining elements.
func agentBReadRESPCommand(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) < 2 || line[0] != '*' {
		return "", fmt.Errorf("expected RESP array, got %q", line)
	}
	var n int
	if _, err := fmt.Sscanf(line[1:], "%d", &n); err != nil {
		return "", err
	}
	name := ""
	for i := 0; i < n; i++ {
		hdr, err := r.ReadString('\n')
		if err != nil {
			return "", err
		}
		hdr = strings.TrimRight(hdr, "\r\n")
		if len(hdr) < 2 || hdr[0] != '$' {
			return "", fmt.Errorf("expected bulk string, got %q", hdr)
		}
		var length int
		if _, err := fmt.Sscanf(hdr[1:], "%d", &length); err != nil {
			return "", err
		}
		buf := make([]byte, length+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		if i == 0 {
			name = string(buf[:length])
		}
	}
	return name, nil
}

func TestAgentBOpenRedisSuccessOverFakeRESP(t *testing.T) {
	addr := agentBFakeRESPServer(t)
	s, err := OpenRedis(context.Background(), "redis://"+addr, 100, 30)
	if err != nil {
		t.Fatalf("OpenRedis over fake RESP server: %v", err)
	}
	_ = s.client.Close()
}

// --- redis mutate ---

func TestAgentBRedisMutateMarshalEvent(t *testing.T) {
	store := newRedisStoreWithClient(newFakeRedisClient(), 1000, 365*24*time.Hour)
	// Seed a fresh record first: pruning is a sequence prefix, and a
	// lone year-10000 event self-prunes (its UnixNano overflows to a
	// value below the retention cutoff), so without the seed the plan
	// would skip the insert — and its Marshal — entirely.
	if err := store.AppendAudit(context.Background(), agentBValidEvent(time.Now())); err != nil {
		t.Fatalf("seed redis audit: %v", err)
	}
	far := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	err := store.mutate(context.Background(), agentBValidEvent(far), nil)
	if err == nil || !strings.Contains(err.Error(), "year outside of range") {
		t.Fatalf("mutate with year-10000 event: err=%v", err)
	}
}

// --- Open ---

func TestAgentBOpenLongFileName(t *testing.T) {
	dir := t.TempDir()
	// A final component over NAME_MAX (255) makes Lstat fail with
	// ENAMETOOLONG (not NotExist) after MkdirAll succeeded on the
	// parent directory.
	path := filepath.Join(dir, "private", strings.Repeat("x", 300)+".db")
	_, err := Open(path, 100, 30)
	if err == nil || !strings.Contains(err.Error(), "inspect security store") {
		t.Fatalf("over-long file name: err=%v", err)
	}
}

// agentBCorruptTopLevelBucketKey rewrites a valid bbolt file so the
// top-level entry named bucketName becomes a plain key instead of a
// bucket: it clears the bucket-leaf flag on that entry in the current
// root leaf page. bbolt exposes no public API to create a top-level
// plain key, so this byte-level edit is the only way to construct the
// foreign/corrupt file Open must fail closed on. Leaf pages carry no
// checksum (only meta pages do, and they are untouched), so the edit
// cannot invalidate the file. Layout (bbolt v1.4.3, little-endian):
// page header is 16 bytes (id u64, flags u16, count u16, overflow
// u32); meta starts at page offset 16 (magic u32, version u32,
// pageSize u32, flags u32, root pgid u64 at +16, txid u64 at +48);
// leaf elements are 16 bytes (flags u32, pos u32, ksize u32, vsize
// u32) and the key sits at element+pos.
func agentBCorruptTopLevelBucketKey(t *testing.T, path string, bucketName string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read store file: %v", err)
	}
	u32 := func(off int) uint32 { return binary.LittleEndian.Uint32(raw[off:]) }
	u64 := func(off int) uint64 { return binary.LittleEndian.Uint64(raw[off:]) }
	u16 := func(off int) uint16 { return binary.LittleEndian.Uint16(raw[off:]) }
	if len(raw) < 8192 {
		t.Fatalf("store file too small: %d bytes", len(raw))
	}
	pageSize := int(u32(24))
	if pageSize <= 0 || len(raw)%pageSize != 0 {
		t.Fatalf("implausible page size %d for %d-byte file", pageSize, len(raw))
	}
	if u32(16) != 0xED0CDAED {
		t.Fatalf("bad bbolt magic: %#x", u32(16))
	}
	// The current meta is the one with the higher txid.
	txid0, txid1 := u64(64), u64(pageSize+64)
	metaOff := 0
	if txid1 > txid0 {
		metaOff = pageSize
	}
	rootPgid := u64(metaOff + 32)
	rootOff := int(rootPgid) * pageSize
	if rootOff+16 > len(raw) {
		t.Fatalf("root page %d out of range", rootPgid)
	}
	if u16(rootOff+8) != 0x02 {
		t.Fatalf("root page is not a leaf page (flags %#x)", u16(rootOff+8))
	}
	if u32(rootOff+12) != 0 {
		t.Fatalf("root leaf page has overflow pages; surgery unsupported")
	}
	count := int(u16(rootOff + 10))
	for i := 0; i < count; i++ {
		elemOff := rootOff + 16 + i*16
		flags := u32(elemOff)
		pos := u32(elemOff + 4)
		ksize := u32(elemOff + 8)
		keyOff := elemOff + int(pos)
		if keyOff+int(ksize) > len(raw) {
			t.Fatalf("leaf key out of range")
		}
		if string(raw[keyOff:keyOff+int(ksize)]) != bucketName {
			continue
		}
		if flags&0x01 == 0 {
			t.Fatalf("entry %q is not a bucket", bucketName)
		}
		binary.LittleEndian.PutUint32(raw[elemOff:], flags&^uint32(0x01))
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatalf("write corrupted store file: %v", err)
		}
		return
	}
	t.Fatalf("top-level entry %q not found in root leaf page", bucketName)
}

func TestAgentBOpenForeignTopLevelKey(t *testing.T) {
	path := agentBStorePath(t)
	s, err := Open(path, 100, 30)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	agentBCorruptTopLevelBucketKey(t, path, string(bucketSessions))
	_, err = Open(path, 100, 30)
	if err == nil || !strings.Contains(err.Error(), "initialize security store") {
		t.Fatalf("open with plain top-level sessions key: err=%v", err)
	}
}

// --- RotateSession / RevokeSession ---

func TestAgentBRotateSessionMarshalError(t *testing.T) {
	s := agentBOpenStore(t, agentBStorePath(t))
	far := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	err := s.CreateSession(context.Background(), agentBHashA, agentBValidSession(far), agentBValidEvent(time.Now()))
	if err == nil || !strings.Contains(err.Error(), "year outside of range") {
		t.Fatalf("create session with year-10000 expiry: err=%v", err)
	}
}

func TestAgentBRotateSessionDeleteOldKeyError(t *testing.T) {
	path := agentBStorePath(t)
	agentBPlantBolt(t, path, func(tx *bolt.Tx) error {
		_, err := tx.Bucket(bucketSessions).CreateBucket(agentBKey(t, agentBHashA))
		return err
	})
	s := agentBOpenStore(t, path)
	err := s.RotateSession(context.Background(), agentBHashA, agentBHashB, agentBValidSession(time.Now().Add(time.Hour)), agentBValidEvent(time.Now()))
	if err == nil || !strings.Contains(err.Error(), "incompatible value") {
		t.Fatalf("rotate with bucket-valued old key: err=%v", err)
	}
	if _, found, _ := s.Session(context.Background(), agentBHashB); found {
		t.Fatal("failed rotation left a session behind")
	}
}

func TestAgentBRotateSessionPutError(t *testing.T) {
	path := agentBStorePath(t)
	agentBPlantBolt(t, path, func(tx *bolt.Tx) error {
		_, err := tx.Bucket(bucketSessions).CreateBucket(agentBKey(t, agentBHashB))
		return err
	})
	s := agentBOpenStore(t, path)
	err := s.RotateSession(context.Background(), "", agentBHashB, agentBValidSession(time.Now().Add(time.Hour)), agentBValidEvent(time.Now()))
	if err == nil || !strings.Contains(err.Error(), "incompatible value") {
		t.Fatalf("rotate with bucket-valued new key: err=%v", err)
	}
}

func TestAgentBRevokeSessionDeleteError(t *testing.T) {
	path := agentBStorePath(t)
	agentBPlantBolt(t, path, func(tx *bolt.Tx) error {
		_, err := tx.Bucket(bucketSessions).CreateBucket(agentBKey(t, agentBHashA))
		return err
	})
	s := agentBOpenStore(t, path)
	err := s.RevokeSession(context.Background(), agentBHashA, agentBValidEvent(time.Now()))
	if err == nil || !strings.Contains(err.Error(), "incompatible value") {
		t.Fatalf("revoke with bucket-valued key: err=%v", err)
	}
}

// --- appendAuditTx ---

func TestAgentBAppendAuditMarshalError(t *testing.T) {
	s := agentBOpenStore(t, agentBStorePath(t))
	far := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	err := s.AppendAudit(context.Background(), agentBValidEvent(far))
	if err == nil || !strings.Contains(err.Error(), "year outside of range") {
		t.Fatalf("append year-10000 event: err=%v", err)
	}
}

func TestAgentBAppendAuditAnchorPutError(t *testing.T) {
	path := agentBStorePath(t)
	oldTs := time.Now().Add(-50 * 24 * time.Hour)
	legacy, err := json.Marshal(AuditEvent{Timestamp: oldTs, Actor: "legacy", Action: "old", Outcome: "success", Status: 200})
	if err != nil {
		t.Fatalf("marshal legacy event: %v", err)
	}
	agentBPlantBolt(t, path, func(tx *bolt.Tx) error {
		// A legacy record (no hash): it verifies cleanly with no
		// chain metadata, and its age makes the next append prune
		// everything, so the anchor write is attempted.
		if err := tx.Bucket(bucketAudit).Put(agentBAuditKey(oldTs, 1), legacy); err != nil {
			return err
		}
		if _, err := tx.Bucket(bucketMeta).CreateBucket(keyChainAnchor); err != nil {
			return err
		}
		return tx.Bucket(bucketMeta).SetSequence(1)
	})
	s := agentBOpenStore(t, path)
	err = s.AppendAudit(context.Background(), agentBValidEvent(time.Now().Add(-40*24*time.Hour)))
	if err == nil || !strings.Contains(err.Error(), "incompatible value") {
		t.Fatalf("append with bucket-valued anchor key: err=%v", err)
	}
}

func TestAgentBAppendAuditHeadPutError(t *testing.T) {
	path := agentBStorePath(t)
	agentBPlantBolt(t, path, func(tx *bolt.Tx) error {
		_, err := tx.Bucket(bucketMeta).CreateBucket(keyChainHead)
		return err
	})
	s := agentBOpenStore(t, path)
	err := s.AppendAudit(context.Background(), agentBValidEvent(time.Now()))
	if err == nil || !strings.Contains(err.Error(), "incompatible value") {
		t.Fatalf("append with bucket-valued head key: err=%v", err)
	}
}
