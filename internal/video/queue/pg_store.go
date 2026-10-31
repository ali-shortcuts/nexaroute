package queue

// PostgreSQL video job store (issue #609, architecture slice 4): the
// JobStore contract over one table for multi-process deployments where
// the local file store cannot be shared. Semantics match the memory
// store exactly (the shared store-contract suite enforces this), with
// two deliberate strengthenings for the cross-process domain:
//
//   - Create/Update/TryClaim are atomic at the database serialization
//     point: a concurrent duplicate idempotency key resolves to exactly
//     one job (UNIQUE partial index + ON CONFLICT mapping), and lease
//     claims are a single conditional UPDATE.
//   - Lease comparisons use the database server clock
//     (clock_timestamp()), not the caller's wall clock, so skewed
//     replica clocks cannot resurrect a live foreign lease.
//
// Columns are authoritative for idempotency_key, lease_owner and
// lease_until; the JSONB payload carries the whole VideoJob record
// (shots, costs, checkpoints). Every read overlays the columns onto
// the unmarshaled struct so callers never see a stale embedded copy.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver for this adapter

	"github.com/ali-shortcuts/nexaroute/internal/video"
)

const (
	videoJobsTable   = "nexaroute_video_jobs"
	videoJobsIdemIdx = "nexaroute_video_jobs_idem_uniq"
	// Advisory-lock key for first-open migration ("NEXAVIDJ").
	videoJobsMigrateLock = int64(0x4e4558415649444a)
)

// PostgresStore is a JobStore over PostgreSQL.
type PostgresStore struct {
	db *sql.DB
}

var _ JobStore = (*PostgresStore)(nil)

