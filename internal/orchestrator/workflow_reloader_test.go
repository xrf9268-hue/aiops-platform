package orchestrator

import (
	"context"
	"errors"
	"os"
	"sort"
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
	path := writeWorkflowForReloadTest(t, "linear", 30000, "AI Ready")
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
	if got := snap.Workflow.Config.Tracker.PollIntervalMs; got != 45000 {
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
	path := writeWorkflowForReloadTest(t, "linear", 30000, "AI Ready")
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
	path := writeWorkflowForReloadTest(t, "linear", 30000, "AI Ready")
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{
		{ID: "issue-ready", Identifier: "ISSUE-1", Title: "ready", State: "AI Ready"},
		{ID: "issue-rework", Identifier: "ISSUE-2", Title: "rework", State: "Rework"},
	}}
	dispatcher := &fakeDispatcher{}
	orch, cancel := startActor(t, Deps{Dispatcher: dispatcher, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()
	poller, err := NewRuntimePoller(trackerClient, orch, runtime, worker.Config{}, nil)
	if err != nil {
		t.Fatalf("new runtime poller: %v", err)
	}

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

	path := writeWorkflowForReloadTest(t, "linear", 30000, "AI Ready")
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
		{ID: "issue-1", Identifier: "ISSUE-1", Title: "one", State: "AI Ready"},
		{ID: "issue-2", Identifier: "ISSUE-2", Title: "two", State: "AI Ready"},
	}}
	dispatcher := &blockingDispatcher{}
	orch := New(NewOrchestratorState(30000, initial.Config.Agent.MaxConcurrentAgents), Deps{Dispatcher: dispatcher, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}
	poller, err := NewRuntimePoller(trackerClient, orch, runtime, worker.Config{}, nil)
	if err != nil {
		t.Fatalf("new runtime poller: %v", err)
	}

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	waitForBlockingDispatcherCount(t, dispatcher, 1)

	writeWorkflowForReloadTestAt(t, path, "linear", 30000, "AI Ready", withReloadTestMaxConcurrentAgents(2))
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

	path := writeWorkflowForReloadTest(t, "linear", 30000, "AI Ready", withReloadTestMaxConcurrentAgents(10), withReloadTestMaxConcurrentAgentsByState(map[string]int{"AI Ready": 1}))
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{
		{ID: "issue-1", Identifier: "ISSUE-1", Title: "one", State: "AI Ready"},
		{ID: "issue-2", Identifier: "ISSUE-2", Title: "two", State: "AI Ready"},
	}}
	dispatcher := &blockingDispatcher{}
	st := NewOrchestratorState(30000, initial.Config.Agent.MaxConcurrentAgents)
	st.MaxConcurrentAgentsByState = initial.Config.Agent.MaxConcurrentAgentsByState
	orch := New(st, Deps{Dispatcher: dispatcher, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}
	poller, err := NewRuntimePoller(trackerClient, orch, runtime, worker.Config{}, nil)
	if err != nil {
		t.Fatalf("new runtime poller: %v", err)
	}

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	waitForBlockingDispatcherCount(t, dispatcher, 1)

	writeWorkflowForReloadTestAt(t, path, "linear", 30000, "AI Ready", withReloadTestMaxConcurrentAgents(10), withReloadTestMaxConcurrentAgentsByState(map[string]int{"ai_ready": 2}))
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

	path := writeWorkflowForReloadTest(t, "linear", 30000, "AI Ready", withReloadTestMaxRetryBackoffMs(1000))
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "ISSUE-1", Title: "one", State: "AI Ready"}}}
	dispatcher := &fakeDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{Dispatcher: dispatcher, Scheduler: RetryScheduler{MaxBackoff: time.Second}})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}
	poller, err := NewRuntimePoller(trackerClient, orch, runtime, worker.Config{}, nil)
	if err != nil {
		t.Fatalf("new runtime poller: %v", err)
	}

	writeWorkflowForReloadTestAt(t, path, "linear", 30000, "AI Ready", withReloadTestMaxRetryBackoffMs(50))
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
			if retry.IssueID == "issue-1" && retry.DueAt.Sub(time.Now()) <= 200*time.Millisecond {
				return true
			}
		}
		return false
	}, time.Second)
}

