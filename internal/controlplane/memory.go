package controlplane

import (
	"bytes"
	"context"
	"sync"
	"time"
)

type MemoryStore struct {
	mu     sync.RWMutex
	items  map[string]Snapshot
	failed bool
}

func NewMemoryStore() *MemoryStore           { return &MemoryStore{items: map[string]Snapshot{}} }
func (s *MemoryStore) SetUnavailable(v bool) { s.mu.Lock(); s.failed = v; s.mu.Unlock() }
func (s *MemoryStore) available() error {
	if s.failed {
		return ErrUnavailable
	}
	return nil
}

func (s *MemoryStore) Get(ctx context.Context, namespace string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.failed {
		return Snapshot{}, ErrUnavailable
	}
	v, ok := s.items[namespace]
	if !ok {
		return Snapshot{}, ErrNotFound
	}
	v.Payload = append([]byte(nil), v.Payload...)
	return v, nil
}

func (s *MemoryStore) Put(ctx context.Context, namespace string, expectedRevision uint64, payload []byte) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if namespace == "" || len(payload) == 0 {
		return Snapshot{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed {
		return Snapshot{}, ErrUnavailable
	}
	old, exists := s.items[namespace]
	if exists && old.Revision != expectedRevision {
		return Snapshot{}, ErrConflict
	}
	if !exists && expectedRevision != 0 {
		return Snapshot{}, ErrConflict
	}
	next := Snapshot{Revision: expectedRevision + 1, Schema: old.Schema, Payload: append([]byte(nil), payload...), UpdatedAt: time.Now().UTC()}
	if next.Schema == 0 {
		next.Schema = 1
	}
	s.items[namespace] = next
	return cloneSnapshot(next), nil
}

func (s *MemoryStore) Delete(ctx context.Context, namespace string, expectedRevision uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed {
		return ErrUnavailable
	}
	old, ok := s.items[namespace]
	if !ok {
		return ErrNotFound
	}
	if old.Revision != expectedRevision {
		return ErrConflict
	}
	delete(s.items, namespace)
	return nil
}

func EqualPayload(a, b Snapshot) bool {
	return a.Revision == b.Revision && bytes.Equal(a.Payload, b.Payload)
}
