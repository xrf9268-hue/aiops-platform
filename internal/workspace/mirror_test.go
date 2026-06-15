package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

// initBareUpstream creates a tiny upstream repo with one commit on `main`
// and returns a `file://` clone URL pointing at it. The repo is bare so
// `git push` can target it without the receiving branch checkout error.
func initBareUpstream(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	bare := filepath.Join(root, "upstream.git")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "init", "-q", "-b", "main", work},
		{"git", "-C", work, "config", "user.email", "u@example.com"},
		{"git", "-C", work, "config", "user.name", "u"},
		{"git", "-C", work, "config", "commit.gpgsign", "false"},
		{"git", "-C", work, "config", "tag.gpgsign", "false"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "-C", work, "add", "."},
		{"git", "-C", work, "commit", "-q", "-m", "seed"},
		{"git", "init", "--bare", "-q", "-b", "main", bare},
		{"git", "-C", work, "remote", "add", "origin", bare},
		{"git", "-C", work, "push", "-q", "origin", "main"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	return "file://" + bare
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	return &Manager{
		Root:       t.TempDir(),
		MirrorRoot: t.TempDir(),
	}
}

func makeTask(id, cloneURL string) task.Task {
	return task.Task{
		ID:            id,
		SourceType:    "linear_issue",
		SourceEventID: id,
		RepoOwner:     "acme",
		RepoName:      "demo",
		CloneURL:      cloneURL,
		BaseBranch:    "main",
		WorkBranch:    "ai/" + id,
	}
}

func TestEnsureMirror_FirstCallClonesSecondCallReuses(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()

	mirror, err := mgr.EnsureMirror(ctx, upstream)
	if err != nil {
		t.Fatalf("first EnsureMirror: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
		t.Fatalf("expected bare repo HEAD at %s: %v", mirror, err)
	}
	// The staging build directory is renamed into place on success; a
	// leftover would mean the deferred failure-path cleanup misfired (#765).
	if _, err := os.Stat(mirror + mirrorStagingSuffix); !os.IsNotExist(err) {
		t.Fatalf("staging dir survived a successful clone: stat err=%v", err)
	}
	// A bare clone has no working tree; assert that absence to confirm
	// we used --mirror rather than a regular clone.
	if _, err := os.Stat(filepath.Join(mirror, ".git")); err == nil {
		t.Fatalf("expected bare repo (no .git/), found one at %s", mirror)
	}

	// Stamp a sentinel file so we can detect whether the second call
	// nuked the cache directory (it must not).
	sentinel := filepath.Join(mirror, "aiops-sentinel")
	if err := os.WriteFile(sentinel, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mirror2, err := mgr.EnsureMirror(ctx, upstream)
	if err != nil {
		t.Fatalf("second EnsureMirror: %v", err)
	}
	if mirror2 != mirror {
		t.Fatalf("mirror path drifted: %s != %s", mirror2, mirror)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("mirror was recreated; sentinel gone: %v", err)
	}
}

func TestEnsureMirror_ConcurrentFirstUseSerializesClone(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()

	const workers = 8
	start := make(chan struct{})
	paths := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			mirror, err := mgr.EnsureMirror(ctx, upstream)
			if err != nil {
				errs <- err
				return
			}
			paths <- mirror
		}()
	}
	close(start)
	wg.Wait()
	close(paths)
	close(errs)

	for err := range errs {
		t.Errorf("EnsureMirror returned error under concurrent first use: %v", err)
	}
	if t.Failed() {
		return
	}
	var want string
	for got := range paths {
		if want == "" {
			want = got
		}
		if got != want {
			t.Fatalf("mirror path drifted under concurrency: got %s want %s", got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(want, "HEAD")); err != nil {
		t.Fatalf("expected bare repo HEAD at %s: %v", want, err)
	}
}

// TestEnsureMirror_HealsLegacyPartialMirrorMissingRefspec is the #765
// legacy-wedge regression: a pre-#764 binary killed mid-clone could leave a
// partial bare repo at the FINAL mirror path — HEAD plus a config carrying
// remote.origin.url, but the bare-clone "do nothing" fetch refspec and no
// refs. The existing-mirror branch must re-assert the remote-tracking
// refspec before its refresh fetch so the mirror heals into a
// worktree-able state instead of wedging every retry until an operator
// deletes the directory.
func TestEnsureMirror_HealsLegacyPartialMirrorMissingRefspec(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()

	mirror := mirrorPathFor(MirrorRoot(mgr.MirrorRoot), upstream)
	if err := os.MkdirAll(filepath.Dir(mirror), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "init", "--bare", "-q", "-b", "main", mirror},
		// `git config remote.origin.url` (not `git remote add`) reproduces
		// the killed-clone config shape: a URL with no fetch refspec.
		{"git", "--git-dir", mirror, "config", "remote.origin.url", upstream},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	// Sanity: the constructed legacy state matches the wedge precondition —
	// HEAD present, no fetch refspec, no refs at all.
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
		t.Fatalf("precondition broken: legacy mirror missing HEAD: %v", err)
	}
	if err := exec.Command("git", "--git-dir", mirror, "config", "--get", "remote.origin.fetch").Run(); err == nil {
		t.Fatal("precondition broken: legacy mirror already has a fetch refspec")
	}
	if err := exec.Command("git", "--git-dir", mirror, "rev-parse", "--verify", "refs/heads/main").Run(); err == nil {
		t.Fatal("precondition broken: legacy mirror already has refs/heads/main")
	}

	tk := makeTask("task-legacy-heal", upstream)
	dir, createdNow, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("PrepareGitWorkspace over legacy partial mirror: %v", err)
	}
	if !createdNow {
		t.Fatal("prepare over legacy partial mirror reported createdNow=false")
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Fatalf("expected README.md inside healed worktree %s: %v", dir, err)
	}
	// The heal is the re-asserted refspec letting the refresh fetch populate
	// the remote-tracking ref the worktree start ref resolves through.
	if err := exec.Command("git", "--git-dir", mirror, "rev-parse", "--verify", "refs/remotes/origin/main").Run(); err != nil {
		t.Fatalf("legacy mirror not healed: refs/remotes/origin/main still missing: %v", err)
	}
}

// TestEnsureMirror_ReplacesMultiValuedFetchRefspec pins the --replace-all on
// the refresh-path refspec re-assert (#765, PR #774 codex P2): when an
// operator has added a second `remote.origin.fetch` value to a healthy
// mirror, plain `git config <key> <value>` refuses multi-valued keys
// ("cannot overwrite multiple values with a single value") and would turn a
// previously fetchable mirror into a permanent EnsureMirror failure.
func TestEnsureMirror_ReplacesMultiValuedFetchRefspec(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()

	mirror, err := mgr.EnsureMirror(ctx, upstream)
	if err != nil {
		t.Fatalf("first EnsureMirror: %v", err)
	}
	if out, err := exec.Command("git", "--git-dir", mirror, "config", "--add", "remote.origin.fetch", "+refs/tags/*:refs/tags/*").CombinedOutput(); err != nil {
		t.Fatalf("add second fetch refspec: %v\n%s", err, out)
	}

	if _, err := mgr.EnsureMirror(ctx, upstream); err != nil {
		t.Fatalf("EnsureMirror over multi-valued fetch refspec: %v", err)
	}
	out, err := exec.Command("git", "--git-dir", mirror, "config", "--get-all", "remote.origin.fetch").Output()
	if err != nil {
		t.Fatalf("read fetch refspecs after refresh: %v", err)
	}
	if got, want := strings.TrimSpace(string(out)), "+refs/heads/*:refs/remotes/origin/*"; got != want {
		t.Fatalf("fetch refspecs after refresh = %q, want exactly %q", got, want)
	}
}

