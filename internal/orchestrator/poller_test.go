package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
	"github.com/xrf9268-hue/aiops-platform/internal/workspace"
)

type fakeIssueTracker struct {
	issues                []tracker.Issue
	err                   error
	partialSuccess        bool
	calls                 int
	preserveMissingFields bool
}

func (f *fakeIssueTracker) ListActiveIssues(_ context.Context) ([]tracker.Issue, error) {
	f.calls++
	if f.err != nil && !f.partialSuccess {
		return nil, f.err
	}
	issues := f.issues
	if !f.preserveMissingFields {
		issues = defaultTrackerIssueTitles(f.issues)
	}
	if f.err != nil {
		return issues, f.err
	}
	return issues, nil
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
	mu            sync.Mutex
	issues        []tracker.Issue
	err           error
	fetchIDErr    error
	fetchRefCalls [][]tracker.IssueRef
	fetchIDStates map[string]string
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

func (f *fakeIssueStateTracker) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]string, error) {
	return f.FetchIssueStatesByRefs(ctx, tracker.IssueRefsFromIDs(issueIDs))
}

func (f *fakeIssueStateTracker) FetchIssueStatesByRefs(_ context.Context, refs []tracker.IssueRef) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchRefCalls = append(f.fetchRefCalls, append([]tracker.IssueRef(nil), refs...))
	if f.err != nil {
		return nil, f.err
	}
	wanted := map[string]struct{}{}
	for _, ref := range refs {
		wanted[ref.ID] = struct{}{}
	}
	out := make(map[string]string, len(refs))
	if f.fetchIDStates != nil {
		for id, state := range f.fetchIDStates {
			if _, ok := wanted[id]; ok {
				out[id] = state
			}
		}
		return out, f.fetchIDErr
	}
	for _, issue := range f.issues {
		if _, ok := wanted[issue.ID]; ok {
			out[issue.ID] = issue.State
		}
	}
	return out, nil
}

func (f *fakeIssueStateTracker) fetchIssueStatesByRefsCalls() [][]tracker.IssueRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]tracker.IssueRef, len(f.fetchRefCalls))
	for i, call := range f.fetchRefCalls {
		out[i] = append([]tracker.IssueRef(nil), call...)
	}
	return out
}

func (f *fakeIssueStateTracker) setFetchIDStates(states map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchIDStates = states
}

func (f *fakeIssueStateTracker) setFetchIDErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchIDErr = err
}

func (f *fakeIssueStateTracker) resetFetchIssueStatesByIDsCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchRefCalls = nil
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

// stuckCancellationDispatcher behaves like cancellationDispatcher but its
// spawned workers never exit — Spawn returns a channel that is never sent on —
// so a reconcile-cancel must wait out the worker-exit timeout. It inherits
// count/contextAt from the embedded cancellationDispatcher.
type stuckCancellationDispatcher struct {
	cancellationDispatcher
}

func (d *stuckCancellationDispatcher) Spawn(ctx context.Context, issue tracker.Issue, _ *int) <-chan WorkerResult {
	d.mu.Lock()
	d.issues = append(d.issues, issue)
	d.contexts = append(d.contexts, ctx)
	d.mu.Unlock()
	return make(chan WorkerResult)
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
	waitForCancellationDispatcherCount(t, dispatcher)

	trackerClient.setIssues([]tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "Cancelled"}})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("cancelled poll once: %v", err)
	}
	waitForContextCanceled(t, dispatcher.contextAt(0))
	waitForNoRunningOrRetrying(t, ctx, orch)
}

func TestPollOnceUsesNarrowStateRefreshForRunningIssue(t *testing.T) {
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
	waitForCancellationDispatcherCount(t, dispatcher)

	trackerClient.resetFetchIssueStatesByIDsCalls()
	trackerClient.setFetchIDStates(map[string]string{"issue-1": "Cancelled"})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("narrow-refresh poll once: %v", err)
	}

	calls := trackerClient.fetchIssueStatesByRefsCalls()
	if len(calls) != 1 {
		t.Fatalf("FetchIssueStatesByRefs calls = %d, want 1", len(calls))
	}
	if got := calls[0]; len(got) != 1 || got[0].ID != "issue-1" || got[0].Identifier != "LIN-1" {
		t.Fatalf("FetchIssueStatesByRefs refs = %#v, want issue-1 with identifier LIN-1", got)
	}
	waitForContextCanceled(t, dispatcher.contextAt(0))
	waitForNoRunningOrRetrying(t, ctx, orch)
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
	waitForCancellationDispatcherCount(t, dispatcher)

	trackerClient.setIssues([]tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "Backlog"}})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("backlog poll once: %v", err)
	}
	waitForContextCanceled(t, dispatcher.contextAt(0))
	waitForNoRunningOrRetrying(t, ctx, orch)
}

