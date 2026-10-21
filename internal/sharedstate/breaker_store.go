package sharedstate

import (
	"context"
	"errors"
	"time"
)

// Options configures a BreakerStore. The zero value is valid and selects the
// breaker namespace, a 30-minute cooldown cap, 1 MiB / 10k-entry document
// limits, and three conflict retries.
type Options struct {
	Namespace        string
	ReplicaID        string
	MaxCooldown      time.Duration
	MaxDocumentBytes int
	MaxEntries       int
	Retries          int
	Now              func() time.Time
}

// View is a decoded, active-only snapshot of the shared breaker document.
type View struct {
	Entries   map[string]Entry
	Revision  uint64
	UpdatedAt time.Time
}

// BreakerStore owns the revision-guarded merge protocol over a Store. It is
// safe for concurrent use when the underlying Store is. It does not hold a
// long-lived local copy: callers decide whether and when a successful Load is
// applied to local health state.
type BreakerStore struct {
	store            Store
	namespace        string
	replicaID        string
	maxCooldown      time.Duration
	maxDocumentBytes int
	maxEntries       int
	retries          int
	now              func() time.Time
}

func NewBreakerStore(store Store, options Options) (*BreakerStore, error) {
	if store == nil {
		return nil, ErrUnavailable
	}
	namespace := options.Namespace
	if namespace == "" {
		namespace = BreakerNamespace
	}
	maxCooldown := options.MaxCooldown
	if maxCooldown <= 0 {
		maxCooldown = DefaultMaxCooldown
	}
	maxBytes := options.MaxDocumentBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxDocumentBytes
	}
	maxEntries := options.MaxEntries
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	retries := options.Retries
	if retries == 0 {
		retries = DefaultRetries
	}
	if retries < 0 {
		return nil, errors.New("shared breaker retries must not be negative")
	}
	replicaID := options.ReplicaID
	if replicaID == "" {
		replicaID = "unknown"
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &BreakerStore{
		store:            store,
		namespace:        namespace,
		replicaID:        replicaID,
		maxCooldown:      maxCooldown,
		maxDocumentBytes: maxBytes,
		maxEntries:       maxEntries,
		retries:          retries,
		now:              now,
	}, nil
}

// Load reads the shared document. Expired entries are filtered out of the
// returned view but are not removed from the store until the next successful
// Publish. A malformed document returns ErrMalformed and must not be applied as
// empty state. A store outage returns ErrUnavailable and must leave the
// caller's local breaker state untouched.
func (s *BreakerStore) Load(ctx context.Context) (View, error) {
	snapshot, err := s.store.Load(ctx, s.namespace)
	if errors.Is(err, ErrNotFound) {
		return View{Entries: map[string]Entry{}}, nil
	}
	if err != nil {
		return View{}, err
	}
	decoded, err := decodeDocument(snapshot.Payload, s.maxEntries, s.maxDocumentBytes)
	if err != nil {
		return View{}, errors.Join(ErrMalformed, err)
	}
	active := Merge(nil, decoded, s.now().UTC(), s.maxCooldown)
	return View{Entries: active, Revision: snapshot.Revision, UpdatedAt: snapshot.UpdatedAt}, nil
}

// Publish merges local markers into the shared document and writes it with a
// revision precondition. On ErrConflict it re-reads, re-merges, and retries at
// most Options.Retries times. Publishing the same markers twice is idempotent:
// when the merge produces the same active document, no write is issued and the
// existing revision is returned.
//
// Publish never deletes a marker. Recovery is expressed by expiry of a
// cooldown deadline, not by a successful local request, so a store outage or a
// local success cannot accidentally clear an open circuit on another replica.
func (s *BreakerStore) Publish(ctx context.Context, local map[string]Entry) (View, error) {
	now := s.now().UTC()
	prepared, err := s.prepareLocal(local, now)
	if err != nil {
		return View{}, err
	}
	var lastErr error
	for attempt := 0; attempt <= s.retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return View{}, err
		}
		snapshot, err := s.store.Load(ctx, s.namespace)
		if errors.Is(err, ErrNotFound) {
			snapshot = Snapshot{}
		} else if err != nil {
			return View{}, err
		}

		current := map[string]Entry{}
		if snapshot.Revision > 0 || len(snapshot.Payload) > 0 {
			current, err = decodeDocument(snapshot.Payload, s.maxEntries, s.maxDocumentBytes)
			if err != nil {
				return View{}, errors.Join(ErrMalformed, err)
			}
		}
		merged := Merge(current, prepared, now, s.maxCooldown)
		if entriesEqual(current, merged) {
			return View{Entries: cloneEntries(merged), Revision: snapshot.Revision, UpdatedAt: snapshot.UpdatedAt}, nil
		}

		payload, err := encodeDocument(merged, s.maxEntries, s.maxDocumentBytes)
		if err != nil {
			return View{}, errors.Join(ErrMalformed, err)
		}
		saved, err := s.store.CompareAndSwap(ctx, s.namespace, snapshot.Revision, payload)
		if errors.Is(err, ErrConflict) {
			lastErr = err
			continue
		}
		if err != nil {
			return View{}, err
		}
		return View{Entries: cloneEntries(merged), Revision: saved.Revision, UpdatedAt: saved.UpdatedAt}, nil
	}
	if lastErr == nil {
		lastErr = ErrConflict
	}
	return View{}, lastErr
}

func (s *BreakerStore) prepareLocal(local map[string]Entry, now time.Time) (map[string]Entry, error) {
	out := make(map[string]Entry, len(local))
	for deployment, entry := range local {
		if deployment == "" {
			return nil, ErrInvalidEntry
		}
		if entry.ObservedAt.IsZero() {
			entry.ObservedAt = now
		}
		if entry.OriginReplica == "" {
			entry.OriginReplica = s.replicaID
		}
		normalized, active, err := NormalizeEntry(entry, now, s.maxCooldown)
		if err != nil {
			return nil, err
		}
		if !active {
			continue
		}
		out[deployment] = normalized
	}
	return out, nil
}
