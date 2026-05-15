package orchestrator

// actor.go is the SPEC §7.4 "single authority" goroutine that serializes
// every mutation against OrchestratorState. The pure data types and
// transition methods live in state.go; this file owns the goroutine,
// the seams the actor uses to spawn workers and pick retry delays, and
// the small set of public entry points (RequestDispatch, ScheduleRetry,
// Snapshot) that callers from outside the actor use to drive it.
//
// Mutation discipline is supplied by an unbuffered ops channel and one
// reader (Run). Every mutation is a stateOp; apply runs on the actor
// goroutine with exclusive access to the state. Long side-effects
// (timer setup, dispatcher.Spawn, follow-up state mutations) belong in
// the followup func returned by apply, which the actor launches on a
// fresh goroutine after apply returns. apply MUST NOT call submit from
// inside itself — the actor is the same goroutine reading from ops and
// would deadlock against itself.
//
// The Elixir reference uses one GenServer per orchestrator with every
// mutation flowing through handle_call / handle_cast / handle_info
// (orchestrator.ex:6,52,74-217); the Go actor here is the direct analog.
//
// PR 2 of the D21+D6 migration plan only ships this actor, the
// Dispatcher seam, and the FixedDelayScheduler matching the legacy
// queue's 60-second behavior. The poll-tick loop (PR 3) and the worker
// rewire (PR 4) layer on top without further changes to the actor's
// shape; D16 swaps FixedDelayScheduler for the SPEC §8.4 exponential
// formula by replacing the Scheduler binding only.