// OpenPostgresStore connects, pings and migrates the schema. The DSN is
// never logged or embedded in returned errors. An unreachable or
// misconfigured database is a hard error, never a silent fallback.
func OpenPostgresStore(ctx context.Context, dsn string) (*PostgresStore, error) {
	if dsn == "" {
		return nil, errors.New("video job store: postgres DSN is required for the postgres backend")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, errors.New("video job store: invalid postgres configuration")
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, errors.New("video job store: postgres is unreachable")
	}
	s := &PostgresStore{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database connections.
func (s *PostgresStore) Close() error {
	return s.db.Close()
}

func (s *PostgresStore) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("video job store: begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Serialize concurrent first-open migrations across processes.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, videoJobsMigrateLock); err != nil {
		return fmt.Errorf("video job store: migration lock: %w", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS ` + videoJobsTable + ` (` +
			`job_id text PRIMARY KEY,` +
			`idempotency_key text NOT NULL DEFAULT '',` +
			`payload jsonb NOT NULL,` +
			`lease_owner text NOT NULL DEFAULT '',` +
			`lease_until timestamptz NULL)`,
		// Empty keys are never indexed, mirroring the memory store
		// (which skips empty idempotency keys).
		`CREATE UNIQUE INDEX IF NOT EXISTS ` + videoJobsIdemIdx +
			` ON ` + videoJobsTable + ` (idempotency_key) WHERE idempotency_key <> ''`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("video job store: migration: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("video job store: commit migration: %w", err)
	}
	return nil
}

// overlayColumns copies the authoritative columns onto the unmarshaled
// record. Callers must never see the possibly-stale embedded copies.
func overlayColumns(j *video.VideoJob, idem, owner string, until sql.NullTime) {
	j.IdempotencyKey = idem
	j.LeaseOwner = owner
	if until.Valid {
		j.LeaseUntil = until.Time
	} else {
		j.LeaseUntil = time.Time{}
	}
}

func scanJob(row interface {
	Scan(dest ...any) error
}) (video.VideoJob, error) {
	var raw []byte
	var idem, owner string
	var until sql.NullTime
	if err := row.Scan(&raw, &idem, &owner, &until); err != nil {
		return video.VideoJob{}, err
	}
	var j video.VideoJob
	if err := json.Unmarshal(raw, &j); err != nil {
		return video.VideoJob{}, fmt.Errorf("video job store: decode job: %w", err)
	}
	overlayColumns(&j, idem, owner, until)
	return j, nil
}

const videoJobColumns = `payload, idempotency_key, lease_owner, lease_until`

func isUniqueViolation(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return pgErr.ConstraintName, true
	}
	return "", false
}

// Create inserts the job. A duplicate job_id is a plain error (matching
// the memory store's "job already exists"); an idempotency key already
// claimed by a different job maps to ErrIdempotencyConflict, so
// concurrent duplicates resolve to exactly one job at the database
// serialization point.
func (s *PostgresStore) Create(ctx context.Context, j video.VideoJob) error {
	raw, err := json.Marshal(j)
	if err != nil {
		return fmt.Errorf("video job store: encode job: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO `+videoJobsTable+` (job_id, idempotency_key, payload, lease_owner, lease_until)`+
			` VALUES ($1, $2, $3, $4, $5)`,
		j.JobID, j.IdempotencyKey, raw, j.LeaseOwner, nullTime(j.LeaseUntil))
	if err != nil {
		if _, ok := isUniqueViolation(err); ok {
			// Replicate the memory store's check order (job_id
			// first): a follow-up lookup tells the two apart even
			// when both constraints fire.
			var exists bool
			if qerr := s.db.QueryRowContext(ctx,
				`SELECT EXISTS(SELECT 1 FROM `+videoJobsTable+` WHERE job_id = $1)`, j.JobID).Scan(&exists); qerr != nil {
				// The probe itself failed (e.g. the store dropped
				// mid-create): surface that failure instead of
				// misclassifying the create as an idempotency
				// conflict.
				return fmt.Errorf("video job store: create conflict probe: %w", qerr)
			} else if exists {
				return ErrJobAlreadyExists
			}
			return ErrIdempotencyConflict
		}
		return fmt.Errorf("video job store: create: %w", err)
	}
	return nil
}

// Get returns the job by id; a missing job is ErrNotFound.
func (s *PostgresStore) Get(ctx context.Context, id string) (video.VideoJob, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx,
		`SELECT `+videoJobColumns+` FROM `+videoJobsTable+` WHERE job_id = $1`, id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return video.VideoJob{}, ErrNotFound
		}
		return video.VideoJob{}, fmt.Errorf("video job store: get: %w", err)
	}
	return j, nil
}

// GetByIdempotency returns the job holding the key; an empty or
// unknown key is ErrNotFound (empty keys are never indexed).
func (s *PostgresStore) GetByIdempotency(ctx context.Context, key string) (video.VideoJob, error) {
	if key == "" {
		return video.VideoJob{}, ErrNotFound
	}
	j, err := scanJob(s.db.QueryRowContext(ctx,
		`SELECT `+videoJobColumns+` FROM `+videoJobsTable+` WHERE idempotency_key = $1`, key))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return video.VideoJob{}, ErrNotFound
		}
		return video.VideoJob{}, fmt.Errorf("video job store: get by idempotency: %w", err)
	}
	return j, nil
}

