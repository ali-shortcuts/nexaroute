// Package securitystore provides the durable local security store used for
// opaque admin sessions and structured audit records. The bbolt database is
// transactionally safe across processes on one host; it is not a distributed
// database and must not be shared between hosts over a network filesystem.
package securitystore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	keyChainHead   = []byte("audit-chain-head")
	keyChainAnchor = []byte("audit-chain-anchor")
)

// chainRef identifies one record in the audit hash chain by its
// monotonic sequence and the record's chain hash.
type chainRef struct {
	Seq  uint64 `json:"seq"`
	Hash string `json:"hash"`
}

// canonicalAuditHash is the versioned chain hash of one audit record.
// The encoding is a domain separator followed by length-prefixed
// fields in a fixed order, so serialization is deterministic and
// unambiguous. The record's own Hash field is excluded; Seq and
// PrevHash are included, which binds linkage into every record.
func canonicalAuditHash(e AuditEvent) string {
	h := sha256.New()
	h.Write([]byte("nexaroute-audit-chain-v1"))
	field := func(s string) {
		fmt.Fprintf(h, "|%d:%s", len(s), s)
	}
	field(strconv.FormatUint(e.Seq, 10))
	field(e.PrevHash)
	field(e.Timestamp.UTC().Format(time.RFC3339Nano))
	field(e.RequestID)
	field(e.Actor)
	field(e.Action)
	field(e.Target)
	field(e.Method)
	field(e.Path)
	field(e.Outcome)
	field(strconv.Itoa(e.Status))
	field(e.Permission)
	field(e.Authn)
	return hex.EncodeToString(h.Sum(nil))
}

// recordHash returns the chain hash of a stored record: its stored
// Hash when chained, or the canonical hash computed from its fields
// for a legacy (pre-chain) record, whose sequence is taken from its
// storage key.
func recordHash(e AuditEvent, keySeq uint64) string {
	if e.Hash != "" {
		return e.Hash
	}
	legacy := e
	legacy.Seq = keySeq
	legacy.PrevHash = ""
	return canonicalAuditHash(legacy)
}

// bootstrapPrevHash finds the chain predecessor for the first
// chained append to a database that has no persisted chain head yet
// (a pre-chain database, or one whose head metadata was never
// written). The predecessor is the stored record with the HIGHEST
// sequence — storage keys sort by timestamp first, so the last key
// in key order is not necessarily the newest record in chain order.
func bootstrapPrevHash(audit *bolt.Bucket) (string, bool, error) {
	var bestKey []byte
	var bestRaw []byte
	var bestSeq uint64
	c := audit.Cursor()
	for k, raw := c.First(); k != nil; k, raw = c.Next() {
		if len(k) != 16 {
			return "", false, errors.New("security store contains an invalid audit key")
		}
		if seq := auditKeySeq(k); bestKey == nil || seq > bestSeq {
			bestKey = append(bestKey[:0], k...)
			bestRaw = append(bestRaw[:0], raw...)
			bestSeq = seq
		}
	}
	if bestKey == nil {
		return "", false, nil
	}
	var prev AuditEvent
	if err := json.Unmarshal(bestRaw, &prev); err != nil {
		return "", false, errors.New("security store contains a malformed audit record")
	}
	return recordHash(prev, bestSeq), true, nil
}

func auditKeySeq(key []byte) uint64 {
	if len(key) != 16 {
		return 0
	}
	return binary.BigEndian.Uint64(key[8:])
}

func loadChainRef(meta *bolt.Bucket, key []byte) (chainRef, bool) {
	raw := meta.Get(key)
	if raw == nil {
		return chainRef{}, false
	}
	var ref chainRef
	if err := json.Unmarshal(raw, &ref); err != nil || ref.Hash == "" {
		return chainRef{}, false
	}
	return ref, true
}

func storeChainRef(meta *bolt.Bucket, key []byte, ref chainRef) error {
	raw, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	return meta.Put(key, raw)
}

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
	// Chain fields (M3-6). Records written before the hash chain was
	// introduced have all three empty/zero and form the unverified
	// legacy segment; see VerifyAuditChain.
	Seq      uint64 `json:"seq,omitempty"`
	PrevHash string `json:"prev_hash,omitempty"`
	Hash     string `json:"hash,omitempty"`
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

// OpenReadOnly opens an existing security store without modifying
// it (bbolt read-only mode; no directory or bucket creation). The
// same file and directory permission rules as Open are enforced.
// Intended for offline verification tooling such as
// `nexaroute audit verify`.
func OpenReadOnly(path string) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("security store path is required")
	}
	directory := filepath.Dir(path)
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
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect security store: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return nil, errors.New("security store must be a regular file, not a symlink")
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("security store permissions must be 0600; fix with chmod 600 %s", path)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open security store read-only: %w", err)
	}
	return &Store{db: db, maxAuditItems: 1_000_000, retention: 0}, nil
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

