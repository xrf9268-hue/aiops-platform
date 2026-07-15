package orchestrator

// actor_reconcile_inactive_test.go pins tracker-reconciliation release behavior
// across partial inactive listings and explicit narrow-refresh absence:
//
//   - Partial-listing safety: an in-process entry whose issue is ABSENT from a
//     (possibly pagination-truncated) inactive listing is "unknown", not
//     "inactive" — its claim stays intact and no cleanup fires. The running
//     loop's guard is covered by existing terminal/non-terminal reconcile tests;
//     these add the retry-queued and blocked loops (dropping their `if !ok`
//     guard otherwise releases the claim).
//
//   - Non-terminal listed release: a blocked entry whose issue IS listed but in a
//     merely-inactive (non-terminal) state must be released WITHOUT workspace
//     cleanup. The retry non-terminal release is covered by
//     TestReconcileInactiveContinuationRetryKeepsWorkspace; the blocked one was
//     not, so gating the release behind the terminal check would slip past the
//     suite (the helper releases via `defer st.ReleaseClaim`, which must run on
//     the non-terminal path too).
//
//   - Confirmed absence: running and blocked entries plus continuation retries
//     release without terminal cleanup; failure/quota retries remain timer-owned.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
)

func TestReconcileAbsentTrackerIssuesWaitsForRunningWorkerDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	id := IssueID("ENG-ABSENT-RUN")
	workerDone := make(chan struct{})
	cancelCause := make(chan error, 1)
	st := NewOrchestratorState(15000, 4)
	st.BeginDispatch(id, &RunningEntry{
		Issue:                                 tracker.Issue{ID: string(id), Identifier: "ENG-1", State: "In Progress"},
		Identifier:                            "ENG-1",
		Done:                                  workerDone,
		CancelWorker:                          func(err error) { cancelCause <- err },
		ReconcileCleanupWorkspace:             true,
		AgentCurrentIssueHandoff:              true,
		AgentCurrentIssueTerminalHandoff:      true,
		AgentCurrentIssueTerminalHandoffState: "Done",
	})
	cleaner := &recordingWorkspaceCleaner{}
	o := New(st, Deps{Dispatcher: &fakeDispatcher{}, Scheduler: RetryScheduler{MaxBackoff: time.Minute}, WorkspaceCleaner: cleaner})
	safeGo("test.reconcile_absent_wait", func() { o.Run(ctx) })
	if err := o.WaitStarted(ctx); err != nil {
		t.Fatalf("WaitStarted() error = %v; want nil", err)
	}

	result := make(chan error, 1)
	safeGo("test.reconcile_absent_call", func() {
		result <- o.ReconcileAbsentTrackerIssuesAndWait(ctx, map[string]struct{}{string(id): {}}, time.Second)
	})

	select {
	case got := <-cancelCause:
		if !errors.Is(got, worker.ErrReconcileCancel) {
			t.Fatalf("cancel cause = %v; want %v", got, worker.ErrReconcileCancel)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel cause received = false after 1s; want true")
	}
	select {
	case err := <-result:
		t.Fatalf("ReconcileAbsentTrackerIssuesAndWait returned before Done closed with error = %v; want call blocked", err)
	default:
	}

	close(workerDone)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("ReconcileAbsentTrackerIssuesAndWait() error = %v; want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ReconcileAbsentTrackerIssuesAndWait returned after Done closed = false after 1s; want true")
	}

	var run RunningEntry
	var found, claimed bool
	o.WithStateForTest(func(got *OrchestratorState) {
		if entry := got.Running[id]; entry != nil {
			run = *entry
			found = true
		}
		_, claimed = got.Claimed[id]
	})
	if !found {
		t.Fatalf("Running[%s] = nil; want non-nil until test worker finalizes", id)
	}
	if claimed {
		t.Errorf("Claimed[%s] present = true; want released after confirmed absence", id)
	}
	if !run.ReconcileCancel || run.ReconcileCleanupWorkspace || run.AgentCurrentIssueHandoff || run.AgentCurrentIssueTerminalHandoff || run.AgentCurrentIssueTerminalHandoffState != "" {
		t.Errorf("running reconcile flags = cancel:%v cleanup:%v handoff:%v terminal:%v state:%q; want true,false,false,false,empty",
			run.ReconcileCancel, run.ReconcileCleanupWorkspace, run.AgentCurrentIssueHandoff,
			run.AgentCurrentIssueTerminalHandoff, run.AgentCurrentIssueTerminalHandoffState)
	}
	if calls := cleaner.snapshot(); len(calls) != 0 {
		t.Fatalf("workspace cleanups = %+v; want none for confirmed absence", calls)
	}
}

