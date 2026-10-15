package securitystore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver for this adapter
)

// PostgreSQL security store (M4-2): the Backend contract over three
// tables — sessions, audit records and chain metadata — for
// multi-process / HA deployments where the local bbolt file cannot be
// shared. Semantics match the bbolt store exactly (the shared
// backend contract suite and cross-backend hash parity tests enforce
// this): every mutation runs in one transaction that first locks the
// allocator metadata row, serializing writers across processes; the
// audit append gate verifies the full chain on the pre-append state
// inside the same transaction, exactly like bbolt's append path.

const (
	securitySessionsTable = "nexaroute_security_sessions"
	securityAuditTable    = "nexaroute_security_audit"
	securityMetaTable     = "nexaroute_security_meta"

	metaKeyHead      = "audit-chain-head"
	metaKeyAnchor    = "audit-chain-anchor"
	metaKeyAllocator = "audit-allocator"
)

// PostgresStore is a Backend over PostgreSQL.
type PostgresStore struct {
	db            *sql.DB
	maxAuditItems int
	retention     time.Duration
}

var _ Backend = (*PostgresStore)(nil)

// OpenPostgres connects, pings and migrates the schema. The DSN is
// supplied by the factory (config value or env: indirection); it is
// never logged or embedded in returned errors by this package.
func OpenPostgres(ctx context.Context, dsn string, maxAuditItems, retentionDays int) (*PostgresStore, error) {
	if maxAuditItems < 100 || maxAuditItems > 1_000_000 {
		return nil, errors.New("security audit max_events must be between 100 and 1000000")
	}
	if retentionDays < 1 || retentionDays > 3650 {
		return nil, errors.New("security audit retention_days must be between 1 and 3650")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, errors.New("securitystore: invalid postgres configuration")
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, errors.New("securitystore: postgres is unreachable")
	}
	s := &PostgresStore{db: db, maxAuditItems: maxAuditItems, retention: time.Duration(retentionDays) * 24 * time.Hour}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *PostgresStore) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("securitystore: begin migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Serialize concurrent first-open migrations across processes.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(0x4e58534543555245)`); err != nil {
		return fmt.Errorf("securitystore: migration lock: %w", err)
	}
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS ` + securitySessionsTable + ` (hash text PRIMARY KEY, value jsonb NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS ` + securityAuditTable + ` (seq bigint PRIMARY KEY, record jsonb NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS ` + securityMetaTable + ` (key text PRIMARY KEY, value jsonb NOT NULL)`,
		// The allocator row always exists so writer transactions can
		// lock it FOR UPDATE even on an empty store.
		`INSERT INTO ` + securityMetaTable + ` (key, value) VALUES ('` + metaKeyAllocator + `', '0') ON CONFLICT (key) DO NOTHING`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("securitystore: migration: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("securitystore: commit migration: %w", err)
	}
	return nil
}

// withWriterTx runs fn in a transaction serialized on the allocator
// row: at most one writer (in any process) plans audit appends or
// session mutations at a time.
func (s *PostgresStore) withWriterTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("securitystore: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var allocatorRaw []byte
	if err := tx.QueryRowContext(ctx, `SELECT value FROM `+securityMetaTable+` WHERE key = $1 FOR UPDATE`, metaKeyAllocator).Scan(&allocatorRaw); err != nil {
		return fmt.Errorf("securitystore: lock allocator: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("securitystore: commit transaction: %w", err)
	}
	return nil
}

func loadChainTx(ctx context.Context, tx *sql.Tx) (chainState, error) {
	var st chainState
	rows, err := tx.QueryContext(ctx, `SELECT seq, record FROM `+securityAuditTable+` ORDER BY seq ASC`)
	if err != nil {
		return st, fmt.Errorf("securitystore: load audit records: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var seq int64
		var raw []byte
		if err := rows.Scan(&seq, &raw); err != nil {
			return st, fmt.Errorf("securitystore: scan audit record: %w", err)
		}
		var event AuditEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return st, errors.New("security store contains a malformed audit record")
		}
		if seq < 0 || event.Seq != uint64(seq) {
			return st, errors.New("security store contains an audit record whose sequence does not match its row")
		}
		st.Records = append(st.Records, event)
	}
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("securitystore: load audit records: %w", err)
	}
	meta := map[string][]byte{}
	mrows, err := tx.QueryContext(ctx, `SELECT key, value FROM `+securityMetaTable)
	if err != nil {
		return st, fmt.Errorf("securitystore: load chain metadata: %w", err)
	}
	defer func() { _ = mrows.Close() }()
	for mrows.Next() {
		var k string
		var v []byte
		if err := mrows.Scan(&k, &v); err != nil {
			return st, fmt.Errorf("securitystore: scan chain metadata: %w", err)
		}
		meta[k] = v
	}
	if err := mrows.Err(); err != nil {
		return st, fmt.Errorf("securitystore: load chain metadata: %w", err)
	}
	st.Head, st.HeadState = parseChainRefMeta(meta[metaKeyHead])
	st.Anchor, st.AnchorState = parseChainRefMeta(meta[metaKeyAnchor])
	if raw, ok := meta[metaKeyAllocator]; ok {
		var n uint64
		if err := json.Unmarshal(raw, &n); err != nil {
			return st, errors.New("security store chain allocator metadata is malformed")
		}
		st.Allocator = n
	}
	return st, nil
}

func parseChainRefMeta(raw []byte) (chainRef, refState) {
	if raw == nil {
		return chainRef{}, refAbsent
	}
	var ref chainRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return chainRef{}, refMalformed
	}
	// A stored ref that parses but is zero-valued (null, {}, or
	// {"seq":0,"hash":""}) is malformed, exactly as the bbolt
	// backend classifies it: no legitimate write path ever stores a
	// zero ref (heads/anchors always carry Seq>=1 and a real hash).
	// Accepting it let a zeroed metadata row pass chain
	// verification as an "emptied" log and fork the audit chain on
	// the next append on the Postgres/Redis backends.
	if ref.Hash == "" || ref.Seq == 0 {
		return chainRef{}, refMalformed
	}
	return ref, refValid
}

func applyPlanTx(ctx context.Context, tx *sql.Tx, plan appendPlan) error {
	for _, seq := range plan.PrunedSeqs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+securityAuditTable+` WHERE seq = $1`, seq); err != nil {
			return fmt.Errorf("securitystore: prune audit record: %w", err)
		}
	}
	insert := true
	for _, seq := range plan.PrunedSeqs {
		if seq == plan.Event.Seq {
			insert = false
		}
	}
	if insert {
		encoded, err := json.Marshal(plan.Event)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+securityAuditTable+` (seq, record) VALUES ($1, $2)`, plan.Event.Seq, encoded); err != nil {
			return fmt.Errorf("securitystore: insert audit record: %w", err)
		}
	}
	if err := putMetaTx(ctx, tx, metaKeyHead, plan.Head); err != nil {
		return err
	}
	if plan.HasAnchor {
		if err := putMetaTx(ctx, tx, metaKeyAnchor, plan.Anchor); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(plan.Allocator)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+securityMetaTable+` (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, metaKeyAllocator, encoded); err != nil {
		return fmt.Errorf("securitystore: store allocator: %w", err)
	}
	return nil
}

func putMetaTx(ctx context.Context, tx *sql.Tx, key string, ref chainRef) error {
	encoded, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+securityMetaTable+` (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key, encoded); err != nil {
		return fmt.Errorf("securitystore: store chain metadata: %w", err)
	}
	return nil
}

// appendAuditInTx verifies, plans and applies one audit append inside
// an already-locked writer transaction.
func (s *PostgresStore) appendAuditInTx(ctx context.Context, tx *sql.Tx, event AuditEvent) error {
	st, err := loadChainTx(ctx, tx)
	if err != nil {
		return err
	}
	plan, err := planAuditAppend(st, event, s.maxAuditItems, s.retention, time.Now())
	if err != nil {
		return err
	}
	return applyPlanTx(ctx, tx, plan)
}

func (s *PostgresStore) AppendAudit(ctx context.Context, event AuditEvent) error {
	return s.withWriterTx(ctx, func(tx *sql.Tx) error {
		return s.appendAuditInTx(ctx, tx, event)
	})
}

func (s *PostgresStore) AuditRecords(ctx context.Context, limit int) ([]AuditEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 {
		limit = 100
	}
	if limit > s.maxAuditItems {
		limit = s.maxAuditItems
	}
	rows, err := s.db.QueryContext(ctx, `SELECT record FROM `+securityAuditTable+` ORDER BY seq DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("securitystore: list audit records: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]AuditEvent, 0, limit)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("securitystore: scan audit record: %w", err)
		}
		var event AuditEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return nil, errors.New("security store contains a malformed audit record")
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

func (s *PostgresStore) VerifyAuditChain(ctx context.Context) (ChainReport, error) {
	if err := ctx.Err(); err != nil {
		return ChainReport{}, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ChainReport{}, fmt.Errorf("securitystore: begin verify: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	st, err := loadChainTx(ctx, tx)
	if err != nil {
		return ChainReport{}, err
	}
	return verifyChainState(st), nil
}

// prepareSession applies the bbolt store's session validation,
// sanitization and timestamp defaults.
func prepareSession(value Session) ([]byte, error) {
	if strings.TrimSpace(value.Subject) == "" || strings.TrimSpace(value.Issuer) == "" || value.ExpiresAt.IsZero() {
		return nil, errors.New("session subject, issuer and expiry are required")
	}
	value.Subject = Sanitize(value.Subject, 200)
	value.Issuer = Sanitize(value.Issuer, 512)
	value.Roles = cleanValues(value.Roles, 16, 32)
	if value.CreatedAt.IsZero() {
		value.CreatedAt = time.Now().UTC()
	}
	if value.LastSeen.IsZero() {
		value.LastSeen = value.CreatedAt
	}
	return json.Marshal(value)
}

func (s *PostgresStore) CreateSession(ctx context.Context, hash string, value Session, event AuditEvent) error {
	return s.RotateSession(ctx, "", hash, value, event)
}

func (s *PostgresStore) RotateSession(ctx context.Context, oldHash, hash string, value Session, event AuditEvent) error {
	if _, err := validSessionHash(hash); err != nil {
		return err
	}
	if oldHash != "" {
		if _, err := validSessionHash(oldHash); err != nil {
			return err
		}
	}
	encoded, err := prepareSession(value)
	if err != nil {
		return err
	}
	return s.withWriterTx(ctx, func(tx *sql.Tx) error {
		if oldHash != "" {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+securitySessionsTable+` WHERE hash = $1`, oldHash); err != nil {
				return fmt.Errorf("securitystore: delete rotated session: %w", err)
			}
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+securitySessionsTable+` WHERE hash = $1)`, hash).Scan(&exists); err != nil {
			return fmt.Errorf("securitystore: check session: %w", err)
		}
		if exists {
			return errors.New("session identifier collision")
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+securitySessionsTable+` (hash, value) VALUES ($1, $2)`, hash, encoded); err != nil {
			return fmt.Errorf("securitystore: insert session: %w", err)
		}
		return s.appendAuditInTx(ctx, tx, event)
	})
}

func (s *PostgresStore) Session(ctx context.Context, hash string) (Session, bool, error) {
	if _, err := validSessionHash(hash); err != nil {
		return Session{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return Session{}, false, err
	}
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM `+securitySessionsTable+` WHERE hash = $1`, hash).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("securitystore: load session: %w", err)
	}
	var value Session
	if err := json.Unmarshal(raw, &value); err != nil {
		return Session{}, false, errors.New("security store contains a malformed session record")
	}
	value.Roles = append([]string(nil), value.Roles...)
	return value, true, nil
}

func (s *PostgresStore) TouchSession(ctx context.Context, hash string, now time.Time, idleTimeout time.Duration) (Session, bool, error) {
	if _, err := validSessionHash(hash); err != nil {
		return Session{}, false, err
	}
	if idleTimeout <= 0 {
		return Session{}, false, errors.New("session idle timeout must be positive")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var value Session
	found := false
	err := s.withWriterTx(ctx, func(tx *sql.Tx) error {
		var raw []byte
		err := tx.QueryRowContext(ctx, `SELECT value FROM `+securitySessionsTable+` WHERE hash = $1 FOR UPDATE`, hash).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("securitystore: load session: %w", err)
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			return errors.New("security store contains a malformed session record")
		}
		if !now.Before(value.ExpiresAt) || now.Sub(value.LastSeen) >= idleTimeout {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+securitySessionsTable+` WHERE hash = $1`, hash); err != nil {
				return fmt.Errorf("securitystore: delete expired session: %w", err)
			}
			return nil
		}
		value.LastSeen = now.UTC()
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE `+securitySessionsTable+` SET value = $2 WHERE hash = $1`, hash, encoded); err != nil {
			return fmt.Errorf("securitystore: touch session: %w", err)
		}
		value.Roles = append([]string(nil), value.Roles...)
		found = true
		return nil
	})
	return value, found, err
}

func (s *PostgresStore) RevokeSession(ctx context.Context, hash string, event AuditEvent) error {
	if _, err := validSessionHash(hash); err != nil {
		return err
	}
	return s.withWriterTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+securitySessionsTable+` WHERE hash = $1`, hash); err != nil {
			return fmt.Errorf("securitystore: revoke session: %w", err)
		}
		return s.appendAuditInTx(ctx, tx, event)
	})
}

func (s *PostgresStore) Close() error {
	return s.db.Close()
}
