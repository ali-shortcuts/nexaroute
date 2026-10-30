package controlplane

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type Continue95FakeStore struct {
	getVal Snapshot
	getErr error
	putVal Snapshot
	putErr error
}

func (f *Continue95FakeStore) Get(context.Context, string) (Snapshot, error) {
	if f.getErr != nil {
		return Snapshot{}, f.getErr
	}
	return f.getVal, nil
}

func (f *Continue95FakeStore) Put(context.Context, string, uint64, []byte) (Snapshot, error) {
	if f.putErr != nil {
		return Snapshot{}, f.putErr
	}
	return f.putVal, nil
}

func (f *Continue95FakeStore) Delete(context.Context, string, uint64) error { return nil }

func TestContinue95ManagerBranches(t *testing.T) {
	ctx := context.Background()
	if _, err := NewManager(nil, LastKnownGood); err == nil {
		t.Error("nil store should fail NewManager")
	}
	if _, err := NewManager(&Continue95FakeStore{}, FailureMode("bogus")); err == nil {
		t.Error("invalid mode should fail NewManager")
	}
	if err := (Availability{Config: FailClosed, Identity: FailClosed, Budget: FailClosed, RateLimit: FailureMode("bogus")}).Validate(); err == nil {
		t.Error("invalid availability mode should fail")
	}
	if err := (Availability{Config: FailClosed, Identity: FailClosed, Budget: FailClosed, RateLimit: FailClosed}).Validate(); err != nil {
		t.Errorf("valid availability should pass: %v", err)
	}

	fake := &Continue95FakeStore{getVal: Snapshot{Revision: 3, Schema: 1, Payload: []byte("abc")}, putVal: Snapshot{Revision: 4, Schema: 1, Payload: []byte("def")}}
	mgr, err := NewManager(fake, LastKnownGood)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// Load success populates last-known-good (previously uncovered path).
	got, err := mgr.Load(ctx, "ns")
	if err != nil || got.Revision != 3 || string(got.Payload) != "abc" {
		t.Fatalf("Load success = %+v err=%v", got, err)
	}
	if _, ok := mgr.LastKnown("ns"); !ok {
		t.Error("LastKnown should hold the loaded snapshot")
	}
	mgr.Invalidate("ns")
	if _, ok := mgr.LastKnown("ns"); ok {
		t.Error("Invalidate should drop last-known-good")
	}
	// Save success re-populates, then an unavailable Load falls back.
	if _, err := mgr.Save(ctx, "ns", 3, []byte("def")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fake.getErr = ErrUnavailable
	fallback, err := mgr.Load(ctx, "ns")
	if err != nil || fallback.Revision != 4 {
		t.Fatalf("Load fallback = %+v err=%v", fallback, err)
	}
	// Load conflict counts conflicts.
	fake.getErr = ErrConflict
	if _, err := mgr.Load(ctx, "ns"); !errors.Is(err, ErrConflict) {
		t.Errorf("Load conflict err = %v", err)
	}
	// Save unavailable / conflict count and return the error.
	fake.putErr = ErrUnavailable
	if _, err := mgr.Save(ctx, "ns", 4, []byte("x")); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Save unavailable err = %v", err)
	}
	fake.putErr = ErrConflict
	if _, err := mgr.Save(ctx, "ns", 4, []byte("x")); !errors.Is(err, ErrConflict) {
		t.Errorf("Save conflict err = %v", err)
	}
	h := mgr.Health()
	if h.Loads == 0 || h.Saves == 0 || h.Unavailable == 0 || h.Conflicts == 0 {
		t.Errorf("health counters incomplete: %+v", h)
	}
	// Reconcile: durable success, non-unavailable error, fallback miss.
	fake.getErr = nil
	fake.getVal = Snapshot{Revision: 9, Schema: 1, Payload: []byte("z")}
	res, err := mgr.Reconcile(ctx, "ns")
	if err != nil || res.Source != "durable" || res.Revision != 9 {
		t.Fatalf("Reconcile durable = %+v err=%v", res, err)
	}
	fake.getErr = ErrConflict
	if _, err := mgr.Reconcile(ctx, "ns"); !errors.Is(err, ErrConflict) {
		t.Errorf("Reconcile conflict err = %v", err)
	}
	mgr2, _ := NewManager(&Continue95FakeStore{getErr: ErrUnavailable}, LastKnownGood)
	if _, err := mgr2.Reconcile(ctx, "missing"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Reconcile fallback miss err = %v", err)
	}
	// Reconcile fallback hit via a prior Save.
	fake2 := &Continue95FakeStore{putVal: Snapshot{Revision: 7, Schema: 1, Payload: []byte("q")}, getErr: ErrUnavailable}
	mgr3, _ := NewManager(fake2, LastKnownGood)
	if _, err := mgr3.Save(ctx, "ns", 6, []byte("q")); err != nil {
		t.Fatalf("Save for reconcile fallback: %v", err)
	}
	res, err = mgr3.Reconcile(ctx, "ns")
	if err != nil || res.Source != "last_known_good" || res.Revision != 7 {
		t.Fatalf("Reconcile fallback = %+v err=%v", res, err)
	}
	// FailClosed manager: unavailable Load does not fall back.
	mgr4, _ := NewManager(&Continue95FakeStore{getErr: ErrUnavailable}, FailClosed)
	if _, err := mgr4.Load(ctx, "ns"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("fail-closed Load err = %v", err)
	}
}

