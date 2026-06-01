package orchestrator

// actor_reconcile_self_stop_test.go pins the #557 fix: per-tick reconciliation
// must DEFER to a self-stopping runner's SPEC §16.5 self-stop when the agent
// moves its own issue to a non-terminal inactive state (PR handoff, e.g. In
// Review), instead of racing it with a mid-turn hard-cancel that finalizes the
// run as PhaseCanceledByReconciliation (invisible in /api/v1/state). The deferral
// is scoped: only a self-stopping runner (RunnerEnforcesMaxTurns) + a
// non-terminal inactive transition + same service route qualifies; everything
// else keeps the existing prompt hard-cancel.

import (
	"context"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// dispatchActiveRun dispatches one issue and waits until the worker is running,
// returning the fake dispatcher slot index (always 0 for the first dispatch).
func dispatchActiveRun(t *testing.T, o *Orchestrator, disp *fakeDispatcher, iss tracker.Issue) {
	t.Helper()
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)
}

// assertNotCanceled fails if the run's context is cancelled within a short grace
// window; the reconcile wait has already returned, so a clean window means the
// worker was left to self-stop.
func assertNotCanceled(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatal("run was hard-cancelled by reconcile; want deferral to the §16.5 self-stop")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestReconcileInactiveDefersSelfStopRunnerToCleanExit is the core #557 case: a
// self-stopping runner whose issue moved to a non-terminal inactive state is NOT
// cancelled; once the worker self-stops (clean exit), it is recorded as completed
// rather than vanishing as CanceledByReconciliation.
func TestReconcileInactiveDefersSelfStopRunnerToCleanExit(t *testing.T) {
	disp := &fakeDispatcher{}
	enforces := true
	o, cancel := startActor(t, Deps{
		Dispatcher:             disp,
		Scheduler:              RetryScheduler{MaxBackoff: time.Minute},
		RunnerEnforcesMaxTurns: &enforces,
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-SS1", Identifier: "ENG-SS1", State: "In Progress"}
	dispatchActiveRun(t, o, disp, iss)

	// Agent handoff: issue now in a non-terminal inactive state (In Review).
	inactive := map[string]tracker.Issue{iss.ID: {ID: iss.ID, Identifier: iss.Identifier, State: "In Review"}}
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), inactive, normalizedStates([]string{"done"}), 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}

	assertNotCanceled(t, disp.contexts[0])
	v, _ := o.Snapshot(context.Background())
	if len(v.Running) != 1 {
		t.Fatalf("Running after deferral = %d; want 1 (run kept for §16.5 self-stop)", len(v.Running))
	}
	// The deferred run must keep its dispatch-time ACTIVE state, not the inactive
	// handoff state, so per-state capacity gates keep counting it correctly until
	// the worker self-stops (a refresh to "In Review" here would mis-bucket it).
	if got := v.Running[0].State; got != "In Progress" {
		t.Fatalf("deferred run State = %q; want %q (kept at dispatch-time active state for capacity accounting)", got, "In Progress")
	}
	if len(v.Completed) != 0 {
		t.Fatalf("Completed before worker exit = %d; want 0", len(v.Completed))
	}

	// Worker self-stops (SPEC §16.5: refresh saw the inactive issue → clean exit).
	disp.finishAt(0, WorkerResult{Err: nil, Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Running) == 0 && len(v.Completed) == 1
	}, time.Second)
}

