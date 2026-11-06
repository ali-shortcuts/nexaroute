package nexaflow

// Fake database/sql driver for the PostgresStore, mirroring the
// securitystore fake: an in-memory emulation of the five workflow
// tables (definitions, revisions, runs, nodes, approvals) plus the
// delivery-claims set, with per-query-shape canned rows and injectable
// query/exec/commit/scan failures. Transactions are copy-on-write
// snapshots: Begin deep-copies the tables, Commit swaps the copy in,
// Rollback discards it. Tests are sequential, so no locking is needed.
//
// The fake is deliberately NOT a SQL engine: each production query
// shape is dispatched on a distinctive substring and answered from the
// tables. If production SQL changes shape, the dispatch below must be
// updated with it (an unmatched query fails loudly).

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
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

var agentBPGDriverSeq atomic.Uint64

// agentBPGRevision is one row of nexaroute_workflow_revisions.
type agentBPGRevision struct {
	revision int64
	hash     string
	status   string
	by       string
	def      []byte
	at       time.Time
}

// agentBPGRun is one row of nexaroute_workflow_runs.
type agentBPGRun struct {
	id         string
	defID      string
	revision   int64
	def        []byte
	state      string
	idem       string
	principal  string
	roles      []byte
	input      []byte
	errCode    string
	errMsg     string
	createdAt  time.Time
	updatedAt  time.Time
	startedAt  *time.Time
	finishedAt *time.Time
	leaseOwner string
	leaseUntil *time.Time
}

// agentBPGNode is one row of nexaroute_workflow_nodes.
type agentBPGNode struct {
	runID      string
	nodeID     string
	attempt    int64
	payload    []byte
	state      string
	leaseOwner string
	leaseUntil *time.Time
}

// agentBPGApproval is one row of nexaroute_workflow_approvals.
type agentBPGApproval struct {
	runID     string
	nodeID    string
	requester string
	approver  string
	decision  string
	at        time.Time
}

// agentBPGTables is the whole fake database. A transaction holds a
// deep copy; Commit swaps it in.
type agentBPGTables struct {
	defs       map[string][]byte
	revs       map[string]map[int64]*agentBPGRevision
	runs       map[string]*agentBPGRun
	nodes      map[string]map[string]map[int64]*agentBPGNode
	approvals  map[string]map[string]*agentBPGApproval
	deliveries map[string]bool
}

func newAgentBPGTables() *agentBPGTables {
	return &agentBPGTables{
		defs:       map[string][]byte{},
		revs:       map[string]map[int64]*agentBPGRevision{},
		runs:       map[string]*agentBPGRun{},
		nodes:      map[string]map[string]map[int64]*agentBPGNode{},
		approvals:  map[string]map[string]*agentBPGApproval{},
		deliveries: map[string]bool{},
	}
}

func (t *agentBPGTables) clone() *agentBPGTables {
	out := newAgentBPGTables()
	for k, v := range t.defs {
		out.defs[k] = append([]byte(nil), v...)
	}
	for defID, revs := range t.revs {
		cp := map[int64]*agentBPGRevision{}
		for rev, r := range revs {
			nr := *r
			nr.def = append([]byte(nil), r.def...)
			cp[rev] = &nr
		}
		out.revs[defID] = cp
	}
	for id, r := range t.runs {
		nr := *r
		nr.def = append([]byte(nil), r.def...)
		nr.roles = append([]byte(nil), r.roles...)
		nr.input = append([]byte(nil), r.input...)
		if r.startedAt != nil {
			v := *r.startedAt
			nr.startedAt = &v
		}
		if r.finishedAt != nil {
			v := *r.finishedAt
			nr.finishedAt = &v
		}
		if r.leaseUntil != nil {
			v := *r.leaseUntil
			nr.leaseUntil = &v
		}
		out.runs[id] = &nr
	}
	for runID, byNode := range t.nodes {
		cpNode := map[string]map[int64]*agentBPGNode{}
		for nodeID, byAttempt := range byNode {
			cpAttempt := map[int64]*agentBPGNode{}
			for att, n := range byAttempt {
				nn := *n
				nn.payload = append([]byte(nil), n.payload...)
				if n.leaseUntil != nil {
					v := *n.leaseUntil
					nn.leaseUntil = &v
				}
				cpAttempt[att] = &nn
			}
			cpNode[nodeID] = cpAttempt
		}
		out.nodes[runID] = cpNode
	}
	for runID, byNode := range t.approvals {
		cp := map[string]*agentBPGApproval{}
		for nodeID, a := range byNode {
			na := *a
			cp[nodeID] = &na
		}
		out.approvals[runID] = cp
	}
	for k, v := range t.deliveries {
		out.deliveries[k] = v
	}
	return out
}

