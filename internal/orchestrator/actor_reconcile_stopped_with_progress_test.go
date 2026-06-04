package orchestrator

// actor_reconcile_stopped_with_progress_test.go pins #557's observability fix: a
// reconcile-stopped run that had completed ≥1 agent turn (made progress — these
// tests simulate the agent's handoff) is surfaced in /api/v1/state's
// ReconcileStoppedWithProgress set, so a progressed-but-reaped run is visible
// instead of absent from Completed. It must NOT be added to
// Completed (a reconcile-stopped run is not a clean §16.5 exit — completed stays
// upstream-aligned), and a 0-turn reconcile cancel (genuine no-progress stop)
// must not be recorded.

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

func recordLinearHandoffMutation(t *testing.T, o *Orchestrator, issueID string) {
	t.Helper()
	err := o.RecordRuntimeEvent(context.Background(), issueID, task.RuntimeEvent{
		Event: task.EventToolCallMutation,
		Payload: map[string]any{
			"tool":            "linear_graphql",
			"operation_field": "issueUpdate",
		},
	})
	if err != nil {
		t.Fatalf("RecordRuntimeEvent(linear_graphql mutation): %v", err)
	}
}

func recordCurrentIssueHandoffMutation(t *testing.T, o *Orchestrator, issueID string) {
	t.Helper()
	err := o.RecordRuntimeEvent(context.Background(), issueID, task.RuntimeEvent{
		Event: task.EventToolCallMutation,
		Payload: map[string]any{
			"tool":                                  "linear_graphql",
			"operation_field":                       "issueUpdate",
			"current_issue_non_active_state_update": true,
		},
	})
	if err != nil {
		t.Fatalf("RecordRuntimeEvent(current issue handoff): %v", err)
	}
}

func recordCurrentIssueTerminalHandoffMutation(t *testing.T, o *Orchestrator, issueID string) {
	t.Helper()
	recordCurrentIssueTerminalHandoffMutationState(t, o, issueID, "Done")
}

func recordCurrentIssueTerminalHandoffMutationState(t *testing.T, o *Orchestrator, issueID, state string) {
	t.Helper()
	payload := map[string]any{
		"tool":                                  "linear_graphql",
		"operation_field":                       "issueUpdate",
		"current_issue_non_active_state_update": true,
		"current_issue_terminal_state_update":   true,
	}
	if state != "" {
		payload["current_issue_terminal_state"] = state
	}
	err := o.RecordRuntimeEvent(context.Background(), issueID, task.RuntimeEvent{
		Event:   task.EventToolCallMutation,
		Payload: payload,
	})
	if err != nil {
		t.Fatalf("RecordRuntimeEvent(current issue terminal handoff): %v", err)
	}
}

// TestReconcileCancelAfterProgressRecordsReconcileStoppedWithProgress: a reconcile-cancel
// of a run that completed a turn is surfaced in ReconcileStoppedWithProgress
// (and the lifetime counter), and is NOT counted as completed or delivered.
func TestReconcileCancelAfterProgressRecordsReconcileStoppedWithProgress(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-RF1", Identifier: "ENG-RF1", State: "In Progress"}
	reconcileCancelRun(t, o, disp, iss, true)

	v, _ := o.Snapshot(context.Background())
	if len(v.ReconcileStoppedWithProgress) != 1 || v.ReconcileStoppedWithProgress[0] != IssueID(iss.ID) {
		t.Fatalf("ReconcileStoppedWithProgress = %v; want [%s]", v.ReconcileStoppedWithProgress, iss.ID)
	}
	if v.CumulativeReconcileStoppedWithProgressTotal != 1 {
		t.Fatalf("CumulativeReconcileStoppedWithProgressTotal = %d; want 1", v.CumulativeReconcileStoppedWithProgressTotal)
	}
	if len(v.Completed) != 0 {
		t.Fatalf("Completed = %v; want empty (a reconcile-stopped run is not a clean §16.5 exit)", v.Completed)
	}
	if len(v.AgentHandoffReconcileStopped) != 0 {
		t.Fatalf("AgentHandoffReconcileStopped = %v; want empty without current-issue handoff", v.AgentHandoffReconcileStopped)
	}
}

