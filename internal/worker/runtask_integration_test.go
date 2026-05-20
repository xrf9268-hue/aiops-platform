package worker_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	worker "github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// initBareUpstreamWithWorkflow seeds a bare upstream repo containing the
// given WORKFLOW.md body plus a README so the worker's worktree-add lands
// on a non-empty base branch. Returns the file:// URL of the bare repo
// and a sample task pre-wired to that URL so the test only has to set
// source identity fields.
func initBareUpstreamWithWorkflow(t *testing.T, workflowBody string) (cloneURL string, baseTask task.Task) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	work := filepath.Join(root, "seed")
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
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if workflowBody != "" {
		if err := os.WriteFile(filepath.Join(work, "WORKFLOW.md"), []byte(workflowBody), 0o644); err != nil {
			t.Fatal(err)
		}
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
	cloneURL = "file://" + bare
	baseTask = task.Task{
		ID:            "tsk_int",
		Title:         "integration",
		Description:   "exercise runTask",
		Actor:         "tester",
		Model:         "mock",
		RepoOwner:     "acme",
		RepoName:      "demo",
		CloneURL:      cloneURL,
		BaseBranch:    "main",
		WorkBranch:    "ai/tsk_int",
		SourceType:    "linear_issue",
		SourceEventID: "issue-uuid",
	}
	return cloneURL, baseTask
}

// linearWorkflowBody is a minimal front-matter that satisfies validateConfig
// while routing the worker's tracker hooks down the linear branch. clone_url
// is overridden at runtime via the env-var expansion (loader expands
// $REPO_URL on every Load), so a single body string can be reused across
// tests that target different temp repos.
const linearWorkflowBody = `---
repo:
  owner: acme
  name: demo
  clone_url: $REPO_URL
tracker:
  kind: linear
  project_slug: platform
agent:
  default: mock
---
do the work for {{task.title}}
`

// workerCfgForIntegration assembles the Config the integration tests share.
func workerCfgForIntegration(t *testing.T) worker.Config {
	t.Helper()
	return workerCfgForIntegrationWithWorkflow(t, linearWorkflowBody)
}

func workerCfgForIntegrationWithWorkflow(t *testing.T, body string) worker.Config {
	t.Helper()
	wf, err := workflow.Load(writeServiceWorkflowForIntegration(t, body))
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	return worker.Config{
		WorkspaceRoot: t.TempDir(),
		MirrorRoot:    t.TempDir(),
		Workflow:      wf,
	}
}

func TestRunTaskHonorsWorkspaceRootPrecedence(t *testing.T) {
	cloneURL, tk := initBareUpstreamWithWorkflow(t, linearWorkflowBody)
	t.Setenv("REPO_URL", cloneURL)

	t.Run("env only", func(t *testing.T) {
		ev := &fakeEmitter{}
		cfg := workerCfgForIntegration(t)
		cfg.WorkspaceRoot = filepath.Join(t.TempDir(), "env-workspaces")

		if rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg); rterr != nil {
			t.Fatalf("runTask: %v", rterr.Err)
		}

		assertTaskWorkdir(t, cfg.WorkspaceRoot, tk)
	})

	t.Run("yaml only", func(t *testing.T) {
		ev := &fakeEmitter{}
		yamlRoot := filepath.Join(t.TempDir(), "yaml-workspaces")
		cfg := workerCfgForIntegrationWithWorkspaceRoot(t, yamlRoot)
		cfg.WorkspaceRoot = ""

		if rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg); rterr != nil {
			t.Fatalf("runTask: %v", rterr.Err)
		}

		assertTaskWorkdir(t, yamlRoot, tk)
	})

	t.Run("yaml wins over env", func(t *testing.T) {
		ev := &fakeEmitter{}
		envRoot := filepath.Join(t.TempDir(), "env-workspaces")
		yamlRoot := filepath.Join(t.TempDir(), "yaml-workspaces")
		cfg := workerCfgForIntegrationWithWorkspaceRoot(t, yamlRoot)
		cfg.WorkspaceRoot = envRoot

		if rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg); rterr != nil {
			t.Fatalf("runTask: %v", rterr.Err)
		}

		assertTaskWorkdir(t, yamlRoot, tk)
		if _, err := os.Stat(filepath.Join(envRoot, "acme", "demo", "linear_issue", "issue-uuid")); !os.IsNotExist(err) {
			t.Fatalf("env workspace root should not be used when workflow workspace.root is set; stat err=%v", err)
		}
	})

	t.Run("explicit yaml default wins over env", func(t *testing.T) {
		ev := &fakeEmitter{}
		envRoot := filepath.Join(t.TempDir(), "env-workspaces")
		yamlRoot := defaultWorkflowWorkspaceRootForTest(t)
		cfg := workerCfgForIntegrationWithWorkspaceRoot(t, yamlRoot)
		cfg.WorkspaceRoot = envRoot

		if rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg); rterr != nil {
			t.Fatalf("runTask: %v", rterr.Err)
		}

		assertTaskWorkdir(t, yamlRoot, tk)
		if _, err := os.Stat(filepath.Join(envRoot, "acme", "demo", "linear_issue", "issue-uuid")); !os.IsNotExist(err) {
			t.Fatalf("env workspace root should not be used when explicit workflow workspace.root equals default; stat err=%v", err)
		}
	})
}

