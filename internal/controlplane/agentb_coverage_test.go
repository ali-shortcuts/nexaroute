package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Agent B coverage: the last control-plane fault paths — a contended
// file lock timing out, a non-contention flock failure, persist failing
// inside Put, and the SQL store's begin/lock/commit failures. Each test
// pins a fail-closed behavior: a failed write is reported, never
// mistaken for a committed revision.

// A lock held by another file description must make acquireFileLock
// fail with the typed timeout error, not hang.
func TestAgentBFileLockTimeout(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "store.lock")
	holder, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
	_, err = acquireFileLock(context.Background(), lockPath, "test", 30*time.Millisecond)
	var timeoutErr *FileLockTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("acquireFileLock contended err = %v, want *FileLockTimeoutError", err)
	}
	if timeoutErr.Path != lockPath || timeoutErr.Op != "test" {
		t.Fatalf("timeout error = %+v, want path %q op %q", timeoutErr, lockPath, "test")
	}
}

// A flock failure that is not contention (EWOULDBLOCK/EAGAIN) must be
// reported as a flock error. There is no deterministic filesystem
// trigger for this — every openable path yields either a lock or
// EWOULDBLOCK — so the test injects the failure through the flockFunc
// seam (syscall.Flock by default).
func TestAgentBFileLockFlockError(t *testing.T) {
	old := flockFunc
	flockFunc = func(int, int) error { return syscall.EINVAL }
	defer func() { flockFunc = old }()
	_, err := acquireFileLock(context.Background(), filepath.Join(t.TempDir(), "store.lock"), "test", time.Second)
	if err == nil || !strings.Contains(err.Error(), "flock control-plane lock file") {
		t.Fatalf("acquireFileLock flock error = %v, want a flock error", err)
	}
}

// persist must surface a directory-creation failure.
func TestAgentBPersistMkdirError(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	store := &FileStore{
		path:     filepath.Join(blocker, "sub", "store.json"),
		atomicFS: defaultAtomicFileFS(),
	}
	if err := store.persist(fileState{Schema: 1, Snapshots: map[string]Snapshot{}}); err == nil || !strings.Contains(err.Error(), "create control-plane store directory") {
		t.Fatalf("persist under a file err = %v, want a directory error", err)
	}
}

// persist must surface a JSON encoding failure (a snapshot timestamp
// outside the JSON time range cannot be marshaled).
func TestAgentBPersistMarshalError(t *testing.T) {
	store, err := NewFileStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	state := fileState{Schema: 1, Snapshots: map[string]Snapshot{
		"x": {Revision: 1, Schema: 1, Payload: []byte("v"), UpdatedAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
	}}
	if err := store.persist(state); err == nil {
		t.Fatal("persist with an unmarshalable timestamp succeeded, want an error")
	}
}

// Put must report a commit failure instead of a phantom success, and
// the namespace must stay unwritten so a retry can succeed.
func TestAgentBPutSurfacesPersistFailure(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.atomicFS = atomicFileFS{
		createTemp: func(string, string) (atomicFile, error) {
			return nil, errors.New("injected temp failure")
		},
		rename:  os.Rename,
		openDir: func(dir string) (atomicDir, error) { return os.Open(dir) },
	}
	ctx := context.Background()
	if _, err := store.Put(ctx, "config", 0, []byte("first")); err == nil {
		t.Fatal("Put with a failing commit succeeded, want an error")
	}
	if _, err := store.Get(ctx, "config"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after failed Put err = %v, want ErrNotFound (no phantom write)", err)
	}
	store.atomicFS = defaultAtomicFileFS()
	if _, err := store.Put(ctx, "config", 0, []byte("first")); err != nil {
		t.Fatalf("Put after restoring the filesystem err = %v", err)
	}
}

// Put must fail closed when its commit fails.
func TestAgentBSQLStorePutCommitError(t *testing.T) {
	state := &scriptedSQL{affected: 1, commitErr: errors.New("commit failed")}
	store, err := NewSQLStore(openScriptedDB(t, state))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), "config", 0, []byte("first")); err == nil || !strings.Contains(err.Error(), "commit control snapshot") {
		t.Fatalf("Put commit error = %v, want a commit error", err)
	}
	if state.commits != 1 {
		t.Fatalf("Put must attempt the commit: commits=%d", state.commits)
	}
}

// Delete must fail closed when the transaction cannot begin, when the
// namespace lock fails, and when its commit fails.
func TestAgentBSQLStoreDeleteBeginLockCommitErrors(t *testing.T) {
	ctx := context.Background()
	beginState := &scriptedSQL{beginErr: errors.New("begin failed")}
	store, _ := NewSQLStore(openScriptedDB(t, beginState))
	if err := store.Delete(ctx, "config", 3); err == nil || !strings.Contains(err.Error(), "begin control write") {
		t.Fatalf("Delete begin error = %v, want a begin error", err)
	}

	lockState := &scriptedSQL{execErr: errors.New("lock failed"), execFailSubstring: "pg_advisory_xact_lock"}
	store, _ = NewSQLStore(openScriptedDB(t, lockState))
	if err := store.Delete(ctx, "config", 3); err == nil || !strings.Contains(err.Error(), "lock control namespace") {
		t.Fatalf("Delete lock error = %v, want a lock error", err)
	}

	commitState := &scriptedSQL{affected: 1, commitErr: errors.New("commit failed")}
	store, _ = NewSQLStore(openScriptedDB(t, commitState))
	if err := store.Delete(ctx, "config", 3); err == nil || !strings.Contains(err.Error(), "commit control snapshot") {
		t.Fatalf("Delete commit error = %v, want a commit error", err)
	}
	if commitState.commits != 1 {
		t.Fatalf("Delete must attempt the commit: commits=%d", commitState.commits)
	}
}
