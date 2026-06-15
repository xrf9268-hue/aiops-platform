package workspace

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrWorktreeOwnershipLockHeld reports that a live process currently owns the
// worktree. It is returned only by non-blocking lock probes.
var ErrWorktreeOwnershipLockHeld = errors.New("worktree ownership lock held")

const worktreeOwnershipLockName = "aiops-owner.lock"

// PreparedGitWorkspace is a prepared git worktree plus the ownership lease that
// protects it from foreign-root reclaim while an agent run is active.
type PreparedGitWorkspace struct {
	Workdir    string
	CreatedNow bool

	release func()
}

// Release drops the ownership lease. It is idempotent so callers can safely
// defer it immediately after a successful prepare.
func (p *PreparedGitWorkspace) Release() {
	if p == nil || p.release == nil {
		return
	}
	p.release()
	p.release = nil
}

func acquireWorktreeOwnership(ctx context.Context, workdir string) (func(), error) {
	lockPath, err := worktreeOwnershipLockPath(ctx, workdir)
	if err != nil {
		return nil, err
	}
	release, err := acquireWorktreeOwnershipFileLock(lockPath, false)
	if err != nil {
		return nil, err
	}
	return release, nil
}

func worktreeOwnershipLockHeld(ctx context.Context, workdir string) bool {
	lockPath, err := worktreeOwnershipLockPath(ctx, workdir)
	if err != nil {
		return true
	}
	release, err := acquireWorktreeOwnershipFileLock(lockPath, true)
	if err == nil {
		release()
		return false
	}
	return true
}

func worktreeOwnershipLockPath(ctx context.Context, workdir string) (string, error) {
	out, err := runGitOutput(ctx, workdir, "rev-parse", "--git-dir")
	if err != nil {
		return "", fmt.Errorf("resolve worktree git dir: %w", err)
	}
	gitDir := strings.TrimSpace(string(out))
	if gitDir == "" {
		return "", errors.New("resolve worktree git dir: empty path")
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(workdir, gitDir)
	}
	return filepath.Join(gitDir, worktreeOwnershipLockName), nil
}