func assertTaskWorkdir(t *testing.T, root string, tk task.Task) {
	t.Helper()
	workdir := filepath.Join(root, tk.RepoOwner, tk.RepoName, "linear_issue", tk.SourceEventID)
	if _, err := os.Stat(filepath.Join(workdir, ".aiops", "RUN_SUMMARY.md")); err != nil {
		t.Fatalf("expected task workspace under %q; stat RUN_SUMMARY.md: %v", workdir, err)
	}
}

func workerCfgForIntegrationWithWorkspaceRoot(t *testing.T, root string) worker.Config {
	t.Helper()
	body := strings.Replace(linearWorkflowBody, "agent:\n", fmt.Sprintf("workspace:\n  root: %s\nagent:\n", root), 1)
	if body == linearWorkflowBody {
		t.Fatal("test workflow fixture no longer contains agent block insertion point")
	}
	return workerCfgForIntegrationWithWorkflow(t, body)
}

func defaultWorkflowWorkspaceRootForTest(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home dir: %v", err)
	}
	return filepath.Join(home, "aiops-workspaces")
}

func writeServiceWorkflowForIntegration(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write service workflow: %v", err)
	}
	return path
}

func TestRunTaskExecutesWorkspaceHooksAroundRunner(t *testing.T) {
	cloneURL, tk := initBareUpstreamWithWorkflow(t, linearWorkflowBody)
	t.Setenv("REPO_URL", cloneURL)

	ev := &fakeEmitter{}
	cfg := workerCfgForIntegration(t)
	cfg.Workflow.Config.Workspace.Hooks = workflow.WorkspaceHooks{
		AfterCreate:  workflow.WorkspaceHook{Commands: []string{"printf after_create >> hook.log"}},
		BeforeRun:    workflow.WorkspaceHook{Commands: []string{"printf before_run >> hook.log"}},
		AfterRun:     workflow.WorkspaceHook{Commands: []string{"printf after_run >> hook.log"}},
		BeforeRemove: workflow.WorkspaceHook{Commands: []string{"printf before_remove >> ../before_remove.log"}},
	}

	if rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg); rterr != nil {
		t.Fatalf("runTask: %v", rterr.Err)
	}

	type step struct {
		kind string
		hook string
	}
	wantSteps := []step{
		{kind: task.EventWorkspaceHookStart, hook: "after_create"},
		{kind: task.EventWorkspaceHookEnd, hook: "after_create"},
		{kind: task.EventWorkspaceHookStart, hook: "before_run"},
		{kind: task.EventWorkspaceHookEnd, hook: "before_run"},
		{kind: task.EventRunnerStart},
		{kind: task.EventRunnerEnd},
		{kind: task.EventWorkspaceHookStart, hook: "after_run"},
		{kind: task.EventWorkspaceHookEnd, hook: "after_run"},
	}
	var got []step
	for _, e := range ev.events {
		switch e.Kind {
		case task.EventWorkspaceHookStart, task.EventWorkspaceHookEnd:
			got = append(got, step{kind: e.Kind, hook: fmt.Sprint(e.Payload.(map[string]any)["hook"])})
		case task.EventRunnerStart, task.EventRunnerEnd:
			got = append(got, step{kind: e.Kind})
		}
	}
	if len(got) < len(wantSteps) {
		t.Fatalf("hook/runner event count = %d, want at least %d; events=%#v", len(got), len(wantSteps), ev.events)
	}
	for i, want := range wantSteps {
		if got[i] != want {
			t.Fatalf("event[%d] = %#v, want %#v; got sequence=%#v", i, got[i], want, got)
		}
	}

	workdir := filepath.Join(cfg.WorkspaceRoot, "acme", "demo", "linear_issue", "issue-uuid")
	body, err := os.ReadFile(filepath.Join(workdir, "hook.log"))
	if err != nil {
		t.Fatalf("read hook log: %v", err)
	}
	if string(body) != "after_createbefore_runafter_run" {
		t.Fatalf("hook log = %q, want hooks to run in order around runner", body)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(workdir), "before_remove.log")); !os.IsNotExist(err) {
		t.Fatalf("before_remove hook should not run during normal RunTask completion; stat err=%v", err)
	}
}

