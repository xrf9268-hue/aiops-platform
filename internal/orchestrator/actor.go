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
// PR 2 of the D21+D6 migration plan shipped this actor, the Dispatcher seam,
// and a scheduler seam. D16 wires that seam to SPEC §8.4 exponential failure
// backoff plus SPEC §16.6's short continuation retry.

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// WorkerResult is the per-run outcome the Dispatcher delivers when its
// spawned worker exits. Err is nil for SPEC §7.3 normal exit; retryable
// errors are treated as abnormal exits and trigger ScheduleRetry(attempt+1).
// NonRetryable errors fail fast and release the claim so deterministic
// configuration/task-build failures do not spin forever. Elapsed is folded
// into CodexTotals.SecondsRunning per SPEC §13.3.
type WorkerResult struct {
	Err          error
	NonRetryable bool
	Elapsed      time.Duration
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

// Scheduler computes the delay before the next retry request.
type Scheduler interface {
	NextDelay(RetryRequest) time.Duration
}

// RetryKind identifies whether the retry follows a failed worker run or a
// clean continuation turn.
type RetryKind string

const (
	RetryKindFailure      RetryKind = "failure"
	RetryKindContinuation RetryKind = "continuation"
)

// RetryRequest describes the retry being scheduled. Attempt is the 1-based
// failure retry attempt for RetryKindFailure. Continuation retries ignore it
// and always use the short SPEC §16.6 delay.
type RetryRequest struct {
	Kind    RetryKind
	Attempt int
}

// RetryScheduler implements the SPEC retry delays: clean continuation retries
// use one second; failure retries use delay=min(10s*2^(attempt-1), MaxBackoff).
type RetryScheduler struct {
	MaxBackoff time.Duration
}

const retryCapacityRecheckDelay = 100 * time.Millisecond
const continuationRetryDelay = time.Second

// NextDelay implements Scheduler.
func (s RetryScheduler) NextDelay(req RetryRequest) time.Duration {
	if req.Kind == RetryKindContinuation {
		return continuationRetryDelay
	}
	if req.Attempt < 1 {
		req.Attempt = 1
	}
	maxBackoff := s.MaxBackoff
	if maxBackoff <= 0 {
		maxBackoff = 5 * time.Minute
	}
	delay := 10 * time.Second
	for i := 1; i < req.Attempt; i++ {
		if delay >= maxBackoff/2 {
			return maxBackoff
		}
		delay *= 2
	}
	if delay > maxBackoff {
		return maxBackoff
	}
	return delay
}

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

// UpdateMaxConcurrentAgents applies a reloaded workflow capacity limit through
// the actor so dispatch and retry capacity checks observe the new value without
// restarting the process.
func (o *Orchestrator) UpdateMaxConcurrentAgents(ctx context.Context, maxConcurrentAgents int) error {
	if maxConcurrentAgents <= 0 {
		return nil
	}
	done := make(chan struct{}, 1)
	op := opFunc(func(st *OrchestratorState) func() {
		st.MaxConcurrentAgents = maxConcurrentAgents
		done <- struct{}{}
		return nil
	})
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

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

// ReconcileTrackerIssues cancels or releases in-process work that is no longer
// tracker-eligible. It is the per-tick half of SPEC §2.1/#78: each tracker poll
// revalidates active runs against the latest tracker state and cancels workers
// whose issues moved out of active states.
func (o *Orchestrator) ReconcileTrackerIssues(ctx context.Context, issuesByID map[string]tracker.Issue, activeStates map[string]struct{}) error {
	return o.ReconcileTrackerIssuesAndWait(ctx, issuesByID, activeStates, 0)
}

// ReconcileTrackerIssuesAndWait performs the same reconciliation as
// ReconcileTrackerIssues, then optionally waits for canceled workers to exit.
// This lets poll ticks provide prompt cancellation semantics without making the
// actor itself block on worker goroutines.
func (o *Orchestrator) ReconcileTrackerIssuesAndWait(ctx context.Context, issuesByID map[string]tracker.Issue, activeStates map[string]struct{}, wait time.Duration) error {
	reply := make(chan []*RunningEntry, 1)
	if err := o.submit(ctx, &reconcileTrackerIssuesOp{issuesByID: issuesByID, activeStates: activeStates, result: reply}); err != nil {
		return err
	}
	var canceled []*RunningEntry
	select {
	case canceled = <-reply:
	case <-ctx.Done():
		return ctx.Err()
	}
	return waitForReconciledWorkers(ctx, canceled, wait)
}

func waitForReconciledWorkers(ctx context.Context, canceled []*RunningEntry, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for _, entry := range canceled {
		if entry.Done == nil {
			continue
		}
		select {
		case <-entry.Done:
		case <-waitCtx.Done():
			return waitCtx.Err()
		}
	}
	return nil
}

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

// ReconcileInactiveTrackerIssuesAndWait cancels only issues explicitly observed
// in a terminal or configured inactive tracker state. Missing issues are
// treated as unknown instead of inactive because tracker adapters may return
// partial state listings under pagination caps.
func (o *Orchestrator) ReconcileInactiveTrackerIssuesAndWait(ctx context.Context, issuesByID map[string]tracker.Issue, workerExitTimeout time.Duration) error {
	reply := make(chan []*RunningEntry, 1)
	op := &reconcileInactiveTrackerIssuesOp{issuesByID: issuesByID, result: reply}
	if err := o.submit(ctx, op); err != nil {
		return err
	}
	var canceled []*RunningEntry
	select {
	case canceled = <-reply:
	case <-ctx.Done():
		return ctx.Err()
	}
	return waitForReconciledWorkers(ctx, canceled, workerExitTimeout)
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
	return o.scheduleRetry(ctx, issue, identifier, RetryRequest{Kind: RetryKindFailure, Attempt: attempt}, attempt, runErr)
}

func (o *Orchestrator) scheduleContinuationRetry(ctx context.Context, issue tracker.Issue, identifier string, attempt int) error {
	return o.scheduleRetry(ctx, issue, identifier, RetryRequest{Kind: RetryKindContinuation, Attempt: attempt}, attempt, "")
}

func (o *Orchestrator) scheduleRetry(ctx context.Context, issue tracker.Issue, identifier string, req RetryRequest, attempt int, runErr string) error {
	delay := o.scheduler.NextDelay(req)
	op := &scheduleRetryOp{
		o:          o,
		issue:      issue,
		identifier: identifier,
		attempt:    attempt,
		delay:      delay,
		runErr:     runErr,
		kind:       req.Kind,
	}
	return o.submit(ctx, op)
}

type reconcileTrackerIssuesOp struct {
	issuesByID   map[string]tracker.Issue
	activeStates map[string]struct{}
	result       chan<- []*RunningEntry
}

func (r *reconcileTrackerIssuesOp) apply(st *OrchestratorState) func() {
	var cancelEntries []*RunningEntry
	for id, run := range st.Running {
		issue, ok := r.issuesByID[string(id)]
		if ok && isActiveTrackerState(issue.State, r.activeStates) {
			continue
		}
		st.ReleaseClaim(id)
		run.ReconcileCancel = true
		cancelEntries = append(cancelEntries, run)
	}
	for id := range st.RetryAttempts {
		issue, ok := r.issuesByID[string(id)]
		if ok && isActiveTrackerState(issue.State, r.activeStates) {
			continue
		}
		st.ReleaseClaim(id)
	}
	return reconcileCancelFollowup(cancelEntries, r.result)
}

type reconcileInactiveTrackerIssuesOp struct {
	issuesByID map[string]tracker.Issue
	result     chan<- []*RunningEntry
}

func (r *reconcileInactiveTrackerIssuesOp) apply(st *OrchestratorState) func() {
	var cancelEntries []*RunningEntry
	for id, run := range st.Running {
		if _, ok := r.issuesByID[string(id)]; !ok {
			continue
		}
		st.ReleaseClaim(id)
		run.ReconcileCancel = true
		cancelEntries = append(cancelEntries, run)
	}
	for id := range st.RetryAttempts {
		if _, ok := r.issuesByID[string(id)]; !ok {
			continue
		}
		st.ReleaseClaim(id)
	}
	return reconcileCancelFollowup(cancelEntries, r.result)
}

func reconcileCancelFollowup(cancelEntries []*RunningEntry, result chan<- []*RunningEntry) func() {
	return func() {
		for _, entry := range cancelEntries {
			if entry.CancelWorker != nil {
				entry.CancelWorker()
			}
		}
		if result != nil {
			result <- cancelEntries
		}
	}
}

func isActiveTrackerState(state string, activeStates map[string]struct{}) bool {
	if len(activeStates) == 0 {
		return false
	}
	_, ok := activeStates[strings.ToLower(strings.TrimSpace(state))]
	return ok
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
	st.ReleaseFailedIfIssueChanged(d.issue)
	attempt := d.attempt
	if d.trackerRechecked {
		if entry, ok := st.RetryAttempts[id]; ok && entry.Kind == RetryKindContinuation {
			if !entry.IsDue(time.Now()) {
				d.result <- ErrNotDispatched
				return nil
			}
			if entry.Timer != nil {
				entry.Timer.Stop()
			}
			if attempt == nil {
				entryAttempt := entry.Attempt
				attempt = &entryAttempt
			}
			delete(st.RetryAttempts, id)
			delete(st.Claimed, id)
		}
	}
	if st.IsClaimed(id) {
		d.result <- ErrNotDispatched
		return nil
	}
	if st.RunningCount() >= st.MaxConcurrentAgents {
		d.result <- ErrCapacityFull
		return nil
	}
	// Reserve the slot synchronously so a concurrent dispatchOp aborts
	// on its IsClaimed check. The followup records Running once the
	// worker is spawned.
	st.Claimed[id] = struct{}{}
	o := d.o
	issue := d.issue
	result := d.result
	return func() {
		o.spawn(id, issue, attempt)
		result <- nil
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
	kind       RetryKind
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
		Kind:       s.kind,
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
	if entry.Kind == RetryKindContinuation {
		// Continuation retries are only a short wake-up signal after a clean
		// worker exit. They must not spawn from the cached issue snapshot: the
		// next poll tick has to observe the issue still active and call
		// RequestDispatchAfterTrackerRecheck, which consumes this entry before
		// spawning the next turn.
		entry.Timer = nil
		return nil
	}
	if st.RunningCount() >= st.MaxConcurrentAgents {
		// Retry timers must obey the same capacity gate as fresh dispatch.
		// Leave the retry queued and arm a short follow-up timer so the issue
		// is retried after capacity can free instead of spawning over the cap.
		if entry.Timer != nil {
			entry.Timer.Stop()
		}
		o := r.o
		id := r.id
		issue := r.issue
		attempt := r.attempt
		entry.DueAt = time.Now().Add(retryCapacityRecheckDelay)
		entry.Timer = time.AfterFunc(retryCapacityRecheckDelay, func() {
			_ = o.submit(o.runCtx, &retryFireOp{
				o:       o,
				id:      id,
				issue:   issue,
				attempt: attempt,
			})
		})
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
	entry      *RunningEntry
	done       chan struct{}
}

func (f *finalizeRunOp) apply(st *OrchestratorState) func() {
	elapsed := f.result.Elapsed
	if elapsed == 0 {
		elapsed = time.Since(f.started)
	}
	if f.result.Err == nil {
		if !st.FinishRunSucceeded(f.id, f.entry, elapsed) {
			close(f.done)
			return nil
		}
		st.Claimed[f.id] = struct{}{}
		close(f.done)
		nextAttempt := 1
		if f.attempt != nil {
			nextAttempt = *f.attempt + 1
		}
		o := f.o
		issue := f.issue
		identifier := f.identifier
		return func() {
			_ = o.scheduleContinuationRetry(o.runCtx, issue, identifier, nextAttempt)
		}
	}
	if f.result.NonRetryable {
		if !st.FinishRunNonRetryableFailed(f.id, f.entry, elapsed) {
			close(f.done)
			return nil
		}
		close(f.done)
		return nil
	}
	if f.entry.ReconcileCancel {
		if !st.FinishRunReconciledCancelled(f.id, f.entry, elapsed) {
			close(f.done)
			return nil
		}
		close(f.done)
		return nil
	}
	if !st.FinishRunFailed(f.id, f.entry, elapsed) {
		close(f.done)
		return nil
	}
	// Hold the Claimed slot across the gap between this apply (which
	// returns control to the actor's select loop) and the
	// scheduleRetryOp that the followup enqueues. Without this re-set,
	// any RequestDispatch op already queued behind finalizeRunOp would
	// observe IsClaimed=false (Running gone, Claimed gone, RetryAttempts
	// not yet set) and dispatch the issue immediately — bypassing
	// backoff and racing a phantom retry timer against a live worker.
	// scheduleRetryOp's call to OrchestratorState.ScheduleRetry re-sets
	// Claimed idempotently, so this is safe.
	st.Claimed[f.id] = struct{}{}
	close(f.done)
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
			entry:      entry,
			done:       workerDone,
		})
	}()
}
