package controlplane

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type scriptedSQL struct {
	mu          sync.Mutex
	rowValues   []driver.Value
	queryErr    error
	execErr     error
	commitErr   error
	affected    int64
	affectedErr error
	queries     []string
	execs       []string
	commits     int
	rollbacks   int
}

type scriptedDriver struct{ state *scriptedSQL }
type scriptedConn struct{ state *scriptedSQL }
type scriptedTx struct{ state *scriptedSQL }
type scriptedResult struct {
	affected int64
	err      error
}
type scriptedRows struct {
	columns []string
	values  []driver.Value
	read    bool
}

var scriptedDriverID atomic.Uint64

func openScriptedDB(t *testing.T, state *scriptedSQL) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("nexaroute-scripted-%d", scriptedDriverID.Add(1))
	sql.Register(name, scriptedDriver{state: state})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func (d scriptedDriver) Open(string) (driver.Conn, error) { return scriptedConn{state: d.state}, nil }
func (c scriptedConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("Prepare is not used")
}
func (c scriptedConn) Close() error              { return nil }
func (c scriptedConn) Begin() (driver.Tx, error) { return scriptedTx{state: c.state}, nil }
func (c scriptedConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return scriptedTx{state: c.state}, nil
}
func (c scriptedConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	c.state.queries = append(c.state.queries, query)
	if c.state.queryErr != nil {
		return nil, c.state.queryErr
	}
	columns := []string{"value"}
	switch {
	case strings.Contains(query, "SELECT revision, schema_version, payload"):
		columns = []string{"revision", "schema_version", "payload", "updated_at"}
	case strings.Contains(query, "SELECT revision, schema_version"):
		columns = []string{"revision", "schema_version"}
	case strings.Contains(query, "SELECT checksum"):
		columns = []string{"checksum"}
	}
	var values []driver.Value
	if c.state.rowValues != nil {
		values = append([]driver.Value(nil), c.state.rowValues...)
	}
	return &scriptedRows{columns: columns, values: values}, nil
}
func (c scriptedConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	c.state.execs = append(c.state.execs, query)
	if c.state.execErr != nil {
		return nil, c.state.execErr
	}
	return scriptedResult{affected: c.state.affected, err: c.state.affectedErr}, nil
}
func (tx scriptedTx) Commit() error {
	tx.state.mu.Lock()
	defer tx.state.mu.Unlock()
	tx.state.commits++
	return tx.state.commitErr
}
func (tx scriptedTx) Rollback() error {
	tx.state.mu.Lock()
	defer tx.state.mu.Unlock()
	tx.state.rollbacks++
	return nil
}
func (r scriptedResult) LastInsertId() (int64, error) { return 0, nil }
func (r scriptedResult) RowsAffected() (int64, error) { return r.affected, r.err }
func (r *scriptedRows) Columns() []string             { return r.columns }
func (r *scriptedRows) Close() error                  { return nil }
func (r *scriptedRows) Next(dest []driver.Value) error {
	if r.read || r.values == nil {
		return io.EOF
	}
	r.read = true
	copy(dest, r.values)
	return nil
}

func TestSQLStoreGetMapsRowsAndErrors(t *testing.T) {
	stamp := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	state := &scriptedSQL{rowValues: []driver.Value{int64(4), int64(2), []byte("payload"), stamp}}
	store, err := NewSQLStore(openScriptedDB(t, state))
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), "config")
	if err != nil || got.Revision != 4 || got.Schema != 2 || string(got.Payload) != "payload" || !got.UpdatedAt.Equal(stamp) {
		t.Fatalf("Get=%+v err=%v", got, err)
	}

	missing := &scriptedSQL{}
	store, _ = NewSQLStore(openScriptedDB(t, missing))
	if _, err = store.Get(context.Background(), "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Get err=%v", err)
	}

	queryFailure := &scriptedSQL{queryErr: errors.New("database offline")}
	store, _ = NewSQLStore(openScriptedDB(t, queryFailure))
	if _, err = store.Get(context.Background(), "config"); err == nil || !strings.Contains(err.Error(), "get control snapshot") {
		t.Fatalf("query failure not wrapped: %v", err)
	}
}

