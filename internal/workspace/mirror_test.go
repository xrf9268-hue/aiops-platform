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