func TestContinue95MemoryAndWrapBranches(t *testing.T) {
	ctx := context.Background()
	ms := NewMemoryStore()
	if _, err := ms.Put(ctx, "ns", 5, []byte("x")); !errors.Is(err, ErrConflict) {
		t.Errorf("memory new-namespace wrong revision err = %v", err)
	}
	ms.SetUnavailable(true)
	if _, err := ms.Put(ctx, "ns", 0, []byte("x")); !errors.Is(err, ErrUnavailable) {
		t.Errorf("memory unavailable Put err = %v", err)
	}
	ms.SetUnavailable(false)
	if got, err := ms.Put(ctx, "ns", 0, []byte("x")); err != nil || got.Revision != 1 {
		t.Fatalf("memory Put = %+v err=%v", got, err)
	}
	if !EqualPayload(Snapshot{Revision: 1, Payload: []byte("x")}, Snapshot{Revision: 1, Payload: []byte("x")}) {
		t.Error("EqualPayload should match identical snapshots")
	}
	if wrapStoreError("op", nil) != nil {
		t.Error("wrap nil should be nil")
	}
	if err := wrapStoreError("op", context.Canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("wrap canceled = %v", err)
	}
	if err := wrapStoreError("op", ErrConflict); !errors.Is(err, ErrConflict) {
		t.Errorf("wrap conflict = %v", err)
	}
	other := wrapStoreError("op", errors.New("dial refused"))
	if !errors.Is(other, ErrUnavailable) || !strings.Contains(other.Error(), "op: dial refused") {
		t.Errorf("wrap unknown = %v, want unavailable-wrapped", other)
	}
	if err := (&FileLockTimeoutError{Path: "/tmp/x.lock", Op: "put", Timeout: time.Second}).Error(); !strings.Contains(err, "put") {
		t.Errorf("lock timeout error = %q", err)
	}
	if !errors.Is((&FileLockTimeoutError{}).Unwrap(), ErrUnavailable) {
		t.Error("lock timeout should unwrap to ErrUnavailable")
	}
	reg, err := DefaultMigrationRegistry()
	if err != nil {
		t.Fatalf("DefaultMigrationRegistry: %v", err)
	}
	all := reg.All()
	if len(all) != 1 || all[0].Version != 1 {
		t.Errorf("default registry = %v", all)
	}
}

