package controlplane

import (
	"context"
	"database/sql"
	"fmt"
)

const migrationTable = "nexaroute_control_migrations"

// ApplyMigrations applies the registry atomically per migration. PostgreSQL's
// advisory transaction lock prevents two gateway replicas from migrating at
// the same time. A checksum mismatch fails closed instead of silently running
// a modified migration under an old version number.
//
// The lock must be acquired BEFORE the migration table is created: two
// replicas starting against a cold database both ran CREATE TABLE IF NOT
// EXISTS concurrently, and PostgreSQL can reject the loser with a
// pg_type_typname_nsp_index duplicate-key error (observed 2026-10-27, HA
// acceptance: one of two simultaneous cold starts failed with exactly
// that error while the other succeeded). With the lock held first, the
// loser waits, then sees the table already present.
func ApplyMigrations(ctx context.Context, db *sql.DB, registry MigrationRegistry) error {
	if db == nil {
		return ErrUnavailable
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtext('nexaroute:migrations'))`); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+migrationTable+` (version BIGINT PRIMARY KEY, name TEXT NOT NULL, checksum TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP)`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	// Best-effort unlock: the connection is closed immediately after, which
	// releases the advisory lock server-side either way.
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtext('nexaroute:migrations'))`)
	}()
	for _, migration := range registry.All() {
		var checksum string
		err := conn.QueryRowContext(ctx, `SELECT checksum FROM `+migrationTable+` WHERE version=$1`, migration.Version).Scan(&checksum)
		if err == nil {
			if checksum != migration.Checksum() {
				return fmt.Errorf("migration %d checksum mismatch", migration.Version)
			}
			continue
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("read migration %d: %w", migration.Version, err)
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", migration.Version, err)
		}
		if _, err = tx.ExecContext(ctx, migration.Up); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %d (%s): %w", migration.Version, migration.Name, err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO `+migrationTable+` (version,name,checksum) VALUES ($1,$2,$3)`, migration.Version, migration.Name, migration.Checksum()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", migration.Version, err)
		}
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", migration.Version, err)
		}
	}
	return nil
}