// agentBPGFakeDB is the shared fake database plus injection hooks.
type agentBPGFakeDB struct {
	tables *agentBPGTables

	failBegin        bool
	failCommit       bool
	failPing         bool
	failQuery        string // QueryContext fails when the query contains this
	failExec         string // ExecContext fails when the query contains this
	failRowsAffected bool
	// rowsErrQuery makes the first Query matching it return rows whose
	// second Next fails, so database/sql surfaces it via rows.Err().
	rowsErrQuery string
	rowsErr      error
	// badRevType makes the revision column of the named run/revision
	// scan as the string "bad", so rows.Scan into *int64 fails.
	badRunRevision map[string]bool
	badRevRevision map[string]bool
	badNodeAttempt map[string]bool // key runID + "\x00" + nodeID + "\x00" + attempt
	// badListDraftsCols / badPruneCols make the matching query return
	// a second column, so the one-destination Scan fails on the
	// column count (a []byte destination accepts every driver value,
	// so no value can fail it).
	badListDraftsCols bool
	badPruneCols      bool
	// closeErrQuery makes the first Query matching it return rows
	// whose Close fails, so database/sql surfaces it via Rows.Close.
	closeErrQuery string
	closeErr      error
}

func newAgentBPGFakeDB() *agentBPGFakeDB {
	return &agentBPGFakeDB{
		tables:         newAgentBPGTables(),
		badRunRevision: map[string]bool{},
		badRevRevision: map[string]bool{},
		badNodeAttempt: map[string]bool{},
	}
}

// --- seeding helpers (write corrupt or pre-built rows directly) ---

func (db *agentBPGFakeDB) seedDraft(defID string, raw []byte) {
	db.tables.defs[defID] = append([]byte(nil), raw...)
}

func (db *agentBPGFakeDB) seedRevision(defID string, rev int64, hash, status, by string, def []byte, at time.Time) {
	if db.tables.revs[defID] == nil {
		db.tables.revs[defID] = map[int64]*agentBPGRevision{}
	}
	db.tables.revs[defID][rev] = &agentBPGRevision{revision: rev, hash: hash, status: status, by: by, def: append([]byte(nil), def...), at: at}
}

func (db *agentBPGFakeDB) seedRun(r *agentBPGRun) {
	db.tables.runs[r.id] = r
}

func (db *agentBPGFakeDB) seedNode(n *agentBPGNode) {
	if db.tables.nodes[n.runID] == nil {
		db.tables.nodes[n.runID] = map[string]map[int64]*agentBPGNode{}
	}
	if db.tables.nodes[n.runID][n.nodeID] == nil {
		db.tables.nodes[n.runID][n.nodeID] = map[int64]*agentBPGNode{}
	}
	db.tables.nodes[n.runID][n.nodeID][n.attempt] = n
}

// --- driver plumbing ---

type agentBPGFakeDriver struct{ db *agentBPGFakeDB }

func (d *agentBPGFakeDriver) Open(name string) (driver.Conn, error) {
	return &agentBPGFakeConn{db: d.db}, nil
}

type agentBPGFakeConn struct {
	db *agentBPGFakeDB
	tx *agentBPGTables // non-nil inside a transaction (copy-on-write)
}

func (c *agentBPGFakeConn) tables() *agentBPGTables {
	if c.tx != nil {
		return c.tx
	}
	return c.db.tables
}

func (c *agentBPGFakeConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("agentb-pg-fake: Prepare not supported; use QueryerContext/ExecerContext")
}

func (c *agentBPGFakeConn) Close() error { return nil }

func (c *agentBPGFakeConn) Begin() (driver.Tx, error) {
	if c.db.failBegin {
		return nil, errors.New("agentb-pg-fake: injected begin failure")
	}
	if c.tx != nil {
		return nil, errors.New("agentb-pg-fake: nested transaction")
	}
	c.tx = c.db.tables.clone()
	return &agentBPGFakeTx{conn: c}, nil
}

func (c *agentBPGFakeConn) Ping(ctx context.Context) error {
	if c.db.failPing {
		return errors.New("agentb-pg-fake: injected ping failure")
	}
	return nil
}

// CheckNamedValue converts []string arguments (used for ANY($1)
// parameters) into a NUL-joined driver.Value; everything else falls
// through to database/sql's default conversion.
func (c *agentBPGFakeConn) CheckNamedValue(nv *driver.NamedValue) error {
	if ss, ok := nv.Value.([]string); ok {
		nv.Value = strings.Join(ss, "\x00")
		return nil
	}
	return driver.ErrSkip
}

type agentBPGFakeTx struct{ conn *agentBPGFakeConn }

func (t *agentBPGFakeTx) Commit() error {
	if t.conn.db.failCommit {
		t.conn.tx = nil
		return errors.New("agentb-pg-fake: injected commit failure")
	}
	t.conn.db.tables = t.conn.tx
	t.conn.tx = nil
	return nil
}

func (t *agentBPGFakeTx) Rollback() error {
	t.conn.tx = nil
	return nil
}

type agentBPGFakeRows struct {
	cols      []string
	rows      [][]driver.Value
	pos       int
	failAfter int // >= 0: Next returns failErr once pos reaches it
	failErr   error
	closeErr  error
}