func TestRuntimePollerRebuildsTrackerClientAfterTrackerConfigReload(t *testing.T) {
	ctx := context.Background()
	path := writeWorkflowForReloadTest(t, "linear", 30000, "AI Ready")
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	trackersByKind := map[string]*fakeIssueStateTracker{
		"linear": {issues: []tracker.Issue{{ID: "linear-ready", Identifier: "ISSUE-1", Title: "ready", State: "AI Ready"}}},
		"gitea":  {issues: []tracker.Issue{{ID: "gitea-rework", Identifier: "ISSUE-2", Title: "rework", State: "Rework"}}},
	}
	factoryCalls := 0
	dispatcher := &fakeDispatcher{}
	orch, cancel := startActor(t, Deps{Dispatcher: dispatcher, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()
	poller, err := NewRuntimePollerWithTrackerFactory(func(cfg workflow.Config) (IssueStateLister, error) {
		factoryCalls++
		return trackersByKind[cfg.Tracker.Kind], nil
	}, orch, runtime, worker.Config{}, nil)
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
	if factoryCalls < 2 {
		t.Fatalf("tracker factory calls = %d, want at least 2 after tracker config reload", factoryCalls)
	}
}

func TestRuntimePollerFetchesLinearIssuesFromEachServiceProject(t *testing.T) {
	ctx := context.Background()
	path := writeWorkflowForReloadTest(t, "linear", 30000, "AI Ready")
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	initial.Config.Tracker.ProjectSlug = ""
	initial.Config.Services = []workflow.ServiceConfig{
		{Name: "api", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "api-platform"}, Repo: workflow.RepoConfig{Owner: "acme", Name: "api", CloneURL: "git@example.com:acme/api.git", DefaultBranch: "main"}},
		{Name: "web", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "web-platform"}, Repo: workflow.RepoConfig{Owner: "acme", Name: "web", CloneURL: "git@example.com:acme/web.git", DefaultBranch: "main"}},
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	trackerClient := &fakeProjectScopedIssueTracker{
		issuesByProject: map[string][]tracker.Issue{
			"api-platform": {{ID: "api-1", Identifier: "API-1", Title: "API work", State: "AI Ready", ProjectSlug: "api-platform"}},
			"web-platform": {{ID: "web-1", Identifier: "WEB-1", Title: "Web work", State: "AI Ready", ProjectSlug: "web-platform"}},
		},
	}
	dispatcher := &recordingDispatcher{}
	orch, cancel := startActor(t, Deps{Dispatcher: dispatcher, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()
	poller, err := NewRuntimePollerWithTrackerFactory(func(cfg workflow.Config) (IssueStateLister, error) {
		return trackerClient.forProject(cfg.Tracker.ProjectSlug), nil
	}, orch, runtime, worker.Config{}, nil)
	if err != nil {
		t.Fatalf("new runtime poller: %v", err)
	}

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll once: %v", err)
	}
	if got := dispatcher.count(); got != 2 {
		t.Fatalf("dispatched issues = %d, want both service projects", got)
	}
	if got := strings.Join(trackerClient.projects(), ","); got != "api-platform,api-platform,web-platform,web-platform" {
		t.Fatalf("queried projects = %q, want active and terminal reconciliation queries for api-platform,web-platform", got)
	}
}

func TestRuntimePollerDispatchesHealthyServiceProjectWhenAnotherProjectPollFails(t *testing.T) {
	ctx := context.Background()
	path := writeWorkflowForReloadTest(t, "linear", 30000, "AI Ready")
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	initial.Config.Tracker.ProjectSlug = ""
	initial.Config.Services = []workflow.ServiceConfig{
		{Name: "api", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "api-platform"}, Repo: workflow.RepoConfig{Owner: "acme", Name: "api", CloneURL: "git@example.com:acme/api.git", DefaultBranch: "main"}},
		{Name: "web", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "web-platform"}, Repo: workflow.RepoConfig{Owner: "acme", Name: "web", CloneURL: "git@example.com:acme/web.git", DefaultBranch: "main"}},
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	trackerClient := &fakeProjectScopedIssueTracker{
		issuesByProject: map[string][]tracker.Issue{
			"api-platform": {{ID: "api-1", Identifier: "API-1", Title: "API work", State: "AI Ready", ProjectSlug: "api-platform"}},
		},
		errsByProject: map[string]error{"web-platform": errors.New("web project unavailable")},
	}
	dispatcher := &recordingDispatcher{}
	orch, cancel := startActor(t, Deps{Dispatcher: dispatcher, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()
	poller, err := NewRuntimePollerWithTrackerFactory(func(cfg workflow.Config) (IssueStateLister, error) {
		return trackerClient.forProject(cfg.Tracker.ProjectSlug), nil
	}, orch, runtime, worker.Config{}, nil)
	if err != nil {
		t.Fatalf("new runtime poller: %v", err)
	}

	err = poller.PollOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "web project unavailable") {
		t.Fatalf("poll once error = %v, want failed project reported", err)
	}
	if got := dispatcher.count(); got != 1 {
		t.Fatalf("dispatched issues = %d, want healthy project issue dispatched despite failed project", got)
	}
	if got := dispatcher.issueAt(0).ID; got != "api-1" {
		t.Fatalf("dispatched issue = %q, want api-1", got)
	}
}

func TestRuntimePollerKeepsRunningFailedProjectIssueWhenDispatchingHealthyPartialPoll(t *testing.T) {
	ctx := context.Background()
	path := writeWorkflowForReloadTest(t, "linear", 30000, "AI Ready")
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	initial.Config.Tracker.ProjectSlug = ""
	initial.Config.Services = []workflow.ServiceConfig{
		{Name: "api", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "api-platform"}, Repo: workflow.RepoConfig{Owner: "acme", Name: "api", CloneURL: "git@example.com:acme/api.git", DefaultBranch: "main"}},
		{Name: "web", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "web-platform"}, Repo: workflow.RepoConfig{Owner: "acme", Name: "web", CloneURL: "git@example.com:acme/web.git", DefaultBranch: "main"}},
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	trackerClient := &fakeProjectScopedIssueTracker{
		issuesByProject: map[string][]tracker.Issue{
			"web-platform": {{ID: "web-1", Identifier: "WEB-1", Title: "Web work", State: "AI Ready", ProjectSlug: "web-platform"}},
		},
	}
	dispatcher := &cancellationDispatcher{}
	orch, cancel := startActor(t, Deps{Dispatcher: dispatcher, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()
	poller, err := NewRuntimePollerWithTrackerFactory(func(cfg workflow.Config) (IssueStateLister, error) {
		return trackerClient.forProject(cfg.Tracker.ProjectSlug), nil
	}, orch, runtime, worker.Config{}, nil)
	if err != nil {
		t.Fatalf("new runtime poller: %v", err)
	}

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll once: %v", err)
	}
	waitForCancellationDispatcherCount(t, dispatcher, 1)

	trackerClient.mu.Lock()
	trackerClient.issuesByProject = map[string][]tracker.Issue{
		"api-platform": {{ID: "api-1", Identifier: "API-1", Title: "API work", State: "AI Ready", ProjectSlug: "api-platform"}},
	}
	trackerClient.errsByProject = map[string]error{"web-platform": errors.New("web project unavailable")}
	trackerClient.mu.Unlock()

	err = poller.PollOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "web project unavailable") {
		t.Fatalf("poll once error = %v, want failed project reported", err)
	}
	waitForCancellationDispatcherCount(t, dispatcher, 2)
	select {
	case <-dispatcher.contextAt(0).Done():
		t.Fatal("running issue from failed project was canceled after partial active poll")
	default:
	}
}

func TestRuntimePollerCancelsHealthyProjectIssueDuringPartialPoll(t *testing.T) {
	ctx := context.Background()
	path := writeWorkflowForReloadTest(t, "linear", 30000, "AI Ready")
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	initial.Config.Tracker.ProjectSlug = ""
	initial.Config.Services = []workflow.ServiceConfig{
		{Name: "api", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "api-platform"}, Repo: workflow.RepoConfig{Owner: "acme", Name: "api", CloneURL: "git@example.com:acme/api.git", DefaultBranch: "main"}},
		{Name: "web", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "web-platform"}, Repo: workflow.RepoConfig{Owner: "acme", Name: "web", CloneURL: "git@example.com:acme/web.git", DefaultBranch: "main"}},
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	trackerClient := &fakeProjectScopedIssueTracker{
		issuesByProject: map[string][]tracker.Issue{
			"api-platform": {{ID: "api-1", Identifier: "API-1", Title: "API work", State: "AI Ready", ProjectSlug: "api-platform"}},
		},
	}
	dispatcher := &cancellationDispatcher{}
	orch, cancel := startActor(t, Deps{Dispatcher: dispatcher, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()
	poller, err := NewRuntimePollerWithTrackerFactory(func(cfg workflow.Config) (IssueStateLister, error) {
		return trackerClient.forProject(cfg.Tracker.ProjectSlug), nil
	}, orch, runtime, worker.Config{}, nil)
	if err != nil {
		t.Fatalf("new runtime poller: %v", err)
	}

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll once: %v", err)
	}
	waitForCancellationDispatcherCount(t, dispatcher, 1)

	trackerClient.mu.Lock()
	trackerClient.issuesByProject = map[string][]tracker.Issue{
		"web-platform": {{ID: "web-1", Identifier: "WEB-1", Title: "Web work", State: "AI Ready", ProjectSlug: "web-platform"}},
	}
	trackerClient.mu.Unlock()

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("web-only poll once: %v", err)
	}
	waitForContextCanceled(t, dispatcher.contextAt(0))
	waitForCancellationDispatcherCount(t, dispatcher, 2)

	trackerClient.mu.Lock()
	trackerClient.issuesByProject = map[string][]tracker.Issue{
		"api-platform": {{ID: "api-1", Identifier: "API-1", Title: "API work", State: "AI Ready", ProjectSlug: "api-platform"}},
	}
	trackerClient.errsByProject = map[string]error{"web-platform": errors.New("web project unavailable")}
	trackerClient.mu.Unlock()

	err = poller.PollOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "web project unavailable") {
		t.Fatalf("poll once error = %v, want failed project reported", err)
	}
	select {
	case <-dispatcher.contextAt(1).Done():
		t.Fatal("healthy project issue was canceled after partial active poll")
	default:
	}
}

