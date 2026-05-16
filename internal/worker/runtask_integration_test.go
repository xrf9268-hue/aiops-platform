package worker_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	worker "github.com/xrf9268-hue/aiops-platform/internal/worker"
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
	return worker.Config{
		WorkspaceRoot: t.TempDir(),
		MirrorRoot:    t.TempDir(),
	}
}

// TestRunTask_SuccessDoesNotPushCreatePROrWriteTracker pins the SPEC §1
// boundary: a successful worker run prepares the workspace, executes the
// agent, and enforces gates, but it does not push branches, create PRs, or
// mutate tracker state. Those writes belong to the agent/tool surface.
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
