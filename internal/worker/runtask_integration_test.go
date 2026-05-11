package worker_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
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

// workerCfgForIntegration assembles the Config the integration tests share:
// per-test workspace/mirror roots plus the fake transitioner/PR-client
// factories. Returns the cfg with fakes already wired.
func workerCfgForIntegration(t *testing.T, tr *fakeTransitioner, pr *fakePRClient) worker.Config {
	t.Helper()
	return worker.Config{
		WorkspaceRoot: t.TempDir(),
		MirrorRoot:    t.TempDir(),
		NewTransitioner: func(_ workflow.TrackerConfig) worker.Transitioner {
			return tr
		},
		NewPRClient: func() worker.PRClient { return pr },
	}
}

// TestRunTask_FiresClaimThenPRCreatedOnSuccess pins acceptance criterion 1
// for issue #60: when runTask completes successfully against a linear
// tracker config, the transitioner sees the move calls in order
// (InProgress on claim, then HumanReview on PR open), and the
// tracker_transition events are sequenced after workflow_resolved so a
// future refactor that reorders OnClaim ahead of ResolveWorkflow is
// caught.
func TestRunTask_FiresClaimThenPRCreatedOnSuccess(t *testing.T) {
	cloneURL, tk := initBareUpstreamWithWorkflow(t, linearWorkflowBody)
	t.Setenv("REPO_URL", cloneURL)

	tr := &fakeTransitioner{}
	pr := &fakePRClient{
		createPR: &gitea.PullRequest{Number: 7, HTMLURL: "http://gitea.local/acme/demo/pulls/7", Title: "chore(ai): integration"},
	}
	ev := &fakeEmitter{}
	cfg := workerCfgForIntegration(t, tr, pr)

	if rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg); rterr != nil {
		t.Fatalf("runTask: %v", rterr.Err)
	}

	if len(tr.moves) != 2 {
		t.Fatalf("MoveIssueToState calls = %d, want 2 (claim + pr); got %#v", len(tr.moves), tr.moves)
	}
	if tr.moves[0] != (moveCall{IssueID: "issue-uuid", State: "In Progress"}) {
		t.Fatalf("first move = %#v, want issue-uuid -> In Progress", tr.moves[0])
	}
	if tr.moves[1] != (moveCall{IssueID: "issue-uuid", State: "Human Review"}) {
		t.Fatalf("second move = %#v, want issue-uuid -> Human Review", tr.moves[1])
	}

	// The PR client must have been asked to create (not just looked up):
	// guards the create-new path explicitly.
	if len(pr.createCalls) != 1 {
		t.Fatalf("CreatePullRequest calls = %d, want 1 on create-new path", len(pr.createCalls))
	}

	// Order: workflow_resolved (from ResolveWorkflow) must appear before
	// the first tracker_transition (from OnClaim). Guards against a
	// refactor that fires OnClaim before the workflow is loaded, which
	// would race the per-task tracker config we resolve from
	// wf.Config.Tracker.
	idxResolved := indexOfEvent(ev, task.EventWorkflowResolved)
	idxFirstTransition := indexOfEvent(ev, task.EventTrackerTransition)
	if idxResolved < 0 {
		t.Fatalf("workflow_resolved event not emitted; events=%#v", ev.events)
	}
	if idxFirstTransition < 0 {
		t.Fatalf("tracker_transition event not emitted")
	}
	if idxFirstTransition < idxResolved {
		t.Fatalf("tracker_transition (idx=%d) fired before workflow_resolved (idx=%d); OnClaim must run after ResolveWorkflow", idxFirstTransition, idxResolved)
	}

	// Order: pr_created must appear before the second tracker_transition,
	// because OnPRCreated only runs when CreatePR returned nil.
	idxPRCreated := indexOfEvent(ev, task.EventPRCreated)
	idxLastTransition := lastIndexOfEvent(ev, task.EventTrackerTransition)
	if idxPRCreated < 0 {
		t.Fatalf("pr_created event not emitted")
	}
	if idxLastTransition <= idxPRCreated {
		t.Fatalf("second tracker_transition (idx=%d) must fire after pr_created (idx=%d)", idxLastTransition, idxPRCreated)
	}
}

