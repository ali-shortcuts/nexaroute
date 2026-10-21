// Package sharedstate implements the smallest safe shared circuit-breaker
// state slice. It persists bounded cooldown/retired markers as a versioned
// revision-checked document. It does not wire itself into routing or health
// state: callers remain responsible for applying returned markers and for
// keeping local state when the store is unavailable.
package sharedstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	// BreakerSchemaVersion is the only document schema this slice writes and
	// accepts. Unknown schemas are rejected rather than interpreted.
	BreakerSchemaVersion = 1

	// BreakerNamespace is the control-plane namespace for breaker markers.
	// The namespace is additive; existing control-plane documents are not
	// migrated or modified.
	BreakerNamespace = "nexaroute:breaker/v1"

	// DefaultMaxCooldown bounds every persisted cooldown deadline. It matches
	// the gateway's default routing cooldown and prevents a replica with a
	// skewed clock from excluding a deployment indefinitely.
	DefaultMaxCooldown = 30 * time.Minute

	// DefaultMaxDocumentBytes and DefaultMaxEntries bound a decoded document
	// so one poisoned writer cannot force unbounded allocation or routing work.
	DefaultMaxDocumentBytes = 1 << 20
	DefaultMaxEntries       = 10000

	// DefaultRetries is the number of conflict retries after the initial
	// compare-and-swap attempt.
	DefaultRetries = 3
)

// EntryStatus is the allow-listed status vocabulary for shared markers. Only
// terminal-or-bounded states cross the store boundary; healthy/degraded/
// half-open/unknown states stay local because they flap at request cadence.
type EntryStatus string

const (
	StatusCooldown EntryStatus = "cooldown"
	StatusRetired  EntryStatus = "retired"
)

// Entry is one deployment's shared marker. Entries are keyed by deployment ID
// in Document; the deployment ID is deliberately not duplicated in the value.
type Entry struct {
	Status        EntryStatus `json:"status"`
	CooldownUntil time.Time   `json:"cooldown_until,omitempty"`
	ReasonClass   string      `json:"reason_class,omitempty"`
	OriginReplica string      `json:"origin_replica,omitempty"`
	ObservedAt    time.Time   `json:"observed_at,omitempty"`
}

type wireDocument struct {
	Schema  int                        `json:"schema"`
	Entries map[string]json.RawMessage `json:"entries"`
}

type document struct {
	Schema  int              `json:"schema"`
	Entries map[string]Entry `json:"entries"`
}

// ErrInvalidEntry reports a local marker that cannot be published. Remote
// malformed entries are skipped entry-wise instead; see decodeDocument.
var ErrInvalidEntry = errors.New("invalid shared breaker entry")

// NormalizeEntry validates one entry and applies the cooldown cap. It returns
// false for an entry that has already expired. It is exported so callers can
// validate local markers before deciding to publish them.
func NormalizeEntry(entry Entry, now time.Time, maxCooldown time.Duration) (Entry, bool, error) {
	if now.IsZero() {
		return Entry{}, false, fmt.Errorf("%w: zero observation time", ErrInvalidEntry)
	}
	switch entry.Status {
	case StatusRetired:
		entry.CooldownUntil = time.Time{}
		return entry, true, nil
	case StatusCooldown:
		if entry.CooldownUntil.IsZero() {
			return Entry{}, false, fmt.Errorf("%w: cooldown has no deadline", ErrInvalidEntry)
		}
		entry.CooldownUntil = entry.CooldownUntil.UTC()
		if !entry.CooldownUntil.After(now) {
			return Entry{}, false, nil
		}
		if maxCooldown > 0 {
			cap := now.Add(maxCooldown)
			if entry.CooldownUntil.After(cap) {
				entry.CooldownUntil = cap
			}
		}
		return entry, true, nil
	default:
		return Entry{}, false, fmt.Errorf("%w: unknown status %q", ErrInvalidEntry, entry.Status)
	}
}

