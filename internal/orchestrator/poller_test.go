package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type fakeIssueTracker struct {
	issues                []tracker.Issue
	err                   error
	calls                 int
	preserveMissingFields bool
}

func (f *fakeIssueTracker) ListActiveIssues(_ context.Context) ([]tracker.Issue, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if f.preserveMissingFields {
		return f.issues, nil
	}
	return defaultTrackerIssueTitles(f.issues), nil
}

func defaultTrackerIssueTitles(issues []tracker.Issue) []tracker.Issue {
	out := make([]tracker.Issue, len(issues))
	copy(out, issues)
	for i := range out {
		if out[i].Title == "" {
			out[i].Title = out[i].Identifier
		}
		if out[i].CreatedAt.IsZero() {
			out[i].CreatedAt = mustTime("2026-05-15T00:00:00Z")
		}
	}
	return out
}

type recordingDispatcher struct {
	mu        sync.Mutex
	issues    []tracker.Issue
	attempts  []*int
	releaseCh chan struct{}
}

func (d *recordingDispatcher) Spawn(_ context.Context, issue tracker.Issue, attempt *int) <-chan WorkerResult {
	d.mu.Lock()
	d.issues = append(d.issues, issue)
	d.attempts = append(d.attempts, attempt)
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

func (d *recordingDispatcher) issueIDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.issues))
	for i, issue := range d.issues {
		out[i] = issue.ID
	}
	return out
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
	return defaultTrackerIssueTitles(out), nil
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
		return defaultTrackerIssueTitles(f.issues[call]), nil
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

func TestPollOnceCancelsRunningIssueWhenActiveIssueNoLongerMatchesRoute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", Title: "API work", State: "In Progress", ProjectSlug: "api-platform"}}}
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
	poller.routing = &workflow.Config{Services: []workflow.ServiceConfig{
		{Name: "api", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "api-platform"}},
	}}
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll once: %v", err)
	}
	waitForCancellationDispatcherCount(t, dispatcher, 1)

	trackerClient.setIssues([]tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", Title: "API work", State: "In Progress", ProjectSlug: "mobile-app"}})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("unrouted poll once: %v", err)
	}
	waitForContextCanceled(t, dispatcher.contextAt(0))
	waitForNoRunningOrRetrying(t, ctx, orch, "issue-1")
}

func TestPollOnceReconcilesRoutedIssuesWhenAnotherIssueHasAmbiguousRoute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", Title: "API work", State: "In Progress", ProjectSlug: "api-platform"}}}
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
	poller.routing = &workflow.Config{Services: []workflow.ServiceConfig{
		{Name: "api", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "api-platform"}},
		{Name: "docs-a", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "docs-platform"}},
		{Name: "docs-b", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "docs-platform"}},
	}}
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll once: %v", err)
	}
	waitForCancellationDispatcherCount(t, dispatcher, 1)

	trackerClient.setIssues([]tracker.Issue{
		{ID: "issue-1", Identifier: "LIN-1", Title: "API work", State: "In Progress", ProjectSlug: "mobile-app"},
		{ID: "issue-2", Identifier: "LIN-2", Title: "Ambiguous docs", State: "In Progress", ProjectSlug: "docs-platform"},
	})
	err := poller.PollOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous poll error = %v, want ambiguity reported", err)
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

