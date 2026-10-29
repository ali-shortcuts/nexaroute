//go:build linux || darwin

package nexaflow

import (
	"fmt"
	"os"
	"syscall"
)

// acquireFileLock opens path and takes an exclusive, non-blocking flock.
// The lock is held until the returned file is closed (or the process
// dies, which releases it automatically) — that release-on-death is what
// makes restart recovery possible without a stale-lock problem.
func acquireFileLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("nexaflow: open file store lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// The lock was not acquired, so this descriptor is
		// discarded; the flock error is the returned error.
		_ = f.Close()
		return nil, fmt.Errorf("nexaflow: file store %q is already open by another process: %w", path, err)
	}
	return f, nil
}

func releaseFileLock(f *os.File) error {
	if f == nil {
		return nil
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return f.Close()
}
