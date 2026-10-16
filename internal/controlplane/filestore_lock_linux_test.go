//go:build linux

package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFileLockTimeoutTypedError holds the advisory lock out-of-band and
// verifies a contended Put fails closed with the typed timeout error (which
// unwraps to ErrUnavailable), then succeeds once the lock is released.
func TestFileLockTimeoutTypedError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := NewFileStoreWithOptions(path, FileStoreOptions{LockTimeout: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := store.Put(ctx, "ns", 0, []byte("seed")); err != nil {
		t.Fatalf("seed put: %v", err)
	}

	release, err := acquireFileLock(ctx, store.lockPath, "test-hold", 10*time.Second)
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	start := time.Now()
	_, err = store.Put(ctx, "ns", 1, []byte("contended"))
	elapsed := time.Since(start)
	var timeoutErr *FileLockTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("contended put error = %v (%T), want *FileLockTimeoutError", err, err)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("timeout error does not unwrap to ErrUnavailable: %v", err)
	}
	if timeoutErr.Op != "put" || timeoutErr.Path != store.lockPath {
		t.Fatalf("timeout error carries wrong context: %+v", timeoutErr)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("lock wait took %v, want bounded ~200ms", elapsed)
	}
	// Same-process Delete must observe the same typed timeout.
	if derr := store.Delete(ctx, "ns", 1); !errors.As(derr, &timeoutErr) {
		t.Fatalf("contended delete error = %v (%T), want *FileLockTimeoutError", derr, derr)
	}
	release()

	if _, err := store.Put(ctx, "ns", 1, []byte("after-release")); err != nil {
		t.Fatalf("put after release: %v", err)
	}
	got, err := store.Get(ctx, "ns")
	if err != nil || string(got.Payload) != "after-release" || got.Revision != 2 {
		t.Fatalf("post-release state: %+v %v", got, err)
	}
	if fi, err := os.Stat(store.lockPath); err != nil {
		t.Fatalf("lock file missing: %v", err)
	} else if fi.Mode().Perm() != 0600 {
		t.Fatalf("lock file perms = %o, want 0600", fi.Mode().Perm())
	}
}

// TestFileLockContextCancel verifies a cancelled context aborts lock
// acquisition instead of waiting out the timeout.
func TestFileLockContextCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := NewFileStoreWithOptions(path, FileStoreOptions{LockTimeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	release, err := acquireFileLock(context.Background(), store.lockPath, "test-hold", 10*time.Second)
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err = store.Put(ctx, "ns", 0, []byte("x"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled put error = %v, want context.Canceled", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancelled lock acquisition waited instead of aborting")
	}
}

// TestRestoreFileStoreRespectsLockTimeout verifies restore — which replaces
// the whole store file — is serialized by the same advisory lock.
func TestRestoreFileStoreRespectsLockTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := store.Put(ctx, "ns", 0, []byte("v1")); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "state.bak")
	if err := BackupFileStore(path, backup); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, "ns", 1, []byte("v2")); err != nil {
		t.Fatal(err)
	}

	release, err := acquireFileLock(ctx, store.lockPath, "test-hold", 10*time.Second)
	if err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	var timeoutErr *FileLockTimeoutError
	rerr := RestoreFileStoreWithOptions(path, backup, FileStoreOptions{LockTimeout: 200 * time.Millisecond})
	if !errors.As(rerr, &timeoutErr) {
		t.Fatalf("contended restore error = %v (%T), want *FileLockTimeoutError", rerr, rerr)
	}
	release()

	if err := RestoreFileStore(path, backup); err != nil {
		t.Fatalf("restore after release: %v", err)
	}
	got, err := store.Get(ctx, "ns")
	if err != nil || string(got.Payload) != "v1" || got.Revision != 1 {
		t.Fatalf("restored state: %+v %v", got, err)
	}
}

// TestBackupFileStoreIsAtomic is the F10 regression test: the backup copy
// must be a complete, valid store document (the pre-fix plain os.WriteFile
// could leave a truncated backup that only failed at restore time).
func TestBackupFileStoreIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), "ns", 0, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "state.bak")
	if err := BackupFileStore(path, backup); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(backup); err != nil {
		t.Fatalf("backup missing: %v", err)
	} else if fi.Mode().Perm() != 0600 {
		t.Fatalf("backup perms = %o, want 0600", fi.Mode().Perm())
	}
	// The backup must be byte-identical to the source and restore cleanly.
	src, _ := os.ReadFile(path)
	dst, _ := os.ReadFile(backup)
	if string(src) != string(dst) {
		t.Fatal("backup bytes differ from source")
	}
	restored := filepath.Join(dir, "restored.json")
	if err := RestoreFileStore(restored, backup); err != nil {
		t.Fatalf("restore from backup: %v", err)
	}
	rs, err := NewFileStore(restored)
	if err != nil {
		t.Fatal(err)
	}
	got, err := rs.Get(context.Background(), "ns")
	if err != nil || string(got.Payload) != "payload" {
		t.Fatalf("restored from backup: %+v %v", got, err)
	}
}
