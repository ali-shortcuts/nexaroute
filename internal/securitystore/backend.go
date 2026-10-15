package securitystore

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// Backend is the runtime-selectable contract for the admin security store:
// admin sessions and the tamper-evident audit chain. The zero-service
// default implementation is the local bbolt Store in this package.
// Redis/PostgreSQL adapters (M4-2) must satisfy this same interface and
// pass the shared contract suite in backend_contract_test.go against the
// real services before they may be selected in a release build.
//
// State classification: only sessions and audit records live behind this
// interface. Rate-limit buckets, provider health, caches and in-flight
// quota reservations remain process-local by design (see
// specs/019-m4-1-storage-backend).
type Backend interface {
	CreateSession(ctx context.Context, hash string, value Session, event AuditEvent) error
	RotateSession(ctx context.Context, oldHash, hash string, value Session, event AuditEvent) error
	Session(ctx context.Context, hash string) (Session, bool, error)
	TouchSession(ctx context.Context, hash string, now time.Time, idleTimeout time.Duration) (Session, bool, error)
	RevokeSession(ctx context.Context, hash string, event AuditEvent) error
	AppendAudit(ctx context.Context, event AuditEvent) error
	// AuditRecords returns at most limit records, newest first.
	AuditRecords(ctx context.Context, limit int) ([]AuditEvent, error)
	VerifyAuditChain(ctx context.Context) (ChainReport, error)
	Close() error
}

// The local bbolt store is the default Backend.
var _ Backend = (*Store)(nil)

// Supported backend names for admin.security_store_backend.
const (
	BackendBBolt    = "bbolt"
	BackendRedis    = "redis"
	BackendPostgres = "postgres"
)

// ValidBackend reports whether name is a recognized backend selection
// (the empty string selects the default).
func ValidBackend(name string) bool {
	switch name {
	case "", BackendBBolt, BackendRedis, BackendPostgres:
		return true
	}
	return false
}

// OpenBackend opens the security store for the selected backend.
//
// Fail-closed selection semantics: an adapter that cannot be
// constructed (bad or unreachable connection string, unknown name) is
// an explicit error, never a silent fallback to a local or in-memory
// store — an operator who configured a durable shared backend must
// learn at startup that it is unavailable, not discover later that
// sessions and audit records went somewhere else.
//
// The connection string may be given literally as security_store_url
// or indirectly as env:VARIABLE_NAME, in which case it is read from
// that environment variable (the M4-1 pattern: secrets stay out of
// durable config).
func OpenBackend(backend, url, path string, maxAuditItems, retentionDays int) (Backend, error) {
	switch strings.TrimSpace(backend) {
	case "", BackendBBolt:
		if strings.TrimSpace(url) != "" {
			return nil, fmt.Errorf("securitystore: security_store_url must be empty for the bbolt backend")
		}
		return Open(path, maxAuditItems, retentionDays)
	case BackendPostgres:
		resolved, err := resolveStoreURL(url, backend)
		if err != nil {
			return nil, err
		}
		return OpenPostgres(context.Background(), resolved, maxAuditItems, retentionDays)
	case BackendRedis:
		resolved, err := resolveStoreURL(url, backend)
		if err != nil {
			return nil, err
		}
		return OpenRedis(context.Background(), resolved, maxAuditItems, retentionDays)
	default:
		return nil, fmt.Errorf("securitystore: unknown security_store_backend %q", backend)
	}
}

// resolveStoreURL resolves the env:VARIABLE_NAME indirection for a
// backend connection string.
func resolveStoreURL(url, backend string) (string, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return "", fmt.Errorf("securitystore: security_store_url is required for the %s backend", backend)
	}
	if name, ok := strings.CutPrefix(url, "env:"); ok {
		name = strings.TrimSpace(name)
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			return "", fmt.Errorf("securitystore: environment variable %s named by security_store_url is unset or empty", name)
		}
		return value, nil
	}
	return url, nil
}
