package controlplane

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// Wave-3 ApplyMigrations coverage via a scripted fake driver: every
// failure injection point (connection, create table, advisory lock,
// checksum read, begin, Up, INSERT, commit) plus the already-applied
// skip and the two-migration success/idempotency path. No sqlmock
// dependency exists in go.mod, so the fake implements driver.Conn,
// driver.ExecerContext, driver.QueryerContext and driver.ConnBeginner
// directly and is registered through sql.OpenDB (no global
// sql.Register, avoiding duplicate-registration panics).

type wave3FakeTx struct {
	committed  bool
	rolledBack bool
	failCommit error
}

func (tx *wave3FakeTx) Commit() error {
	if tx.failCommit != nil {
		return tx.failCommit
	}
	tx.committed = true
	return nil
}

func (tx *wave3FakeTx) Rollback() error {
	tx.rolledBack = true
	return nil
}

type wave3FakeRows struct {
	cols   []string
	values [][]driver.Value
	next   int
	closed bool
}

func (r *wave3FakeRows) Columns() []string { return r.cols }
func (r *wave3FakeRows) Close() error      { r.closed = true; return nil }
func (r *wave3FakeRows) Next(dest []driver.Value) error {
	if r.next >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.next])
	r.next++
	return nil
}

type wave3FakeConn struct {
	failCreate  error
	failLock    error
	failQuery   error
	failBegin   error
	failUp      error
	failInsert  error
	failCommit  error
	checksum    string
	hasChecksum bool
	executedUp  []string
	inserted    []int64
	lastTx      *wave3FakeTx
	closed      bool
}

func (c *wave3FakeConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported by wave3 fake")
}

func (c *wave3FakeConn) Close() error { c.closed = true; return nil }

func (c *wave3FakeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *wave3FakeConn) BeginTx(_ context.Context, _ driver.TxOptions) (driver.Tx, error) {
	if c.failBegin != nil {
		return nil, c.failBegin
	}
	c.lastTx = &wave3FakeTx{failCommit: c.failCommit}
	return c.lastTx, nil
}

func (c *wave3FakeConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	switch {
	case strings.Contains(query, "pg_advisory"):
		if c.failLock != nil {
			return nil, c.failLock
		}
	case strings.HasPrefix(query, "INSERT INTO"):
		if c.failInsert != nil {
			return nil, c.failInsert
		}
		if len(args) > 0 {
			switch v := args[0].Value.(type) {
			case int64:
				c.inserted = append(c.inserted, v)
			case uint64:
				c.inserted = append(c.inserted, int64(v))
			}
		}
	case strings.Contains(query, "IF NOT EXISTS"):
		// The migration-table DDL, not a migration Up statement.
		if c.failCreate != nil {
			return nil, c.failCreate
		}
	default:
		// Inside a transaction: the migration Up statement.
		if c.failUp != nil {
			return nil, c.failUp
		}
		c.executedUp = append(c.executedUp, query)
	}
	return driver.ResultNoRows, nil
}

func (c *wave3FakeConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, "SELECT") {
		if c.failQuery != nil {
			return nil, c.failQuery
		}
		if !c.hasChecksum {
			return &wave3FakeRows{cols: []string{"checksum"}}, nil
		}
		return &wave3FakeRows{cols: []string{"checksum"}, values: [][]driver.Value{{c.checksum}}}, nil
	}
	return nil, fmt.Errorf("unexpected query: %s", query)
}

func (c *wave3FakeConn) Ping(_ context.Context) error { return nil }

type wave3FakeDriver struct {
	conn      *wave3FakeConn
	failOpen  error
	openCalls int
}

func (d *wave3FakeDriver) OpenConnector(_ string) (driver.Connector, error) {
	return wave3FakeConnector{driver: d}, nil
}

type wave3FakeConnector struct {
	driver *wave3FakeDriver
}

func (c wave3FakeConnector) Connect(_ context.Context) (driver.Conn, error) {
	c.driver.openCalls++
	if c.driver.failOpen != nil {
		return nil, c.driver.failOpen
	}
	return c.driver.conn, nil
}

