package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

func TestReconcileCancelledRunRetainsSessionUsageWithoutHandoff(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	defer cancel()

	iss := tracker.Issue{
		ID:         "4879759235",
		Identifier: "#12",
		URL:        "https://github.com/example/repo/issues/12",
		State:      "aiops:todo",
	}
	recordSessionUsageFixture(t, o, disp, iss)

	inactive := map[string]tracker.Issue{iss.ID: {
		ID:         iss.ID,
		Identifier: iss.Identifier,
		URL:        iss.URL,
		State:      "aiops:human-review",
	}}
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), inactive, normalizedStates([]string{"closed"}), 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait() = %v; want nil", err)
	}
	disp.finishAt(0, WorkerResult{Err: context.Canceled, Elapsed: 2 * time.Second})

	view := waitForEndedSessionUsage(t, o)
	endedIssue := iss
	endedIssue.State = "aiops:human-review"
	assertEndedSessionUsage(t, view.CompletedSessionUsage[0], endedIssue, "reconcile_ineligible", 2)
	if len(view.Completed) != 0 || len(view.AgentHandoffReconcileStopped) != 0 || len(view.Retrying) != 0 {
		t.Fatalf("reconcile bookkeeping = completed:%v handoff:%v retrying:%v; want all empty", view.Completed, view.AgentHandoffReconcileStopped, view.Retrying)
	}
}

func TestFailedRunRetainsSessionUsageBeforeRetry(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{
		Dispatcher: disp,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	defer cancel()

	iss := tracker.Issue{
		ID:         "4879759351",
		Identifier: "#13",
		URL:        "https://github.com/example/repo/issues/13",
		State:      "aiops:todo",
	}
	recordSessionUsageFixture(t, o, disp, iss)
	disp.finishAt(0, WorkerResult{Err: errors.New("runner failed"), Elapsed: 3 * time.Second})

	view := waitForFailedSessionUsage(t, o)
	assertEndedSessionUsage(t, view.CompletedSessionUsage[0], iss, "failed", 3)
	if len(view.Completed) != 0 {
		t.Fatalf("Completed = %v; want empty after failed run", view.Completed)
	}
	if len(view.Retrying) != 1 || view.Retrying[0].Attempt != 1 {
		t.Fatalf("Retrying = %+v; want one attempt-1 retry", view.Retrying)
	}
}

func TestEndedSessionUsageRecordsRunIdentityOnce(t *testing.T) {
	state := NewOrchestratorState(15_000, 1)
	iss := tracker.Issue{ID: "once", Identifier: "#14", State: "aiops:todo"}
	id := IssueID(iss.ID)
	run := runningEntry(t, iss)
	run.CodexInputTokens = 10
	run.CodexOutputTokens = 2
	run.CodexTotalTokens = 15
	state.BeginDispatch(id, run)

	if !state.FinishRunFailed(id, run, time.Second) {
		t.Fatal("FinishRunFailed(first) = false; want true")
	}
	if state.FinishRunFailed(id, run, time.Second) {
		t.Fatal("FinishRunFailed(second) = true; want stale finalization rejected")
	}
	if got := len(state.Snapshot().CompletedSessionUsage); got != 1 {
		t.Fatalf("CompletedSessionUsage rows = %d; want exactly 1", got)
	}
}

func TestEndedSessionUsageSharesRecentCompletedFIFOCap(t *testing.T) {
	state := NewOrchestratorState(15_000, 1)
	state.MaxRecentCompleted = 2
	tests := []struct {
		issueID string
		finish  func(IssueID, *RunningEntry) bool
	}{
		{issueID: "first", finish: func(id IssueID, run *RunningEntry) bool {
			return state.FinishRunFailed(id, run, time.Second)
		}},
		{issueID: "second", finish: func(id IssueID, run *RunningEntry) bool {
			return state.FinishRunReconciledCancelled(id, run, time.Second)
		}},
		{issueID: "third", finish: func(id IssueID, run *RunningEntry) bool {
			return state.FinishRunFailed(id, run, time.Second)
		}},
	}

	for _, tt := range tests {
		iss := tracker.Issue{ID: tt.issueID, Identifier: tt.issueID, State: "active"}
		id := IssueID(iss.ID)
		run := runningEntry(t, iss)
		state.BeginDispatch(id, run)
		if !tt.finish(id, run) {
			t.Fatalf("finish(%q) = false; want true", tt.issueID)
		}
	}

	rows := state.Snapshot().CompletedSessionUsage
	if len(rows) != 2 || rows[0].IssueID != "second" || rows[0].Outcome != "reconcile_ineligible" || rows[1].IssueID != "third" || rows[1].Outcome != "failed" {
		t.Fatalf("CompletedSessionUsage = %+v; want FIFO-capped second/reconcile then third/failed", rows)
	}
}

func recordSessionUsageFixture(t *testing.T, o *Orchestrator, disp *fakeDispatcher, iss tracker.Issue) {
	t.Helper()
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch() = %v; want nil", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	events := []task.RuntimeEvent{
		{
			Event: task.EventWorkflowResolved,
			Payload: map[string]any{
				"source": "file",
				"path":   "/srv/maker/WORKFLOW.md",
			},
		},
		{
			Event: task.EventSessionStarted,
			Payload: map[string]any{
				"session_id":     "thread-12-turn-1",
				"thread_id":      "thread-12",
				"turn_id":        "turn-1",
				"agent_provider": "codex-app-server",
				"agent_model":    "gpt-5.6-sol",
			},
		},
		absoluteUsageEvent(100, 20, 123),
	}
	for _, event := range events {
		if err := o.RecordRuntimeEvent(context.Background(), iss.ID, event); err != nil {
			t.Fatalf("RecordRuntimeEvent(%q) = %v; want nil", event.Event, err)
		}
	}
}