func TestReconcileCancelAfterCurrentIssueHandoffWithTurnRecordsAgentHandoff(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-RF6", Identifier: "ENG-RF6", State: "In Progress"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)
	recordCurrentIssueHandoffMutation(t, o, iss.ID)
	if err := o.RecordRuntimeEvent(context.Background(), iss.ID, task.RuntimeEvent{Event: task.EventTurnCompleted}); err != nil {
		t.Fatalf("RecordRuntimeEvent(turn_completed): %v", err)
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

	v, _ := o.Snapshot(context.Background())
	if len(v.AgentHandoffReconcileStopped) != 1 || v.AgentHandoffReconcileStopped[0] != IssueID(iss.ID) {
		t.Fatalf("AgentHandoffReconcileStopped = %v; want [%s]", v.AgentHandoffReconcileStopped, iss.ID)
	}
	if v.CumulativeAgentHandoffReconcileStoppedTotal != 1 {
		t.Fatalf("CumulativeAgentHandoffReconcileStoppedTotal = %d; want 1", v.CumulativeAgentHandoffReconcileStoppedTotal)
	}
	if len(v.ReconcileStoppedWithProgress) != 1 || v.ReconcileStoppedWithProgress[0] != IssueID(iss.ID) {
		t.Fatalf("ReconcileStoppedWithProgress = %v; want [%s]", v.ReconcileStoppedWithProgress, iss.ID)
	}
	if len(v.Completed) != 0 {
		t.Fatalf("Completed = %v; want empty (agent handoff reconcile stop is not a clean §16.5 exit)", v.Completed)
	}
}

func TestReconcileCancelAfterLinearMutationWithoutCurrentIssueHandoffDoesNotRecordAgentHandoff(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-RF4", Identifier: "ENG-RF4", State: "In Progress"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)
	recordLinearHandoffMutation(t, o, iss.ID)

	inactive := map[string]tracker.Issue{iss.ID: {ID: iss.ID, Identifier: iss.Identifier, State: "In Review"}}
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), inactive, normalizedStates([]string{"done"}), 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}
	disp.finishAt(0, WorkerResult{Err: context.Canceled, Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Running) == 0
	}, time.Second)

	v, _ := o.Snapshot(context.Background())
	if len(v.AgentHandoffReconcileStopped) != 0 {
		t.Fatalf("AgentHandoffReconcileStopped = %v; want empty for generic Linear mutation without current-issue handoff", v.AgentHandoffReconcileStopped)
	}
	if v.CumulativeAgentHandoffReconcileStoppedTotal != 0 {
		t.Fatalf("CumulativeAgentHandoffReconcileStoppedTotal = %d; want 0", v.CumulativeAgentHandoffReconcileStoppedTotal)
	}
	if len(v.ReconcileStoppedWithProgress) != 0 {
		t.Fatalf("ReconcileStoppedWithProgress = %v; want empty without turn_completed", v.ReconcileStoppedWithProgress)
	}
	if len(v.Completed) != 0 {
		t.Fatalf("Completed = %v; want empty (reconcile stop is not a clean §16.5 exit)", v.Completed)
	}
}

func TestReconcileCancelAfterCurrentIssueHandoffWithoutTurnRecordsAgentHandoff(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-RF8", Identifier: "ENG-RF8", State: "In Progress"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)
	recordCurrentIssueHandoffMutation(t, o, iss.ID)

	inactive := map[string]tracker.Issue{iss.ID: {ID: iss.ID, Identifier: iss.Identifier, State: "In Review"}}
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), inactive, normalizedStates([]string{"done"}), 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}
	disp.finishAt(0, WorkerResult{Err: context.Canceled, Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Running) == 0
	}, time.Second)

	v, _ := o.Snapshot(context.Background())
	if len(v.AgentHandoffReconcileStopped) != 1 || v.AgentHandoffReconcileStopped[0] != IssueID(iss.ID) {
		t.Fatalf("AgentHandoffReconcileStopped = %v; want [%s]", v.AgentHandoffReconcileStopped, iss.ID)
	}
	if v.CumulativeAgentHandoffReconcileStoppedTotal != 1 {
		t.Fatalf("CumulativeAgentHandoffReconcileStoppedTotal = %d; want 1", v.CumulativeAgentHandoffReconcileStoppedTotal)
	}
	if len(v.ReconcileStoppedWithProgress) != 0 {
		t.Fatalf("ReconcileStoppedWithProgress = %v; want empty without turn_completed", v.ReconcileStoppedWithProgress)
	}
	if len(v.Completed) != 0 {
		t.Fatalf("Completed = %v; want empty (agent handoff reconcile stop is not a clean §16.5 exit)", v.Completed)
	}
}

