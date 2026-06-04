package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

func TestReconcileTerminalRunRecordsOperatorTerminalStopAndSuppressesDispatch(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  RetryScheduler{MaxBackoff: time.Minute},
	})
	defer cancel()

	issue := tracker.Issue{ID: "ENG-STOP-1", Identifier: "ENG-STOP-1", State: "In Progress", Title: "stop me"}
	if err := o.RequestDispatch(context.Background(), issue, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	terminal := tracker.Issue{ID: issue.ID, Identifier: issue.Identifier, State: "Canceled", Title: issue.Title}
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		issue.ID: terminal,
	}, normalizedStates([]string{"Canceled"}), 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}
	disp.finishAt(0, WorkerResult{Err: context.Canceled, Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		view, _ := o.Snapshot(context.Background())
		return len(view.Running) == 0 && len(view.OperatorTerminalStops) == 1
	}, time.Second)

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	stop := view.OperatorTerminalStops[0]
	if stop.IssueID != IssueID(issue.ID) || stop.State != "Canceled" {
		t.Fatalf("operator terminal stop = %+v; want issue %s state Canceled", stop, issue.ID)
	}
	if !sawRuntimeEvent(view.RecentEvents, RuntimeEventOperatorTerminalStop, IssueID(issue.ID)) {
		t.Fatalf("RecentEvents = %+v; want operator_terminal_stop for %s", view.RecentEvents, issue.ID)
	}

	reactivated := tracker.Issue{ID: issue.ID, Identifier: issue.Identifier, State: "In Progress", Title: issue.Title}
	if err := o.RequestDispatch(context.Background(), reactivated, nil); !errors.Is(err, ErrNotDispatched) {
		t.Fatalf("dispatch after operator terminal stop err = %v; want ErrNotDispatched", err)
	}
	waitFor(t, func() bool {
		view, _ := o.Snapshot(context.Background())
		return len(view.OperatorTerminalStops) == 1 &&
			view.OperatorTerminalStops[0].SuppressedDispatches == 1 &&
			!view.OperatorTerminalStops[0].FirstSuppressedAt.IsZero()
	}, time.Second)
	view, _ = o.Snapshot(context.Background())
	if got := disp.count(); got != 1 {
		t.Fatalf("dispatcher spawn count = %d; want only original run", got)
	}
	if !sawRuntimeEvent(view.RecentEvents, RuntimeEventOperatorTerminalStopDispatchSuppressed, IssueID(issue.ID)) {
		t.Fatalf("RecentEvents = %+v; want first dispatch suppression event", view.RecentEvents)
	}

	if err := o.RequestDispatch(context.Background(), reactivated, nil); !errors.Is(err, ErrNotDispatched) {
		t.Fatalf("second dispatch after operator terminal stop err = %v; want ErrNotDispatched", err)
	}
	view, _ = o.Snapshot(context.Background())
	if countRuntimeEvents(view.RecentEvents, RuntimeEventOperatorTerminalStopDispatchSuppressed, IssueID(issue.ID)) != 1 {
		t.Fatalf("RecentEvents = %+v; want dispatch suppression event deduped", view.RecentEvents)
	}
}

func TestRetryFireDispatchSuppressedAfterOperatorTerminalStop(t *testing.T) {
	st := NewOrchestratorState(15000, 10)
	id := IssueID("ENG-STOP-RETRY")
	active := tracker.Issue{ID: string(id), Identifier: "ENG-STOP-RETRY", State: "In Progress", Title: "retry after stop"}
	entry := &RetryEntry{
		Issue:      active,
		IssueID:    id,
		Identifier: active.Identifier,
		Attempt:    2,
		DueAt:      time.Now().Add(-time.Second),
		Kind:       RetryKindFailure,
	}
	st.ScheduleRetry(entry)
	st.RecordOperatorTerminalStop(id, tracker.Issue{
		ID:         active.ID,
		Identifier: active.Identifier,
		State:      "Canceled",
		Title:      active.Title,
	}, time.Now().UTC())

	followup := retryFireDispatchTail(st, entry, id, entry.Attempt, &Orchestrator{})
	if followup != nil {
		t.Fatalf("retryFireDispatchTail returned followup after Operator Terminal Stop; want dispatch suppressed")
	}
	if st.IsClaimed(id) {
		t.Fatalf("issue %s is still claimed after retry-fire suppression; want retry claim released", id)
	}
	if _, ok := st.RetryAttempts[id]; ok {
		t.Fatalf("RetryAttempts[%s] still present after retry-fire suppression", id)
	}
	stop, ok := st.LookupOperatorTerminalStop(id)
	if !ok || stop.SuppressedDispatches != 1 || stop.FirstSuppressedAt.IsZero() {
		t.Fatalf("operator terminal stop after retry-fire suppression = %+v ok=%v; want one suppression", stop, ok)
	}
	if !sawRuntimeEvent(st.Snapshot().RecentEvents, RuntimeEventOperatorTerminalStopDispatchSuppressed, id) {
		t.Fatalf("RecentEvents = %+v; want retry-fire dispatch suppression event", st.Snapshot().RecentEvents)
	}
}

