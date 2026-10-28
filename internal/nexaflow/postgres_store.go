package nexaflow

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver for this adapter
)

// PostgreSQL workflow store (Phase 2): the Store contract over five
// tables for multi-process deployments where the file store cannot be
// shared. Semantics match the memory store (the shared store-contract
// suite enforces this), with two deliberate strengthenings for the
// cross-process domain, copied from the video queue store:
//   - Create/Update/TryClaim are atomic at the database serialization
//     point: a concurrent duplicate idempotency key resolves to exactly
//     one run (partial unique index), and lease claims are a single
//     conditional UPDATE.
//   - Lease comparisons use the database server clock
//     (clock_timestamp()), not the caller's wall clock, so skewed
//     replica clocks cannot resurrect a live foreign lease.
//
// Columns are authoritative for state, idempotency_key, lease_owner
// and lease_until; the JSONB payload carries the whole record. Every
// read overlays the columns onto the unmarshaled struct so callers
// never see a stale embedded copy.
//
// Migrations use the video advisory-lock CREATE TABLE IF NOT EXISTS
// pattern (not a second migration framework): first open takes the
// advisory lock, creates missing tables/indexes, and releases it.
const (
	wfDefsTable      = "nexaroute_workflow_definitions"
	wfRevsTable      = "nexaroute_workflow_revisions"
	wfRunsTable      = "nexaroute_workflow_runs"
	wfNodesTable     = "nexaroute_workflow_nodes"
	wfApprovalsTable = "nexaroute_workflow_approvals"
	wfRunsIdemIdx    = "nexaroute_workflow_runs_idem_uniq"
	// Advisory-lock key for first-open migration ("NEXAWFLW").
	wfMigrateLock = int64(0x4e45584157464c57)
)

// PostgresStore implements Store over database/sql. The caller owns
// driver registration and pooling; use OpenPostgresStore for the
// standard pgx-backed construction.
type PostgresStore struct {
	db *sql.DB
}