func TestPollOnceRefreshesRunningIssueStateWithoutRouting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := tracker.Issue{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{first}}
	dispatcher := &cancellationDispatcher{}
	st := NewOrchestratorState(30000, 10)
	st.MaxConcurrentAgentsByState = map[string]int{"rework": 1}
	orch := New(st, Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPollerWithReconciliation(trackerClient, orch, ReconciliationConfig{
		ActiveStates:      []string{"In Progress", "Rework"},
		TerminalStates:    []string{"Cancelled"},
		InactiveStates:    []string{"Backlog"},
		WorkerExitTimeout: time.Second,
	})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll: %v", err)
	}
	waitForCancellationDispatcherCount(t, dispatcher, 1)

	moved := first
	moved.State = "Rework"
	trackerClient.setIssues([]tracker.Issue{moved})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("refresh poll: %v", err)
	}

	select {
	case <-dispatcher.contextAt(0).Done():
		t.Fatalf("running issue was canceled by non-routed refresh path")
	default:
	}

	other := tracker.Issue{ID: "issue-2", Identifier: "LIN-2", State: "Rework"}
	if err := orch.RequestDispatch(ctx, other, nil); !errors.Is(err, ErrCapacityFull) {
		t.Fatalf("dispatch second Rework after refresh err = %v, want ErrCapacityFull", err)
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

func TestPollOnceFiltersTodoIssuesBlockedByNonTerminalBlockers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueTracker{issues: []tracker.Issue{
		{ID: "blocked-todo", Identifier: "LIN-1", State: "Todo", BlockedBy: []tracker.BlockerRef{{ID: "open-blocker", Identifier: "LIN-0", State: "In Progress"}}},
		{ID: "unblocked-todo", Identifier: "LIN-2", State: "Todo", BlockedBy: []tracker.BlockerRef{{ID: "done-blocker", Identifier: "LIN-9", State: "Done"}}},
	}}
	dispatcher := &recordingDispatcher{releaseCh: make(chan struct{})}
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
	waitForDispatcherCount(t, dispatcher, 1)
	if got := dispatcher.issueAt(0).ID; got != "unblocked-todo" {
		t.Fatalf("dispatched issue ID = %q, want only Todo issue whose blockers are terminal", got)
	}
	close(dispatcher.releaseCh)
}

func TestPollOnceSkipsMalformedTrackerCandidates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueTracker{preserveMissingFields: true, issues: []tracker.Issue{
		{ID: "missing-identifier", Title: "Missing identifier", State: "AI Ready"},
		{ID: "missing-title", Identifier: "LIN-2", State: "AI Ready"},
		{ID: "missing-state", Identifier: "LIN-3", Title: "Missing state"},
		{ID: "valid", Identifier: "LIN-4", Title: "Ready work", State: "AI Ready", CreatedAt: mustTime("2026-05-15T00:00:00Z")},
	}}
	dispatcher := &recordingDispatcher{releaseCh: make(chan struct{})}
	orch := New(NewOrchestratorState(30000, 4), Deps{
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
	if got := dispatcher.issueAt(0).ID; got != "valid" {
		t.Fatalf("dispatched issue ID = %q, want only candidate with required identity fields", got)
	}
	close(dispatcher.releaseCh)
}

func TestPollOnceDispatchesMissingCreatedAtCandidateAfterDatedCandidates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueTracker{preserveMissingFields: true, issues: []tracker.Issue{
		{ID: "missing-created-at", Identifier: "LIN-2", Title: "Missing created timestamp", State: "AI Ready", Priority: 1},
		{ID: "dated", Identifier: "LIN-1", Title: "Dated", State: "AI Ready", Priority: 1, CreatedAt: mustTime("2026-05-15T00:00:00Z")},
	}}
	dispatcher := &recordingDispatcher{releaseCh: make(chan struct{})}
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
	waitForDispatcherCount(t, dispatcher, 2)
	if got, want := dispatcher.issueIDs(), []string{"dated", "missing-created-at"}; got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("dispatch order = %v, want missing created_at sorted last after dated candidates %v", got, want)
	}
	close(dispatcher.releaseCh)
}

func TestPollOnceIgnoresBlockersForNonTodoStates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueTracker{issues: []tracker.Issue{
		{ID: "ready-blocked", Identifier: "LIN-1", State: "AI Ready", BlockedBy: []tracker.BlockerRef{{ID: "open-blocker", Identifier: "LIN-0", State: "In Progress"}}},
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
		t.Fatalf("poll once: %v", err)
	}
	waitForDispatcherCount(t, dispatcher, 1)
	if got := dispatcher.issueAt(0).ID; got != "ready-blocked" {
		t.Fatalf("dispatched issue ID = %q, want non-Todo issue dispatched despite blockers", got)
	}
	close(dispatcher.releaseCh)
}

