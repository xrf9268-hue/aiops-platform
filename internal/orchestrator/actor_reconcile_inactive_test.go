package orchestrator

// actor_reconcile_inactive_test.go pins reconcileInactiveTrackerIssuesOp release
// behaviors the existing reconcile suite left unpinned, before #499 decomposes
// the op's three per-collection loops into helpers. Two classes are covered:
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

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

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
