package cost

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

type fakeCostRow struct {
	provider      string
	model         string
	currency      string
	estimated     float64
	upper         float64
	billable      float64
	priceKnown    bool
	state         reservationState
	actual        float64
	entryListed   bool
	createdSerial int
}

type fakeCostDB struct {
	mu         sync.Mutex
	rows       map[string]*fakeCostRow
	serial     int
	failBegin  bool
	failClose  bool
	failExec   string
	failQuery  string
	failCommit bool
	skipInsert bool
	badEntries bool
}

type fakeCostDriver struct{ db *fakeCostDB }

func (d *fakeCostDriver) Open(string) (driver.Conn, error) { return &fakeCostConn{db: d.db}, nil }

type fakeCostConn struct {
	db *fakeCostDB
	tx *fakeCostTx
}

func (c *fakeCostConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("fake cost: Prepare is not used")
}
func (c *fakeCostConn) Close() error {
	if c.db.failClose {
		return errors.New("fake cost: injected close failure")
	}
	return nil
}
func (c *fakeCostConn) Ping(context.Context) error { return nil }
func (c *fakeCostConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *fakeCostConn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.db.failBegin {
		return nil, errors.New("fake cost: injected begin failure")
	}
	c.db.mu.Lock()
	tx := &fakeCostTx{db: c.db, conn: c, snapshot: cloneCostRows(c.db.rows)}
	c.db.mu.Unlock()
	c.tx = tx
	return tx, nil
}
func (c *fakeCostConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	return c.db.exec(query, args)
}
func (c *fakeCostConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.db.mu.Lock()
	defer c.db.mu.Unlock()
	return c.db.query(query, args)
}

type fakeCostTx struct {
	db       *fakeCostDB
	conn     *fakeCostConn
	snapshot map[string]*fakeCostRow
	done     bool
}

func (t *fakeCostTx) Commit() error {
	if t.done {
		return nil
	}
	t.done = true
	t.conn.tx = nil
	if t.db.failCommit {
		t.db.mu.Lock()
		t.db.rows = cloneCostRows(t.snapshot)
		t.db.mu.Unlock()
		return errors.New("fake cost: injected commit failure")
	}
	return nil
}
func (t *fakeCostTx) Rollback() error {
	if !t.done {
		t.done = true
		t.conn.tx = nil
		t.db.mu.Lock()
		t.db.rows = cloneCostRows(t.snapshot)
		t.db.mu.Unlock()
	}
	return nil
}

type fakeCostResult struct{}

func (fakeCostResult) LastInsertId() (int64, error) { return 0, nil }
func (fakeCostResult) RowsAffected() (int64, error) { return 1, nil }

type fakeCostRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *fakeCostRows) Columns() []string { return r.columns }
func (r *fakeCostRows) Close() error      { return nil }
func (r *fakeCostRows) Next(dest []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

var fakeCostDriverID atomic.Uint64

func openFakeCostDB(t *testing.T, state *fakeCostDB) *sql.DB {
	t.Helper()
	if state.rows == nil {
		state.rows = map[string]*fakeCostRow{}
	}
	name := fmt.Sprintf("nexaroute-cost-fake-%d", fakeCostDriverID.Add(1))
	sql.Register(name, &fakeCostDriver{db: state})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func fakeCostLedger(t *testing.T, state *fakeCostDB) *PostgresLedger {
	t.Helper()
	return &PostgresLedger{db: openFakeCostDB(t, state)}
}

func cloneCostRows(in map[string]*fakeCostRow) map[string]*fakeCostRow {
	out := make(map[string]*fakeCostRow, len(in))
	for id, row := range in {
		copyRow := *row
		out[id] = &copyRow
	}
	return out
}

func costArgString(v driver.Value) string {
	switch value := v.(type) {
	case string:
		return value
	case []byte:
		return string(value)
	default:
		return fmt.Sprint(value)
	}
}

func costArgFloat(v driver.Value) float64 {
	switch value := v.(type) {
	case float64:
		return value
	case int64:
		return float64(value)
	case []byte:
		var f float64
		_, _ = fmt.Sscan(string(value), &f)
		return f
	case string:
		var f float64
		_, _ = fmt.Sscan(value, &f)
		return f
	default:
		return 0
	}
}

func (f *fakeCostDB) exec(query string, args []driver.NamedValue) (driver.Result, error) {
	if f.failExec != "" && strings.Contains(query, f.failExec) {
		return nil, errors.New("fake cost: injected exec failure")
	}
	switch {
	case strings.Contains(query, "pg_advisory_xact_lock"), strings.Contains(query, "CREATE TABLE IF NOT EXISTS"):
		return fakeCostResult{}, nil
	case strings.Contains(query, "INSERT INTO "+videoCostTable):
		if !f.skipInsert {
			id := costArgString(args[0].Value)
			if _, exists := f.rows[id]; !exists {
				f.serial++
				f.rows[id] = &fakeCostRow{
					provider:      costArgString(args[1].Value),
					model:         costArgString(args[2].Value),
					currency:      costArgString(args[3].Value),
					estimated:     costArgFloat(args[4].Value),
					upper:         costArgFloat(args[5].Value),
					billable:      costArgFloat(args[6].Value),
					priceKnown:    args[7].Value == true,
					state:         reservedState,
					entryListed:   strings.Contains(query, "'reserved', true)"),
					createdSerial: f.serial,
				}
			}
		}
		return fakeCostResult{}, nil
	case strings.Contains(query, "UPDATE "+videoCostTable+" SET state = 'released'"):
		if row := f.rows[costArgString(args[0].Value)]; row != nil && row.state == reservedState {
			row.state = releasedState
		}
		return fakeCostResult{}, nil
	case strings.Contains(query, "UPDATE "+videoCostTable+" SET state = $2"):
		if row := f.rows[costArgString(args[0].Value)]; row != nil {
			row.state = reservationState(costArgString(args[1].Value))
			row.estimated = costArgFloat(args[2].Value)
			row.actual = costArgFloat(args[3].Value)
		}
		return fakeCostResult{}, nil
	default:
		return nil, fmt.Errorf("fake cost: unsupported exec %q", query)
	}
}

func (f *fakeCostDB) query(query string, args []driver.NamedValue) (driver.Rows, error) {
	if f.failQuery != "" && strings.Contains(query, f.failQuery) {
		return nil, errors.New("fake cost: injected query failure")
	}
	switch {
	case strings.Contains(query, "COALESCE(SUM("):
		state := costArgString(args[0].Value)
		total := 0.0
		for _, row := range f.rows {
			if string(row.state) != state {
				continue
			}
			switch {
			case strings.Contains(query, "SUM(actual_usd)"):
				total += row.actual
			case strings.Contains(query, "SUM(estimated_usd)"):
				total += row.estimated
			case strings.Contains(query, "SUM(upper_bound_usd)"):
				total += row.upper
			}
		}
		return &fakeCostRows{
			columns: []string{"total"},
			values:  [][]driver.Value{{total}},
		}, nil
	case strings.Contains(query, "WHERE entry_listed ORDER BY"):
		listed := make([]*fakeCostRow, 0, len(f.rows))
		for _, row := range f.rows {
			if row.entryListed {
				listed = append(listed, row)
			}
		}
		sort.Slice(listed, func(i, j int) bool { return listed[i].createdSerial < listed[j].createdSerial })
		rows := make([][]driver.Value, 0, len(listed))
		for _, row := range listed {
			values := []driver.Value{row.provider, row.model, row.currency, row.estimated, row.upper, row.billable, row.priceKnown, string(row.state), row.actual, row.entryListed}
			if f.badEntries {
				values[4] = struct{}{}
			}
			rows = append(rows, values)
		}
		return &fakeCostRows{columns: costRowColumnsList(), values: rows}, nil
	case strings.Contains(query, "WHERE job_id = $1"):
		row := f.rows[costArgString(args[0].Value)]
		if row == nil {
			return &fakeCostRows{columns: costRowColumnsList()}, nil
		}
		return &fakeCostRows{
			columns: costRowColumnsList(),
			values: [][]driver.Value{{
				row.provider, row.model, row.currency, row.estimated, row.upper,
				row.billable, row.priceKnown, string(row.state), row.actual, row.entryListed,
			}},
		}, nil
	default:
		return nil, fmt.Errorf("fake cost: unsupported query %q", query)
	}
}

func costRowColumnsList() []string {
	return []string{"provider", "model", "currency", "estimated_usd", "upper_bound_usd", "billable_seconds", "price_known", "state", "actual_usd", "entry_listed"}
}

func TestPostgresLedgerFakeMigrateReserveReleaseAndEntries(t *testing.T) {
	ctx := context.Background()
	ledger := fakeCostLedger(t, &fakeCostDB{})
	if err := ledger.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	estimate := knownEstimate(2)
	if err := ledger.ReserveForJob("job-a", estimate, 0); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if got := ledger.Reserved(); got != 2 {
		t.Fatalf("reserved = %v, want 2", got)
	}
	if entries := ledger.Entries(); len(entries) != 1 || entries[0].EstimatedUSD != 2 {
		t.Fatalf("entries = %+v", entries)
	}

	if err := ledger.ReserveForJob("job-a", knownEstimate(9), 0); err != nil {
		t.Fatalf("duplicate reserve while reserved must be a no-op: %v", err)
	}
	if got := ledger.Reserved(); got != 2 {
		t.Fatalf("duplicate reserve changed reserved to %v", got)
	}
	if entries := ledger.Entries(); len(entries) != 1 || entries[0].EstimatedUSD != 2 {
		t.Fatalf("duplicate reserve changed entries: %+v", entries)
	}

	ledger.ReleaseForJob("job-a")
	if got := ledger.Reserved(); got != 0 {
		t.Fatalf("reserved after release = %v, want 0", got)
	}
	ledger.ReleaseForJob("missing")
	if err := ledger.ReserveForJob("job-a", estimate, 0); err == nil || !strings.Contains(err.Error(), "already been finalized") {
		t.Fatalf("reserve after release err = %v, want finalized error", err)
	}

	if err := ledger.ReserveForJob("budget", estimate, 1); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("over-budget reserve err = %v", err)
	}
	if err := ledger.ReserveForJob("", estimate, 0); err == nil {
		t.Fatal("empty job id was accepted")
	}
	if err := ledger.ReserveForJob("unknown", video.CostEstimate{}, 0); !errors.Is(err, ErrUnknownPrice) {
		t.Fatalf("unknown estimate err = %v", err)
	}

	entries := ledger.Entries()
	entries[0].EstimatedUSD = 999
	if got := ledger.Entries(); len(got) != 1 || got[0].EstimatedUSD != 2 {
		t.Fatalf("Entries result mutated durable state: %+v", got)
	}
}

func TestPostgresLedgerFakeSettlementStateMachine(t *testing.T) {
	ledger := fakeCostLedger(t, &fakeCostDB{})

	if err := ledger.SettleActual("actual", 3, 2); err != nil {
		t.Fatalf("initial actual settlement: %v", err)
	}
	if got := ledger.Spent(); got != 3 {
		t.Fatalf("spent = %v, want 3", got)
	}
	if err := ledger.SettleActual("actual", 3, 2); err != nil {
		t.Fatalf("idempotent actual settlement: %v", err)
	}
	if got := ledger.Spent(); got != 3 {
		t.Fatalf("spent after replay = %v, want 3", got)
	}
	if err := ledger.SettleActual("actual", 4, 2); err == nil || !strings.Contains(err.Error(), "conflicting actual-cost settlement") {
		t.Fatalf("conflicting actual err = %v", err)
	}

	if err := ledger.SettleEstimate("estimate", 1.25); err != nil {
		t.Fatalf("estimate settlement: %v", err)
	}
	if err := ledger.SettleEstimate("estimate", 99); err != nil {
		t.Fatalf("repeated estimate settlement: %v", err)
	}
	if got := ledger.EstimatedSpent(); got != 1.25 {
		t.Fatalf("estimated spent = %v, want 1.25", got)
	}
	if err := ledger.SettleActual("estimate", 2, 1.25); err != nil {
		t.Fatalf("estimate-to-actual upgrade: %v", err)
	}
	if got := ledger.EstimatedSpent(); got != 0 {
		t.Fatalf("estimated spent after upgrade = %v, want 0", got)
	}
	if got := ledger.Spent(); got != 5 {
		t.Fatalf("spent after upgrade = %v, want 5", got)
	}

	if err := ledger.ReserveForJob("released", knownEstimate(4), 0); err != nil {
		t.Fatal(err)
	}
	ledger.ReleaseForJob("released")
	if err := ledger.SettleEstimate("released", 4); err != nil {
		t.Fatalf("settle released job: %v", err)
	}
	if got := ledger.EstimatedSpent(); got != 4 {
		t.Fatalf("estimated spent after released estimate = %v, want 4", got)
	}

	if err := ledger.SettleActual("fallback", 4, 0.5); err != nil {
		t.Fatalf("fallback settlement: %v", err)
	}
	estimate, actual, delta, reconciled, ok := ledger.Reconciliation("fallback")
	if !ok || !reconciled || estimate != 0.5 || actual != 4 || delta != 3.5 {
		t.Fatalf("fallback reconciliation = (%v,%v,%v,%v,%v)", estimate, actual, delta, reconciled, ok)
	}
	if entries := ledger.Entries(); len(entries) != 1 || entries[0].EstimatedUSD != 4 {
		t.Fatalf("Entries included settlement placeholders: %+v", entries)
	}

	if err := ledger.ReserveForJob("zero-estimate", knownEstimate(0), 0); err != nil {
		t.Fatal(err)
	}
	if err := ledger.SettleActual("zero-estimate", 0.75, 2); err != nil {
		t.Fatalf("settle zero-estimate reservation: %v", err)
	}
	estimate, actual, delta, reconciled, ok = ledger.Reconciliation("zero-estimate")
	if !ok || !reconciled || estimate != 2 || actual != 0.75 || delta != -1.25 {
		t.Fatalf("zero-estimate reconciliation = (%v,%v,%v,%v,%v)", estimate, actual, delta, reconciled, ok)
	}

	if err := ledger.SettleActual("", 1, 1); !errors.Is(err, ErrInvalidEstimate) {
		t.Fatalf("empty settlement job err = %v", err)
	}
	if err := ledger.SettleEstimate("bad-estimate", -1); !errors.Is(err, ErrInvalidEstimate) {
		t.Fatalf("invalid estimate err = %v", err)
	}
}

func TestPostgresLedgerFakeReconciliationStates(t *testing.T) {
	state := &fakeCostDB{rows: map[string]*fakeCostRow{
		"reserved":  {state: reservedState, estimated: 2, upper: 2, priceKnown: true},
		"estimated": {state: estimateSettledState, estimated: 2, upper: 2, priceKnown: true},
		"actual":    {state: actualSettledState, estimated: 2, upper: 2, priceKnown: true, actual: 3},
	}}
	ledger := fakeCostLedger(t, state)

	estimate, actual, delta, reconciled, ok := ledger.Reconciliation("missing")
	if ok || reconciled || estimate != 0 || actual != 0 || delta != 0 {
		t.Fatalf("missing reconciliation = (%v,%v,%v,%v,%v)", estimate, actual, delta, reconciled, ok)
	}
	estimate, actual, delta, reconciled, ok = ledger.Reconciliation("reserved")
	if !ok || reconciled || estimate != 2 || actual != 0 || delta != 0 {
		t.Fatalf("reserved reconciliation = (%v,%v,%v,%v,%v)", estimate, actual, delta, reconciled, ok)
	}
	estimate, actual, delta, reconciled, ok = ledger.Reconciliation("estimated")
	if !ok || reconciled || estimate != 2 || actual != 0 || delta != 0 {
		t.Fatalf("estimate reconciliation = (%v,%v,%v,%v,%v)", estimate, actual, delta, reconciled, ok)
	}
	estimate, actual, delta, reconciled, ok = ledger.Reconciliation("actual")
	if !ok || !reconciled || estimate != 2 || actual != 3 || delta != 1 {
		t.Fatalf("actual reconciliation = (%v,%v,%v,%v,%v)", estimate, actual, delta, reconciled, ok)
	}

	invalidCases := []struct {
		name             string
		estimate, actual float64
		wantEst, wantAct float64
	}{
		{name: "NaN actual", estimate: 2, actual: math.NaN(), wantEst: 2, wantAct: 0},
		{name: "infinite actual", estimate: 2, actual: math.Inf(1), wantEst: 2, wantAct: 0},
		{name: "negative actual", estimate: 2, actual: -1, wantEst: 2, wantAct: 0},
		{name: "NaN estimate", estimate: math.NaN(), actual: 3, wantEst: 0, wantAct: 3},
		{name: "infinite estimate", estimate: math.Inf(1), actual: 3, wantEst: 0, wantAct: 3},
		{name: "negative estimate", estimate: -1, actual: 3, wantEst: 0, wantAct: 3},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			state.rows["invalid"] = &fakeCostRow{
				state: actualSettledState, estimated: tc.estimate, upper: 2,
				priceKnown: true, actual: tc.actual,
			}
			estimate, actual, delta, reconciled, ok := ledger.Reconciliation("invalid")
			if !ok || reconciled || estimate != tc.wantEst || actual != tc.wantAct || delta != 0 {
				t.Fatalf("invalid reconciliation = (%v,%v,%v,%v,%v), want (%v,%v,0,false,true)",
					estimate, actual, delta, reconciled, ok, tc.wantEst, tc.wantAct)
			}
		})
	}
}

