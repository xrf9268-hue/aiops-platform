package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// TestResolveDispatchClaim_Table characterizes every branch of the dispatch
// claim gate extracted from (*dispatchOp).apply: fresh vs tracker-rechecked,
// present vs absent retry entry, consumable due continuation vs non-consumable
// (wrong kind / not due) entry, and the already-claimed deny paths. The
// tracker-rechecked + absent + claimed deny had no dedicated coverage before
// this extraction.
func TestResolveDispatchClaim_Table(t *testing.T) {
	const id = IssueID("ENG-DISP")
	due := time.Now().Add(-time.Hour)
	notDue := time.Now().Add(time.Hour)
	entry := func(kind RetryKind, attempt int, dueAt time.Time) *RetryEntry {
		return &RetryEntry{IssueID: id, Identifier: string(id), Kind: kind, Attempt: attempt, DueAt: dueAt, ContinuationTurnCount: 4}
	}
	cases := []struct {
		name              string
		rechecked         bool
		setup             func(st *OrchestratorState)
		wantConsumed      bool
		wantConsumedKind  RetryKind // checked only when wantConsumed
		wantContAttempt   int
		wantContTurnCount int
		wantDeny          bool
	}{
		{name: "fresh unclaimed proceeds", rechecked: false, setup: func(*OrchestratorState) {}, wantDeny: false},
		{name: "fresh claimed denied", rechecked: false, setup: func(st *OrchestratorState) { st.Claimed[id] = struct{}{} }, wantDeny: true},
		{name: "rechecked absent unclaimed proceeds", rechecked: true, setup: func(*OrchestratorState) {}, wantDeny: false},
		{name: "rechecked absent claimed denied", rechecked: true, setup: func(st *OrchestratorState) { st.Claimed[id] = struct{}{} }, wantDeny: true},
		{name: "rechecked failure retry denied", rechecked: true, setup: func(st *OrchestratorState) {
			st.RetryAttempts[id] = entry(RetryKindFailure, 2, due)
		}, wantDeny: true},
		{name: "rechecked quota retry denied", rechecked: true, setup: func(st *OrchestratorState) {
			st.RetryAttempts[id] = entry(RetryKindQuotaBackoff, 1, due)
		}, wantDeny: true},
		{name: "rechecked due continuation consumed", rechecked: true, setup: func(st *OrchestratorState) {
			st.RetryAttempts[id] = entry(RetryKindContinuation, 3, due)
		}, wantConsumed: true, wantConsumedKind: RetryKindContinuation, wantContAttempt: 3, wantContTurnCount: 4, wantDeny: false},
		{name: "rechecked due continuation attempt zero", rechecked: true, setup: func(st *OrchestratorState) {
			st.RetryAttempts[id] = entry(RetryKindContinuation, 0, due)
		}, wantConsumed: true, wantConsumedKind: RetryKindContinuation, wantContAttempt: 0, wantContTurnCount: 4, wantDeny: false},
		{name: "rechecked not-due continuation denied", rechecked: true, setup: func(st *OrchestratorState) {
			st.RetryAttempts[id] = entry(RetryKindContinuation, 3, notDue)
		}, wantDeny: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := NewOrchestratorState(15000, 10)
			tc.setup(st)
			consumed, contAttempt, contTurnCount, deny := resolveDispatchClaim(st, id, tc.rechecked)
			if deny != tc.wantDeny {
				t.Fatalf("resolveDispatchClaim deny = %v; want %v", deny, tc.wantDeny)
			}
			switch {
			case tc.wantConsumed && (consumed == nil || consumed.Kind != tc.wantConsumedKind):
				t.Fatalf("resolveDispatchClaim consumed = %+v; want a %q entry", consumed, tc.wantConsumedKind)
			case !tc.wantConsumed && consumed != nil:
				t.Fatalf("resolveDispatchClaim consumed = %+v; want nil (no entry consumed)", consumed)
			}
			if contAttempt != tc.wantContAttempt {
				t.Fatalf("resolveDispatchClaim continuationAttempt = %d; want %d", contAttempt, tc.wantContAttempt)
			}
			if contTurnCount != tc.wantContTurnCount {
				t.Fatalf("resolveDispatchClaim continuationTurnCount = %d; want %d", contTurnCount, tc.wantContTurnCount)
			}
		})
	}
}

