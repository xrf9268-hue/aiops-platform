package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// installHungGitStub puts a fake `git` first on PATH so the runGit* seams can
// be exercised against a subprocess that never exits on its own, returning
// the stub path so tests can rewrite it mid-test. The stub inherits the rest
// of PATH so /bin/sh and sleep resolve normally. Tests that call this must
// not use t.Parallel (process-wide PATH mutation).
func installHungGitStub(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "git")
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write git stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return stub
}

func overrideGitBudget(t *testing.T, budget *time.Duration, d time.Duration) {
	t.Helper()
	old := *budget
	*budget = d
	t.Cleanup(func() { *budget = old })
}

// TestGitOperationsAreBoundedWithoutCallerDeadline pins the #759 fix at the
// seam: every runGit* helper must enforce its own deadline even when the
// caller's context — like the cancel-only dispatch run context — carries
// none. Deleting the context.WithTimeout wrap in a helper makes its subtest
// fail on the nil-error assertion after the stub's full 10s sleep.
func TestGitOperationsAreBoundedWithoutCallerDeadline(t *testing.T) {
	// exec replaces sh so the context kill reaches the sleeping process
	// itself and the output pipe closes immediately; the escaped-descendant
	// variant is covered separately below.
	installHungGitStub(t, "#!/bin/sh\nexec sleep 10\n")
	dir := t.TempDir()
	cases := []struct {
		name   string
		budget *time.Duration
		run    func(context.Context) error
	}{
		{"runGit", &gitLocalTimeout, func(ctx context.Context) error {
			return runGit(ctx, dir, "status")
		}},
		{"runGitQuiet", &gitLocalTimeout, func(ctx context.Context) error {
			return runGitQuiet(ctx, dir, "status")
		}},
		{"runGitRedacted", &gitNetworkTimeout, func(ctx context.Context) error {
			return runGitRedacted(ctx, dir, "fetch")
		}},
		{"runGitOutput", &gitLocalTimeout, func(ctx context.Context) error {
			_, err := runGitOutput(ctx, dir, "rev-parse")
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			overrideGitBudget(t, tc.budget, 100*time.Millisecond)
			start := time.Now()
			err := tc.run(context.Background())
			elapsed := time.Since(start)
			if err == nil {
				t.Fatalf("%s(hung git) = nil error; want deadline error", tc.name)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s(hung git) = %v; want errors.Is(err, context.DeadlineExceeded)", tc.name, err)
			}
			if elapsed > 5*time.Second {
				t.Fatalf("%s(hung git) returned after %v; want roughly the 100ms budget", tc.name, elapsed)
			}
		})
	}
}

// TestRunGitParentCancellationPreemptsBudget proves the per-op timeout does
// not weaken the existing cancellation path: a parent WithCancelCause cancel
// (reconcile-cancel, shutdown) pre-empts the multi-minute production budget
// and its cause survives classification for errors.Is callers.
func TestRunGitParentCancellationPreemptsBudget(t *testing.T) {
	installHungGitStub(t, "#!/bin/sh\nsleep 10\n")
	sentinel := errors.New("reconcile cancel")
	ctx, cancel := context.WithCancelCause(context.Background())
	timer := time.AfterFunc(100*time.Millisecond, func() { cancel(sentinel) })
	defer timer.Stop()
	defer cancel(nil)
	start := time.Now()
	err := runGit(ctx, t.TempDir(), "status")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("runGit(canceled parent) = nil error; want cancellation error")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("runGit(canceled parent) = %v; want errors.Is(err, sentinel cause)", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("runGit(canceled parent) returned after %v; want prompt cancellation", elapsed)
	}
}

// TestRunGitFoldsActionableStderrIntoError pins #978: a failed local git op
// must surface git's actionable fatal/error line in the returned error — what
// the runtime event / dashboard shows — not just "exit status NNN", while
// keeping the underlying *exec.ExitError classifiable for callers. The stub
// emits a trailing non-actionable `hint:` line after the `fatal:` so the test
// also proves the fold selects the fatal line, not merely the last line.
// Deleting the foldGitStderr call in runGit makes the fatal-substring
// assertion fail against a bare "exit status".
func TestRunGitFoldsActionableStderrIntoError(t *testing.T) {
	installHungGitStub(t, "#!/bin/sh\n"+
		"echo \"warning: noisy progress\" >&2\n"+
		"echo \"fatal: ambiguous object name: 'origin/main'\" >&2\n"+
		"echo \"hint: use a fully-qualified ref\" >&2\n"+
		"exit 128\n")
	err := runGit(context.Background(), t.TempDir(), "worktree", "add", "wd", "origin/main")
	if err == nil {
		t.Fatal("runGit(failing git) = nil error; want the git failure")
	}
	const want = "fatal: ambiguous object name: 'origin/main'"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("runGit(failing git) = %q; want error containing git's actionable line %q", err.Error(), want)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("runGit(failing git) = %v; want errors.As(err, *exec.ExitError) preserved for classification", err)
	}
}

