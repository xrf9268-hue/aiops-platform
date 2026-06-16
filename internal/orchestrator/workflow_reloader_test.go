package orchestrator

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// newRuntimePollerForTest builds a RuntimePoller through the production
// NewRuntimePollerWithTrackerFactory constructor (the deleted NewRuntimePoller
// wrapper used to wrap a single lister in this same closure). lister is served
// for every workflow snapshot so reload-driven tracker selection still resolves
// to the test fake.
func newRuntimePollerForTest(t *testing.T, lister IssueStateLister, orch *Orchestrator, runtime *WorkflowRuntime) *RuntimePoller {
	t.Helper()
	// These reload tests dispatch through the actor's own fakeDispatcher; the
	// poller only needs a *RuntimeDispatcher to receive SetIssueStateRefresher,
	// so a standalone instance is sufficient. The actor's-dispatcher-is-the-
	// poller's-dispatcher invariant is pinned by
	// TestRuntimePollerWiresRefresherOntoProvidedDispatcher.
	dispatcher, err := NewRuntimeDispatcher(runtime, worker.Config{}, nil)
	if err != nil {
		t.Fatalf("new runtime dispatcher: %v", err)
	}
	poller, err := NewRuntimePollerWithTrackerFactory(func(workflow.Config) (IssueStateLister, error) {
		return lister, nil
	}, orch, runtime, dispatcher)
	if err != nil {
		t.Fatalf("new runtime poller: %v", err)
	}
	return poller
}

type recordingWorkflowReloadEmitter struct {
	mu     sync.Mutex
	events []recordedWorkflowReloadEvent
}

type recordedWorkflowReloadEvent struct {
	kind    string
	message string
	payload any
}

func (e *recordingWorkflowReloadEmitter) AddEvent(_ context.Context, _ string, kind, msg string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, recordedWorkflowReloadEvent{kind: kind, message: msg})
	return nil
}

func (e *recordingWorkflowReloadEmitter) AddEventWithPayload(_ context.Context, _ string, kind, msg string, payload any) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, recordedWorkflowReloadEvent{kind: kind, message: msg, payload: payload})
	return nil
}

func (e *recordingWorkflowReloadEmitter) count(kind string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	count := 0
	for _, ev := range e.events {
		if ev.kind == kind {
			count++
		}
	}
	return count
}