func TestRuntimePollerDoesNotFanOutServiceProjectsForGitea(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Tracker.Kind = "gitea"
	cfg.Tracker.ProjectSlug = "https://gitea.example.com"
	cfg.Services = []workflow.ServiceConfig{
		{Name: "api", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "api-platform"}, Repo: workflow.RepoConfig{Owner: "acme", Name: "api", CloneURL: "git@example.com:acme/api.git", DefaultBranch: "main"}},
		{Name: "web", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "web-platform"}, Repo: workflow.RepoConfig{Owner: "acme", Name: "web", CloneURL: "git@example.com:acme/web.git", DefaultBranch: "main"}},
	}

	got := TrackerProjectConfigs(cfg)
	if len(got) != 1 {
		t.Fatalf("tracker configs = %d, want one non-Linear tracker config", len(got))
	}
	if got[0].Tracker.ProjectSlug != "https://gitea.example.com" {
		t.Fatalf("tracker project slug = %q, want original Gitea base URL", got[0].Tracker.ProjectSlug)
	}
}

func TestRuntimePollerOnlyAppliesRoutingToLinearServiceWorkflows(t *testing.T) {
	ctx := context.Background()
	path := writeWorkflowForReloadTest(t, "gitea", 30000, "AI Ready")
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	initial.Config.Repo = workflow.RepoConfig{Owner: "acme", Name: "fallback", CloneURL: "git@example.com:acme/fallback.git", DefaultBranch: "main"}
	initial.Config.Services = []workflow.ServiceConfig{
		{Name: "api", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "api-platform"}, Repo: workflow.RepoConfig{Owner: "acme", Name: "api", CloneURL: "git@example.com:acme/api.git", DefaultBranch: "main"}},
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{
		{ID: "gitea-ready", Identifier: "ISSUE-1", Title: "ready", State: "AI Ready"},
	}}
	dispatcher := &recordingDispatcher{}
	orch, cancel := startActor(t, Deps{Dispatcher: dispatcher, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()
	poller, err := NewRuntimePoller(trackerClient, orch, runtime, worker.Config{}, nil)
	if err != nil {
		t.Fatalf("new runtime poller: %v", err)
	}
	if poller.poller.routing != nil {
		t.Fatal("gitea runtime poller enabled Linear service routing")
	}

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll once: %v", err)
	}
	waitFor(t, func() bool { return dispatcher.count() == 1 }, time.Second)
	got := dispatcher.issueAt(0)
	if got.ID != "gitea-ready" {
		t.Fatalf("dispatched issue = %q, want gitea-ready", got.ID)
	}
	if got.ServiceName != "" {
		t.Fatalf("gitea issue service = %q, want no Linear service routing", got.ServiceName)
	}

	linearPath := writeWorkflowForReloadTest(t, "linear", 30000, "AI Ready")
	linearWorkflow, err := workflow.Load(linearPath)
	if err != nil {
		t.Fatalf("load linear workflow: %v", err)
	}
	linearWorkflow.Config.Tracker.ProjectSlug = ""
	linearWorkflow.Config.Services = []workflow.ServiceConfig{
		{Name: "api", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "api-platform"}, Repo: workflow.RepoConfig{Owner: "acme", Name: "api", CloneURL: "git@example.com:acme/api.git", DefaultBranch: "main"}},
	}
	linearRuntime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: linearWorkflow, Path: linearPath, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new linear runtime: %v", err)
	}
	linearPoller, err := NewRuntimePoller(&fakeIssueStateTracker{}, orch, linearRuntime, worker.Config{}, nil)
	if err != nil {
		t.Fatalf("new linear runtime poller: %v", err)
	}
	if linearPoller.poller.routing == nil {
		t.Fatal("linear service runtime poller did not enable service routing")
	}
}