func TestPollOnceTreatsDefaultSpecTerminalBlockersAsUnblocked(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueTracker{issues: []tracker.Issue{
		{ID: "todo-closed", Identifier: "LIN-1", Title: "Closed blocker", State: "Todo", BlockedBy: []tracker.BlockerRef{{ID: "blocker-1", Identifier: "LIN-0", State: "Closed"}}},
		{ID: "todo-duplicate", Identifier: "LIN-2", Title: "Duplicate blocker", State: "Todo", BlockedBy: []tracker.BlockerRef{{ID: "blocker-2", Identifier: "LIN-3", State: "Duplicate"}}},
	}}
	dispatcher := &recordingDispatcher{releaseCh: make(chan struct{})}
	orch := New(NewOrchestratorState(30000, 2), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPoller(trackerClient, orch)
	poller.reconcile.TerminalStates = []string{"Done", "Canceled"}
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll once: %v", err)
	}
	waitForDispatcherCount(t, dispatcher, 2)
	if got, want := dispatcher.issueIDs(), []string{"todo-closed", "todo-duplicate"}; got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("dispatched issue IDs = %v, want %v", got, want)
	}
	close(dispatcher.releaseCh)
}

func TestSelectRoutedCandidatesMatchesLinearProjectTeamLabelAndCustomField(t *testing.T) {
	cfg := workflow.Config{Services: []workflow.ServiceConfig{
		{
			Name: "api",
			Repo: workflow.RepoConfig{Owner: "acme", Name: "api", CloneURL: "git@example.com:acme/api.git"},
			Tracker: workflow.ServiceTrackerRouteConfig{
				ProjectSlug:  "api-platform",
				TeamKey:      "ENG",
				Labels:       []string{"backend"},
				CustomFields: map[string]string{"Runtime": "go"},
			},
		},
	}}
	issues := []tracker.Issue{
		{ID: "issue-1", Identifier: "LIN-1", Title: "API work", State: "AI Ready", ProjectSlug: "api-platform", TeamKey: "ENG", Labels: []string{"backend", "customer"}, CustomFields: map[string]string{"Runtime": "go"}},
	}

	got, err := selectRoutedCandidates(issues, cfg)
	if err != nil {
		t.Fatalf("selectRoutedCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("routed candidates = %d, want 1", len(got))
	}
	if got[0].ServiceName != "api" {
		t.Fatalf("routed candidate service = %q, want api", got[0].ServiceName)
	}
}

func TestSelectRoutedCandidatesSkipsUnmatchedLinearIssueWithoutFallbackRepo(t *testing.T) {
	cfg := workflow.Config{Services: []workflow.ServiceConfig{
		{Name: "api", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "api-platform"}},
	}}
	issues := []tracker.Issue{
		{ID: "issue-1", Identifier: "LIN-1", Title: "Mobile work", State: "AI Ready", ProjectSlug: "mobile-app"},
	}

	got, err := selectRoutedCandidates(issues, cfg)
	if err != nil {
		t.Fatalf("selectRoutedCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("routed candidates = %#v, want unmatched issue skipped", got)
	}
}

func TestSelectRoutedCandidatesKeepsUnmatchedLinearIssueForFallbackRepo(t *testing.T) {
	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{ProjectSlug: "core-platform"},
		Repo:    workflow.RepoConfig{Owner: "acme", Name: "fallback", CloneURL: "git@example.com:acme/fallback.git"},
		Services: []workflow.ServiceConfig{
			{Name: "api", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "api-platform"}},
		},
	}
	issues := []tracker.Issue{
		{ID: "issue-1", Identifier: "LIN-1", Title: "Core work", State: "AI Ready", ProjectSlug: "core-platform"},
	}

	got, err := selectRoutedCandidates(issues, cfg)
	if err != nil {
		t.Fatalf("selectRoutedCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("routed candidates = %d, want unmatched issue kept for fallback repo", len(got))
	}
	if got[0].ServiceName != "" {
		t.Fatalf("fallback candidate service = %q, want empty service", got[0].ServiceName)
	}
}