// Merge returns the monotonic union of current and incoming markers. It
// prunes expired entries, skips invalid entries, clamps cooldown deadlines to
// now+maxCooldown, keeps Retired sticky, and keeps the later cooldown deadline.
// An absent or failed remote view therefore never clears a local marker.
func Merge(current, incoming map[string]Entry, now time.Time, maxCooldown time.Duration) map[string]Entry {
	out := make(map[string]Entry, len(current)+len(incoming))
	apply := func(source map[string]Entry) {
		for deployment, entry := range source {
			if deployment == "" {
				continue
			}
			normalized, ok, _ := NormalizeEntry(entry, now, maxCooldown)
			if !ok {
				continue
			}
			existing, exists := out[deployment]
			if !exists {
				out[deployment] = normalized
				continue
			}
			out[deployment] = mergeEntry(existing, normalized)
		}
	}
	apply(current)
	apply(incoming)
	return out
}

func mergeEntry(current, incoming Entry) Entry {
	if current.Status == StatusRetired {
		return current
	}
	if incoming.Status == StatusRetired {
		return incoming
	}
	if incoming.CooldownUntil.After(current.CooldownUntil) {
		return incoming
	}
	return current
}

func encodeDocument(entries map[string]Entry, maxEntries, maxBytes int) ([]byte, error) {
	if entries == nil {
		entries = map[string]Entry{}
	}
	if maxEntries > 0 && len(entries) > maxEntries {
		return nil, fmt.Errorf("shared breaker document has %d entries, limit is %d", len(entries), maxEntries)
	}
	payload, err := json.Marshal(document{Schema: BreakerSchemaVersion, Entries: entries})
	if err != nil {
		return nil, err
	}
	if maxBytes > 0 && len(payload) > maxBytes {
		return nil, fmt.Errorf("shared breaker document is %d bytes, limit is %d", len(payload), maxBytes)
	}
	return payload, nil
}

func decodeDocument(payload []byte, maxEntries, maxBytes int) (map[string]Entry, error) {
	if maxBytes > 0 && len(payload) > maxBytes {
		return nil, fmt.Errorf("shared breaker document is %d bytes, limit is %d", len(payload), maxBytes)
	}
	var raw wireDocument
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("decode shared breaker document: %w", err)
	}
	if raw.Schema != BreakerSchemaVersion {
		return nil, fmt.Errorf("unsupported shared breaker schema %d", raw.Schema)
	}
	if maxEntries > 0 && len(raw.Entries) > maxEntries {
		return nil, fmt.Errorf("shared breaker document has %d entries, limit is %d", len(raw.Entries), maxEntries)
	}
	out := make(map[string]Entry, len(raw.Entries))
	for deployment, value := range raw.Entries {
		if deployment == "" {
			continue
		}
		var entry Entry
		if err := json.Unmarshal(value, &entry); err != nil {
			continue
		}
		if entry.Status != StatusCooldown && entry.Status != StatusRetired {
			continue
		}
		if entry.Status == StatusCooldown && entry.CooldownUntil.IsZero() {
			continue
		}
		out[deployment] = entry
	}
	return out, nil
}

func entriesEqual(a, b map[string]Entry) bool {
	if len(a) != len(b) {
		return false
	}
	for key, left := range a {
		right, ok := b[key]
		if !ok || left.Status != right.Status ||
			!left.CooldownUntil.Equal(right.CooldownUntil) ||
			left.ReasonClass != right.ReasonClass ||
			left.OriginReplica != right.OriginReplica ||
			!left.ObservedAt.Equal(right.ObservedAt) {
			return false
		}
	}
	return true
}

func cloneEntries(entries map[string]Entry) map[string]Entry {
	if entries == nil {
		return map[string]Entry{}
	}
	out := make(map[string]Entry, len(entries))
	for key, entry := range entries {
		out[key] = entry
	}
	return out
}