func TestPollOnceCancelsRunningIssueWhenNarrowRefreshLeavesActiveStates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", Title: "API work", State: "In Progress"}}}
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
	waitForCancellationDispatcherCount(t, dispatcher)

	trackerClient.setIssues(nil)
	trackerClient.resetFetchIssueStatesByIDsCalls()
	trackerClient.setFetchIDStates(map[string]string{"issue-1": "Cancelled"})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("refresh poll once: %v", err)
	}
	if got := trackerClient.fetchIssueStatesByRefsCalls(); len(got) != 1 {
		t.Fatalf("FetchIssueStatesByRefs calls = %d, want 1", len(got))
	}
	waitForContextCanceled(t, dispatcher.contextAt(0))
	waitForNoRunningOrRetrying(t, ctx, orch)
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
	waitForCancellationDispatcherCount(t, dispatcher)

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
	waitForCancellationDispatcherCount(t, dispatcher)

	if err := poller.PollOnce(ctx); err == nil || !strings.Contains(err.Error(), "inactive state fetch failed") {
		t.Fatalf("terminal poll once error = %v, want inactive fetch error", err)
	}
	waitForContextCanceled(t, dispatcher.contextAt(0))
	waitForNoRunningOrRetrying(t, ctx, orch)
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
	waitForCancellationDispatcherCount(t, dispatcher)

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
	waitForCancellationDispatcherCount(t, dispatcher)

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

func TestPollOnceAppliesPartialRunningIDRefreshWhenRefreshReturnsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}}
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
	waitForCancellationDispatcherCount(t, dispatcher)
	trackerClient.resetFetchIssueStatesByIDsCalls()
	trackerClient.setFetchIDStates(map[string]string{"issue-1": "Rework"})
	trackerClient.setFetchIDErr(errors.New("secondary tracker state refresh failed"))

	if err := poller.PollOnce(ctx); err == nil || !strings.Contains(err.Error(), "secondary tracker state refresh failed") {
		t.Fatalf("refresh poll error = %v, want partial refresh error", err)
	}
	calls := trackerClient.fetchIssueStatesByRefsCalls()
	if len(calls) != 1 {
		t.Fatalf("FetchIssueStatesByRefs calls = %d, want 1", len(calls))
	}
	if got := calls[0]; len(got) != 1 || got[0].ID != "issue-1" {
		t.Fatalf("FetchIssueStatesByRefs refs = %#v, want [issue-1]", got)
	}

	select {
	case <-dispatcher.contextAt(0).Done():
		t.Fatalf("running issue was canceled by partial refresh error")
	default:
	}
	other := tracker.Issue{ID: "issue-2", Identifier: "LIN-2", State: "Rework"}
	if err := orch.RequestDispatch(ctx, other, nil); !errors.Is(err, ErrCapacityFull) {
		t.Fatalf("dispatch second Rework after partial refresh err = %v, want ErrCapacityFull", err)
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
	waitForCancellationDispatcherCount(t, dispatcher)

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

// TestPollOnceContinuesReconciliationWhenStalledRunCleanupTimesOut pins the
// #285 regression: a Part A (SPEC §8.5) stall reconciliation that times out
// waiting for a wedged worker to exit must not block Part B's tracker-state
// reconciliation for unrelated running issues in the same poll tick. Without
// the fix, one stuck worker would freeze global reconciliation until it
// eventually exited.
func TestPollOnceContinuesReconciliationWhenStalledRunCleanupTimesOut(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{
		{ID: "wedged", Identifier: "LIN-1", State: "In Progress"},
		{ID: "movable", Identifier: "LIN-2", State: "In Progress"},
	}}
	dispatcher := &stuckCancellationDispatcher{}
	orch := New(NewOrchestratorState(30000, 4), Deps{
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
		StallTimeoutMs:    50,
	})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll once: %v", err)
	}
	waitForStuckCancellationDispatcherCount(t, dispatcher, 2)

	// Backdate the wedged run so Part A tries to cancel it; the stuck
	// dispatcher never closes its done channel, so the cancel-wait times
	// out with context.DeadlineExceeded.
	orch.WithStateForTest(func(st *OrchestratorState) {
		st.Running["wedged"].LastEventAt = time.Now().Add(-10 * time.Second)
	})

	// Move the unrelated "movable" run to Cancelled. Part B must still
	// reconcile it (cancel its worker context) even after Part A's wait
	// timed out on the wedged run.
	trackerClient.setIssues([]tracker.Issue{
		{ID: "wedged", Identifier: "LIN-1", State: "In Progress"},
		{ID: "movable", Identifier: "LIN-2", State: "Cancelled"},
	})

	if err := poller.PollOnce(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stall poll once error = %v, want context deadline exceeded surfaced as non-fatal", err)
	}

	waitForContextCanceled(t, dispatcher.contextAt(0))
	waitForContextCanceled(t, dispatcher.contextAt(1))
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

	// NewPoller without a reconciliation config leaves reconcile.TerminalStates
	// empty; filterEligibleCandidates falls back to workflow.DefaultConfig's
	// SPEC §5.3.1 5-state set, so a Done blocker is still treated as terminal.
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
	// Use workflow.DefaultConfig().Tracker.TerminalStates so the test actually
	// exercises the SPEC §5.3.1 5-state default ("Done", "Canceled",
	// "Cancelled", "Closed", "Duplicate"). Previously this was hard-coded to
	// ["Done", "Canceled"] and relied on filterEligibleCandidates's now-removed
	// hardcoded overlay (#232) to backfill the remaining three terminal states.
	poller.reconcile.TerminalStates = workflow.DefaultConfig().Tracker.TerminalStates
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll once: %v", err)
	}
	waitForDispatcherCount(t, dispatcher, 2)
	if got, want := dispatcher.issueIDs(), []string{"todo-closed", "todo-duplicate"}; got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("dispatched issue IDs = %v, want %v", got, want)
	}
	close(dispatcher.releaseCh)
}

