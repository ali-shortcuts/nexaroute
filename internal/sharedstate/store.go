package sharedstate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ali-shortcuts/nexaroute/internal/controlplane"
)

var (
	// ErrNotFound means the namespace has never been written.
	ErrNotFound = errors.New("shared breaker state not found")
	// ErrConflict means the revision precondition did not hold. Callers may
	// re-read and retry; the high-level Store does so automatically.
	ErrConflict = errors.New("shared breaker state revision conflict")
	// ErrUnavailable means the backing store could not be reached. Callers
	// must keep their current local breaker state; an outage never means that
	// circuits are closed.
	ErrUnavailable = errors.New("shared breaker state store unavailable")
	// ErrMalformed means a stored document is unreadable or outside the
	// supported schema/limits. It must not be interpreted as empty state.
	ErrMalformed = errors.New("malformed shared breaker state")
)

// Snapshot is the versioned document shape required from a state store. The
// revision is opaque except that zero means "not created yet". Implementations
// must compare-and-swap atomically and return defensive copies.
type Snapshot struct {
	Revision  uint64
	Payload   []byte
	UpdatedAt time.Time
}

// Store is the explicit state-store interface for the breaker slice. It is
// deliberately narrow: load and revision-guarded write only. No TTL, delete,
// list, or watch capability is assumed, so existing control-plane backends can
// satisfy it without a migration.
type Store interface {
	Load(ctx context.Context, namespace string) (Snapshot, error)
	CompareAndSwap(ctx context.Context, namespace string, expectedRevision uint64, payload []byte) (Snapshot, error)
}

// ControlPlaneStore adapts the existing control-plane Store (file, memory,
// PostgreSQL, or Redis) to the breaker state-store contract. It adds no new
// backend, driver, table, or migration: the revision-guarded semantics are the
// ones already documented and tested by internal/controlplane.
type ControlPlaneStore struct {
	store controlplane.Store
}

func NewControlPlaneStore(store controlplane.Store) (*ControlPlaneStore, error) {
	if store == nil {
		return nil, ErrUnavailable
	}
	return &ControlPlaneStore{store: store}, nil
}

func (s *ControlPlaneStore) Load(ctx context.Context, namespace string) (Snapshot, error) {
	snapshot, err := s.store.Get(ctx, namespace)
	if err != nil {
		return Snapshot{}, mapControlPlaneError(err)
	}
	return Snapshot{Revision: snapshot.Revision, Payload: append([]byte(nil), snapshot.Payload...), UpdatedAt: snapshot.UpdatedAt}, nil
}

func (s *ControlPlaneStore) CompareAndSwap(ctx context.Context, namespace string, expectedRevision uint64, payload []byte) (Snapshot, error) {
	snapshot, err := s.store.Put(ctx, namespace, expectedRevision, payload)
	if err != nil {
		return Snapshot{}, mapControlPlaneError(err)
	}
	return Snapshot{Revision: snapshot.Revision, Payload: append([]byte(nil), snapshot.Payload...), UpdatedAt: snapshot.UpdatedAt}, nil
}

func mapControlPlaneError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, controlplane.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, controlplane.ErrConflict):
		return ErrConflict
	case errors.Is(err, controlplane.ErrUnavailable):
		return ErrUnavailable
	case errors.Is(err, controlplane.ErrMalformedSnapshot):
		return ErrMalformed
	default:
		// Any untyped backend failure is unsafe to interpret as durable state.
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
}
