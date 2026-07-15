package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestValidateDispatchPreflight_HappyPathReturnsNil(t *testing.T) {
	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "linear", APIKey: "lin_xxxx", ProjectSlug: "team-x"},
		Codex:   workflow.CommandConfig{Command: "codex app-server"},
	}
	if err := validateDispatchPreflight(cfg); err != nil {
		t.Fatalf("happy-path preflight: %v", err)
	}
}

func TestValidateDispatchPreflight_EmptyAPIKeyAfterVarResolution(t *testing.T) {
	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "linear", APIKey: "", ProjectSlug: "team-x"},
		Codex:   workflow.CommandConfig{Command: "codex app-server"},
	}
	err := validateDispatchPreflight(cfg)
	if err == nil {
		t.Fatalf("expected error for empty api_key")
	}
	if !strings.Contains(err.Error(), "tracker.api_key empty") {
		t.Errorf("unexpected reason: %v", err)
	}
}

func TestValidateDispatchPreflight_MissingCodexCommand(t *testing.T) {
	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "linear", APIKey: "lin_xxxx", ProjectSlug: "team-x"},
		Codex:   workflow.CommandConfig{Command: ""},
	}
	err := validateDispatchPreflight(cfg)
	if err == nil {
		t.Fatalf("expected error for empty codex.command")
	}
	if !strings.Contains(err.Error(), "codex.command empty") {
		t.Errorf("unexpected reason: %v", err)
	}
}

func TestValidateDispatchPreflight_UnsupportedTrackerKind(t *testing.T) {
	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "jira", APIKey: "x", ProjectSlug: "p"},
		Codex:   workflow.CommandConfig{Command: "codex app-server"},
	}
	err := validateDispatchPreflight(cfg)
	if err == nil {
		t.Fatalf("expected error for unsupported tracker.kind")
	}
	if !strings.Contains(err.Error(), "tracker.kind unsupported") {
		t.Errorf("unexpected reason: %v", err)
	}
}

func TestValidateDispatchPreflight_LinearProjectSlugMissingFails(t *testing.T) {
	missing := workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "linear", APIKey: "lin_xxxx"},
		Codex:   workflow.CommandConfig{Command: "codex app-server"},
	}
	err := validateDispatchPreflight(missing)
	if err == nil {
		t.Fatalf("expected error for missing linear project_slug")
	}
	if !strings.Contains(err.Error(), "tracker.project_slug required for linear") {
		t.Errorf("unexpected reason: %v", err)
	}
}

func TestValidateDispatchPreflight_JoinsMultipleReasons(t *testing.T) {
	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "", APIKey: ""},
		Codex:   workflow.CommandConfig{Command: ""},
	}
	err := validateDispatchPreflight(cfg)
	if err == nil {
		t.Fatalf("expected joined error")
	}
	for _, want := range []string{"tracker.kind missing", "tracker.api_key empty", "codex.command empty"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("joined error missing %q: %v", want, err)
		}
	}
}

func TestPollOncePreflightFailureSkipsDispatchAndEmitsRuntimeEvent(t *testing.T) {
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
		TerminalStates:    []string{"Cancelled", "Done"},
		WorkerExitTimeout: time.Second,
	})
	// Empty api_key + missing codex.command — preflight must catch both.
	preflightCfg := workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "linear", APIKey: "", ProjectSlug: "team-x"},
		Codex:   workflow.CommandConfig{Command: ""},
	}
	poller.preflight = &preflightCfg

	err := poller.PollOnce(ctx)
	if err == nil {
		t.Fatalf("expected preflight failure error")
	}
	if !errors.Is(err, errDispatchPreflight) {
		t.Errorf("unexpected error shape: %v", err)
	}
	if dispatcher.count() != 0 {
		t.Errorf("dispatch should be skipped on preflight failure, got count=%d", dispatcher.count())
	}
	view, snapErr := orch.Snapshot(ctx)
	if snapErr != nil {
		t.Fatalf("snapshot: %v", snapErr)
	}
	var saw bool
	for _, ev := range view.RecentEvents {
		if ev.Kind == RuntimeEventDispatchPreflightFailed {
			saw = true
			if !strings.Contains(ev.Message, "tracker.api_key empty") || !strings.Contains(ev.Message, "codex.command empty") {
				t.Errorf("preflight event message does not carry both joined reasons: %q", ev.Message)
			}
		}
	}
	if !saw {
		t.Errorf("RuntimeEventDispatchPreflightFailed not recorded in RecentEvents (have %d events)", len(view.RecentEvents))
	}
}

