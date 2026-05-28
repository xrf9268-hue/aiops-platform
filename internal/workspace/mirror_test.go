package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
// (cached deps, build outputs, .aiops policy feedback) must survive a
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

// TestPrepareGitWorkspace_RecreatesWhenPathIsSymlink covers the security
// gate on the reuse path: a symlink planted at the workspace path could
// otherwise redirect the reuse-path `git reset` / `git checkout -B` into
// a repository outside the workspace root. PrepareGitWorkspace must
// refuse the reuse, remove the symlink (without following it), and
// recreate a fresh worktree linked to OUR mirror, reporting
// `createdNow=true`.
func TestPrepareGitWorkspace_RecreatesWhenPathIsSymlink(t *testing.T) {
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

	// Capture the mirror's git-common-dir for later comparison.
	firstCommon, err := exec.Command("git", "-C", dir, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		t.Fatalf("first git-common-dir: %v", err)
	}
	firstCommonReal, err := filepath.EvalSymlinks(strings.TrimSpace(string(firstCommon)))
	if err != nil {
		t.Fatalf("eval first common: %v", err)
	}

	// Replace the workspace path with a symlink pointing at an attacker
	// controlled git repo elsewhere on disk. The reuse path must NOT
	// follow this symlink and run `git reset` inside it.
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

	dir2, createdNow, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("second prepare after symlink swap: %v", err)
	}
	if dir != dir2 {
		t.Fatalf("path changed across runs: %s vs %s", dir, dir2)
	}
	if !createdNow {
		t.Fatal("second prepare reported createdNow=false; symlinked path must fall back to recreate")
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

	// The recreated workspace must be linked to OUR mirror (same
	// git-common-dir as the first prepare), not to the attacker's repo.
	secondCommon, err := exec.Command("git", "-C", dir2, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		t.Fatalf("second git-common-dir: %v", err)
	}
	secondCommonReal, err := filepath.EvalSymlinks(strings.TrimSpace(string(secondCommon)))
	if err != nil {
		t.Fatalf("eval second common: %v", err)
	}
	if secondCommonReal != firstCommonReal {
		t.Fatalf("recreated workspace linked to a different mirror: before=%q after=%q", firstCommonReal, secondCommonReal)
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

// TestPrepareGitWorkspace_ReuseSurvivesIntentToAddFromPriorDiffstat locks
// the reuse-path `git reset --quiet HEAD -- .` invariant at the workspace
// boundary. A prior run's `EnforcePolicy` / `Diffstat` calls
// `git add --intent-to-add --all`, leaving untracked files staged as
// empty-blob entries in the index. Without the reset, the next prepare's
// `git checkout --force -B` would treat those entries as
// "files-in-index-not-in-target-ref" and delete them from the working
// tree — silently nuking cached deps and hook artifacts that SPEC §9.1
// reuse semantics promise to preserve.
//
// `TestPrepareGitWorkspace_RerunReusesWorkspaceAcrossRuns` covers the
// happy reuse path but never calls Diffstat, so the reset is a no-op
// there. Only the worker-integration test
// `TestRunTaskReusesWorkspaceAcrossRunsAndGatesAfterCreate` exercises
// this scenario today, which means removing the reset only fails the
// worker test, not anything in the workspace package. This test pins the
// invariant where the code lives.
func TestPrepareGitWorkspace_ReuseSurvivesIntentToAddFromPriorDiffstat(t *testing.T) {
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
	// (build outputs, .aiops policy feedback, etc.).
	if err := os.WriteFile(filepath.Join(dir, "cached-dep.txt"), []byte("cache\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".aiops"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aiops", "POLICY_VIOLATION_FEEDBACK.md"), []byte("feedback\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Diffstat runs `git add --intent-to-add --all`, leaving cached-dep.txt
	// and the .aiops file in the index as empty-blob entries. The next
	// prepare's reset must drop those entries before the checkout, or
	// `git checkout --force -B` would treat them as removable.
	if _, err := Diffstat(ctx, dir); err != nil {
		t.Fatalf("Diffstat: %v", err)
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
	for _, rel := range []string{"cached-dep.txt", filepath.Join(".aiops", "POLICY_VIOLATION_FEEDBACK.md")} {
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
// explicitly by deleting `refs/remotes/origin/main` from a freshly
// prepared mirror and emptying the fetch refspec so the next
// `ensureMirrorLocked`'s fetch does not repopulate it — guarding the
// fallback against a future refactor of EnsureMirror's refspec layout
// (and ensuring `file://` test fixtures continue to work even when the
// mirror only carries `refs/heads/<base>`).
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
	// Strip `refs/remotes/origin/*` and empty the fetch refspec so the
	// next prepare's `git fetch --prune --tags origin` does not bring
	// `origin/main` back. With no refspec, the fetch is effectively a
	// no-op for ref tracking; the mirror keeps its `refs/heads/main` from
	// the original `git clone --bare`.
	if out, err := exec.Command("git", "--git-dir", mirror, "update-ref", "-d", "refs/remotes/origin/main").CombinedOutput(); err != nil {
		t.Fatalf("delete refs/remotes/origin/main: %v\n%s", err, out)
	}
	if out, err := exec.Command("git", "--git-dir", mirror, "config", "--unset-all", "remote.origin.fetch").CombinedOutput(); err != nil {
		t.Fatalf("unset remote.origin.fetch: %v\n%s", err, out)
	}
	// Sanity: confirm the precondition the fallback branch needs — the
	// remote-tracking ref is gone but the bare head ref survives.
	if err := exec.Command("git", "--git-dir", mirror, "rev-parse", "--verify", "refs/remotes/origin/main").Run(); err == nil {
		t.Fatal("precondition broken: refs/remotes/origin/main still resolves on the mirror")
	}
	if err := exec.Command("git", "--git-dir", mirror, "rev-parse", "--verify", "refs/heads/main").Run(); err != nil {
		t.Fatalf("precondition broken: refs/heads/main missing on mirror: %v", err)
	}

	// New task → first-touch path runs through the startRef resolution.
	// With `origin/main` deleted, the `git rev-parse --verify origin/main`
	// gate fails and startRef falls back to bare `main`; the subsequent
	// `git worktree add ... main` must succeed against the mirror's
	// `refs/heads/main`.
	tk := makeTask("task-fallback", upstream)
	dir, createdNow, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("prepare with startRef fallback: %v", err)
	}
	if !createdNow {
		t.Fatal("first prepare for new task reported createdNow=false")
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

// TestCommitAndPush_RetryOverwritesRemoteBranch covers the issue #7 scenario:
// a previous attempt for the same task ID already pushed a commit to
// origin/ai/<id>, then the worker retried and produced a different commit on
// the same work branch. The second push must succeed (using --force-with-lease
// internally) so the existing PR points at the latest run, instead of failing
// with a non-fast-forward error and leaving the task stuck.
func TestCommitAndPush_RetryOverwritesRemoteBranch(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()
	tk := makeTask("retry-task", upstream)

	dir, _, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "first.txt"), []byte("first attempt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := configureGitIdentity(dir); err != nil {
		t.Fatalf("configure identity: %v", err)
	}
	if err := CommitAndPush(ctx, dir, "first attempt", tk.WorkBranch); err != nil {
		t.Fatalf("first CommitAndPush: %v", err)
	}

	// Simulate the worker re-claiming the same task: PrepareGitWorkspace
	// resets the worktree to a fresh checkout off main, so the retry's local
	// branch tip diverges from the remote one we just pushed.
	dir2, _, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("second prepare: %v", err)
	}
	if err := configureGitIdentity(dir2); err != nil {
		t.Fatalf("configure identity: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir2, "second.txt"), []byte("retry\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CommitAndPush(ctx, dir2, "retry attempt", tk.WorkBranch); err != nil {
		t.Fatalf("retry CommitAndPush: %v", err)
	}

	// Confirm the remote tip is the retry commit (contains second.txt and
	// not first.txt) — i.e. the retry overwrote the previous push instead
	// of being silently merged into it.
	bare := strings.TrimPrefix(upstream, "file://")
	out, err := exec.Command("git", "--git-dir", bare, "ls-tree", "-r", "--name-only", tk.WorkBranch).CombinedOutput()
	if err != nil {
		t.Fatalf("ls-tree: %v\n%s", err, out)
	}
	tree := string(out)
	if !strings.Contains(tree, "second.txt") {
		t.Fatalf("expected retry commit on remote, missing second.txt; tree=%q", tree)
	}
	if strings.Contains(tree, "first.txt") {
		t.Fatalf("expected retry to overwrite remote, but first.txt still present; tree=%q", tree)
	}
}

// TestCommitAndPush_RemoteBranchDeletedRecreates covers the regression
// flagged in PR #51 review: when a previous attempt pushed origin/<branch>
// and then an operator (or a separate cleanup workflow) deleted that branch
// upstream, the retry must not get wedged on a stale local tracking ref. The
// push is expected to recreate the branch on the remote rather than fail
// with `stale info` from --force-with-lease.
func TestCommitAndPush_RemoteBranchDeletedRecreates(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()
	tk := makeTask("deleted-task", upstream)

	// First attempt: create the branch upstream.
	dir, _, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	if err := configureGitIdentity(dir); err != nil {
		t.Fatalf("configure identity: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "first.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CommitAndPush(ctx, dir, "first", tk.WorkBranch); err != nil {
		t.Fatalf("first CommitAndPush: %v", err)
	}

	// Operator cleanup: delete the work branch from the bare upstream.
	bare := strings.TrimPrefix(upstream, "file://")
	if out, err := exec.Command("git", "--git-dir", bare, "update-ref", "-d", "refs/heads/"+tk.WorkBranch).CombinedOutput(); err != nil {
		t.Fatalf("delete remote branch: %v\n%s", err, out)
	}

	// Seed a stale local tracking ref so the retry's git push has something
	// to (mis)compute a lease against — the exact condition the review
	// flagged as breaking the previous unconditional --force-with-lease path.
	if out, err := exec.Command("git", "-C", dir, "fetch", "origin", "main").CombinedOutput(); err != nil {
		t.Fatalf("seed: fetch main: %v\n%s", err, out)
	}
	mainSHA, err := exec.Command("git", "-C", dir, "rev-parse", "origin/main").Output()
	if err != nil {
		t.Fatalf("seed: rev-parse: %v", err)
	}
	if out, err := exec.Command("git", "-C", dir, "update-ref", "refs/remotes/origin/"+tk.WorkBranch, strings.TrimSpace(string(mainSHA))).CombinedOutput(); err != nil {
		t.Fatalf("seed stale tracking ref: %v\n%s", err, out)
	}

	// Retry: same task ID, fresh worktree, new content. Must succeed and
	// recreate the branch on the bare upstream.
	dir2, _, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("second prepare: %v", err)
	}
	if err := configureGitIdentity(dir2); err != nil {
		t.Fatalf("configure identity: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir2, "retry.txt"), []byte("retry\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CommitAndPush(ctx, dir2, "retry", tk.WorkBranch); err != nil {
		t.Fatalf("retry CommitAndPush after upstream branch deletion: %v", err)
	}

	out, err := exec.Command("git", "--git-dir", bare, "ls-tree", "-r", "--name-only", tk.WorkBranch).CombinedOutput()
	if err != nil {
		t.Fatalf("ls-tree: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "retry.txt") {
		t.Fatalf("expected branch to be recreated with retry.txt; tree=%q", out)
	}
}

// unsetEnvForTest unsets the named env vars for the duration of the test,
// restoring the prior values via t.Cleanup. testing.T.Setenv only supports
// SET; we need UNSET semantics for tests that simulate a vanilla CI runner
// where GIT_* identity vars are absent (not "set to empty string", which git
// treats as "use this empty value" rather than "fall back to next source").
func unsetEnvForTest(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		old, present := os.LookupEnv(k)
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unset %s: %v", k, err)
		}
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(k, old)
			} else {
				_ = os.Unsetenv(k)
			}
		})
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

func TestCleanup_RemovesOldWorktreesKeepsRecent(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()

	old := makeTask("old", upstream)
	recent := makeTask("recent", upstream)

	oldDir, _, err := mgr.PrepareGitWorkspace(ctx, old)
	if err != nil {
		t.Fatalf("prepare old: %v", err)
	}
	recentDir, _, err := mgr.PrepareGitWorkspace(ctx, recent)
	if err != nil {
		t.Fatalf("prepare recent: %v", err)
	}

	// Backdate the "old" worktree so it sits well outside the cleanup
	// threshold while "recent" stays inside it.
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(oldDir, past, past); err != nil {
		t.Fatal(err)
	}

	report, err := mgr.Cleanup(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if report.Removed != 1 {
		t.Fatalf("expected 1 removal, got %+v", report)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatalf("old worktree still present at %s", oldDir)
	}
	if _, err := os.Stat(recentDir); err != nil {
		t.Fatalf("recent worktree should still exist: %v", err)
	}

	// The mirror itself must survive cleanup so subsequent tasks reuse it.
	mirror := mirrorPathFor(MirrorRoot(mgr.MirrorRoot), upstream)
	if _, err := os.Stat(filepath.Join(mirror, "HEAD")); err != nil {
		t.Fatalf("mirror lost during cleanup: %v", err)
	}
}

func TestCleanup_MissingRootIsNotAnError(t *testing.T) {
	mgr := &Manager{Root: filepath.Join(t.TempDir(), "does-not-exist")}
	report, err := mgr.Cleanup(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("expected nil error for missing root, got %v", err)
	}
	if report.Removed != 0 || report.Failed != 0 {
		t.Fatalf("expected empty report, got %+v", report)
	}
}

func TestMirrorRoot_OverrideWins(t *testing.T) {
	if got := MirrorRoot("/tmp/explicit"); got != "/tmp/explicit" {
		t.Fatalf("override ignored: %s", got)
	}
	if got := MirrorRoot(""); got == "" {
		t.Fatal("default mirror root must not be empty")
	}
}

// TestCommitAndPush_WorksWithoutHostGitIdent verifies the worker is
// self-sufficient on a host with NO global git config — the situation on
// fresh CI runners and hardened container images. Before this fix,
// `git commit` would error with "empty ident name not allowed" because the
// per-task worktree inherits no identity from the bare mirror. Now the
// worker passes its own identity inline via -c flags; the test confirms
// the commit lands with the expected author/committer fields.
func TestCommitAndPush_WorksWithoutHostGitIdent(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()
	tk := makeTask("no-ident-task", upstream)

	dir, _, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("payload\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate a host with no git ident: point HOME at an empty temp dir
	// (so ~/.gitconfig does not exist) and unset every GIT_* env var that
	// could leak an identity from the developer's environment. Without the
	// production fix, git commit would now fail with "empty ident name".
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	unsetEnvForTest(t,
		"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL",
		"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
	)

	if err := CommitAndPush(ctx, dir, "no-ident attempt", tk.WorkBranch); err != nil {
		t.Fatalf("CommitAndPush should succeed without host git ident: %v", err)
	}

	bare := strings.TrimPrefix(upstream, "file://")
	out, err := exec.Command("git", "--git-dir", bare,
		"log", "-1", "--format=%an|%ae|%cn|%ce", tk.WorkBranch).CombinedOutput()
	if err != nil {
		t.Fatalf("git log on bare: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	want := CommitIdentName + "|" + CommitIdentEmail + "|" + CommitIdentName + "|" + CommitIdentEmail
	if got != want {
		t.Fatalf("commit identity = %q, want %q", got, want)
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
