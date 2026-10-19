package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const snapshotTable = "nexaroute_control_snapshots"

// SQLStore uses database/sql so the control plane does not couple its contract
// to one PostgreSQL driver. The caller owns driver registration and pooling.
type SQLStore struct{ db *sql.DB }

func NewSQLStore(db *sql.DB) (*SQLStore, error) {
	if db == nil {
		return nil, errors.New("nil database")
	}
	return &SQLStore{db: db}, nil
}

func (s *SQLStore) Get(ctx context.Context, namespace string) (Snapshot, error) {
	var out Snapshot
	err := s.db.QueryRowContext(ctx, `SELECT revision, schema_version, payload, updated_at FROM `+snapshotTable+` WHERE namespace = $1`, namespace).
		Scan(&out.Revision, &out.Schema, &out.Payload, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	if err != nil {
		return Snapshot{}, wrapStoreError("get control snapshot", err)
	}
	return out, nil
}

func (s *SQLStore) Put(ctx context.Context, namespace string, expectedRevision uint64, payload []byte) (Snapshot, error) {
	if namespace == "" || len(payload) == 0 {
		return Snapshot{}, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Snapshot{}, wrapStoreError("begin control write", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	// Serialize writers per namespace for the whole transaction. A plain
	// SELECT ... FOR UPDATE cannot lock a row that does not exist yet, so
	// two competing first writes to a fresh namespace would both pass the
	// revision check and one would leak a raw unique-violation at INSERT.
	// The advisory transaction lock makes the loser observe the winner's
	// committed row and fail with the deterministic ErrConflict instead.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, namespace); err != nil {
		return Snapshot{}, wrapStoreError("lock control namespace", err)
	}
	var current uint64
	var schema int
	scanErr := tx.QueryRowContext(ctx, `SELECT revision, schema_version FROM `+snapshotTable+` WHERE namespace = $1 FOR UPDATE`, namespace).Scan(&current, &schema)
	if errors.Is(scanErr, sql.ErrNoRows) {
		if expectedRevision != 0 {
			return Snapshot{}, ErrConflict
		}
		current, schema = 0, 1
	} else if scanErr != nil {
		return Snapshot{}, wrapStoreError("lock control snapshot", scanErr)
	} else if current != expectedRevision {
		return Snapshot{}, ErrConflict
	}
	next := Snapshot{Revision: current + 1, Schema: schema, Payload: append([]byte(nil), payload...), UpdatedAt: time.Now().UTC()}
	if current == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO `+snapshotTable+` (namespace, revision, schema_version, payload, updated_at) VALUES ($1,$2,$3,$4,$5)`, namespace, next.Revision, next.Schema, next.Payload, next.UpdatedAt)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE `+snapshotTable+` SET revision=$2, schema_version=$3, payload=$4, updated_at=$5 WHERE namespace=$1 AND revision=$6`, namespace, next.Revision, next.Schema, next.Payload, next.UpdatedAt, expectedRevision)
	}
	if err != nil {
		return Snapshot{}, wrapStoreError("write control snapshot", err)
	}
	if err = tx.Commit(); err != nil {
		return Snapshot{}, wrapStoreError("commit control snapshot", err)
	}
	committed = true
	return next, nil
}

func (s *SQLStore) Delete(ctx context.Context, namespace string, expectedRevision uint64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM `+snapshotTable+` WHERE namespace=$1 AND revision=$2`, namespace, expectedRevision)
	if err != nil {
		return wrapStoreError("delete control snapshot", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return wrapStoreError("control delete result", err)
	}
	if n == 0 {
		// No row matched namespace+revision: distinguish a missing
		// namespace (ErrNotFound, like every other backend) from a
		// revision mismatch on an existing namespace (ErrConflict).
		// Mapping both to ErrConflict made "delete something that
		// was never created" look like a write conflict.
		var probe int64
		qerr := s.db.QueryRowContext(ctx, `SELECT revision FROM `+snapshotTable+` WHERE namespace=$1`, namespace).Scan(&probe)
		if errors.Is(qerr, sql.ErrNoRows) {
			return ErrNotFound
		}
		if qerr != nil {
			return wrapStoreError("delete control snapshot", qerr)
		}
		return ErrConflict
	}
	return nil
}

func ControlPlaneSQLSchema() string {
	return `CREATE TABLE IF NOT EXISTS nexaroute_control_snapshots (
  namespace TEXT PRIMARY KEY,
  revision BIGINT NOT NULL,
  schema_version INTEGER NOT NULL,
  payload BYTEA NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL
);`
}
