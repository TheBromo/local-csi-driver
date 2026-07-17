// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package nodeprep

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"time"
)

// lockRetryInterval is the poll interval while waiting for the host-wide
// preparation lock.
const lockRetryInterval = 2 * time.Second

// acquireLock takes an exclusive flock on the given file, which should live
// on a host tmpfs (e.g. /run) so stale locks disappear on reboot. It polls
// until the lock is acquired or the context is done, and returns a release
// function.
func acquireLock(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open lock file %s: %w", path, err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				// The lock is released when the descriptor closes; errors
				// are unrecoverable and irrelevant at this point.
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close() //nolint:errcheck // read-only descriptor
			}, nil
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			f.Close() //nolint:errcheck // lock error takes precedence
			return nil, fmt.Errorf("failed to lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			f.Close() //nolint:errcheck // context error takes precedence
			return nil, fmt.Errorf("timed out waiting for lock %s: %w", path, ctx.Err())
		case <-time.After(lockRetryInterval):
		}
	}
}