func TestWorkflowRuntimeReloadSuccessAtomicallySwapsConfigAndEmitsEvent(t *testing.T) {
	path := writeWorkflowForReloadTest(t, "linear", 30000)
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	emitter := &recordingWorkflowReloadEmitter{}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{
		Initial:        initial,
		Path:           path,
		Source:         workflow.SourceFile,
		ReloadInterval: time.Millisecond,
		Emitter:        emitter,
		EventTaskID:    "workflow-runtime",
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	writeWorkflowForReloadTestAt(t, path, "linear", 45000, "Rework")
	if err := runtime.ReloadOnce(context.Background()); err != nil {
		t.Fatalf("reload once: %v", err)
	}

	snap := runtime.Current()
	if got := snap.Workflow.Config.Polling.IntervalMs; got != 45000 {
		t.Fatalf("poll interval after reload = %d, want 45000", got)
	}
	if got := snap.Workflow.Config.Tracker.ActiveStates; len(got) != 1 || got[0] != "Rework" {
		t.Fatalf("active states after reload = %#v, want [Rework]", got)
	}
	if got := emitter.count(task.EventWorkflowReloaded); got != 1 {
		t.Fatalf("workflow_reload event count = %d, want 1", got)
	}
}

func TestWorkflowRuntimeReloadUnchangedFileDoesNotEmitRepeatedSuccessEvents(t *testing.T) {
	path := writeWorkflowForReloadTest(t, "linear", 30000)
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	emitter := &recordingWorkflowReloadEmitter{}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{
		Initial:        initial,
		Path:           path,
		Source:         workflow.SourceFile,
		ReloadInterval: time.Millisecond,
		Emitter:        emitter,
		EventTaskID:    "workflow-runtime",
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	if err := runtime.ReloadOnce(context.Background()); err != nil {
		t.Fatalf("first unchanged reload: %v", err)
	}
	if err := runtime.ReloadOnce(context.Background()); err != nil {
		t.Fatalf("second unchanged reload: %v", err)
	}

	if got := emitter.count(task.EventWorkflowReloaded); got != 0 {
		t.Fatalf("workflow_reload event count for unchanged reloads = %d, want 0", got)
	}
}

func TestRuntimePollerUsesReloadedTrackerStatesFromSameWorkflowPath(t *testing.T) {
	ctx := context.Background()
	path := writeWorkflowForReloadTest(t, "linear", 30000)
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{
		{ID: "issue-ready", Identifier: "ISSUE-1", Title: "ready", State: "Todo"},
		{ID: "issue-rework", Identifier: "ISSUE-2", Title: "rework", State: "Rework"},
	}}
	dispatcher := &fakeDispatcher{}
	orch, cancel := startActor(t, Deps{Dispatcher: dispatcher, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()
	poller := newRuntimePollerForTest(t, trackerClient, orch, runtime)

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	waitFor(t, func() bool { return dispatcher.count() == 1 }, time.Second)
	if got := dispatcher.issueAt(0).ID; got != "issue-ready" {
		t.Fatalf("first dispatched issue = %q, want issue-ready", got)
	}

	dispatcher.finishAt(0, WorkerResult{Elapsed: time.Millisecond})
	writeWorkflowForReloadTestAt(t, path, "linear", 30000, "Rework")
	if err := runtime.ReloadOnce(ctx); err != nil {
		t.Fatalf("reload workflow: %v", err)
	}
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	waitFor(t, func() bool { return dispatcher.count() == 2 }, time.Second)
	if got := dispatcher.issueAt(1).ID; got != "issue-rework" {
		t.Fatalf("second dispatched issue after same-path state reload = %q, want issue-rework", got)
	}
}

func TestRuntimePollerAppliesReloadedMaxConcurrentAgentsToDispatchCapacity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	path := writeWorkflowForReloadTest(t, "linear", 30000)
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	initial.Config.Agent.MaxConcurrentAgents = 1
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{
		{ID: "issue-1", Identifier: "ISSUE-1", Title: "one", State: "Todo"},
		{ID: "issue-2", Identifier: "ISSUE-2", Title: "two", State: "Todo"},
	}}
	dispatcher := &blockingDispatcher{}
	orch := New(NewOrchestratorState(30000, initial.Config.Agent.MaxConcurrentAgents), Deps{Dispatcher: dispatcher, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}
	poller := newRuntimePollerForTest(t, trackerClient, orch, runtime)

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	waitForBlockingDispatcherCount(t, dispatcher, 1)

	writeWorkflowForReloadTestAt(t, path, "linear", 30000, "Todo", withReloadTestMaxConcurrentAgents(2))
	if err := runtime.ReloadOnce(ctx); err != nil {
		t.Fatalf("reload workflow: %v", err)
	}
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	waitForBlockingDispatcherCount(t, dispatcher, 2)
}

func TestRuntimePollerAppliesReloadedMaxConcurrentAgentsByStateToDispatchCapacity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	path := writeWorkflowForReloadTest(t, "linear", 30000, withReloadTestMaxConcurrentAgents(10), withReloadTestMaxConcurrentAgentsByState(map[string]int{"Todo": 1}))
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{
		{ID: "issue-1", Identifier: "ISSUE-1", Title: "one", State: "Todo"},
		{ID: "issue-2", Identifier: "ISSUE-2", Title: "two", State: "Todo"},
	}}
	dispatcher := &blockingDispatcher{}
	st := NewOrchestratorState(30000, initial.Config.Agent.MaxConcurrentAgents)
	st.MaxConcurrentAgentsByState = initial.Config.Agent.MaxConcurrentAgentsByState
	orch := New(st, Deps{Dispatcher: dispatcher, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}
	poller := newRuntimePollerForTest(t, trackerClient, orch, runtime)

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	waitForBlockingDispatcherCount(t, dispatcher, 1)

	writeWorkflowForReloadTestAt(t, path, "linear", 30000, "Todo", withReloadTestMaxConcurrentAgents(10), withReloadTestMaxConcurrentAgentsByState(map[string]int{"todo": 2}))
	if err := runtime.ReloadOnce(ctx); err != nil {
		t.Fatalf("reload workflow: %v", err)
	}
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	waitForBlockingDispatcherCount(t, dispatcher, 2)
}

func TestRuntimePollerAppliesReloadedMaxRetryBackoffToFailureRetries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	path := writeWorkflowForReloadTest(t, "linear", 30000, withReloadTestMaxRetryBackoffMs(1000))
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "ISSUE-1", Title: "one", State: "Todo"}}}
	dispatcher := &fakeDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{Dispatcher: dispatcher, Scheduler: RetryScheduler{MaxBackoff: time.Second}})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}
	poller := newRuntimePollerForTest(t, trackerClient, orch, runtime)

	writeWorkflowForReloadTestAt(t, path, "linear", 30000, "Todo", withReloadTestMaxRetryBackoffMs(50))
	if err := runtime.ReloadOnce(ctx); err != nil {
		t.Fatalf("reload workflow: %v", err)
	}
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll after retry backoff reload: %v", err)
	}
	waitFor(t, func() bool { return dispatcher.count() == 1 }, time.Second)
	dispatcher.finishAt(0, WorkerResult{Err: errors.New("boom"), Elapsed: time.Millisecond})

	waitFor(t, func() bool {
		view, err := orch.Snapshot(ctx)
		if err != nil {
			return false
		}
		for _, retry := range view.Retrying {
			if retry.IssueID == "issue-1" && time.Until(retry.DueAt) <= 200*time.Millisecond {
				return true
			}
		}
		return false
	}, time.Second)
}

