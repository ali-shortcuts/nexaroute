//go:build linux

package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// flockFunc wraps syscall.Flock so tests can drive the non-EWOULDBLOCK
// error path. No filesystem in the test environment fails flock with an
// error other than EWOULDBLOCK, and that branch must stay fail-closed: a
// real flock error is not "another instance is running".
var flockFunc = syscall.Flock

func Acquire(path string) (*Lock, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	// Enforce 0600 even if the lock file pre-existed with wider permissions.
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, false, err
	}
	if err := flockFunc(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &Lock{file: f}, true, nil
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = flockFunc(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}