func waitForEndedSessionUsage(t *testing.T, o *Orchestrator) StateView {
	t.Helper()
	var view StateView
	waitFor(t, func() bool {
		var err error
		view, err = o.Snapshot(context.Background())
		return err == nil && len(view.Running) == 0 && len(view.CompletedSessionUsage) == 1
	}, time.Second)
	return view
}

func waitForFailedSessionUsage(t *testing.T, o *Orchestrator) StateView {
	t.Helper()
	var view StateView
	waitFor(t, func() bool {
		var err error
		view, err = o.Snapshot(context.Background())
		return err == nil && len(view.Running) == 0 && len(view.CompletedSessionUsage) == 1 &&
			len(view.Retrying) == 1 && view.Retrying[0].Attempt == 1
	}, time.Second)
	return view
}

func assertEndedSessionUsage(t *testing.T, got SessionUsageView, iss tracker.Issue, outcome string, runtimeSeconds float64) {
	t.Helper()
	wantTokens := (TokensView{InputTokens: 100, OutputTokens: 20, TotalTokens: 123})
	if got.IssueID != IssueID(iss.ID) || got.Identifier != iss.Identifier || got.IssueURL != iss.URL || got.State != iss.State {
		t.Fatalf("session identity = %+v; want issue %s/%s/%s in state %s", got, iss.ID, iss.Identifier, iss.URL, iss.State)
	}
	if got.SessionID != "thread-12-turn-1" || got.WorkflowSource != "file" || got.WorkflowPath != "/srv/maker/WORKFLOW.md" {
		t.Fatalf("session metadata = %+v; want recorded session and workflow", got)
	}
	if got.AgentProvider != "codex-app-server" || got.AgentModel != "gpt-5.6-sol" {
		t.Fatalf("agent metadata = %q/%q; want codex-app-server/gpt-5.6-sol", got.AgentProvider, got.AgentModel)
	}
	if got.Tokens != wantTokens || got.RuntimeSeconds != runtimeSeconds || got.CompletedAt.IsZero() || got.Outcome != outcome {
		t.Fatalf("session usage = %+v; want tokens=%+v runtime=%v nonzero timestamp outcome=%q", got, wantTokens, runtimeSeconds, outcome)
	}
}