func TestFinalizeTerminalSelfStopRecordsStopAndCleansWithoutContinuation(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        &sequenceScheduler{delays: []time.Duration{time.Millisecond}},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()

	issue := tracker.Issue{ID: "ENG-STOP-2", Identifier: "ENG-STOP-2", State: "In Progress", Title: "self stop"}
	const wsPath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-STOP-2"
	dispatchRunningIssue(t, o, disp, issue, wsPath, 1)
	if err := o.RecordRuntimeEvent(context.Background(), issue.ID, task.RuntimeEvent{Event: task.EventTurnCompleted}); err != nil {
		t.Fatalf("RecordRuntimeEvent: %v", err)
	}

	disp.finishAt(0, WorkerResult{
		Elapsed: time.Millisecond,
		IssueExitState: &runner.IssueStateSnapshot{
			Found:    true,
			State:    "Done",
			Active:   false,
			Terminal: true,
		},
	})
	waitFor(t, func() bool {
		view, _ := o.Snapshot(context.Background())
		return len(view.Running) == 0 &&
			len(view.Retrying) == 0 &&
			len(view.OperatorTerminalStops) == 1 &&
			len(cleaner.snapshot()) == 1
	}, time.Second)

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Completed) != 0 {
		t.Fatalf("Completed = %v; want empty because terminal self-stop is not clean continuation", view.Completed)
	}
	if len(view.Retrying) != 0 {
		t.Fatalf("Retrying = %+v; want no continuation after terminal self-stop", view.Retrying)
	}
	calls := cleaner.snapshot()
	if len(calls) != 1 {
		t.Fatalf("workspace cleanups = %+v; want one terminal cleanup", calls)
	}
	if calls[0].IssueID != IssueID(issue.ID) || calls[0].State != "Done" || calls[0].Path != wsPath {
		t.Fatalf("cleanup = %+v; want issue %s state Done path %s", calls[0], issue.ID, wsPath)
	}
}

func TestFinalizeAgentOwnedTerminalHandoffDoesNotSuppressFutureRework(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        &sequenceScheduler{delays: []time.Duration{time.Millisecond}},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()

	issue := tracker.Issue{ID: "ENG-STOP-HANDOFF", Identifier: "ENG-STOP-HANDOFF", State: "In Progress", Title: "handoff"}
	const wsPath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-STOP-HANDOFF"
	dispatchRunningIssue(t, o, disp, issue, wsPath, 1)
	if err := o.RecordRuntimeEvent(context.Background(), issue.ID, task.RuntimeEvent{
		Event: task.EventToolCallMutation,
		Payload: map[string]any{
			"tool":                                  "linear_graphql",
			"operation_field":                       "issueUpdate",
			"current_issue_non_active_state_update": true,
		},
	}); err != nil {
		t.Fatalf("RecordRuntimeEvent(issueUpdate handoff): %v", err)
	}

	disp.finishAt(0, WorkerResult{
		Elapsed: time.Millisecond,
		IssueExitState: &runner.IssueStateSnapshot{
			Found:    true,
			State:    "Done",
			Active:   false,
			Terminal: true,
		},
	})
	waitFor(t, func() bool {
		view, _ := o.Snapshot(context.Background())
		return len(view.Running) == 0 && len(cleaner.snapshot()) == 1
	}, time.Second)

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.OperatorTerminalStops) != 0 {
		t.Fatalf("OperatorTerminalStops = %+v; want empty for agent-owned terminal handoff", view.OperatorTerminalStops)
	}

	rework := tracker.Issue{ID: issue.ID, Identifier: issue.Identifier, State: "In Progress", Title: issue.Title}
	if err := o.RequestDispatch(context.Background(), rework, nil); err != nil {
		t.Fatalf("RequestDispatch after agent-owned terminal handoff = %v; want rework dispatch", err)
	}
	waitFor(t, func() bool { return disp.count() == 2 }, time.Second)
}

