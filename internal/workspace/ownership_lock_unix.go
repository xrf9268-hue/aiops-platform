//go:build !windows

package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func acquireWorktreeOwnershipFileLock(lockPath string, nonblock bool) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir worktree ownership lock parent: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open worktree ownership lock %s: %w", lockPath, err)
	}
	flags := syscall.LOCK_EX
	if nonblock {
		flags |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), flags); err != nil {
		_ = f.Close()
		if nonblock && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
			return nil, ErrWorktreeOwnershipLockHeld
		}
		return nil, fmt.Errorf("flock worktree ownership %s: %w", lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