// TestEnsureMirror_FailedCloneRemovesStagingDir is the #765 staging-leak
// regression: a failed first clone — the on-disk shape of a #759 per-op
// timeout killing `git clone --bare` mid-transfer — must not leave the
// `<mirror>.git.staging` build directory behind. Before the deferred
// cleanup, a repo removed from config after one failed first clone leaked
// its staging dir until an operator deleted it by hand.
func TestEnsureMirror_FailedCloneRemovesStagingDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH git shim requires a POSIX shell")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	mgr := newTestManager(t)
	ctx := context.Background()

	// A git shim whose `clone` leaves a partial staging directory and exits
	// non-zero, mimicking a clone killed before it could clean up. Non-clone
	// invocations must not happen on this path; fail loudly if they do.
	shimDir := t.TempDir()
	shim := `#!/bin/sh
if [ "$1" = "clone" ]; then
  for last; do :; done
  mkdir -p "$last"
  printf 'ref: refs/heads/main\n' > "$last/HEAD"
  exit 128
fi
echo "unexpected git invocation: $*" >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cloneURL := "file:///nonexistent/aiops-765-staging.git"
	if _, err := mgr.EnsureMirror(ctx, cloneURL); err == nil {
		t.Fatal("EnsureMirror succeeded under a failing clone shim; want error")
	}
	mirror := mirrorPathFor(MirrorRoot(mgr.MirrorRoot), cloneURL)
	if _, err := os.Stat(mirror + mirrorStagingSuffix); !os.IsNotExist(err) {
		t.Fatalf("staging dir left behind after failed clone: stat err=%v", err)
	}
	if _, err := os.Stat(mirror); !os.IsNotExist(err) {
		t.Fatalf("final mirror path materialized despite failed clone: stat err=%v", err)
	}
}

func TestPrepareGitWorkspace_ConcurrentFirstUseSharesMirror(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()

	const workers = 4
	start := make(chan struct{})
	dirs := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			dir, _, err := mgr.PrepareGitWorkspace(ctx, makeTask(fmt.Sprintf("task-concurrent-%d", i), upstream))
			if err != nil {
				errs <- err
				return
			}
			dirs <- dir
		}()
	}
	close(start)
	wg.Wait()
	close(dirs)
	close(errs)

	for err := range errs {
		t.Errorf("PrepareGitWorkspace returned error under concurrent first use: %v", err)
	}
	if t.Failed() {
		return
	}
	seen := map[string]struct{}{}
	for dir := range dirs {
		if _, ok := seen[dir]; ok {
			t.Fatalf("duplicate worktree dir under concurrency: %s", dir)
		}
		seen[dir] = struct{}{}
		if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
			t.Fatalf("expected README.md inside %s: %v", dir, err)
		}
	}
	mirror := mirrorPathFor(MirrorRoot(mgr.MirrorRoot), upstream)
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
		t.Fatalf("expected shared mirror at %s: %v", mirror, err)
	}
}

func TestPrepareGitWorkspace_IsolatedWorktreesShareMirror(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()

	t1 := makeTask("task-a", upstream)
	t2 := makeTask("task-b", upstream)

	dir1, createdNow, err := mgr.PrepareGitWorkspace(ctx, t1)
	if err != nil {
		t.Fatalf("prepare t1: %v", err)
	}
	if !createdNow {
		t.Fatal("first prepare for t1 reported createdNow=false")
	}
	dir2, createdNow, err := mgr.PrepareGitWorkspace(ctx, t2)
	if err != nil {
		t.Fatalf("prepare t2: %v", err)
	}
	if !createdNow {
		t.Fatal("first prepare for t2 reported createdNow=false")
	}
	if dir1 == dir2 {
		t.Fatalf("expected isolated worktrees, both at %s", dir1)
	}

	// Each worktree must contain the seed file from upstream.
	for _, d := range []string{dir1, dir2} {
		if _, err := os.Stat(filepath.Join(d, "README.md")); err != nil {
			t.Fatalf("expected README.md inside %s: %v", d, err)
		}
	}

	// Writing in t1 must not affect t2.
	marker1 := filepath.Join(dir1, "only-in-1.txt")
	if err := os.WriteFile(marker1, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir2, "only-in-1.txt")); !os.IsNotExist(err) {
		t.Fatalf("worktrees not isolated: file leaked into %s (err=%v)", dir2, err)
	}

	// Both worktrees should be on their respective work branches.
	for _, tc := range []struct {
		dir, want string
	}{{dir1, t1.WorkBranch}, {dir2, t2.WorkBranch}} {
		out, err := exec.Command("git", "-C", tc.dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
		if err != nil {
			t.Fatalf("rev-parse %s: %v", tc.dir, err)
		}
		if got := strings.TrimSpace(string(out)); got != tc.want {
			t.Fatalf("worktree %s on branch %q, want %q", tc.dir, got, tc.want)
		}
	}

	// The mirror cache must contain exactly one bare repo for the
	// upstream URL — that's the whole point of the cache.
	mirror := mirrorPathFor(MirrorRoot(mgr.MirrorRoot), upstream)
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
		t.Fatalf("expected shared mirror at %s: %v", mirror, err)
	}
}

func TestPrepareGitWorkspaceDoesNotExposeBaseRefMarkerInWorktree(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()
	tk := makeTask("task-base-marker", upstream)

	dir, _, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".aiops-base")); !os.IsNotExist(err) {
		t.Fatalf("internal base marker leaked into worktree: %v", err)
	}
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "" {
		t.Fatalf("prepared worktree has visible changes: %q", got)
	}
}

func TestPrepareGitWorkspaceDoesNotSetBaseBranchAsWorkBranchUpstream(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()
	tk := makeTask("task-no-base-upstream", upstream)

	dir, _, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	out, err := cmd.CombinedOutput()
	if err == nil && strings.TrimSpace(string(out)) == "origin/main" {
		t.Fatalf("work branch must not track base branch as upstream")
	}
}

// TestPrepareGitWorkspace_RerunReusesWorkspaceAcrossRuns covers SPEC §9.1:
// workspaces are reused across runs for the same issue. Untracked artifacts
// (cached deps, build outputs) must survive a
// re-prepare, while tracked-file modifications get snapped back to the
// refreshed base ref so the next runner starts from a clean tracked state.
func TestPrepareGitWorkspace_RerunReusesWorkspaceAcrossRuns(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()
	tk := makeTask("task-x", upstream)

	dir, createdNow, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	if !createdNow {
		t.Fatal("first prepare reported createdNow=false")
	}
	// Untracked cache that the next run must inherit.
	if err := os.WriteFile(filepath.Join(dir, "stale.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Local modification to a tracked file that the next run must NOT
	// inherit — the reuse path resets tracked state to origin/<base>.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dir2, createdNow, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("second prepare: %v", err)
	}
	if createdNow {
		t.Fatal("second prepare reported createdNow=true; SPEC §9.1 requires reuse")
	}
	if dir != dir2 {
		t.Fatalf("path changed across runs: %s vs %s", dir, dir2)
	}
	if body, err := os.ReadFile(filepath.Join(dir2, "stale.txt")); err != nil {
		t.Fatalf("untracked file should survive reuse per SPEC §9.1: %v", err)
	} else if string(body) != "x" {
		t.Fatalf("untracked file body = %q, want %q", body, "x")
	}
	if body, err := os.ReadFile(filepath.Join(dir2, "README.md")); err != nil {
		t.Fatalf("read README.md after reuse: %v", err)
	} else if string(body) == "dirty\n" {
		t.Fatalf("tracked-file modification survived reuse; want reset to base")
	}
	// The work branch tip must equal origin/main (i.e., the base ref) so
	// the next run starts from a clean tracked state regardless of what
	// the previous attempt committed on the work branch.
	headOut, err := exec.Command("git", "-C", dir2, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	baseOut, err := exec.Command("git", "-C", dir2, "rev-parse", "origin/main").Output()
	if err != nil {
		t.Fatalf("rev-parse origin/main: %v", err)
	}
	if strings.TrimSpace(string(headOut)) != strings.TrimSpace(string(baseOut)) {
		t.Fatalf("HEAD = %q, want origin/main = %q after reuse", strings.TrimSpace(string(headOut)), strings.TrimSpace(string(baseOut)))
	}
	branchOut, err := exec.Command("git", "-C", dir2, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse --abbrev-ref HEAD: %v", err)
	}
	if got := strings.TrimSpace(string(branchOut)); got != tk.WorkBranch {
		t.Fatalf("worktree on branch %q after reuse, want %q", got, tk.WorkBranch)
	}
}

// TestPrepareGitWorkspaceRejectsWhenPathIsSymlinkOutsideRoot covers the
// cleanup boundary on the recreate path: a symlink planted at the workspace
// path can point outside the workspace root. PrepareGitWorkspace must refuse
// the reuse and fail closed through SafeRemove rather than treating the path as
// a normal cleanup target.
func TestPrepareGitWorkspaceRejectsWhenPathIsSymlinkOutsideRoot(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()
	tk := makeTask("task-symlink", upstream)

	dir, createdNow, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	if !createdNow {
		t.Fatal("first prepare reported createdNow=false")
	}

	// Replace the workspace path with a symlink pointing at an attacker
	// controlled git repo elsewhere on disk. Neither the reuse path nor the
	// recreate cleanup path may follow or delete through this symlink.
	attacker := t.TempDir()
	for _, args := range [][]string{
		{"git", "init", "-q", "-b", "main", attacker},
		{"git", "-C", attacker, "config", "user.email", "u@example.com"},
		{"git", "-C", attacker, "config", "user.name", "u"},
		{"git", "-C", attacker, "config", "commit.gpgsign", "false"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(attacker, "canary.txt"), []byte("attacker"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "-C", attacker, "add", "."},
		{"git", "-C", attacker, "commit", "-q", "-m", "attacker"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	canaryRef, err := exec.Command("git", "-C", attacker, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("attacker rev-parse: %v", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove workspace dir for symlink swap: %v", err)
	}
	if err := os.Symlink(attacker, dir); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	if _, _, err := mgr.PrepareGitWorkspace(ctx, tk); !errors.Is(err, ErrSafeRemoveEscapesRoot) {
		t.Fatalf("second prepare after symlink swap err = %v, want ErrSafeRemoveEscapesRoot", err)
	}

	// The attacker repo must be untouched — its HEAD ref must not have
	// been moved by our `git reset` / `git checkout -B`.
	canaryRefAfter, err := exec.Command("git", "-C", attacker, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("attacker rev-parse after: %v", err)
	}
	if string(canaryRef) != string(canaryRefAfter) {
		t.Fatalf("attacker repo HEAD moved by reuse path: before=%q after=%q", canaryRef, canaryRefAfter)
	}
	if body, err := os.ReadFile(filepath.Join(attacker, "canary.txt")); err != nil {
		t.Fatalf("attacker canary file removed: %v", err)
	} else if string(body) != "attacker" {
		t.Fatalf("attacker canary file modified: %q", body)
	}
	linkInfo, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("symlink path removed after rejected cleanup: %v", err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("workspace path mode = %v, want symlink left in place after rejected cleanup", linkInfo.Mode())
	}
}

// TestPrepareGitWorkspace_RecreatesWhenPathIsForeignGitRepo covers the
// second half of the same gate: an independent git repo planted at the
// workspace path (e.g. a previous agent `git init` that bypassed the
// worktree linkage) still passes `git rev-parse --git-dir`, but its
// git-common-dir does NOT resolve to our mirror. The reuse path must
// reject it and recreate from the mirror.
func TestPrepareGitWorkspace_RecreatesWhenPathIsForeignGitRepo(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()
	tk := makeTask("task-foreign", upstream)

	dir, createdNow, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	if !createdNow {
		t.Fatal("first prepare reported createdNow=false")
	}

	firstCommon, err := exec.Command("git", "-C", dir, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		t.Fatalf("first git-common-dir: %v", err)
	}
	firstCommonReal, err := filepath.EvalSymlinks(strings.TrimSpace(string(firstCommon)))
	if err != nil {
		t.Fatalf("eval first common: %v", err)
	}

	// Rewrite the workspace path as an independent git repo with a
	// distinctive commit. The reuse path must not adopt it.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove workspace dir for foreign-repo swap: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "init", "-q", "-b", "main", dir},
		{"git", "-C", dir, "config", "user.email", "u@example.com"},
		{"git", "-C", dir, "config", "user.name", "u"},
		{"git", "-C", dir, "config", "commit.gpgsign", "false"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "foreign.txt"), []byte("foreign"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "-C", dir, "add", "."},
		{"git", "-C", dir, "commit", "-q", "-m", "foreign"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	dir2, createdNow, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("second prepare after foreign-repo swap: %v", err)
	}
	if dir != dir2 {
		t.Fatalf("path changed across runs: %s vs %s", dir, dir2)
	}
	if !createdNow {
		t.Fatal("second prepare reported createdNow=false; foreign git repo must fall back to recreate")
	}
	if _, err := os.Stat(filepath.Join(dir2, "foreign.txt")); !os.IsNotExist(err) {
		t.Fatalf("foreign repo content survived recreate; stat err=%v", err)
	}
	secondCommon, err := exec.Command("git", "-C", dir2, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		t.Fatalf("second git-common-dir: %v", err)
	}
	secondCommonReal, err := filepath.EvalSymlinks(strings.TrimSpace(string(secondCommon)))
	if err != nil {
		t.Fatalf("eval second common: %v", err)
	}
	if secondCommonReal != firstCommonReal {
		t.Fatalf("recreated workspace not linked back to our mirror: before=%q after=%q", firstCommonReal, secondCommonReal)
	}
}

// TestPrepareGitWorkspace_RecreatesCorruptedWorkdir covers the safety net
// for the reuse path: if the leftover dir at the workspace path is not a
// valid git worktree (crashed prepare, partial rm -rf, hostile leftover),
// PrepareGitWorkspace must fall back to a clean recreate and report
// createdNow=true so the worker fires `after_create` again.
func TestPrepareGitWorkspace_RecreatesCorruptedWorkdir(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()
	tk := makeTask("task-corrupt", upstream)

	dir, createdNow, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	if !createdNow {
		t.Fatal("first prepare reported createdNow=false")
	}
	// Corrupt the worktree linkage so `git rev-parse --git-dir` fails.
	gitLink := filepath.Join(dir, ".git")
	if err := os.RemoveAll(gitLink); err != nil {
		t.Fatalf("remove .git linkage: %v", err)
	}

	dir2, createdNow, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("second prepare after corruption: %v", err)
	}
	if dir != dir2 {
		t.Fatalf("path changed across runs: %s vs %s", dir, dir2)
	}
	if !createdNow {
		t.Fatal("second prepare reported createdNow=false; corrupted leftover must fall back to recreate")
	}
	if _, err := os.Stat(filepath.Join(dir2, ".git")); err != nil {
		t.Fatalf("recreated workdir missing .git linkage: %v", err)
	}
}

// TestPrepareGitWorkspace_RecreatesWhenPathIsSymlinkToPeerWorktree covers
// the load-bearing scenario for the symlink gate: a symlink planted at the
// workspace path that points at a peer worktree of the SAME mirror.
// `git rev-parse --git-common-dir` would happily report the matching
// mirror (so the foreign-repo gate would not trip), and without the
// `os.Lstat` symlink check the reuse-path `git reset` / `git checkout -B`
// would silently mutate the peer worktree's tracked state and work-branch
// ref. The gate must refuse the reuse, recreate a fresh worktree, and
// leave the peer worktree's branch tip and tracked files untouched.
func TestPrepareGitWorkspace_RecreatesWhenPathIsSymlinkToPeerWorktree(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()
	victim := makeTask("task-sibling-victim", upstream)
	pivot := makeTask("task-sibling-pivot", upstream)

	victimDir, _, err := mgr.PrepareGitWorkspace(ctx, victim)
	if err != nil {
		t.Fatalf("prepare victim: %v", err)
	}
	if err := configureGitIdentity(victimDir); err != nil {
		t.Fatalf("configure victim identity: %v", err)
	}
	// Commit a distinctive tracked change on the victim's work branch so a
	// stray `git checkout -B <pivot-work-branch>` inside the victim would
	// either move the branch ref off the commit or rewrite the tracked
	// file. Either mutation is observable from the bare mirror's refs and
	// from the victim worktree's content.
	if err := os.WriteFile(filepath.Join(victimDir, "victim.txt"), []byte("victim-content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "-C", victimDir, "add", "victim.txt"},
		{"git", "-C", victimDir, "commit", "-q", "-m", "victim commit"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	victimHeadBefore, err := exec.Command("git", "-C", victimDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("victim rev-parse HEAD: %v", err)
	}

	pivotDir, _, err := mgr.PrepareGitWorkspace(ctx, pivot)
	if err != nil {
		t.Fatalf("prepare pivot: %v", err)
	}
	if pivotDir == victimDir {
		t.Fatalf("pivot and victim collapsed to same workdir %q", pivotDir)
	}
	// Replace the pivot workspace path with a symlink pointing at the
	// victim's peer worktree (same mirror).
	if err := os.RemoveAll(pivotDir); err != nil {
		t.Fatalf("remove pivot dir for symlink swap: %v", err)
	}
	if err := os.Symlink(victimDir, pivotDir); err != nil {
		t.Fatalf("plant sibling symlink: %v", err)
	}

	pivotDir2, createdNow, err := mgr.PrepareGitWorkspace(ctx, pivot)
	if err != nil {
		t.Fatalf("re-prepare pivot after symlink swap: %v", err)
	}
	if pivotDir2 != pivotDir {
		t.Fatalf("pivot dir changed: %s vs %s", pivotDir2, pivotDir)
	}
	if !createdNow {
		t.Fatal("re-prepare pivot reported createdNow=false; sibling symlink must fall through to recreate")
	}

	// Victim worktree's branch tip and tracked content must be unchanged
	// — neither the pivot's `git reset` nor the pivot's `git checkout -B`
	// can have been redirected into it.
	victimHeadAfter, err := exec.Command("git", "-C", victimDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("victim rev-parse HEAD after: %v", err)
	}
	if string(victimHeadAfter) != string(victimHeadBefore) {
		t.Fatalf("victim work branch moved by pivot reuse: before=%q after=%q", victimHeadBefore, victimHeadAfter)
	}
	if body, err := os.ReadFile(filepath.Join(victimDir, "victim.txt")); err != nil {
		t.Fatalf("victim tracked file removed: %v", err)
	} else if string(body) != "victim-content\n" {
		t.Fatalf("victim tracked file mutated: %q", body)
	}

	// The recreated pivot worktree must be a real worktree (not a
	// symlink) and on the pivot's work branch.
	lstatInfo, err := os.Lstat(pivotDir2)
	if err != nil {
		t.Fatalf("lstat recreated pivot: %v", err)
	}
	if lstatInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("recreated pivot is still a symlink: %v", lstatInfo.Mode())
	}
	branchOut, err := exec.Command("git", "-C", pivotDir2, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("pivot rev-parse --abbrev-ref HEAD: %v", err)
	}
	if got := strings.TrimSpace(string(branchOut)); got != pivot.WorkBranch {
		t.Fatalf("recreated pivot on branch %q, want %q", got, pivot.WorkBranch)
	}
}

// TestPrepareGitWorkspace_ReuseSurvivesIntentToAddEntries locks the reuse-path
// `git reset --quiet HEAD -- .` invariant at the workspace boundary. If a prior
// run leaves untracked files staged as intent-to-add (empty-blob) entries in
// the index — e.g. a hook running `git add -N` / `git add --intent-to-add` —
// then without the reset the next prepare's `git checkout --force -B` would
// treat those entries as "files-in-index-not-in-target-ref" and delete them
// from the working tree, silently nuking cached deps and hook artifacts that
// SPEC §9.1 reuse semantics promise to preserve.
func TestPrepareGitWorkspace_ReuseSurvivesIntentToAddEntries(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()
	tk := makeTask("task-intent-to-add", upstream)

	dir, createdNow, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	if !createdNow {
		t.Fatal("first prepare reported createdNow=false")
	}
	// Cached untracked artifacts a prior agent run would leave behind
	// (build outputs, hook caches, etc.).
	if err := os.WriteFile(filepath.Join(dir, "cached-dep.txt"), []byte("cache\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".aiops"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aiops", "cache-marker.txt"), []byte("cache\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Stage the untracked files as intent-to-add (empty-blob) entries, the
	// condition the reuse reset must clear. The next prepare's reset must drop
	// those entries before the checkout, or `git checkout --force -B` would
	// treat them as removable.
	if err := runGit(ctx, dir, "add", "--intent-to-add", "--all"); err != nil {
		t.Fatalf("git add --intent-to-add: %v", err)
	}

	dir2, createdNow, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("second prepare: %v", err)
	}
	if createdNow {
		t.Fatal("second prepare reported createdNow=true; SPEC §9.1 reuse expected")
	}
	if dir != dir2 {
		t.Fatalf("workspace dir changed across runs: %s vs %s", dir, dir2)
	}
	for _, rel := range []string{"cached-dep.txt", filepath.Join(".aiops", "cache-marker.txt")} {
		if body, err := os.ReadFile(filepath.Join(dir2, rel)); err != nil {
			t.Fatalf("untracked artifact %q removed across reuse despite intent-to-add gate: %v", rel, err)
		} else if len(body) == 0 {
			t.Fatalf("untracked artifact %q truncated on reuse: empty body", rel)
		}
	}
}

func TestPrepareGitWorkspaceProtectsSensitiveAiopsArtifacts(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()
	tk := makeTask("task-sensitive-artifacts", upstream)

	dir, _, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("PrepareGitWorkspace: %v", err)
	}
	gitDirOut, err := exec.Command("git", "-C", dir, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		t.Fatalf("git rev-parse --absolute-git-dir: %v", err)
	}
	hooksPathOut, err := exec.Command("git", "-C", dir, "config", "--worktree", "--get", "core.hooksPath").Output()
	if err != nil {
		t.Fatalf("git config --worktree core.hooksPath: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(hooksPathOut)), strings.TrimSpace(string(gitDirOut))) {
		t.Fatalf("hooksPath = %q, want under worktree git dir %q", hooksPathOut, gitDirOut)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".aiops"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range sensitiveArtifactPaths {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte("sensitive\n"), SensitiveArtifactFileMode); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	if err := runGitQuiet(ctx, dir, "add", "."); err != nil {
		t.Fatalf("git add .: %v", err)
	}
	cmd := exec.Command("git", "diff", "--cached", "--name-only", "-z", "--")
	cmd.Dir = dir
	staged, err := cmd.Output()
	if err != nil {
		t.Fatalf("git diff --cached --name-only: %v", err)
	}
	for _, rel := range sensitiveArtifactPaths {
		if strings.Contains("\x00"+string(staged), "\x00"+rel+"\x00") {
			t.Fatalf("sensitive artifact %q was staged; staged=%v", rel, staged)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".aiops", "PLAN.md"), []byte("handoff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := configureGitIdentity(dir); err != nil {
		t.Fatalf("configure identity: %v", err)
	}
	if err := runGitQuiet(ctx, dir, "add", "."); err != nil {
		t.Fatalf("git add allowlist: %v", err)
	}
	if out, err := exec.Command("git", "-C", dir, "commit", "-m", "allowed handoff artifacts").CombinedOutput(); err != nil {
		t.Fatalf("commit allowlist: %v\n%s", err, out)
	}

	if err := os.MkdirAll(filepath.Join(dir, ".aiops", "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aiops", "logs", "runner.log"), []byte("log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runGitQuiet(ctx, dir, "add", "."); err != nil {
		t.Fatalf("git add ignored log artifact: %v", err)
	}
	cmd = exec.Command("git", "diff", "--cached", "--name-only", "--", ".aiops/logs/runner.log")
	cmd.Dir = dir
	staged, err = cmd.Output()
	if err != nil {
		t.Fatalf("git diff ignored log artifact: %v", err)
	}
	if len(staged) != 0 {
		t.Fatalf("ignored log artifact was staged: %q", staged)
	}
	if err := runGitQuiet(ctx, dir, "add", "-f", ".aiops/PROMPT.md", ".aiops/logs/runner.log"); err != nil {
		t.Fatalf("force-add sensitive artifact: %v", err)
	}
	commitCmd := exec.Command("git", "commit", "-m", "blocked")
	commitCmd.Dir = dir
	out, err := commitCmd.CombinedOutput()
	if err == nil {
		t.Fatal("git commit succeeded, want pre-commit hook failure")
	}
	output := string(out)
	for _, want := range []string{".aiops/PROMPT.md", ".aiops/logs/runner.log"} {
		if !strings.Contains(output, want) {
			t.Fatalf("git commit output = %q, want %s", output, want)
		}
	}
}

func TestWriteSensitiveArtifactRejectsSymlinkedAiopsPaths(t *testing.T) {
	root := t.TempDir()
	aiopsDir := filepath.Join(root, ".aiops")
	target := filepath.Join(root, "PROMPT.md")
	if err := os.Symlink(".", aiopsDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := WriteSensitiveArtifact(filepath.Join(aiopsDir, "PROMPT.md"), []byte("secret\n")); err == nil {
		t.Fatal("WriteSensitiveArtifact followed symlinked .aiops dir")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("symlinked .aiops wrote outside private dir: %v", err)
	}
	if err := os.Remove(aiopsDir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(aiopsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../PROMPT.md", filepath.Join(aiopsDir, "PROMPT.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := WriteSensitiveArtifact(filepath.Join(aiopsDir, "PROMPT.md"), []byte("secret\n")); err == nil {
		t.Fatal("WriteSensitiveArtifact followed symlinked sensitive artifact")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("symlinked artifact wrote outside private dir: %v", err)
	}
}

func TestWriteSensitiveArtifactReplacesHardLinkedArtifact(t *testing.T) {
	root := t.TempDir()
	aiopsDir := filepath.Join(root, ".aiops")
	if err := os.MkdirAll(aiopsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "PROMPT.md")
	if err := os.WriteFile(target, []byte("public\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(aiopsDir, "PROMPT.md")
	if err := os.Link(target, artifact); err != nil {
		t.Skipf("hardlink unavailable: %v", err)
	}

	if err := WriteSensitiveArtifact(artifact, []byte("secret\n")); err != nil {
		t.Fatalf("WriteSensitiveArtifact: %v", err)
	}
	if body, err := os.ReadFile(target); err != nil {
		t.Fatal(err)
	} else if string(body) != "public\n" {
		t.Fatalf("hardlinked public target was modified: %q", body)
	}
	if body, err := os.ReadFile(artifact); err != nil {
		t.Fatal(err)
	} else if string(body) != "secret\n" {
		t.Fatalf("artifact body = %q, want secret", body)
	}
}

// TestPrepareGitWorkspace_StartRefFallsBackToBareBaseBranchName covers the
// `startRef = t.BaseBranch` fallback in PrepareGitWorkspace. The production
// path (HTTPS clone via EnsureMirror) always populates
// `refs/remotes/origin/<base>` through the post-clone fetch refspec
// rewrite, so `git rev-parse --verify origin/<base>` succeeds and the
// fallback never fires in production. This test drives the fallback
// explicitly by deleting the base branch on the UPSTREAM so the next
// `ensureMirrorLocked`'s `fetch --prune` removes `refs/remotes/origin/main`
// from the mirror while the mirror keeps its bare `refs/heads/main` from
// the original `git clone --bare`. (The earlier construction — unsetting
// `remote.origin.fetch` on the mirror — stopped driving the fallback when
// the existing-mirror branch began re-asserting the refspec before every
// refresh fetch, #765.) This guards the fallback against a future refactor
// of EnsureMirror's refspec layout and ensures `file://` test fixtures
// continue to work even when the mirror only carries `refs/heads/<base>`.
func TestPrepareGitWorkspace_StartRefFallsBackToBareBaseBranchName(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()

	// Prime the mirror via a first task so the bare clone + refspec config
	// exist on disk.
	primer := makeTask("task-primer", upstream)
	if _, _, err := mgr.PrepareGitWorkspace(ctx, primer); err != nil {
		t.Fatalf("prime mirror: %v", err)
	}

	mirror := mirrorPathFor(MirrorRoot(mgr.MirrorRoot), upstream)
	// Capture the expected start-ref commit (refs/heads/main on the bare
	// mirror) so we can assert the recreated worktree's HEAD matches it
	// once the fallback resolves to bare `main` instead of `origin/main`.
	mainCommit, err := exec.Command("git", "--git-dir", mirror, "rev-parse", "refs/heads/main").Output()
	if err != nil {
		t.Fatalf("mirror rev-parse refs/heads/main: %v", err)
	}
	// Delete the base branch on the upstream so the next prepare's
	// `git fetch --prune --tags origin` prunes `refs/remotes/origin/main`
	// from the mirror; the mirror keeps its bare `refs/heads/main` from the
	// original `git clone --bare` because fetch never touches local heads.
	upstreamPath := strings.TrimPrefix(upstream, "file://")
	if out, err := exec.Command("git", "--git-dir", upstreamPath, "update-ref", "-d", "refs/heads/main").CombinedOutput(); err != nil {
		t.Fatalf("delete upstream refs/heads/main: %v\n%s", err, out)
	}
	// Sanity: confirm the precondition the fallback branch needs — the
	// upstream branch is gone but the mirror's bare head ref survives.
	if err := exec.Command("git", "--git-dir", upstreamPath, "rev-parse", "--verify", "refs/heads/main").Run(); err == nil {
		t.Fatal("precondition broken: refs/heads/main still resolves on the upstream")
	}
	if err := exec.Command("git", "--git-dir", mirror, "rev-parse", "--verify", "refs/heads/main").Run(); err != nil {
		t.Fatalf("precondition broken: refs/heads/main missing on mirror: %v", err)
	}

	// New task → first-touch path runs through the startRef resolution.
	// The refresh fetch prunes `origin/main`, the `git rev-parse --verify
	// origin/main` gate fails, and startRef falls back to bare `main`; the
	// subsequent `git worktree add ... main` must succeed against the
	// mirror's `refs/heads/main`.
	tk := makeTask("task-fallback", upstream)
	dir, createdNow, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("prepare with startRef fallback: %v", err)
	}
	if !createdNow {
		t.Fatal("first prepare for new task reported createdNow=false")
	}
	// Post-condition: the prune actually removed the remote-tracking ref,
	// so the prepare above really exercised the fallback rather than
	// resolving `origin/main`.
	if err := exec.Command("git", "--git-dir", mirror, "rev-parse", "--verify", "refs/remotes/origin/main").Run(); err == nil {
		t.Fatal("fetch --prune kept refs/remotes/origin/main; the fallback was not driven")
	}
	headOut, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("worktree rev-parse HEAD: %v", err)
	}
	if strings.TrimSpace(string(headOut)) != strings.TrimSpace(string(mainCommit)) {
		t.Fatalf("worktree HEAD = %q, want bare-main commit %q after startRef fallback",
			strings.TrimSpace(string(headOut)), strings.TrimSpace(string(mainCommit)))
	}
	branchOut, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("worktree rev-parse --abbrev-ref HEAD: %v", err)
	}
	if got := strings.TrimSpace(string(branchOut)); got != tk.WorkBranch {
		t.Fatalf("worktree on branch %q, want %q", got, tk.WorkBranch)
	}
}

