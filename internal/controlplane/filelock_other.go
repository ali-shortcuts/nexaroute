//go:build !linux

package controlplane

import (
	"context"
	"time"
)

// acquireFileLock on platforms without syscall flock support fails closed: a
// write that proceeded without the inter-process lock could silently lose a
// concurrent process's update, so FileStore writes refuse loudly instead of
// degrading to a silent single-process mode.
func acquireFileLock(ctx context.Context, lockPath, op string, timeout time.Duration) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, ErrCrossProcessLockUnsupported
}