func TestSelectRoutedCandidatesSkipsUnmatchedServiceProjectIssueWithFallbackRepo(t *testing.T) {
	cfg := workflow.Config{
		Repo: workflow.RepoConfig{Owner: "acme", Name: "fallback", CloneURL: "git@example.com:acme/fallback.git"},
		Services: []workflow.ServiceConfig{
			{Name: "api", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "api-platform", Labels: []string{"api"}}},
		},
	}
	issues := []tracker.Issue{
		{ID: "issue-1", Identifier: "LIN-1", Title: "Unmatched service work", State: "AI Ready", ProjectSlug: "api-platform", Labels: []string{"docs"}},
	}

	got, err := selectRoutedCandidates(issues, cfg)
	if err != nil {
		t.Fatalf("selectRoutedCandidates: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("routed candidates = %#v, want unmatched service-project issue skipped instead of fallback repo", got)
	}
}

func TestSelectRoutedCandidatesRejectsAmbiguousLinearRoute(t *testing.T) {
	cfg := workflow.Config{Services: []workflow.ServiceConfig{
		{Name: "api-a", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "api-platform"}},
		{Name: "api-b", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "api-platform"}},
	}}
	issues := []tracker.Issue{
		{ID: "issue-1", Identifier: "LIN-1", Title: "Ambiguous work", State: "AI Ready", ProjectSlug: "api-platform"},
	}

	_, err := selectRoutedCandidates(issues, cfg)
	if err == nil {
		t.Fatal("selectRoutedCandidates returned nil error, want ambiguous route error")
	}
	if !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "issue-1") || !strings.Contains(err.Error(), "api-a") || !strings.Contains(err.Error(), "api-b") {
		t.Fatalf("ambiguous route error = %q, want issue and matching services", err)
	}
}

func TestSelectRoutedCandidatesUsesTopLevelLinearProjectAsServiceDefault(t *testing.T) {
	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{ProjectSlug: "api-platform"},
		Services: []workflow.ServiceConfig{
			{Name: "api", Tracker: workflow.ServiceTrackerRouteConfig{TeamKey: "ENG"}},
			{Name: "mobile", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "mobile-app", TeamKey: "ENG"}},
		},
	}
	issues := []tracker.Issue{
		{ID: "api-issue", Identifier: "LIN-1", Title: "API work", State: "AI Ready", ProjectSlug: "api-platform", TeamKey: "ENG"},
		{ID: "mobile-issue", Identifier: "LIN-2", Title: "Mobile work", State: "AI Ready", ProjectSlug: "mobile-app", TeamKey: "ENG"},
	}

	got, err := selectRoutedCandidates(issues, cfg)
	if err != nil {
		t.Fatalf("selectRoutedCandidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("routed candidates = %d, want 2", len(got))
	}
	if got[0].ServiceName != "api" {
		t.Fatalf("api issue service = %q, want api", got[0].ServiceName)
	}
	if got[1].ServiceName != "mobile" {
		t.Fatalf("mobile issue service = %q, want mobile", got[1].ServiceName)
	}
}

func TestSelectRoutedCandidatesIgnoresServiceWithoutExplicitRouteBeforeProjectDefault(t *testing.T) {
	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{ProjectSlug: "api-platform"},
		Services: []workflow.ServiceConfig{
			{Name: "catchall", Tracker: workflow.ServiceTrackerRouteConfig{}},
			{Name: "api", Tracker: workflow.ServiceTrackerRouteConfig{TeamKey: "ENG"}},
		},
	}
	issues := []tracker.Issue{
		{ID: "api-issue", Identifier: "LIN-1", Title: "API work", State: "AI Ready", ProjectSlug: "api-platform", TeamKey: "ENG"},
	}

	got, err := selectRoutedCandidates(issues, cfg)
	if err != nil {
		t.Fatalf("selectRoutedCandidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("routed candidates = %d, want only explicitly routed service", len(got))
	}
	if got[0].ServiceName != "api" {
		t.Fatalf("routed candidate service = %q, want api", got[0].ServiceName)
	}
}