func TestRunTaskRendersAttemptVariable(t *testing.T) {
	cloneURL, tk := initBareUpstreamWithWorkflow(t, strings.Replace(linearWorkflowBody, "do the work for {{task.title}}", "attempt {{ attempt }} for {{ task.title }}", 1))
	t.Setenv("REPO_URL", cloneURL)
	tk.Attempts = 2

	ev := &fakeEmitter{}
	cfg := workerCfgForIntegration(t)
	cfg.Workflow.PromptTemplate = "attempt {{ attempt }} for {{ task.title }}"

	if rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg); rterr != nil {
		t.Fatalf("runTask: %v", rterr.Err)
	}
	workdir := filepath.Join(cfg.WorkspaceRoot, "acme", "demo", "linear_issue", "issue-uuid")
	prompt, err := os.ReadFile(filepath.Join(workdir, ".aiops", "PROMPT.md"))
	if err != nil {
		t.Fatalf("read rendered prompt: %v", err)
	}
	if !strings.Contains(string(prompt), "attempt 2 for integration") {
		t.Fatalf("rendered prompt = %q, want attempt variable from task", prompt)
	}
}

func TestRunTaskRendersFirstAttemptAsEmptyForBareAttempt(t *testing.T) {
	cloneURL, tk := initBareUpstreamWithWorkflow(t, strings.Replace(linearWorkflowBody, "do the work for {{task.title}}", `attempt={{ attempt }} for {{ task.title }}`, 1))
	t.Setenv("REPO_URL", cloneURL)

	ev := &fakeEmitter{}
	cfg := workerCfgForIntegration(t)
	cfg.Workflow.PromptTemplate = `attempt={{ attempt }} for {{ task.title }}`

	if rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg); rterr != nil {
		t.Fatalf("runTask: %v", rterr.Err)
	}
	workdir := filepath.Join(cfg.WorkspaceRoot, "acme", "demo", "linear_issue", "issue-uuid")
	prompt, err := os.ReadFile(filepath.Join(workdir, ".aiops", "PROMPT.md"))
	if err != nil {
		t.Fatalf("read rendered prompt: %v", err)
	}
	if strings.Contains(string(prompt), "<nil>") || !strings.Contains(string(prompt), "attempt= for integration") {
		t.Fatalf("rendered prompt = %q, want bare first attempt to render empty, not <nil>", prompt)
	}
}

