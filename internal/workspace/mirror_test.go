package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		ID:         id,
		RepoOwner:  "acme",
		RepoName:   "demo",
		CloneURL:   cloneURL,
		BaseBranch: "main",
		WorkBranch: "ai/" + id,
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

func TestPrepareGitWorkspace_IsolatedWorktreesShareMirror(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()

	t1 := makeTask("task-a", upstream)
	t2 := makeTask("task-b", upstream)

	dir1, err := mgr.PrepareGitWorkspace(ctx, t1)
	if err != nil {
		t.Fatalf("prepare t1: %v", err)
	}
	dir2, err := mgr.PrepareGitWorkspace(ctx, t2)
	if err != nil {
		t.Fatalf("prepare t2: %v", err)
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

func TestPrepareGitWorkspace_RerunReusesPathIdempotently(t *testing.T) {
	upstream := initBareUpstream(t)
	mgr := newTestManager(t)
	ctx := context.Background()
	tk := makeTask("task-x", upstream)

	dir, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("first prepare: %v", err)
	}
	// Simulate a partial run leaving an extra file behind. The next
	// PrepareGitWorkspace must give us a clean checkout.
	if err := os.WriteFile(filepath.Join(dir, "stale.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir2, err := mgr.PrepareGitWorkspace(ctx, tk)
	if err != nil {
		t.Fatalf("second prepare: %v", err)
	}
	if dir != dir2 {
		t.Fatalf("path changed across runs: %s vs %s", dir, dir2)
	}
	if _, err := os.Stat(filepath.Join(dir2, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale file survived re-prepare: %v", err)
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

	dir, err := mgr.PrepareGitWorkspace(ctx, tk)
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
	dir2, err := mgr.PrepareGitWorkspace(ctx, tk)
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
	dir, err := mgr.PrepareGitWorkspace(ctx, tk)
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
	dir2, err := mgr.PrepareGitWorkspace(ctx, tk)
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

// configureGitIdentity sets a deterministic committer for the per-test
// worktree. PrepareGitWorkspace inherits config from the bare mirror, but the
// mirror is created without an identity (we never commit inside it), so a
// commit issued from the worktree would otherwise fail in CI environments
// without a global git identity.
func configureGitIdentity(dir string) error {
	for _, args := range [][]string{
		{"git", "-C", dir, "config", "user.email", "u@example.com"},
		{"git", "-C", dir, "config", "user.name", "u"},
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

	oldDir, err := mgr.PrepareGitWorkspace(ctx, old)
	if err != nil {
		t.Fatalf("prepare old: %v", err)
	}
	recentDir, err := mgr.PrepareGitWorkspace(ctx, recent)
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