// TestRunTask_FiresPRCreatedOnReusePath covers the second half of AC1:
// when CreatePR takes the reuse-existing branch (FindOpenPullRequest
// returns a PR), OnPRCreated must still fire so the linked Linear issue
// flips to Human Review. A retry that lands on an already-open PR should
// not skip the tracker handoff.
func TestRunTask_FiresPRCreatedOnReusePath(t *testing.T) {
	cloneURL, tk := initBareUpstreamWithWorkflow(t, linearWorkflowBody)
	t.Setenv("REPO_URL", cloneURL)

	tr := &fakeTransitioner{}
	pr := &fakePRClient{
		findResult: &gitea.PullRequest{Number: 13, HTMLURL: "http://gitea.local/acme/demo/pulls/13", Title: "chore(ai): integration"},
	}
	cfg := workerCfgForIntegration(t, tr, pr)
	ev := &fakeEmitter{}

	if rterr := worker.RunTaskForTest(context.Background(), ev, tk, cfg); rterr != nil {
		t.Fatalf("runTask: %v", rterr.Err)
	}

	if len(pr.createCalls) != 0 {
		t.Fatalf("CreatePullRequest must not be called on reuse path; got %d", len(pr.createCalls))
	}
	if len(tr.moves) != 2 {
		t.Fatalf("MoveIssueToState calls = %d, want 2 even on reuse path; got %#v", len(tr.moves), tr.moves)
	}
	if tr.moves[1].State != "Human Review" {
		t.Fatalf("second move state = %q, want \"Human Review\" on PR reuse", tr.moves[1].State)
	}
	if got := len(ev.byKind(task.EventPRReused)); got != 1 {
		t.Fatalf("pr_reused events = %d, want 1", got)
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

func lastIndexOfEvent(ev *fakeEmitter, kind string) int {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	for i := len(ev.events) - 1; i >= 0; i-- {
		if ev.events[i].Kind == kind {
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

// TestRun_OnFailureGatedByTerminality pins acceptance criterion 2: the
// worker loop must call cfg.NewTransitioner(rterr.Cfg.Tracker) (the entry
// point for OnFailure) only when handleTaskFailure reports terminal=true.
// A re-queued failure (terminal=false) must skip the tracker write so the
// linked Linear issue does not flicker between In Progress and Rework
// during transient retries, which would otherwise trip the poller's
// Rework re-enqueue path.
func TestRun_OnFailureGatedByTerminality(t *testing.T) {
	cases := []struct {
		name          string
		failResult    bool
		wantNewTrCall int
	}{
		{"requeue_skips_onfailure", false, 0},
		{"terminal_invokes_onfailure", true, 1},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Drive runTask to fail at PrepareGitWorkspace by pointing
			// CloneURL at a path that cannot be cloned. This keeps
			// rterr.Cfg empty (Kind="") so cfg.NewTransitioner's call
			// count is the cleanest signal of which branch Run took.
			tk := &task.Task{
				ID:            "tsk_fail",
				CloneURL:      "file:///definitely-not-a-real-path/" + tc.name,
				BaseBranch:    "main",
				WorkBranch:    "ai/tsk_fail",
				SourceType:    "linear_issue",
				SourceEventID: "issue-uuid",
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			store := &fakeRunStore{
				task:       tk,
				failResult: tc.failResult,
			}
			store.onClaimed = func() {
				// Cancel after Fail returns so Run exits the loop
				// before re-entering Claim. The brief delay defers
				// cancellation until after the terminal-branch
				// NewTransitioner call has happened.
				go func() {
					time.Sleep(20 * time.Millisecond)
					cancel()
				}()
			}

			var newTrCalls int
			var trMu sync.Mutex
			cfg := worker.Config{
				WorkspaceRoot: t.TempDir(),
				MirrorRoot:    t.TempDir(),
				NewTransitioner: func(_ workflow.TrackerConfig) worker.Transitioner {
					trMu.Lock()
					newTrCalls++
					trMu.Unlock()
					return nil
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
			trMu.Lock()
			got := newTrCalls
			trMu.Unlock()
			if got != tc.wantNewTrCall {
				t.Fatalf("NewTransitioner calls = %d, want %d (terminal=%v)", got, tc.wantNewTrCall, tc.failResult)
			}
		})
	}
}