// TestPrepareGitWorkspace_ReclaimsBranchAfterWorkspaceRootChange is the #854
// regression: the bare mirror cache is keyed by clone URL, not by
// workspace.root, so it outlives a workspace.root change. A per-issue worktree
// ai/N registered at the OLD root's path — still on disk, so `worktree prune`
// keeps it — made the next dispatch's `git worktree add -B ai/N <new-root-path>`
// fail with `fatal: 'ai/N' is already used by worktree at '<old-root-path>'`,
// wedging the run in Failed on every retry. Preparing the SAME issue/branch at a
// NEW root that shares the SAME bare mirror must reclaim the stale registration
// and reach a usable worktree under the new root.
func TestPrepareGitWorkspace_ReclaimsBranchAfterWorkspaceRootChange(t *testing.T) {
	upstream := initBareUpstream(t)
	mirrorRoot := t.TempDir() // shared across both roots, as in production
	ctx := context.Background()
	tk := makeTask("2", upstream) // WorkBranch ai/2, matching the issue's repro

	rootA := t.TempDir()
	mgrA := &Manager{Root: rootA, MirrorRoot: mirrorRoot}
	dirA, createdNow, err := mgrA.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("prepare at root A: %v", err)
	}
	if !createdNow {
		t.Fatal("first prepare at root A reported createdNow=false")
	}
	// Leave dirA on disk — the load-bearing precondition. With the old worktree
	// dir gone, `worktree prune` alone already recovers; the bug needs the stale
	// dir present so prune keeps the colliding registration.
	if _, err := os.Stat(dirA); err != nil {
		t.Fatalf("root A worktree dir missing before root change: %v", err)
	}

	rootB := t.TempDir()
	mgrB := &Manager{Root: rootB, MirrorRoot: mirrorRoot} // SAME mirror, new root
	dirB, createdNow, err := mgrB.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		// Pre-fix: `git worktree add -B ai/2` collides with the root-A registration.
		t.Fatalf("prepare same issue at new root over shared mirror: %v", err)
	}
	if !createdNow {
		t.Fatal("prepare at root B reported createdNow=false; new-root worktree must be created fresh")
	}
	if dirB == dirA {
		t.Fatalf("root B workdir collapsed onto root A path %q", dirA)
	}
	// The reclaimed worktree must be usable: seed file present and on the work branch.
	if _, err := os.Stat(filepath.Join(dirB, "README.md")); err != nil {
		t.Fatalf("expected README.md inside reclaimed worktree %s: %v", dirB, err)
	}
	branchOut, err := exec.Command("git", "-C", dirB, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse --abbrev-ref HEAD: %v", err)
	}
	if got := strings.TrimSpace(string(branchOut)); got != tk.WorkBranch {
		t.Fatalf("reclaimed worktree on branch %q, want %q", got, tk.WorkBranch)
	}
	// The mirror must now register ai/2 at the NEW path; the stale root-A
	// registration is gone. Compare via EvalSymlinks because git records the
	// canonicalized worktree path (e.g. /private/var on macOS).
	mirror := mirrorPathFor(MirrorRoot(mirrorRoot), upstream)
	holder := worktreePathForBranch(ctx, mirror, tk.WorkBranch)
	if holder == "" {
		t.Fatal("ai/2 no longer registered after reclaim + recreate")
	}
	holderReal, err := filepath.EvalSymlinks(holder)
	if err != nil {
		t.Fatalf("eval holder %q: %v", holder, err)
	}
	dirBReal, err := filepath.EvalSymlinks(dirB)
	if err != nil {
		t.Fatalf("eval dirB %q: %v", dirB, err)
	}
	if holderReal != dirBReal {
		t.Fatalf("ai/2 registered at %q (real %q), want reclaimed at new path %q (real %q)", holder, holderReal, dirB, dirBReal)
	}
}