func TestContinue95FileStoreErrorBranches(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if _, err := NewFileStore(""); err == nil {
		t.Error("empty file store path should fail")
	}
	if _, err := NewFileStoreWithOptions(filepath.Join(dir, "s2.json"), FileStoreOptions{LockTimeout: -time.Second}); err != nil {
		t.Errorf("negative lock timeout should select default: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Get(cancelled, "ns"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get cancelled err = %v", err)
	}
	if _, err := store.Put(cancelled, "ns", 0, []byte("x")); !errors.Is(err, context.Canceled) {
		t.Errorf("Put cancelled err = %v", err)
	}
	if err := store.Delete(cancelled, "ns", 0); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete cancelled err = %v", err)
	}
	// Read error: store path is a directory.
	dirStore, _ := NewFileStore(dir)
	if _, err := dirStore.Get(ctx, "ns"); err == nil {
		t.Error("Get on directory store should fail")
	}
	// Lock-directory error: store path lives under a regular file.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked, _ := NewFileStore(filepath.Join(blocker, "store.json"))
	if _, err := blocked.Put(ctx, "ns", 0, []byte("x")); err == nil {
		t.Error("Put under a regular file should fail at lock creation")
	}
	// Backup: missing source, corrupt source, and backup under a regular file.
	if err := BackupFileStore(filepath.Join(dir, "missing.json"), filepath.Join(dir, "b.json")); err == nil {
		t.Error("Backup missing source should fail")
	}
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte(`{"schema": 2, "snapshots": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := BackupFileStore(corrupt, filepath.Join(dir, "b.json")); err == nil {
		t.Error("Backup corrupt source should fail")
	}
	good := filepath.Join(dir, "good.json")
	goodStore, _ := NewFileStore(good)
	if _, err := goodStore.Put(ctx, "ns", 0, []byte("payload")); err != nil {
		t.Fatalf("seed good store: %v", err)
	}
	if err := BackupFileStore(good, filepath.Join(blocker, "b.json")); err == nil {
		t.Error("Backup under a regular file should fail creating temp")
	}
	backup := filepath.Join(dir, "backup.json")
	if err := BackupFileStore(good, backup); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	// Restore: invalid backup, empty target path.
	badBackup := filepath.Join(dir, "badbackup.json")
	if err := os.WriteFile(badBackup, []byte(`{"schema": 9, "snapshots": {}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RestoreFileStore(filepath.Join(dir, "restored.json"), badBackup); err == nil {
		t.Error("Restore invalid backup should fail")
	}
	if err := RestoreFileStore(filepath.Join(dir, "missing-backup.json"), filepath.Join(dir, "nope.json")); err == nil {
		t.Error("Restore missing backup should fail")
	}
	if err := RestoreFileStore("", backup); err == nil {
		t.Error("Restore empty target should fail")
	}
	restored := filepath.Join(dir, "restored.json")
	if err := RestoreFileStore(restored, backup); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	rs, _ := NewFileStore(restored)
	got, err := rs.Get(ctx, "ns")
	if err != nil || string(got.Payload) != "payload" {
		t.Fatalf("restored Get = %+v err=%v", got, err)
	}
}

type Continue95FakeRedisHash struct {
	fields  map[string]string
	getErr  error
	evalRet any
	evalErr error
}

func (f *Continue95FakeRedisHash) HGetAll(context.Context, string) (map[string]string, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.fields, nil
}

func (f *Continue95FakeRedisHash) Eval(context.Context, string, []string, ...any) (any, error) {
	return f.evalRet, f.evalErr
}

func TestContinue95RedisParseAndDeleteBranches(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	valid := map[string]string{"revision": "3", "schema": "1", "payload": "abc", "updated_at": now}
	if snap, err := parseRedisSnapshot(valid); err != nil || snap.Revision != 3 {
		t.Fatalf("parse valid = %+v err=%v", snap, err)
	}
	missing := []struct {
		name   string
		fields map[string]string
	}{
		// Four fields but the required key absent, so the length check
		// passes and the per-key branch is what fails.
		{"no revision", map[string]string{"schema": "1", "payload": "abc", "updated_at": now, "extra": "x"}},
		{"no schema", map[string]string{"revision": "3", "payload": "abc", "updated_at": now, "extra": "x"}},
		{"no payload", map[string]string{"revision": "3", "schema": "1", "updated_at": now, "extra": "x"}},
		{"no updated", map[string]string{"revision": "3", "schema": "1", "payload": "abc", "extra": "x"}},
		{"bad revision", map[string]string{"revision": "0", "schema": "1", "payload": "abc", "updated_at": now}},
		{"bad schema", map[string]string{"revision": "3", "schema": "0", "payload": "abc", "updated_at": now}},
		{"bad time", map[string]string{"revision": "3", "schema": "1", "payload": "abc", "updated_at": "not-a-time"}},
		{"wrong count", map[string]string{"revision": "3"}},
	}
	for _, tc := range missing {
		if _, err := parseRedisSnapshot(tc.fields); !errors.Is(err, ErrMalformedSnapshot) {
			t.Errorf("%s: err = %v, want ErrMalformedSnapshot", tc.name, err)
		}
	}
	if _, err := NewRedisStore(nil); err == nil {
		t.Error("nil redis client should fail")
	}
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		ret  any
		want error
	}{
		{"non-int result", "oops", nil},
		{"malformed status", int64(-2), ErrMalformedSnapshot},
		{"unknown status", int64(99), nil},
		{"deleted ok", int64(1), nil},
		{"conflict", int64(0), ErrConflict},
	} {
		rs, err := NewRedisStore(&Continue95FakeRedisHash{evalRet: tc.ret})
		if err != nil {
			t.Fatal(err)
		}
		err = rs.Delete(ctx, "ns", 1)
		if tc.want != nil {
			if !errors.Is(err, tc.want) {
				t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
			}
		} else if tc.name == "non-int result" || tc.name == "unknown status" {
			if err == nil {
				t.Errorf("%s: expected an error", tc.name)
			}
		} else if err != nil && !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
	// Get via the fake hash client round-trips a valid snapshot.
	rs, _ := NewRedisStore(&Continue95FakeRedisHash{fields: valid})
	got, err := rs.Get(ctx, "ns")
	if err != nil || got.Revision != 3 {
		t.Fatalf("redis Get = %+v err=%v", got, err)
	}
}

// --- Fake SQL driver for SQLStore error paths ---

type Continue95SQLRows struct {
	cols   []string
	values [][]driver.Value
	pos    int
}

func (r *Continue95SQLRows) Columns() []string { return r.cols }
func (r *Continue95SQLRows) Close() error      { return nil }
func (r *Continue95SQLRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.pos])
	r.pos++
	return nil
}