func (r *agentBPGFakeRows) Columns() []string { return r.cols }
func (r *agentBPGFakeRows) Close() error      { return r.closeErr }
func (r *agentBPGFakeRows) Next(dest []driver.Value) error {
	if r.failErr != nil && r.pos == r.failAfter {
		return r.failErr
	}
	if r.pos >= len(r.rows) {
		return io.EOF
	}
	copy(dest, r.rows[r.pos])
	r.pos++
	return nil
}

type agentBPGFakeResult struct {
	n   int64
	err error
}

func (r agentBPGFakeResult) LastInsertId() (int64, error) {
	return 0, errors.New("agentb-pg-fake: LastInsertId not supported")
}

func (r agentBPGFakeResult) RowsAffected() (int64, error) {
	if r.err != nil {
		return 0, r.err
	}
	return r.n, nil
}

func (db *agentBPGFakeDB) result(n int64) driver.Result {
	if db.failRowsAffected {
		return agentBPGFakeResult{err: errors.New("agentb-pg-fake: injected RowsAffected failure")}
	}
	return agentBPGFakeResult{n: n}
}

// --- argument helpers ---

func agentBArgString(v driver.Value) string {
	s, _ := v.(string)
	return s
}

func agentBArgInt64(v driver.Value) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

func agentBArgBytes(v driver.Value) []byte {
	switch b := v.(type) {
	case []byte:
		return append([]byte(nil), b...)
	case string:
		return []byte(b)
	default:
		return nil
	}
}

func agentBArgTime(v driver.Value) time.Time {
	t, _ := v.(time.Time)
	return t
}

func agentBArgBool(v driver.Value) bool {
	b, _ := v.(bool)
	return b
}

func agentBArgTimePtr(v driver.Value) *time.Time {
	if v == nil {
		return nil
	}
	t, _ := v.(time.Time)
	return &t
}

// nullDriverValue converts a *time.Time to a driver.Value (nil = NULL).
func agentBNullTime(t *time.Time) driver.Value {
	if t == nil {
		return nil
	}
	return *t
}

// agentBOpenFakePG registers the fake driver under a unique name and
// returns a *sql.DB over it.
func agentBOpenFakePG(t *testing.T, db *agentBPGFakeDB) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("agentb-fake-pg-nexaflow-%d", agentBPGDriverSeq.Add(1))
	sql.Register(name, &agentBPGFakeDriver{db: db})
	d, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("open fake pg: %v", err)
	}
	return d
}

// --- row builders (column order must match the production Scan) ---

func agentBRunRow(db *agentBPGFakeDB, r *agentBPGRun) []driver.Value {
	var rev driver.Value = r.revision
	if db.badRunRevision[r.id] {
		rev = "bad"
	}
	return []driver.Value{
		r.id, r.defID, rev, append([]byte(nil), r.def...), r.state, r.idem, r.principal,
		append([]byte(nil), r.roles...), append([]byte(nil), r.input...),
		r.errCode, r.errMsg, r.createdAt, r.updatedAt,
		agentBNullTime(r.startedAt), agentBNullTime(r.finishedAt),
		r.leaseOwner, agentBNullTime(r.leaseUntil),
	}
}

func agentBNodeRow(db *agentBPGFakeDB, n *agentBPGNode) []driver.Value {
	var att driver.Value = n.attempt
	key := n.runID + "\x00" + n.nodeID + "\x00" + strconv.FormatInt(n.attempt, 10)
	if db.badNodeAttempt[key] {
		att = "bad"
	}
	return []driver.Value{n.runID, n.nodeID, att, append([]byte(nil), n.payload...), n.state, n.leaseOwner, agentBNullTime(n.leaseUntil)}
}

func agentBRevRow(db *agentBPGFakeDB, defID string, r *agentBPGRevision) []driver.Value {
	var rev driver.Value = r.revision
	if db.badRevRevision[defID] {
		rev = "bad"
	}
	return []driver.Value{rev, r.hash, append([]byte(nil), r.def...), r.status, r.by, r.at}
}

func agentBArgs(args []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, len(args))
	for i, nv := range args {
		out[i] = nv.Value
	}
	return out
}

func agentBPlaceholder(query, marker string) int {
	i := strings.Index(query, marker)
	if i < 0 {
		return 0
	}
	rest := query[i+len(marker):]
	n := 0
	for _, ch := range rest {
		if ch < '0' || ch > '9' {
			break
		}
		n = n*10 + int(ch-'0')
	}
	return n
}

func (c *agentBPGFakeConn) applyRowsErr(query string, rows *agentBPGFakeRows) {
	if c.db.rowsErrQuery != "" && strings.Contains(query, c.db.rowsErrQuery) {
		err := c.db.rowsErr
		if err == nil {
			err = errors.New("agentb-pg-fake: injected rows error")
		}
		rows.failAfter = 1
		rows.failErr = err
	}
	if c.db.closeErrQuery != "" && strings.Contains(query, c.db.closeErrQuery) {
		err := c.db.closeErr
		if err == nil {
			err = errors.New("agentb-pg-fake: injected rows close failure")
		}
		rows.closeErr = err
	}
}

