package worker_test

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
agent:
  default: mock
---
do the work for {{task.title}}
`

// workerCfgForIntegration assembles the Config the integration tests share.
func workerCfgForIntegration(t *testing.T) worker.Config {
	t.Helper()
	wf, err := workflow.Load(writeServiceWorkflowForIntegration(t, linearWorkflowBody))
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	return worker.Config{
		WorkspaceRoot: t.TempDir(),
		MirrorRoot:    t.TempDir(),
		Workflow:      wf,
	}
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

	workdir := filepath.Join(cfg.WorkspaceRoot, "acme", "demo", "linear-issue", "issue-uuid")
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
	workdir := filepath.Join(cfg.WorkspaceRoot, "acme", "demo", "linear-issue", "issue-uuid")
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

func TestRunTaskExecutesAfterRunHookWhenRunnerFails(t *testing.T) {
	cloneURL, tk := initBareUpstreamWithWorkflow(t, linearWorkflowBody)
	t.Setenv("REPO_URL", cloneURL)
	tk.Model = "does-not-exist"

	ev := &fakeEmitter{}
	cfg := workerCfgForIntegration(t)
	cfg.Workflow.Config.Hooks = workflow.WorkspaceHooks{
		AfterRun: workflow.WorkspaceHook{Commands: []string{"printf after_run >> hook.log"}},
	}

	rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg)
	if rterr == nil {
		t.Fatal("runTask succeeded, want runner setup failure")
	}

	workdir := filepath.Join(cfg.WorkspaceRoot, "acme", "demo", "linear-issue", "issue-uuid")
	body, err := os.ReadFile(filepath.Join(workdir, "hook.log"))
	if err != nil {
		t.Fatalf("read hook log: %v", err)
	}
	if string(body) != "after_run" {
		t.Fatalf("hook log = %q, want after_run hook to execute on failed attempt", body)
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

	workdir := filepath.Join(cfg.WorkspaceRoot, tk.RepoOwner, tk.RepoName, "linear-issue", tk.SourceEventID)
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