func TestRuntimePollerAppliesReloadedMaxContinuationTurnsToCleanContinuationBudget(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	path := writeWorkflowForReloadTest(t, "linear", 30000, withReloadTestMaxContinuationTurns(5))
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "ISSUE-1", Title: "one", State: "Todo"}}}
	dispatcher := &fakeDispatcher{}
	st := NewOrchestratorState(30000, 1)
	st.MaxContinuationTurns = initial.Config.Agent.MaxContinuationTurns
	orch := New(st, Deps{Dispatcher: dispatcher, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}
	poller := newRuntimePollerForTest(t, trackerClient, orch, runtime)

	writeWorkflowForReloadTestAt(t, path, "linear", 30000, "Todo", withReloadTestMaxContinuationTurns(1))
	if err := runtime.ReloadOnce(ctx); err != nil {
		t.Fatalf("reload workflow: %v", err)
	}
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll after continuation budget reload: %v", err)
	}
	waitFor(t, func() bool { return dispatcher.count() == 1 }, time.Second)
	dispatcher.finishAt(0, WorkerResult{Elapsed: time.Millisecond})

	waitFor(t, func() bool {
		view, err := orch.Snapshot(ctx)
		return err == nil && len(view.Blocked) == 1 && len(view.Retrying) == 0 &&
			view.Blocked[0].Method == "continuation_budget" &&
			strings.Contains(view.Blocked[0].Error, "max_continuation_turns=1")
	}, time.Second)
}