// Update replaces the job record. The terminal-overwrite guard
// (checkTerminalUpdate) runs inside the same row-locked transaction as
// the write, so two processes cannot both pass the guard and commit
// conflicting states.
func (s *PostgresStore) Update(ctx context.Context, j video.VideoJob) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("video job store: begin update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var raw []byte
	var idem, owner string
	var until sql.NullTime
	if err := tx.QueryRowContext(ctx,
		`SELECT `+videoJobColumns+` FROM `+videoJobsTable+` WHERE job_id = $1 FOR UPDATE`, j.JobID).Scan(&raw, &idem, &owner, &until); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("video job store: update lock: %w", err)
	}
	var current video.VideoJob
	if err := json.Unmarshal(raw, &current); err != nil {
		return fmt.Errorf("video job store: decode current job: %w", err)
	}
	// The lease columns are authoritative (TryClaim mutates only the
	// columns); overlay them before the terminal guard compares, or a
	// replay of the authoritative record after a claim is rejected
	// with a spurious ErrTerminalJob.
	overlayColumns(&current, idem, owner, until)
	if err := checkTerminalUpdate(current, j); err != nil {
		return err
	}
	next, err := json.Marshal(j)
	if err != nil {
		return fmt.Errorf("video job store: encode job: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE `+videoJobsTable+` SET payload = $2, idempotency_key = $3, lease_owner = $4, lease_until = $5 WHERE job_id = $1`,
		j.JobID, next, j.IdempotencyKey, j.LeaseOwner, nullTime(j.LeaseUntil)); err != nil {
		if _, ok := isUniqueViolation(err); ok {
			return ErrIdempotencyConflict
		}
		return fmt.Errorf("video job store: update: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("video job store: commit update: %w", err)
	}
	return nil
}

// Delete removes the job; the idempotency mapping is the column, so it
// goes away with the row (matching the memory store: pruning frees the
// key). Deleting a missing job is a no-op.
func (s *PostgresStore) Delete(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM `+videoJobsTable+` WHERE job_id = $1`, id); err != nil {
		return fmt.Errorf("video job store: delete: %w", err)
	}
	return nil
}

// List returns all jobs in job_id order.
func (s *PostgresStore) List(ctx context.Context) ([]video.VideoJob, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+videoJobColumns+` FROM `+videoJobsTable+` ORDER BY job_id`)
	if err != nil {
		return nil, fmt.Errorf("video job store: list: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []video.VideoJob
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("video job store: list rows: %w", err)
	}
	return out, nil
}

// TryClaim atomically leases the job to owner for ttl. The check and
// the set are one UPDATE evaluated against the database server clock,
// so two processes cannot both win and skewed replica clocks cannot
// resurrect a live foreign lease. A live foreign lease refuses with
// (zero, false, nil); a missing job is ErrNotFound; a same-owner call
// renews.
func (s *PostgresStore) TryClaim(ctx context.Context, id, owner string, ttl time.Duration, now time.Time) (video.VideoJob, bool, error) {
	_ = now // server clock (clock_timestamp) is authoritative; see above.
	secs := ttl.Seconds()
	var raw []byte
	var idem, leaseOwner string
	var until sql.NullTime
	err := s.db.QueryRowContext(ctx,
		`UPDATE `+videoJobsTable+` SET lease_owner = $2,`+
			` lease_until = clock_timestamp() + ($3 * interval '1 second')`+
			` WHERE job_id = $1`+
			` AND (lease_owner = '' OR lease_until IS NULL OR lease_until <= clock_timestamp() OR lease_owner = $2)`+
			` RETURNING payload, idempotency_key, lease_owner, lease_until`,
		id, owner, secs).Scan(&raw, &idem, &leaseOwner, &until)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			var exists bool
			if qerr := s.db.QueryRowContext(ctx,
				`SELECT EXISTS(SELECT 1 FROM `+videoJobsTable+` WHERE job_id = $1)`, id).Scan(&exists); qerr != nil {
				return video.VideoJob{}, false, fmt.Errorf("video job store: claim existence check: %w", qerr)
			}
			if !exists {
				return video.VideoJob{}, false, ErrNotFound
			}
			return video.VideoJob{}, false, nil
		}
		return video.VideoJob{}, false, fmt.Errorf("video job store: claim: %w", err)
	}
	var j video.VideoJob
	if err := json.Unmarshal(raw, &j); err != nil {
		return video.VideoJob{}, false, fmt.Errorf("video job store: decode claimed job: %w", err)
	}
	overlayColumns(&j, idem, leaseOwner, until)
	return j, true, nil
}

func nullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}