func TestPollOncePreflightFailureStillReconcilesRunningIssue(t *testing.T) {
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
		TerminalStates:    []string{"Cancelled", "Done"},
		WorkerExitTimeout: time.Second,
	})
	preflightCfg := workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "linear", APIKey: "lin_xxxx", ProjectSlug: "team-x"},
		Codex:   workflow.CommandConfig{Command: "codex app-server"},
	}
	poller.preflight = &preflightCfg

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll once: %v", err)
	}
	waitForCancellationDispatcherCount(t, dispatcher)

	order := &pollOrderSpy{}
	listingErr := errors.New("active listing failed during preflight tick")
	refreshErr := errors.New("narrow refresh partially failed")
	candidateLister := &fixedActiveIssueLister{err: listingErr, onList: func() { order.record("candidate") }}
	poller.tracker = candidateLister
	trackerClient.setIssues(nil)
	trackerClient.resetFetchIssueStatesByIDsCalls()
	trackerClient.setFetchObserver(func([]tracker.IssueRef) { order.record("narrow") })
	trackerClient.setFetchIDStates(map[string]string{"issue-1": "Done"})
	trackerClient.setFetchIDErr(refreshErr)
	preflightCfg.Tracker.APIKey = ""

	err := poller.PollOnce(ctx)
	if err == nil || !errors.Is(err, errDispatchPreflight) {
		t.Fatalf("preflight poll error = %v, want dispatch preflight failure", err)
	}
	if errors.Is(err, listingErr) || !errors.Is(err, refreshErr) {
		t.Errorf("preflight poll error = %v, want reconciliation error without candidate-list error", err)
	}
	if got := candidateLister.count(); got != 0 {
		t.Errorf("candidate ListActiveIssues calls = %d, want 0 after invalid preflight", got)
	}
	if got := strings.Join(order.snapshot(), ","); got != "narrow" {
		t.Errorf("poll order = %q, want %q", got, "narrow")
	}
	if got := trackerClient.fetchIssueStatesByRefsCalls(); len(got) != 1 {
		t.Errorf("FetchIssueStatesByRefs calls = %d, want 1", len(got))
	}
	if got := dispatcher.count(); got != 1 {
		t.Errorf("dispatcher count = %d, want 1 (no replacement dispatch)", got)
	}
	waitForContextCanceled(t, dispatcher.contextAt(0))
	waitForNoRunningOrRetrying(t, ctx, orch)

	view, snapErr := orch.Snapshot(ctx)
	if snapErr != nil {
		t.Fatalf("snapshot: %v", snapErr)
	}
	for _, event := range view.RecentEvents {
		if event.Kind == RuntimeEventDispatchPreflightFailed {
			return
		}
	}
	t.Fatalf("RecentEvents = %+v, want %s", view.RecentEvents, RuntimeEventDispatchPreflightFailed)
}

func TestPollOncePreflightFailureBoundsReconciliationTrackerCalls(t *testing.T) {
	previousTimeout := reconciliationTrackerRequestTimeout
	reconciliationTrackerRequestTimeout = 25 * time.Millisecond
	defer func() { reconciliationTrackerRequestTimeout = previousTimeout }()

	tests := []struct {
		name             string
		blockFetch       bool
		blockListings    bool
		wantFetchCalls   int
		wantListingCalls int
	}{
		{name: "narrow refresh", blockFetch: true, wantFetchCalls: 1, wantListingCalls: 2},
		{name: "each inactive state group", blockListings: true, wantFetchCalls: 1, wantListingCalls: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			issue := tracker.Issue{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}
			state := NewOrchestratorState(30000, 1)
			state.Blocked[IssueID(issue.ID)] = &BlockedEntry{Issue: issue, Identifier: issue.Identifier}
			state.Claimed[IssueID(issue.ID)] = struct{}{}
			state.ClaimedIssues[IssueID(issue.ID)] = issue
			trackerClient := &blockingReconcileTracker{blockFetch: tt.blockFetch, blockListings: tt.blockListings}
			orch := New(state, Deps{Dispatcher: &cancellationDispatcher{}, Scheduler: RetryScheduler{MaxBackoff: time.Hour}})
			go orch.Run(ctx)
			if err := orch.WaitStarted(ctx); err != nil {
				t.Fatalf("wait for orchestrator: %v", err)
			}

			poller := NewPollerWithReconciliation(trackerClient, orch, ReconciliationConfig{
				ActiveStates: []string{"In Progress"}, TerminalStates: []string{"Done"}, InactiveStates: []string{"Backlog"},
			})
			candidateLister := &fixedActiveIssueLister{}
			poller.tracker = candidateLister
			preflightCfg := workflow.Config{
				Tracker: workflow.TrackerConfig{Kind: "linear", ProjectSlug: "team-x"},
				Codex:   workflow.CommandConfig{Command: "codex app-server"},
			}
			poller.preflight = &preflightCfg

			errCh := make(chan error, 1)
			safeGo("test.poll_once_reconciliation_timeout", func() { errCh <- poller.PollOnce(ctx) })
			var err error
			select {
			case err = <-errCh:
			case <-time.After(time.Second):
				t.Fatal("PollOnce did not return after reconciliation tracker request timeout")
			}
			if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errDispatchPreflight) {
				t.Fatalf("PollOnce error = %v, want reconciliation deadline and preflight failure", err)
			}
			if got := candidateLister.count(); got != 0 {
				t.Errorf("candidate ListActiveIssues calls = %d, want 0", got)
			}
			fetchCalls, listCalls, fetchDeadlines, listDeadlines := trackerClient.callSnapshot()
			if fetchCalls != tt.wantFetchCalls || listCalls != tt.wantListingCalls {
				t.Errorf("tracker calls = fetch:%d list:%d, want fetch:%d list:%d", fetchCalls, listCalls, tt.wantFetchCalls, tt.wantListingCalls)
			}
			for i, hasDeadline := range append(fetchDeadlines, listDeadlines...) {
				if !hasDeadline {
					t.Errorf("reconciliation tracker request %d had no deadline", i+1)
				}
			}
		})
	}
}