func TestRuntimePollerRebuildsTrackerClientAfterTrackerConfigReload(t *testing.T) {
	ctx := context.Background()
	path := writeWorkflowForReloadTest(t, "linear", 30000)
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	trackersByKind := map[string]*fakeIssueStateTracker{
		"linear": {issues: []tracker.Issue{{ID: "linear-ready", Identifier: "ISSUE-1", Title: "ready", State: "Todo"}}},
		"gitea":  {issues: []tracker.Issue{{ID: "gitea-rework", Identifier: "ISSUE-2", Title: "rework", State: "Rework"}}},
	}
	factoryCalls := 0
	dispatcher := &fakeDispatcher{}
	orch, cancel := startActor(t, Deps{Dispatcher: dispatcher, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()
	runtimeDispatcher, err := NewRuntimeDispatcher(runtime, worker.Config{}, nil)
	if err != nil {
		t.Fatalf("new runtime dispatcher: %v", err)
	}
	poller, err := NewRuntimePollerWithTrackerFactory(func(cfg workflow.Config) (IssueStateLister, error) {
		factoryCalls++
		return trackersByKind[cfg.Tracker.Kind], nil
	}, orch, runtime, runtimeDispatcher)
	if err != nil {
		t.Fatalf("new runtime poller: %v", err)
	}

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	waitFor(t, func() bool { return dispatcher.count() == 1 }, time.Second)
	if got := dispatcher.issueAt(0).ID; got != "linear-ready" {
		t.Fatalf("first dispatched issue = %q, want linear-ready", got)
	}
	// The SPEC §16.5 refresher must track the rebuilt tracker client on the
	// poller's dispatcher across reloads, not just at construction: pin it to
	// the linear client now and the gitea client after reload so a regression
	// that re-points the refresher only on first construction is caught.
	if got := runtimeDispatcher.currentRefresher(); got != IssueStateRefresher(trackersByKind["linear"]) {
		t.Fatalf("refresher after first poll = %v, want linear tracker client", got)
	}

	dispatcher.finishAt(0, WorkerResult{Elapsed: time.Millisecond})
	writeWorkflowForReloadTestAt(t, path, "gitea", 30000, "Rework")
	if err := runtime.ReloadOnce(ctx); err != nil {
		t.Fatalf("reload workflow: %v", err)
	}
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	waitFor(t, func() bool { return dispatcher.count() == 2 }, time.Second)
	if got := dispatcher.issueAt(1).ID; got != "gitea-rework" {
		t.Fatalf("second dispatched issue after tracker config reload = %q, want gitea-rework", got)
	}
	if got := runtimeDispatcher.currentRefresher(); got != IssueStateRefresher(trackersByKind["gitea"]) {
		t.Fatalf("refresher after tracker config reload = %v, want gitea tracker client; reload path must re-point the dispatcher refresher", got)
	}
	if factoryCalls < 2 {
		t.Fatalf("tracker factory calls = %d, want at least 2 after tracker config reload", factoryCalls)
	}
}

// TestRuntimePollerRetryListerWrapsEligibilityFilter pins the retry-fire
// lister chain: the orchestrator must receive an eligibleActiveIssueLister
// wrap so a Todo issue with a non-terminal blocker is filtered the same way
// the poll loop filters it (filterEligibleCandidates). Without this test a
// future refactor that dropped the eligibility wrap from the retry path
// would silently regress.
func TestRuntimePollerRetryListerWrapsEligibilityFilter(t *testing.T) {
	ctx := context.Background()
	path := writeWorkflowForReloadTest(t, "gitea", 30000)
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	initial.Config.Repo = workflow.RepoConfig{Owner: "acme", Name: "fallback", CloneURL: "git@example.com:acme/fallback.git", DefaultBranch: "main"}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	trackerClient := &fakeIssueStateTracker{}
	dispatcher := &recordingDispatcher{}
	orch, cancel := startActor(t, Deps{Dispatcher: dispatcher, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()
	poller := newRuntimePollerForTest(t, trackerClient, orch, runtime)
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll once: %v", err)
	}

	lister := orch.currentCandidateLister()
	if lister == nil {
		t.Fatal("orchestrator candidate lister not installed by RuntimePoller")
	}

	// Behavioral assertion: feed in a Todo issue blocked by a non-terminal
	// blocker and confirm the eligibility wrap drops it. Without the wrap
	// the issue would surface and the retry would dispatch.
	trackerClient.issues = []tracker.Issue{{
		ID: "gitea-blocked", Identifier: "BLOCKED-1", Title: "blocked todo", State: "Todo",
		BlockedBy: []tracker.BlockerRef{{ID: "gitea-blocker", Identifier: "BLOCKER-1", State: "In Progress"}},
	}}
	got, err := lister.ListActiveIssues(ctx)
	if err != nil {
		t.Fatalf("ListActiveIssues: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("lister surfaced Todo+non-terminal-blocker = %+v, want filterEligibleCandidates to drop it (eligibility wrap missing?)", got)
	}

	// Positive control: a clean issue passes the wrap.
	trackerClient.issues = []tracker.Issue{{ID: "gitea-ready", Identifier: "READY-1", Title: "ready", State: "Todo"}}
	got, err = lister.ListActiveIssues(ctx)
	if err != nil {
		t.Fatalf("ListActiveIssues (positive control): %v", err)
	}
	if len(got) != 1 || got[0].ID != "gitea-ready" {
		t.Fatalf("lister surfaced issues = %+v, want exactly gitea-ready", got)
	}
}

// TestRuntimePollerRetryListerThreadsRequiredLabels pins the retry-fire
// (SPEC §16.6 candidate-fetch) routability site for tracker.required_labels:
// the eligibleActiveIssueLister the orchestrator receives must carry
// snap.Reconciliation.RequiredLabels so a fired failure/quota retry whose
// issue lost a required label is refused the same way the poll loop refuses it.
// Without it the retry-fire gate silently no-ops — the production-no-op class
// the dispatch + cmd/worker fixes in this PR close. Mutation: drop
// `requiredLabels:` from runtime_poller.go's eligibleActiveIssueLister literal
// (or RequiredLabels from ReconciliationConfigFromWorkflow) and this fails.
func TestRuntimePollerRetryListerThreadsRequiredLabels(t *testing.T) {
	ctx := context.Background()
	path := writeWorkflowForReloadTest(t, "gitea", 30000)
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	initial.Config.Repo = workflow.RepoConfig{Owner: "acme", Name: "fallback", CloneURL: "git@example.com:acme/fallback.git", DefaultBranch: "main"}
	initial.Config.Tracker.RequiredLabels = []string{"aiops-ready"}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	trackerClient := &fakeIssueStateTracker{}
	dispatcher := &recordingDispatcher{}
	orch, cancel := startActor(t, Deps{Dispatcher: dispatcher, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()
	poller := newRuntimePollerForTest(t, trackerClient, orch, runtime)
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll once: %v", err)
	}

	lister := orch.currentCandidateLister()
	if lister == nil {
		t.Fatal("orchestrator candidate lister not installed by RuntimePoller")
	}

	// An active-state issue missing the required label must be dropped on the
	// retry-fire seam: a queued retry whose issue lost the label is ineligible.
	trackerClient.issues = []tracker.Issue{{
		ID: "gitea-unlabeled", Identifier: "UNLABELED-1", Title: "missing required label", State: "Todo",
	}}
	got, err := lister.ListActiveIssues(ctx)
	if err != nil {
		t.Fatalf("ListActiveIssues(unlabeled) error = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Fatalf("retry-fire lister.ListActiveIssues(active issue missing %v) = %+v; want none (required-label gate must thread to the retry-fire lister)", initial.Config.Tracker.RequiredLabels, got)
	}

	// Positive control: the same active issue carrying the required label passes.
	trackerClient.issues = []tracker.Issue{{
		ID: "gitea-labeled", Identifier: "LABELED-1", Title: "has required label", State: "Todo",
		Labels: []string{"aiops-ready"},
	}}
	got, err = lister.ListActiveIssues(ctx)
	if err != nil {
		t.Fatalf("ListActiveIssues(labeled) error = %v, want nil", err)
	}
	if len(got) != 1 || got[0].ID != "gitea-labeled" {
		t.Fatalf("retry-fire lister.ListActiveIssues(active issue with %v) = %+v; want exactly gitea-labeled", initial.Config.Tracker.RequiredLabels, got)
	}
}

func TestWorkflowRuntimeReloadFailureKeepsPreviousConfigAndEmitsFailureEvent(t *testing.T) {
	path := writeWorkflowForReloadTest(t, "linear", 30000)
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	emitter := &recordingWorkflowReloadEmitter{}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{
		Initial:        initial,
		Path:           path,
		Source:         workflow.SourceFile,
		ReloadInterval: time.Millisecond,
		Emitter:        emitter,
		EventTaskID:    "workflow-runtime",
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	writeWorkflowForReloadTestAt(t, path, "unsupported", 45000, "Rework")
	if err := runtime.ReloadOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "tracker.kind") {
		t.Fatalf("reload error = %v, want tracker.kind validation error", err)
	}

	snap := runtime.Current()
	if got := snap.Workflow.Config.Polling.IntervalMs; got != 30000 {
		t.Fatalf("poll interval after failed reload = %d, want previous 30000", got)
	}
	if got := snap.Workflow.Config.Tracker.ActiveStates; len(got) != 1 || got[0] != "Todo" {
		t.Fatalf("active states after failed reload = %#v, want previous [Todo]", got)
	}
	if got := emitter.count(task.EventWorkflowReloadFailed); got != 1 {
		t.Fatalf("workflow_reload_failed event count = %d, want 1", got)
	}
}

func TestWorkflowFileFingerprintDoesNotMarkNonMissingReadErrorsAsMissing(t *testing.T) {
	_, err := workflowFileFingerprint(t.TempDir())
	if err == nil {
		t.Fatalf("workflowFileFingerprint directory path error = nil, want read error")
	}
	if errors.Is(err, workflow.ErrMissingWorkflowFile) {
		t.Fatalf("workflowFileFingerprint directory path error = %T %[1]v, must not match ErrMissingWorkflowFile", err)
	}
	if got, ok := workflow.ErrorCategory(err); ok {
		t.Fatalf("ErrorCategory(directory fingerprint error) = %q, true; want uncategorized", got)
	}
}

func TestRunWorkflowReloadLoopPollFallbackReloadsChangedWorkflow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := writeWorkflowForReloadTest(t, "linear", 30000)
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	sleeper := &reloadLoopTestSleeper{
		afterFirst: func() {
			writeWorkflowForReloadTestAt(t, path, "linear", 42000, "Rework")
		},
	}

	err = RunWorkflowReloadLoop(ctx, runtime, WorkflowReloadLoopOptions{Sleep: sleeper.sleep, StopAfterChecks: 2})
	if err != nil {
		t.Fatalf("run reload loop: %v", err)
	}

	snap := runtime.Current()
	if got := snap.Workflow.Config.Polling.IntervalMs; got != 42000 {
		t.Fatalf("poll interval after polling reload loop = %d, want 42000", got)
	}
	if got := snap.Workflow.Config.Tracker.ActiveStates; len(got) != 1 || got[0] != "Rework" {
		t.Fatalf("active states after polling reload loop = %#v, want [Rework]", got)
	}
}

func TestRunPollLoopWithRuntimeUsesReloadedPollingCadence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := writeWorkflowForReloadTest(t, "linear", 25)
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	poller := &countingPollOnce{afterFirst: func() {
		writeWorkflowForReloadTestAt(t, path, "linear", 75, "Todo")
		_ = runtime.ReloadOnce(context.Background())
	}}
	sleeper := &recordingPollSleeper{}

	err = RunPollLoopWithRuntime(ctx, poller, runtime, PollLoopRuntimeOptions{Sleep: sleeper.sleep, StopAfterPolls: 2})
	if err != nil {
		t.Fatalf("run poll loop: %v", err)
	}
	if got, want := sleeper.durations, []time.Duration{25 * time.Millisecond, 75 * time.Millisecond}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("sleep durations = %v, want %v", got, want)
	}
}

func TestRunPollLoopWithRuntimePollerHonorsInjectedSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := writeWorkflowForReloadTest(t, "linear", 60000)
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	disp := &recordingDispatcher{releaseCh: make(chan struct{})}
	orch, orchCancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Second}})
	defer orchCancel()
	poller := newRuntimePollerForTest(t, &fakeIssueStateTracker{}, orch, runtime)
	sleeper := &recordingPollSleeper{}

	start := time.Now()
	err = RunPollLoopWithRuntime(ctx, poller, runtime, PollLoopRuntimeOptions{Sleep: sleeper.sleep, StopAfterPolls: 1})
	if err != nil {
		t.Fatalf("run poll loop: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("runtime poll loop ignored injected sleep and waited %v", elapsed)
	}
	if got, want := sleeper.durations, []time.Duration{60 * time.Second}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("sleep durations = %v, want %v", got, want)
	}
}

