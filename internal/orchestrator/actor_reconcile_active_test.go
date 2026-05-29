package orchestrator

// actor_reconcile_active_test.go pins reconcileTrackerIssuesOp (the per-tick
// active-revalidation pass) keep-vs-release branches the existing suite left
// unpinned, before #499 decomposes its three per-collection loops into helpers.
// A mutation audit showed the running route-change/refresh and retry
// route-change branches are covered, but two were not: a running entry observed
// in a NON-ACTIVE state must be cancelled (the isActiveTrackerState gate — the
// documented "cancels workers whose issues moved out of active states"), and a
// BLOCKED entry whose route changes must be released (parity with the covered
// running/retry route-change tests).

import (
	"context"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// TestReconcileTrackerIssuesCancelsRunMovedToNonActiveState pins the
// isActiveTrackerState gate: a running entry still listed on the same route but
// whose state is no longer active must have its worker cancelled.
func TestReconcileTrackerIssuesCancelsRunMovedToNonActiveState(t *testing.T) {
	disp := &fakeDispatcher{}
	o, cancel := startActor(t, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-NA1", Identifier: "ENG-NA1", State: "In Progress"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch: %v", err)
	}
	waitFor(t, func() bool { return disp.count() == 1 }, time.Second)

	// Same id and route, but moved to a non-active state — must cancel.
	active := map[string]tracker.Issue{iss.ID: {ID: iss.ID, Identifier: iss.Identifier, State: "Backlog"}}
	if err := o.ReconcileTrackerIssues(context.Background(), active, normalizedStates([]string{"In Progress"})); err != nil {
		t.Fatalf("ReconcileTrackerIssues: %v", err)
	}
	select {
	case <-disp.contexts[0].Done():
	case <-time.After(time.Second):
		t.Fatal("run moved to a non-active state was not canceled")
	}
	disp.finishAt(0, WorkerResult{Err: context.Canceled, Elapsed: time.Millisecond})
	waitFor(t, func() bool {
		v, _ := o.Snapshot(context.Background())
		return len(v.Running) == 0
	}, time.Second)
}

// TestReconcileTrackerIssuesReleasesBlockedWhenServiceRouteChanges pins the
// blocked route-change release: a blocked entry still in an active state but now
// routed to a different service is released (kept-claimed only on same route).
func TestReconcileTrackerIssuesReleasesBlockedWhenServiceRouteChanges(t *testing.T) {
	disp := &fakeDispatcher{}
	cleaner := &recordingWorkspaceCleaner{}
	o, cancel := startActor(t, Deps{
		Dispatcher:       disp,
		Scheduler:        RetryScheduler{MaxBackoff: time.Minute},
		WorkspaceCleaner: cleaner,
	})
	defer cancel()

	iss := tracker.Issue{ID: "ENG-BR1", Identifier: "ENG-BR1", State: "AI Ready", ServiceName: "api"}
	blockIssue(t, o, disp, iss, 0, 1)

	// Same active state, different service route -> release (route changed);
	// no refreshedByID, so this is a non-terminal release that keeps the
	// workspace.
	active := map[string]tracker.Issue{
		iss.ID: {ID: iss.ID, Identifier: iss.Identifier, State: "AI Ready", ServiceName: "web"},
	}
	if err := o.ReconcileTrackerIssuesAndWait(context.Background(), active,
		map[string]struct{}{"ai ready": {}}, nil, nil, time.Second); err != nil {
		t.Fatalf("ReconcileTrackerIssuesAndWait: %v", err)
	}

	view, err := o.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(view.Blocked) != 0 {
		t.Errorf("Blocked after route-change reconcile = %d; want 0 (route-changed block released)", len(view.Blocked))
	}
	if calls := cleaner.snapshot(); len(calls) != 0 {
		t.Errorf("workspace cleanups = %+v; want none (route change keeps the workspace)", calls)
	}
}
