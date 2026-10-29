package nexaflow

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// DeliveryLedger is the durable exactly-once claim for webhook
// deliveries and schedule slots. Claim returns true only for the first
// claim of a key; every later claim (same process, another process, or
// after a restart) returns false. This is what the #689 in-memory
// WebhookVerifier cache cannot provide: it is lost on restart and
// invisible to other processes.
//
// The ledger claims a delivery, not a run outcome: a delivery whose run
// failed may still be re-sent by the operator through a new delivery
// (new signature/nonce), but the identical delivery is never executed
// twice. A claim is never released: releasing would re-open replay.
type DeliveryLedger interface {
	Claim(ctx context.Context, key string) (bool, error)
}

// MemoryDeliveryLedger is the test-only ledger.
type MemoryDeliveryLedger struct {
	mu   sync.Mutex
	seen map[string]bool
}

func NewMemoryDeliveryLedger() *MemoryDeliveryLedger {
	return &MemoryDeliveryLedger{seen: map[string]bool{}}
}

func (l *MemoryDeliveryLedger) Claim(_ context.Context, key string) (bool, error) {
	if key == "" {
		return false, errors.New("nexaflow: delivery key is empty")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen[key] {
		return false, nil
	}
	l.seen[key] = true
	return true, nil
}

// FileDeliveryLedger claims via atomic O_EXCL file creation in a
// directory shared by every process on this host. It survives restarts
// and is cross-process safe on a local filesystem. It is single-host
// only: it must never be presented as multi-host protection (that is
// the Postgres ledger's job).
type FileDeliveryLedger struct {
	dir string
}

func NewFileDeliveryLedger(dir string) (*FileDeliveryLedger, error) {
	if dir == "" {
		return nil, errors.New("nexaflow: delivery ledger directory is empty")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("nexaflow: create delivery ledger dir: %w", err)
	}
	return &FileDeliveryLedger{dir: dir}, nil
}

func (l *FileDeliveryLedger) Claim(_ context.Context, key string) (bool, error) {
	if key == "" {
		return false, errors.New("nexaflow: delivery key is empty")
	}
	sum := sha256.Sum256([]byte(key))
	name := hex.EncodeToString(sum[:]) + ".claimed"
	path := filepath.Join(l.dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("nexaflow: claim delivery: %w", err)
	}
	_, werr := f.WriteString(key + "\n")
	cerr := f.Close()
	if werr != nil {
		return false, fmt.Errorf("nexaflow: claim delivery: %w", werr)
	}
	if cerr != nil {
		return false, fmt.Errorf("nexaflow: claim delivery: %w", cerr)
	}
	if d, err := os.Open(l.dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return true, nil
}

// PostgresDeliveryLedger claims via a single-row INSERT with
// ON CONFLICT DO NOTHING: the unique primary key is the claim, so the
// claim is atomic across processes and hosts sharing the database.
// Ensure must be called once before Claim (migrations apply it under
// an advisory lock in production wiring; here it is explicit so tests
// control schema setup).
type PostgresDeliveryLedger struct {
	db *sql.DB
}

func NewPostgresDeliveryLedger(db *sql.DB) (*PostgresDeliveryLedger, error) {
	if db == nil {
		return nil, errors.New("nexaflow: postgres delivery ledger requires a database")
	}
	return &PostgresDeliveryLedger{db: db}, nil
}

// EnsurePostgresDeliverySchema creates the deliveries table if absent.
// It is idempotent and safe to call concurrently.
func EnsurePostgresDeliverySchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS nexaroute_workflow_deliveries (
		delivery_key TEXT PRIMARY KEY,
		claimed_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`)
	return err
}

func (l *PostgresDeliveryLedger) Claim(ctx context.Context, key string) (bool, error) {
	if key == "" {
		return false, errors.New("nexaflow: delivery key is empty")
	}
	res, err := l.db.ExecContext(ctx, `INSERT INTO nexaroute_workflow_deliveries (delivery_key) VALUES ($1) ON CONFLICT (delivery_key) DO NOTHING`, key)
	if err != nil {
		return false, fmt.Errorf("nexaflow: claim delivery: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("nexaflow: claim delivery: %w", err)
	}
	return n == 1, nil
}