func TestRunTaskLeavesFirstAttemptAbsentForDefaultFilter(t *testing.T) {
	cloneURL, tk := initBareUpstreamWithWorkflow(t, strings.Replace(linearWorkflowBody, "do the work for {{task.title}}", `attempt {{ attempt | default: "first run" }} for {{ task.title }}`, 1))
	t.Setenv("REPO_URL", cloneURL)

	ev := &fakeEmitter{}
	cfg := workerCfgForIntegration(t)
	cfg.Workflow.PromptTemplate = `attempt {{ attempt | default: "first run" }} for {{ task.title }}`

	if rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg); rterr != nil {
		t.Fatalf("runTask: %v", rterr.Err)
	}
	workdir := filepath.Join(cfg.WorkspaceRoot, "acme", "demo", "linear_issue", "issue-uuid")
	prompt, err := os.ReadFile(filepath.Join(workdir, ".aiops", "PROMPT.md"))
	if err != nil {
		t.Fatalf("read rendered prompt: %v", err)
	}
	if !strings.Contains(string(prompt), "attempt first run for integration") {
		t.Fatalf("rendered prompt = %q, want first attempt absent so default filter applies", prompt)
	}
}

func TestRunTaskExposesIssueObjectToPromptTemplate(t *testing.T) {
	cloneURL, tk := initBareUpstreamWithWorkflow(t, strings.Replace(linearWorkflowBody, "do the work for {{task.title}}", `issue {{ issue.identifier }} {{ issue.title }} {{ issue.id | default: "no-id" }}`, 1))
	t.Setenv("REPO_URL", cloneURL)
	tk.SourceEventID = "LIN-123"

	ev := &fakeEmitter{}
	cfg := workerCfgForIntegration(t)
	cfg.Workflow.PromptTemplate = `issue {{ issue.identifier }} {{ issue.title }} {{ issue.id | default: "no-id" }}`

	if rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg); rterr != nil {
		t.Fatalf("runTask: %v", rterr.Err)
	}
	workdir := filepath.Join(cfg.WorkspaceRoot, "acme", "demo", "linear_issue", "lin-123")
	prompt, err := os.ReadFile(filepath.Join(workdir, ".aiops", "PROMPT.md"))
	if err != nil {
		t.Fatalf("read rendered prompt: %v", err)
	}
	if !strings.Contains(string(prompt), "issue LIN-123 integration no-id") {
		t.Fatalf("rendered prompt = %q, want issue object fields without conflating id and identifier", prompt)
	}
}

func TestRunTaskFailsOnUnknownPromptVariable(t *testing.T) {
	cloneURL, tk := initBareUpstreamWithWorkflow(t, strings.Replace(linearWorkflowBody, "do the work for {{task.title}}", "do the work for {{ missing }}", 1))
	t.Setenv("REPO_URL", cloneURL)

	ev := &fakeEmitter{}
	cfg := workerCfgForIntegration(t)
	cfg.Workflow.PromptTemplate = "do the work for {{ missing }}"

	rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg)
	if rterr == nil {
		t.Fatal("runTask succeeded, want template render failure")
	}
	if !strings.Contains(rterr.Err.Error(), "template_render_error") || !strings.Contains(rterr.Err.Error(), "missing") {
		t.Fatalf("runTask error = %q, want typed missing variable render error", rterr.Err)
	}
	if !rterr.NonRetryable {
		t.Fatal("runTask render failure was retryable, want deterministic template errors to fail fast")
	}
	var renderErr *workflow.TemplateRenderError
	if !errors.As(rterr.Err, &renderErr) {
		t.Fatalf("runTask error type = %T, want *workflow.TemplateRenderError", rterr.Err)
	}
}

func TestRunTaskExecutesAfterCreateHookOnRecreatedWorkspace(t *testing.T) {
	cloneURL, tk := initBareUpstreamWithWorkflow(t, linearWorkflowBody)
	t.Setenv("REPO_URL", cloneURL)

	ev := &fakeEmitter{}
	cfg := workerCfgForIntegration(t)
	cfg.Workflow.Config.Hooks = workflow.WorkspaceHooks{
		AfterCreate: workflow.WorkspaceHook{Commands: []string{"printf after_create >> hook.log"}},
	}

	if rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg); rterr != nil {
		t.Fatalf("first runTask: %v", rterr.Err)
	}
	workdir := filepath.Join(cfg.WorkspaceRoot, "acme", "demo", "linear_issue", "issue-uuid")
	if err := os.WriteFile(filepath.Join(workdir, "stale.txt"), []byte("stale"), 0o644); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}

	if rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg); rterr != nil {
		t.Fatalf("second runTask: %v", rterr.Err)
	}

	body, err := os.ReadFile(filepath.Join(workdir, "hook.log"))
	if err != nil {
		t.Fatalf("read recreated hook log: %v", err)
	}
	if string(body) != "after_create" {
		t.Fatalf("recreated hook log = %q, want after_create from the second fresh checkout", body)
	}
	if _, err := os.Stat(filepath.Join(workdir, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("second run should recreate a clean workspace without stale marker; stat err=%v", err)
	}
}