import (
	"context"
	"errors"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// WorkerResult is the per-run outcome the Dispatcher delivers when its
// spawned worker exits. Err is nil for SPEC §7.3 normal exit; any
// non-nil Err is treated as abnormal exit and triggers
// ScheduleRetry(attempt+1). Elapsed is folded into
// CodexTotals.SecondsRunning per SPEC §13.3.
type WorkerResult struct {
	Err     error
	Elapsed time.Duration
}

// Dispatcher is the seam through which the actor spawns a per-issue
// worker goroutine. The returned channel must yield exactly one
// WorkerResult and then close (or close without yielding to signal
// cancellation). The actor watches it to drive normal/abnormal exit
// transitions.
//
// PR 2 only ships the seam — the production binding to internal/worker
// arrives in PR 4 of the migration plan
// (docs/design/d21-d6-orchestrator-state.md).
type Dispatcher interface {
	Spawn(ctx context.Context, issue tracker.Issue, attempt *int) <-chan WorkerResult
}

// Scheduler computes the delay before the next retry of a given
// 1-based attempt counter. PR 2 ships FixedDelayScheduler matching the
// legacy queue's 60-second retry so behavior is unchanged when the
// worker migrates to the actor; D16 (#90) swaps in the SPEC §8.4
// exponential-backoff formula by replacing the Scheduler binding only.
type Scheduler interface {
	NextDelay(attempt int) time.Duration
}

// FixedDelayScheduler returns the same delay regardless of attempt.
// Matches the existing internal/queue/postgres.go retry interval so the
// PR 4 cutover is observably equivalent.
type FixedDelayScheduler struct {
	Delay time.Duration
}

// NextDelay implements Scheduler.
func (f FixedDelayScheduler) NextDelay(int) time.Duration { return f.Delay }

// Deps bundles construction-time dependencies so adding a new one
// later doesn't ripple through every call site.
type Deps struct {
	Dispatcher Dispatcher
	Scheduler  Scheduler
}

// Orchestrator is the SPEC §3.1 / §7.4 "single authority" that owns
// OrchestratorState. All mutations flow through the actor goroutine
// started by Run; reads via Snapshot also serialize through the actor
// so callers always observe a consistent view.
type Orchestrator struct {
	ops chan stateOp

	state      *OrchestratorState
	dispatcher Dispatcher
	scheduler  Scheduler

	// runCtx is captured by Run so followup goroutines can cancel
	// their work when the actor stops. Set once at the top of Run
	// before close(started); reads from outside the actor synchronize
	// via the started channel close.
	runCtx  context.Context
	started chan struct{}
}

// New constructs an Orchestrator over an existing state value. Callers
// must call Run(ctx) in a separate goroutine before the actor processes
// any submitted op; tests can wait via WaitStarted.
func New(state *OrchestratorState, deps Deps) *Orchestrator {
	return &Orchestrator{
		ops:        make(chan stateOp),
		state:      state,
		dispatcher: deps.Dispatcher,
		scheduler:  deps.Scheduler,
		started:    make(chan struct{}),
	}
}

// Run is the actor loop. It drains the ops channel until ctx is
// cancelled, applying each op against the state and (if the op
// returns one) launching its followup on a fresh goroutine so the
// followup can do I/O or submit further ops without deadlocking.
//
// Run must be called exactly once per Orchestrator and exits when ctx
// is cancelled. Pending submits at that point return ctx.Err() via
// the caller's context, not the orchestrator's — see submit.
func (o *Orchestrator) Run(ctx context.Context) {
	o.runCtx = ctx
	close(o.started)
	for {
		select {
		case <-ctx.Done():
			return
		case op := <-o.ops:
			if followup := op.apply(o.state); followup != nil {
				go followup()
			}
		}
	}
}

// WaitStarted blocks until Run has begun executing or ctx is cancelled.
// Tests use it to avoid races between setup goroutines and the actor's
// initialization; production callers usually construct + Run together.
func (o *Orchestrator) WaitStarted(ctx context.Context) error {
	select {
	case <-o.started:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// submit delivers op to the actor. It blocks until the actor reads the
// op or ctx is cancelled. Callers outside the actor (tests, timer
// callbacks, follow-up closures) use this helper. The actor itself
// MUST NOT call submit from inside an apply method — the unbuffered
// ops channel has exactly one reader (the actor goroutine), and a
// re-entrant send would deadlock against it.
func (o *Orchestrator) submit(ctx context.Context, op stateOp) error {
	select {
	case o.ops <- op:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// stateOp is one mutation against OrchestratorState. apply runs on the
// actor goroutine; the optional followup runs on a fresh goroutine
// after apply returns. apply MUST NOT block on the ops channel.
type stateOp interface {
	apply(*OrchestratorState) (followup func())
}

// opFunc adapts a function literal to stateOp so simple ops don't need
// a struct just to carry their behavior.
type opFunc func(*OrchestratorState) func()

func (f opFunc) apply(st *OrchestratorState) func() { return f(st) }

// Snapshot returns a SPEC §13.3-shaped view of the orchestrator state.
// The snapshot is taken on the actor goroutine so it observes a
// consistent state between mutations. Returns ctx.Err() if ctx is
// cancelled before the actor produces the view.
func (o *Orchestrator) Snapshot(ctx context.Context) (StateView, error) {
	reply := make(chan StateView, 1)
	op := opFunc(func(st *OrchestratorState) func() {
		reply <- st.Snapshot()
		return nil
	})
	if err := o.submit(ctx, op); err != nil {
		return StateView{}, err
	}
	select {
	case v := <-reply:
		return v, nil
	case <-ctx.Done():
		return StateView{}, ctx.Err()
	}
}

// ErrNotDispatched is returned by RequestDispatch when the issue was
// already claimed (running, retry-queued, or otherwise reserved) and
// dispatch was therefore deduped. It is not an error condition — SPEC
// §7.4's duplicate-dispatch guard is doing its job — but callers
// distinguish "rejected" from "succeeded" by inspecting it.
var ErrNotDispatched = errors.New("orchestrator: issue already claimed")

// RequestDispatch is the public entry to dispatch issue if no other
// claim exists. It returns nil on accepted dispatch (a worker is being
// spawned) and ErrNotDispatched if the actor saw an existing claim.
//
// Dispatch decisions are serialized through the actor: concurrent calls
// for the same issue produce at most one Running entry, even when many
// goroutines race on the same id.
func (o *Orchestrator) RequestDispatch(ctx context.Context, issue tracker.Issue, attempt *int) error {
	reply := make(chan bool, 1)
	op := &dispatchOp{o: o, issue: issue, attempt: attempt, accepted: reply}
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	select {
	case ok := <-reply:
		if !ok {
			return ErrNotDispatched
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ScheduleRetry enters the SPEC §7.1 retry-queued substate for issue.
// The orchestrator picks a delay via Scheduler.NextDelay(attempt),
// stores a RetryEntry under RetryAttempts, holds the Claimed slot so
// concurrent ticks cannot dispatch the issue, and starts a timer that
// re-dispatches through the actor when it fires.
//
// The 1-based attempt counter is the attempt number this retry will
// run as (i.e. the prior run was attempt-1, or 0 for first-run).
func (o *Orchestrator) ScheduleRetry(ctx context.Context, issue tracker.Issue, identifier string, attempt int, runErr string) error {
	delay := o.scheduler.NextDelay(attempt)
	op := &scheduleRetryOp{
		o:          o,
		issue:      issue,
		identifier: identifier,
		attempt:    attempt,
		delay:      delay,
		runErr:     runErr,
	}
	return o.submit(ctx, op)
}

// dispatchOp is the actor-side half of RequestDispatch: it checks
// IsClaimed and either reserves the slot (followup spawns + records
// Running) or signals dispatch denied. The two-step design keeps the
// dispatch decision atomic against concurrent claims while keeping I/O
// off the actor goroutine.
type dispatchOp struct {
	o        *Orchestrator
	issue    tracker.Issue
	attempt  *int
	accepted chan<- bool
}

func (d *dispatchOp) apply(st *OrchestratorState) func() {
	id := IssueID(d.issue.ID)
	if st.IsClaimed(id) {
		d.accepted <- false
		return nil
	}
	// Reserve the slot synchronously so a concurrent dispatchOp aborts
	// on its IsClaimed check. The followup records Running once the
	// worker is spawned.
	st.Claimed[id] = struct{}{}
	o := d.o
	issue := d.issue
	attempt := d.attempt
	accepted := d.accepted
	return func() {
		o.spawn(id, issue, attempt)
		accepted <- true
	}
}

// scheduleRetryOp is the actor-side half of ScheduleRetry: it stores
// the RetryEntry through OrchestratorState.ScheduleRetry (which stops
// any prior timer for the same id) and starts a new timer whose
// callback submits a retryFireOp.
type scheduleRetryOp struct {
	o          *Orchestrator
	issue      tracker.Issue
	identifier string
	attempt    int
	delay      time.Duration
	runErr     string
}

func (s *scheduleRetryOp) apply(st *OrchestratorState) func() {
	id := IssueID(s.issue.ID)
	o := s.o
	issue := s.issue
	attempt := s.attempt
	// time.AfterFunc schedules immediately and is cheap (no goroutine
	// until fire), so we can safely create the timer on the actor
	// without blocking. ScheduleRetry needs the Timer set on the entry
	// before storing so a stale prior timer is stopped atomically.
	entry := &RetryEntry{
		IssueID:    id,
		Identifier: s.identifier,
		Attempt:    attempt,
		DueAt:      time.Now().Add(s.delay),
		Error:      s.runErr,
	}
	entry.Timer = time.AfterFunc(s.delay, func() {
		_ = o.submit(o.runCtx, &retryFireOp{
			o:       o,
			id:      id,
			issue:   issue,
			attempt: attempt,
		})
	})
	st.ScheduleRetry(entry)
	return nil
}

// retryFireOp is the actor-side handler for a fired retry timer. The
// SPEC §16.6 retry path is "if the entry is still queued, re-dispatch;
// otherwise drop the fire." Two timers may race here in pathological
// cases (a ScheduleRetry replace where the prior timer's Stop missed
// because the callback was already queued); the attempt-equality
// guard makes the stale fire a no-op.
type retryFireOp struct {
	o       *Orchestrator
	id      IssueID
	issue   tracker.Issue
	attempt int
}

func (r *retryFireOp) apply(st *OrchestratorState) func() {
	entry, ok := st.RetryAttempts[r.id]
	if !ok {
		// Already consumed by reconciliation (ReleaseClaim) or by an
		// earlier fire of the same retry. Either is correct.
		return nil
	}
	if entry.Attempt != r.attempt {
		// A newer ScheduleRetry replaced this entry; the older timer
		// fired late. Drop the stale fire — the newer entry will
		// re-dispatch on its own timer.
		return nil
	}
	// Consume the retry entry but keep Claimed: the re-dispatch
	// immediately re-adds Running, and dropping Claimed in between
	// would let a concurrent tick race in.
	delete(st.RetryAttempts, r.id)
	o := r.o
	issue := r.issue
	attempt := r.attempt
	return func() {
		o.spawn(r.id, issue, &attempt)
	}
}

// finalizeRunOp is the actor-side handler for a worker exit. Result.Err
// drives the SPEC §7.3 normal/abnormal exit branch; the followup
// schedules a retry on abnormal exit.
type finalizeRunOp struct {
	o          *Orchestrator
	id         IssueID
	issue      tracker.Issue
	identifier string
	attempt    *int
	result     WorkerResult
	started    time.Time
	done       chan struct{}
}

func (f *finalizeRunOp) apply(st *OrchestratorState) func() {
	elapsed := f.result.Elapsed
	if elapsed == 0 {
		elapsed = time.Since(f.started)
	}
	if f.result.Err == nil {
		st.FinishRunSucceeded(f.id, elapsed)
	} else {
		st.FinishRunFailed(f.id, elapsed)
	}
	close(f.done)
	if f.result.Err == nil {
		return nil
	}
	// Schedule a retry with attempt+1. Per SPEC §4.1.5 the first run's
	// RetryAttempt is nil; the first retry is attempt 1, the second 2,
	// and so on.
	nextAttempt := 1
	if f.attempt != nil {
		nextAttempt = *f.attempt + 1
	}
	o := f.o
	issue := f.issue
	identifier := f.identifier
	runErr := f.result.Err.Error()
	return func() {
		_ = o.ScheduleRetry(o.runCtx, issue, identifier, nextAttempt, runErr)
	}
}

// spawn asks the dispatcher for a worker, records the Running entry
// through the actor, and starts the watcher goroutine that submits
// finalizeRunOp on worker exit. The caller must already hold the
// Claimed slot for id (set by dispatchOp.apply or persisted across
// retryFireOp.apply); spawn does not check IsClaimed.
//
// spawn is invoked from a followup goroutine, never from inside an
// apply method, so its calls into o.submit are safe.
func (o *Orchestrator) spawn(id IssueID, issue tracker.Issue, attempt *int) {
	runCtx, cancel := context.WithCancel(o.runCtx)
	resultCh := o.dispatcher.Spawn(runCtx, issue, attempt)
	startedAt := time.Now()
	workerDone := make(chan struct{})
	entry := &RunningEntry{
		Issue:        issue,
		Identifier:   issue.Identifier,
		StartedAt:    startedAt,
		RetryAttempt: attempt,
		CancelWorker: cancel,
		Done:         workerDone,
	}
	_ = o.submit(o.runCtx, opFunc(func(st *OrchestratorState) func() {
		st.Running[id] = entry
		return nil
	}))
	go func() {
		var res WorkerResult
		select {
		case r, ok := <-resultCh:
			cancel()
			if ok {
				res = r
			} else {
				// Dispatcher closed without yielding a result: treat
				// as a cancellation, which becomes an abnormal exit
				// and triggers a retry per SPEC §7.3.
				res = WorkerResult{Err: context.Canceled, Elapsed: time.Since(startedAt)}
			}
		case <-o.runCtx.Done():
			cancel()
			close(workerDone)
			return
		}
		_ = o.submit(o.runCtx, &finalizeRunOp{
			o:          o,
			id:         id,
			issue:      issue,
			identifier: issue.Identifier,
			attempt:    attempt,
			result:     res,
			started:    startedAt,
			done:       workerDone,
		})
	}()
}