func (c *agentBPGFakeConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.db.failQuery != "" && strings.Contains(query, c.db.failQuery) {
		return nil, errors.New("agentb-pg-fake: injected query failure")
	}
	v := agentBArgs(args)
	t := c.tables()

	// Drafts.
	if strings.HasPrefix(query, `SELECT draft FROM `+wfDefsTable+` WHERE definition_id = $1`) {
		raw, ok := t.defs[agentBArgString(v[0])]
		if !ok {
			return &agentBPGFakeRows{cols: []string{"draft"}}, nil
		}
		return &agentBPGFakeRows{cols: []string{"draft"}, rows: [][]driver.Value{{append([]byte(nil), raw...)}}}, nil
	}
	if strings.HasPrefix(query, `SELECT draft FROM `+wfDefsTable+` ORDER BY definition_id`) {
		ids := make([]string, 0, len(t.defs))
		for id := range t.defs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		rows := &agentBPGFakeRows{cols: []string{"draft"}}
		for _, id := range ids {
			rows.rows = append(rows.rows, []driver.Value{append([]byte(nil), t.defs[id]...)})
		}
		if c.db.badListDraftsCols {
			// database/sql rejects Scan when the column count
			// differs from the destination count; the adapter scans
			// one []byte per row, so a second column fails it.
			rows.cols = []string{"draft", "extra"}
			for i := range rows.rows {
				rows.rows[i] = append(rows.rows[i], int64(0))
			}
		}
		c.applyRowsErr(query, rows)
		return rows, nil
	}

	// Revisions. (The EXISTS checks come before GetRevision: the
	// EXISTS query contains GetRevision's WHERE substring.)
	if strings.Contains(query, `SELECT EXISTS(SELECT 1 FROM `+wfRevsTable) {
		_, ok := t.revs[agentBArgString(v[0])][agentBArgInt64(v[1])]
		return &agentBPGFakeRows{cols: []string{"exists"}, rows: [][]driver.Value{{ok}}}, nil
	}
	if strings.Contains(query, `SELECT EXISTS(SELECT 1 FROM `+wfRunsTable) {
		_, ok := t.runs[agentBArgString(v[0])]
		return &agentBPGFakeRows{cols: []string{"exists"}, rows: [][]driver.Value{{ok}}}, nil
	}
	if strings.Contains(query, `FROM `+wfRevsTable+` WHERE definition_id = $1 ORDER BY revision DESC LIMIT 1`) {
		revs := t.revs[agentBArgString(v[0])]
		if len(revs) == 0 {
			return &agentBPGFakeRows{cols: []string{"revision", "content_hash", "definition", "status", "published_by", "published_at"}}, nil
		}
		var last int64 = -1
		for rev := range revs {
			if rev > last {
				last = rev
			}
		}
		return &agentBPGFakeRows{
			cols: []string{"revision", "content_hash", "definition", "status", "published_by", "published_at"},
			rows: [][]driver.Value{agentBRevRow(c.db, agentBArgString(v[0]), revs[last])},
		}, nil
	}
	if strings.Contains(query, `FROM `+wfRevsTable+` WHERE definition_id = $1 AND revision = $2`) {
		rev, ok := t.revs[agentBArgString(v[0])][agentBArgInt64(v[1])]
		if !ok {
			return &agentBPGFakeRows{cols: []string{"content_hash", "definition", "status", "published_by", "published_at"}}, nil
		}
		r := agentBRevRow(c.db, agentBArgString(v[0]), rev)
		return &agentBPGFakeRows{
			cols: []string{"content_hash", "definition", "status", "published_by", "published_at"},
			rows: [][]driver.Value{{r[1], r[2], r[3], r[4], r[5]}},
		}, nil
	}
	if strings.Contains(query, `FROM `+wfRevsTable+` WHERE definition_id = $1 ORDER BY revision`) {
		revs := t.revs[agentBArgString(v[0])]
		nums := make([]int64, 0, len(revs))
		for rev := range revs {
			nums = append(nums, rev)
		}
		sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
		rows := &agentBPGFakeRows{cols: []string{"revision", "content_hash", "definition", "status", "published_by", "published_at"}}
		for _, rev := range nums {
			rows.rows = append(rows.rows, agentBRevRow(c.db, agentBArgString(v[0]), revs[rev]))
		}
		c.applyRowsErr(query, rows)
		return rows, nil
	}

	// Runs. (The run-lock query comes first: it scans one column,
	// while the generic run query below returns the full 17.)
	if strings.Contains(query, `SELECT run_id FROM `+wfRunsTable+` WHERE run_id = $1 FOR UPDATE`) {
		if _, ok := t.runs[agentBArgString(v[0])]; !ok {
			return &agentBPGFakeRows{cols: []string{"run_id"}}, nil
		}
		return &agentBPGFakeRows{cols: []string{"run_id"}, rows: [][]driver.Value{{agentBArgString(v[0])}}}, nil
	}
	if strings.Contains(query, `SELECT attempt FROM `+wfNodesTable) {
		// Moved above the node-row branches: TryClaim's resolve
		// query contains the fallback query's WHERE substring, but
		// scans one column, not the full node row.
		byNode := t.nodes[agentBArgString(v[0])][agentBArgString(v[1])]
		var max int64 = -1
		for att := range byNode {
			if att > max {
				max = att
			}
		}
		if max < 0 {
			return &agentBPGFakeRows{cols: []string{"attempt"}}, nil
		}
		return &agentBPGFakeRows{cols: []string{"attempt"}, rows: [][]driver.Value{{max}}}, nil
	}
	if strings.Contains(query, `SELECT COUNT(*) FROM `+wfRunsTable) {
		defID := agentBArgString(v[0])
		var n int64
		for _, r := range t.runs {
			if r.defID == defID && r.state != "succeeded" && r.state != "failed" && r.state != "cancelled" {
				n++
			}
		}
		return &agentBPGFakeRows{cols: []string{"count"}, rows: [][]driver.Value{{n}}}, nil
	}
	if strings.Contains(query, `FROM `+wfRunsTable+` WHERE run_id = $1`) {
		r, ok := t.runs[agentBArgString(v[0])]
		if !ok {
			return &agentBPGFakeRows{cols: strings.Split(runColumns, ", ")}, nil
		}
		return &agentBPGFakeRows{cols: strings.Split(runColumns, ", "), rows: [][]driver.Value{agentBRunRow(c.db, r)}}, nil
	}
	if strings.Contains(query, `FROM `+wfRunsTable+` WHERE idempotency_key = $1`) {
		for _, r := range t.runs {
			if r.idem == agentBArgString(v[0]) {
				return &agentBPGFakeRows{cols: strings.Split(runColumns, ", "), rows: [][]driver.Value{agentBRunRow(c.db, r)}}, nil
			}
		}
		return &agentBPGFakeRows{cols: strings.Split(runColumns, ", ")}, nil
	}
	if strings.Contains(query, `FROM `+wfRunsTable+` WHERE 1=1`) {
		var defID, state string
		var limit int64 = -1
		if n := agentBPlaceholder(query, "AND definition_id = $"); n > 0 {
			defID = agentBArgString(v[n-1])
		}
		if n := agentBPlaceholder(query, "AND state = $"); n > 0 {
			state = agentBArgString(v[n-1])
		}
		if n := agentBPlaceholder(query, "LIMIT $"); n > 0 {
			limit = agentBArgInt64(v[n-1])
		}
		var all []*agentBPGRun
		for _, r := range t.runs {
			if defID != "" && r.defID != defID {
				continue
			}
			if state != "" && r.state != state {
				continue
			}
			all = append(all, r)
		}
		sort.Slice(all, func(i, j int) bool {
			if !all[i].createdAt.Equal(all[j].createdAt) {
				return all[i].createdAt.Before(all[j].createdAt)
			}
			return all[i].id < all[j].id
		})
		rows := &agentBPGFakeRows{cols: strings.Split(runColumns, ", ")}
		for _, r := range all {
			if limit >= 0 && int64(len(rows.rows)) >= limit {
				break
			}
			rows.rows = append(rows.rows, agentBRunRow(c.db, r))
		}
		c.applyRowsErr(query, rows)
		return rows, nil
	}
	if strings.Contains(query, `SELECT run_id FROM `+wfRunsTable+` WHERE state = ANY($1)`) {
		terminal := map[string]bool{}
		for _, s := range strings.Split(agentBArgString(v[0]), "\x00") {
			terminal[s] = true
		}
		before := agentBArgTime(v[1])
		var ids []string
		for _, r := range t.runs {
			if terminal[r.state] && r.finishedAt != nil && r.finishedAt.Before(before) {
				ids = append(ids, r.id)
			}
		}
		sort.Strings(ids)
		rows := &agentBPGFakeRows{cols: []string{"run_id"}}
		for _, id := range ids {
			rows.rows = append(rows.rows, []driver.Value{id})
		}
		if c.db.badPruneCols {
			rows.cols = []string{"run_id", "extra"}
			for i := range rows.rows {
				rows.rows[i] = append(rows.rows[i], int64(0))
			}
		}
		c.applyRowsErr(query, rows)
		return rows, nil
	}

	// Nodes.
	if strings.Contains(query, `FROM `+wfNodesTable+` WHERE run_id=$1 AND node_id=$2 AND payload->>'append_operation_id'=$3 LIMIT 1`) {
		byNode := t.nodes[agentBArgString(v[0])][agentBArgString(v[1])]
		var atts []int64
		for att := range byNode {
			atts = append(atts, att)
		}
		sort.Slice(atts, func(i, j int) bool { return atts[i] < atts[j] })
		for _, att := range atts {
			var payload map[string]any
			if err := json.Unmarshal(byNode[att].payload, &payload); err == nil {
				if op, _ := payload["append_operation_id"].(string); op == agentBArgString(v[2]) {
					return &agentBPGFakeRows{
						cols: []string{"run_id", "node_id", "attempt", "payload", "state", "lease_owner", "lease_until"},
						rows: [][]driver.Value{agentBNodeRow(c.db, byNode[att])},
					}, nil
				}
			}
		}
		return &agentBPGFakeRows{cols: []string{"run_id", "node_id", "attempt", "payload", "state", "lease_owner", "lease_until"}}, nil
	}
	if strings.Contains(query, `SELECT MAX(attempt) FROM `+wfNodesTable) {
		byNode := t.nodes[agentBArgString(v[0])][agentBArgString(v[1])]
		var max int64 = -1
		for att := range byNode {
			if att > max {
				max = att
			}
		}
		var val driver.Value
		if max >= 0 {
			val = max
		}
		return &agentBPGFakeRows{cols: []string{"max"}, rows: [][]driver.Value{{val}}}, nil
	}
	if strings.Contains(query, `FROM `+wfNodesTable+` WHERE run_id = $1 AND node_id = $2 AND attempt = $3 FOR UPDATE`) {
		n, ok := t.nodes[agentBArgString(v[0])][agentBArgString(v[1])][agentBArgInt64(v[2])]
		if !ok {
			return &agentBPGFakeRows{cols: []string{"run_id", "node_id", "attempt", "payload", "state", "lease_owner", "lease_until"}}, nil
		}
		return &agentBPGFakeRows{
			cols: []string{"run_id", "node_id", "attempt", "payload", "state", "lease_owner", "lease_until"},
			rows: [][]driver.Value{agentBNodeRow(c.db, n)},
		}, nil
	}
	if strings.Contains(query, `FROM `+wfNodesTable+` WHERE run_id = $1 AND node_id = $2 ORDER BY attempt DESC LIMIT 1 FOR UPDATE`) {
		byNode := t.nodes[agentBArgString(v[0])][agentBArgString(v[1])]
		var max int64 = -1
		for att := range byNode {
			if att > max {
				max = att
			}
		}
		if max < 0 {
			return &agentBPGFakeRows{cols: []string{"run_id", "node_id", "attempt", "payload", "state", "lease_owner", "lease_until"}}, nil
		}
		return &agentBPGFakeRows{
			cols: []string{"run_id", "node_id", "attempt", "payload", "state", "lease_owner", "lease_until"},
			rows: [][]driver.Value{agentBNodeRow(c.db, byNode[max])},
		}, nil
	}
	if strings.Contains(query, `FROM `+wfNodesTable+` WHERE run_id = $1 ORDER BY node_id, attempt`) {
		byNode := t.nodes[agentBArgString(v[0])]
		var nodeIDs []string
		for nodeID := range byNode {
			nodeIDs = append(nodeIDs, nodeID)
		}
		sort.Strings(nodeIDs)
		rows := &agentBPGFakeRows{cols: []string{"run_id", "node_id", "attempt", "payload", "state", "lease_owner", "lease_until"}}
		for _, nodeID := range nodeIDs {
			var atts []int64
			for att := range byNode[nodeID] {
				atts = append(atts, att)
			}
			sort.Slice(atts, func(i, j int) bool { return atts[i] < atts[j] })
			for _, att := range atts {
				rows.rows = append(rows.rows, agentBNodeRow(c.db, byNode[nodeID][att]))
			}
		}
		c.applyRowsErr(query, rows)
		return rows, nil
	}
	if strings.Contains(query, `RETURNING payload, state, lease_owner, lease_until`) {
		// TryClaim conditional update: claim the current attempt when
		// the state is claimable and no live foreign lease blocks.
		n := t.nodes[agentBArgString(v[0])][agentBArgString(v[1])][agentBArgInt64(v[2])]
		if n == nil {
			return &agentBPGFakeRows{cols: []string{"payload", "state", "lease_owner", "lease_until"}}, nil
		}
		owner := agentBArgString(v[3])
		ttl := v[4].(float64)
		now := time.Now()
		claimable := n.state == "pending" || n.state == "claimed"
		leaseFree := n.leaseOwner == "" || n.leaseUntil == nil || !n.leaseUntil.After(now) || n.leaseOwner == owner
		if !claimable || !leaseFree {
			return &agentBPGFakeRows{cols: []string{"payload", "state", "lease_owner", "lease_until"}}, nil
		}
		until := now.Add(time.Duration(ttl * float64(time.Second)))
		n.state = "claimed"
		n.leaseOwner = owner
		n.leaseUntil = &until
		return &agentBPGFakeRows{
			cols: []string{"payload", "state", "lease_owner", "lease_until"},
			rows: [][]driver.Value{{append([]byte(nil), n.payload...), "claimed", owner, until}},
		}, nil
	}

	// Approvals.
	if strings.Contains(query, `FROM `+wfApprovalsTable+` WHERE run_id = $1 AND node_id = $2 FOR UPDATE`) {
		a, ok := t.approvals[agentBArgString(v[0])][agentBArgString(v[1])]
		if !ok {
			return &agentBPGFakeRows{cols: []string{"decision", "approver", "requester", "decided_at"}}, nil
		}
		return &agentBPGFakeRows{
			cols: []string{"decision", "approver", "requester", "decided_at"},
			rows: [][]driver.Value{{a.decision, a.approver, a.requester, a.at}},
		}, nil
	}
	if strings.Contains(query, `FROM `+wfApprovalsTable+` WHERE run_id = $1 AND node_id = $2`) {
		a, ok := t.approvals[agentBArgString(v[0])][agentBArgString(v[1])]
		if !ok {
			return &agentBPGFakeRows{cols: []string{"run_id", "node_id", "requester", "approver", "decision", "decided_at"}}, nil
		}
		return &agentBPGFakeRows{
			cols: []string{"run_id", "node_id", "requester", "approver", "decision", "decided_at"},
			rows: [][]driver.Value{{a.runID, a.nodeID, a.requester, a.approver, a.decision, a.at}},
		}, nil
	}

	return nil, fmt.Errorf("agentb-pg-fake: unmatched query: %.120s", query)
}

