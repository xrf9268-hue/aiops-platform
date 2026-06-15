package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
	"github.com/xrf9268-hue/aiops-platform/internal/workspace"
)

type blockingOwnershipRunner struct {
	started chan runner.RunInput
	release chan struct{}
}

func (b blockingOwnershipRunner) Run(ctx context.Context, in runner.RunInput) (runner.Result, error) {
	b.started <- in
	select {
	case <-b.release:
		return runner.Result{Summary: "ok"}, nil
	case <-ctx.Done():
		return runner.Result{}, ctx.Err()
	}
}

func TestRunTaskHoldsWorktreeOwnershipDuringRunner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("worktree ownership uses flock on Unix")
	}
	cloneURL := initOwnershipBareUpstream(t)
	tk := task.Task{
		ID:            "871",
		Title:         "ownership",
		Description:   "hold worktree ownership",
		Actor:         "tester",
		Model:         "blocking-owner",
		RepoOwner:     "acme",
		RepoName:      "demo",
		CloneURL:      cloneURL,
		BaseBranch:    "main",
		WorkBranch:    "ai/871",
		SourceType:    "linear_issue",
		SourceEventID: "871",
	}
	wf := &workflow.Workflow{Config: workflow.DefaultConfig()}
	wf.PromptTemplate = "do {{ task.title }}"
	wf.Config.Agent.Default = "blocking-owner"

	rootA := t.TempDir()
	mirrorRoot := t.TempDir()
	started := make(chan runner.RunInput, 1)
	release := make(chan struct{})
	oldNewRunner := newRunner
	newRunner = func(string) (runner.Runner, error) {
		return blockingOwnershipRunner{started: started, release: release}, nil
	}
	t.Cleanup(func() { newRunner = oldNewRunner })

	errCh := make(chan *RunTaskError, 1)
	go func() {
		errCh <- RunTask(context.Background(), &fakeEmitter{}, tk, Config{
			WorkspaceRoot: rootA,
			MirrorRoot:    mirrorRoot,
			Workflow:      wf,
		})
	}()

	runInput := <-started
	marker := filepath.Join(runInput.Workdir, "live-run-marker.txt")
	if err := os.WriteFile(marker, []byte("live\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rootB := t.TempDir()
	mgrB := workspace.New(rootB)
	mgrB.MirrorRoot = mirrorRoot
	if _, _, err := mgrB.PrepareGitWorkspace(context.Background(), tk); err == nil {
		t.Fatal("foreign-root prepare succeeded while RunTask runner still held the worktree ownership lock")
	}
	if body, err := os.ReadFile(marker); err != nil {
		t.Fatalf("active runner worktree deleted by reclaim: %v", err)
	} else if string(body) != "live\n" {
		t.Fatalf("active runner marker mutated by reclaim: %q", body)
	}

	close(release)
	if rterr := <-errCh; rterr != nil {
		t.Fatalf("RunTask returned error: %v", rterr.Err)
	}
	if _, _, err := mgrB.PrepareGitWorkspace(context.Background(), tk); err != nil {
		t.Fatalf("foreign-root prepare after RunTask released ownership: %v", err)
	}
}

func initOwnershipBareUpstream(t *testing.T) string {
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
