package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type fileState struct {
	Schema    int                 `json:"schema"`
	Snapshots map[string]Snapshot `json:"snapshots"`
}

// FileStore is the default durable control-plane backend. Every write is
// serialized, fsynced, and atomically renamed; an interrupted write leaves the
// previous valid state intact.
type FileStore struct {
	path string
	mu   sync.Mutex
}

func NewFileStore(path string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("control-plane file store path is empty")
	}
	return &FileStore{path: path}, nil
}

func (s *FileStore) Get(ctx context.Context, namespace string) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.load()
	if err != nil {
		return Snapshot{}, err
	}
	v, ok := state.Snapshots[namespace]
	if !ok {
		return Snapshot{}, ErrNotFound
	}
	return cloneSnapshot(v), nil
}

func (s *FileStore) Put(ctx context.Context, namespace string, expectedRevision uint64, payload []byte) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	if namespace == "" || len(payload) == 0 {
		return Snapshot{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.load()
	if err != nil {
		return Snapshot{}, err
	}
	old, exists := state.Snapshots[namespace]
	if (!exists && expectedRevision != 0) || (exists && old.Revision != expectedRevision) {
		return Snapshot{}, ErrConflict
	}
	next := Snapshot{Revision: expectedRevision + 1, Schema: old.Schema, Payload: append([]byte(nil), payload...), UpdatedAt: time.Now().UTC()}
	if next.Schema == 0 {
		next.Schema = 1
	}
	state.Snapshots[namespace] = next
	if err := s.persist(state); err != nil {
		return Snapshot{}, err
	}
	return cloneSnapshot(next), nil
}

func (s *FileStore) Delete(ctx context.Context, namespace string, expectedRevision uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, err := s.load()
	if err != nil {
		return err
	}
	old, ok := state.Snapshots[namespace]
	if !ok {
		return ErrNotFound
	}
	if old.Revision != expectedRevision {
		return ErrConflict
	}
	delete(state.Snapshots, namespace)
	return s.persist(state)
}

func (s *FileStore) load() (fileState, error) {
	state := fileState{Schema: 1, Snapshots: map[string]Snapshot{}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("read control-plane store: %w", err)
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return state, fmt.Errorf("decode control-plane store: %w", err)
	}
	if state.Schema != 1 || state.Snapshots == nil {
		return state, errors.New("unsupported control-plane file store schema")
	}
	return state, nil
}

func (s *FileStore) persist(state fileState) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return fmt.Errorf("create control-plane store directory: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".controlplane-*")
	if err != nil {
		return fmt.Errorf("create control-plane temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(append(data, '\n'))
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write control-plane store: %w", err)
	}
	if err = os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("commit control-plane store: %w", err)
	}
	if dir, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func BackupFileStore(path, backup string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var state fileState
	if err := json.Unmarshal(data, &state); err != nil || state.Schema != 1 || state.Snapshots == nil {
		return errors.New("invalid control-plane backup source")
	}
	if err := os.WriteFile(backup, data, 0600); err != nil {
		return err
	}
	return nil
}

func RestoreFileStore(path, backup string) error {
	data, err := os.ReadFile(backup)
	if err != nil {
		return err
	}
	var state fileState
	if err := json.Unmarshal(data, &state); err != nil || state.Schema != 1 || state.Snapshots == nil {
		return errors.New("invalid control-plane backup")
	}
	store, err := NewFileStore(path)
	if err != nil {
		return err
	}
	return store.persist(state)
}