func TestPrepareGitWorkspace_ReclaimRefusesLiveOwnedForeignRootWorktree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("worktree ownership uses flock on Unix")
	}
	upstream := initBareUpstream(t)
	mirrorRoot := t.TempDir()
	ctx := context.Background()
	tk := makeTask("2", upstream)

	rootA := t.TempDir()
	mgrA := &Manager{Root: rootA, MirrorRoot: mirrorRoot}
	ownedA, err := mgrA.PrepareGitWorkspaceOwned(ctx, tk)
	if err != nil {
		t.Fatalf("prepare owned at root A: %v", err)
	}
	defer ownedA.Release()
	marker := filepath.Join(ownedA.Workdir, "live-peer-marker.txt")
	if err := os.WriteFile(marker, []byte("live\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rootB := t.TempDir()
	mgrB := &Manager{Root: rootB, MirrorRoot: mirrorRoot}
	if _, _, err := mgrB.PrepareGitWorkspace(ctx, tk); err == nil {
		t.Fatal("prepare same branch at new root succeeded while the foreign-root owner lock was held; reclaim must fail safely")
	}
	if body, err := os.ReadFile(marker); err != nil {
		t.Fatalf("live peer worktree deleted by reclaim: %v", err)
	} else if string(body) != "live\n" {
		t.Fatalf("live peer marker mutated by reclaim: %q", body)
	}

	mirror := mirrorPathFor(MirrorRoot(mirrorRoot), upstream)
	holder := worktreePathForBranch(ctx, mirror, tk.WorkBranch)
	holderReal, err := filepath.EvalSymlinks(holder)
	if err != nil {
		t.Fatalf("eval holder %q: %v", holder, err)
	}
	ownedReal, err := filepath.EvalSymlinks(ownedA.Workdir)
	if err != nil {
		t.Fatalf("eval owned workdir %q: %v", ownedA.Workdir, err)
	}
	if holderReal != ownedReal {
		t.Fatalf("live ai/2 registration moved: holder=%q (real %q), want %q (real %q)", holder, holderReal, ownedA.Workdir, ownedReal)
	}
}

func TestPrepareGitWorkspace_ReclaimsReleasedOwnedWorktreeAfterRootChange(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("worktree ownership uses flock on Unix")
	}
	upstream := initBareUpstream(t)
	mirrorRoot := t.TempDir()
	ctx := context.Background()
	tk := makeTask("2", upstream)

	rootA := t.TempDir()
	mgrA := &Manager{Root: rootA, MirrorRoot: mirrorRoot}
	ownedA, err := mgrA.PrepareGitWorkspaceOwned(ctx, tk)
	if err != nil {
		t.Fatalf("prepare owned at root A: %v", err)
	}
	oldDir := ownedA.Workdir
	ownedA.Release()

	rootB := t.TempDir()
	mgrB := &Manager{Root: rootB, MirrorRoot: mirrorRoot}
	dirB, createdNow, err := mgrB.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("prepare same issue at new root after owner release: %v", err)
	}
	if !createdNow {
		t.Fatal("prepare after owner release reported createdNow=false")
	}
	if dirB == oldDir {
		t.Fatalf("root B workdir collapsed onto old path %q", oldDir)
	}
}

