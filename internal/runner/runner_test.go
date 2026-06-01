package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// TestNew_RemovedCodexExecRunnerIsUnknown pins the #541 removal: the
// non-SPEC `codex` (one-shot `codex exec`) runner is no longer in the
// registry, while the SPEC §10 `codex-app-server` runner still resolves.
func TestNew_RemovedCodexExecRunnerIsUnknown(t *testing.T) {
	if _, err := New("codex"); err == nil {
		t.Fatalf("New(%q) = nil error; want unknown-runner error after #541 removal", "codex")
	} else if !strings.Contains(err.Error(), "unknown runner") {
		t.Fatalf("New(%q) error = %q; want it to mention %q", "codex", err, "unknown runner")
	}

	r, err := New(NameCodexAppServer)
	if err != nil {
		t.Fatalf("New(%q) = %v; want the SPEC §10 app-server runner", NameCodexAppServer, err)
	}
	if _, ok := r.(CodexAppServerRunner); !ok {
		t.Fatalf("New(%q) = %T; want CodexAppServerRunner", NameCodexAppServer, r)
	}
}

// shellTestWorkdir creates a temp workdir with a stub .aiops/PROMPT.md
// so the ShellRunner's `< .aiops/PROMPT.md` redirection does not fail
// before the actual command runs (we care about the kill path, not
// the prompt plumbing).
func shellTestWorkdir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".aiops"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aiops", "PROMPT.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestMockRunnerTimeoutReturnsTimeoutError verifies that when the mock
// runner is asked to sleep longer than the parent context's deadline,
// it returns *TimeoutError (not a generic ctx.Err()) so worker retry
// policy can route it to the timeout-specific bucket.
func TestMockRunnerTimeoutReturnsTimeoutError(t *testing.T) {
	t.Parallel()
	r := MockRunner{Sleep: 5 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := r.Run(ctx, RunInput{
		Task:     task.Task{ID: "tsk_test", Model: "mock"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from timed-out runner, got nil")
	}
	if !IsTimeout(err) {
		t.Fatalf("expected TimeoutError, got %T: %v", err, err)
	}
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("errors.As to *TimeoutError failed: %v", err)
	}
	if te.Elapsed <= 0 {
		t.Fatalf("expected non-zero elapsed, got %v", te.Elapsed)
	}
	// We should have returned promptly when ctx fired, well before the
	// 5s sleep would have completed naturally.
	if elapsed >= 2*time.Second {
		t.Fatalf("runner did not honor ctx cancellation; elapsed=%v", elapsed)
	}
}

// TestMockRunnerNoTimeoutWhenSleepShort confirms the happy path: with
// adequate budget the mock runner returns Result without a TimeoutError.
func TestMockRunnerNoTimeoutWhenSleepShort(t *testing.T) {
	t.Parallel()
	r := MockRunner{Sleep: 5 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := r.Run(ctx, RunInput{
		Task:     task.Task{ID: "tsk_ok", Model: "mock"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Summary == "" {
		t.Fatal("expected non-empty Result.Summary on success")
	}
	if IsTimeout(err) {
		t.Fatal("IsTimeout should be false for nil error")
	}
}

// TestShellRunnerKillsRunawayProcess wires the real ShellRunner against
// a `sleep 30` command and asserts that a 50ms timeout actually kills
// the subprocess (i.e. ctx-driven SIGTERM/SIGKILL works end-to-end). The
// guard `time.Since(start) < 5s` would fail loudly if the kill path
// regressed and we waited the full sleep budget.
func TestShellRunnerKillsRunawayProcess(t *testing.T) {
	t.Parallel()
	workdir := shellTestWorkdir(t)
	wf := workflow.Workflow{Config: workflow.Config{
		Workspace: workflow.WorkspaceConfig{Root: filepath.Dir(workdir)},
		Claude:    workflow.CommandConfig{Command: "sleep 30"},
	}}
	r := ShellRunner{Name: "claude"}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := r.Run(ctx, RunInput{
		Task:     task.Task{ID: "tsk_shell"},
		Workflow: wf,
		Workdir:  workdir,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from killed sh subprocess")
	}
	if !IsTimeout(err) {
		t.Fatalf("expected TimeoutError from shell runner, got %T: %v", err, err)
	}
	// Even with the SIGTERM->SIGKILL grace (5s) the wait must complete
	// well before sleep 30s would have. Allow generous slack for CI.
	if elapsed > 10*time.Second {
		t.Fatalf("shell runner did not kill subprocess promptly; elapsed=%v", elapsed)
	}
}

// TestShellRunnerNonTimeoutErrorNotMisclassified guarantees a runner
// that exits non-zero quickly (no ctx expiry) is *not* tagged as a
// TimeoutError — verify-vs-timeout retry routing depends on this.
func TestShellRunnerNonTimeoutErrorNotMisclassified(t *testing.T) {
	t.Parallel()
	workdir := shellTestWorkdir(t)
	wf := workflow.Workflow{Config: workflow.Config{
		Workspace: workflow.WorkspaceConfig{Root: filepath.Dir(workdir)},
		Claude:    workflow.CommandConfig{Command: "exit 3"},
	}}
	r := ShellRunner{Name: "claude"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := r.Run(ctx, RunInput{
		Task:     task.Task{ID: "tsk_nonzero"},
		Workflow: wf,
		Workdir:  workdir,
	})
	if err == nil {
		t.Fatal("expected non-zero exit error")
	}
	if IsTimeout(err) {
		t.Fatalf("non-zero exit must not be classified as timeout: %v", err)
	}
}

func TestShellRunnerDoesNotInheritWorkerSecretsByDefault(t *testing.T) {
	workdir := shellTestWorkdir(t)
	t.Setenv("LINEAR_API_KEY", "linear-secret")
	t.Setenv("GITEA_TOKEN", "gitea-secret")
	t.Setenv("GITHUB_TOKEN", "github-secret")

	wf := workflow.Workflow{Config: workflow.Config{
		Workspace: workflow.WorkspaceConfig{Root: filepath.Dir(workdir)},
		Claude: workflow.CommandConfig{
			Command: "env > shell-env.txt",
		},
	}}

	if _, err := (ShellRunner{Name: "claude"}).Run(context.Background(), RunInput{
		Task:     task.Task{ID: "tsk_shell_env"},
		Workflow: wf,
		Workdir:  workdir,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(workdir, "shell-env.txt"))
	if err != nil {
		t.Fatalf("read shell-env.txt: %v", err)
	}
	for _, secretName := range []string{"LINEAR_API_KEY", "GITEA_TOKEN", "GITHUB_TOKEN"} {
		if strings.Contains(string(body), secretName+"=") {
			t.Fatalf("runner env leaked %s:\n%s", secretName, body)
		}
	}
	if !strings.Contains(string(body), "PATH=") {
		t.Fatalf("runner env lost baseline PATH:\n%s", body)
	}
}

func TestShellRunnerDoesNotSourceProfileByDefault(t *testing.T) {
	workdir := shellTestWorkdir(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".profile"), []byte("export PROFILE_CANARY=from-profile\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	wf := workflow.Workflow{Config: workflow.Config{
		Workspace: workflow.WorkspaceConfig{Root: filepath.Dir(workdir)},
		Claude: workflow.CommandConfig{
			Command: `printf '%s' "${PROFILE_CANARY:-}" > profile-canary.txt`,
		},
	}}

	if _, err := (ShellRunner{Name: "claude"}).Run(context.Background(), RunInput{
		Task:     task.Task{ID: "tsk_shell_profile"},
		Workflow: wf,
		Workdir:  workdir,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(workdir, "profile-canary.txt"))
	if err != nil {
		t.Fatalf("read profile-canary.txt: %v", err)
	}
	if got := string(body); got != "" {
		t.Fatalf("shell runner sourced HOME profile; canary=%q", got)
	}
}

func TestShellRunnerHonorsExplicitEnvPassthrough(t *testing.T) {
	workdir := shellTestWorkdir(t)
	t.Setenv("AIOPS_RUNNER_CANARY", "allowed-value")
	t.Setenv("LINEAR_API_KEY", "linear-secret")

	wf := workflow.Workflow{Config: workflow.Config{
		Workspace: workflow.WorkspaceConfig{Root: filepath.Dir(workdir)},
		Claude: workflow.CommandConfig{
			Command:        "env > shell-env.txt",
			EnvPassthrough: []string{"AIOPS_RUNNER_CANARY"},
		},
	}}

	if _, err := (ShellRunner{Name: "claude"}).Run(context.Background(), RunInput{
		Task:     task.Task{ID: "tsk_shell_env_allow"},
		Workflow: wf,
		Workdir:  workdir,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(workdir, "shell-env.txt"))
	if err != nil {
		t.Fatalf("read shell-env.txt: %v", err)
	}
	if !strings.Contains(string(body), "AIOPS_RUNNER_CANARY=allowed-value") {
		t.Fatalf("runner env missing explicit passthrough:\n%s", body)
	}
	if strings.Contains(string(body), "LINEAR_API_KEY=") {
		t.Fatalf("runner env leaked token outside explicit passthrough:\n%s", body)
	}
}

func TestAgentEnvRejectsTrackerTokenPassthrough(t *testing.T) {
	t.Setenv("AIOPS_RUNNER_CANARY", "allowed-value")
	t.Setenv("LINEAR_API_KEY", "linear-secret")
	t.Setenv("GITEA_TOKEN", "gitea-secret")
	t.Setenv("GITHUB_TOKEN", "github-secret")

	body := strings.Join(agentEnv([]string{"AIOPS_RUNNER_CANARY", "LINEAR_API_KEY", "GITEA_TOKEN", "GITHUB_TOKEN"}, workflow.Config{}), "\n")
	if !strings.Contains(body, "AIOPS_RUNNER_CANARY=allowed-value") {
		t.Fatalf("agent env missing non-secret passthrough:\n%s", body)
	}
	for _, secretName := range []string{"LINEAR_API_KEY", "GITEA_TOKEN", "GITHUB_TOKEN"} {
		if strings.Contains(body, secretName+"=") {
			t.Fatalf("agent env leaked denied passthrough %s:\n%s", secretName, body)
		}
	}
}

func TestAgentEnvRejectsTrackerAPIKeyValuePassthrough(t *testing.T) {
	t.Setenv("AIOPS_RUNNER_CANARY", "allowed-value")
	t.Setenv("AIOPS_TEST_TRACKER_TOKEN", "tracker-secret")

	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{APIKey: "tracker-secret"},
	}
	body := strings.Join(agentEnv([]string{"AIOPS_RUNNER_CANARY", "AIOPS_TEST_TRACKER_TOKEN"}, cfg), "\n")
	if !strings.Contains(body, "AIOPS_RUNNER_CANARY=allowed-value") {
		t.Fatalf("agent env missing non-secret passthrough:\n%s", body)
	}
	if strings.Contains(body, "AIOPS_TEST_TRACKER_TOKEN=") {
		t.Fatalf("agent env leaked tracker API key value:\n%s", body)
	}
}

func TestAgentEnvUsesLoginShellPATHSnapshot(t *testing.T) {
	env := agentEnvWithLookup(
		nil,
		workflow.Config{},
		func(name string) (string, bool) {
			if name == "PATH" {
				return "/worker/path", true
			}
			return "", false
		},
		func() string { return "/login/path" },
	)
	body := strings.Join(env, "\n")
	if !strings.Contains(body, "PATH=/login/path") {
		t.Fatalf("agent env did not use login-shell PATH snapshot:\n%s", body)
	}
	if strings.Contains(body, "PATH=/worker/path") {
		t.Fatalf("agent env used raw worker PATH instead of login-shell snapshot:\n%s", body)
	}
}

func TestIsTimeoutNilAndOther(t *testing.T) {
	t.Parallel()
	if IsTimeout(nil) {
		t.Fatal("IsTimeout(nil) should be false")
	}
	if IsTimeout(errors.New("boom")) {
		t.Fatal("IsTimeout on plain error should be false")
	}
	te := &TimeoutError{Timeout: time.Second, Elapsed: time.Second, Cause: errors.New("x")}
	if !IsTimeout(te) {
		t.Fatal("IsTimeout on *TimeoutError should be true")
	}
	wrapped := errors.Join(errors.New("ctx"), te)
	if !IsTimeout(wrapped) {
		t.Fatal("IsTimeout should unwrap joined errors")
	}
}

// analysisOnlyWorkflow builds a workflow whose policy mode is "analysis_only"
// so the mock runner takes the analysis-artifact branch (writes PLAN.md and
// skips the per-task <ID>.md file).
func analysisOnlyWorkflow() workflow.Workflow {
	wf := workflow.Workflow{}
	wf.Config.Policy.Mode = "analysis_only"
	return wf
}

// mockRunTask is a fixed task used by the artifact characterization tests so
// the asserted file names and template substitutions stay stable.
func mockRunTask() task.Task {
	return task.Task{ID: "tsk_chr", Title: "Characterize", Actor: "actor-x", Model: "model-y"}
}

// TestMockRunnerArtifactMatrix pins exactly which files (MockRunner).Run writes
// for each flag/policy combination and their content, covering the per-helper
// guards that the pre-#521 suite left unexercised. These assertions are the
// contract the #521 extraction must preserve byte-for-byte.
func TestMockRunnerArtifactMatrix(t *testing.T) {
	t.Parallel()

	const (
		sourceFile    = "mock-source-change.txt"
		sourceContent = "mock source change\n"
	)

	tests := []struct {
		name     string
		runner   MockRunner
		workflow workflow.Workflow
		// present maps a relative path to its exact expected content.
		present map[string]string
		// absent lists relative paths that must NOT exist.
		absent []string
	}{
		{
			name:     "default non-analysis writes task md only",
			runner:   MockRunner{},
			workflow: workflow.Workflow{},
			present: map[string]string{
				filepath.Join(".aiops", "tsk_chr.md"): "",
			},
			absent: []string{
				sourceFile,
				filepath.Join(".aiops", "PLAN.md"),
				filepath.Join(".aiops", "WORKFLOW.md"),
				// The RUN_SUMMARY gate was removed (#561); the mock no longer
				// writes RUN_SUMMARY.md.
				filepath.Join(".aiops", "RUN_SUMMARY.md"),
			},
		},
		{
			name:     "WriteSourceFiles emits source change file",
			runner:   MockRunner{WriteSourceFiles: true},
			workflow: workflow.Workflow{},
			present: map[string]string{
				sourceFile:                            sourceContent,
				filepath.Join(".aiops", "tsk_chr.md"): "",
			},
			absent: []string{filepath.Join(".aiops", "PLAN.md")},
		},
		{
			name:     "analysis_only writes PLAN.md and skips task md",
			runner:   MockRunner{},
			workflow: analysisOnlyWorkflow(),
			present: map[string]string{
				filepath.Join(".aiops", "PLAN.md"): "",
			},
			absent: []string{
				filepath.Join(".aiops", "tsk_chr.md"),
				sourceFile,
				filepath.Join(".aiops", "RUN_SUMMARY.md"),
			},
		},
		{
			name:     "analysis_only with SkipAnalysisPlan writes neither PLAN nor task md",
			runner:   MockRunner{SkipAnalysisPlan: true},
			workflow: analysisOnlyWorkflow(),
			present:  map[string]string{},
			absent: []string{
				filepath.Join(".aiops", "PLAN.md"),
				filepath.Join(".aiops", "tsk_chr.md"),
				sourceFile,
				filepath.Join(".aiops", "RUN_SUMMARY.md"),
			},
		},
		{
			name:     "WriteAiopsWorkflow writes tracked workflow edit",
			runner:   MockRunner{WriteAiopsWorkflow: true},
			workflow: workflow.Workflow{},
			present: map[string]string{
				filepath.Join(".aiops", "WORKFLOW.md"): "tracked workflow edit\n",
				filepath.Join(".aiops", "tsk_chr.md"):  "",
			},
			absent: nil,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			workdir := t.TempDir()
			res, err := tc.runner.Run(context.Background(), RunInput{
				Task:     mockRunTask(),
				Workflow: tc.workflow,
				Workdir:  workdir,
			})
			if err != nil {
				t.Fatalf("Run(%q) error = %v; want nil", tc.name, err)
			}
			if res.Summary != "mock completed" {
				t.Fatalf("Run(%q).Summary = %q; want %q", tc.name, res.Summary, "mock completed")
			}
			for rel, wantContent := range tc.present {
				path := filepath.Join(workdir, rel)
				body, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Errorf("ReadFile(%q) = %v; want file present", rel, readErr)
					continue
				}
				if wantContent != "" && string(body) != wantContent {
					t.Errorf("content(%q) = %q; want %q", rel, string(body), wantContent)
				}
			}
			for _, rel := range tc.absent {
				path := filepath.Join(workdir, rel)
				if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
					t.Errorf("Stat(%q) = %v; want os.IsNotExist", rel, statErr)
				}
			}
		})
	}
}

// TestMockRunnerCommitOnlyArtifactsCommitsPlan pins the CommitSourceFiles +
// CommitOnlyArtifacts branch: the runner must `git add` and commit
// .aiops/PLAN.md (not mock-source-change.txt), and must not write the source
// file at all. This exercises the commitMockChange guard and commitPath
// selection that the pre-#521 suite never reached.
func TestMockRunnerCommitOnlyArtifactsCommitsPlan(t *testing.T) {
	t.Parallel()
	workdir := initGitRepo(t)

	r := MockRunner{CommitSourceFiles: true, CommitOnlyArtifacts: true}
	if _, err := r.Run(context.Background(), RunInput{
		Task:     mockRunTask(),
		Workflow: analysisOnlyWorkflow(),
		Workdir:  workdir,
	}); err != nil {
		t.Fatalf("Run(CommitOnlyArtifacts) error = %v; want nil", err)
	}

	// The source file must be absent because the verbatim guard
	// (WriteSourceFiles || (CommitSourceFiles && !CommitOnlyArtifacts)) is
	// false when CommitOnlyArtifacts is set.
	if _, err := os.Stat(filepath.Join(workdir, "mock-source-change.txt")); !os.IsNotExist(err) {
		t.Fatalf("Stat(mock-source-change.txt) = %v; want os.IsNotExist", err)
	}

	committed := gitLastCommitFiles(t, workdir)
	wantPath := filepath.Join(".aiops", "PLAN.md")
	if len(committed) != 1 || committed[0] != wantPath {
		t.Fatalf("committed files = %v; want [%q]", committed, wantPath)
	}
}

// TestMockRunnerCommitSourceChangeCommitsSourceFile pins the default commit
// branch (CommitSourceFiles without CommitOnlyArtifacts): the committed path
// is mock-source-change.txt.
func TestMockRunnerCommitSourceChangeCommitsSourceFile(t *testing.T) {
	t.Parallel()
	workdir := initGitRepo(t)

	r := MockRunner{CommitSourceFiles: true}
	if _, err := r.Run(context.Background(), RunInput{
		Task:     mockRunTask(),
		Workflow: workflow.Workflow{},
		Workdir:  workdir,
	}); err != nil {
		t.Fatalf("Run(CommitSourceFiles) error = %v; want nil", err)
	}

	committed := gitLastCommitFiles(t, workdir)
	if len(committed) != 1 || committed[0] != "mock-source-change.txt" {
		t.Fatalf("committed files = %v; want [%q]", committed, "mock-source-change.txt")
	}
}

// TestMockRunnerSetBaseToHeadWritesWorkspaceBaseConfig pins the SetBaseToHead
// branch: the runner records aiops.workspaceBase=HEAD in the repo's local git
// config.
func TestMockRunnerSetBaseToHeadWritesWorkspaceBaseConfig(t *testing.T) {
	t.Parallel()
	workdir := initGitRepo(t)

	r := MockRunner{SetBaseToHead: true}
	if _, err := r.Run(context.Background(), RunInput{
		Task:     mockRunTask(),
		Workflow: workflow.Workflow{},
		Workdir:  workdir,
	}); err != nil {
		t.Fatalf("Run(SetBaseToHead) error = %v; want nil", err)
	}

	got := gitConfigLocal(t, workdir, "aiops.workspaceBase")
	if got != "HEAD" {
		t.Fatalf("git config aiops.workspaceBase = %q; want %q", got, "HEAD")
	}
}

// TestMockRunnerManualCancelReturnsBareContextError pins the non-deadline
// cancellation path: when the context is cancelled (not deadline-exceeded)
// while the runner is sleeping, Run returns the bare ctx.Err()
// (context.Canceled) rather than a *TimeoutError. The pre-#521 suite only
// pinned the DeadlineExceeded branch.
func TestMockRunnerManualCancelReturnsBareContextError(t *testing.T) {
	t.Parallel()
	r := MockRunner{Sleep: 5 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel shortly after Run begins sleeping so the select observes
	// ctx.Done() with context.Canceled (no deadline on this context).
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	_, err := r.Run(ctx, RunInput{
		Task:     mockRunTask(),
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	})
	if err == nil {
		t.Fatal("Run(manual-cancel) error = nil; want context.Canceled")
	}
	if IsTimeout(err) {
		t.Fatalf("Run(manual-cancel) IsTimeout = true (%T); want bare context error", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run(manual-cancel) error = %v; want errors.Is(context.Canceled)", err)
	}
}

// initGitRepo creates a temp dir with an initialized git repo holding one
// commit so the mock runner's commit / config branches operate against a real
// repo.
func initGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "seed.txt")
	runGit(t, dir, "commit", "-q", "-m", "seed")
	return dir
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s = %v\n%s", strings.Join(args, " "), err, out)
	}
}

// gitLastCommitFiles returns the files changed by HEAD relative to its parent.
func gitLastCommitFiles(t *testing.T, dir string) []string {
	t.Helper()
	cmd := exec.Command("git", "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git diff-tree HEAD = %v\n%s", err, out)
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files
}

func gitConfigLocal(t *testing.T, dir, key string) string {
	t.Helper()
	cmd := exec.Command("git", "config", "--local", "--get", key)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git config --get %s = %v\n%s", key, err, out)
	}
	return strings.TrimSpace(string(out))
}
