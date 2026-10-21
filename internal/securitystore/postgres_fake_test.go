package securitystore

// Wave C-1 PostgreSQL fake: an in-memory database/sql driver emulating
// the securitystore schema (three tables + the exact query shapes the
// adapter issues) so the PostgresStore adapter — writer serialization,
// chain load/verify/plan/apply, session lifecycle — is proven in the
// default unit pass with no external service. The build-tagged
// integration suite proves the same contract against a real server;
// claims about PG behavior rest on fakes + that gated suite.
//
// The fake honors the adapter's concurrency discipline: writer
// transactions serialize on a mutex (like the allocator FOR UPDATE
// lock), so the parallel-append tests are meaningful. Error injection
// hooks (failBegin/failCommit/failQuery/failExec substrings) drive the
// adapter's fail-closed paths.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakePGDB struct {
	mu       sync.Mutex
	writerMu sync.Mutex
	sessions map[string][]byte
	audit    map[int64][]byte
	meta     map[string][]byte

	failBegin  bool
	failCommit bool
	failQuery  string // QueryContext fails when the query contains this
	failExec   string // ExecContext fails when the query contains this
}

func newFakePGDB() *fakePGDB {
	return &fakePGDB{
		sessions: map[string][]byte{},
		audit:    map[int64][]byte{},
		meta:     map[string][]byte{},
	}
}

type fakePGDriver struct{ db *fakePGDB }

func (d *fakePGDriver) Open(string) (driver.Conn, error) { return &fakePGConn{db: d.db}, nil }

type fakePGConn struct{ db *fakePGDB }

func (c *fakePGConn) Prepare(query string) (driver.Stmt, error) {
	return &fakePGStmt{conn: c, query: query}, nil
}
func (c *fakePGConn) Close() error { return nil }
func (c *fakePGConn) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

// CheckNamedValue lets the adapter's uint64 sequence arguments pass
// through database/sql (which otherwise rejects non-driver values
// before they reach the driver).
func (c *fakePGConn) CheckNamedValue(nv *driver.NamedValue) error {
	switch v := nv.Value.(type) {
	case uint64:
		nv.Value = int64(v)
	case uint:
		nv.Value = int64(v)
	case int:
		nv.Value = int64(v)
	case int32:
		nv.Value = int64(v)
	case string, []byte, int64, bool, nil, time.Time:
	default:
		_ = v
		return fmt.Errorf("fake-pg: unsupported arg type %T", nv.Value)
	}
	return nil
}

func (c *fakePGConn) Begin() (driver.Tx, error) { return c.beginTx(driver.TxOptions{}) }

func (c *fakePGConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.beginTx(opts)
}

func (c *fakePGConn) beginTx(opts driver.TxOptions) (driver.Tx, error) {
	if c.db.failBegin {
		return nil, errors.New("fake-pg: injected begin failure")
	}
	tx := &fakePGTx{db: c.db}
	if !opts.ReadOnly {
		c.db.writerMu.Lock()
		tx.write = true
		// Snapshot for rollback: writer transactions are
		// serialized, so restoring the snapshot on rollback
		// emulates real transaction atomicity (a failed
		// session rotation or audit append leaves no partial
		// writes behind).
		c.db.mu.Lock()
		tx.snapSessions = cloneBytesMap(c.db.sessions)
		tx.snapAudit = cloneAuditMap(c.db.audit)
		tx.snapMeta = cloneBytesMap(c.db.meta)
		c.db.mu.Unlock()
	}
	return tx, nil
}