// TestPrepareGitWorkspace_ReclaimLeavesPeerBranchWorktreeIntact pins the #854
// safety contract (acceptance criterion 4): reclaiming a stale foreign-root
// registration must be branch-scoped — it may drop only the EXACT colliding
// work branch, never a peer worktree for a different issue that shares the same
// bare mirror. A blanket "drop everything outside the current root" would wipe a
// live peer's branch ref and working tree; this test fails under that mutation.
func TestPrepareGitWorkspace_ReclaimLeavesPeerBranchWorktreeIntact(t *testing.T) {
	upstream := initBareUpstream(t)
	mirrorRoot := t.TempDir()
	ctx := context.Background()

	rootA := t.TempDir()
	mgrA := &Manager{Root: rootA, MirrorRoot: mirrorRoot}
	// Issue N (the one whose root changes) and a peer issue M, both prepared
	// under root A against the shared mirror; both worktree dirs stay on disk.
	taskN := makeTask("2", upstream) // ai/2
	taskM := makeTask("7", upstream) // ai/7, the peer
	if _, _, err := mgrA.PrepareGitWorkspace(ctx, taskN); err != nil {
		t.Fatalf("prepare issue N at root A: %v", err)
	}
	peerDir, _, err := mgrA.PrepareGitWorkspace(ctx, taskM)
	if err != nil {
		t.Fatalf("prepare peer issue M at root A: %v", err)
	}
	// Commit a distinctive change on the peer's work branch so any stray reclaim
	// of its registration would be observable as a moved branch ref / lost file.
	if err := configureGitIdentity(peerDir); err != nil {
		t.Fatalf("configure peer identity: %v", err)
	}
	if err := os.WriteFile(filepath.Join(peerDir, "peer.txt"), []byte("peer-content\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "-C", peerDir, "add", "peer.txt"},
		{"git", "-C", peerDir, "commit", "-q", "-m", "peer commit"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	peerHeadBefore, err := exec.Command("git", "-C", peerDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("peer rev-parse HEAD: %v", err)
	}

	// Re-prepare ONLY issue N at a new root sharing the same mirror. The reclaim
	// must drop ai/2's stale root-A registration but leave the peer ai/7 alone.
	rootB := t.TempDir()
	mgrB := &Manager{Root: rootB, MirrorRoot: mirrorRoot}
	if _, _, err := mgrB.PrepareGitWorkspace(ctx, taskN); err != nil {
		t.Fatalf("re-prepare issue N at new root: %v", err)
	}

	// Peer worktree dir, tracked content, and branch tip must all be untouched.
	if body, err := os.ReadFile(filepath.Join(peerDir, "peer.txt")); err != nil {
		t.Fatalf("peer tracked file removed by reclaim: %v", err)
	} else if string(body) != "peer-content\n" {
		t.Fatalf("peer tracked file mutated by reclaim: %q", body)
	}
	peerHeadAfter, err := exec.Command("git", "-C", peerDir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("peer rev-parse HEAD after: %v", err)
	}
	if string(peerHeadAfter) != string(peerHeadBefore) {
		t.Fatalf("peer work branch moved by reclaim: before=%q after=%q", peerHeadBefore, peerHeadAfter)
	}
	// The peer's registration must still resolve to its original dir.
	mirror := mirrorPathFor(MirrorRoot(mirrorRoot), upstream)
	holder := worktreePathForBranch(ctx, mirror, taskM.WorkBranch)
	holderReal, err := filepath.EvalSymlinks(holder)
	if err != nil {
		t.Fatalf("eval peer holder %q: %v", holder, err)
	}
	peerReal, err := filepath.EvalSymlinks(peerDir)
	if err != nil {
		t.Fatalf("eval peerDir %q: %v", peerDir, err)
	}
	if holderReal != peerReal {
		t.Fatalf("peer ai/7 registration changed: holder=%q (real %q), want %q (real %q)", holder, holderReal, peerDir, peerReal)
	}
}

// TestPrepareGitWorkspace_ReclaimRefusesWhenHolderOverlapsNewRoot pins the #869
// path-overlap guard. If the operator points workspace.root AT the stale
// worktree (root == holder) or INSIDE it (holder is an ancestor of root), a
// naive "reclaim anything outside the current root" would os.RemoveAll the
// holder — deleting the current root and everything under it. Reclaim must
// refuse such a holder and fail safely on the collision. The two cases pin both
// halves of the guard (`realHolder == realRoot` and `pathContainedUnder`).
func TestPrepareGitWorkspace_ReclaimRefusesWhenHolderOverlapsNewRoot(t *testing.T) {
	upstream := initBareUpstream(t)
	ctx := context.Background()
	cases := []struct {
		name  string
		rootB func(dirA string) string
	}{
		{"new root equals the stale worktree", func(dirA string) string { return dirA }},
		{"new root nested inside the stale worktree", func(dirA string) string { return filepath.Join(dirA, "nested-root") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mirrorRoot := t.TempDir()
			tk := makeTask("2", upstream)
			rootA := t.TempDir()
			mgrA := &Manager{Root: rootA, MirrorRoot: mirrorRoot}
			dirA, _, err := mgrA.PrepareGitWorkspace(ctx, tk)
			if err != nil {
				t.Fatalf("prepare at root A: %v", err)
			}
			// A marker in the stale worktree; a wrongful os.RemoveAll(holder) erases it.
			marker := filepath.Join(dirA, "do-not-delete.txt")
			if err := os.WriteFile(marker, []byte("keep\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			mgrB := &Manager{Root: tc.rootB(dirA), MirrorRoot: mirrorRoot}
			if _, _, err := mgrB.PrepareGitWorkspace(ctx, tk); err == nil {
				t.Fatal("prepare under a root overlapping the stale worktree succeeded; reclaim must refuse an at-or-above-root holder and fail safely on the collision")
			}
			// The stale worktree (== or an ancestor of the new root) must be intact.
			if body, err := os.ReadFile(marker); err != nil {
				t.Fatalf("overlapping holder content deleted by reclaim: %v", err)
			} else if string(body) != "keep\n" {
				t.Fatalf("overlapping holder content mutated: %q", body)
			}
		})
	}
}

// TestPrepareGitWorkspace_ReclaimsWhenNewRootIsAncestorOfOldWorktree pins the
// #854 ancestor-root-change case (codex P2 on 44fcfda): when workspace.root is
// moved UP to an ANCESTOR of the old worktree, the stale ai/N registration sits
// INSIDE the new root. It must still be reclaimed — it is this worker's own
// stale subdir worktree, never a live peer (a separate AIOPS_MIRROR_ROOT per
// worker means a peer never shares our mirror). A guard that refused every
// in-root holder would skip the reclaim and wedge the worktree-add collision.
func TestPrepareGitWorkspace_ReclaimsWhenNewRootIsAncestorOfOldWorktree(t *testing.T) {
	upstream := initBareUpstream(t)
	mirrorRoot := t.TempDir()
	ctx := context.Background()
	tk := makeTask("2", upstream)

	rootB := filepath.Join(t.TempDir(), "ws") // the new (ancestor) root
	rootA := filepath.Join(rootB, "maker")    // old root, a subdir of rootB
	mgrA := &Manager{Root: rootA, MirrorRoot: mirrorRoot}
	dirA, _, err := mgrA.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("prepare at old root A: %v", err)
	}
	if _, err := os.Stat(dirA); err != nil {
		t.Fatalf("old worktree missing before root change: %v", err)
	}

	mgrB := &Manager{Root: rootB, MirrorRoot: mirrorRoot}
	dirB, createdNow, err := mgrB.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		// With an over-broad "refuse every in-root holder" guard, the reclaim is
		// skipped and `git worktree add -B ai/2` collides.
		t.Fatalf("prepare at ancestor root B over shared mirror: %v", err)
	}
	if !createdNow {
		t.Fatal("prepare at ancestor root B reported createdNow=false")
	}
	if dirB == dirA {
		t.Fatalf("root B workdir collapsed onto old path %q", dirA)
	}
	if _, err := os.Stat(filepath.Join(dirB, "README.md")); err != nil {
		t.Fatalf("expected README.md inside reclaimed worktree %s: %v", dirB, err)
	}
	branchOut, err := exec.Command("git", "-C", dirB, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		t.Fatalf("rev-parse --abbrev-ref HEAD: %v", err)
	}
	if got := strings.TrimSpace(string(branchOut)); got != tk.WorkBranch {
		t.Fatalf("reclaimed worktree on branch %q, want %q", got, tk.WorkBranch)
	}
}

// TestPrepareGitWorkspace_ReclaimDoesNotDeleteRepurposedHolderPath pins the
// #869 fail-closed rule (codex P2 on 9d91779): reclaim deletes a foreign holder
// only after confirming it is still a live worktree of our mirror. If the old
// worktree path was repurposed for non-worktree data after a workspace.root
// change (here: its .git is removed and operator data planted), reclaim must NOT
// erase what now lives there — it fails closed and lets the collision surface.
func TestPrepareGitWorkspace_ReclaimDoesNotDeleteRepurposedHolderPath(t *testing.T) {
	upstream := initBareUpstream(t)
	mirrorRoot := t.TempDir()
	ctx := context.Background()
	tk := makeTask("2", upstream)

	rootA := t.TempDir()
	mgrA := &Manager{Root: rootA, MirrorRoot: mirrorRoot}
	dirA, _, err := mgrA.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("prepare at root A: %v", err)
	}
	// Repurpose the old worktree path: drop its .git link (no longer a valid
	// worktree) and plant operator data the reclaim must not delete. The mirror's
	// admin registration for ai/2 still points here, so reclaim still selects it.
	if err := os.RemoveAll(filepath.Join(dirA, ".git")); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dirA, "operator-data.txt")
	if err := os.WriteFile(keep, []byte("precious\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rootB := t.TempDir()
	mgrB := &Manager{Root: rootB, MirrorRoot: mirrorRoot}
	// Prep may fail (the lingering registration collides) — the invariant is that
	// the repurposed data is never deleted, regardless of prep outcome.
	_, _, _ = mgrB.PrepareGitWorkspace(ctx, tk)
	if body, err := os.ReadFile(keep); err != nil {
		t.Fatalf("repurposed holder data deleted by reclaim: %v", err)
	} else if string(body) != "precious\n" {
		t.Fatalf("repurposed holder data mutated: %q", body)
	}
}

// TestIsForeignRootHolder pins the symlink-correct, two-case root scope the #854
// reclaim rests on (clean-code rule 11: mutation-test the wiring seam). A holder
// is reclaimable UNLESS it is this issue's own workdir, or it is / contains the
// current root. git records the canonicalized worktree path (/private/var on
// macOS) while root and workdir arrive in the operator-supplied symlinked form;
// without evalSymlinksOr a same-real-path holder would read as foreign.
func TestIsForeignRootHolder(t *testing.T) {
	realRoot := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	// workspace.root is supplied as the symlink; workdir lives under it.
	mkdir := func(parent, branchID string) string {
		p := filepath.Join(parent, "acme", "demo", "linear_issue", branchID)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	workdir := mkdir(linkRoot, "2")

	// Same real dir as workdir (canonicalized through the root symlink): NOT
	// foreign. Reverting evalSymlinksOr to raw compares fails this — the symlink seam.
	canonSelf := filepath.Join(realRoot, "acme", "demo", "linear_issue", "2")
	if isForeignRootHolder(linkRoot, workdir, canonSelf) {
		t.Errorf("same-real-path holder %q (canonicalized via root symlink) misclassified as foreign", canonSelf)
	}
	// The current root itself, and an ancestor of it: NOT foreign — reclaiming
	// would os.RemoveAll the root / an ancestor of the workdir (the overlap guard).
	if isForeignRootHolder(linkRoot, workdir, realRoot) {
		t.Errorf("holder == current root misclassified as foreign")
	}
	if ancestor := filepath.Dir(realRoot); isForeignRootHolder(linkRoot, workdir, ancestor) {
		t.Errorf("holder %q (ancestor of current root) misclassified as foreign", ancestor)
	}
	// A stale worktree INSIDE the root but not the workdir (the #854
	// ancestor-root-change case maps the old worktree here): IS reclaimable.
	// Re-introducing an "in-root holder → refuse" case fails this.
	inRoot := mkdir(realRoot, "9")
	if !isForeignRootHolder(linkRoot, workdir, inRoot) {
		t.Errorf("stale in-root holder %q must be reclaimable (foreign)", inRoot)
	}
	// A holder under a genuinely different root (the disjoint #854 case): foreign.
	foreign := mkdir(t.TempDir(), "2")
	if !isForeignRootHolder(linkRoot, workdir, foreign) {
		t.Errorf("holder %q under a different root must be foreign", foreign)
	}
}

func TestPathForUsesStableSanitizedIssueIdentifier(t *testing.T) {
	mgr := &Manager{Root: "/workspaces"}

	first := makeTask("tsk-first", "file:///tmp/repo.git")
	first.SourceEventID = "Issue/ABC 123!!Needs_Fix"
	second := first
	second.ID = "tsk-second"
	second.WorkBranch = "ai/tsk-second"

	if got, want := mgr.PathFor(first), filepath.Join("/workspaces", "acme", "demo", "linear_issue", "Issue_ABC_123__Needs_Fix"); got != want {
		t.Fatalf("PathFor() = %q, want %q", got, want)
	}

	unsafeSource := first
	unsafeSource.SourceType = "../Linear Issue//Needs_Safety"
	if got, want := mgr.PathFor(unsafeSource), filepath.Join("/workspaces", "acme", "demo", ".._Linear_Issue__Needs_Safety", "Issue_ABC_123__Needs_Fix"); got != want {
		t.Fatalf("PathFor() unsafe source type = %q, want %q", got, want)
	}
	if clean := filepath.Clean(mgr.PathFor(unsafeSource)); !strings.HasPrefix(clean, "/workspaces/") {
		t.Fatalf("PathFor() escaped workspace root: %q", clean)
	}
	collidingOwner := first
	collidingOwner.RepoOwner = "acme-demo"
	collidingOwner.RepoName = ""
	if got := mgr.PathFor(collidingOwner); got == mgr.PathFor(first) {
		t.Fatalf("owner/name boundary collapsed into colliding workspace path %q", got)
	}
	if got := mgr.PathFor(second); got != mgr.PathFor(first) {
		t.Fatalf("same source issue used different paths: %q vs %q", got, mgr.PathFor(first))
	}

	other := first
	other.SourceEventID = "Issue/ABC 124"
	if got := mgr.PathFor(other); got == mgr.PathFor(first) {
		t.Fatalf("different issue identifiers collided at %q", got)
	}

	collidingSourceBoundary := first
	collidingSourceBoundary.SourceType = "linear_issue-issue"
	collidingSourceBoundary.SourceEventID = "ABC 123!!Needs_Fix"
	if got := mgr.PathFor(collidingSourceBoundary); got == mgr.PathFor(first) {
		t.Fatalf("source type/event boundary collapsed into colliding workspace path %q", got)
	}

	fallback := first
	fallback.SourceType = "manual"
	fallback.SourceEventID = ""
	if got, want := mgr.PathFor(fallback), filepath.Join("/workspaces", "acme", "demo", "tsk-first"); got != want {
		t.Fatalf("PathFor() fallback = %q, want %q", got, want)
	}
}

// TestSanitizeComponentFollowsSpec covers the SPEC §4.2 sanitization rule:
// any character outside [A-Za-z0-9._-] is replaced with `_`, and case is
// preserved verbatim. The harness additions (rune length cap, path-traversal
// guard, empty fallback) are exercised in the tail of the table.
func TestSanitizeComponentFollowsSpec(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// SPEC §4.2 examples — case preserved, single-char substitution.
		{"preserves ascii identifier case", "ABC-123", "ABC-123"},
		{"replaces slash with underscore", "feat/new-ui", "feat_new-ui"},
		{"replaces space with underscore", "a b c", "a_b_c"},
		{"preserves leading and trailing underscores", "___leading___", "___leading___"},
		{"replaces every non-ascii rune one-for-one", "日本語-456", "___-456"},
		{"replaces only invalid characters", "  Issue/ABC 123!!Needs_Fix  ", "__Issue_ABC_123__Needs_Fix__"},
		// Harness extensions.
		{"empty input falls back to unknown", "", "unknown"},
		{"separators-only collapses to unknown via substitution", "!!!", "___"},
		{"single dot maps to unknown to block path traversal", ".", "unknown"},
		{"double dot maps to unknown to block path traversal", "..", "unknown"},
		{"interior double-dot is left untouched", "a..b", "a..b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeComponent(tc.in); got != tc.want {
				t.Fatalf("SanitizeComponent(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	long := strings.Repeat("A", maxSanitizedLength+20)
	if got := SanitizeComponent(long); len([]rune(got)) != maxSanitizedLength {
		t.Fatalf("SanitizeComponent(long) rune length = %d, want %d", len([]rune(got)), maxSanitizedLength)
	}
	unicodeLong := strings.Repeat("界", maxSanitizedLength+20)
	if got := SanitizeComponent(unicodeLong); len([]rune(got)) != maxSanitizedLength || !utf8.ValidString(got) {
		t.Fatalf("SanitizeComponent(unicode long) = %q (runes=%d valid=%v), want %d valid runes", got, len([]rune(got)), utf8.ValidString(got), maxSanitizedLength)
	}
}

// TestSanitizeComponentCasesDoNotCollide guards against the case-conflation
// that lowercasing-based sanitization caused for distinct Linear identifiers.
func TestSanitizeComponentCasesDoNotCollide(t *testing.T) {
	if SanitizeComponent("ABC-123") == SanitizeComponent("abc-123") {
		t.Fatalf("uppercase and lowercase identifiers collapsed into the same workspace key")
	}
}

// configureGitIdentity sets a deterministic committer for the per-test
// worktree. PrepareGitWorkspace inherits config from the bare mirror, but the
// mirror is created without an identity (we never commit inside it), so a
// commit issued from the worktree would otherwise fail in CI environments
// without a global git identity. The gpgsign disables protect the fixture
// from inheriting a host-level commit.gpgsign=true when the signing program
// is missing or broken (see #288).
func configureGitIdentity(dir string) error {
	for _, args := range [][]string{
		{"git", "-C", dir, "config", "user.email", "u@example.com"},
		{"git", "-C", dir, "config", "user.name", "u"},
		{"git", "-C", dir, "config", "commit.gpgsign", "false"},
		{"git", "-C", dir, "config", "tag.gpgsign", "false"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			return errFromOut(args, err, out)
		}
	}
	return nil
}

func errFromOut(args []string, err error, out []byte) error {
	return &gitCmdError{Args: args, Err: err, Out: string(out)}
}

type gitCmdError struct {
	Args []string
	Err  error
	Out  string
}

func (e *gitCmdError) Error() string {
	return strings.Join(e.Args, " ") + ": " + e.Err.Error() + "\n" + e.Out
}

func TestMirrorRoot_OverrideWins(t *testing.T) {
	if got := MirrorRoot("/tmp/explicit"); got != "/tmp/explicit" {
		t.Fatalf("override ignored: %s", got)
	}
	if got := MirrorRoot(""); got == "" {
		t.Fatal("default mirror root must not be empty")
	}
}

func TestMirrorPathFor_StableAcrossSchemes(t *testing.T) {
	root := "/cache"
	cases := []struct {
		in   string
		want string
	}{
		{"https://gitea.example.com/acme/demo.git", "/cache/gitea.example.com/acme/demo.git"},
		{"https://gitea.example.com/acme/demo", "/cache/gitea.example.com/acme/demo.git"},
		{"git@gitea.example.com:acme/demo.git", "/cache/gitea.example.com/acme/demo.git"},
	}
	for _, tc := range cases {
		if got := mirrorPathFor(root, tc.in); got != tc.want {
			t.Errorf("mirrorPathFor(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestMirrorPathFor_ContainsHostileCloneURLs is the #665 regression: a
// malformed clone URL path (path-traversal hops, backslashes, empty/garbage
// input) must never let mirrorPathFor escape the mirror root. clone_url is
// operator-controlled config, so this is defense-in-depth containment rather
// than an externally-triggerable escape — but the derived filesystem path must
// stay under root regardless of input shape.
func TestMirrorPathFor_ContainsHostileCloneURLs(t *testing.T) {
	const root = "/cache"
	// want is the exact structured path each hostile URL maps to. Asserting the
	// precise path (not merely "is it contained") pins down layer 1: every "."
	// or ".." segment becomes "unknown" and the readable host/owner/repo layout
	// survives, so a hostile URL gets a sane structured path rather than the
	// containment backstop's flat fallback. Reverting the per-segment sanitizer
	// would route these through the fallback and fail the exact match.
	cases := []struct {
		in   string
		want string // forward-slash form; converted per-OS below
	}{
		{"https://host/../../evil.git", "/cache/host/unknown/unknown/evil.git"},
		{"git@host:../../evil.git", "/cache/host/unknown/unknown/evil.git"},
		{"https://host/a/../evil.git", "/cache/host/a/unknown/evil.git"},
		{"https://../owner/repo.git", "/cache/unknown/owner/repo.git"},
		{`..\evil.git`, "/cache/unknown/.._evil.git"},
		{`https://host/..\..\evil.git`, "/cache/host/.._.._evil.git"},
		{"https://host/../../../../../../etc/passwd", "/cache/host/unknown/unknown/unknown/unknown/unknown/unknown/etc/passwd.git"},
		{"", "/cache/unknown/repo.git"},
		{"   ", "/cache/unknown/repo.git"},
		{"not a url", "/cache/unknown/not_a_url.git"},
	}
	for _, tc := range cases {
		want := filepath.FromSlash(tc.want)
		got := mirrorPathFor(root, tc.in)
		if got != want {
			t.Errorf("mirrorPathFor(%q) = %q; want %q", tc.in, got, want)
		}
		// Independently of the exact-path assertion above, the result must stay
		// strictly under root for every input shape (#665).
		assertMirrorUnderRoot(t, root, tc.in, got)
		// Derivation is deterministic: the same clone URL maps to the same path.
		if again := mirrorPathFor(root, tc.in); again != got {
			t.Errorf("mirrorPathFor(%q) not deterministic: %q then %q", tc.in, got, again)
		}
	}
}

// assertMirrorUnderRoot fails the test unless got resolves to a location
// strictly below root. It recomputes containment independently of the
// production helper (filepath.Rel on absolute paths) so a bug in
// pathContainedUnder cannot mask an actual escape, and it uses Rel rather than
// a string prefix so a sibling like "<root>-evil" is correctly rejected.
func assertMirrorUnderRoot(t *testing.T, root, in, got string) {
	t.Helper()
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("Abs(%q): %v", root, err)
	}
	gotAbs, err := filepath.Abs(got)
	if err != nil {
		t.Fatalf("Abs(%q): %v", got, err)
	}
	rel, err := filepath.Rel(rootAbs, gotAbs)
	if err != nil {
		t.Fatalf("mirrorPathFor(%q) = %q; Rel(%q, %q): %v", in, got, rootAbs, gotAbs, err)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		t.Errorf("mirrorPathFor(%q) = %q; want a path strictly under root %q (rel = %q)", in, got, rootAbs, rel)
	}
}

func TestPathContainedUnder(t *testing.T) {
	root := filepath.FromSlash("/cache/mirrors")
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"direct child", filepath.Join(root, "host", "repo.git"), true},
		{"deep child", filepath.Join(root, "a", "b", "c.git"), true},
		{"root itself", root, false},
		{"parent escape", filepath.Join(root, "..", "evil.git"), false},
		{"grandparent escape", filepath.Join(root, "..", "..", "evil.git"), false},
		{"sibling sharing prefix", root + "-evil", false},
		{"unrelated absolute path", filepath.FromSlash("/etc/passwd"), false},
	}
	for _, tc := range cases {
		if got := pathContainedUnder(root, tc.path); got != tc.want {
			t.Errorf("pathContainedUnder(%q, %q) [%s] = %v; want %v", root, tc.path, tc.name, got, tc.want)
		}
	}
}

// TestMirrorPathUnderRoot_FallsBackOnEscape drives the containment backstop
// directly. The per-segment sanitizer in mirrorPathFor means a real clone URL
// never produces an escaping candidate, so this is the only path that exercises
// the fallback branch — without it, that branch would be an untested placebo.
func TestMirrorPathUnderRoot_FallsBackOnEscape(t *testing.T) {
	root := t.TempDir()

	good := filepath.Join(root, "host", "owner", "repo.git")
	if got := mirrorPathUnderRoot(root, good, "https://host/owner/repo.git"); got != good {
		t.Errorf("mirrorPathUnderRoot(root, contained) = %q; want %q unchanged", got, good)
	}

	escape := filepath.Join(root, "..", "..", "evil.git")
	url := "https://host/../../evil.git"
	got := mirrorPathUnderRoot(root, escape, url)
	if !pathContainedUnder(root, got) {
		t.Errorf("mirrorPathUnderRoot(root, escaping %q) = %q; want a path under root %q", escape, got, root)
	}
	if again := mirrorPathUnderRoot(root, escape, url); again != got {
		t.Errorf("mirrorPathUnderRoot fallback not deterministic for %q: %q then %q", url, got, again)
	}
	if other := mirrorPathUnderRoot(root, escape, "https://host/../../other.git"); other == got {
		t.Errorf("mirrorPathUnderRoot collapsed distinct clone URLs onto the same fallback %q", got)
	}
}

// TestMirrorPathFor_NormalEdgeComponents pins intentional, non-hostile
// sanitization behaviour that the #665 switch to the shared sanitizeComponent
// changed or exposed: a host with an explicit port has its ":" rewritten (the
// old mirror sanitizer left it intact — the new behaviour is also Windows-safe);
// dots/uppercase in owner/repo names are preserved (no behaviour change); and an
// empty path component maps to "unknown" rather than relying on the (now
// removed) empty-repoPath fallback branch.
func TestMirrorPathFor_NormalEdgeComponents(t *testing.T) {
	const root = "/cache"
	cases := []struct {
		in   string
		want string
	}{
		// Host with an explicit port: ":" is now sanitized to "_".
		{"https://gitea.example.com:8443/acme/demo.git", "/cache/gitea.example.com_8443/acme/demo.git"},
		// Dots and uppercase in owner/repo are valid path chars, kept verbatim.
		{"https://github.com/My.Org/My.Repo.git", "/cache/github.com/My.Org/My.Repo.git"},
		// Empty trailing path component -> "unknown" (exercises the path the
		// removed `if repoPath == ""` branch used to guard).
		{"https://host/owner/.git", "/cache/host/owner/unknown.git"},
		{"https://host/.git", "/cache/host/unknown.git"},
	}
	for _, tc := range cases {
		want := filepath.FromSlash(tc.want)
		if got := mirrorPathFor(root, tc.in); got != want {
			t.Errorf("mirrorPathFor(%q) = %q; want %q", tc.in, got, want)
		}
	}
}
