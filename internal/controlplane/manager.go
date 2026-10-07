package controlplane

import (
	"context"
	"sync"
)

type Manager struct {
	store Store
	mode  FailureMode
	mu    sync.RWMutex
	last  map[string]Snapshot
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
	value, err := m.store.Get(ctx, namespace)
	if err == nil {
		m.mu.Lock()
		m.last[namespace] = cloneSnapshot(value)
		m.mu.Unlock()
		return value, nil
	}
	if err != ErrUnavailable || m.mode != LastKnownGood {
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
		return Snapshot{}, err
	}
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
func cloneSnapshot(v Snapshot) Snapshot { v.Payload = append([]byte(nil), v.Payload...); return v }