func TestWorkflowRuntimeReloadFailureKeepsPreviousConfigAndEmitsFailureEvent(t *testing.T) {
	path := writeWorkflowForReloadTest(t, "linear", 30000, "AI Ready")
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
	if got := snap.Workflow.Config.Tracker.PollIntervalMs; got != 30000 {
		t.Fatalf("poll interval after failed reload = %d, want previous 30000", got)
	}
	if got := snap.Workflow.Config.Tracker.ActiveStates; len(got) != 1 || got[0] != "AI Ready" {
		t.Fatalf("active states after failed reload = %#v, want previous [AI Ready]", got)
	}
	if got := emitter.count(task.EventWorkflowReloadFailed); got != 1 {
		t.Fatalf("workflow_reload_failed event count = %d, want 1", got)
	}
}

func TestRunWorkflowReloadLoopPollFallbackReloadsChangedWorkflow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := writeWorkflowForReloadTest(t, "linear", 30000, "AI Ready")
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
	if got := snap.Workflow.Config.Tracker.PollIntervalMs; got != 42000 {
		t.Fatalf("poll interval after polling reload loop = %d, want 42000", got)
	}
	if got := snap.Workflow.Config.Tracker.ActiveStates; len(got) != 1 || got[0] != "Rework" {
		t.Fatalf("active states after polling reload loop = %#v, want [Rework]", got)
	}
}