func TestReconcileCancelAfterCurrentIssueHandoffActiveRefreshDoesNotRecordAgentHandoff(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-RF9", Identifier: "ENG-RF9", State: "In Progress"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)
	recordCurrentIssueHandoffMutation(t, o, iss.ID)

	active := tracker.Issue{ID: iss.ID, Identifier: iss.Identifier, State: "In Progress"}
	if err := o.RefreshActiveTrackerIssues(context.Background(), map[string]tracker.Issue{
		iss.ID: active,
	}, normalizedStates([]string{"In Progress"})); err != nil {
		t.Fatalf("RefreshActiveTrackerIssues: %v", err)
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

	v, _ := o.Snapshot(context.Background())
	if len(v.AgentHandoffReconcileStopped) != 0 {
		t.Fatalf("AgentHandoffReconcileStopped = %v; want empty after active refresh cleared the stale handoff", v.AgentHandoffReconcileStopped)
	}
	if v.CumulativeAgentHandoffReconcileStoppedTotal != 0 {
		t.Fatalf("CumulativeAgentHandoffReconcileStoppedTotal = %d; want 0 after active refresh", v.CumulativeAgentHandoffReconcileStoppedTotal)
	}
	if len(v.ReconcileStoppedWithProgress) != 0 {
		t.Fatalf("ReconcileStoppedWithProgress = %v; want empty without turn_completed", v.ReconcileStoppedWithProgress)
	}
	if len(v.Completed) != 0 {
		t.Fatalf("Completed = %v; want empty", v.Completed)
	}
}

func TestReconcileCancelAfterRejectedAndPostStopMutationsDoesNotRecordAgentHandoff(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-RF5", Identifier: "ENG-RF5", State: "In Progress"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	for _, ev := range []task.RuntimeEvent{
		{
			Event: task.EventToolCallMutationRejected,
			Payload: map[string]any{
				"tool":            "linear_graphql",
				"operation_field": "issueUpdate",
				"reason":          "current_issue_active_state_update",
				"found":           true,
				"state":           "In Progress",
				"terminal":        false,
			},
		},
		{
			Event: task.EventToolCallMutationPostOperatorTerminalStop,
			Payload: map[string]any{
				"tool":            "linear_ai_workpad",
				"operation_field": "commentCreate",
			},
		},
	} {
		if err := o.RecordRuntimeEvent(context.Background(), iss.ID, ev); err != nil {
			t.Fatalf("RecordRuntimeEvent(%s): %v", ev.Event, err)
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

	v, _ := o.Snapshot(context.Background())
	if len(v.AgentHandoffReconcileStopped) != 0 {
		t.Fatalf("AgentHandoffReconcileStopped = %v; want empty for rejected/post-stop audit events", v.AgentHandoffReconcileStopped)
	}
	if len(v.ReconcileStoppedWithProgress) != 0 {
		t.Fatalf("ReconcileStoppedWithProgress = %v; want empty without completed turn", v.ReconcileStoppedWithProgress)
	}
}

// TestReconcileCancelNoTurnDoesNotRecordReconcileStoppedWithProgress: a reconcile-cancel
// before any turn completed is a genuine no-progress stop and must not be
// surfaced as a handoff.
func TestReconcileCancelNoTurnDoesNotRecordReconcileStoppedWithProgress(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-RF2", Identifier: "ENG-RF2", State: "In Progress"}
	reconcileCancelRun(t, o, disp, iss, false)

	v, _ := o.Snapshot(context.Background())
	if len(v.ReconcileStoppedWithProgress) != 0 {
		t.Fatalf("ReconcileStoppedWithProgress = %v; want empty (0-turn cancel is a genuine no-progress stop)", v.ReconcileStoppedWithProgress)
	}
	if v.CumulativeReconcileStoppedWithProgressTotal != 0 {
		t.Fatalf("CumulativeReconcileStoppedWithProgressTotal = %d; want 0", v.CumulativeReconcileStoppedWithProgressTotal)
	}
	if len(v.AgentHandoffReconcileStopped) != 0 {
		t.Fatalf("AgentHandoffReconcileStopped = %v; want empty without Linear handoff activity", v.AgentHandoffReconcileStopped)
	}
}

// TestReconcileTerminalCancelAfterHandoffRecordsReconcileStoppedWithProgress exercises the
// OTHER record site — applyReconciledCancelCleanup, reached when the issue moved
// to a TERMINAL state (ReconcileCleanupWorkspace=true) after a completed turn —
// so deleting that branch's recordReconcileStoppedWithProgress call fails a test.
func TestReconcileTerminalCancelAfterHandoffRecordsReconcileStoppedWithProgress(t *testing.T) {
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
	if len(v.ReconcileStoppedWithProgress) != 1 || v.ReconcileStoppedWithProgress[0] != IssueID(iss.ID) {
		t.Fatalf("ReconcileStoppedWithProgress = %v; want [%s] (terminal cancel after handoff)", v.ReconcileStoppedWithProgress, iss.ID)
	}
	if len(v.Completed) != 0 {
		t.Fatalf("Completed = %v; want empty", v.Completed)
	}
}

func TestReconcileTerminalCancelAfterCurrentIssueHandoffWithTurnRecordsAgentHandoff(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-RF7", Identifier: "ENG-RF7", State: "In Progress"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)
	recordCurrentIssueTerminalHandoffMutation(t, o, iss.ID)
	if err := o.RecordRuntimeEvent(context.Background(), iss.ID, task.RuntimeEvent{Event: task.EventTurnCompleted}); err != nil {
		t.Fatalf("RecordRuntimeEvent(turn_completed): %v", err)
	}

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
	if len(v.AgentHandoffReconcileStopped) != 1 || v.AgentHandoffReconcileStopped[0] != IssueID(iss.ID) {
		t.Fatalf("AgentHandoffReconcileStopped = %v; want [%s]", v.AgentHandoffReconcileStopped, iss.ID)
	}
	if len(v.ReconcileStoppedWithProgress) != 1 || v.ReconcileStoppedWithProgress[0] != IssueID(iss.ID) {
		t.Fatalf("ReconcileStoppedWithProgress = %v; want [%s]", v.ReconcileStoppedWithProgress, iss.ID)
	}
	if len(v.OperatorTerminalStops) != 0 {
		t.Fatalf("OperatorTerminalStops = %+v; want empty for agent-owned terminal handoff", v.OperatorTerminalStops)
	}
	if len(v.Completed) != 0 {
		t.Fatalf("Completed = %v; want empty", v.Completed)
	}

	rework := tracker.Issue{ID: iss.ID, Identifier: iss.Identifier, State: "In Progress"}
	if err := o.RequestDispatch(context.Background(), rework, nil); err != nil {
		t.Fatalf("RequestDispatch after agent-owned terminal handoff = %v; want rework dispatch", err)
	}
	waitFor(t, func() bool { return disp.count() == 2 }, time.Second)
}

// TestRecordReconcileStoppedWithProgressCapsAndDedup pins the FIFO cap + dedup + cumulative
// semantics of recordReconcileStoppedWithProgress (mirrors the Completed-set boundary test):
// the recent set is capped at MaxRecentCompleted with oldest-evicted, a repeat id
// is a no-op for the set/order but still increments the lifetime counter.
func TestRecordReconcileStoppedWithProgressCapsAndDedup(t *testing.T) {
	s := NewOrchestratorState(1000, 4)
	s.MaxRecentCompleted = 2 // shrink the recent cap for the boundary check

	s.recordReconcileStoppedWithProgress("a")
	s.recordReconcileStoppedWithProgress("b")
	if got := len(s.reconcileStoppedWithProgressOrder); got != 2 {
		t.Fatalf("order len at cap = %d; want 2", got)
	}
	// N+1: oldest ("a") evicted from both the order slice and the set.
	s.recordReconcileStoppedWithProgress("c")
	if got := len(s.reconcileStoppedWithProgressOrder); got != 2 {
		t.Fatalf("order len at N+1 = %d; want 2 (capped)", got)
	}
	if _, ok := s.ReconcileStoppedWithProgress["a"]; ok {
		t.Fatalf("ReconcileStoppedWithProgress still has evicted oldest %q; order=%v", "a", s.reconcileStoppedWithProgressOrder)
	}
	if len(s.ReconcileStoppedWithProgress) != len(s.reconcileStoppedWithProgressOrder) {
		t.Fatalf("set/order out of sync: set=%d order=%d", len(s.ReconcileStoppedWithProgress), len(s.reconcileStoppedWithProgressOrder))
	}
	// Dedup: repeating a live id bumps the cumulative counter but not the set.
	s.recordReconcileStoppedWithProgress("c")
	if got := len(s.reconcileStoppedWithProgressOrder); got != 2 {
		t.Fatalf("order len after dedup = %d; want 2", got)
	}
	if got := s.CumulativeReconcileStoppedWithProgressTotal; got != 4 {
		t.Fatalf("CumulativeReconcileStoppedWithProgressTotal = %d; want 4 (a,b,c,c)", got)
	}
}
