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
//
// Cross-process safety: on Linux every read-modify-write cycle (Put, Delete)
// and every restore is additionally serialized across processes with an
// advisory flock on a lock file inside the store directory, so two gateway
// processes sharing one data directory observe optimistic-revision
// ErrConflict instead of silently losing each other's updates. Lock
// acquisition waits at most FileStoreOptions.LockTimeout (default 30s) and
// then fails closed with a *FileLockTimeoutError; it never hangs indefinitely
// and never proceeds without the lock. On platforms without flock support,
// writes fail closed with ErrCrossProcessLockUnsupported.
type FileStore struct {
	path        string
	lockPath    string
	lockTimeout time.Duration
	mu          sync.Mutex
}

// FileStoreOptions tunes FileStore behavior. The zero value is valid.
type FileStoreOptions struct {
	// LockTimeout bounds advisory lock acquisition for Put, Delete and
	// RestoreFileStore. Zero or negative selects DefaultFileLockTimeout.
	LockTimeout time.Duration
}

func NewFileStore(path string) (*FileStore, error) {
	return NewFileStoreWithOptions(path, FileStoreOptions{})
}

func NewFileStoreWithOptions(path string, opts FileStoreOptions) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("control-plane file store path is empty")
	}
	timeout := opts.LockTimeout
	if timeout <= 0 {
		timeout = DefaultFileLockTimeout
	}
	return &FileStore{path: path, lockPath: lockFilePath(path), lockTimeout: timeout}, nil
}

// withStoreLock serializes fn against other processes sharing the store
// directory. The in-process mutex must already be held by the caller where
// one exists; flock alone does not serialize goroutines inside this process
// because each acquisition opens its own file description.
func withStoreLock[T any](ctx context.Context, lockPath, op string, timeout time.Duration, fn func() (T, error)) (T, error) {
	release, err := acquireFileLock(ctx, lockPath, op, timeout)
	if err != nil {
		var zero T
		return zero, err
	}
	defer release()
	return fn()
}

// withStoreLockErr is withStoreLock for operations that only return an error.
func withStoreLockErr(ctx context.Context, lockPath, op string, timeout time.Duration, fn func() error) error {
	_, err := withStoreLock(ctx, lockPath, op, timeout, func() (struct{}, error) {
		return struct{}{}, fn()
	})
	return err
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
	return withStoreLock(ctx, s.lockPath, "put", s.lockTimeout, func() (Snapshot, error) {
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
	})
}

func (s *FileStore) Delete(ctx context.Context, namespace string, expectedRevision uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return withStoreLockErr(ctx, s.lockPath, "delete", s.lockTimeout, func() error {
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
	})
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
	if err := writeFileAtomic(filepath.Dir(s.path), ".controlplane-*", s.path, append(data, '\n')); err != nil {
		return err
	}
	return nil
}

// writeFileAtomic durably replaces target with data, written verbatim: temp
// file in the same directory (same filesystem, so rename is atomic), 0600,
// fsync of the file and of the directory entry. A crash at any point leaves
// either the old or the new content at target, never a torn write.
func writeFileAtomic(dir, pattern, target string, data []byte) error {
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return fmt.Errorf("create control-plane temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
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
	if err = os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("commit control-plane store: %w", err)
	}
	if dirF, err := os.Open(dir); err == nil {
		_ = dirF.Sync()
		_ = dirF.Close()
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
	// F10: the backup copy used to be a plain non-atomic os.WriteFile, so a
	// crash mid-write left a truncated backup that only failed at restore
	// time. Write it atomically like the store itself.
	return writeFileAtomic(filepath.Dir(backup), ".controlplane-backup-*", backup, data)
}

func RestoreFileStore(path, backup string) error {
	return RestoreFileStoreWithOptions(path, backup, FileStoreOptions{})
}

// RestoreFileStoreWithOptions restores path from backup, serialized against
// other processes sharing the store directory via the advisory store lock.
func RestoreFileStoreWithOptions(path, backup string, opts FileStoreOptions) error {
	data, err := os.ReadFile(backup)
	if err != nil {
		return err
	}
	var state fileState
	if err := json.Unmarshal(data, &state); err != nil || state.Schema != 1 || state.Snapshots == nil {
		return errors.New("invalid control-plane backup")
	}
	store, err := NewFileStoreWithOptions(path, opts)
	if err != nil {
		return err
	}
	// Restore replaces the whole store file: hold the inter-process lock so a
	// concurrent Put/Delete cannot interleave between our read and persist.
	return withStoreLockErr(context.Background(), store.lockPath, "restore", store.lockTimeout, func() error {
		return store.persist(state)
	})
}
