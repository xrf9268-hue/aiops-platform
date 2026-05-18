package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
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
	mu        sync.Mutex
	issues    []tracker.Issue
	releaseCh chan struct{}
}

func (d *recordingDispatcher) Spawn(_ context.Context, issue tracker.Issue, _ *int) <-chan WorkerResult {
	d.mu.Lock()
	d.issues = append(d.issues, issue)
	releaseCh := d.releaseCh
	d.mu.Unlock()
	ch := make(chan WorkerResult, 1)
	go func() {
		if releaseCh != nil {
			<-releaseCh
		}
		ch <- WorkerResult{Elapsed: time.Millisecond}
		close(ch)
	}()
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

type completingEmitter struct {
	worker.LogEventEmitter
	mu        sync.Mutex
	completed []string
}

func (e *completingEmitter) Complete(_ context.Context, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.completed = append(e.completed, id)
	return nil
}

func (e *completingEmitter) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.completed)
}

func (e *completingEmitter) completedID(i int) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.completed[i]
}

func TestCompleteRecordedTaskMarksOptionalQueueRowTerminal(t *testing.T) {
	ctx := context.Background()
	emitter := &completingEmitter{}

	if err := completeRecordedTask(ctx, emitter, "row-1", "issue-1"); err != nil {
		t.Fatalf("completeRecordedTask returned error: %v", err)
	}
	if got := emitter.count(); got != 1 {
		t.Fatalf("completed task rows = %d, want 1", got)
	}
}

func TestCompleteRecordedTaskUsesRecordedQueueRowID(t *testing.T) {
	ctx := context.Background()
	emitter := &completingEmitter{}

	if err := completeRecordedTask(ctx, emitter, "tsk_recorded", "issue-1"); err != nil {
		t.Fatalf("completeRecordedTask returned error: %v", err)
	}
	if got := emitter.completedID(0); got != "tsk_recorded" {
		t.Fatalf("completed task id = %q, want recorded queue row id", got)
	}
}

type erroringTaskDispatcher struct {
	mu    sync.Mutex
	calls int
}

func (d *erroringTaskDispatcher) Spawn(_ context.Context, _ tracker.Issue, _ *int) <-chan WorkerResult {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	ch := make(chan WorkerResult, 1)
	ch <- WorkerResult{Err: errors.New("repo.clone_url missing in WORKFLOW.md"), NonRetryable: true, Elapsed: time.Millisecond}
	close(ch)
	return ch
}

func (d *erroringTaskDispatcher) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

type blockingDispatcher struct {
	mu     sync.Mutex
	issues []tracker.Issue
}

func (d *blockingDispatcher) Spawn(ctx context.Context, issue tracker.Issue, _ *int) <-chan WorkerResult {
	d.mu.Lock()
	d.issues = append(d.issues, issue)
	d.mu.Unlock()
	ch := make(chan WorkerResult, 1)
	go func() {
		<-ctx.Done()
		ch <- WorkerResult{Err: ctx.Err(), Elapsed: time.Millisecond}
		close(ch)
	}()
	return ch
}

func (d *blockingDispatcher) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.issues)
}

type fakeIssueStateTracker struct {
	mu     sync.Mutex
	issues []tracker.Issue
	err    error
}

type fakeIssueStateTrackerByCall struct {
	mu        sync.Mutex
	issues    [][]tracker.Issue
	errByCall []error
	calls     int
}

func (f *fakeIssueStateTracker) ListActiveIssues(ctx context.Context) ([]tracker.Issue, error) {
	return f.ListIssuesByStates(ctx, nil)
}

func (f *fakeIssueStateTracker) ListIssuesByStates(_ context.Context, states []string) ([]tracker.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	wanted := normalizedStates(states)
	out := make([]tracker.Issue, 0, len(f.issues))
	for _, issue := range f.issues {
		if isActiveTrackerState(issue.State, wanted) {
			out = append(out, issue)
		}
	}
	return out, nil
}

func (f *fakeIssueStateTracker) setIssues(issues []tracker.Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues = issues
}

func (f *fakeIssueStateTracker) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeIssueStateTrackerByCall) ListIssuesByStates(_ context.Context, _ []string) ([]tracker.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := f.calls
	f.calls++
	if call < len(f.errByCall) && f.errByCall[call] != nil {
		return nil, f.errByCall[call]
	}
	if call < len(f.issues) {
		return f.issues[call], nil
	}
	return nil, nil
}

type cancellationDispatcher struct {
	mu       sync.Mutex
	issues   []tracker.Issue
	contexts []context.Context
}