type Continue95SQLTx struct {
	conn *Continue95SQLConn
}

func (tx *Continue95SQLTx) Commit() error {
	if tx.conn.failCommit != nil {
		return tx.conn.failCommit
	}
	return nil
}
func (tx *Continue95SQLTx) Rollback() error { return nil }

type Continue95SQLConn struct {
	failBegin    error
	failAdvisory error
	failScan     error
	scanNoRows   bool
	failInsert   error
	failCommit   error
	failDelete   error
	failProbe    error
}

func (c *Continue95SQLConn) Prepare(query string) (driver.Stmt, error) {
	return nil, fmt.Errorf("prepare not supported: %s", query)
}
func (c *Continue95SQLConn) Close() error { return nil }
func (c *Continue95SQLConn) Begin() (driver.Tx, error) {
	if c.failBegin != nil {
		return nil, c.failBegin
	}
	return &Continue95SQLTx{conn: c}, nil
}
func (c *Continue95SQLConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Begin()
}
func (c *Continue95SQLConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	switch {
	case strings.Contains(query, "pg_advisory"):
		if c.failAdvisory != nil {
			return nil, c.failAdvisory
		}
		return driver.ResultNoRows, nil
	case strings.HasPrefix(query, "INSERT INTO"):
		if c.failInsert != nil {
			return nil, c.failInsert
		}
		return driver.ResultNoRows, nil
	case strings.HasPrefix(query, "UPDATE"):
		if c.failInsert != nil {
			return nil, c.failInsert
		}
		return driver.ResultNoRows, nil
	case strings.HasPrefix(query, "DELETE FROM"):
		if c.failDelete != nil {
			return nil, c.failDelete
		}
		return Continue95SQLResult{rows: 0}, nil
	}
	return driver.ResultNoRows, nil
}
func (c *Continue95SQLConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "SELECT revision, schema_version") {
		if c.failScan != nil {
			return nil, c.failScan
		}
		if c.scanNoRows {
			return &Continue95SQLRows{cols: []string{"revision", "schema_version"}}, nil
		}
		return &Continue95SQLRows{cols: []string{"revision", "schema_version"}, values: [][]driver.Value{{int64(2), int64(1)}}}, nil
	}
	if strings.Contains(query, "SELECT revision FROM") {
		if c.failProbe != nil {
			return nil, c.failProbe
		}
		return &Continue95SQLRows{cols: []string{"revision"}}, nil
	}
	return nil, fmt.Errorf("unexpected query: %s", query)
}
func (c *Continue95SQLConn) Ping(context.Context) error { return nil }

type Continue95SQLResult struct{ rows int64 }

func (r Continue95SQLResult) LastInsertId() (int64, error) { return 0, nil }
func (r Continue95SQLResult) RowsAffected() (int64, error) { return r.rows, nil }

type Continue95SQLDriver struct{ conn *Continue95SQLConn }

func (d *Continue95SQLDriver) Open(string) (driver.Conn, error) { return d.conn, nil }
func (d *Continue95SQLDriver) OpenConnector(string) (driver.Connector, error) {
	return Continue95SQLConnector{driver: d}, nil
}

