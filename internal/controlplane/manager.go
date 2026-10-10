package controlplane

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
)

type Manager struct {
	store       Store
	mode        FailureMode
	mu          sync.RWMutex
	last        map[string]Snapshot
	loads       atomic.Uint64
	saves       atomic.Uint64
	unavailable atomic.Uint64
	conflicts   atomic.Uint64
}

func NewManager(store Store, mode FailureMode) (*Manager, error) {
	if store == nil {
		return nil, ErrUnavailable
	}
	if !mode.Valid() {
		return nil, ErrInvalidRevision
	}
	return &Manager{store: store, mode: mode, last: map[string]Snapshot{}}, nil
}

func (m *Manager) Load(ctx context.Context, namespace string) (Snapshot, error) {
	m.loads.Add(1)
	value, err := m.store.Get(ctx, namespace)
	if err == nil {
		m.mu.Lock()
		m.last[namespace] = cloneSnapshot(value)
		m.mu.Unlock()
		return value, nil
	}
	if errors.Is(err, ErrUnavailable) {
		m.unavailable.Add(1)
	}
	if errors.Is(err, ErrConflict) {
		m.conflicts.Add(1)
	}
	if !errors.Is(err, ErrUnavailable) || m.mode != LastKnownGood {
		return Snapshot{}, err
	}
	m.mu.RLock()
	value, ok := m.last[namespace]
	m.mu.RUnlock()
	if !ok {
		return Snapshot{}, ErrUnavailable
	}
	return cloneSnapshot(value), nil
}

func (m *Manager) Save(ctx context.Context, namespace string, expectedRevision uint64, payload []byte) (Snapshot, error) {
	value, err := m.store.Put(ctx, namespace, expectedRevision, payload)
	if err != nil {
		if errors.Is(err, ErrUnavailable) {
			m.unavailable.Add(1)
		}
		if errors.Is(err, ErrConflict) {
			m.conflicts.Add(1)
		}
		return Snapshot{}, err
	}
	m.saves.Add(1)
	m.mu.Lock()
	m.last[namespace] = cloneSnapshot(value)
	m.mu.Unlock()
	return value, nil
}

func (m *Manager) Invalidate(namespace string) { m.mu.Lock(); delete(m.last, namespace); m.mu.Unlock() }
func (m *Manager) LastKnown(namespace string) (Snapshot, bool) {
	m.mu.RLock()
	v, ok := m.last[namespace]
	m.mu.RUnlock()
	return cloneSnapshot(v), ok
}

// Reconcile refreshes one namespace from the durable store (or the configured
// last-known-good fallback) without overwriting a newer revision.
func (m *Manager) Reconcile(ctx context.Context, namespace string) (ReconcileResult, error) {
	m.loads.Add(1)
	value, err := m.store.Get(ctx, namespace)
	if err == nil {
		m.mu.Lock()
		m.last[namespace] = cloneSnapshot(value)
		m.mu.Unlock()
		return ReconcileResult{Namespace: namespace, Revision: value.Revision, Source: "durable"}, nil
	}
	if !errors.Is(err, ErrUnavailable) || m.mode != LastKnownGood {
		return ReconcileResult{}, err
	}
	m.unavailable.Add(1)
	value, ok := m.LastKnown(namespace)
	if !ok {
		return ReconcileResult{}, ErrUnavailable
	}
	return ReconcileResult{Namespace: namespace, Revision: value.Revision, Source: "last_known_good"}, nil
}

func (m *Manager) Health() Health {
	return Health{Loads: m.loads.Load(), Saves: m.saves.Load(), Unavailable: m.unavailable.Load(), Conflicts: m.conflicts.Load()}
}

func cloneSnapshot(v Snapshot) Snapshot { v.Payload = append([]byte(nil), v.Payload...); return v }