func (d *cancellationDispatcher) Spawn(ctx context.Context, issue tracker.Issue, _ *int) <-chan WorkerResult {
	d.mu.Lock()
	d.issues = append(d.issues, issue)
	d.contexts = append(d.contexts, ctx)
	d.mu.Unlock()
	ch := make(chan WorkerResult, 1)
	go func() {
		<-ctx.Done()
		ch <- WorkerResult{Err: ctx.Err(), Elapsed: time.Millisecond}
		close(ch)
	}()
	return ch
}

func (d *cancellationDispatcher) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.issues)
}

func (d *cancellationDispatcher) contextAt(i int) context.Context {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.contexts[i]
}

type stuckCancellationDispatcher struct {
	mu       sync.Mutex
	issues   []tracker.Issue
	contexts []context.Context
}

func (d *stuckCancellationDispatcher) Spawn(ctx context.Context, issue tracker.Issue, _ *int) <-chan WorkerResult {
	d.mu.Lock()
	d.issues = append(d.issues, issue)
	d.contexts = append(d.contexts, ctx)
	d.mu.Unlock()
	return make(chan WorkerResult)
}

func (d *stuckCancellationDispatcher) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.issues)
}

func (d *stuckCancellationDispatcher) contextAt(i int) context.Context {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.contexts[i]
}

func TestPollOnceCancelsRunningIssueWhenTrackerMovesToCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}}
	dispatcher := &cancellationDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPollerWithReconciliation(trackerClient, orch, ReconciliationConfig{
		ActiveStates:      []string{"In Progress", "Rework"},
		TerminalStates:    []string{"Cancelled", "Done"},
		WorkerExitTimeout: time.Second,
	})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll once: %v", err)
	}
	waitForCancellationDispatcherCount(t, dispatcher, 1)

	trackerClient.setIssues([]tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "Cancelled"}})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("cancelled poll once: %v", err)
	}
	waitForContextCanceled(t, dispatcher.contextAt(0))
	waitForNoRunningOrRetrying(t, ctx, orch, "issue-1")
}

func TestPollOnceCancelsRunningIssueWhenTrackerMovesToBacklog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}}
	dispatcher := &cancellationDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPollerWithReconciliation(trackerClient, orch, ReconciliationConfig{
		ActiveStates:      []string{"In Progress", "Rework"},
		TerminalStates:    []string{"Cancelled", "Done"},
		InactiveStates:    []string{"Backlog"},
		WorkerExitTimeout: time.Second,
	})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll once: %v", err)
	}
	waitForCancellationDispatcherCount(t, dispatcher, 1)

	trackerClient.setIssues([]tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "Backlog"}})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("backlog poll once: %v", err)
	}
	waitForContextCanceled(t, dispatcher.contextAt(0))
	waitForNoRunningOrRetrying(t, ctx, orch, "issue-1")
}

func TestPollOnceTrackerErrorDoesNotCancelRunningIssue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}}
	dispatcher := &cancellationDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPollerWithReconciliation(trackerClient, orch, ReconciliationConfig{
		ActiveStates:      []string{"In Progress", "Rework"},
		TerminalStates:    []string{"Cancelled", "Done"},
		WorkerExitTimeout: time.Second,
	})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll once: %v", err)
	}
	waitForCancellationDispatcherCount(t, dispatcher, 1)

	trackerClient.setErr(fmt.Errorf("tracker 5xx"))
	if err := poller.PollOnce(ctx); err == nil {
		t.Fatalf("tracker-error poll once returned nil, want error")
	}
	select {
	case <-dispatcher.contextAt(0).Done():
		t.Fatalf("running issue context was canceled after tracker error")
	default:
	}
}

func TestPollOnceCancelsTerminalIssueBeforeReturningLaterInactiveFetchError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueStateTrackerByCall{issues: [][]tracker.Issue{
		{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}},
		{},
		{},
		{},
		{{ID: "issue-1", Identifier: "LIN-1", State: "Done"}},
	}, errByCall: []error{
		nil,
		nil,
		nil,
		nil,
		nil,
		fmt.Errorf("inactive state fetch failed"),
	}}
	dispatcher := &cancellationDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPollerWithReconciliation(trackerClient, orch, ReconciliationConfig{
		ActiveStates:      []string{"In Progress", "Rework"},
		TerminalStates:    []string{"Cancelled", "Done"},
		InactiveStates:    []string{"Backlog"},
		WorkerExitTimeout: time.Second,
	})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll once: %v", err)
	}
	waitForCancellationDispatcherCount(t, dispatcher, 1)

	if err := poller.PollOnce(ctx); err == nil || !strings.Contains(err.Error(), "inactive state fetch failed") {
		t.Fatalf("terminal poll once error = %v, want inactive fetch error", err)
	}
	waitForContextCanceled(t, dispatcher.contextAt(0))
	waitForNoRunningOrRetrying(t, ctx, orch, "issue-1")
}