func TestWorkerTaskDispatcherThreadsRunAttemptIntoTask(t *testing.T) {
	attempt := 3
	dispatcher := WorkerTaskDispatcher{
		BuildTask: func(issue tracker.Issue) (task.Task, error) {
			return task.Task{ID: "issue-1", Attempts: 99}, nil
		},
		Config:  worker.Config{Workflow: &workflow.Workflow{}},
		Emitter: nil,
	}

	tk, recordedTaskID, err := dispatcher.buildTaskWithAttempt(tracker.Issue{ID: "issue-1"}, &attempt)
	if err != nil {
		t.Fatalf("buildTaskWithAttempt: %v", err)
	}
	if recordedTaskID != "issue-1" {
		t.Fatalf("recordedTaskID = %q, want issue-1", recordedTaskID)
	}
	want := attempt + 1
	if tk.Attempts != want {
		t.Fatalf("task attempts = %d, want run attempt %d from retry counter %d", tk.Attempts, want, attempt)
	}
}

func TestWorkerTaskDispatcherThreadsFirstRetryAsSecondRunAttempt(t *testing.T) {
	attempt := 1
	dispatcher := WorkerTaskDispatcher{
		BuildTask: func(issue tracker.Issue) (task.Task, error) {
			return task.Task{ID: "issue-1"}, nil
		},
		Config: worker.Config{Workflow: &workflow.Workflow{}},
	}

	tk, _, err := dispatcher.buildTaskWithAttempt(tracker.Issue{ID: "issue-1"}, &attempt)
	if err != nil {
		t.Fatalf("buildTaskWithAttempt: %v", err)
	}
	if tk.Attempts != 2 {
		t.Fatalf("task attempts = %d, want first retry to render as run attempt 2", tk.Attempts)
	}
}

func TestWorkerTaskDispatcherLeavesAttemptWhenRetryAttemptNil(t *testing.T) {
	dispatcher := WorkerTaskDispatcher{
		BuildTask: func(issue tracker.Issue) (task.Task, error) {
			return task.Task{ID: "issue-1", Attempts: 0}, nil
		},
		Config: worker.Config{Workflow: &workflow.Workflow{}},
	}

	tk, _, err := dispatcher.buildTaskWithAttempt(tracker.Issue{ID: "issue-1"}, nil)
	if err != nil {
		t.Fatalf("buildTaskWithAttempt: %v", err)
	}
	if tk.Attempts != 0 {
		t.Fatalf("task attempts = %d, want original first-run attempts", tk.Attempts)
	}
}

func TestWorkerTaskDispatcherCopiesRetryAttemptBeforeRun(t *testing.T) {
	attempt := 1
	dispatcher := WorkerTaskDispatcher{
		BuildTask: func(issue tracker.Issue) (task.Task, error) {
			return task.Task{ID: "issue-1"}, nil
		},
		Config: worker.Config{Workflow: &workflow.Workflow{}},
	}

	tk, _, err := dispatcher.buildTaskWithAttempt(tracker.Issue{ID: "issue-1"}, &attempt)
	if err != nil {
		t.Fatalf("buildTaskWithAttempt: %v", err)
	}
	attempt = 99
	if tk.Attempts != 2 {
		t.Fatalf("task attempts changed after caller mutation: got %d, want copied run attempt 2", tk.Attempts)
	}
}

func TestTaskFromIssueUsesServiceRepoDefaultBranch(t *testing.T) {
	cfg := workflow.Config{
		Repo: workflow.RepoConfig{Owner: "fallback", Name: "fallback", CloneURL: "git@example.com:fallback/fallback.git", DefaultBranch: "main"},
		Services: []workflow.ServiceConfig{
			{Name: "api", Repo: workflow.RepoConfig{Owner: "acme", Name: "api", CloneURL: "git@example.com:acme/api.git", DefaultBranch: "main"}},
		},
	}
	issue := tracker.Issue{ID: "issue-1", Identifier: "LIN-1", Title: "API work", ServiceName: "api"}

	got, err := TaskFromIssue(issue, cfg)
	if err != nil {
		t.Fatalf("TaskFromIssue: %v", err)
	}
	if got.RepoOwner != "acme" || got.RepoName != "api" || got.CloneURL != "git@example.com:acme/api.git" {
		t.Fatalf("task repo = %s/%s %s, want acme/api service repo", got.RepoOwner, got.RepoName, got.CloneURL)
	}
	if got.BaseBranch != "main" {
		t.Fatalf("task base branch = %q, want main", got.BaseBranch)
	}
}