func TestRunPollLoopWithRuntimeUsesReloadedPollingCadence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := writeWorkflowForReloadTest(t, "linear", 25, "AI Ready")
	initial, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("load initial workflow: %v", err)
	}
	runtime, err := NewWorkflowRuntime(WorkflowRuntimeConfig{Initial: initial, Path: path, Source: workflow.SourceFile})
	if err != nil {
		t.Fatalf("new runtime: %v", err)
	}
	poller := &countingPollOnce{afterFirst: func() {
		writeWorkflowForReloadTestAt(t, path, "linear", 75, "AI Ready")
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
	path := writeWorkflowForReloadTest(t, "linear", 60000, "AI Ready")
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
	poller, err := NewRuntimePoller(&fakeIssueStateTracker{}, orch, runtime, worker.Config{}, nil)
	if err != nil {
		t.Fatalf("new runtime poller: %v", err)
	}
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

type fakeProjectScopedIssueTracker struct {
	mu              sync.Mutex
	project         string
	root            *fakeProjectScopedIssueTracker
	issuesByProject map[string][]tracker.Issue
	errsByProject   map[string]error
	queriedProjects []string
}

func (f *fakeProjectScopedIssueTracker) forProject(project string) IssueStateLister {
	return &fakeProjectScopedIssueTracker{project: project, root: f}
}

func (f *fakeProjectScopedIssueTracker) ListIssuesByStates(_ context.Context, states []string) ([]tracker.Issue, error) {
	root := f.root
	if root == nil {
		root = f
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	root.queriedProjects = append(root.queriedProjects, f.project)
	if err := root.errsByProject[f.project]; err != nil {
		return nil, err
	}
	wanted := normalizedStates(states)
	out := make([]tracker.Issue, 0, len(root.issuesByProject[f.project]))
	for _, issue := range root.issuesByProject[f.project] {
		if isActiveTrackerState(issue.State, wanted) {
			out = append(out, issue)
		}
	}
	return out, nil
}

func (f *fakeProjectScopedIssueTracker) projects() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.queriedProjects...)
	sort.Strings(out)
	return out
}

func (s *reloadLoopTestSleeper) sleep(_ context.Context, _ time.Duration) error {
	s.calls++
	if s.calls == 1 && s.afterFirst != nil {
		s.afterFirst()
	}
	return nil
}

func writeWorkflowForReloadTest(t *testing.T, trackerKind string, pollIntervalMs int, activeState string, opts ...reloadWorkflowTestOption) string {
	t.Helper()
	path := t.TempDir() + "/WORKFLOW.md"
	writeWorkflowForReloadTestAt(t, path, trackerKind, pollIntervalMs, activeState, opts...)
	return path
}

func writeWorkflowForReloadTestAt(t *testing.T, path, trackerKind string, pollIntervalMs int, activeState string, opts ...reloadWorkflowTestOption) {
	t.Helper()
	cfg := reloadWorkflowTestConfig{maxConcurrentAgents: 100, maxRetryBackoffMs: 300000}
	for _, opt := range opts {
		opt(&cfg)
	}
	projectSlug := ""
	if trackerKind == "linear" {
		projectSlug = "  project_slug: platform\n"
	}
	content := "---\n" +
		"repo:\n" +
		"  owner: xrf9268-hue\n" +
		"  name: aiops-platform\n" +
		"  clone_url: https://github.com/xrf9268-hue/aiops-platform.git\n" +
		"tracker:\n" +
		"  kind: " + trackerKind + "\n" +
		projectSlug +
		"  active_states: [\"" + activeState + "\"]\n" +
		"  terminal_states: [\"Done\"]\n" +
		"polling:\n" +
		"  interval_ms: " + itoaForReloadTest(pollIntervalMs) + "\n" +
		"agent:\n" +
		"  default: mock\n" +
		"  max_concurrent_agents: " + itoaForReloadTest(cfg.maxConcurrentAgents) + "\n" +
		"  max_retry_backoff_ms: " + itoaForReloadTest(cfg.maxRetryBackoffMs) + "\n" +
		reloadTestMaxConcurrentAgentsByStateYAML(cfg.maxConcurrentAgentsByState) +
		"---\n" +
		"Prompt body\n"
	if err := osWriteFileForReloadTest(path, []byte(content)); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
}

type reloadWorkflowTestConfig struct {
	maxConcurrentAgents        int
	maxRetryBackoffMs          int
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

var osWriteFileForReloadTest = func(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

func itoaForReloadTest(v int) string {
	return strconv.Itoa(v)
}