func TestPollOnceDoesNotCancelRunningIssueStillInActiveTrackerListing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}}
	dispatcher := &cancellationDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPollerWithReconciliation(trackerClient, orch, ReconciliationConfig{
		ActiveStates:      []string{"In Progress"},
		TerminalStates:    []string{"Cancelled"},
		InactiveStates:    []string{"Backlog"},
		WorkerExitTimeout: time.Second,
	})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll once: %v", err)
	}
	waitForCancellationDispatcherCount(t, dispatcher, 1)

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("active-listing poll once: %v", err)
	}
	select {
	case <-dispatcher.contextAt(0).Done():
		t.Fatalf("running issue context was canceled while still in active tracker listing")
	default:
	}
}

func TestPollOnceDoesNotCancelRunningIssueMissingFromPartialTrackerListing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}}
	dispatcher := &cancellationDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPollerWithReconciliation(trackerClient, orch, ReconciliationConfig{
		ActiveStates:      []string{"In Progress"},
		TerminalStates:    []string{"Cancelled"},
		InactiveStates:    []string{"Backlog"},
		WorkerExitTimeout: time.Second,
	})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll once: %v", err)
	}
	waitForCancellationDispatcherCount(t, dispatcher, 1)

	trackerClient.setIssues(nil)
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("partial-listing poll once: %v", err)
	}
	select {
	case <-dispatcher.contextAt(0).Done():
		t.Fatalf("running issue context was canceled after missing from partial tracker listing")
	default:
	}
}

func TestPollOnceReturnsErrorWhenCanceledWorkerDoesNotExitBeforeTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}}
	dispatcher := &stuckCancellationDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPollerWithReconciliation(trackerClient, orch, ReconciliationConfig{
		ActiveStates:      []string{"In Progress"},
		TerminalStates:    []string{"Cancelled"},
		WorkerExitTimeout: time.Millisecond,
	})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll once: %v", err)
	}
	waitForStuckCancellationDispatcherCount(t, dispatcher, 1)

	trackerClient.setIssues([]tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "Cancelled"}})
	if err := poller.PollOnce(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel poll once error = %v, want context deadline exceeded", err)
	}
	waitForContextCanceled(t, dispatcher.contextAt(0))
}

func TestPollOnceDispatchesActiveCandidatesWhenCanceledWorkerDoesNotExitBeforeTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}}
	dispatcher := &stuckCancellationDispatcher{}
	orch := New(NewOrchestratorState(30000, 2), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPollerWithReconciliation(trackerClient, orch, ReconciliationConfig{
		ActiveStates:      []string{"In Progress"},
		TerminalStates:    []string{"Cancelled"},
		WorkerExitTimeout: time.Millisecond,
	})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll once: %v", err)
	}
	waitForStuckCancellationDispatcherCount(t, dispatcher, 1)

	trackerClient.setIssues([]tracker.Issue{
		{ID: "issue-1", Identifier: "LIN-1", State: "Cancelled"},
		{ID: "issue-2", Identifier: "LIN-2", State: "In Progress"},
	})
	if err := poller.PollOnce(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel poll once error = %v, want context deadline exceeded", err)
	}
	waitForContextCanceled(t, dispatcher.contextAt(0))
	waitForStuckCancellationDispatcherCount(t, dispatcher, 2)
}

func TestPollOnceDispatchesTrackerCandidatesThroughRuntimeStateWithoutQueue(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "AI Ready"}}}
	releaseCh := make(chan struct{})
	dispatcher := &recordingDispatcher{releaseCh: releaseCh}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
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
	close(releaseCh)
}

func TestPollOnceRedispatchesIssueAfterPriorRunCompleted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "Rework"}}}
	dispatcher := &recordingDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
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

func TestPollOnceFailsFastAfterBuildTaskFailureWithoutRetryLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "AI Ready", UpdatedAt: "2026-05-17T00:00:00Z"}}}
	dispatcher := &erroringTaskDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPoller(trackerClient, orch)
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll once: %v", err)
	}
	waitForNoRunningOrRetrying(t, ctx, orch, "issue-1")

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("second poll once: %v", err)
	}
	waitForNoRunningOrRetrying(t, ctx, orch, "issue-1")
	if got := dispatcher.count(); got != 1 {
		t.Fatalf("deterministic build failure dispatched %d times, want 1", got)
	}
}