func cloneBytesMap(m map[string][]byte) map[string][]byte {
	out := make(map[string][]byte, len(m))
	for k, v := range m {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

func cloneAuditMap(m map[int64][]byte) map[int64][]byte {
	out := make(map[int64][]byte, len(m))
	for k, v := range m {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

type fakePGTx struct {
	db           *fakePGDB
	write        bool
	done         bool
	snapSessions map[string][]byte
	snapAudit    map[int64][]byte
	snapMeta     map[string][]byte
}

func (x *fakePGTx) finish() {
	if !x.done {
		x.done = true
		if x.write {
			x.db.writerMu.Unlock()
		}
	}
}

func (x *fakePGTx) Commit() error {
	if x.done {
		return nil
	}
	if x.db.failCommit {
		x.finish()
		return errors.New("fake-pg: injected commit failure")
	}
	x.finish()
	return nil
}

func (x *fakePGTx) Rollback() error {
	if !x.done && x.write {
		x.db.mu.Lock()
		x.db.sessions = x.snapSessions
		x.db.audit = x.snapAudit
		x.db.meta = x.snapMeta
		x.db.mu.Unlock()
	}
	x.finish()
	return nil
}

type fakePGStmt struct {
	conn  *fakePGConn
	query string
}

func (s *fakePGStmt) Close() error  { return nil }
func (s *fakePGStmt) NumInput() int { return -1 }
func (s *fakePGStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.conn.exec(context.Background(), s.query, named(args))
}
func (s *fakePGStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.conn.query(context.Background(), s.query, named(args))
}

func named(args []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(args))
	for i, v := range args {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}

func argString(v driver.Value) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprint(t)
	}
}

func argBytes(v driver.Value) []byte {
	switch t := v.(type) {
	case []byte:
		return append([]byte(nil), t...)
	case string:
		return []byte(t)
	default:
		return []byte(fmt.Sprint(t))
	}
}

func argInt64(v driver.Value) int64 {
	switch t := v.(type) {
	case int64:
		return t
	case int:
		return int64(t)
	case int32:
		return int64(t)
	case uint64:
		return int64(t)
	case float64:
		return int64(t)
	case []byte:
		n, _ := strconv.ParseInt(string(t), 10, 64)
		return n
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	default:
		return 0
	}
}

type fakePGRows struct {
	cols []string
	rows [][]driver.Value
	pos  int
}

func (r *fakePGRows) Columns() []string { return r.cols }
func (r *fakePGRows) Close() error      { return nil }
func (r *fakePGRows) Next(dest []driver.Value) error {
	if r.pos >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.pos])
	r.pos++
	return nil
}

func (c *fakePGConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.query(ctx, query, args)
}

func (c *fakePGConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.exec(ctx, query, args)
}

func vals(args []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, len(args))
	for i, a := range args {
		out[i] = a.Value
	}
	return out
}