func TestFinalizeTerminalSelfStopAfterCommentStillRecordsOperatorStop(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        &sequenceScheduler{delays: []time.Duration{time.Millisecond}},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()

	issue := tracker.Issue{ID: "ENG-STOP-COMMENT", Identifier: "ENG-STOP-COMMENT", State: "In Progress", Title: "operator stop after comment"}
	const wsPath = "/var/aiops/workspaces/acme/repo/linear_issue/ENG-STOP-COMMENT"
	dispatchRunningIssue(t, o, disp, issue, wsPath, 1)
	if err := o.RecordRuntimeEvent(context.Background(), issue.ID, task.RuntimeEvent{
		Event: task.EventToolCallMutation,
		Payload: map[string]any{
			"tool":            "linear_graphql",
			"operation_field": "commentCreate",
		},
	}); err != nil {
		t.Fatalf("RecordRuntimeEvent(commentCreate): %v", err)
	}

	disp.finishAt(0, WorkerResult{
		Elapsed: time.Millisecond,
		IssueExitState: &runner.IssueStateSnapshot{
			Found:    true,
			State:    "Canceled",
			Active:   false,
			Terminal: true,
		},
	})
	waitFor(t, func() bool {
		view, _ := o.Snapshot(context.Background())
		return len(view.Running) == 0 && len(view.OperatorTerminalStops) == 1 && len(cleaner.snapshot()) == 1
	}, time.Second)

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	stop := view.OperatorTerminalStops[0]
	if stop.IssueID != IssueID(issue.ID) || stop.State != "Canceled" {
		t.Fatalf("operator terminal stop = %+v; want issue %s state Canceled", stop, issue.ID)
	}
}

func TestCleanupRecheckActiveAfterOperatorTerminalStopDoesNotResumeContinuation(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        &sequenceScheduler{delays: []time.Duration{time.Millisecond}},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()
	o.SetRetryTerminalStateResolver(staticStateRefresher{"ENG-STOP-3": "In Progress"}, []string{"Done"})

	issue := tracker.Issue{ID: "ENG-STOP-3", Identifier: "ENG-STOP-3", State: "In Progress", Title: "cleanup recheck"}
	if err := o.scheduleContinuationRetry(context.Background(), issue, issue.Identifier, 2, 1, Workspace{
		Path: "/var/aiops/workspaces/acme/repo/linear_issue/ENG-STOP-3",
		Root: testWorkspaceRoot,
	}); err != nil {
		t.Fatalf("scheduleContinuationRetry: %v", err)
	}
	waitFor(t, func() bool {
		view, _ := o.Snapshot(context.Background())
		return len(view.Retrying) == 1
	}, time.Second)

	terminal := tracker.Issue{ID: issue.ID, Identifier: issue.Identifier, State: "Done", Title: issue.Title}
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		issue.ID: terminal,
	}, normalizedStates([]string{"Done"}), 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}
	waitFor(t, func() bool {
		view, _ := o.Snapshot(context.Background())
		return len(view.Retrying) == 0 && len(view.OperatorTerminalStops) == 1
	}, time.Second)
	time.Sleep(25 * time.Millisecond)

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Retrying) != 0 {
		t.Fatalf("Retrying after active cleanup recheck = %+v; want no resumed continuation under sticky stop", view.Retrying)
	}
	if got := disp.count(); got != 0 {
		t.Fatalf("dispatcher count = %d; want no poll-wake dispatch under sticky stop", got)
	}
	if calls := cleaner.snapshot(); len(calls) != 0 {
		t.Fatalf("workspace cleanup calls = %+v; want skip because recheck saw active", calls)
	}
}

func TestNonTerminalInactiveHandoffDoesNotRecordOperatorTerminalStop(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  RetryScheduler{MaxBackoff: time.Minute},
	})
	defer cancel()

	issue := tracker.Issue{ID: "ENG-STOP-4", Identifier: "ENG-STOP-4", State: "In Progress", Title: "handoff"}
	if err := o.RequestDispatch(context.Background(), issue, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	inactive := tracker.Issue{ID: issue.ID, Identifier: issue.Identifier, State: "In Review", Title: issue.Title}
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), map[string]tracker.Issue{
		issue.ID: inactive,
	}, normalizedStates([]string{"Done"}), 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}
	disp.finishAt(0, WorkerResult{Err: context.Canceled, Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		view, _ := o.Snapshot(context.Background())
		return len(view.Running) == 0
	}, time.Second)

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.OperatorTerminalStops) != 0 {
		t.Fatalf("OperatorTerminalStops = %+v; want empty for non-terminal inactive handoff", view.OperatorTerminalStops)
	}
}

func sawRuntimeEvent(events []RuntimeEvent, kind RuntimeEventKind, id IssueID) bool {
	return countRuntimeEvents(events, kind, id) > 0
}

func countRuntimeEvents(events []RuntimeEvent, kind RuntimeEventKind, id IssueID) int {
	count := 0
	for _, ev := range events {
		if ev.Kind == kind && ev.IssueID == id {
			count++
		}
	}
	return count
}
