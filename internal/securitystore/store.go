// Package securitystore provides the durable local security store used for
// opaque admin sessions and structured audit records. The bbolt database is
// transactionally safe across processes on one host; it is not a distributed
// database and must not be shared between hosts over a network filesystem.
package securitystore

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	bolt "go.etcd.io/bbolt"
)

var (
	ErrInvalidSession = errors.New("invalid session identifier")
	ErrInvalidAudit   = errors.New("invalid audit event")
)

var (
	bucketSessions = []byte("sessions-v1")
	bucketAudit    = []byte("audit-v1")
	bucketMeta     = []byte("meta-v1")
	keyAuditSeq    = []byte("audit-sequence")
)

type Session struct {
	Subject    string    `json:"subject"`
	Issuer     string    `json:"issuer"`
	Roles      []string  `json:"roles"`
	PolicyHash string    `json:"policy_hash"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeen   time.Time `json:"last_seen"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// AuditEvent intentionally has no fields for request bodies, tokens, cookies,
// authorization codes, or arbitrary claims. Values are sanitized again at the
// storage boundary so callers cannot accidentally persist control characters.
type AuditEvent struct {
	Timestamp  time.Time `json:"timestamp"`
	RequestID  string    `json:"request_id"`
	Actor      string    `json:"actor"`
	Action     string    `json:"action"`
	Target     string    `json:"target"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Outcome    string    `json:"outcome"`
	Status     int       `json:"status"`
	Permission string    `json:"permission,omitempty"`
	Authn      string    `json:"authentication,omitempty"`
}

type Store struct {
	db            *bolt.DB
	maxAuditItems int
	retention     time.Duration
}

// Open creates or opens a private bbolt database. The database itself is
// chmod'd to 0600 on creation and refuses a pre-existing group/world-readable
// file. Every successful write is a bbolt transaction with the default fsync
// behavior enabled.
func Open(path string, maxAuditItems, retentionDays int) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("security store path is required")
	}
	if maxAuditItems < 100 || maxAuditItems > 1_000_000 {
		return nil, errors.New("security audit max_events must be between 100 and 1000000")
	}
	if retentionDays < 1 || retentionDays > 3650 {
		return nil, errors.New("security audit retention_days must be between 1 and 3650")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create security store directory: %w", err)
	}
	dirInfo, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect security store directory: %w", err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 || !dirInfo.IsDir() {
		return nil, errors.New("security store parent must be a real directory, not a symlink")
	}
	if dirInfo.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("security store directory permissions must be 0700; fix with chmod 700 %s", directory)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			return nil, errors.New("security store must be a regular file, not a symlink")
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("security store permissions must be 0600; fix with chmod 600 %s", path)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect security store: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open security store: %w", err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketSessions, bucketAudit, bucketMeta} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize security store: %w", err)
	}
	return &Store{db: db, maxAuditItems: maxAuditItems, retention: time.Duration(retentionDays) * 24 * time.Hour}, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func validSessionHash(hash string) ([]byte, error) {
	if len(hash) != 64 {
		return nil, ErrInvalidSession
	}
	b, err := hex.DecodeString(hash)
	if err != nil || len(b) != 32 {
		return nil, ErrInvalidSession
	}
	return b, nil
}

func (s *Store) CreateSession(ctx context.Context, hash string, value Session, event AuditEvent) error {
	return s.RotateSession(ctx, "", hash, value, event)
}

// RotateSession atomically replaces a previous opaque session identifier,
// persists the new server-side record, and writes the login event.
func (s *Store) RotateSession(ctx context.Context, oldHash, hash string, value Session, event AuditEvent) error {
	key, err := validSessionHash(hash)
	if err != nil {
		return err
	}
	var oldKey []byte
	if oldHash != "" {
		oldKey, err = validSessionHash(oldHash)
		if err != nil {
			return err
		}
	}
	if strings.TrimSpace(value.Subject) == "" || strings.TrimSpace(value.Issuer) == "" || value.ExpiresAt.IsZero() {
		return errors.New("session subject, issuer and expiry are required")
	}
	if err := ctx.Err(); err != nil {
		return err
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
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		sessions := tx.Bucket(bucketSessions)
		if len(oldKey) != 0 {
			if err := sessions.Delete(oldKey); err != nil {
				return err
			}
		}
		if sessions.Get(key) != nil {
			return errors.New("session identifier collision")
		}
		if err := sessions.Put(key, encoded); err != nil {
			return err
		}
		_, err := appendAuditTx(tx, s, event)
		return err
	})
}

func (s *Store) Session(ctx context.Context, hash string) (Session, bool, error) {
	key, err := validSessionHash(hash)
	if err != nil {
		return Session{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return Session{}, false, err
	}
	var value Session
	found := false
	err = s.db.View(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw := tx.Bucket(bucketSessions).Get(key)
		if raw == nil {
			return nil
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			return errors.New("security store contains a malformed session record")
		}
		value.Roles = append([]string(nil), value.Roles...)
		found = true
		return nil
	})
	return value, found, err
}

// TouchSession validates and advances idle time in one write transaction. An
// expired or revoked session is never made live by touching it.
func (s *Store) TouchSession(ctx context.Context, hash string, now time.Time, idleTimeout time.Duration) (Session, bool, error) {
	key, err := validSessionHash(hash)
	if err != nil {
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
	err = s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		b := tx.Bucket(bucketSessions)
		raw := b.Get(key)
		if raw == nil {
			return nil
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			return errors.New("security store contains a malformed session record")
		}
		if !now.Before(value.ExpiresAt) || now.Sub(value.LastSeen) >= idleTimeout {
			if err := b.Delete(key); err != nil {
				return err
			}
			return nil
		}
		value.LastSeen = now.UTC()
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if err := b.Put(key, encoded); err != nil {
			return err
		}
		value.Roles = append([]string(nil), value.Roles...)
		found = true
		return nil
	})
	return value, found, err
}

// RevokeSession atomically revokes an existing session and records the logout
// event, so a successful revocation cannot be left without its audit record.
func (s *Store) RevokeSession(ctx context.Context, hash string, event AuditEvent) error {
	key, err := validSessionHash(hash)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := tx.Bucket(bucketSessions).Delete(key); err != nil {
			return err
		}
		_, err := appendAuditTx(tx, s, event)
		return err
	})
}

func (s *Store) AppendAudit(ctx context.Context, event AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := appendAuditTx(tx, s, event)
		return err
	})
}

func (s *Store) AuditRecords(ctx context.Context, limit int) ([]AuditEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 {
		limit = 100
	}
	if limit > s.maxAuditItems {
		limit = s.maxAuditItems
	}
	out := make([]AuditEvent, 0, limit)
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		c := tx.Bucket(bucketAudit).Cursor()
		for k, raw := c.Last(); k != nil && len(out) < limit; k, raw = c.Prev() {
			var event AuditEvent
			if err := json.Unmarshal(raw, &event); err != nil {
				return errors.New("security store contains a malformed audit record")
			}
			out = append(out, event)
		}
		return nil
	})
	return out, err
}

func appendAuditTx(tx *bolt.Tx, s *Store, event AuditEvent) (AuditEvent, error) {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	} else {
		event.Timestamp = event.Timestamp.UTC()
	}
	event.RequestID = Sanitize(event.RequestID, 128)
	event.Actor = Sanitize(event.Actor, 200)
	event.Action = Sanitize(event.Action, 128)
	event.Target = Sanitize(event.Target, 200)
	event.Method = Sanitize(event.Method, 16)
	event.Path = Sanitize(event.Path, 256)
	event.Outcome = Sanitize(event.Outcome, 64)
	event.Permission = Sanitize(event.Permission, 64)
	event.Authn = Sanitize(event.Authn, 64)
	if event.Actor == "" || event.Action == "" || event.Outcome == "" || event.Status < 0 || event.Status > 599 {
		return AuditEvent{}, ErrInvalidAudit
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return AuditEvent{}, err
	}
	meta := tx.Bucket(bucketMeta)
	seq, err := meta.NextSequence()
	if err != nil {
		return AuditEvent{}, err
	}
	key := make([]byte, 16)
	binary.BigEndian.PutUint64(key[:8], uint64(event.Timestamp.UnixNano()))
	binary.BigEndian.PutUint64(key[8:], seq)
	audit := tx.Bucket(bucketAudit)
	if err := audit.Put(key, encoded); err != nil {
		return AuditEvent{}, err
	}
	cutoff := time.Now().Add(-s.retention).UnixNano()
	count := 0
	c := audit.Cursor()
	for k, _ := c.First(); k != nil; {
		if len(k) != 16 {
			return AuditEvent{}, errors.New("security store contains an invalid audit key")
		}
		stamp := int64(binary.BigEndian.Uint64(k[:8]))
		if stamp >= cutoff {
			break
		}
		if err := c.Delete(); err != nil {
			return AuditEvent{}, err
		}
		k, _ = c.Next()
	}
	count = audit.Stats().KeyN
	for count > s.maxAuditItems {
		k, _ := audit.Cursor().First()
		if k == nil {
			break
		}
		if err := audit.Delete(k); err != nil {
			return AuditEvent{}, err
		}
		count--
	}
	return event, nil
}

func cleanValues(values []string, maxItems, maxLen int) []string {
	out := make([]string, 0, min(len(values), maxItems))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = Sanitize(value, maxLen)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
		if len(out) == maxItems {
			break
		}
	}
	return out
}

// Sanitize removes control characters and bounds UTF-8 text before it reaches
// durable audit storage. JSON encoding escapes the remaining content.
func Sanitize(value string, maxBytes int) string {
	value = strings.TrimSpace(value)
	if maxBytes < 1 {
		return ""
	}
	var b strings.Builder
	b.Grow(min(len(value), maxBytes))
	for _, r := range value {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			continue
		}
		if b.Len()+len(string(r)) > maxBytes {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}