type countingPollOnce struct {
	calls      int
	afterFirst func()
}

func (p *countingPollOnce) PollOnce(_ context.Context) error {
	p.calls++
	if p.calls == 1 && p.afterFirst != nil {
		p.afterFirst()
	}
	return nil
}

type recordingPollSleeper struct {
	durations []time.Duration
}

func (s *recordingPollSleeper) sleep(_ context.Context, d time.Duration) error {
	s.durations = append(s.durations, d)
	return nil
}

type reloadLoopTestSleeper struct {
	calls      int
	afterFirst func()
}

func (s *reloadLoopTestSleeper) sleep(_ context.Context, _ time.Duration) error {
	s.calls++
	if s.calls == 1 && s.afterFirst != nil {
		s.afterFirst()
	}
	return nil
}

func writeWorkflowForReloadTest(t *testing.T, trackerKind string, pollIntervalMs int, opts ...reloadWorkflowTestOption) string {
	t.Helper()
	path := t.TempDir() + "/WORKFLOW.md"
	writeWorkflowForReloadTestAt(t, path, trackerKind, pollIntervalMs, "Todo", opts...)
	return path
}

func writeWorkflowForReloadTestAt(t *testing.T, path, trackerKind string, pollIntervalMs int, activeState string, opts ...reloadWorkflowTestOption) {
	t.Helper()
	cfg := reloadWorkflowTestConfig{maxConcurrentAgents: 100, maxRetryBackoffMs: 300000}
	for _, opt := range opts {
		opt(&cfg)
	}
	content := "---\n" +
		"repo:\n" +
		"  owner: xrf9268-hue\n" +
		"  name: aiops-platform\n" +
		"  clone_url: https://github.com/xrf9268-hue/aiops-platform.git\n" +
		"tracker:\n" +
		"  kind: " + trackerKind + "\n" +
		"  api_key: lin_dummy_for_test\n" +
		reloadTestLinearProjectSlugYAML(trackerKind) +
		"  active_states: [\"" + activeState + "\"]\n" +
		"  terminal_states: [\"Done\"]\n" +
		"polling:\n" +
		"  interval_ms: " + itoaForReloadTest(pollIntervalMs) + "\n" +
		"agent:\n" +
		"  default: mock\n" +
		"  max_concurrent_agents: " + itoaForReloadTest(cfg.maxConcurrentAgents) + "\n" +
		"  max_retry_backoff_ms: " + itoaForReloadTest(cfg.maxRetryBackoffMs) + "\n" +
		reloadTestMaxContinuationTurnsYAML(cfg.maxContinuationTurns) +
		reloadTestMaxConcurrentAgentsByStateYAML(cfg.maxConcurrentAgentsByState) +
		"---\n" +
		"Prompt body\n"
	if err := osWriteFileForReloadTest(path, []byte(content)); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
}

