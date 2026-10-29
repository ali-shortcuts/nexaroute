//go:build !linux && !darwin

package nexaflow

import (
	"fmt"
	"os"
)

// acquireFileLock is a no-op on platforms without flock: the file store
// still gets atomic-rename durability, but concurrent writers on the
// same path are not excluded. Multi-writer deployments must use the
// PostgreSQL store.
func acquireFileLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("nexaflow: open file store lock: %w", err)
	}
	return f, nil
}

func releaseFileLock(f *os.File) error {
	if f == nil {
		return nil
	}
	return f.Close()
}
