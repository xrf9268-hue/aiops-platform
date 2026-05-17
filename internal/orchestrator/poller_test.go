package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

type fakeIssueTracker struct {
	issues []tracker.Issue
	err    error
	calls  int
}

func (f *fakeIssueTracker) ListActiveIssues(_ context.Context) ([]tracker.Issue, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.issues, nil
}

type recordingDispatcher struct {
	mu     sync.Mutex
	issues []tracker.Issue
}

func (d *recordingDispatcher) Spawn(_ context.Context, issue tracker.Issue, _ *int) <-chan WorkerResult {
	d.mu.Lock()
	d.issues = append(d.issues, issue)
	d.mu.Unlock()
	ch := make(chan WorkerResult, 1)
	ch <- WorkerResult{Elapsed: time.Millisecond}
	close(ch)
	return ch
}

func (d *recordingDispatcher) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.issues)
}

func (d *recordingDispatcher) issueAt(i int) tracker.Issue {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.issues[i]
}

type erroringTaskDispatcher struct{}

func (d erroringTaskDispatcher) Spawn(_ context.Context, _ tracker.Issue, _ *int) <-chan WorkerResult {
	ch := make(chan WorkerResult, 1)
	ch <- WorkerResult{Err: errors.New("repo.clone_url missing in WORKFLOW.md"), Elapsed: time.Millisecond}
	close(ch)
	return ch
}

func TestPollOnceDispatchesTrackerCandidatesThroughRuntimeStateWithoutQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "AI Ready"}}}
	dispatcher := &recordingDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  FixedDelayScheduler{Delay: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPoller(trackerClient, orch)
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll once: %v", err)
	}

	if trackerClient.calls != 1 {
		t.Fatalf("ListActiveIssues calls = %d, want 1", trackerClient.calls)
	}
	if got := dispatcher.count(); got != 1 {
		t.Fatalf("dispatcher issues = %d, want 1", got)
	}
	if got := dispatcher.issueAt(0).ID; got != "issue-1" {
		t.Fatalf("dispatched issue ID = %q, want issue-1", got)
	}

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("second poll once: %v", err)
	}
	if got := dispatcher.count(); got != 1 {
		t.Fatalf("dispatcher issues after duplicate poll = %d, want 1", got)
	}
}

func TestPollOnceRedispatchesIssueAfterPriorRunCompleted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "Rework"}}}
	dispatcher := &recordingDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  FixedDelayScheduler{Delay: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPoller(trackerClient, orch)
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll once: %v", err)
	}
	waitForDispatcherCount(t, dispatcher, 1)
	waitForCompleted(t, ctx, orch, "issue-1")

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("rework poll once: %v", err)
	}
	waitForDispatcherCount(t, dispatcher, 2)
}

func TestPollOnceRetriesAfterBuildTaskFailureWithoutLeakingRunningState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "AI Ready"}}}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: erroringTaskDispatcher{},
		Scheduler:  FixedDelayScheduler{Delay: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPoller(trackerClient, orch)
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll once: %v", err)
	}
	waitForRetryQueued(t, ctx, orch, "issue-1")

	view, err := orch.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, running := range view.Running {
		if running.IssueID == "issue-1" {
			t.Fatalf("issue-1 still running after build-task failure")
		}
	}
}

func waitForDispatcherCount(t *testing.T, dispatcher *recordingDispatcher, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := dispatcher.count(); got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("dispatcher issues = %d, want %d", dispatcher.count(), want)
}

func waitForCompleted(t *testing.T, ctx context.Context, orch *Orchestrator, id IssueID) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		view, err := orch.Snapshot(ctx)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		for _, completed := range view.Completed {
			if completed == id {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("issue %s was not marked completed", id)
}

func waitForRetryQueued(t *testing.T, ctx context.Context, orch *Orchestrator, id IssueID) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		view, err := orch.Snapshot(ctx)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		for _, retry := range view.Retrying {
			if retry.IssueID == id {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("issue %s was not retry queued", id)
}