func (c *agentBPGFakeConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if c.db.failExec != "" && strings.Contains(query, c.db.failExec) {
		return nil, errors.New("agentb-pg-fake: injected exec failure")
	}
	v := agentBArgs(args)
	t := c.tables()

	// Migration + delivery schema: no-ops.
	if strings.HasPrefix(query, `SELECT pg_advisory_xact_lock`) ||
		strings.HasPrefix(query, `CREATE TABLE IF NOT EXISTS`) ||
		strings.HasPrefix(query, `ALTER TABLE`) ||
		strings.HasPrefix(query, `CREATE UNIQUE INDEX IF NOT EXISTS`) {
		return c.db.result(0), nil
	}

	// Drafts.
	if strings.HasPrefix(query, `INSERT INTO `+wfDefsTable) {
		t.defs[agentBArgString(v[0])] = agentBArgBytes(v[1])
		return c.db.result(1), nil
	}

	// Revisions.
	if strings.HasPrefix(query, `UPDATE `+wfRevsTable+` SET status = $1 WHERE definition_id = $2 AND status = $3`) {
		var n int64
		for _, r := range t.revs[agentBArgString(v[1])] {
			if r.status == agentBArgString(v[2]) {
				r.status = agentBArgString(v[0])
				n++
			}
		}
		return c.db.result(n), nil
	}
	if strings.HasPrefix(query, `INSERT INTO `+wfRevsTable) {
		defID := agentBArgString(v[0])
		rev := agentBArgInt64(v[1])
		if t.revs[defID] == nil {
			t.revs[defID] = map[int64]*agentBPGRevision{}
		}
		if _, exists := t.revs[defID][rev]; exists {
			return nil, &pgconn.PgError{Severity: "ERROR", Code: "23505", Message: "duplicate key value violates unique constraint"}
		}
		t.revs[defID][rev] = &agentBPGRevision{
			revision: rev, hash: agentBArgString(v[2]), def: agentBArgBytes(v[3]),
			status: agentBArgString(v[4]), by: agentBArgString(v[5]), at: agentBArgTime(v[6]),
		}
		return c.db.result(1), nil
	}
	if strings.HasPrefix(query, `UPDATE `+wfRevsTable+` SET status = $1 WHERE definition_id = $2 AND revision = $3 AND status = $4`) {
		r, ok := t.revs[agentBArgString(v[1])][agentBArgInt64(v[2])]
		if !ok || r.status != agentBArgString(v[3]) {
			return c.db.result(0), nil
		}
		r.status = agentBArgString(v[0])
		return c.db.result(1), nil
	}

	// Runs.
	if strings.HasPrefix(query, `INSERT INTO `+wfRunsTable) {
		id := agentBArgString(v[0])
		idem := agentBArgString(v[5])
		if _, exists := t.runs[id]; exists {
			return nil, &pgconn.PgError{Severity: "ERROR", Code: "23505", Message: "duplicate key value violates unique constraint", ConstraintName: "nexaroute_workflow_runs_pkey"}
		}
		if idem != "" {
			for _, r := range t.runs {
				if r.idem == idem {
					return nil, &pgconn.PgError{Severity: "ERROR", Code: "23505", Message: "duplicate key value violates unique constraint", ConstraintName: wfRunsIdemIdx}
				}
			}
		}
		t.runs[id] = &agentBPGRun{
			id: id, defID: agentBArgString(v[1]), revision: agentBArgInt64(v[2]),
			def: agentBArgBytes(v[3]), state: agentBArgString(v[4]), idem: idem,
			principal: agentBArgString(v[6]), roles: agentBArgBytes(v[7]), input: agentBArgBytes(v[8]),
			errCode: agentBArgString(v[9]), errMsg: agentBArgString(v[10]),
			createdAt: agentBArgTime(v[11]), updatedAt: agentBArgTime(v[12]),
			startedAt: agentBArgTimePtr(v[13]), finishedAt: agentBArgTimePtr(v[14]),
			leaseOwner: agentBArgString(v[15]), leaseUntil: agentBArgTimePtr(v[16]),
		}
		return c.db.result(1), nil
	}
	if strings.HasPrefix(query, `UPDATE `+wfRunsTable+` SET state = $2`) {
		r, ok := t.runs[agentBArgString(v[0])]
		if !ok {
			return c.db.result(0), nil
		}
		r.state = agentBArgString(v[1])
		r.errCode = agentBArgString(v[2])
		r.errMsg = agentBArgString(v[3])
		r.updatedAt = agentBArgTime(v[4])
		r.startedAt = agentBArgTimePtr(v[5])
		r.finishedAt = agentBArgTimePtr(v[6])
		return c.db.result(1), nil
	}
	if strings.HasPrefix(query, `DELETE FROM `+wfNodesTable+` WHERE run_id = ANY($1)`) {
		var n int64
		for _, id := range strings.Split(agentBArgString(v[0]), "\x00") {
			if id == "" {
				continue
			}
			if byNode, ok := t.nodes[id]; ok {
				for _, byAtt := range byNode {
					n += int64(len(byAtt))
				}
				delete(t.nodes, id)
			}
		}
		return c.db.result(n), nil
	}
	if strings.HasPrefix(query, `DELETE FROM `+wfApprovalsTable+` WHERE run_id = ANY($1)`) {
		var n int64
		for _, id := range strings.Split(agentBArgString(v[0]), "\x00") {
			if id == "" {
				continue
			}
			if byNode, ok := t.approvals[id]; ok {
				n += int64(len(byNode))
				delete(t.approvals, id)
			}
		}
		return c.db.result(n), nil
	}
	if strings.HasPrefix(query, `DELETE FROM `+wfRunsTable+` WHERE run_id = ANY($1)`) {
		var n int64
		for _, id := range strings.Split(agentBArgString(v[0]), "\x00") {
			if id == "" {
				continue
			}
			if _, ok := t.runs[id]; ok {
				delete(t.runs, id)
				n++
			}
		}
		return c.db.result(n), nil
	}

	// Nodes.
	if strings.HasPrefix(query, `INSERT INTO `+wfNodesTable) {
		runID, nodeID := agentBArgString(v[0]), agentBArgString(v[1])
		att := agentBArgInt64(v[2])
		if t.nodes[runID] == nil {
			t.nodes[runID] = map[string]map[int64]*agentBPGNode{}
		}
		if t.nodes[runID][nodeID] == nil {
			t.nodes[runID][nodeID] = map[int64]*agentBPGNode{}
		}
		if _, exists := t.nodes[runID][nodeID][att]; exists {
			return nil, &pgconn.PgError{Severity: "ERROR", Code: "23505", Message: "duplicate key value violates unique constraint"}
		}
		t.nodes[runID][nodeID][att] = &agentBPGNode{
			runID: runID, nodeID: nodeID, attempt: att, payload: agentBArgBytes(v[3]),
			state: agentBArgString(v[4]), leaseOwner: agentBArgString(v[5]), leaseUntil: agentBArgTimePtr(v[6]),
		}
		return c.db.result(1), nil
	}
	if strings.HasPrefix(query, `UPDATE `+wfNodesTable+` SET payload = $4`) {
		n, ok := t.nodes[agentBArgString(v[0])][agentBArgString(v[1])][agentBArgInt64(v[2])]
		if !ok {
			return c.db.result(0), nil
		}
		n.payload = agentBArgBytes(v[3])
		n.state = agentBArgString(v[4])
		n.leaseOwner = agentBArgString(v[5])
		n.leaseUntil = agentBArgTimePtr(v[6])
		return c.db.result(1), nil
	}
	if strings.HasPrefix(query, `UPDATE `+wfNodesTable+` SET lease_owner = '', lease_until = NULL`) {
		byNode := t.nodes[agentBArgString(v[0])][agentBArgString(v[1])]
		var max int64 = -1
		for att := range byNode {
			if att > max {
				max = att
			}
		}
		if max < 0 {
			return c.db.result(0), nil
		}
		n := byNode[max]
		if n.leaseOwner != agentBArgString(v[2]) {
			return c.db.result(0), nil
		}
		n.leaseOwner = ""
		n.leaseUntil = nil
		return c.db.result(1), nil
	}

	// Approvals.
	if strings.HasPrefix(query, `INSERT INTO `+wfApprovalsTable) {
		runID, nodeID := agentBArgString(v[0]), agentBArgString(v[1])
		if t.approvals[runID] == nil {
			t.approvals[runID] = map[string]*agentBPGApproval{}
		}
		if _, exists := t.approvals[runID][nodeID]; exists {
			return c.db.result(0), nil
		}
		t.approvals[runID][nodeID] = &agentBPGApproval{
			runID: runID, nodeID: nodeID, requester: agentBArgString(v[2]),
			approver: agentBArgString(v[3]), decision: agentBArgString(v[4]), at: agentBArgTime(v[5]),
		}
		return c.db.result(1), nil
	}

	// Delivery claims.
	if strings.HasPrefix(query, `INSERT INTO nexaroute_workflow_deliveries`) {
		key := agentBArgString(v[0])
		if t.deliveries[key] {
			return c.db.result(0), nil
		}
		t.deliveries[key] = true
		return c.db.result(1), nil
	}

	return nil, fmt.Errorf("agentb-pg-fake: unmatched exec: %.120s", query)
}
