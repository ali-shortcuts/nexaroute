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

// A cancelled context fails before any directory or file is touched.
func TestWave4FileLockCancelledContextFailsClosed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := acquireFileLock(ctx, filepath.Join(t.TempDir(), "x.lock"), "op", time.Second); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// A non-positive timeout means the default: the lock is still acquired
// promptly when free, and releasing it lets a later acquire succeed.
func TestWave4FileLockNonPositiveTimeoutAcquiresAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lock")
	release, err := acquireFileLock(context.Background(), path, "op", 0)
	if err != nil {
		t.Fatal(err)
	}
	release()
	release2, err := acquireFileLock(context.Background(), path, "op", -time.Second)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	release2()
}

// A pre-existing lock file with wider permissions is tightened to 0600
// on acquire.
func TestWave4FileLockTightensExistingPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lock")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
	release, err := acquireFileLock(context.Background(), path, "op", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o, want 0600", info.Mode().Perm())
	}
}

// A lock path below a regular file cannot create its directory.
func TestWave4FileLockDirectoryCreationFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireFileLock(context.Background(), filepath.Join(blocker, "sub", "x.lock"), "op", time.Second); err == nil || !containsWave4CP(err.Error(), "create control-plane lock directory") {
		t.Fatalf("err = %v, want a lock-directory creation error", err)
	}
}

// A lock path that is itself a directory cannot be opened read-write.
func TestWave4FileLockOpenFailure(t *testing.T) {
	dir := t.TempDir()
	if _, err := acquireFileLock(context.Background(), dir, "op", time.Second); err == nil || !containsWave4CP(err.Error(), "open control-plane lock file") {
		t.Fatalf("err = %v, want a lock-file open error", err)
	}
}

// A contended lock with a tiny timeout reports a FileLockTimeoutError
// carrying the path, operation and timeout, unwrapping to ErrUnavailable.
func TestWave4FileLockTimeoutError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lock")
	release, err := acquireFileLock(context.Background(), path, "op", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, err = acquireFileLock(context.Background(), path, "wait-op", time.Millisecond)
	var timeoutErr *FileLockTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("err = %v, want a FileLockTimeoutError", err)
	}
	if timeoutErr.Path != path || timeoutErr.Op != "wait-op" || timeoutErr.Timeout != time.Millisecond {
		t.Fatalf("timeout error = %+v, want path/op/1ms", timeoutErr)
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want it to unwrap to ErrUnavailable", err)
	}
}

// Cancelling while contended returns the context error, not a timeout.
func TestWave4FileLockCancellationWhileContended(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lock")
	release, err := acquireFileLock(context.Background(), path, "op", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := acquireFileLock(ctx, path, "op", time.Minute)
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled acquire did not return")
	}
}

func containsWave4CP(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
