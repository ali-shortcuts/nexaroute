package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock a stable sibling inode, not config.json, which atomic saves replace.
// Never unlink the lock: that would let concurrent processes lock different inodes.
// The kernel releases the lock on exit, including SIGKILL; no stale PID guessing.
func lockInstance(path string) (*os.File, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, fmt.Errorf("NexaRoute is already running for config %s; stop it before starting another instance", path)
		}
		return nil, fmt.Errorf("lock config: %w", err)
	}
	return f, nil
}