func TestReconcileAbsentTrackerIssuesReleasesOnlyAbsenceOwnedClaims(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st := NewOrchestratorState(15000, 8)
	issueFor := func(id IssueID) tracker.Issue {
		return tracker.Issue{ID: string(id), Identifier: string(id), State: "In Progress"}
	}
	blockedID := IssueID("ABSENT-BLOCKED")
	st.Blocked[blockedID] = &BlockedEntry{Issue: issueFor(blockedID), Identifier: string(blockedID)}
	st.Claimed[blockedID] = struct{}{}
	st.ClaimedIssues[blockedID] = issueFor(blockedID)

	continuationID := IssueID("ABSENT-CONTINUATION")
	continuationTimer := time.NewTimer(time.Hour)
	st.ScheduleRetry(&RetryEntry{
		Issue: issueFor(continuationID), IssueID: continuationID, Identifier: string(continuationID),
		Kind: RetryKindContinuation, Timer: continuationTimer,
	})
	retainedTimers := make(map[IssueID]*time.Timer)
	for _, tc := range []struct {
		id   IssueID
		kind RetryKind
	}{
		{id: "ABSENT-FAILURE", kind: RetryKindFailure},
		{id: "ABSENT-CAPACITY-DEFERRED", kind: RetryKindFailure},
		{id: "ABSENT-QUOTA", kind: RetryKindQuotaBackoff},
		{id: "UNKNOWN-CONTINUATION", kind: RetryKindContinuation},
	} {
		timer := time.NewTimer(time.Hour)
		retainedTimers[tc.id] = timer
		st.ScheduleRetry(&RetryEntry{Issue: issueFor(tc.id), IssueID: tc.id, Identifier: string(tc.id), Kind: tc.kind, Timer: timer})
	}

	o := New(st, Deps{Dispatcher: &fakeDispatcher{}, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	safeGo("test.reconcile_absent_owned_claims", func() { o.Run(ctx) })
	if err := o.WaitStarted(ctx); err != nil {
		t.Fatalf("WaitStarted() error = %v; want nil", err)
	}
	absent := map[string]struct{}{
		string(blockedID): {}, string(continuationID): {},
		"ABSENT-FAILURE": {}, "ABSENT-CAPACITY-DEFERRED": {}, "ABSENT-QUOTA": {},
	}
	if err := o.ReconcileAbsentTrackerIssuesAndWait(ctx, absent, time.Second); err != nil {
		t.Fatalf("ReconcileAbsentTrackerIssuesAndWait() error = %v; want nil", err)
	}

	o.WithStateForTest(func(got *OrchestratorState) {
		for _, id := range []IssueID{blockedID, continuationID} {
			if got.IsClaimed(id) {
				t.Errorf("IsClaimed(%s) = true; want released", id)
			}
		}
		for _, id := range []IssueID{"ABSENT-FAILURE", "ABSENT-CAPACITY-DEFERRED", "ABSENT-QUOTA", "UNKNOWN-CONTINUATION"} {
			if !got.IsClaimed(id) || got.RetryAttempts[id] == nil {
				t.Errorf("retry %s retained = %v claimed = %v; want both true", id, got.RetryAttempts[id] != nil, got.IsClaimed(id))
			}
		}
	})
	if continuationTimer.Stop() {
		t.Fatal("released continuation timer active = true; want false after ReleaseClaim")
	}
	for id, timer := range retainedTimers {
		if !timer.Stop() {
			t.Errorf("retained retry timer %s was stopped; want timer ownership preserved", id)
		}
	}
}

// blockIssue dispatches issue and drives it to the Blocked state via an
// input-required runtime event, returning once the actor reports it blocked.
func blockIssue(t *testing.T, o *Orchestrator, disp *fakeDispatcher, issue tracker.Issue, spawnIndex, wantBlocked int) {
	t.Helper()
	if err := o.RequestDispatch(context.Background(), issue, nil); err != nil {
		t.Fatalf("RequestDispatch %s: %v", issue.ID, err)
	}
	waitFor(t, func() bool { return disp.count() == spawnIndex+1 }, time.Second)
	if err := o.RecordRuntimeEvent(context.Background(), issue.ID, task.RuntimeEvent{
		Event:   task.EventTurnInputRequired,
		Payload: map[string]any{"method": "item/tool/requestUserInput"},
	}); err != nil {
		t.Fatalf("RecordRuntimeEvent %s: %v", issue.ID, err)
	}
	disp.finishAt(spawnIndex, WorkerResult{Err: errors.New("input required"), Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Blocked) == wantBlocked
	}, time.Second)
}

// TestReconcileInactiveLeavesUnlistedRetryClaimed pins that a retry-queued entry
// survives an inactive reconcile that does not list its issue.
func TestReconcileInactiveLeavesUnlistedRetryClaimed(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        RetryScheduler{MaxBackoff: time.Minute},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()

	issue := tracker.Issue{ID: "ENG-PR1", Identifier: "ENG-PR1", State: "In Progress"}
	if err := o.RequestDispatch(context.Background(), issue, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)
	disp.finishAt(0, WorkerResult{Err: errors.New("transient"), Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Running) == 0 && len(v.Retrying) == 1
	}, time.Second)

	// Inactive listing that omits ENG-PR1 (a pagination-truncated fetch). The
	// retry must be treated as unknown, not released.
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(),
		map[string]tracker.Issue{}, map[string]struct{}{"done": {}}, time.Second); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Retrying) != 1 {
		t.Errorf("Retrying after reconcile omitting the issue = %d; want 1 (unlisted retry must stay claimed)", len(view.Retrying))
	}
	if calls := cleaner.snapshot(); len(calls) != 0 {
		t.Errorf("workspace cleanups = %+v; want none for an unlisted retry", calls)
	}
}