func TestTaskFromIssueNamespacesServiceRoutedSourceEventID(t *testing.T) {
	cfg := workflow.Config{
		Repo: workflow.RepoConfig{Owner: "fallback", Name: "fallback", CloneURL: "git@example.com:fallback/fallback.git", DefaultBranch: "main"},
		Services: []workflow.ServiceConfig{
			{Name: "api", Repo: workflow.RepoConfig{Owner: "acme", Name: "mono", CloneURL: "git@example.com:acme/mono.git", DefaultBranch: "main"}},
			{Name: "web", Repo: workflow.RepoConfig{Owner: "acme", Name: "mono", CloneURL: "git@example.com:acme/mono.git", DefaultBranch: "main"}},
		},
	}
	apiIssue := tracker.Issue{ID: "issue-1", Identifier: "LIN-1", Title: "API work", ServiceName: "api"}
	webIssue := apiIssue
	webIssue.ServiceName = "web"

	apiTask, err := TaskFromIssue(apiIssue, cfg)
	if err != nil {
		t.Fatalf("TaskFromIssue(api): %v", err)
	}
	webTask, err := TaskFromIssue(webIssue, cfg)
	if err != nil {
		t.Fatalf("TaskFromIssue(web): %v", err)
	}

	if apiTask.SourceEventID != "issue-1|service|api" {
		t.Fatalf("api source event id = %q, want service-scoped issue id", apiTask.SourceEventID)
	}
	if webTask.SourceEventID != "issue-1|service|web" {
		t.Fatalf("web source event id = %q, want service-scoped issue id", webTask.SourceEventID)
	}
	if apiTask.SourceEventID == webTask.SourceEventID {
		t.Fatalf("service-routed tasks share source event id %q", apiTask.SourceEventID)
	}
}

func TestTaskFromIssueRejectsUnknownService(t *testing.T) {
	cfg := workflow.Config{
		Repo: workflow.RepoConfig{Owner: "fallback", Name: "fallback", CloneURL: "git@example.com:fallback/fallback.git", DefaultBranch: "main"},
	}
	issue := tracker.Issue{ID: "issue-1", Identifier: "LIN-1", Title: "API work", ServiceName: "missing"}

	_, err := TaskFromIssue(issue, cfg)
	if err == nil {
		t.Fatal("TaskFromIssue returned nil error, want unknown service error")
	}
	if !strings.Contains(err.Error(), `service "missing" not found`) {
		t.Fatalf("TaskFromIssue error = %q, want unknown service", err)
	}
}

func TestPollOnceSortsCandidatesByTrackerPriorityCreatedAtIdentifier(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueTracker{issues: []tracker.Issue{
		{ID: "unprioritized", Identifier: "LIN-0", State: "AI Ready", Priority: 0, CreatedAt: mustTime("2026-05-14T00:00:00Z")},
		{ID: "later-high", Identifier: "LIN-9", State: "AI Ready", Priority: 2, CreatedAt: mustTime("2026-05-17T00:00:00Z")},
		{ID: "middle-tie-b", Identifier: "LIN-B", State: "AI Ready", Priority: 1, CreatedAt: mustTime("2026-05-16T00:00:00Z")},
		{ID: "oldest", Identifier: "LIN-1", State: "AI Ready", Priority: 1, CreatedAt: mustTime("2026-05-15T00:00:00Z")},
		{ID: "offset-newer", Identifier: "LIN-O", State: "AI Ready", Priority: 1, CreatedAt: mustTime("2026-05-15T01:00:00+02:00")},
		{ID: "middle-tie-a", Identifier: "LIN-A", State: "AI Ready", Priority: 1, CreatedAt: mustTime("2026-05-16T00:00:00Z")},
	}}
	dispatcher := &recordingDispatcher{releaseCh: make(chan struct{})}
	orch := New(NewOrchestratorState(30000, 6), Deps{
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
	waitForDispatcherCount(t, dispatcher, 6)
	got := dispatcher.issueIDs()
	want := []string{"offset-newer", "oldest", "middle-tie-a", "middle-tie-b", "later-high", "unprioritized"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dispatch order = %v, want %v", got, want)
		}
	}
	close(dispatcher.releaseCh)
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

	dispatcher := &recordingDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Millisecond}},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}
	poller := NewPoller(&fakeIssueTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "ISSUE-1", Title: "Issue 1", State: "AI Ready"}}}, orch)

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll once: %v", err)
	}
	waitForDispatcherCount(t, dispatcher, 1)
	waitForCompleted(t, ctx, orch, "issue-1")

	waitForRetryDue(t, ctx, orch, "issue-1")
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("rework poll once: %v", err)
	}
	waitForDispatcherCount(t, dispatcher, 2)
	dispatcher.mu.Lock()
	secondAttempt := dispatcher.attempts[1]
	dispatcher.mu.Unlock()
	if secondAttempt != nil {
		t.Fatalf("continuation dispatch carried retry attempt = %d, want nil", *secondAttempt)
	}
}