// TestPollOnceTodoBlockerHonorsOperatorConfiguredTerminalStates is the
// regression for #232 — when an operator explicitly subsets terminal_states
// (e.g. ["Done"] only), the Todo blocker rule MUST observe that subset, not
// any hardcoded English fallback. Closed and Duplicate blockers are open per
// the operator config, so both Todo issues stay blocked.
func TestPollOnceTodoBlockerHonorsOperatorConfiguredTerminalStates(t *testing.T) {
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
	// Operator subsets terminal_states to just ["Done"]. Closed and Duplicate
	// must NOT be treated as terminal — both Todo issues stay blocked.
	poller.reconcile.TerminalStates = []string{"Done"}
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll once: %v", err)
	}
	// Give the orchestrator a beat to evaluate. No dispatches expected.
	time.Sleep(50 * time.Millisecond)
	if got := dispatcher.count(); got != 0 {
		t.Fatalf("dispatcher count = %d, want 0 (Closed/Duplicate blockers must be treated as open under operator config without them)", got)
	}
	close(dispatcher.releaseCh)
}

// TestFilterEligibleCandidatesExplicitEmptyTerminalStatesBlocksAll confirms
// that an explicitly empty terminal_states slice from
// NewPollerWithReconciliation reaches filterEligibleCandidates verbatim — it
// is NOT silently replaced by the DefaultConfig 5-state set. SPEC §5.3.1
// default semantics: defaults apply on omission, not on explicit override.
func TestFilterEligibleCandidatesExplicitEmptyTerminalStatesBlocksAll(t *testing.T) {
	issues := []tracker.Issue{
		{ID: "todo-done", Identifier: "LIN-1", Title: "Done blocker", State: "Todo", BlockedBy: []tracker.BlockerRef{{ID: "blk", Identifier: "LIN-0", State: "Done"}}},
	}
	out := filterEligibleCandidates(issues, []string{})
	if len(out) != 0 {
		t.Fatalf("filterEligibleCandidates with explicit [] = %d issues, want 0 (no states are terminal → all blockers open → Todo blocked); got=%#v", len(out), out)
	}
}