// cancelAwareDispatcher yields its worker result only after the spawn
// context is canceled plus a teardown delay, modeling a real agent run that
// needs time to wind down (subprocess kill, artifact write) after shutdown
// cancellation. released is closed strictly before the result is delivered,
// so a drain that returned without consuming the result observes it still
// open.
type cancelAwareDispatcher struct {
	teardown time.Duration
	released chan struct{}
}

func (d *cancelAwareDispatcher) Spawn(ctx context.Context, _ tracker.Issue, _ *int, _ DispatchOptions) <-chan WorkerResult {
	out := make(chan WorkerResult, 1)
	go func() {
		<-ctx.Done()
		time.Sleep(d.teardown)
		close(d.released)
		out <- WorkerResult{Err: context.Canceled}
		close(out)
	}()
	return out
}

// TestWaitForWorkers_DrainsInFlightWorkerOnShutdown reproduces the issue
// #1030 SIGTERM path: the actor context is canceled while a worker is
// mid-run and the worker needs teardown time after observing the
// cancellation. WaitForWorkers must block until the worker's result is
// collected — deleting the workerWG handoff in spawn (or restoring the old
// early return in the runCtx.Done fanout branch) makes WaitForWorkers return
// during the teardown window, failing the released-channel assertion below.
func TestWaitForWorkers_DrainsInFlightWorkerOnShutdown(t *testing.T) {
	disp := &cancelAwareDispatcher{teardown: 300 * time.Millisecond, released: make(chan struct{})}
	st := NewOrchestratorState(15000, 100)
	o := New(st, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	ctx, cancel := context.WithCancel(context.Background())
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}

	iss := tracker.Issue{ID: "ENG-drain", Identifier: "ENG-drain", Title: "shutdown drain"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch(%s) = %v; want nil", iss.ID, err)
	}

	// Worker is running (blocked on ctx.Done). Simulate SIGTERM.
	cancel()

	if !o.WaitForWorkers(5 * time.Second) {
		t.Fatalf("WaitForWorkers(5s) = false; want true (worker exits on cancel)")
	}
	select {
	case <-disp.released:
	default:
		t.Fatalf("WaitForWorkers returned before the dispatcher delivered the worker result")
	}
}

// TestWaitForWorkers_TimesOutOnStuckWorker pins the bounded-grace contract:
// a worker that ignores cancellation must not wedge shutdown forever.
func TestWaitForWorkers_TimesOutOnStuckWorker(t *testing.T) {
	disp := &fakeDispatcher{} // never delivers a result
	st := NewOrchestratorState(15000, 100)
	o := New(st, Deps{Dispatcher: disp, Scheduler: RetryScheduler{MaxBackoff: time.Minute}})
	ctx, cancel := context.WithCancel(context.Background())
	go o.Run(ctx)
	if err := o.WaitStarted(context.Background()); err != nil {
		t.Fatalf("WaitStarted: %v", err)
	}
	defer cancel()

	iss := tracker.Issue{ID: "ENG-stuck", Identifier: "ENG-stuck", Title: "stuck worker"}
	if err := o.RequestDispatch(context.Background(), iss, nil); err != nil {
		t.Fatalf("RequestDispatch(%s) = %v; want nil", iss.ID, err)
	}
	if o.WaitForWorkers(50 * time.Millisecond) {
		t.Fatalf("WaitForWorkers(50ms) = true; want false (worker never exits)")
	}
}

func TestCleanTurnBudgetForContinuationBudget(t *testing.T) {
	tests := []struct {
		name          string
		maxTurns      int
		consumedTurns int
		want          int
	}{
		{name: "fresh dispatch gets full issue budget", maxTurns: 7, consumedTurns: 0, want: 7},
		{name: "continuation dispatch gets remaining budget", maxTurns: 7, consumedTurns: 5, want: 2},
		{name: "exhausted budget has no remaining turn", maxTurns: 7, consumedTurns: 7, want: 0},
		{name: "overspent budget has no remaining turn", maxTurns: 7, consumedTurns: 9, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanTurnBudgetForContinuationBudget(tt.maxTurns, tt.consumedTurns); got != tt.want {
				t.Fatalf("cleanTurnBudgetForContinuationBudget(%d, %d) = %d; want %d", tt.maxTurns, tt.consumedTurns, got, tt.want)
			}
		})
	}
}