// TestReconcileInactiveCancelsNonSelfStopRunner pins that the deferral is scoped
// to self-stopping runners: a one-shot runner (mock / shell claude, no §16.5
// loop) whose issue went non-terminal-inactive is still hard-cancelled, since
// nothing else would stop it.
func TestReconcileInactiveCancelsNonSelfStopRunner(t *testing.T) {
	disp := &fakeDispatcher{}
	enforces := false
	o, cancel := startActor(t, Deps{
		Dispatcher:             disp,
		Scheduler:              RetryScheduler{MaxBackoff: time.Minute},
		RunnerEnforcesMaxTurns: &enforces,
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-SS2", Identifier: "ENG-SS2", State: "In Progress"}
	dispatchActiveRun(t, o, disp, iss)

	inactive := map[string]tracker.Issue{iss.ID: {ID: iss.ID, Identifier: iss.Identifier, State: "In Review"}}
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), inactive, normalizedStates([]string{"done"}), 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}

	select {
	case <-disp.contexts[0].Done():
	case <-time.After(time.Second):
		t.Fatal("non-self-stop run on inactive issue was not cancelled; want hard-cancel")
	}
	disp.finishAt(0, WorkerResult{Err: context.Canceled, Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Running) == 0
	}, time.Second)
}

// TestReconcileInactiveCancelsSelfStopRunnerOnTerminalState pins that the
// deferral is scoped to NON-terminal transitions: a self-stopping runner whose
// issue went to a terminal state (genuinely done/cancelled — typically an
// operator action, not a handoff) is still hard-cancelled promptly.
func TestReconcileInactiveCancelsSelfStopRunnerOnTerminalState(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	enforces := true
	o, cancel := startActor(t, Deps{
		Dispatcher:             disp,
		Scheduler:              RetryScheduler{MaxBackoff: time.Minute},
		RunnerEnforcesMaxTurns: &enforces,
		WorkspaceCleaner:       cleaner,
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-SS3", Identifier: "ENG-SS3", State: "In Progress"}
	dispatchActiveRun(t, o, disp, iss)

	terminal := map[string]tracker.Issue{iss.ID: {ID: iss.ID, Identifier: iss.Identifier, State: "Done"}}
	if err := o.ReconcileInactiveTrackerIssuesAndWait(context.Background(), terminal, normalizedStates([]string{"done"}), 0); err != nil {
		t.Fatalf("ReconcileInactiveTrackerIssuesAndWait: %v", err)
	}

	select {
	case <-disp.contexts[0].Done():
	case <-time.After(time.Second):
		t.Fatal("self-stop run on a TERMINAL issue was not cancelled; want prompt hard-cancel")
	}
	disp.finishAt(0, WorkerResult{Err: context.Canceled, Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Running) == 0
	}, time.Second)
}

// TestReconcileActiveDefersSelfStopRunnerOnNonTerminalInactive covers the
// routing-aware active pass (reconcileTrackerIssuesOp): a self-stopping run whose
// refreshed state left the active set for a non-terminal inactive state on the
// same route is deferred, not cancelled.
func TestReconcileActiveDefersSelfStopRunnerOnNonTerminalInactive(t *testing.T) {
	disp := &fakeDispatcher{}
	enforces := true
	o, cancel := startActor(t, Deps{
		Dispatcher:             disp,
		Scheduler:              RetryScheduler{MaxBackoff: time.Minute},
		RunnerEnforcesMaxTurns: &enforces,
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-SS4", Identifier: "ENG-SS4", State: "In Progress", ServiceName: "api"}
	dispatchActiveRun(t, o, disp, iss)

	// Active listing no longer contains the issue (it left active); the narrow
	// refresh observed it in a non-terminal inactive state on the same route.
	refreshed := map[string]tracker.Issue{iss.ID: {ID: iss.ID, Identifier: iss.Identifier, State: "In Review", ServiceName: "api"}}
	if err := o.ReconcileTrackerIssuesAndWait(context.Background(),
		map[string]tracker.Issue{}, normalizedStates([]string{"in progress"}),
		normalizedStates([]string{"done"}), refreshed, 0); err != nil {
		t.Fatalf("ReconcileTrackerIssuesAndWait: %v", err)
	}

	assertNotCanceled(t, disp.contexts[0])
	v, _ := o.Snapshot(context.Background())
	if len(v.Running) != 1 {
		t.Fatalf("Running after active-pass deferral = %d; want 1", len(v.Running))
	}
}