// ChainReport is the result of VerifyAuditChain. Legacy records
// (written before the hash chain existed) are counted separately and
// are never reported as verified.
type ChainReport struct {
	TotalRecords   int
	LegacyRecords  int
	ChainedRecords int
	FirstSeq       uint64
	LastSeq        uint64
	HeadHash       string
	AnchorSeq      uint64
	HasAnchor      bool
	Verified       bool
	Problems       []string
}

// VerifyAuditChain revalidates every stored audit record: each
// chained record's hash is recomputed, linkage to its predecessor is
// checked, sequences must be strictly increasing without duplicates,
// the first surviving record must link to the pruning anchor when one
// exists, and the persisted chain head must match the newest
// surviving record (which detects tail truncation). Verification is
// read-only.
func (s *Store) VerifyAuditChain(ctx context.Context) (ChainReport, error) {
	if err := ctx.Err(); err != nil {
		return ChainReport{}, err
	}
	report := ChainReport{Verified: true}
	problem := func(format string, args ...any) {
		report.Verified = false
		report.Problems = append(report.Problems, fmt.Sprintf(format, args...))
	}
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		meta := tx.Bucket(bucketMeta)
		audit := tx.Bucket(bucketAudit)
		// Fail closed on missing storage structures: a database whose
		// audit or meta bucket is absent has lost integrity evidence
		// and must never report VERIFIED. (A database with both
		// buckets present but zero records is a legitimate empty
		// history; pre-chain databases always have both buckets
		// because Open creates them.)
		if audit == nil {
			problem("audit bucket (audit-v1) is missing; the audit history and its integrity evidence are absent")
		}
		if meta == nil {
			problem("meta bucket (meta-v1) is missing; chain head and pruning anchor evidence are absent")
		}
		anchor, hasAnchor := chainRef{}, false
		if meta != nil {
			anchor, hasAnchor = loadChainRef(meta, keyChainAnchor)
		}
		report.HasAnchor = hasAnchor
		if hasAnchor {
			report.AnchorSeq = anchor.Seq
		}
		if audit == nil {
			return nil
		}
		type stored struct {
			event AuditEvent
			key   []byte
		}
		var records []stored
		c := audit.Cursor()
		for k, raw := c.First(); k != nil; k, raw = c.Next() {
			report.TotalRecords++
			if len(k) != 16 {
				problem("audit record with key %x has an invalid storage key length %d (want 16)", k, len(k))
				continue
			}
			var event AuditEvent
			if err := json.Unmarshal(raw, &event); err != nil {
				problem("audit record with key %x is malformed and cannot be verified", k)
				continue
			}
			keyCopy := make([]byte, len(k))
			copy(keyCopy, k)
			records = append(records, stored{event: event, key: keyCopy})
		}
		// Chain order is sequence order: stored Seq for chained
		// records, key sequence for legacy records.
		seqOf := func(r stored) uint64 {
			if r.event.Hash != "" {
				return r.event.Seq
			}
			return auditKeySeq(r.key)
		}
		sort.SliceStable(records, func(i, j int) bool { return seqOf(records[i]) < seqOf(records[j]) })
		var prev *stored
		var prevSeq uint64
		for i := range records {
			r := records[i]
			seq := seqOf(r)
			if prev != nil && seq <= prevSeq {
				problem("audit sequence %d is duplicated or out of order", seq)
			}
			// Storage-key consistency: the key's timestamp must
			// match the record's timestamp at the storage format's
			// nanosecond precision, and for chained records the
			// key's sequence must match the record's own sequence.
			// (Legacy records carry no stored sequence; their key
			// sequence is authoritative by definition.)
			if keyTs := int64(binary.BigEndian.Uint64(r.key[:8])); keyTs != r.event.Timestamp.UnixNano() {
				problem("audit record seq %d has a storage key timestamp that does not match its record timestamp (key corruption)", seq)
			}
			if r.event.Hash != "" && r.event.Seq != auditKeySeq(r.key) {
				problem("audit record seq %d has a storage key sequence %d that does not match its record sequence (key corruption)", r.event.Seq, auditKeySeq(r.key))
			}
			if r.event.Hash == "" {
				report.LegacyRecords++
				prev = &records[i]
				prevSeq = seq
				continue
			}
			report.ChainedRecords++
			if got := canonicalAuditHash(r.event); got != r.event.Hash {
				problem("audit record seq %d was modified after it was written (hash mismatch)", seq)
			}
			switch {
			case prev != nil:
				if want := recordHash(prev.event, auditKeySeq(prev.key)); r.event.PrevHash != want {
					problem("audit record seq %d does not link to its predecessor (history was deleted, inserted, or reordered)", seq)
				}
			case hasAnchor:
				if r.event.PrevHash != anchor.Hash {
					problem("first surviving audit record seq %d does not link to the pruning anchor seq %d (history before the retained window was altered)", seq, anchor.Seq)
				}
				if seq != anchor.Seq+1 && report.LegacyRecords == 0 {
					problem("gap between pruning anchor seq %d and first surviving audit record seq %d", anchor.Seq, seq)
				}
			default:
				if r.event.PrevHash != "" {
					problem("first chained audit record seq %d links to a predecessor that does not exist and no pruning anchor is recorded", seq)
				}
			}
			prev = &records[i]
			prevSeq = seq
		}
		if len(records) > 0 {
			report.FirstSeq = seqOf(records[0])
			last := records[len(records)-1]
			report.LastSeq = seqOf(last)
			report.HeadHash = recordHash(last.event, auditKeySeq(last.key))
		}
		if meta != nil {
			if head, ok := loadChainRef(meta, keyChainHead); ok {
				switch {
				case len(records) == 0:
					// An emptied bucket keeps the head at the anchor.
					if !hasAnchor || head != anchor {
						problem("chain head metadata does not match the pruning anchor for an empty audit log")
					}
					report.HeadHash = head.Hash
				case head.Seq != report.LastSeq || head.Hash != report.HeadHash:
					problem("chain head metadata (seq %d) does not match the newest surviving audit record (seq %d); the newest records may have been truncated", head.Seq, report.LastSeq)
				}
			} else if report.ChainedRecords > 0 {
				problem("audit records are chained but chain head metadata is missing")
			}
		}
		return nil
	})
	if err != nil {
		return ChainReport{}, err
	}
	return report, nil
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
	meta := tx.Bucket(bucketMeta)
	seq, err := meta.NextSequence()
	if err != nil {
		return AuditEvent{}, err
	}
	audit := tx.Bucket(bucketAudit)
	// Resolve the chain predecessor before inserting: the persisted
	// chain head when present; otherwise the newest stored record
	// (bootstrap for pre-chain databases, where a legacy record's hash
	// is computed from its stored fields and key sequence); otherwise
	// the genesis empty hash.
	event.Seq = seq
	if head, ok := loadChainRef(meta, keyChainHead); ok {
		event.PrevHash = head.Hash
	} else if prevHash, found, err := bootstrapPrevHash(audit); err != nil {
		return AuditEvent{}, err
	} else if found {
		event.PrevHash = prevHash
	} else {
		event.PrevHash = ""
	}
	event.Hash = canonicalAuditHash(event)
	encoded, err := json.Marshal(event)
	if err != nil {
		return AuditEvent{}, err
	}
	key := make([]byte, 16)
	binary.BigEndian.PutUint64(key[:8], uint64(event.Timestamp.UnixNano()))
	binary.BigEndian.PutUint64(key[8:], seq)
	if err := audit.Put(key, encoded); err != nil {
		return AuditEvent{}, err
	}
	// Pruning deletes a prefix of the chain in sequence order (oldest
	// sequences first) whose records are over the retention age or
	// beyond the count bound. Restricting pruning to a sequence
	// prefix is what keeps the surviving chain verifiable: the chain
	// anchor records the newest pruned record, and the first survivor
	// always links directly to it. A backdated record can therefore
	// outlive its retention age when a lower-sequence record still
	// qualifies for retention; the count bound is always enforced.
	type keyInfo struct {
		key []byte
		seq uint64
		ts  int64
	}
	var infos []keyInfo
	c := audit.Cursor()
	for k, _ := c.First(); k != nil; k, _ = c.Next() {
		if len(k) != 16 {
			return AuditEvent{}, errors.New("security store contains an invalid audit key")
		}
		keyCopy := make([]byte, 16)
		copy(keyCopy, k)
		infos = append(infos, keyInfo{key: keyCopy, seq: auditKeySeq(k), ts: int64(binary.BigEndian.Uint64(k[:8]))})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].seq < infos[j].seq })
	cutoff := time.Now().Add(-s.retention).UnixNano()
	toDelete := 0
	for toDelete < len(infos) {
		overAge := infos[toDelete].ts < cutoff
		overCount := len(infos)-toDelete > s.maxAuditItems
		if !overAge && !overCount {
			break
		}
		toDelete++
	}
	var pruned *chainRef
	for _, info := range infos[:toDelete] {
		raw := audit.Get(info.key)
		var old AuditEvent
		if err := json.Unmarshal(raw, &old); err != nil {
			return AuditEvent{}, errors.New("security store contains a malformed audit record")
		}
		ref := chainRef{Seq: info.seq, Hash: recordHash(old, info.seq)}
		pruned = &ref
		if err := audit.Delete(info.key); err != nil {
			return AuditEvent{}, err
		}
	}
	if pruned != nil {
		if err := storeChainRef(meta, keyChainAnchor, *pruned); err != nil {
			return AuditEvent{}, err
		}
	}
	// The head names the chain tail in sequence order: the record just
	// appended (appends are serialized by the bbolt write
	// transaction), or the anchor when pruning removed every record
	// including it.
	head := chainRef{Seq: event.Seq, Hash: event.Hash}
	if toDelete == len(infos) && pruned != nil {
		head = *pruned
	}
	if err := storeChainRef(meta, keyChainHead, head); err != nil {
		return AuditEvent{}, err
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