func TestSQLStorePutInsertUpdateConflictAndRollback(t *testing.T) {
	ctx := context.Background()
	insertState := &scriptedSQL{affected: 1}
	store, _ := NewSQLStore(openScriptedDB(t, insertState))
	created, err := store.Put(ctx, "config", 0, []byte("first"))
	if err != nil || created.Revision != 1 || created.Schema != 1 || string(created.Payload) != "first" {
		t.Fatalf("insert=%+v err=%v", created, err)
	}
	if len(insertState.execs) < 1 || !strings.Contains(insertState.execs[0], "INSERT INTO") || insertState.commits != 1 {
		t.Fatalf("insert transaction not committed: %+v", insertState)
	}

	updateState := &scriptedSQL{rowValues: []driver.Value{int64(3), int64(7)}, affected: 1}
	store, _ = NewSQLStore(openScriptedDB(t, updateState))
	updated, err := store.Put(ctx, "config", 3, []byte("next"))
	if err != nil || updated.Revision != 4 || updated.Schema != 7 || !strings.Contains(updateState.execs[0], "UPDATE") {
		t.Fatalf("update=%+v err=%v execs=%v", updated, err, updateState.execs)
	}

	staleState := &scriptedSQL{rowValues: []driver.Value{int64(3), int64(1)}}
	store, _ = NewSQLStore(openScriptedDB(t, staleState))
	if _, err = store.Put(ctx, "config", 2, []byte("stale")); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update err=%v", err)
	}
	if staleState.rollbacks != 1 || len(staleState.execs) != 0 {
		t.Fatalf("stale update wrote or failed to rollback: %+v", staleState)
	}

	if _, err = store.Put(ctx, "", 0, []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty namespace err=%v", err)
	}
	if _, err = store.Put(ctx, "config", 0, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty payload err=%v", err)
	}
}

func TestSQLStoreDeleteResultAndFailureCases(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state *scriptedSQL
		want  error
	}{
		{"deleted", &scriptedSQL{affected: 1}, nil},
		{"stale revision", &scriptedSQL{affected: 0}, ErrConflict},
		{"exec error", &scriptedSQL{execErr: errors.New("write failed")}, errors.New("delete control snapshot")},
		{"result error", &scriptedSQL{affectedErr: errors.New("rows affected unavailable")}, errors.New("control delete result")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := NewSQLStore(openScriptedDB(t, tc.state))
			if err != nil {
				t.Fatal(err)
			}
			err = store.Delete(context.Background(), "config", 3)
			if tc.want == nil {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || (errors.Is(tc.want, ErrConflict) && !errors.Is(err, ErrConflict)) || (!errors.Is(tc.want, ErrConflict) && !strings.Contains(err.Error(), tc.want.Error())) {
				t.Fatalf("Delete err=%v want=%v", err, tc.want)
			}
		})
	}
}

func TestApplyMigrationsRunsNewVersionAndPropagatesDDLFailure(t *testing.T) {
	registry, err := NewMigrationRegistry(Migration{Version: 1, Name: "initial", Up: "CREATE TABLE sample (id INT)", Down: "DROP TABLE sample"})
	if err != nil {
		t.Fatal(err)
	}
	state := &scriptedSQL{}
	if err = ApplyMigrations(context.Background(), openScriptedDB(t, state), registry); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(state.execs, "\n")
	for _, statement := range []string{"CREATE TABLE IF NOT EXISTS " + migrationTable, "pg_advisory_lock", "CREATE TABLE sample", "INSERT INTO " + migrationTable, "pg_advisory_unlock"} {
		if !strings.Contains(joined, statement) {
			t.Errorf("migration execution missing %q in %s", statement, joined)
		}
	}
	if state.commits != 1 {
		t.Fatalf("migration tx commits=%d", state.commits)
	}

	failed := &scriptedSQL{execErr: errors.New("permission denied")}
	if err = ApplyMigrations(context.Background(), openScriptedDB(t, failed), registry); err == nil || !strings.Contains(err.Error(), "create migration table") {
		t.Fatalf("DDL failure was not wrapped: %v", err)
	}
}
