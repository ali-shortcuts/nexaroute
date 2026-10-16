package controlplane

import (
	"errors"
	"path/filepath"
	"time"
)

// DefaultFileLockTimeout bounds every advisory lock acquisition performed by
// the file store. Acquisition never blocks indefinitely and never returns
// success without holding the lock.
const DefaultFileLockTimeout = 30 * time.Second

// ErrCrossProcessLockUnsupported is returned by FileStore write operations on
// platforms without advisory flock support. The store fails closed rather
// than risk silent cross-process lost updates.
var ErrCrossProcessLockUnsupported = errors.New("control-plane file store cross-process locking requires Linux")

// FileLockTimeoutError is returned when the advisory store lock cannot be
// acquired within the configured bound. It unwraps to ErrUnavailable so
// callers treating the store as unavailable keep working, while errors.As
// exposes the typed timeout for observability and tests.
type FileLockTimeoutError struct {
	Path    string
	Op      string
	Timeout time.Duration
}

func (e *FileLockTimeoutError) Error() string {
	return "control-plane file store lock timeout: op " + e.Op + " on " + e.Path +
		" after " + e.Timeout.String() + " (another process holds the lock)"
}

func (e *FileLockTimeoutError) Unwrap() error { return ErrUnavailable }

// lockFilePath returns the advisory lock file for a store path. It lives
// inside the store directory so it shares the store's lifetime and
// permissions; the file itself is only ever flocked, never read for state.
func lockFilePath(storePath string) string {
	return filepath.Join(filepath.Dir(storePath), ".controlplane.lock")
}

// acquireFileLock opens (creating, 0600) the lock file and acquires an
// exclusive advisory lock, waiting at most timeout. The returned release
// function must be called exactly once. Platform behavior lives in
// filelock_linux.go (real flock) and filelock_other.go (loud fail-closed).