func TestRunTaskDoesNotExecuteAfterRunHookWhenRunnerSetupFails(t *testing.T) {
	cloneURL, tk := initBareUpstreamWithWorkflow(t, linearWorkflowBody)
	t.Setenv("REPO_URL", cloneURL)
	tk.Model = "does-not-exist"

	ev := &fakeEmitter{}
	cfg := workerCfgForIntegration(t)
	cfg.Workflow.Config.Hooks = workflow.WorkspaceHooks{
		BeforeRun: workflow.WorkspaceHook{Commands: []string{"printf before_run >> hook.log"}},
		AfterRun:  workflow.WorkspaceHook{Commands: []string{"printf after_run >> hook.log"}},
	}

	rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg)
	if rterr == nil {
		t.Fatal("runTask succeeded, want runner setup failure")
	}

	workdir := filepath.Join(cfg.WorkspaceRoot, "acme", "demo", "linear_issue", "issue-uuid")
	if body, err := os.ReadFile(filepath.Join(workdir, "hook.log")); err == nil {
		t.Fatalf("hook log = %q, want no before_run/after_run hooks when runner setup fails", body)
	} else if !os.IsNotExist(err) {
		t.Fatalf("read hook log: %v", err)
	}
	for _, e := range ev.byKind(task.EventWorkspaceHookStart) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			t.Fatalf("hook start payload = %#v, want map", e.Payload)
		}
		hook := fmt.Sprint(payload["hook"])
		if hook == "before_run" || hook == "after_run" {
			t.Fatalf("unexpected %s hook start after runner setup failure; events=%#v", hook, ev.events)
		}
	}
}

func TestRunTaskRunsBeforeRemoveHookWhenAfterCreateFails(t *testing.T) {
	cloneURL, tk := initBareUpstreamWithWorkflow(t, linearWorkflowBody)
	t.Setenv("REPO_URL", cloneURL)

	ev := &fakeEmitter{}
	cfg := workerCfgForIntegration(t)
	marker := filepath.Join(cfg.WorkspaceRoot, "before-remove.marker")
	cfg.Workflow.Config.Hooks = workflow.WorkspaceHooks{
		AfterCreate: workflow.WorkspaceHook{Commands: []string{"printf after_create > hook.log", "exit 7"}},
		BeforeRemove: workflow.WorkspaceHook{Commands: []string{
			"test -d . && printf before_remove > " + shellQuoteForTest(marker),
		}},
	}

	rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg)
	if rterr == nil {
		t.Fatal("runTask succeeded, want after_create hook failure")
	}

	workdir := filepath.Join(cfg.WorkspaceRoot, "acme", "demo", "linear_issue", "issue-uuid")
	if _, err := os.Stat(workdir); !os.IsNotExist(err) {
		t.Fatalf("workdir stat err = %v, want removed after after_create failure", err)
	}
	if body, err := os.ReadFile(marker); err != nil || string(body) != "before_remove" {
		t.Fatalf("before_remove marker = %q, %v; want hook to run before removing failed workspace", body, err)
	}
	var beforeRemoveStarts int
	for _, e := range ev.byKind(task.EventWorkspaceHookStart) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			t.Fatalf("hook start payload = %#v, want map", e.Payload)
		}
		if fmt.Sprint(payload["hook"]) == "before_remove" {
			beforeRemoveStarts++
		}
	}
	if beforeRemoveStarts != 1 {
		t.Fatalf("before_remove hook starts = %d, want 1; events=%#v", beforeRemoveStarts, ev.events)
	}
}