type Continue95SQLConnector struct{ driver *Continue95SQLDriver }

func (c Continue95SQLConnector) Connect(context.Context) (driver.Conn, error) {
	return c.driver.conn, nil
}
func (c Continue95SQLConnector) Driver() driver.Driver { return c.driver }

func Continue95OpenSQL(conn *Continue95SQLConn) *sql.DB {
	return sql.OpenDB(Continue95SQLConnector{driver: &Continue95SQLDriver{conn: conn}})
}

func TestContinue95SQLStoreErrorBranches(t *testing.T) {
	ctx := context.Background()
	// Begin failure.
	db := Continue95OpenSQL(&Continue95SQLConn{failBegin: errors.New("dial refused")})
	ss, err := NewSQLStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ss.Put(ctx, "ns", 0, []byte("x")); err == nil {
		t.Error("Put with failing Begin should fail")
	}
	if _, err := NewSQLStore(nil); err == nil {
		t.Error("nil SQL db should fail")
	}
	// Advisory-lock failure.
	db = Continue95OpenSQL(&Continue95SQLConn{failAdvisory: errors.New("lock refused")})
	ss, _ = NewSQLStore(db)
	if _, err := ss.Put(ctx, "ns", 0, []byte("x")); err == nil {
		t.Error("Put with failing advisory lock should fail")
	}
	// Scan returns no rows + nonzero expected revision -> conflict.
	db = Continue95OpenSQL(&Continue95SQLConn{scanNoRows: true})
	ss, _ = NewSQLStore(db)
	if _, err := ss.Put(ctx, "ns", 5, []byte("x")); !errors.Is(err, ErrConflict) {
		t.Errorf("Put missing-row wrong revision err = %v", err)
	}
	// Scan error (non-NoRows).
	db = Continue95OpenSQL(&Continue95SQLConn{failScan: errors.New("scan boom")})
	ss, _ = NewSQLStore(db)
	if _, err := ss.Put(ctx, "ns", 0, []byte("x")); err == nil {
		t.Error("Put with failing scan should fail")
	}
	// Insert failure after a no-rows scan.
	db = Continue95OpenSQL(&Continue95SQLConn{scanNoRows: true, failInsert: errors.New("insert boom")})
	ss, _ = NewSQLStore(db)
	if _, err := ss.Put(ctx, "ns", 0, []byte("x")); err == nil {
		t.Error("Put with failing insert should fail")
	}
	// Commit failure after a successful insert.
	db = Continue95OpenSQL(&Continue95SQLConn{scanNoRows: true, failCommit: errors.New("commit boom")})
	ss, _ = NewSQLStore(db)
	if _, err := ss.Put(ctx, "ns", 0, []byte("x")); err == nil {
		t.Error("Put with failing commit should fail")
	}
	// Delete: zero rows affected, then probe query fails.
	db = Continue95OpenSQL(&Continue95SQLConn{failProbe: errors.New("probe boom")})
	ss, _ = NewSQLStore(db)
	if err := ss.Delete(ctx, "ns", 1); err == nil {
		t.Error("Delete with failing probe should fail")
	}
	// Delete: zero rows, probe returns no rows -> not found.
	db = Continue95OpenSQL(&Continue95SQLConn{})
	ss, _ = NewSQLStore(db)
	if err := ss.Delete(ctx, "ns", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete missing namespace err = %v", err)
	}
}