func (c *fakePGConn) query(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	db := c.db
	if db.failQuery != "" && strings.Contains(query, db.failQuery) {
		return nil, errors.New("fake-pg: injected query failure")
	}
	v := vals(args)
	db.mu.Lock()
	defer db.mu.Unlock()
	switch {
	case strings.Contains(query, `SELECT value FROM `+securityMetaTable):
		// Allocator lock read.
		raw, ok := db.meta[argString(v[0])]
		if !ok {
			return &fakePGRows{cols: []string{"value"}}, nil
		}
		return &fakePGRows{cols: []string{"value"}, rows: [][]driver.Value{{append([]byte(nil), raw...)}}}, nil
	case strings.Contains(query, `SELECT seq, record FROM `+securityAuditTable):
		seqs := make([]int64, 0, len(db.audit))
		for seq := range db.audit {
			seqs = append(seqs, seq)
		}
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
		rows := &fakePGRows{cols: []string{"seq", "record"}}
		for _, seq := range seqs {
			rows.rows = append(rows.rows, []driver.Value{seq, append([]byte(nil), db.audit[seq]...)})
		}
		return rows, nil
	case strings.Contains(query, `SELECT key, value FROM `+securityMetaTable):
		keys := make([]string, 0, len(db.meta))
		for k := range db.meta {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		rows := &fakePGRows{cols: []string{"key", "value"}}
		for _, k := range keys {
			rows.rows = append(rows.rows, []driver.Value{k, append([]byte(nil), db.meta[k]...)})
		}
		return rows, nil
	case strings.Contains(query, "SELECT EXISTS"):
		_, ok := db.sessions[argString(v[0])]
		return &fakePGRows{cols: []string{"exists"}, rows: [][]driver.Value{{ok}}}, nil
	case strings.Contains(query, `SELECT value FROM `+securitySessionsTable):
		raw, ok := db.sessions[argString(v[0])]
		if !ok {
			return &fakePGRows{cols: []string{"value"}}, nil
		}
		return &fakePGRows{cols: []string{"value"}, rows: [][]driver.Value{{append([]byte(nil), raw...)}}}, nil
	case strings.Contains(query, `SELECT record FROM `+securityAuditTable):
		limit := argInt64(v[0])
		seqs := make([]int64, 0, len(db.audit))
		for seq := range db.audit {
			seqs = append(seqs, seq)
		}
		sort.Slice(seqs, func(i, j int) bool { return seqs[i] > seqs[j] })
		rows := &fakePGRows{cols: []string{"record"}}
		for _, seq := range seqs {
			if int64(len(rows.rows)) >= limit {
				break
			}
			rows.rows = append(rows.rows, []driver.Value{append([]byte(nil), db.audit[seq]...)})
		}
		return rows, nil
	default:
		return nil, fmt.Errorf("fake-pg: unhandled query %q", query)
	}
}

type fakePGResult struct{ n int64 }

func (r fakePGResult) LastInsertId() (int64, error) {
	return 0, errors.New("fake-pg: no last insert id")
}
func (r fakePGResult) RowsAffected() (int64, error) { return r.n, nil }

func (c *fakePGConn) exec(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	db := c.db
	if db.failExec != "" && strings.Contains(query, db.failExec) {
		return nil, errors.New("fake-pg: injected exec failure")
	}
	v := vals(args)
	db.mu.Lock()
	defer db.mu.Unlock()
	switch {
	case strings.Contains(query, "pg_advisory_xact_lock"):
		return fakePGResult{}, nil
	case strings.HasPrefix(strings.TrimSpace(query), "CREATE TABLE"):
		return fakePGResult{}, nil
	case strings.Contains(query, securityMetaTable) && strings.Contains(query, "DO NOTHING"):
		if _, ok := db.meta[metaKeyAllocator]; !ok {
			db.meta[metaKeyAllocator] = []byte("0")
		}
		return fakePGResult{}, nil
	case strings.Contains(query, `DELETE FROM `+securityAuditTable):
		delete(db.audit, argInt64(v[0]))
		return fakePGResult{n: 1}, nil
	case strings.Contains(query, `INSERT INTO `+securityAuditTable):
		db.audit[argInt64(v[0])] = argBytes(v[1])
		return fakePGResult{n: 1}, nil
	case strings.Contains(query, `INSERT INTO `+securityMetaTable):
		db.meta[argString(v[0])] = argBytes(v[1])
		return fakePGResult{n: 1}, nil
	case strings.Contains(query, `DELETE FROM `+securitySessionsTable):
		delete(db.sessions, argString(v[0]))
		return fakePGResult{n: 1}, nil
	case strings.Contains(query, `INSERT INTO `+securitySessionsTable):
		db.sessions[argString(v[0])] = argBytes(v[1])
		return fakePGResult{n: 1}, nil
	case strings.Contains(query, `UPDATE `+securitySessionsTable):
		db.sessions[argString(v[0])] = argBytes(v[1])
		return fakePGResult{n: 1}, nil
	default:
		return nil, fmt.Errorf("fake-pg: unhandled exec %q", query)
	}
}

var fakePGDriverSeq atomic.Int64

// openFakePostgres builds a PostgresStore over the in-memory fake and
// runs migrations, like OpenPostgres minus the network.
func openFakePostgres(t *testing.T, maxItems int, retention time.Duration) *PostgresStore {
	t.Helper()
	return openFakePostgresOn(t, newFakePGDB(), maxItems, retention)
}

func openFakePostgresOn(t *testing.T, db *fakePGDB, maxItems int, retention time.Duration) *PostgresStore {
	t.Helper()
	name := fmt.Sprintf("wavec-fake-pg-%d", fakePGDriverSeq.Add(1))
	sql.Register(name, &fakePGDriver{db: db})
	raw, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	s := &PostgresStore{db: raw, maxAuditItems: maxItems, retention: retention}
	if err := s.migrate(context.Background()); err != nil {
		t.Fatalf("fake migrate: %v", err)
	}
	return s
}

// fakePGCorruptAudit rewrites one fake audit row's JSON in place.
func fakePGCorruptAudit(t *testing.T, db *fakePGDB, seq int64, mutate func(*AuditEvent)) {
	t.Helper()
	db.mu.Lock()
	defer db.mu.Unlock()
	raw, ok := db.audit[seq]
	if !ok {
		t.Fatalf("fake audit row %d missing", seq)
	}
	var event AuditEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	mutate(&event)
	enc, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	db.audit[seq] = enc
}