// TestRunTask_SuccessDoesNotPushCreatePROrWriteTracker pins the SPEC §1
// boundary: a successful worker run prepares the workspace, executes the
// agent, and enforces gates, but it does not push branches, create PRs, or
// mutate tracker state. Those writes belong to the agent/tool surface.
func TestAnalysisOnlyRunAllowsPlanArtifactWithoutSourceChanges(t *testing.T) {
	analysisWorkflow := strings.Replace(linearWorkflowBody, "agent:\n  default: mock", "agent:\n  default: mock\npolicy:\n  mode: analysis_only", 1)
	cloneURL, tk := initBareUpstreamWithWorkflow(t, analysisWorkflow)
	t.Setenv("REPO_URL", cloneURL)

	ev := &fakeEmitter{}
	cfg := workerCfgForIntegration(t)
	cfg.Workflow.Config.Policy.Mode = "analysis_only"

	if rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg); rterr != nil {
		t.Fatalf("runTask: %v", rterr.Err)
	}

	workdir := filepath.Join(cfg.WorkspaceRoot, tk.RepoOwner, tk.RepoName, "linear_issue", tk.SourceEventID)
	if _, err := os.Stat(filepath.Join(workdir, ".aiops", "PLAN.md")); err != nil {
		t.Fatalf("analysis-only run should write plan artifact: %v", err)
	}
	refs, err := exec.Command("git", "--git-dir", cloneURL[len("file://"):], "for-each-ref", "--format=%(refname:short)", "refs/heads").CombinedOutput()
	if err != nil {
		t.Fatalf("list upstream refs: %v\n%s", err, refs)
	}
	if string(refs) != "main\n" {
		t.Fatalf("analysis-only run must not push work branches; upstream refs:\n%s", refs)
	}
}

func TestAnalysisOnlyRunRejectsSourceChanges(t *testing.T) {
	testAnalysisOnlyMockFailure(t, "mock-source-change", "analysis-only run changed source files", task.EventAnalysisOnlyViolation)
}

func TestAnalysisOnlyRunRejectsCommittedSourceChanges(t *testing.T) {
	testAnalysisOnlyMockFailure(t, "mock-commit-source-change", "analysis-only run changed source files", task.EventAnalysisOnlyViolation)
}

func TestAnalysisOnlyRunRejectsCommittedSourceChangesEvenIfRunnerMutatesBaseConfig(t *testing.T) {
	testAnalysisOnlyMockFailure(t, "mock-commit-source-change-and-reset-base-config", "analysis-only run changed source files", task.EventAnalysisOnlyViolation)
}

func TestAnalysisOnlyRunRejectsCommittedArtifacts(t *testing.T) {
	testAnalysisOnlyMockFailure(t, "mock-commit-analysis-artifact", "analysis-only run created commits", task.EventAnalysisOnlyViolation)
}

func TestAnalysisOnlyRunRequiresPlanArtifact(t *testing.T) {
	testAnalysisOnlyMockFailure(t, "mock-no-plan", "analysis-only run did not produce .aiops/PLAN.md", task.EventAnalysisOnlyViolation)
}