func TestContinue95FileStoreMoreBranches(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	// FileStore Delete branches: missing, conflict, success, load error.
	store, _ := NewFileStore(filepath.Join(dir, "store.json"))
	if _, err := store.Put(ctx, "ns", 0, []byte("v1")); err != nil {
		t.Fatalf("seed Put: %v", err)
	}
	if err := store.Delete(ctx, "missing", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete missing err = %v", err)
	}
	if err := store.Delete(ctx, "ns", 99); !errors.Is(err, ErrConflict) {
		t.Errorf("Delete wrong revision err = %v", err)
	}
	if err := store.Delete(ctx, "ns", 1); err != nil {
		t.Errorf("Delete success err = %v", err)
	}
	if _, err := store.Get(ctx, "ns"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete err = %v", err)
	}
	dirStore, _ := NewFileStore(dir)
	if err := dirStore.Delete(ctx, "ns", 0); err == nil {
		t.Error("Delete on directory store should fail at load")
	}
	// Load branches: invalid JSON, wrong schema, nil snapshots.
	for _, tc := range []struct {
		name string
		body string
	}{
		{"invalid json", `not json`},
		{"wrong schema", `{"schema": 2, "snapshots": {}}`},
		{"nil snapshots", `{"schema": 1}`},
	} {
		p := filepath.Join(dir, "bad-"+tc.name+".json")
		// Avoid spaces in the file name for the first case only.
		p = filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".json")
		if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
			t.Fatal(err)
		}
		s, _ := NewFileStore(p)
		if _, err := s.Get(ctx, "ns"); err == nil {
			t.Errorf("%s: Get should fail at load", tc.name)
		}
	}
	// Persist write failure: seed a store, then make its directory
	// read-only so CreateTemp fails inside persist (lock + load still
	// succeed because both files already exist).
	roDir := filepath.Join(dir, "ro")
	if err := os.MkdirAll(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	roStore, _ := NewFileStore(filepath.Join(roDir, "store.json"))
	if _, err := roStore.Put(ctx, "ns", 0, []byte("v1")); err != nil {
		t.Fatalf("seed ro store: %v", err)
	}
	if err := os.Chmod(roDir, 0o555); err != nil {
		t.Fatal(err)
	}
	_, roErr := roStore.Put(ctx, "ns", 1, []byte("v2"))
	if err := os.Chmod(roDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if roErr == nil {
		t.Error("Put into a read-only directory should fail at persist")
	}
	// Restore onto an existing directory: rename of the temp file onto
	// a directory fails at the commit step.
	good := filepath.Join(dir, "good2.json")
	gs, _ := NewFileStore(good)
	if _, err := gs.Put(ctx, "ns", 0, []byte("payload")); err != nil {
		t.Fatalf("seed good2: %v", err)
	}
	backup := filepath.Join(dir, "backup2.json")
	if err := BackupFileStore(good, backup); err != nil {
		t.Fatalf("backup2: %v", err)
	}
	targetDir := filepath.Join(dir, "targetdir")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RestoreFileStore(targetDir, backup); err == nil {
		t.Error("Restore onto a directory should fail at rename commit")
	}
}

func TestContinue95FileLockChmodBranch(t *testing.T) {
	// The lock file is a symlink to /dev/null, which opens fine for
	// read/write but cannot be chmodded by a non-owner, so
	// acquireFileLock fails at the chmod step. As root, chmod
	// succeeds regardless of ownership, so this branch is only
	// exercisable by a non-root user (e.g. CI runners).
	if os.Geteuid() == 0 {
		t.Skip("chmod-failure branch requires a non-root user")
	}
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Symlink("/dev/null", filepath.Join(dir, ".controlplane.lock")); err != nil {
		t.Fatal(err)
	}
	linkStore, _ := NewFileStore(filepath.Join(dir, "store.json"))
	if _, err := linkStore.Put(ctx, "ns", 0, []byte("x")); err == nil {
		t.Error("Put with an un-chmoddable lock file should fail")
	}
}

func TestContinue95ManagerLoadFallbackMiss(t *testing.T) {
	mgr, err := NewManager(&Continue95FakeStore{getErr: ErrUnavailable}, LastKnownGood)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Load(context.Background(), "never-seen"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Load fallback miss err = %v", err)
	}
}

func TestContinue95FileLockTimeoutAndCtx(t *testing.T) {
	// Hold the advisory lock from this process (a separate open file
	// description, so flock conflicts) and verify a short-timeout Put
	// fails closed with a timeout error, and a context deadline aborts
	// the wait with the context error.
	dir := t.TempDir()
	storePath := filepath.Join(dir, "store.json")
	lockPath := filepath.Join(dir, ".controlplane.lock")
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("holder flock: %v", err)
	}
	defer syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
	store, err := NewFileStoreWithOptions(storePath, FileStoreOptions{LockTimeout: 30 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Put(context.Background(), "ns", 0, []byte("x"))
	if err == nil {
		t.Fatal("Put under a held lock should time out")
	}
	var timeoutErr *FileLockTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Errorf("Put lock error = %v, want FileLockTimeoutError", err)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("lock timeout should unwrap to ErrUnavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	store2, _ := NewFileStoreWithOptions(storePath, FileStoreOptions{LockTimeout: 5 * time.Second})
	if _, err := store2.Put(ctx, "ns", 0, []byte("x")); err == nil {
		t.Error("Put with an expiring context under a held lock should fail")
	}
}
