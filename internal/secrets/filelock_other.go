//go:build !linux

package secrets

import "errors"

func acquireSecretLock(lockPath string) (func(), error) {
	return nil, errors.New("migration and rewrap require a flock-based file lock, which is only implemented on Linux; refusing to run without mutual exclusion")
}