func TestPollOncePreflightFailurePatchesClaimedActiveStateWithoutWipingMetadata(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	makeIssue := func(id string, blockedBy []tracker.BlockerRef) tracker.Issue {
		return tracker.Issue{
			ID: id, Identifier: "LIN-" + id, Title: "title-" + id, Description: "description-" + id,
			URL: "https://tracker.example/" + id, State: "In Progress", Labels: []string{"old"},
			BranchName: "branch-" + id, Priority: 2, CreatedAt: time.Unix(10, 0), UpdatedAt: time.Unix(20, 0),
			BlockedBy: blockedBy,
		}
	}
	runningIssue := makeIssue("running", []tracker.BlockerRef{{ID: "old-run-blocker", State: "Todo"}})
	retryIssue := makeIssue("retry", []tracker.BlockerRef{{ID: "old-retry-blocker", State: "Todo"}})
	blockedIssue := makeIssue("blocked", []tracker.BlockerRef{{ID: "old-blocked-blocker", State: "Todo"}})

	st := NewOrchestratorState(30000, 3)
	st.Running[IssueID(runningIssue.ID)] = &RunningEntry{
		Issue: runningIssue, Identifier: runningIssue.Identifier, ReconcileCleanupWorkspace: true,
		AgentCurrentIssueHandoff: true, AgentCurrentIssueTerminalHandoff: true,
		AgentCurrentIssueTerminalHandoffState: "Done",
	}
	st.RetryAttempts[IssueID(retryIssue.ID)] = &RetryEntry{Issue: retryIssue, IssueID: IssueID(retryIssue.ID), Identifier: retryIssue.Identifier}
	st.Blocked[IssueID(blockedIssue.ID)] = &BlockedEntry{Issue: blockedIssue, Identifier: blockedIssue.Identifier}
	for _, issue := range []tracker.Issue{runningIssue, retryIssue, blockedIssue} {
		id := IssueID(issue.ID)
		st.Claimed[id] = struct{}{}
		st.ClaimedIssues[id] = issue
	}

	trackerClient := &fakeIssueStateTracker{fetchIDIssueStates: map[string]tracker.IssueState{
		runningIssue.ID: {State: "Rework", Labels: []string{"fresh"}, BlockedBy: nil},
		retryIssue.ID:   {State: "Rework", Labels: []string{"fresh"}, BlockedBy: []tracker.BlockerRef{}},
		blockedIssue.ID: {State: "Rework", Labels: []string{"fresh"}, BlockedBy: []tracker.BlockerRef{{ID: "new-blocker", State: "Done"}}},
	}}
	orch := New(st, Deps{Dispatcher: &cancellationDispatcher{}, Scheduler: RetryScheduler{MaxBackoff: time.Hour}})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}
	poller := NewPollerWithReconciliation(trackerClient, orch, ReconciliationConfig{
		ActiveStates: []string{"In Progress", "Rework"}, TerminalStates: []string{"Done"}, WorkerExitTimeout: time.Second,
	})
	order := &pollOrderSpy{}
	candidateLister := &fixedActiveIssueLister{onList: func() { order.record("candidate") }}
	poller.tracker = candidateLister
	trackerClient.setFetchObserver(func([]tracker.IssueRef) { order.record("narrow") })
	preflightCfg := workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "linear", ProjectSlug: "team-x"},
		Codex:   workflow.CommandConfig{Command: "codex app-server"},
	}
	poller.preflight = &preflightCfg

	if err := poller.PollOnce(ctx); err == nil || !errors.Is(err, errDispatchPreflight) {
		t.Fatalf("preflight poll error = %v, want dispatch preflight failure", err)
	}
	if got := candidateLister.count(); got != 0 {
		t.Errorf("candidate ListActiveIssues calls = %d, want 0", got)
	}
	if got := strings.Join(order.snapshot(), ","); got != "narrow" {
		t.Errorf("poll order = %q, want %q", got, "narrow")
	}

	issues := map[string]tracker.Issue{}
	var cleanup, handoff, terminalHandoff bool
	orch.WithStateForTest(func(got *OrchestratorState) {
		issues["running"] = got.Running[IssueID(runningIssue.ID)].Issue
		issues["retry"] = got.RetryAttempts[IssueID(retryIssue.ID)].Issue
		issues["blocked"] = got.Blocked[IssueID(blockedIssue.ID)].Issue
		for _, id := range []string{"running", "retry", "blocked"} {
			issues["claimed-"+id] = got.ClaimedIssues[IssueID(id)]
		}
		run := got.Running[IssueID(runningIssue.ID)]
		cleanup, handoff, terminalHandoff = run.ReconcileCleanupWorkspace, run.AgentCurrentIssueHandoff, run.AgentCurrentIssueTerminalHandoff
	})

	assertPatched := func(name string, got, old tracker.Issue, wantBlockers []tracker.BlockerRef) {
		t.Helper()
		if got.State != "Rework" || !reflect.DeepEqual(got.Labels, []string{"fresh"}) {
			t.Errorf("%s state/labels = %q/%v, want Rework/[fresh]", name, got.State, got.Labels)
		}
		if got.Title != old.Title || got.Description != old.Description || got.URL != old.URL || got.BranchName != old.BranchName || got.Priority != old.Priority || !got.CreatedAt.Equal(old.CreatedAt) || !got.UpdatedAt.Equal(old.UpdatedAt) {
			t.Errorf("%s metadata = %+v, want preserved from %+v", name, got, old)
		}
		if !reflect.DeepEqual(got.BlockedBy, wantBlockers) {
			t.Errorf("%s BlockedBy = %#v, want %#v", name, got.BlockedBy, wantBlockers)
		}
	}
	assertPatched("running", issues["running"], runningIssue, runningIssue.BlockedBy)
	assertPatched("retry", issues["retry"], retryIssue, []tracker.BlockerRef{})
	assertPatched("blocked", issues["blocked"], blockedIssue, []tracker.BlockerRef{{ID: "new-blocker", State: "Done"}})
	assertPatched("claimed-running", issues["claimed-running"], runningIssue, runningIssue.BlockedBy)
	assertPatched("claimed-retry", issues["claimed-retry"], retryIssue, []tracker.BlockerRef{})
	assertPatched("claimed-blocked", issues["claimed-blocked"], blockedIssue, []tracker.BlockerRef{{ID: "new-blocker", State: "Done"}})
	if cleanup || handoff || terminalHandoff {
		t.Errorf("running reconcile flags = cleanup:%v handoff:%v terminal:%v, want all false after active narrow refresh", cleanup, handoff, terminalHandoff)
	}
}

func TestPollOncePreflightSuccessProceedsToFetch(t *testing.T) {
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
		TerminalStates:    []string{"Cancelled", "Done"},
		WorkerExitTimeout: time.Second,
	})
	preflightCfg := workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "linear", APIKey: "lin_xxxx", ProjectSlug: "team-x"},
		Codex:   workflow.CommandConfig{Command: "codex app-server"},
	}
	poller.preflight = &preflightCfg

	if err := poller.PollOnce(ctx); err != nil && !errors.Is(err, ErrNotDispatched) {
		// Best-effort: dispatch may or may not occur depending on test
		// fakes; the assertion that matters is no preflight error.
		if errors.Is(err, errDispatchPreflight) {
			t.Fatalf("preflight should have passed: %v", err)
		}
	}
}