func TestPollOnceReleasesNonRetryableFailureAfterTrackerStateChanges(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "AI Ready", UpdatedAt: "2026-05-17T00:00:00Z"}}}
	dispatcher := &erroringTaskDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPoller(trackerClient, orch)
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll once: %v", err)
	}
	waitForNoRunningOrRetrying(t, ctx, orch, "issue-1")

	trackerClient.issues = []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "Rework", UpdatedAt: "2026-05-17T00:05:00Z"}}
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll once after tracker state changed: %v", err)
	}
	waitForNoRunningOrRetrying(t, ctx, orch, "issue-1")
	if got := dispatcher.count(); got != 2 {
		t.Fatalf("changed tracker issue dispatched %d times, want retry after state change", got)
	}
}

func TestPollOnceDoesNotExceedMaxConcurrentAgents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueTracker{issues: []tracker.Issue{
		{ID: "issue-1", Identifier: "LIN-1", State: "AI Ready"},
		{ID: "issue-2", Identifier: "LIN-2", State: "AI Ready"},
		{ID: "issue-3", Identifier: "LIN-3", State: "AI Ready"},
	}}
	dispatcher := &blockingDispatcher{}
	orch := New(NewOrchestratorState(30000, 2), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPoller(trackerClient, orch)
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll once: %v", err)
	}
	waitForBlockingDispatcherCount(t, dispatcher, 2)

	if got := dispatcher.count(); got != 2 {
		t.Fatalf("dispatcher issues = %d, want max_concurrent_agents limit 2", got)
	}
}

func TestPollOnceDispatchesOverflowIssueAfterCapacityFrees(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueTracker{issues: []tracker.Issue{
		{ID: "issue-1", Identifier: "LIN-1", State: "AI Ready"},
		{ID: "issue-2", Identifier: "LIN-2", State: "AI Ready"},
	}}
	firstRunBlocked := make(chan struct{})
	dispatcher := &recordingDispatcher{releaseCh: firstRunBlocked}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPoller(trackerClient, orch)
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("first poll once: %v", err)
	}
	waitForDispatcherCount(t, dispatcher, 1)
	if got := dispatcher.issueAt(0).ID; got != "issue-1" {
		t.Fatalf("first dispatched issue ID = %q, want issue-1", got)
	}

	close(firstRunBlocked)
	waitForCompleted(t, ctx, orch, "issue-1")

	secondRunBlocked := make(chan struct{})
	dispatcher.mu.Lock()
	dispatcher.releaseCh = secondRunBlocked
	dispatcher.mu.Unlock()
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("second poll once after capacity freed: %v", err)
	}
	waitForDispatcherCount(t, dispatcher, 2)
	if got := dispatcher.issueAt(1).ID; got != "issue-2" {
		t.Fatalf("second dispatched issue ID = %q, want overflow issue-2", got)
	}
	close(secondRunBlocked)
}

func TestPollOnceDropsOverflowIssueThatIsNoLongerActive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueTracker{issues: []tracker.Issue{
		{ID: "issue-1", Identifier: "LIN-1", State: "AI Ready"},
		{ID: "issue-2", Identifier: "LIN-2", State: "AI Ready"},
	}}
	dispatcher := &recordingDispatcher{releaseCh: make(chan struct{})}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPoller(trackerClient, orch)
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("first poll once: %v", err)
	}
	waitForDispatcherCount(t, dispatcher, 1)

	close(dispatcher.releaseCh)
	waitForCompleted(t, ctx, orch, "issue-1")
	dispatcher.releaseCh = nil
	trackerClient.issues = []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "AI Ready"}}

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("second poll once after issue-2 left active states: %v", err)
	}
	waitForDispatcherCount(t, dispatcher, 2)
	if got := dispatcher.issueAt(1).ID; got != "issue-1" {
		t.Fatalf("second dispatched issue ID = %q, want fresh active issue-1", got)
	}
}

func waitForCancellationDispatcherCount(t *testing.T, dispatcher *cancellationDispatcher, want int) {
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

func waitForStuckCancellationDispatcherCount(t *testing.T, dispatcher *stuckCancellationDispatcher, want int) {
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

func waitForContextCanceled(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatalf("context was not canceled")
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

func waitForBlockingDispatcherCount(t *testing.T, dispatcher *blockingDispatcher, want int) {
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

func waitForNoRunningOrRetrying(t *testing.T, ctx context.Context, orch *Orchestrator, id IssueID) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		view, err := orch.Snapshot(ctx)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		if !hasRunningOrRetrying(view, id) {
			return
		}
		time.Sleep(time.Millisecond)
	}

	view, err := orch.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	t.Fatalf("issue %s still running or retrying after deterministic build failure: running=%v retrying=%v", id, view.Running, view.Retrying)
}

func hasRunningOrRetrying(view StateView, id IssueID) bool {
	for _, running := range view.Running {
		if running.IssueID == id {
			return true
		}
	}
	for _, retry := range view.Retrying {
		if retry.IssueID == id {
			return true
		}
	}
	return false
}