// TestFilterEligibleCandidatesUsesOnlyConfiguredTerminalSet is the direct
// unit test for filterEligibleCandidates: when operator's terminal_states is
// ["Released"], a Todo issue blocked by a "Released" blocker passes the
// filter (blocker is terminal per config), while a Todo issue blocked by a
// "Closed" blocker is filtered (Closed is no longer hardcoded as terminal).
func TestFilterEligibleCandidatesUsesOnlyConfiguredTerminalSet(t *testing.T) {
	issues := []tracker.Issue{
		{ID: "todo-closed", Identifier: "LIN-1", Title: "Closed blocker", State: "Todo", BlockedBy: []tracker.BlockerRef{{ID: "blk", Identifier: "LIN-0", State: "Closed"}}},
		{ID: "todo-released", Identifier: "LIN-2", Title: "Released blocker", State: "Todo", BlockedBy: []tracker.BlockerRef{{ID: "blk", Identifier: "LIN-9", State: "Released"}}},
	}
	out := filterEligibleCandidates(issues, []string{"Released"})
	if len(out) != 1 {
		t.Fatalf("filterEligibleCandidates = %d issues, want 1; got=%#v", len(out), out)
	}
	if out[0].ID != "todo-released" {
		t.Fatalf("passed issue = %q, want todo-released (its blocker is in operator-configured terminal state)", out[0].ID)
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

	tk, err := dispatcher.buildTaskWithAttempt(tracker.Issue{ID: "issue-1"}, &attempt)
	if err != nil {
		t.Fatalf("buildTaskWithAttempt: %v", err)
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

	tk, err := dispatcher.buildTaskWithAttempt(tracker.Issue{ID: "issue-1"}, &attempt)
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

	tk, err := dispatcher.buildTaskWithAttempt(tracker.Issue{ID: "issue-1"}, nil)
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

	tk, err := dispatcher.buildTaskWithAttempt(tracker.Issue{ID: "issue-1"}, &attempt)
	if err != nil {
		t.Fatalf("buildTaskWithAttempt: %v", err)
	}
	attempt = 99
	if tk.Attempts != 2 {
		t.Fatalf("task attempts changed after caller mutation: got %d, want copied run attempt 2", tk.Attempts)
	}
}

func TestWorkerTaskDispatcherReportsWorkspacePathBeforeRun(t *testing.T) {
	root := t.TempDir()
	tk := task.Task{
		ID:            "issue-183",
		RepoOwner:     "acme",
		RepoName:      "demo",
		SourceType:    "github_issue",
		SourceEventID: "183",
	}
	reported := make(chan string, 1)
	dispatcher := WorkerTaskDispatcher{
		BuildTask: func(issue tracker.Issue) (task.Task, error) {
			return tk, nil
		},
		Config: worker.Config{WorkspaceRoot: root, Workflow: &workflow.Workflow{}},
		WorkspacePrepared: func(_ context.Context, _ tracker.Issue, _ task.Task, path string) {
			reported <- path
		},
	}

	result := dispatcher.Spawn(context.Background(), tracker.Issue{ID: "issue-183"}, nil)

	select {
	case got := <-reported:
		if want := workspace.New(root).PathFor(tk); got != want {
			t.Fatalf("workspace path = %q, want %q", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("workspace path was not reported before worker run")
	}
	<-result
}

// TestIssueRenderVarsCoversSpec4_1_1FieldSet pins SPEC §4.1.1 + §12.1: the
// prebuilt issue snapshot the worker hands to the prompt template must
// expose every normalized field. Empty labels/blocked_by must materialize as
// empty slices (not nil) so strict-mode templates iterating over them do
// not surface a render error.
func TestIssueRenderVarsCoversSpec4_1_1FieldSet(t *testing.T) {
	created := mustTime("2026-05-20T10:00:00Z")
	updated := mustTime("2026-05-21T12:00:00Z")
	got := IssueRenderVars(tracker.Issue{
		ID:          "lin-456",
		Identifier:  "LIN-456",
		Title:       "integration",
		Description: "desc",
		Priority:    2,
		State:       "In Progress",
		BranchName:  "feat/auth-cleanup",
		URL:         "https://linear.app/x/issue/LIN-456",
		Labels:      []string{"priority:p2", "area:auth"},
		BlockedBy: []tracker.BlockerRef{
			{ID: "lin-200", Identifier: "LIN-200", State: "Todo"},
		},
		CreatedAt: created,
		UpdatedAt: updated,
	})
	for _, k := range []string{
		"id", "identifier", "title", "description", "priority", "state",
		"branch_name", "url", "labels", "blocked_by", "created_at", "updated_at",
	} {
		if _, ok := got[k]; !ok {
			t.Errorf("IssueRenderVars missing key %q in %#v", k, got)
		}
	}
	labels, _ := got["labels"].([]string)
	if len(labels) != 2 || labels[0] != "priority:p2" {
		t.Errorf("labels = %#v, want copy preserving order", got["labels"])
	}
	blockedBy, _ := got["blocked_by"].([]map[string]any)
	if len(blockedBy) != 1 || blockedBy[0]["identifier"] != "LIN-200" || blockedBy[0]["state"] != "Todo" {
		t.Errorf("blocked_by = %#v, want one normalized blocker", got["blocked_by"])
	}
}

// TestIssueRenderVarsEmptySlicesMaterializeNonNil pins the empty-collection
// invariant: a tracker.Issue with no labels / no blockers must render as
// empty slices, never nil, so strict-mode templates iterating with
// `{% for ... %}` do not crash. nil interface{} would also be rendered as
// `<nil>` by fmt.Sprint, which surprises operators.
func TestIssueRenderVarsEmptySlicesMaterializeNonNil(t *testing.T) {
	got := IssueRenderVars(tracker.Issue{ID: "x", Identifier: "X-1"})
	labels, ok := got["labels"].([]string)
	if !ok || labels == nil || len(labels) != 0 {
		t.Errorf("labels = %#v, want non-nil empty []string", got["labels"])
	}
	blockedBy, ok := got["blocked_by"].([]map[string]any)
	if !ok || blockedBy == nil || len(blockedBy) != 0 {
		t.Errorf("blocked_by = %#v, want non-nil empty []map[string]any", got["blocked_by"])
	}
}

// TestTaskFromIssuePopulatesIssueRender pins the wiring: TaskFromIssue must
// stamp the SPEC §4.1.1 snapshot onto Task.IssueRender so the worker's
// runtask can populate the prompt template's `issue` variable.
func TestTaskFromIssuePopulatesIssueRender(t *testing.T) {
	cfg := workflow.Config{
		Repo: workflow.RepoConfig{Owner: "acme", Name: "demo", CloneURL: "git@example.com:acme/demo.git", DefaultBranch: "main"},
	}
	issue := tracker.Issue{ID: "lin-1", Identifier: "LIN-1", Title: "x", State: "In Progress", Labels: []string{"a"}}
	got, err := TaskFromIssue(issue, cfg)
	if err != nil {
		t.Fatalf("TaskFromIssue: %v", err)
	}
	if got.IssueRender == nil {
		t.Fatal("TaskFromIssue IssueRender = nil, want SPEC §4.1.1 snapshot")
	}
	if got.IssueRender["state"] != "In Progress" {
		t.Errorf("IssueRender[state] = %#v, want In Progress", got.IssueRender["state"])
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
	waitForNoRunningOrRetrying(t, ctx, orch)

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("second poll once: %v", err)
	}
	waitForNoRunningOrRetrying(t, ctx, orch)
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
	waitForNoRunningOrRetrying(t, ctx, orch)

	trackerClient.issues = []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "Rework", UpdatedAt: mustTime("2026-05-17T00:05:00Z")}}
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll once after tracker state changed: %v", err)
	}
	waitForNoRunningOrRetrying(t, ctx, orch)
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

func waitForCancellationDispatcherCount(t *testing.T, dispatcher *cancellationDispatcher) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if dispatcher.count() == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("dispatcher issues = %d, want 1", dispatcher.count())
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

func waitForNoRunningOrRetrying(t *testing.T, ctx context.Context, orch *Orchestrator) {
	id := IssueID("issue-1")
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

// TestPollOnceDispatchesPartialIssuesWhenTrackerReturnsCategorizedError covers
// the multi-tracker partial-success case: ListActiveIssues may return
// (issues, errors.Join(...)) where one tracker fails (a typed
// tracker.ErrorCategory error) while another succeeds. PollOnce must keep
// dispatching the successful issues; only fail-out early when no issues were
// fetched at all.
func TestPollOnceDispatchesPartialIssuesWhenTrackerReturnsCategorizedError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	categorizedErr := errors.Join(
		errors.New("benign other"),
		tracker.NewError(tracker.CategoryLinearGraphQLErrors, "linear partial outage", nil),
	)
	trackerClient := &fakeIssueTracker{
		issues: []tracker.Issue{
			{ID: "issue-1", Identifier: "GH-1", State: "AI Ready"},
			{ID: "issue-2", Identifier: "GH-2", State: "AI Ready"},
		},
		err:            categorizedErr,
		partialSuccess: true,
	}
	dispatcher := &recordingDispatcher{}
	orch := New(NewOrchestratorState(30000, 4), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}
	poller := NewPoller(trackerClient, orch)

	pollErr := poller.PollOnce(ctx)
	if pollErr == nil {
		t.Fatal("PollOnce returned nil, want partial-success error reported")
	}
	if cat, ok := tracker.ErrorCategory(pollErr); !ok || cat != tracker.CategoryLinearGraphQLErrors {
		t.Fatalf("PollOnce error category = %q ok=%v, want %q true", cat, ok, tracker.CategoryLinearGraphQLErrors)
	}
	waitForDispatcherCount(t, dispatcher, 2)
	ids := dispatcher.issueIDs()
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, []string{"issue-1", "issue-2"}) {
		t.Fatalf("dispatched issue IDs = %v, want partial issues [issue-1 issue-2]", ids)
	}
}
