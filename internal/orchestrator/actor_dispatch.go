package orchestrator

// actor_dispatch.go holds the dispatch path: the public RequestDispatch
// entry points, the dispatchOp stateOp, and the spawn seam that launches a
// worker for a claimed issue. See actor.go for the actor's mutation discipline.

import (
	"context"
	"errors"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// ErrNotDispatched is returned by RequestDispatch when the issue was
// already claimed (running, retry-queued, or otherwise reserved) and
// dispatch was therefore deduped. It is not an error condition — SPEC
// §7.4's duplicate-dispatch guard is doing its job — but callers
// distinguish "rejected" from "succeeded" by inspecting it.
var ErrNotDispatched = errors.New("orchestrator: issue already claimed")

// ErrCapacityFull is returned by RequestDispatch when the issue is eligible and
// unclaimed, but the orchestrator is already running the configured maximum
// number of agents. Callers should keep the issue eligible for a future poll
// tick rather than treating it as a duplicate dispatch.
var ErrCapacityFull = errors.New("orchestrator: max_concurrent_agents reached")

// RequestDispatch is the public entry to dispatch issue if no other
// claim exists. It returns nil on accepted dispatch (a worker is being
// spawned) and ErrNotDispatched if the actor saw an existing claim.
//
// Dispatch decisions are serialized through the actor: concurrent calls
// for the same issue produce at most one Running entry, even when many
// goroutines race on the same id.
func (o *Orchestrator) RequestDispatch(ctx context.Context, issue tracker.Issue, attempt *int) error {
	reply := make(chan error, 1)
	op := &dispatchOp{o: o, issue: issue, attempt: attempt, result: reply}
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (o *Orchestrator) RequestDispatchAfterTrackerRecheck(ctx context.Context, issue tracker.Issue, attempt *int) error {
	reply := make(chan error, 1)
	op := &dispatchOp{o: o, issue: issue, attempt: attempt, result: reply, trackerRechecked: true}
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// dispatchOp is the actor-side half of RequestDispatch: it checks
// IsClaimed and either reserves the slot (followup spawns + records
// Running) or signals dispatch denied. The two-step design keeps the
// dispatch decision atomic against concurrent claims while keeping I/O
// off the actor goroutine.
type dispatchOp struct {
	o                *Orchestrator
	issue            tracker.Issue
	attempt          *int
	result           chan<- error
	trackerRechecked bool
}

func (d *dispatchOp) apply(st *OrchestratorState) func() {
	id := IssueID(d.issue.ID)
	if st.IsCleaningWorkspace(id) {
		// A terminal-transition workspace cleanup is in flight for this issue
		// (SPEC §18.1). Deny dispatch so a re-opened issue cannot land on the
		// deterministic workspace path while before_remove/SafeRemove runs; the
		// next poll tick retries once the cleanup clears the mark.
		d.result <- ErrNotDispatched
		return nil
	}
	consumedContinuation, continuationAttempt, deny := resolveDispatchClaim(st, id, d.trackerRechecked)
	if deny {
		d.result <- ErrNotDispatched
		return nil
	}
	if st.RunningCount() >= st.MaxConcurrentAgents {
		d.result <- ErrCapacityFull
		return nil
	}
	capacityExcluded := IssueID("")
	if consumedContinuation != nil {
		capacityExcluded = id
	}
	if st.StateCapacityFullExcluding(d.issue.State, capacityExcluded) {
		d.result <- ErrCapacityFull
		return nil
	}
	if consumedContinuation != nil {
		if consumedContinuation.Timer != nil {
			consumedContinuation.Timer.Stop()
		}
		delete(st.RetryAttempts, id)
		delete(st.Claimed, id)
	}
	st.RecordEvent(RuntimeEvent{
		Kind:       RuntimeEventCandidate,
		IssueID:    id,
		Identifier: d.issue.Identifier,
		Message:    "candidate fetched from tracker",
	})
	// Reserve the slot synchronously so a concurrent dispatchOp aborts
	// on its IsClaimed check. The followup records Running once the
	// worker is spawned.
	st.Claimed[id] = struct{}{}
	st.ClaimedIssues[id] = d.issue
	o := d.o
	issue := d.issue
	attempt := d.attempt
	result := d.result
	return func() {
		o.spawn(id, issue, attempt, continuationAttempt)
		result <- nil
	}
}

// resolveDispatchClaim decides whether a dispatchOp may proceed past the claim
// gate, and (for a tracker-rechecked dispatch) which queued retry entry it
// consumes. A fresh dispatch is denied only when the issue is already claimed.
// A tracker-rechecked dispatch consumes a DUE continuation entry (returned so the
// caller can clear it, carrying its turn count forward as continuationAttempt);
// it is denied when the queued entry is any other kind (e.g. a failure retry,
// which stays claimed until retryFireOp re-dispatches it) or not yet due, and —
// when no such entry exists — when the issue is already claimed. deny is true on
// every rejection path.
func resolveDispatchClaim(st *OrchestratorState, id IssueID, trackerRechecked bool) (consumed *RetryEntry, continuationAttempt int, deny bool) {
	if !trackerRechecked {
		return nil, 0, st.IsClaimed(id)
	}
	entry, ok := st.RetryAttempts[id]
	if !ok {
		return nil, 0, st.IsClaimed(id)
	}
	if entry.Kind != RetryKindContinuation {
		return nil, 0, true
	}
	if !entry.IsDue(time.Now()) {
		return nil, 0, true
	}
	return entry, entry.Attempt, false
}

// spawn asks the dispatcher for a worker, records the Running entry
// through the actor, and starts the watcher goroutine that submits
// finalizeRunOp on worker exit. The caller must already hold the
// Claimed slot for id (set by dispatchOp.apply or persisted across
// retryFireOp.apply); spawn does not check IsClaimed.
//
// spawn is invoked from a followup goroutine, never from inside an
// apply method, so its calls into o.submit are safe.
func (o *Orchestrator) spawn(id IssueID, issue tracker.Issue, attempt *int, continuationAttempt int) {
	runCtx, cancel := context.WithCancelCause(o.runCtx)
	startedAt := time.Now()
	workerDone := make(chan struct{})
	entry := &RunningEntry{
		Issue:               issue,
		Identifier:          issue.Identifier,
		StartedAt:           startedAt,
		RetryAttempt:        attempt,
		ContinuationAttempt: continuationAttempt,
		CancelWorker:        cancel,
		Done:                workerDone,
	}
	registered := make(chan struct{})
	if err := o.submit(o.runCtx, opFunc(func(st *OrchestratorState) func() {
		st.Running[id] = entry
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventRunning, IssueID: id, Identifier: issue.Identifier, Message: "dispatched to agent", At: startedAt})
		close(registered)
		return nil
	})); err != nil {
		cancel(nil)
		close(workerDone)
		return
	}
	select {
	case <-registered:
	case <-o.runCtx.Done():
		cancel(nil)
		close(workerDone)
		return
	}
	resultCh := o.dispatcher.Spawn(runCtx, issue, attempt)
	go func() {
		defer recoverPanic("orchestrator.spawn_result_fanout")
		var res WorkerResult
		select {
		case r, ok := <-resultCh:
			cancel(nil)
			if ok {
				res = r
			} else {
				// Dispatcher closed without yielding a result: treat
				// as a cancellation, which becomes an abnormal exit
				// and triggers a retry per SPEC §7.3.
				res = WorkerResult{Err: context.Canceled, Elapsed: time.Since(startedAt)}
			}
		case <-o.runCtx.Done():
			cancel(nil)
			close(workerDone)
			return
		}
		// workerDone is closed in exactly one path: either by
		// finalizeRunOp.apply once the actor accepts this submit, or by this
		// goroutine when submit fails because o.runCtx was canceled before
		// the actor accepted the op (typical SIGTERM shutdown race).
		// Dropping the submit error here would leak the close and stall every
		// consumer waiting on entry.Done — including reconcile termination and
		// the graceful-shutdown drain path.
		if err := o.submit(o.runCtx, &finalizeRunOp{
			o:          o,
			id:         id,
			issue:      issue,
			identifier: issue.Identifier,
			attempt:    attempt,
			result:     res,
			started:    startedAt,
			entry:      entry,
			done:       workerDone,
		}); err != nil {
			close(workerDone)
		}
	}()
}