func reloadTestLinearProjectSlugYAML(trackerKind string) string {
	if trackerKind != "linear" {
		return ""
	}
	return "  project_slug: platform\n"
}

type reloadWorkflowTestConfig struct {
	maxConcurrentAgents        int
	maxRetryBackoffMs          int
	maxContinuationTurns       int
	maxConcurrentAgentsByState map[string]int
}

type reloadWorkflowTestOption func(*reloadWorkflowTestConfig)

func withReloadTestMaxConcurrentAgents(n int) reloadWorkflowTestOption {
	return func(cfg *reloadWorkflowTestConfig) {
		cfg.maxConcurrentAgents = n
	}
}

func withReloadTestMaxRetryBackoffMs(n int) reloadWorkflowTestOption {
	return func(cfg *reloadWorkflowTestConfig) {
		cfg.maxRetryBackoffMs = n
	}
}

func withReloadTestMaxContinuationTurns(n int) reloadWorkflowTestOption {
	return func(cfg *reloadWorkflowTestConfig) {
		cfg.maxContinuationTurns = n
	}
}

func withReloadTestMaxConcurrentAgentsByState(caps map[string]int) reloadWorkflowTestOption {
	return func(cfg *reloadWorkflowTestConfig) {
		cfg.maxConcurrentAgentsByState = caps
	}
}