func TestPostgresLedgerFakeFailurePaths(t *testing.T) {
	ctx := context.Background()
	estimate := knownEstimate(1)

	t.Run("migrate failures", func(t *testing.T) {
		cases := []struct {
			name  string
			state *fakeCostDB
			check string
		}{
			{name: "begin", state: &fakeCostDB{failBegin: true}, check: "begin migration"},
			{name: "lock", state: &fakeCostDB{failExec: "pg_advisory_xact_lock"}, check: "migration lock"},
			{name: "create", state: &fakeCostDB{failExec: "CREATE TABLE"}, check: "migration:"},
			{name: "commit", state: &fakeCostDB{failCommit: true}, check: "commit migration"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := fakeCostLedger(t, tc.state).migrate(ctx)
				if err == nil || !strings.Contains(err.Error(), tc.check) {
					t.Fatalf("migrate err = %v, want %q", err, tc.check)
				}
			})
		}
	})

	t.Run("reserve failures", func(t *testing.T) {
		cases := []struct {
			name  string
			state *fakeCostDB
			check string
		}{
			{name: "begin", state: &fakeCostDB{failBegin: true}, check: "begin reserve"},
			{name: "insert", state: &fakeCostDB{failExec: "INSERT INTO"}, check: "reserve job"},
			{name: "read", state: &fakeCostDB{failQuery: "WHERE job_id"}, check: "read reservation"},
			{name: "commit", state: &fakeCostDB{failCommit: true}, check: "commit reserve"},
			{name: "vanished", state: &fakeCostDB{skipInsert: true}, check: "vanished"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := fakeCostLedger(t, tc.state).ReserveForJob("job", estimate, 0)
				if err == nil || !strings.Contains(err.Error(), tc.check) {
					t.Fatalf("reserve err = %v, want %q", err, tc.check)
				}
			})
		}
	})

	t.Run("settlement failures", func(t *testing.T) {
		cases := []struct {
			name  string
			state *fakeCostDB
			check string
		}{
			{name: "begin", state: &fakeCostDB{failBegin: true}, check: "begin settlement"},
			{name: "placeholder", state: &fakeCostDB{failExec: "INSERT INTO"}, check: "settlement placeholder"},
			{name: "read", state: &fakeCostDB{failQuery: "WHERE job_id"}, check: "read settlement"},
			{name: "update", state: &fakeCostDB{failExec: "SET state = $2"}, check: "write settlement"},
			{name: "commit", state: &fakeCostDB{failCommit: true}, check: "commit settlement"},
			{name: "vanished", state: &fakeCostDB{skipInsert: true}, check: "vanished"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				err := fakeCostLedger(t, tc.state).SettleActual("job", 1, 1)
				if err == nil || !strings.Contains(err.Error(), tc.check) {
					t.Fatalf("settle err = %v, want %q", err, tc.check)
				}
			})
		}
	})

	t.Run("aggregate and list degradation", func(t *testing.T) {
		state := &fakeCostDB{failQuery: "nexaroute_video_cost_reservations"}
		ledger := fakeCostLedger(t, state)
		if got := ledger.Spent(); got != 0 {
			t.Fatalf("Spent on query failure = %v", got)
		}
		if got := ledger.EstimatedSpent(); got != 0 {
			t.Fatalf("EstimatedSpent on query failure = %v", got)
		}
		if got := ledger.Reserved(); got != 0 {
			t.Fatalf("Reserved on query failure = %v", got)
		}
		if entries := ledger.Entries(); entries != nil {
			t.Fatalf("Entries on query failure = %+v", entries)
		}
	})

	t.Run("entries scan failure", func(t *testing.T) {
		state := &fakeCostDB{
			rows: map[string]*fakeCostRow{
				"job": {provider: "p", model: "m", state: reservedState, entryListed: true, priceKnown: true},
			},
			badEntries: true,
		}
		if entries := fakeCostLedger(t, state).Entries(); entries != nil {
			t.Fatalf("Entries with a scan failure = %+v", entries)
		}
	})

	t.Run("release ignores store failure", func(t *testing.T) {
		state := &fakeCostDB{
			rows: map[string]*fakeCostRow{
				"job": {state: reservedState, estimated: 1, upper: 1, priceKnown: true},
			},
			failExec: "SET state = 'released'",
		}
		ledger := fakeCostLedger(t, state)
		ledger.ReleaseForJob("job")
		if got := ledger.Reserved(); got != 1 {
			t.Fatalf("failed release dropped reservation: reserved = %v", got)
		}
	})

	t.Run("reconciliation store failure", func(t *testing.T) {
		ledger := fakeCostLedger(t, &fakeCostDB{failQuery: "WHERE job_id"})
		if _, _, _, _, ok := ledger.Reconciliation("job"); ok {
			t.Fatal("Reconciliation on query failure reported ok")
		}
	})

	t.Run("close", func(t *testing.T) {
		ledger := fakeCostLedger(t, &fakeCostDB{})
		if err := ledger.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	})
}