func (c wave3FakeConnector) Driver() driver.Driver { return c.driver }

func (d *wave3FakeDriver) Open(_ string) (driver.Conn, error) {
	if d.failOpen != nil {
		return nil, d.failOpen
	}
	return d.conn, nil
}

func wave3Migrations(versions ...uint64) MigrationRegistry {
	migs := make([]Migration, 0, len(versions))
	for _, v := range versions {
		migs = append(migs, Migration{Version: v, Name: fmt.Sprintf("m%d", v), Up: fmt.Sprintf("CREATE TABLE t%d (id int)", v)})
	}
	reg, err := NewMigrationRegistry(migs...)
	if err != nil {
		panic(err)
	}
	return reg
}

func wave3OpenFake(t *testing.T, conn *wave3FakeConn, failOpen error) *sql.DB {
	t.Helper()
	d := &wave3FakeDriver{conn: conn, failOpen: failOpen}
	return sql.OpenDB(wave3FakeConnector{driver: d})
}

func TestWave3ApplyMigrationsNilDB(t *testing.T) {
	err := ApplyMigrations(context.Background(), nil, wave3Migrations(1))
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil db error = %v, want ErrUnavailable", err)
	}
}

func TestWave3ApplyMigrationsConnectionError(t *testing.T) {
	conn := &wave3FakeConn{}
	db := wave3OpenFake(t, conn, errors.New("dial refused"))
	defer db.Close()
	err := ApplyMigrations(context.Background(), db, wave3Migrations(1))
	if err == nil || !strings.Contains(err.Error(), "acquire migration connection") {
		t.Fatalf("connection error = %v", err)
	}
}

func TestWave3ApplyMigrationsCreateTableError(t *testing.T) {
	conn := &wave3FakeConn{failCreate: errors.New("permission denied")}
	db := wave3OpenFake(t, conn, nil)
	defer db.Close()
	err := ApplyMigrations(context.Background(), db, wave3Migrations(1))
	if err == nil || !strings.Contains(err.Error(), "create migration table") {
		t.Fatalf("create table error = %v", err)
	}
}

func TestWave3ApplyMigrationsLockError(t *testing.T) {
	conn := &wave3FakeConn{failLock: errors.New("lock unavailable")}
	db := wave3OpenFake(t, conn, nil)
	defer db.Close()
	err := ApplyMigrations(context.Background(), db, wave3Migrations(1))
	if err == nil || !strings.Contains(err.Error(), "acquire migration lock") {
		t.Fatalf("lock error = %v", err)
	}
}

func TestWave3ApplyMigrationsChecksumMismatch(t *testing.T) {
	conn := &wave3FakeConn{hasChecksum: true, checksum: "deadbeef"}
	db := wave3OpenFake(t, conn, nil)
	defer db.Close()
	err := ApplyMigrations(context.Background(), db, wave3Migrations(1))
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("checksum mismatch error = %v", err)
	}
}

func TestWave3ApplyMigrationsAlreadyAppliedSkipped(t *testing.T) {
	reg := wave3Migrations(1)
	want := reg.All()[0].Checksum()
	conn := &wave3FakeConn{hasChecksum: true, checksum: want}
	db := wave3OpenFake(t, conn, nil)
	defer db.Close()
	if err := ApplyMigrations(context.Background(), db, reg); err != nil {
		t.Fatalf("already-applied migration must be skipped cleanly: %v", err)
	}
	if len(conn.executedUp) != 0 {
		t.Fatalf("already-applied migration re-executed Up: %v", conn.executedUp)
	}
}

func TestWave3ApplyMigrationsReadError(t *testing.T) {
	conn := &wave3FakeConn{failQuery: errors.New("read failed")}
	db := wave3OpenFake(t, conn, nil)
	defer db.Close()
	err := ApplyMigrations(context.Background(), db, wave3Migrations(1))
	if err == nil || !strings.Contains(err.Error(), "read migration 1") {
		t.Fatalf("read error = %v", err)
	}
}