func TestAnalysisOnlyRunRequiresFreshPlanArtifact(t *testing.T) {
	analysisWorkflow := strings.Replace(linearWorkflowBody, "agent:\n  default: mock", "agent:\n  default: mock-no-plan\npolicy:\n  mode: analysis_only", 1)
	cloneURL, tk := initBareUpstreamWithWorkflow(t, analysisWorkflow)
	t.Setenv("REPO_URL", cloneURL)
	tk.Model = "mock-no-plan"
	seed := cloneURL[len("file://"):]
	work := filepath.Join(t.TempDir(), "seed-plan")
	for _, args := range [][]string{
		{"git", "clone", "-q", seed, work},
		{"git", "-C", work, "config", "user.email", "u@example.com"},
		{"git", "-C", work, "config", "user.name", "u"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(filepath.Join(work, ".aiops"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".aiops", "PLAN.md"), []byte("stale committed plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "-C", work, "add", ".aiops/PLAN.md"},
		{"git", "-C", work, "commit", "-q", "-m", "seed stale plan"},
		{"git", "-C", work, "push", "-q", "origin", "main"},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	ev := &fakeEmitter{}
	cfg := workerCfgForIntegration(t)
	cfg.Workflow.Config.Agent.Default = "mock-no-plan"
	cfg.Workflow.Config.Policy.Mode = "analysis_only"

	rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg)
	if rterr == nil {
		t.Fatal("analysis-only run with only a stale base PLAN.md succeeded, want policy failure")
	}
	if !strings.Contains(rterr.Err.Error(), "analysis-only run did not produce .aiops/PLAN.md") {
		t.Fatalf("error = %v, want stale plan rejection", rterr.Err)
	}
	if got := len(ev.byKind(task.EventAnalysisOnlyViolation)); got != 1 {
		t.Fatalf("analysis_only_violation events = %d, want 1; events=%#v", got, ev.events)
	}
}

func TestAnalysisOnlyRunRejectsRepoOwnedAiopsChanges(t *testing.T) {
	testAnalysisOnlyMockFailure(t, "mock-aiops-workflow-change", "analysis-only run changed source files", task.EventAnalysisOnlyViolation)
}

func testAnalysisOnlyMockFailure(t *testing.T, runnerName, wantErr, wantEvent string) {
	t.Helper()
	analysisWorkflow := strings.Replace(linearWorkflowBody, "agent:\n  default: mock", "agent:\n  default: "+runnerName+"\npolicy:\n  mode: analysis_only", 1)
	cloneURL, tk := initBareUpstreamWithWorkflow(t, analysisWorkflow)
	t.Setenv("REPO_URL", cloneURL)
	tk.Model = runnerName

	ev := &fakeEmitter{}
	cfg := workerCfgForIntegration(t)
	cfg.Workflow.Config.Agent.Default = runnerName
	cfg.Workflow.Config.Policy.Mode = "analysis_only"

	rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg)
	if rterr == nil {
		t.Fatalf("analysis-only run with %s succeeded, want policy failure", runnerName)
	}
	if !strings.Contains(rterr.Err.Error(), wantErr) {
		t.Fatalf("error = %v, want %q", rterr.Err, wantErr)
	}
	if got := len(ev.byKind(wantEvent)); got != 1 {
		t.Fatalf("%s events = %d, want 1; events=%#v", wantEvent, got, ev.events)
	}
}

func TestRunTask_SuccessDoesNotPushCreatePROrWriteTracker(t *testing.T) {
	cloneURL, tk := initBareUpstreamWithWorkflow(t, linearWorkflowBody)
	t.Setenv("REPO_URL", cloneURL)

	ev := &fakeEmitter{}
	cfg := workerCfgForIntegration(t)

	if rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg); rterr != nil {
		t.Fatalf("runTask: %v", rterr.Err)
	}

	for _, forbidden := range []string{
		task.EventPush,
		task.EventPRCreated,
		task.EventPRReused,
		task.EventTrackerTransition,
		task.EventTrackerTransitionError,
		task.EventTrackerComment,
	} {
		if got := len(ev.byKind(forbidden)); got != 0 {
			t.Fatalf("worker emitted forbidden event %q %d time(s); events=%#v", forbidden, got, ev.events)
		}
	}
	if idxResolved := indexOfEvent(ev, task.EventWorkflowResolved); idxResolved < 0 {
		t.Fatalf("workflow_resolved event not emitted; events=%#v", ev.events)
	}
	if got := len(ev.byKind(task.EventRunnerEnd)); got != 1 {
		t.Fatalf("runner_end events = %d, want 1; events=%#v", got, ev.events)
	}
	if got := len(ev.byKind(task.EventVerifyEnd)); got != 1 {
		t.Fatalf("verify_end events = %d, want 1; events=%#v", got, ev.events)
	}

	refs, err := exec.Command("git", "--git-dir", cloneURL[len("file://"):], "for-each-ref", "--format=%(refname:short)", "refs/heads").CombinedOutput()
	if err != nil {
		t.Fatalf("list upstream refs: %v\n%s", err, refs)
	}
	if string(refs) != "main\n" {
		t.Fatalf("worker must not push work branches; upstream refs:\n%s", refs)
	}
}

// indexOfEvent returns the position of the first recorded event with the
// given kind, or -1 if none exists. The events slice is read under the
// emitter's lock to stay race-free with concurrent writers (in practice
// runTask is single-goroutine but the lock keeps the helper honest).
func indexOfEvent(ev *fakeEmitter, kind string) int {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	for i, e := range ev.events {
		if e.Kind == kind {
			return i
		}
	}
	return -1
}

// fakeRunStore implements the structural contract Run requires from its
// store (Claim/Complete + EventEmitter + failingStore) so Run can be
// driven without Postgres. It hands out a single task, then synthesises
// ctx cancellation via the onClaimed hook so the worker loop exits
// deterministically after the failure path runs.
type fakeRunStore struct {
	task       *task.Task
	failResult bool
	failErr    error

	mu        sync.Mutex
	claimed   bool
	events    []recordedEvent
	failCalls int
	onClaimed func()
}

func (s *fakeRunStore) Claim(ctx context.Context) (*task.Task, error) {
	s.mu.Lock()
	already := s.claimed
	s.claimed = true
	s.mu.Unlock()
	if already {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.task, nil
}

func (s *fakeRunStore) Complete(_ context.Context, _ string) error { return nil }

func (s *fakeRunStore) AddEvent(_ context.Context, _, kind, msg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, recordedEvent{Kind: kind, Message: msg})
	return nil
}

func (s *fakeRunStore) AddEventWithPayload(_ context.Context, _, kind, msg string, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, recordedEvent{Kind: kind, Message: msg, Payload: payload})
	return nil
}