func TestPollOnceKeepsDueContinuationRetryMissingFromActiveListing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dispatcher := &recordingDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Millisecond}},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}
	issue := tracker.Issue{ID: "issue-missing", Identifier: "ISSUE-MISSING", Title: "Issue missing from capped active listing", State: "AI Ready"}
	poller := NewPoller(&fakeIssueTracker{issues: []tracker.Issue{issue}}, orch)

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll once: %v", err)
	}
	waitForDispatcherCount(t, dispatcher, 1)
	waitForCompleted(t, ctx, orch, IssueID(issue.ID))
	waitForRetryDue(t, ctx, orch, IssueID(issue.ID))

	poller.tracker = &fakeIssueTracker{}
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("missing poll once: %v", err)
	}
	view, err := orch.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Retrying) != 1 || view.Retrying[0].IssueID != IssueID(issue.ID) {
		t.Fatalf("retrying after issue missing from active listing = %+v, want continuation retained", view.Retrying)
	}
	if got := dispatcher.count(); got != 1 {
		t.Fatalf("missing continuation poll spawned %d workers, want no new dispatch", got-1)
	}
}

func TestRunPollLoopWakesWhenContinuationRetryTimerFires(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dispatcher := &recordingDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  &sequenceScheduler{delays: []time.Duration{time.Millisecond}},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}
	issue := tracker.Issue{ID: "issue-1", Identifier: "ISSUE-1", Title: "Issue 1", State: "AI Ready"}
	poller := NewPoller(&fakeIssueTracker{issues: []tracker.Issue{issue}}, orch)

	done := make(chan error, 1)
	go func() {
		done <- RunPollLoop(ctx, poller, time.Hour)
	}()
	waitForDispatcherCount(t, dispatcher, 2)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunPollLoop error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunPollLoop did not exit after context cancel")
	}
}

func TestPollOnceFailsFastAfterBuildTaskFailureWithoutRetryLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "AI Ready", UpdatedAt: mustTime("2026-05-17T00:00:00Z")}}}
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

	trackerClient := &fakeIssueTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "AI Ready", UpdatedAt: mustTime("2026-05-17T00:00:00Z")}}}
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

	trackerClient.issues = []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "Rework", UpdatedAt: mustTime("2026-05-17T00:05:00Z")}}
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
		Scheduler:  RetryScheduler{MaxBackoff: time.Millisecond},
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
	waitForRetryDue(t, ctx, orch, "issue-1")

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

func waitForRetryDue(t *testing.T, ctx context.Context, orch *Orchestrator, id IssueID) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		view, err := orch.Snapshot(ctx)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		for _, retry := range view.Retrying {
			if retry.IssueID == id && !retry.DueAt.After(time.Now()) {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}

	view, err := orch.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	t.Fatalf("issue %s retry did not become due: retrying=%v", id, view.Retrying)
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

func mustTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		panic(err)
	}
	return parsed
}