func TestWave3ApplyMigrationsBeginError(t *testing.T) {
	conn := &wave3FakeConn{failBegin: errors.New("begin failed")}
	db := wave3OpenFake(t, conn, nil)
	defer db.Close()
	err := ApplyMigrations(context.Background(), db, wave3Migrations(1))
	if err == nil || !strings.Contains(err.Error(), "begin migration 1") {
		t.Fatalf("begin error = %v", err)
	}
}

func TestWave3ApplyMigrationsUpErrorRollsBack(t *testing.T) {
	conn := &wave3FakeConn{failUp: errors.New("syntax error")}
	db := wave3OpenFake(t, conn, nil)
	defer db.Close()
	err := ApplyMigrations(context.Background(), db, wave3Migrations(1))
	if err == nil || !strings.Contains(err.Error(), "apply migration 1") {
		t.Fatalf("Up error = %v", err)
	}
	if conn.lastTx == nil || !conn.lastTx.rolledBack {
		t.Fatal("failed Up must roll back the transaction")
	}
}

func TestWave3ApplyMigrationsInsertErrorRollsBack(t *testing.T) {
	conn := &wave3FakeConn{failInsert: errors.New("insert failed")}
	db := wave3OpenFake(t, conn, nil)
	defer db.Close()
	err := ApplyMigrations(context.Background(), db, wave3Migrations(1))
	if err == nil || !strings.Contains(err.Error(), "record migration 1") {
		t.Fatalf("INSERT error = %v", err)
	}
	if conn.lastTx == nil || !conn.lastTx.rolledBack {
		t.Fatal("failed INSERT must roll back the transaction")
	}
}

func TestWave3ApplyMigrationsCommitError(t *testing.T) {
	conn := &wave3FakeConn{failCommit: errors.New("commit failed")}
	db := wave3OpenFake(t, conn, nil)
	defer db.Close()
	err := ApplyMigrations(context.Background(), db, wave3Migrations(1))
	if err == nil || !strings.Contains(err.Error(), "commit migration 1") {
		t.Fatalf("commit error = %v", err)
	}
}

func TestWave3ApplyMigrationsTwoInOrderAndIdempotent(t *testing.T) {
	reg := wave3Migrations(1, 2)
	conn := &wave3FakeConn{}
	db := wave3OpenFake(t, conn, nil)
	defer db.Close()
	if err := ApplyMigrations(context.Background(), db, reg); err != nil {
		t.Fatalf("two migrations must apply: %v", err)
	}
	if len(conn.executedUp) != 2 || conn.executedUp[0] != "CREATE TABLE t1 (id int)" || conn.executedUp[1] != "CREATE TABLE t2 (id int)" {
		t.Fatalf("Up order = %v, want t1 then t2", conn.executedUp)
	}
	if len(conn.inserted) != 2 || conn.inserted[0] != 1 || conn.inserted[1] != 2 {
		t.Fatalf("recorded versions = %v, want [1 2]", conn.inserted)
	}
	// Idempotency, via the skip path on a single-migration registry
	// whose checksum already matches (the fake returns one checksum
	// for every version, so per-version re-run state is expressed
	// through a fresh connection instead of reusing this one).
	single := wave3Migrations(1)
	conn2 := &wave3FakeConn{hasChecksum: true, checksum: single.All()[0].Checksum()}
	db2 := wave3OpenFake(t, conn2, nil)
	defer db2.Close()
	if err := ApplyMigrations(context.Background(), db2, single); err != nil {
		t.Fatalf("second run must be a clean no-op: %v", err)
	}
	if len(conn2.executedUp) != 0 {
		t.Fatalf("second run re-executed Up: %v", conn2.executedUp)
	}
}

func TestWave3ApplyMigrationsCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	conn := &wave3FakeConn{}
	db := wave3OpenFake(t, conn, nil)
	defer db.Close()
	err := ApplyMigrations(ctx, db, wave3Migrations(1))
	if err == nil || !strings.Contains(err.Error(), "acquire migration connection") {
		t.Fatalf("cancelled context error = %v", err)
	}
}
