//go:build !windows

package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquireMirrorFileLock opens (creating as needed) a sidecar file alongside
// the mirror directory and takes an exclusive `flock` advisory lock on it.
// The returned release function unlocks and closes the file. Two processes
// sharing AIOPS_MIRROR_ROOT on the same host serialize their mirror
// fetch/clone/worktree-add operations through this lock, closing the
// process-local-sync.Mutex gap called out in SPEC §9.5 (and #228).
//
// The lock file lives next to the mirror so administrative removal of the
// mirror is a one-step operation (`rm -rf <mirror>` plus `<mirror>.lock`);
// callers must not delete the lock file while holding the lock.
//
// Returning an error is intentionally rare: a flock failure here means the
// kernel rejected the syscall (file descriptor exhaustion, ENOLCK on NFS)
// rather than ordinary contention, and the caller should fail closed rather
// than racing on a degraded mirror. Operators running on NFS-mounted mirror
// roots should switch to a local filesystem — flock on NFS is silently
// best-effort on Linux and unsupported on macOS.
func acquireMirrorFileLock(mirror string) (func(), error) {
	lockPath := mirror + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir mirror lock parent: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open mirror lock %s: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("flock mirror %s: %w", lockPath, err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