// TestRunGitRedactedTreatsWaitDelayOnSuccessAsSuccess pins the
// exec.ErrWaitDelay edge: when git exits 0 but a descendant (ssh
// ControlPersist master, background auto-gc) keeps the inherited output pipe
// open past gitWaitGrace, Wait returns ErrWaitDelay despite the operation
// having succeeded. classifyGitContextErr must report success, not fail the
// dispatch on a healthy repo.
func TestRunGitRedactedTreatsWaitDelayOnSuccessAsSuccess(t *testing.T) {
	installHungGitStub(t, "#!/bin/sh\nsleep 10 &\nexit 0\n")
	overrideGitBudget(t, &gitWaitGrace, 200*time.Millisecond)
	start := time.Now()
	err := runGitRedacted(context.Background(), t.TempDir(), "fetch")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("runGitRedacted(successful git, descendant holds pipe) = %v; want nil", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("runGitRedacted(successful git, descendant holds pipe) returned after %v; want the wait grace, not the descendant's exit", elapsed)
	}
}

// TestEnsureMirror_TimeoutKilledCloneLeavesNoPartialMirror pins the staging
// rename in cloneMirrorLocked: a clone killed by the #759 network budget must
// not leave a HEAD-bearing partial directory at the final mirror path, or the
// next attempt takes the existing-mirror fetch branch against a repo that
// never got its remote-tracking refspec and every retry wedges. The second
// EnsureMirror proves the failed attempt's leftovers do not block recovery.
func TestEnsureMirror_TimeoutKilledCloneLeavesNoPartialMirror(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not installed: %v", err)
	}
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	mirror := mirrorPathFor(MirrorRoot(mgr.MirrorRoot), upstream)

	// Hang mid-clone after creating the destination's HEAD, mimicking the
	// partial state a SIGKILLed `git clone --bare` leaves behind.
	stub := "#!/bin/sh\nfor last; do :; done\nmkdir -p \"$last\"\n: > \"$last/HEAD\"\nexec sleep 10\n"
	stubPath := installHungGitStub(t, stub)
	overrideGitBudget(t, &gitNetworkTimeout, 200*time.Millisecond)

	if _, err := mgr.EnsureMirror(context.Background(), upstream); err == nil {
		t.Fatalf("EnsureMirror(hung clone) = nil error; want deadline error")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("EnsureMirror(hung clone) = %v; want errors.Is(err, context.DeadlineExceeded)", err)
	}
	if _, err := os.Stat(mirror); !os.IsNotExist(err) {
		t.Fatalf("os.Stat(%s) after killed clone = %v; want IsNotExist (no partial mirror at the final path)", mirror, err)
	}

	// Restore a working git and prove the next attempt recovers.
	if err := os.WriteFile(stubPath, []byte("#!/bin/sh\nexec "+realGit+" \"$@\"\n"), 0o755); err != nil {
		t.Fatalf("rewrite git stub: %v", err)
	}
	overrideGitBudget(t, &gitNetworkTimeout, time.Minute)
	got, err := mgr.EnsureMirror(context.Background(), upstream)
	if err != nil {
		t.Fatalf("EnsureMirror(recovery) = %v; want success after failed first clone", err)
	}
	if _, err := os.Stat(filepath.Join(got, "HEAD")); err != nil {
		t.Fatalf("os.Stat(recovered mirror HEAD) = %v; want bare repo at %s", err, got)
	}
}

// TestRunGitRedactedReclaimsWaitWhenDescendantHoldsPipe pins the WaitDelay
// half of #759: runGitRedacted forwards output through io.Writer pipes, so a
// descendant that inherits the pipe and outlives the killed git process
// (ssh, credential helper) would hold cmd.Wait open to its own exit without
// the grace bound. Deleting cmd.WaitDelay in runGitRedacted stalls this test
// for the stub's full 10s sleep and fails the elapsed assertion.
func TestRunGitRedactedReclaimsWaitWhenDescendantHoldsPipe(t *testing.T) {
	installHungGitStub(t, "#!/bin/sh\nsleep 10 &\nsleep 10\n")
	overrideGitBudget(t, &gitNetworkTimeout, 100*time.Millisecond)
	overrideGitBudget(t, &gitWaitGrace, 200*time.Millisecond)
	start := time.Now()
	err := runGitRedacted(context.Background(), t.TempDir(), "fetch")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("runGitRedacted(hung git with escaped child) = nil error; want deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runGitRedacted(hung git with escaped child) = %v; want errors.Is(err, context.DeadlineExceeded)", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("runGitRedacted(hung git with escaped child) returned after %v; want budget+grace, not the child's exit", elapsed)
	}
}