// OpenPostgresStore opens a pgx connection pool, verifies it, and runs
// the first-open migration.
func OpenPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("nexaflow: open postgres store: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("nexaflow: ping postgres store: %w", err)
	}
	s := &PostgresStore{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// NewPostgresStore wraps an existing pool and runs the migration. It
// exists for tests that already own a *sql.DB.
func NewPostgresStore(ctx context.Context, db *sql.DB) (*PostgresStore, error) {
	s := &PostgresStore{db: db}
	if err := s.migrate(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Close releases the pool.
func (s *PostgresStore) Close() error { return s.db.Close() }

func (s *PostgresStore) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("nexaflow: begin workflow migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, wfMigrateLock); err != nil {
		return fmt.Errorf("nexaflow: workflow migration lock: %w", err)
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS ` + wfDefsTable + ` (
			definition_id TEXT PRIMARY KEY,
			draft JSONB NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
		)`,
		`CREATE TABLE IF NOT EXISTS ` + wfRevsTable + ` (
			definition_id TEXT NOT NULL,
			revision BIGINT NOT NULL,
			content_hash TEXT NOT NULL,
			definition JSONB NOT NULL,
			status TEXT NOT NULL DEFAULT 'published',
			published_by TEXT NOT NULL DEFAULT '',
			published_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
			PRIMARY KEY (definition_id, revision)
		)`,
		`ALTER TABLE ` + wfRevsTable + ` ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'published'`,
		`CREATE TABLE IF NOT EXISTS ` + wfRunsTable + ` (
			run_id TEXT PRIMARY KEY,
			definition_id TEXT NOT NULL,
			revision BIGINT NOT NULL,
			definition JSONB NOT NULL,
			state TEXT NOT NULL,
			idempotency_key TEXT NOT NULL DEFAULT '',
			principal TEXT NOT NULL DEFAULT '',
			principal_roles JSONB NOT NULL DEFAULT '[]'::jsonb,
			input JSONB NOT NULL DEFAULT '{}'::jsonb,
			error_code TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
			started_at TIMESTAMPTZ NULL,
			finished_at TIMESTAMPTZ NULL,
			lease_owner TEXT NOT NULL DEFAULT '',
			lease_until TIMESTAMPTZ NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS ` + wfRunsIdemIdx + ` ON ` + wfRunsTable + ` (idempotency_key) WHERE idempotency_key <> ''`,
		`CREATE TABLE IF NOT EXISTS ` + wfNodesTable + ` (
			run_id TEXT NOT NULL,
			node_id TEXT NOT NULL,
			attempt INT NOT NULL,
			payload JSONB NOT NULL,
			state TEXT NOT NULL,
			lease_owner TEXT NOT NULL DEFAULT '',
			lease_until TIMESTAMPTZ NULL,
			PRIMARY KEY (run_id, node_id, attempt)
		)`,
		`CREATE TABLE IF NOT EXISTS ` + wfApprovalsTable + ` (
			run_id TEXT NOT NULL,
			node_id TEXT NOT NULL,
			requester TEXT NOT NULL DEFAULT '',
			approver TEXT NOT NULL DEFAULT '',
			decision TEXT NOT NULL,
			decided_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
			PRIMARY KEY (run_id, node_id)
		)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("nexaflow: workflow migration statement: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("nexaflow: commit workflow migration: %w", err)
	}
	return nil
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return raw
}

func (s *PostgresStore) SaveDraft(ctx context.Context, def Definition) error {
	if def.ID == "" {
		return errors.New("nexaflow: definition id is required")
	}
	canon := cloneDefinition(def)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO `+wfDefsTable+` (definition_id, draft, updated_at) VALUES ($1, $2, clock_timestamp())
		 ON CONFLICT (definition_id) DO UPDATE SET draft = EXCLUDED.draft, updated_at = clock_timestamp()`,
		def.ID, mustJSON(canon))
	return err
}

func (s *PostgresStore) GetDraft(ctx context.Context, definitionID string) (Definition, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT draft FROM `+wfDefsTable+` WHERE definition_id = $1`, definitionID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Definition{}, ErrNotFound
	}
	if err != nil {
		return Definition{}, err
	}
	var def Definition
	if err := json.Unmarshal(raw, &def); err != nil {
		return Definition{}, err
	}
	return cloneDefinition(def), nil
}

func (s *PostgresStore) Publish(ctx context.Context, definitionID, publishedBy string) (PublishedDefinition, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PublishedDefinition{}, err
	}
	defer tx.Rollback()
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT draft FROM `+wfDefsTable+` WHERE definition_id = $1 FOR UPDATE`, definitionID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return PublishedDefinition{}, ErrNotFound
	}
	if err != nil {
		return PublishedDefinition{}, err
	}
	var draft Definition
	if err := json.Unmarshal(raw, &draft); err != nil {
		return PublishedDefinition{}, err
	}
	draft = cloneDefinition(draft)
	hash, err := ContentHash(&draft)
	if err != nil {
		return PublishedDefinition{}, err
	}
	var lastHash, lastStatus string
	var lastRev int64
	var lastDef []byte
	var lastBy string
	var lastAt time.Time
	err = tx.QueryRowContext(ctx,
		`SELECT revision, content_hash, definition, status, published_by, published_at FROM `+wfRevsTable+` WHERE definition_id = $1 ORDER BY revision DESC LIMIT 1`,
		definitionID).Scan(&lastRev, &lastHash, &lastDef, &lastStatus, &lastBy, &lastAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return PublishedDefinition{}, err
	}
	if err == nil && lastStatus == string(DefinitionPublished) && lastHash == hash {
		var def Definition
		if err := json.Unmarshal(lastDef, &def); err != nil {
			return PublishedDefinition{}, err
		}
		if err := tx.Commit(); err != nil {
			return PublishedDefinition{}, err
		}
		return PublishedDefinition{
			Version:     DefinitionVersion{DefinitionID: definitionID, Revision: uint64(lastRev), ContentHash: lastHash, Status: DefinitionPublished},
			Definition:  cloneDefinition(def),
			PublishedBy: lastBy, PublishedAt: lastAt,
		}, nil
	}
	next := lastRev + 1
	if errors.Is(err, sql.ErrNoRows) {
		next = 1
	}
	now := time.Now()
	if _, err := tx.ExecContext(ctx,
		`UPDATE `+wfRevsTable+` SET status = $1 WHERE definition_id = $2 AND status = $3`,
		string(DefinitionSuperseded), definitionID, string(DefinitionPublished)); err != nil {
		return PublishedDefinition{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO `+wfRevsTable+` (definition_id, revision, content_hash, definition, status, published_by, published_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		definitionID, next, hash, mustJSON(draft), string(DefinitionPublished), publishedBy, now); err != nil {
		return PublishedDefinition{}, err
	}
	if err := tx.Commit(); err != nil {
		return PublishedDefinition{}, err
	}
	return PublishedDefinition{
		Version:     DefinitionVersion{DefinitionID: definitionID, Revision: uint64(next), ContentHash: hash, Status: DefinitionPublished},
		Definition:  draft,
		PublishedBy: publishedBy, PublishedAt: now,
	}, nil
}

func (s *PostgresStore) SupersedeRevision(ctx context.Context, definitionID string, revision uint64) (PublishedDefinition, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PublishedDefinition{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE `+wfRevsTable+` SET status = $1 WHERE definition_id = $2 AND revision = $3 AND status = $4`,
		string(DefinitionSuperseded), definitionID, revision, string(DefinitionPublished))
	if err != nil {
		return PublishedDefinition{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return PublishedDefinition{}, err
	}
	if n == 0 {
		var exists bool
		err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+wfRevsTable+` WHERE definition_id = $1 AND revision = $2)`, definitionID, revision).Scan(&exists)
		if err != nil {
			return PublishedDefinition{}, err
		}
		if !exists {
			return PublishedDefinition{}, ErrNotFound
		}
		return PublishedDefinition{}, ErrStateConflict
	}
	if err := tx.Commit(); err != nil {
		return PublishedDefinition{}, err
	}
	return s.GetRevision(ctx, definitionID, revision)
}

func (s *PostgresStore) GetRevision(ctx context.Context, definitionID string, revision uint64) (PublishedDefinition, error) {
	var hash, by, status string
	var at time.Time
	var raw []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT content_hash, definition, status, published_by, published_at FROM `+wfRevsTable+` WHERE definition_id = $1 AND revision = $2`,
		definitionID, revision).Scan(&hash, &raw, &status, &by, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return PublishedDefinition{}, ErrNotFound
	}
	if err != nil {
		return PublishedDefinition{}, err
	}
	var def Definition
	if err := json.Unmarshal(raw, &def); err != nil {
		return PublishedDefinition{}, err
	}
	return PublishedDefinition{
		Version:     DefinitionVersion{DefinitionID: definitionID, Revision: revision, ContentHash: hash, Status: status},
		Definition:  cloneDefinition(def),
		PublishedBy: by, PublishedAt: at,
	}, nil
}

func (s *PostgresStore) ListRevisions(ctx context.Context, definitionID string) ([]PublishedDefinition, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT revision, content_hash, definition, status, published_by, published_at FROM `+wfRevsTable+` WHERE definition_id = $1 ORDER BY revision`,
		definitionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PublishedDefinition
	for rows.Next() {
		var rev int64
		var hash, by, status string
		var at time.Time
		var raw []byte
		if err := rows.Scan(&rev, &hash, &raw, &status, &by, &at); err != nil {
			return nil, err
		}
		var def Definition
		if err := json.Unmarshal(raw, &def); err != nil {
			return nil, err
		}
		out = append(out, PublishedDefinition{
			Version:     DefinitionVersion{DefinitionID: definitionID, Revision: uint64(rev), ContentHash: hash, Status: status},
			Definition:  cloneDefinition(def),
			PublishedBy: by, PublishedAt: at,
		})
	}
	return out, rows.Err()
}

func scanRun(row interface{ Scan(...any) error }) (Run, error) {
	var r Run
	var defRaw, inputRaw, rolesRaw []byte
	var started, finished, leaseUntil sql.NullTime
	var leaseOwner string
	err := row.Scan(&r.ID, &r.DefinitionID, &r.Revision, &defRaw, &r.State, &r.IdempotencyKey,
		&r.Principal, &rolesRaw, &inputRaw, &r.ErrorCode, &r.Error,
		&r.CreatedAt, &r.UpdatedAt, &started, &finished, &leaseOwner, &leaseUntil)
	if err != nil {
		return Run{}, err
	}
	if err := json.Unmarshal(defRaw, &r.Definition); err != nil {
		return Run{}, err
	}
	r.Definition = cloneDefinition(r.Definition)
	if err := json.Unmarshal(rolesRaw, &r.PrincipalRoles); err != nil {
		return Run{}, err
	}
	var input map[string]any
	if err := json.Unmarshal(inputRaw, &input); err != nil {
		return Run{}, err
	}
	r.Input = deepCopyMap(input)
	if started.Valid {
		r.StartedAt = started.Time
	}
	if finished.Valid {
		r.FinishedAt = finished.Time
	}
	r.LeaseOwner = leaseOwner
	if leaseUntil.Valid {
		r.LeaseUntil = leaseUntil.Time
	}
	return r, nil
}

const runColumns = `run_id, definition_id, revision, definition, state, idempotency_key, principal, principal_roles, input, error_code, error, created_at, updated_at, started_at, finished_at, lease_owner, lease_until`

func (s *PostgresStore) CreateRun(ctx context.Context, run Run) error {
	if run.ID == "" {
		return errors.New("nexaflow: run id is required")
	}
	if run.State == "" {
		run.State = RunCreated
	}
	run.Definition = cloneDefinition(run.Definition)
	now := time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO `+wfRunsTable+` (`+runColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		run.ID, run.DefinitionID, run.Revision, mustJSON(run.Definition), string(run.State),
		run.IdempotencyKey, run.Principal, mustJSON(run.PrincipalRoles), mustJSON(deepCopyMap(run.Input)),
		run.ErrorCode, run.Error, now, now, nullTime(run.StartedAt), nullTime(run.FinishedAt),
		run.LeaseOwner, nullTime(run.LeaseUntil))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if pgErr.ConstraintName == wfRunsIdemIdx {
				return ErrIdempotencyConflict
			}
			return ErrAlreadyExists
		}
		return err
	}
	return nil
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func (s *PostgresStore) GetRun(ctx context.Context, runID string) (Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM `+wfRunsTable+` WHERE run_id = $1`, runID)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

func (s *PostgresStore) GetRunByIdempotency(ctx context.Context, key string) (Run, error) {
	if key == "" {
		return Run{}, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM `+wfRunsTable+` WHERE idempotency_key = $1`, key)
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

func (s *PostgresStore) UpdateRunState(ctx context.Context, runID string, expected RunState, state RunState, errorCode, errMsg string) (Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback()
	row := tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM `+wfRunsTable+` WHERE run_id = $1 FOR UPDATE`, runID)
	cur, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	if err != nil {
		return Run{}, err
	}
	if expected != "" && cur.State != expected {
		return Run{}, ErrStateConflict
	}
	if cur.State.Terminal() && cur.State != state {
		return Run{}, ErrTerminalState
	}
	if !validRunTransition(cur.State, state) {
		return Run{}, ErrStateConflict
	}
	now := time.Now()
	started := cur.StartedAt
	if cur.State != RunRunning && state == RunRunning && started.IsZero() {
		started = now
	}
	finished := cur.FinishedAt
	if state.Terminal() && finished.IsZero() {
		finished = now
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE `+wfRunsTable+` SET state = $2, error_code = $3, error = $4, updated_at = $5, started_at = $6, finished_at = $7 WHERE run_id = $1`,
		runID, string(state), errorCode, errMsg, now, nullTime(started), nullTime(finished)); err != nil {
		return Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return Run{}, err
	}
	cur.State = state
	cur.ErrorCode = errorCode
	cur.Error = errMsg
	cur.UpdatedAt = now
	cur.StartedAt = started
	cur.FinishedAt = finished
	return cur, nil
}

func (s *PostgresStore) ListRuns(ctx context.Context, filter RunFilter) ([]Run, error) {
	query := `SELECT ` + runColumns + ` FROM ` + wfRunsTable + ` WHERE 1=1`
	args := []any{}
	if filter.DefinitionID != "" {
		args = append(args, filter.DefinitionID)
		query += fmt.Sprintf(` AND definition_id = $%d`, len(args))
	}
	if filter.State != "" {
		args = append(args, string(filter.State))
		query += fmt.Sprintf(` AND state = $%d`, len(args))
	}
	query += ` ORDER BY created_at, run_id`
	if filter.Limit > 0 {
		args = append(args, filter.Limit)
		query += fmt.Sprintf(` LIMIT $%d`, len(args))
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PostgresStore) Prune(ctx context.Context, before time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	terminal := []string{string(RunSucceeded), string(RunFailed), string(RunCancelled)}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM `+wfNodesTable+` WHERE run_id IN (SELECT run_id FROM `+wfRunsTable+` WHERE state = ANY($1) AND finished_at IS NOT NULL AND finished_at < $2)`,
		terminal, before); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM `+wfApprovalsTable+` WHERE run_id IN (SELECT run_id FROM `+wfRunsTable+` WHERE state = ANY($1) AND finished_at IS NOT NULL AND finished_at < $2)`,
		terminal, before); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM `+wfRunsTable+` WHERE state = ANY($1) AND finished_at IS NOT NULL AND finished_at < $2`,
		terminal, before)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}

func scanNode(row interface{ Scan(...any) error }) (NodeExecution, error) {
	var e NodeExecution
	var raw []byte
	var state string
	var leaseOwner string
	var leaseUntil sql.NullTime
	err := row.Scan(&e.RunID, &e.NodeID, &e.Attempt, &raw, &state, &leaseOwner, &leaseUntil)
	if err != nil {
		return NodeExecution{}, err
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return NodeExecution{}, err
	}
	e.State = NodeState(state)
	e.LeaseOwner = leaseOwner
	if leaseUntil.Valid {
		e.LeaseUntil = leaseUntil.Time
	}
	e.Input = deepCopyMap(e.Input)
	e.Output = deepCopyMap(e.Output)
	e.ResumeInput = deepCopyMap(e.ResumeInput)
	return e, nil
}

func (s *PostgresStore) AppendNodeExecution(ctx context.Context, exec NodeExecution) (NodeExecution, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NodeExecution{}, err
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+wfRunsTable+` WHERE run_id = $1)`, exec.RunID).Scan(&exists); err != nil {
		return NodeExecution{}, err
	}
	if !exists {
		return NodeExecution{}, ErrNotFound
	}
	var maxAttempt sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(attempt) FROM `+wfNodesTable+` WHERE run_id = $1 AND node_id = $2`, exec.RunID, exec.NodeID).Scan(&maxAttempt); err != nil {
		return NodeExecution{}, err
	}
	attempt := 0
	if maxAttempt.Valid {
		attempt = int(maxAttempt.Int64) + 1
	}
	exec.Attempt = attempt
	exec.IdempotencyKey = NodeIDKey(exec.RunID, exec.NodeID, attempt)
	if exec.State == "" {
		exec.State = NodePending
	}
	now := time.Now()
	exec.CreatedAt = now
	exec.UpdatedAt = now
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO `+wfNodesTable+` (run_id, node_id, attempt, payload, state, lease_owner, lease_until) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		exec.RunID, exec.NodeID, attempt, mustJSON(exec), string(exec.State), exec.LeaseOwner, nullTime(exec.LeaseUntil)); err != nil {
		return NodeExecution{}, err
	}
	if err := tx.Commit(); err != nil {
		return NodeExecution{}, err
	}
	return exec, nil
}

func (s *PostgresStore) SaveNodeExecution(ctx context.Context, exec NodeExecution, expected NodeState) (NodeExecution, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NodeExecution{}, err
	}
	defer tx.Rollback()
	row := tx.QueryRowContext(ctx,
		`SELECT run_id, node_id, attempt, payload, state, lease_owner, lease_until FROM `+wfNodesTable+` WHERE run_id = $1 AND node_id = $2 AND attempt = $3 FOR UPDATE`,
		exec.RunID, exec.NodeID, exec.Attempt)
	cur, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		// The caller may pass Attempt 0 for the current row without
		// knowing it; resolve the current attempt instead.
		row2 := tx.QueryRowContext(ctx,
			`SELECT run_id, node_id, attempt, payload, state, lease_owner, lease_until FROM `+wfNodesTable+` WHERE run_id = $1 AND node_id = $2 ORDER BY attempt DESC LIMIT 1 FOR UPDATE`,
			exec.RunID, exec.NodeID)
		cur, err = scanNode(row2)
		if errors.Is(err, sql.ErrNoRows) {
			return NodeExecution{}, ErrNotFound
		}
		if err != nil {
			return NodeExecution{}, err
		}
		// The resolved row is authoritative for attempt identity.
		exec.Attempt = cur.Attempt
	}
	if err != nil {
		return NodeExecution{}, err
	}
	if expected != "" && cur.State != expected {
		return NodeExecution{}, ErrStateConflict
	}
	if cur.State.Terminal() && cur.State != exec.State {
		return NodeExecution{}, ErrTerminalState
	}
	if !validNodeTransition(cur.State, exec.State) {
		return NodeExecution{}, ErrStateConflict
	}
	exec.IdempotencyKey = cur.IdempotencyKey
	exec.CreatedAt = cur.CreatedAt
	if exec.State.Terminal() {
		exec.LeaseOwner = ""
		exec.LeaseUntil = time.Time{}
	}
	now := time.Now()
	if cur.State != NodeRunning && exec.State == NodeRunning && exec.StartedAt.IsZero() {
		exec.StartedAt = now
	}
	if exec.State.Terminal() && exec.FinishedAt.IsZero() {
		exec.FinishedAt = now
	}
	exec.UpdatedAt = now
	if _, err := tx.ExecContext(ctx,
		`UPDATE `+wfNodesTable+` SET payload = $4, state = $5, lease_owner = $6, lease_until = $7 WHERE run_id = $1 AND node_id = $2 AND attempt = $3`,
		exec.RunID, exec.NodeID, exec.Attempt, mustJSON(exec), string(exec.State), exec.LeaseOwner, nullTime(exec.LeaseUntil)); err != nil {
		return NodeExecution{}, err
	}
	if err := tx.Commit(); err != nil {
		return NodeExecution{}, err
	}
	return exec, nil
}

func (s *PostgresStore) GetNodeExecutions(ctx context.Context, runID string) ([]NodeExecution, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+wfRunsTable+` WHERE run_id = $1)`, runID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT run_id, node_id, attempt, payload, state, lease_owner, lease_until FROM `+wfNodesTable+` WHERE run_id = $1 ORDER BY node_id, attempt`,
		runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NodeExecution
	for rows.Next() {
		e, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *PostgresStore) TryClaim(ctx context.Context, runID, nodeID, owner string, ttl time.Duration, now time.Time) (NodeExecution, bool, error) {
	// The server clock is authoritative; the caller's now is ignored,
	// exactly like the video queue store.
	_ = now
	// Claim the current (highest) attempt: resolve it, then run the
	// conditional update. Two statements under one transaction keep
	// the resolve-and-claim atomic for concurrent claimants.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return NodeExecution{}, false, err
	}
	defer tx.Rollback()
	var attempt int
	err = tx.QueryRowContext(ctx,
		`SELECT attempt FROM `+wfNodesTable+` WHERE run_id = $1 AND node_id = $2 ORDER BY attempt DESC LIMIT 1 FOR UPDATE`,
		runID, nodeID).Scan(&attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return NodeExecution{}, false, ErrNotFound
	}
	if err != nil {
		return NodeExecution{}, false, err
	}
	var raw []byte
	var state string
	var leaseOwner string
	var leaseUntil sql.NullTime
	err = tx.QueryRowContext(ctx,
		`UPDATE `+wfNodesTable+` SET state = 'claimed', lease_owner = $4, lease_until = clock_timestamp() + ($5 * interval '1 second')
		 WHERE run_id = $1 AND node_id = $2 AND attempt = $3
		 AND state IN ('pending', 'claimed')
		 AND (lease_owner = '' OR lease_until IS NULL OR lease_until <= clock_timestamp() OR lease_owner = $4)
		 RETURNING payload, state, lease_owner, lease_until`,
		runID, nodeID, attempt, owner, ttl.Seconds()).Scan(&raw, &state, &leaseOwner, &leaseUntil)
	if errors.Is(err, sql.ErrNoRows) {
		// No row matched: a live foreign lease blocks, or the state
		// is not claimable. Either way the claim loses, without error.
		_ = tx.Commit()
		return NodeExecution{}, false, nil
	}
	if err != nil {
		return NodeExecution{}, false, err
	}
	var e NodeExecution
	if err := json.Unmarshal(raw, &e); err != nil {
		return NodeExecution{}, false, err
	}
	e.State = NodeState(state)
	e.LeaseOwner = leaseOwner
	if leaseUntil.Valid {
		e.LeaseUntil = leaseUntil.Time
	}
	e.Input = deepCopyMap(e.Input)
	e.Output = deepCopyMap(e.Output)
	e.ResumeInput = deepCopyMap(e.ResumeInput)
	if err := tx.Commit(); err != nil {
		return NodeExecution{}, false, err
	}
	return e, true, nil
}