func reloadTestMaxConcurrentAgentsByStateYAML(caps map[string]int) string {
	if len(caps) == 0 {
		return ""
	}
	out := "  max_concurrent_agents_by_state:\n"
	for state, cap := range caps {
		out += "    " + state + ": " + itoaForReloadTest(cap) + "\n"
	}
	return out
}

func reloadTestMaxContinuationTurnsYAML(n int) string {
	if n <= 0 {
		return ""
	}
	return "  max_continuation_turns: " + itoaForReloadTest(n) + "\n"
}

var osWriteFileForReloadTest = func(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

func itoaForReloadTest(v int) string {
	return strconv.Itoa(v)
}

func TestWorkflowRuntimeReloadOnceDedupesIdenticalFailures(t *testing.T) {
	emitter := &recordingWorkflowReloadEmitter{}
	path := writeWorkflowForReloadTest(t, "linear", 30000)
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{
		Initial:     initial,
		Path:        path,
		Source:      workflow.SourceFile,
		Emitter:     emitter,
		EventTaskID: "workflow-runtime",
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	// Make the file invalid (unsupported tracker.kind). First ReloadOnce must
	// emit one EventWorkflowReloadFailed; the next five must dedupe.
	writeWorkflowForReloadTestAt(t, path, "unsupported", 30000, "Rework")
	if err := runtime.ReloadOnce(context.Background()); err == nil {
		t.Fatal("first reload of invalid file must return an error")
	}
	if got := emitter.count(task.EventWorkflowReloadFailed); got != 1 {
		t.Fatalf("after first failure, reload_failed count = %d, want 1", got)
	}
	for i := 0; i < 5; i++ {
		if err := runtime.ReloadOnce(context.Background()); err == nil {
			t.Fatalf("reload %d of identical invalid file must still return an error", i+2)
		}
	}
	if got := emitter.count(task.EventWorkflowReloadFailed); got != 1 {
		t.Fatalf("after 6 calls with identical invalid fingerprint, reload_failed count = %d, want 1 (deduped)", got)
	}

	// Fix the file → exactly one EventWorkflowReloaded; the dedupe state clears.
	writeWorkflowForReloadTestAt(t, path, "linear", 42000, "Rework")
	if err := runtime.ReloadOnce(context.Background()); err != nil {
		t.Fatalf("reload after fix: %v", err)
	}
	if got := emitter.count(task.EventWorkflowReloaded); got != 1 {
		t.Fatalf("after recovery, reloaded count = %d, want 1", got)
	}

	// Break it again with a *different* invalid fingerprint — must emit a new
	// reload_failed (fingerprint changed, so the dedupe cache misses).
	writeWorkflowForReloadTestAt(t, path, "unsupported", 42000, "Rework")
	if err := runtime.ReloadOnce(context.Background()); err == nil {
		t.Fatal("reload of newly-invalid file must return an error")
	}
	if got := emitter.count(task.EventWorkflowReloadFailed); got != 2 {
		t.Fatalf("after second distinct failure, reload_failed count = %d, want 2", got)
	}
}

func TestWorkflowRuntimeReloadOnceDedupesMissingFile(t *testing.T) {
	emitter := &recordingWorkflowReloadEmitter{}
	path := writeWorkflowForReloadTest(t, "linear", 30000)
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{
		Initial:     initial,
		Path:        path,
		Source:      workflow.SourceFile,
		Emitter:     emitter,
		EventTaskID: "workflow-runtime",
	})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatalf("remove workflow file: %v", err)
	}

	for i := 0; i < 6; i++ {
		if err := runtime.ReloadOnce(context.Background()); err == nil {
			t.Fatalf("reload %d with missing file must return an error", i+1)
		}
	}
	if got := emitter.count(task.EventWorkflowReloadFailed); got != 1 {
		t.Fatalf("after 6 missing-file reloads, reload_failed count = %d, want 1 (deduped via read-error sentinel)", got)
	}
}
