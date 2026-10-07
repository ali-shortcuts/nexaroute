package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
		return Snapshot{}, fmt.Errorf("get control snapshot: %w", err)
	}
	return out, nil
}

func (s *SQLStore) Put(ctx context.Context, namespace string, expectedRevision uint64, payload []byte) (Snapshot, error) {
	if namespace == "" || len(payload) == 0 {
		return Snapshot{}, ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Snapshot{}, fmt.Errorf("begin control write: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	var current uint64
	var schema int
	scanErr := tx.QueryRowContext(ctx, `SELECT revision, schema_version FROM `+snapshotTable+` WHERE namespace = $1 FOR UPDATE`, namespace).Scan(&current, &schema)
	if errors.Is(scanErr, sql.ErrNoRows) {
		if expectedRevision != 0 {
			return Snapshot{}, ErrConflict
		}
		current, schema = 0, 1
	} else if scanErr != nil {
		return Snapshot{}, fmt.Errorf("lock control snapshot: %w", scanErr)
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
		return Snapshot{}, fmt.Errorf("write control snapshot: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return Snapshot{}, fmt.Errorf("commit control snapshot: %w", err)
	}
	committed = true
	return next, nil
}

func (s *SQLStore) Delete(ctx context.Context, namespace string, expectedRevision uint64) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM `+snapshotTable+` WHERE namespace=$1 AND revision=$2`, namespace, expectedRevision)
	if err != nil {
		return fmt.Errorf("delete control snapshot: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("control delete result: %w", err)
	}
	if n == 0 {
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