func (s *fakeRunStore) Fail(_ context.Context, _, _ string) (bool, error) {
	s.mu.Lock()
	s.failCalls++
	cb := s.onClaimed
	s.mu.Unlock()
	if cb != nil {
		cb()
	}
	return s.failResult, s.failErr
}

func (s *fakeRunStore) FailTerminal(_ context.Context, _, _ string) error {
	s.mu.Lock()
	s.failCalls++
	cb := s.onClaimed
	s.mu.Unlock()
	if cb != nil {
		cb()
	}
	return s.failErr
}

func (s *fakeRunStore) FailTimeout(_ context.Context, _, _ string, _ int) (bool, error) {
	// runTask in these tests fails at PrepareGitWorkspace, which is not a
	// runner.TimeoutError, so handleTaskFailure always takes the Fail()
	// path. Implementing FailTimeout here just keeps the interface satisfied.
	return false, nil
}

func (s *fakeRunStore) byKind(kind string) []recordedEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []recordedEvent
	for _, e := range s.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func shellQuoteForTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// TestRun_DoesNotWriteTrackerOnFailure pins the SPEC §1 boundary for the
// failure path: terminality only controls queue retry state. The worker must
// not construct tracker transitioners or move the linked issue to Rework.
func TestRun_DoesNotWriteTrackerOnFailure(t *testing.T) {
	tk := &task.Task{
		ID:            "tsk_fail",
		CloneURL:      "file:///definitely-not-a-real-path/spec-boundary",
		BaseBranch:    "main",
		WorkBranch:    "ai/tsk_fail",
		SourceType:    "linear_issue",
		SourceEventID: "issue-uuid",
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &fakeRunStore{
		task:       tk,
		failResult: true,
	}
	store.onClaimed = func() {
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()
	}

	cfg := worker.Config{
		WorkspaceRoot: t.TempDir(),
		MirrorRoot:    t.TempDir(),
		Workflow: &workflow.Workflow{
			Config:         workflow.DefaultConfig(),
			PromptTemplate: workflow.DefaultPrompt(),
			Source:         workflow.SourceDefault,
		},
	}

	done := make(chan struct{})
	go func() {
		worker.Run(ctx, store, cfg)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("Run did not exit within 5s")
	}

	if store.failCalls != 1 {
		t.Fatalf("Fail calls = %d, want 1", store.failCalls)
	}
	for _, forbidden := range []string{
		task.EventTrackerTransition,
		task.EventTrackerTransitionError,
		task.EventTrackerComment,
	} {
		if got := len(store.byKind(forbidden)); got != 0 {
			t.Fatalf("worker emitted forbidden tracker event %q %d time(s); events=%#v", forbidden, got, store.events)
		}
	}
}