// TestReconcileInactiveLeavesUnlistedBlockedClaimed pins that a blocked entry
// survives an inactive reconcile that does not list its issue.
func TestReconcileInactiveLeavesUnlistedBlockedClaimed(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        RetryScheduler{MaxBackoff: time.Minute},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()

	issue := tracker.Issue{ID: "ENG-PB1", Identifier: "ENG-PB1", State: "Todo"}
	blockIssue(t, o, disp, issue, 0, 1)

	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(),
		map[string]tracker.Issue{}, map[string]struct{}{"done": {}}, time.Second); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Blocked) != 1 {
		t.Errorf("Blocked after reconcile omitting the issue = %d; want 1 (unlisted block must stay claimed)", len(view.Blocked))
	}
	if calls := cleaner.snapshot(); len(calls) != 0 {
		t.Errorf("workspace cleanups = %+v; want none for an unlisted block", calls)
	}
}

// TestReconcileInactiveNonTerminalBlockedReleasesWithoutCleanup pins that a
// blocked entry whose issue IS listed but in a merely-inactive (non-terminal)
// state is released — its claim removed — WITHOUT a workspace cleanup. This is
// the release-but-keep-workspace branch upstream reconcile_blocked_issue_state
// takes for a non-terminal inactive transition; gating the release behind the
// terminal check (rather than releasing on every listed entry) would leave the
// block claimed and pass the rest of the suite.
func TestReconcileInactiveNonTerminalBlockedReleasesWithoutCleanup(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        RetryScheduler{MaxBackoff: time.Minute},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()

	issue := tracker.Issue{ID: "ENG-NB1", Identifier: "ENG-NB1", State: "Todo"}
	blockIssue(t, o, disp, issue, 0, 1)

	// Listed, but in a configured-inactive non-terminal state (Backlog, with
	// only "done" terminal): release the block, keep the workspace.
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		issue.ID: {ID: issue.ID, Identifier: issue.Identifier, State: "Backlog"},
	}, map[string]struct{}{"done": {}}, time.Second); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Blocked) != 0 {
		t.Errorf("Blocked after non-terminal inactive reconcile = %d; want 0 (listed non-terminal block must be released)", len(view.Blocked))
	}
	if calls := cleaner.snapshot(); len(calls) != 0 {
		t.Errorf("workspace cleanups = %+v; want none for a non-terminal release", calls)
	}
}
