//go:build linux

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// fileLockPollInterval is the retry step while waiting for a contended lock.
// Short enough to keep handoff latency low, long enough to avoid syscall spam.
const fileLockPollInterval = 5 * time.Millisecond

func acquireFileLock(ctx context.Context, lockPath, op string, timeout time.Duration) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = DefaultFileLockTimeout
	}
	// The store directory is created on demand by persist; the lock must be
	// acquirable before the first write, so create it here too (0700, store
	// convention). MkdirAll is idempotent under concurrent creators.
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return nil, fmt.Errorf("create control-plane lock directory: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open control-plane lock file: %w", err)
	}
	// Enforce 0600 even if the lock file pre-existed with wider permissions.
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("secure control-plane lock file: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, fmt.Errorf("flock control-plane lock file: %w", err)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			_ = f.Close()
			return nil, &FileLockTimeoutError{Path: lockPath, Op: op, Timeout: timeout}
		}
		wait := fileLockPollInterval
		if remaining < wait {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
}