func (s *PostgresStore) ReleaseLease(ctx context.Context, runID, nodeID, owner string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE `+wfNodesTable+` SET lease_owner = '', lease_until = NULL
		 WHERE run_id = $1 AND node_id = $2 AND attempt = (SELECT MAX(attempt) FROM `+wfNodesTable+` WHERE run_id = $1 AND node_id = $2)
		 AND lease_owner = $3`,
		runID, nodeID, owner)
	return err
}

func (s *PostgresStore) RecordApproval(ctx context.Context, appr Approval) (Approval, error) {
	if appr.Decision != "approved" && appr.Decision != "rejected" {
		return Approval{}, errors.New("nexaflow: approval decision must be approved or rejected")
	}
	if appr.Approver != "" && appr.Requester != "" && appr.Approver == appr.Requester {
		return Approval{}, errors.New("nexaflow: approver must not be the requester")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Approval{}, err
	}
	defer tx.Rollback()
	var prevDecision, prevApprover string
	err = tx.QueryRowContext(ctx,
		`SELECT decision, approver FROM `+wfApprovalsTable+` WHERE run_id = $1 AND node_id = $2 FOR UPDATE`,
		appr.RunID, appr.NodeID).Scan(&prevDecision, &prevApprover)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Approval{}, err
	}
	if err == nil {
		if prevDecision == appr.Decision && prevApprover == appr.Approver {
			var decidedAt time.Time
			_ = tx.QueryRowContext(ctx,
				`SELECT decided_at FROM `+wfApprovalsTable+` WHERE run_id = $1 AND node_id = $2`,
				appr.RunID, appr.NodeID).Scan(&decidedAt)
			_ = tx.Commit()
			appr.DecidedAt = decidedAt
			return appr, nil
		}
		return Approval{}, ErrApprovalConflict
	}
	now := time.Now()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO `+wfApprovalsTable+` (run_id, node_id, requester, approver, decision, decided_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		appr.RunID, appr.NodeID, appr.Requester, appr.Approver, appr.Decision, now); err != nil {
		return Approval{}, err
	}
	if err := tx.Commit(); err != nil {
		return Approval{}, err
	}
	appr.DecidedAt = now
	return appr, nil
}

func (s *PostgresStore) GetApproval(ctx context.Context, runID, nodeID string) (Approval, error) {
	var a Approval
	err := s.db.QueryRowContext(ctx,
		`SELECT run_id, node_id, requester, approver, decision, decided_at FROM `+wfApprovalsTable+` WHERE run_id = $1 AND node_id = $2`,
		runID, nodeID).Scan(&a.RunID, &a.NodeID, &a.Requester, &a.Approver, &a.Decision, &a.DecidedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Approval{}, ErrNoApprovalRecord
	}
	return a, err
}
