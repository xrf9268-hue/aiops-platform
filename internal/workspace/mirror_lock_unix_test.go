//go:build !windows

package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// mirrorFlockSubprocessEnv lets a re-exec'd copy of `go test` enter the
// child-process branch in TestMirrorFileLockSerializesAcrossProcesses. The
// value is the mirror path the child should lock; the child also writes a
// ready sentinel file at <mirror>.ready when it has the lock so the parent
// only races on contention after the child has actually grabbed it.
const mirrorFlockSubprocessEnv = "AIOPS_TEST_MIRROR_FLOCK_HOLDER"

// TestMirrorFileLockSerializesAcrossProcesses pins SPEC §9.5 cross-process
// mutual exclusion for AIOPS_MIRROR_ROOT (#228): two independent workers
// sharing a mirror root must serialize git fetch/clone/worktree-add against
// the same bare repo, not just race on git's own per-ref locks. The test
// re-execs the test binary as a child that takes the flock and holds it for
// `holdFor`; the parent's own acquire must block until the child releases.
func TestMirrorFileLockSerializesAcrossProcesses(t *testing.T) {
	if path := os.Getenv(mirrorFlockSubprocessEnv); path != "" {
		// Child branch: acquire, signal ready, hold, release, exit.
		release, err := acquireMirrorFileLock(path)
		if err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(path+".ready", []byte("ok"), 0o644); err != nil {
			release()
			os.Exit(3)
		}
		time.Sleep(750 * time.Millisecond)
		release()
		os.Exit(0)
	}

	dir := t.TempDir()
	mirror := filepath.Join(dir, "mirror.git")

	cmd := exec.Command(os.Args[0], "-test.run", "TestMirrorFileLockSerializesAcrossProcesses")
	cmd.Env = append(os.Environ(), mirrorFlockSubprocessEnv+"="+mirror)
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn lock-holder subprocess: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	// Wait up to 5s for the child to take the lock — far longer than needed
	// on any sane CI host, short enough to surface a child crash quickly.
	deadline := time.Now().Add(5 * time.Second)
	ready := mirror + ".ready"
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock-holder subprocess never signaled ready (%s); the child likely failed acquireMirrorFileLock", ready)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Parent attempts its own acquire — must block until child releases.
	start := time.Now()
	release, err := acquireMirrorFileLock(mirror)
	if err != nil {
		t.Fatalf("parent acquireMirrorFileLock: %v", err)
	}
	elapsed := time.Since(start)
	release()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("lock-holder subprocess exited %v", err)
	}

	// Child held the lock for 750ms; parent should have blocked at least
	// 100ms (allowing scheduler jitter). If we got through under 100ms the
	// file lock was not effective.
	if elapsed < 100*time.Millisecond {
		t.Fatalf("parent acquired the flock in %v under contention; want ≥100ms (child held for 750ms)", elapsed)
	}
}

// TestAcquireMirrorLockFailsClosedOnFlockError pins the fail-closed
// invariant from PR #283's codex P1: when the file lock can't be taken
// (here: the sidecar path is a directory, so OpenFile returns EISDIR), the
// helper must release the process-local mutex and return an error rather
// than silently degrading to in-process-only locking. Silent degradation
// would re-open the cross-process race the lock exists to close exactly in
// the failure mode (ENOLCK on NFS, fd exhaustion) where shared
// deployments need it most.
func TestAcquireMirrorLockFailsClosedOnFlockError(t *testing.T) {
	dir := t.TempDir()
	mirror := filepath.Join(dir, "mirror.git")
	// Pre-create the sidecar path as a directory so OpenFile(<mirror>.lock,
	// O_RDWR|O_CREATE) cannot open it as a regular file.
	if err := os.MkdirAll(mirror+".lock", 0o755); err != nil {
		t.Fatalf("seed lock dir: %v", err)
	}

	_, err := acquireMirrorLock(mirror)
	if err == nil {
		t.Fatal("acquireMirrorLock returned nil error when flock could not be taken; want fail-closed")
	}

	// Process-local mutex must have been released — a subsequent call that
	// also fails should not deadlock waiting on the inner sync.Mutex.
	done := make(chan struct{})
	go func() {
		_, _ = acquireMirrorLock(mirror)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("acquireMirrorLock did not release process-local mutex on flock failure; second call deadlocked")
	}
}

// TestAcquireMirrorFileLockReleaseAllowsReacquire pins the release-clears-
// the-lock invariant: after the returned release fn runs, the next caller
// can acquire without blocking. Without this the in-process retry path
// (poll loop tick N+1) would deadlock against itself.
func TestAcquireMirrorFileLockReleaseAllowsReacquire(t *testing.T) {
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	release, err := acquireMirrorFileLock(mirror)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	release()
	start := time.Now()
	release2, err := acquireMirrorFileLock(mirror)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	defer release2()
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("reacquire after release took %v, want immediate", elapsed)
	}
	// Sanity: the sidecar file must exist on disk between calls (operators
	// inspect it with `lsof` to attribute a stuck mirror to a specific
	// worker pid).
	if _, err := os.Stat(mirror + ".lock"); err != nil {
		t.Fatalf("expected lock sidecar at %s: %v", mirror+".lock", err)
	}
}
