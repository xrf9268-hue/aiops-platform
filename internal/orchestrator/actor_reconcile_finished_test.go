package orchestrator

// actor_reconcile_finished_test.go pins #557's observability fix: a reconcile-
// stopped run that had handed off (completed ≥1 agent turn) is surfaced in
// /api/v1/state's ReconcileFinished set, so a successful-but-reaped handoff is
// visible instead of absent from both Completed and Failed. It must NOT be added
// to Completed (a reconcile-stopped run is not a clean §16.5 exit — completed
// stays upstream-aligned), and a 0-turn reconcile cancel (genuine no-progress
// stop) must not be recorded.

import (
	"context"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// reconcileCancelRun dispatches one issue, optionally records a completed turn,
// then reconcile-cancels it (issue observed inactive) and drives the worker exit.
func reconcileCancelRun(t *testing.T, o *Orchestrator, disp *fakeDispatcher, iss tracker.Issue, completedTurn bool) {
	t.Helper()
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	if completedTurn {
		// turn_completed bumps Session.TurnCount → runHasCompletedTurn is true
		// (the agent did work / handed off before reconcile reaped the run).
		if err := o.RecordRuntimeEvent(context.Background(), iss.ID, task.RuntimeEvent{Event: task.EventTurnCompleted}); err != nil {
			t.Fatalf("RecordRuntimeEvent: %v", err)
		}
	}

	inactive := map[string]tracker.Issue{iss.ID: {ID: iss.ID, Identifier: iss.Identifier, State: "In Review"}}
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), inactive, normalizedStates([]string{"done"}), 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}
	disp.finishAt(0, WorkerResult{Err: context.Canceled, Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Running) == 0
	}, time.Second)
}

// TestReconcileCancelAfterHandoffRecordsReconcileFinished: a reconcile-cancel of
// a run that completed a turn is surfaced in ReconcileFinished (and the lifetime
// counter), and is NOT counted as completed.
func TestReconcileCancelAfterHandoffRecordsReconcileFinished(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-RF1", Identifier: "ENG-RF1", State: "In Progress"}
	reconcileCancelRun(t, o, disp, iss, true)

	v, _ := o.Snapshot(context.Background())
	if len(v.ReconcileFinished) != 1 || v.ReconcileFinished[0] != IssueID(iss.ID) {
		t.Fatalf("ReconcileFinished = %v; want [%s]", v.ReconcileFinished, iss.ID)
	}
	if v.CumulativeReconcileFinishedTotal != 1 {
		t.Fatalf("CumulativeReconcileFinishedTotal = %d; want 1", v.CumulativeReconcileFinishedTotal)
	}
	if len(v.Completed) != 0 {
		t.Fatalf("Completed = %v; want empty (a reconcile-stopped run is not a clean §16.5 exit)", v.Completed)
	}
}

// TestReconcileCancelNoTurnDoesNotRecordReconcileFinished: a reconcile-cancel
// before any turn completed is a genuine no-progress stop and must not be
// surfaced as a handoff.
func TestReconcileCancelNoTurnDoesNotRecordReconcileFinished(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-RF2", Identifier: "ENG-RF2", State: "In Progress"}
	reconcileCancelRun(t, o, disp, iss, false)

	v, _ := o.Snapshot(context.Background())
	if len(v.ReconcileFinished) != 0 {
		t.Fatalf("ReconcileFinished = %v; want empty (0-turn cancel is a genuine no-progress stop)", v.ReconcileFinished)
	}
	if v.CumulativeReconcileFinishedTotal != 0 {
		t.Fatalf("CumulativeReconcileFinishedTotal = %d; want 0", v.CumulativeReconcileFinishedTotal)
	}
}

// TestReconcileTerminalCancelAfterHandoffRecordsReconcileFinished exercises the
// OTHER record site — applyReconciledCancelCleanup, reached when the issue moved
// to a TERMINAL state (ReconcileCleanupWorkspace=true) after a completed turn —
// so deleting that branch's recordReconcileFinished call fails a test.
func TestReconcileTerminalCancelAfterHandoffRecordsReconcileFinished(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-RF3", Identifier: "ENG-RF3", State: "In Progress"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)
	if err := o.RecordRuntimeEvent(context.Background(), iss.ID, task.RuntimeEvent{Event: task.EventTurnCompleted}); err != nil {
		t.Fatalf("RecordRuntimeEvent: %v", err)
	}
	// Terminal transition → reconcileInactiveRun sets ReconcileCleanupWorkspace,
	// routing finalize through applyReconciledCancelCleanup.
	terminal := map[string]tracker.Issue{iss.ID: {ID: iss.ID, Identifier: iss.Identifier, State: "Done"}}
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), terminal, normalizedStates([]string{"done"}), 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}
	disp.finishAt(0, WorkerResult{Err: context.Canceled, Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Running) == 0
	}, time.Second)

	v, _ := o.Snapshot(context.Background())
	if len(v.ReconcileFinished) != 1 || v.ReconcileFinished[0] != IssueID(iss.ID) {
		t.Fatalf("ReconcileFinished = %v; want [%s] (terminal cancel after handoff)", v.ReconcileFinished, iss.ID)
	}
	if len(v.Completed) != 0 {
		t.Fatalf("Completed = %v; want empty", v.Completed)
	}
}

// TestRecordReconcileFinishedCapsAndDedup pins the FIFO cap + dedup + cumulative
// semantics of recordReconcileFinished (mirrors the Completed-set boundary test):
// the recent set is capped at MaxRecentCompleted with oldest-evicted, a repeat id
// is a no-op for the set/order but still increments the lifetime counter.
func TestRecordReconcileFinishedCapsAndDedup(t *testing.T) {
	s := NewOrchestratorState(1000, 4)
	s.MaxRecentCompleted = 2 // shrink the recent cap for the boundary check

	s.recordReconcileFinished("a")
	s.recordReconcileFinished("b")
	if got := len(s.reconcileFinishedOrder); got != 2 {
		t.Fatalf("order len at cap = %d; want 2", got)
	}
	// N+1: oldest ("a") evicted from both the order slice and the set.
	s.recordReconcileFinished("c")
	if got := len(s.reconcileFinishedOrder); got != 2 {
		t.Fatalf("order len at N+1 = %d; want 2 (capped)", got)
	}
	if _, ok := s.ReconcileFinished["a"]; ok {
		t.Fatalf("ReconcileFinished still has evicted oldest %q; order=%v", "a", s.reconcileFinishedOrder)
	}
	if len(s.ReconcileFinished) != len(s.reconcileFinishedOrder) {
		t.Fatalf("set/order out of sync: set=%d order=%d", len(s.ReconcileFinished), len(s.reconcileFinishedOrder))
	}
	// Dedup: repeating a live id bumps the cumulative counter but not the set.
	s.recordReconcileFinished("c")
	if got := len(s.reconcileFinishedOrder); got != 2 {
		t.Fatalf("order len after dedup = %d; want 2", got)
	}
	if got := s.CumulativeReconcileFinishedTotal; got != 4 {
		t.Fatalf("CumulativeReconcileFinishedTotal = %d; want 4 (a,b,c,c)", got)
	}
}
