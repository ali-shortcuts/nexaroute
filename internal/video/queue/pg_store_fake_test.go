package queue

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

type fakeQueueRow struct {
	payload []byte
	idem    string
	owner   string
	until   time.Time
}

type fakeQueueDB struct {
	mu         sync.Mutex
	rows       map[string]*fakeQueueRow
	failBegin  bool
	failClose  bool
	failCommit bool
	failExec   string
	failQuery  string
	failProbe  bool
}

type fakeQueueDriver struct{ db *fakeQueueDB }

func (d *fakeQueueDriver) Open(string) (driver.Conn, error) { return &fakeQueueConn{db: d.db}, nil }

type fakeQueueConn struct {
	db *fakeQueueDB
	tx *fakeQueueTx
}

func (c *fakeQueueConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake queue: Prepare is not used")
}
func (c *fakeQueueConn) Close() error {
	if c.db.failClose {
		return errors.New("fake queue: injected close failure")
	}
	return nil
}
func (c *fakeQueueConn) Ping(context.Context) error { return nil }
func (c *fakeQueueConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *fakeQueueConn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.db.failBegin {
		return nil, errors.New("fake queue: injected begin failure")
	}
	c.db.mu.Lock()
	tx := &fakeQueueTx{db: c.db, conn: c, snapshot: cloneQueueRows(c.db.rows)}
	c.db.mu.Unlock()
	c.tx = tx
	return tx, nil
}
func (c *fakeQueueConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	return c.db.exec(query, args)
}
func (c *fakeQueueConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	return c.db.query(query, args)
}

type fakeQueueTx struct {
	db       *fakeQueueDB
	conn     *fakeQueueConn
	snapshot map[string]*fakeQueueRow
	done     bool
}

func (t *fakeQueueTx) Commit() error {
	if t.done {
		return nil
	}
	t.done = true
	t.conn.tx = nil
	if t.db.failCommit {
		t.db.mu.Lock()
		t.db.rows = cloneQueueRows(t.snapshot)
		t.db.mu.Unlock()
		return errors.New("fake queue: injected commit failure")
	}
	return nil
}
func (t *fakeQueueTx) Rollback() error {
	if !t.done {
		t.done = true
		t.conn.tx = nil
		t.db.mu.Lock()
		t.db.rows = cloneQueueRows(t.snapshot)
		t.db.mu.Unlock()
	}
	return nil
}

type fakeQueueResult struct{}

func (fakeQueueResult) LastInsertId() (int64, error) { return 0, nil }
func (fakeQueueResult) RowsAffected() (int64, error) { return 1, nil }

type fakeQueueRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *fakeQueueRows) Columns() []string { return r.columns }
func (r *fakeQueueRows) Close() error      { return nil }
func (r *fakeQueueRows) Next(dest []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

var fakeQueueDriverID atomic.Uint64

func openFakeQueueDB(t *testing.T, state *fakeQueueDB) *sql.DB {
	t.Helper()
	if state.rows == nil {
		state.rows = map[string]*fakeQueueRow{}
	}
	name := fmt.Sprintf("nexaroute-queue-fake-%d", fakeQueueDriverID.Add(1))
	sql.Register(name, &fakeQueueDriver{db: state})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func fakeQueueStore(t *testing.T, state *fakeQueueDB) (*PostgresStore, *fakeQueueDB) {
	t.Helper()
	return &PostgresStore{db: openFakeQueueDB(t, state)}, state
}

func cloneQueueRows(in map[string]*fakeQueueRow) map[string]*fakeQueueRow {
	out := make(map[string]*fakeQueueRow, len(in))
	for id, row := range in {
		copyRow := *row
		copyRow.payload = append([]byte(nil), row.payload...)
		out[id] = &copyRow
	}
	return out
}

func queueArgString(v driver.Value) string {
	switch value := v.(type) {
	case string:
		return value
	case []byte:
		return string(value)
	default:
		return fmt.Sprint(value)
	}
}

func queueArgBytes(v driver.Value) []byte {
	switch value := v.(type) {
	case []byte:
		return append([]byte(nil), value...)
	case string:
		return []byte(value)
	default:
		return []byte(fmt.Sprint(value))
	}
}

func queueArgTime(v driver.Value) time.Time {
	if v == nil {
		return time.Time{}
	}
	if stamp, ok := v.(time.Time); ok {
		return stamp
	}
	return time.Time{}
}

func queueUniqueViolation(constraint string) error {
	return &pgconn.PgError{Code: "23505", ConstraintName: constraint}
}

func (f *fakeQueueDB) exec(query string, args []driver.NamedValue) (driver.Result, error) {
	if f.failExec != "" && strings.Contains(query, f.failExec) {
		return nil, errors.New("fake queue: injected exec failure")
	}
	switch {
	case strings.Contains(query, "pg_advisory_xact_lock"), strings.Contains(query, "CREATE TABLE IF NOT EXISTS"), strings.Contains(query, "CREATE UNIQUE INDEX IF NOT EXISTS"):
		return fakeQueueResult{}, nil
	case strings.Contains(query, "INSERT INTO "+videoJobsTable):
		id := queueArgString(args[0].Value)
		idem := queueArgString(args[1].Value)
		if _, exists := f.rows[id]; exists {
			return nil, queueUniqueViolation("nexaroute_video_jobs_pkey")
		}
		if idem != "" {
			for otherID, row := range f.rows {
				if otherID != id && row.idem == idem {
					return nil, queueUniqueViolation(videoJobsIdemIdx)
				}
			}
		}
		f.rows[id] = &fakeQueueRow{
			payload: queueArgBytes(args[2].Value),
			idem:    idem,
			owner:   queueArgString(args[3].Value),
			until:   queueArgTime(args[4].Value),
		}
		return fakeQueueResult{}, nil
	case strings.Contains(query, "UPDATE "+videoJobsTable+" SET payload = $2"):
		id := queueArgString(args[0].Value)
		row := f.rows[id]
		if row == nil {
			return nil, fmt.Errorf("fake queue: update of missing row %s", id)
		}
		idem := queueArgString(args[2].Value)
		if idem != "" {
			for otherID, other := range f.rows {
				if otherID != id && other.idem == idem {
					return nil, queueUniqueViolation(videoJobsIdemIdx)
				}
			}
		}
		row.payload = queueArgBytes(args[1].Value)
		row.idem = idem
		row.owner = queueArgString(args[3].Value)
		row.until = queueArgTime(args[4].Value)
		return fakeQueueResult{}, nil
	case strings.Contains(query, "DELETE FROM "+videoJobsTable):
		delete(f.rows, queueArgString(args[0].Value))
		return fakeQueueResult{}, nil
	default:
		return nil, fmt.Errorf("fake queue: unsupported exec %q", query)
	}
}

func fakeQueueValues(row *fakeQueueRow) []driver.Value {
	var until driver.Value
	if !row.until.IsZero() {
		until = row.until
	}
	return []driver.Value{append([]byte(nil), row.payload...), row.idem, row.owner, until}
}

func fakeQueueColumns() []string {
	return []string{"payload", "idempotency_key", "lease_owner", "lease_until"}
}

func (f *fakeQueueDB) query(query string, args []driver.NamedValue) (driver.Rows, error) {
	if f.failQuery != "" && strings.Contains(query, f.failQuery) {
		return nil, errors.New("fake queue: injected query failure")
	}
	switch {
	case strings.Contains(query, "SELECT EXISTS(SELECT 1 FROM "+videoJobsTable):
		if f.failProbe {
			return nil, errors.New("fake queue: injected existence probe failure")
		}
		_, exists := f.rows[queueArgString(args[0].Value)]
		return &fakeQueueRows{columns: []string{"exists"}, values: [][]driver.Value{{exists}}}, nil
	case strings.Contains(query, "SET lease_owner = $2") && strings.Contains(query, "RETURNING"):
		id := queueArgString(args[0].Value)
		owner := queueArgString(args[1].Value)
		ttlSeconds := 0.0
		if seconds, ok := args[2].Value.(float64); ok {
			ttlSeconds = seconds
		}
		row := f.rows[id]
		if row == nil {
			return &fakeQueueRows{columns: fakeQueueColumns()}, nil
		}
		now := time.Now()
		if row.owner == "" || row.until.IsZero() || !row.until.After(now) || row.owner == owner {
			row.owner = owner
			row.until = now.Add(time.Duration(ttlSeconds * float64(time.Second)))
			return &fakeQueueRows{columns: fakeQueueColumns(), values: [][]driver.Value{fakeQueueValues(row)}}, nil
		}
		return &fakeQueueRows{columns: fakeQueueColumns()}, nil
	case strings.Contains(query, "WHERE job_id = $1"):
		row := f.rows[queueArgString(args[0].Value)]
		if row == nil {
			return &fakeQueueRows{columns: fakeQueueColumns()}, nil
		}
		return &fakeQueueRows{columns: fakeQueueColumns(), values: [][]driver.Value{fakeQueueValues(row)}}, nil
	case strings.Contains(query, "WHERE idempotency_key = $1"):
		key := queueArgString(args[0].Value)
		for _, row := range f.rows {
			if row.idem == key {
				return &fakeQueueRows{columns: fakeQueueColumns(), values: [][]driver.Value{fakeQueueValues(row)}}, nil
			}
		}
		return &fakeQueueRows{columns: fakeQueueColumns()}, nil
	case strings.Contains(query, "ORDER BY job_id"):
		ids := make([]string, 0, len(f.rows))
		for id := range f.rows {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		values := make([][]driver.Value, 0, len(ids))
		for _, id := range ids {
			values = append(values, fakeQueueValues(f.rows[id]))
		}
		return &fakeQueueRows{columns: fakeQueueColumns(), values: values}, nil
	default:
		return nil, fmt.Errorf("fake queue: unsupported query %q", query)
	}
}

func TestPostgresStoreFakeMigrateCRUDAndIdempotency(t *testing.T) {
	ctx := context.Background()
	store, _ := fakeQueueStore(t, &fakeQueueDB{})
	if err := store.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	first := queueJob("j1", "k1")
	if err := store.Create(ctx, first); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.Get(ctx, "j1")
	if err != nil || got.JobID != "j1" || got.IdempotencyKey != "k1" || !got.LeaseUntil.IsZero() {
		t.Fatalf("Get = %+v, err = %v", got, err)
	}
	byKey, err := store.GetByIdempotency(ctx, "k1")
	if err != nil || byKey.JobID != "j1" {
		t.Fatalf("GetByIdempotency = %+v, err = %v", byKey, err)
	}
	if err := store.Create(ctx, queueJob("j1", "other")); err == nil || err.Error() != "job already exists" {
		t.Fatalf("duplicate job id err = %v", err)
	}
	if err := store.Create(ctx, queueJob("j2", "k1")); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("duplicate idempotency err = %v", err)
	}
	for _, id := range []string{"j3", "j4"} {
		if err := store.Create(ctx, queueJob(id, "")); err != nil {
			t.Fatalf("create empty key %s: %v", id, err)
		}
	}
	if _, err := store.GetByIdempotency(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty key lookup err = %v", err)
	}
	if _, err := store.GetByIdempotency(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing key lookup err = %v", err)
	}
	if _, err := store.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing get err = %v", err)
	}

	first.State = video.StateProcessing
	if err := store.Update(ctx, first); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err = store.Get(ctx, "j1")
	if err != nil || got.State != video.StateProcessing {
		t.Fatalf("Get after update = %+v, err = %v", got, err)
	}
	if err := store.Update(ctx, queueJob("missing", "")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing err = %v", err)
	}

	list, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].JobID != "j1" || list[1].JobID != "j3" || list[2].JobID != "j4" {
		t.Fatalf("List order/content = %+v", list)
	}

	if err := store.Delete(ctx, "j1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, "j1"); err != nil {
		t.Fatalf("deleting a missing job should be a no-op: %v", err)
	}
	if _, err := store.Get(ctx, "j1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete err = %v", err)
	}
	if _, err := store.GetByIdempotency(ctx, "k1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("idempotency mapping survived delete: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestPostgresStoreFakeTerminalUpdatesAndLeases(t *testing.T) {
	ctx := context.Background()
	store, state := fakeQueueStore(t, &fakeQueueDB{})
	job := queueJob("j1", "")
	if err := store.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	done := job
	done.State = video.StateCompleted
	if err := store.Update(ctx, done); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if err := store.Update(ctx, done); err != nil {
		t.Fatalf("identical terminal replay: %v", err)
	}
	stale := done
	stale.State = video.StateProcessing
	if err := store.Update(ctx, stale); !errors.Is(err, ErrTerminalJob) {
		t.Fatalf("stale terminal update err = %v", err)
	}

	now := time.Now()
	claimed, ok, err := store.TryClaim(ctx, "j1", "owner-a", 5*time.Minute, now)
	if err != nil || !ok || claimed.LeaseOwner != "owner-a" || !claimed.LeaseUntil.After(now) {
		t.Fatalf("first claim = %+v, ok=%v err=%v", claimed, ok, err)
	}
	if _, ok, err := store.TryClaim(ctx, "j1", "owner-b", time.Minute, now); err != nil || ok {
		t.Fatalf("live foreign claim: ok=%v err=%v", ok, err)
	}
	renewed, ok, err := store.TryClaim(ctx, "j1", "owner-a", 10*time.Minute, now)
	if err != nil || !ok || !renewed.LeaseUntil.After(claimed.LeaseUntil) {
		t.Fatalf("same-owner renewal = %+v, ok=%v err=%v", renewed, ok, err)
	}

	authoritative, err := store.Get(ctx, "j1")
	if err != nil {
		t.Fatal(err)
	}
	if authoritative.LeaseOwner != "owner-a" || authoritative.LeaseUntil.IsZero() {
		t.Fatalf("lease columns not overlaid: %+v", authoritative)
	}
	if err := store.Update(ctx, authoritative); err != nil {
		t.Fatalf("terminal replay after claim: %v", err)
	}

	state.mu.Lock()
	state.rows["j1"].until = now.Add(-time.Minute)
	state.mu.Unlock()
	if _, ok, err := store.TryClaim(ctx, "j1", "owner-b", time.Minute, now); err != nil || !ok {
		t.Fatalf("expired lease claim: ok=%v err=%v", ok, err)
	}
	if _, _, err := store.TryClaim(ctx, "missing", "owner-a", time.Minute, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing claim err = %v", err)
	}
}

func TestPostgresStoreFakeFailurePaths(t *testing.T) {
	ctx := context.Background()

	t.Run("migration failures", func(t *testing.T) {
		cases := []struct {
			name  string
			state *fakeQueueDB
			check string
		}{
			{name: "begin", state: &fakeQueueDB{failBegin: true}, check: "begin migration"},
			{name: "lock", state: &fakeQueueDB{failExec: "pg_advisory_xact_lock"}, check: "migration lock"},
			{name: "table", state: &fakeQueueDB{failExec: "CREATE TABLE"}, check: "migration:"},
			{name: "index", state: &fakeQueueDB{failExec: "CREATE UNIQUE INDEX"}, check: "migration:"},
			{name: "commit", state: &fakeQueueDB{failCommit: true}, check: "commit migration"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				store, _ := fakeQueueStore(t, tc.state)
				err := store.migrate(ctx)
				if err == nil || !strings.Contains(err.Error(), tc.check) {
					t.Fatalf("migrate err = %v, want %q", err, tc.check)
				}
			})
		}
	})

	t.Run("create failures", func(t *testing.T) {
		t.Run("exec", func(t *testing.T) {
			store, _ := fakeQueueStore(t, &fakeQueueDB{failExec: "INSERT INTO"})
			err := store.Create(ctx, queueJob("j1", ""))
			if err == nil || !strings.Contains(err.Error(), "create:") {
				t.Fatalf("create err = %v", err)
			}
		})
		t.Run("conflict probe", func(t *testing.T) {
			payload, _ := json.Marshal(queueJob("j1", ""))
			state := &fakeQueueDB{
				rows:      map[string]*fakeQueueRow{"j1": {payload: payload}},
				failProbe: true,
			}
			store, _ := fakeQueueStore(t, state)
			err := store.Create(ctx, queueJob("j1", ""))
			if err == nil || !strings.Contains(err.Error(), "conflict probe") {
				t.Fatalf("create conflict probe err = %v", err)
			}
		})
	})

	t.Run("read failures", func(t *testing.T) {
		t.Run("get query", func(t *testing.T) {
			store, _ := fakeQueueStore(t, &fakeQueueDB{failQuery: "WHERE job_id"})
			if _, err := store.Get(ctx, "j1"); err == nil || !strings.Contains(err.Error(), "get:") {
				t.Fatalf("get err = %v", err)
			}
		})
		t.Run("get decode", func(t *testing.T) {
			state := &fakeQueueDB{rows: map[string]*fakeQueueRow{"j1": {payload: []byte("{")}}}
			store, _ := fakeQueueStore(t, state)
			if _, err := store.Get(ctx, "j1"); err == nil || !strings.Contains(err.Error(), "decode job") {
				t.Fatalf("get decode err = %v", err)
			}
		})
		t.Run("idempotency query", func(t *testing.T) {
			store, _ := fakeQueueStore(t, &fakeQueueDB{failQuery: "WHERE idempotency_key"})
			if _, err := store.GetByIdempotency(ctx, "k"); err == nil || !strings.Contains(err.Error(), "get by idempotency") {
				t.Fatalf("idempotency get err = %v", err)
			}
		})
		t.Run("idempotency decode", func(t *testing.T) {
			state := &fakeQueueDB{rows: map[string]*fakeQueueRow{"j1": {idem: "k", payload: []byte("{")}}}
			store, _ := fakeQueueStore(t, state)
			if _, err := store.GetByIdempotency(ctx, "k"); err == nil || !strings.Contains(err.Error(), "decode job") {
				t.Fatalf("idempotency decode err = %v", err)
			}
		})
	})

	t.Run("update failures", func(t *testing.T) {
		valid, _ := json.Marshal(queueJob("j1", ""))
		cases := []struct {
			name  string
			state *fakeQueueDB
			check string
		}{
			{name: "begin", state: &fakeQueueDB{failBegin: true}, check: "begin update"},
			{name: "lock query", state: &fakeQueueDB{failQuery: "FOR UPDATE"}, check: "update lock"},
			{name: "decode current", state: &fakeQueueDB{rows: map[string]*fakeQueueRow{"j1": {payload: []byte("{")}}}, check: "decode current job"},
			{name: "write", state: &fakeQueueDB{rows: map[string]*fakeQueueRow{"j1": {payload: valid}}, failExec: "SET payload = $2"}, check: "update:"},
			{name: "commit", state: &fakeQueueDB{rows: map[string]*fakeQueueRow{"j1": {payload: valid}}, failCommit: true}, check: "commit update"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				store, _ := fakeQueueStore(t, tc.state)
				err := store.Update(ctx, queueJob("j1", ""))
				if err == nil || !strings.Contains(err.Error(), tc.check) {
					t.Fatalf("update err = %v, want %q", err, tc.check)
				}
			})
		}
		t.Run("lock missing", func(t *testing.T) {
			store, _ := fakeQueueStore(t, &fakeQueueDB{})
			if err := store.Update(ctx, queueJob("missing", "")); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing update err = %v", err)
			}
		})
		t.Run("idempotency conflict", func(t *testing.T) {
			state := &fakeQueueDB{}
			store, _ := fakeQueueStore(t, state)
			if err := store.Create(ctx, queueJob("j1", "k1")); err != nil {
				t.Fatal(err)
			}
			if err := store.Create(ctx, queueJob("j2", "k2")); err != nil {
				t.Fatal(err)
			}
			conflict := queueJob("j2", "k1")
			if err := store.Update(ctx, conflict); !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("update idempotency conflict err = %v", err)
			}
		})
		t.Run("commit rollback", func(t *testing.T) {
			state := &fakeQueueDB{}
			store, _ := fakeQueueStore(t, state)
			job := queueJob("j1", "")
			if err := store.Create(ctx, job); err != nil {
				t.Fatal(err)
			}
			state.failCommit = true
			changed := job
			changed.State = video.StateProcessing
			if err := store.Update(ctx, changed); err == nil {
				t.Fatal("commit failure was not reported")
			}
			state.failCommit = false
			got, err := store.Get(ctx, "j1")
			if err != nil || got.State != job.State {
				t.Fatalf("failed commit was not rolled back: %+v, err=%v", got, err)
			}
		})
	})

	t.Run("delete failure", func(t *testing.T) {
		store, _ := fakeQueueStore(t, &fakeQueueDB{failExec: "DELETE FROM"})
		if err := store.Delete(ctx, "j1"); err == nil || !strings.Contains(err.Error(), "delete:") {
			t.Fatalf("delete err = %v", err)
		}
	})

	t.Run("list failures", func(t *testing.T) {
		t.Run("query", func(t *testing.T) {
			store, _ := fakeQueueStore(t, &fakeQueueDB{failQuery: "ORDER BY job_id"})
			if _, err := store.List(ctx); err == nil || !strings.Contains(err.Error(), "list:") {
				t.Fatalf("list err = %v", err)
			}
		})
		t.Run("decode", func(t *testing.T) {
			state := &fakeQueueDB{rows: map[string]*fakeQueueRow{"j1": {payload: []byte("{")}}}
			store, _ := fakeQueueStore(t, state)
			if _, err := store.List(ctx); err == nil || !strings.Contains(err.Error(), "decode job") {
				t.Fatalf("list decode err = %v", err)
			}
		})
	})

	t.Run("claim failures", func(t *testing.T) {
		t.Run("query", func(t *testing.T) {
			store, _ := fakeQueueStore(t, &fakeQueueDB{failQuery: "RETURNING"})
			if _, _, err := store.TryClaim(ctx, "j1", "owner", time.Minute, time.Now()); err == nil || !strings.Contains(err.Error(), "claim:") {
				t.Fatalf("claim query err = %v", err)
			}
		})
		t.Run("existence probe", func(t *testing.T) {
			live := &fakeQueueRow{owner: "owner-a", until: time.Now().Add(time.Hour)}
			state := &fakeQueueDB{rows: map[string]*fakeQueueRow{"j1": live}, failProbe: true}
			store, _ := fakeQueueStore(t, state)
			if _, _, err := store.TryClaim(ctx, "j1", "owner-b", time.Minute, time.Now()); err == nil || !strings.Contains(err.Error(), "existence check") {
				t.Fatalf("claim probe err = %v", err)
			}
		})
		t.Run("decode", func(t *testing.T) {
			state := &fakeQueueDB{rows: map[string]*fakeQueueRow{"j1": {payload: []byte("{")}}}
			store, _ := fakeQueueStore(t, state)
			if _, _, err := store.TryClaim(ctx, "j1", "owner", time.Minute, time.Now()); err == nil || !strings.Contains(err.Error(), "decode claimed job") {
				t.Fatalf("claim decode err = %v", err)
			}
		})
	})

	t.Run("close failure", func(t *testing.T) {
		store, _ := fakeQueueStore(t, &fakeQueueDB{failClose: true})
		if err := store.db.PingContext(ctx); err != nil {
			t.Fatalf("ping: %v", err)
		}
		if err := store.Close(); err == nil || !strings.Contains(err.Error(), "injected close") {
			t.Fatalf("close err = %v", err)
		}
	})

	t.Run("null time", func(t *testing.T) {
		if got := nullTime(time.Time{}); got.Valid {
			t.Fatalf("zero time = %+v, want NULL", got)
		}
		stamp := time.Now()
		if got := nullTime(stamp); !got.Valid || !got.Time.Equal(stamp) {
			t.Fatalf("non-zero time = %+v", got)
		}
	})
}
