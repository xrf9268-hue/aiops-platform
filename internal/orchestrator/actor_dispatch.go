package orchestrator

// actor_dispatch.go holds the dispatch path: the public RequestDispatch
// entry points, the dispatchOp stateOp, and the spawn seam that launches a
// worker for a claimed issue. See actor.go for the actor's mutation discipline.

import (
	"context"
	"errors"
	"fmt"
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
	if st.IsOperatorTerminalStopped(id) {
		if entry, first := st.RecordOperatorTerminalDispatchSuppressed(id, d.issue, "active_candidate_after_operator_terminal_stop"); first {
			st.RecordEvent(RuntimeEvent{
				Kind:       RuntimeEventOperatorTerminalStopDispatchSuppressed,
				IssueID:    id,
				Identifier: entry.Identifier,
				Message:    "dispatch suppressed after Operator Terminal Stop",
			})
		}
		d.result <- ErrNotDispatched
		return nil
	}
	consumedContinuation, continuationAttempt, continuationTurnCount, deny := resolveDispatchClaim(st, id, d.trackerRechecked)
	if deny {
		d.result <- ErrNotDispatched
		return nil
	}
	if d.blockConsumedContinuationIfBudgetExceeded(st, id, consumedContinuation, continuationTurnCount) {
		d.result <- ErrNotDispatched
		return nil
	}
	cleanTurnBudget := cleanTurnBudgetForContinuationBudget(st.MaxContinuationTurns, continuationTurnCount)
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
	workspace := Workspace{}
	if consumedContinuation != nil {
		workspace = consumedContinuation.Workspace
	}
	consumeContinuationRetry(st, id, consumedContinuation)
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
		defer func() { result <- nil }()
		o.spawn(id, issue, attempt, continuationAttempt, continuationTurnCount, cleanTurnBudget, workspace)
	}
}

func (d *dispatchOp) blockConsumedContinuationIfBudgetExceeded(st *OrchestratorState, id IssueID, retry *RetryEntry, continuationTurnCount int) bool {
	if retry == nil || !continuationBudgetExceeded(st.MaxContinuationTurns, continuationTurnCount) {
		return false
	}
	runErr := continuationBudgetError(continuationTurnCount, st.MaxContinuationTurns)
	if st.BlockRetryWithReason(id, retry, d.issue, time.Now().UTC(), continuationBudgetBlockMethod, runErr) {
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventContinuationBudgetBlocked, IssueID: id, Identifier: continuationBudgetIdentifier(d.issue, retry), Message: runErr})
	}
	return true
}

func continuationBudgetIdentifier(issue tracker.Issue, retry *RetryEntry) string {
	if issue.Identifier != "" {
		return issue.Identifier
	}
	if retry == nil {
		return ""
	}
	return retry.Identifier
}

func consumeContinuationRetry(st *OrchestratorState, id IssueID, retry *RetryEntry) {
	if retry == nil {
		return
	}
	if retry.Timer != nil {
		retry.Timer.Stop()
	}
	delete(st.RetryAttempts, id)
	delete(st.Claimed, id)
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
func resolveDispatchClaim(st *OrchestratorState, id IssueID, trackerRechecked bool) (consumed *RetryEntry, continuationAttempt, continuationTurnCount int, deny bool) {
	if !trackerRechecked {
		return nil, 0, 0, st.IsClaimed(id)
	}
	entry, allow := dispatchClaimGateAllows(st, id, time.Now())
	if !allow {
		return nil, 0, 0, true
	}
	if entry == nil {
		return nil, 0, 0, false
	}
	return entry, entry.Attempt, entry.ContinuationTurnCount, false
}

// dispatchClaimGateAllows reports whether a tracker-rechecked dispatch for id
// would pass the claim gate right now: the issue is entirely unclaimed
// (entry nil), or its queued retry entry is a DUE continuation (entry
// returned for the caller to consume). Shared by resolveDispatchClaim and
// dispatchClaimableIssueIDsOp so the dispatch gate and the poller's
// pre-dispatch revalidation subset cannot drift (#740).
func dispatchClaimGateAllows(st *OrchestratorState, id IssueID, now time.Time) (entry *RetryEntry, allow bool) {
	queued, ok := st.RetryAttempts[id]
	if !ok {
		return nil, !st.IsClaimed(id)
	}
	if queued.Kind != RetryKindContinuation || !queued.IsDue(now) {
		return nil, false
	}
	return queued, true
}

// DispatchClaimableIssueIDs returns the subset of ids a tracker-rechecked
// dispatch would currently pass the claim gate for. The poller uses it to
// revalidate (and dispatch) only candidates that can actually spawn this
// tick — the same trim upstream gets from should_dispatch_issue? running
// before revalidate_issue_for_dispatch (orchestrator.ex:776-777, 909-910) —
// instead of re-fetching tracker state for already-running issues every tick.
// The dispatch op re-resolves the claim authoritatively, so a claim taken
// between this read and the dispatch is still denied there.
func (o *Orchestrator) DispatchClaimableIssueIDs(ctx context.Context, ids []IssueID) (map[IssueID]struct{}, error) {
	reply := make(chan map[IssueID]struct{}, 1)
	if err := o.submit(ctx, &dispatchClaimableIssueIDsOp{ids: ids, result: reply}); err != nil {
		return nil, err
	}
	select {
	case claimable := <-reply:
		return claimable, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type dispatchClaimableIssueIDsOp struct {
	ids    []IssueID
	result chan<- map[IssueID]struct{}
}

func (d *dispatchClaimableIssueIDsOp) apply(st *OrchestratorState) func() {
	now := time.Now()
	claimable := make(map[IssueID]struct{}, len(d.ids))
	for _, id := range d.ids {
		if _, allow := dispatchClaimGateAllows(st, id, now); allow {
			claimable[id] = struct{}{}
		}
	}
	d.result <- claimable
	return nil
}

func cleanTurnBudgetForContinuationBudget(maxContinuationTurns, continuationTurnCount int) int {
	remaining := maxContinuationTurns - continuationTurnCount
	if remaining < 0 {
		return 0
	}
	return remaining
}

// WaitForWorkers blocks until every spawned worker goroutine has consumed its
// dispatcher result (the worker subprocess has exited and its outcome is
// collected) or grace elapses, and reports whether the drain completed. main
// calls it after the poll loop returns on SIGTERM/SIGINT: without the wait the
// process exits mid-run, racing the runner's subprocess kill and skipping
// after_run/workspace teardown (BEAM gets the ordered child shutdown for free
// from the supervision tree; a Go main return provides no such guarantee —
// AGENTS.md cross-cutting checklist item 2). Workers observe the canceled run
// context and exit on their own; grace only bounds how long shutdown waits for
// that to happen.
func (o *Orchestrator) WaitForWorkers(grace time.Duration) bool {
	drained := make(chan struct{})
	safeGo("orchestrator.worker_drain_wait", func() {
		o.workerWG.Wait()
		close(drained)
	})
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-drained:
		return true
	case <-timer.C:
		return false
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
func (o *Orchestrator) spawn(id IssueID, issue tracker.Issue, attempt *int, continuationAttempt, continuationTurnCount, cleanTurnBudget int, workspace Workspace) {
	workerTracked := false
	fanoutStarted := false
	runningRegistered := false
	var cancel context.CancelCauseFunc
	var startedAt time.Time
	var workerDone chan struct{}
	var entry *RunningEntry
	recovery := spawnPanicRecovery{
		o:                 o,
		id:                id,
		issue:             issue,
		attempt:           attempt,
		workspace:         workspace,
		workerTracked:     &workerTracked,
		fanoutStarted:     &fanoutStarted,
		runningRegistered: &runningRegistered,
		cancel:            &cancel,
		startedAt:         &startedAt,
		workerDone:        &workerDone,
		entry:             &entry,
	}
	defer recovery.recover()
	// Register with the drain group before anything that can start a worker
	// so WaitForWorkers can never observe a live subprocess it isn't
	// tracking; every return path below either hands the slot to the fanout
	// goroutine or releases it.
	o.workerWG.Add(1)
	workerTracked = true
	runCtx, runCancel := context.WithCancelCause(o.runCtx)
	cancel = runCancel
	startedAt = time.Now()
	workerDone = make(chan struct{})
	entry = &RunningEntry{
		Issue:                 issue,
		Identifier:            issue.Identifier,
		StartedAt:             startedAt,
		RetryAttempt:          attempt,
		ContinuationAttempt:   continuationAttempt,
		ContinuationTurnCount: continuationTurnCount,
		Workspace:             workspace,
		CancelWorker:          cancel,
		Done:                  workerDone,
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
		o.workerWG.Done()
		workerTracked = false
		return
	}
	select {
	case <-registered:
		runningRegistered = true
	case <-o.runCtx.Done():
		cancel(nil)
		close(workerDone)
		o.workerWG.Done()
		workerTracked = false
		return
	}
	resultCh := o.dispatcher.Spawn(runCtx, issue, attempt, DispatchOptions{CleanTurnBudget: cleanTurnBudget})
	o.watchWorkerResult(id, issue, attempt, resultCh, startedAt, cancel, entry, workerDone)
	fanoutStarted = true
	workerTracked = false
}

func (o *Orchestrator) watchWorkerResult(id IssueID, issue tracker.Issue, attempt *int, resultCh <-chan WorkerResult, startedAt time.Time, cancel context.CancelCauseFunc, entry *RunningEntry, workerDone chan struct{}) {
	go func() {
		defer o.workerWG.Done()
		defer recoverPanic("orchestrator.spawn_result_fanout")
		res := o.awaitWorkerResult(resultCh, startedAt, cancel)
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

type spawnPanicRecovery struct {
	o                 *Orchestrator
	id                IssueID
	issue             tracker.Issue
	attempt           *int
	workspace         Workspace
	workerTracked     *bool
	fanoutStarted     *bool
	runningRegistered *bool
	cancel            *context.CancelCauseFunc
	startedAt         *time.Time
	workerDone        *chan struct{}
	entry             **RunningEntry
}

func (r spawnPanicRecovery) recover() {
	recovered := recover()
	if recovered == nil {
		return
	}
	spawnErr := fmt.Errorf("orchestrator spawn panic: %v", recovered)
	recoverPanicValue("orchestrator.spawn", recovered)
	if r.cancel != nil && *r.cancel != nil {
		(*r.cancel)(spawnErr)
	}
	if r.fanoutStarted != nil && *r.fanoutStarted {
		return
	}
	if r.canFinalizeRunning() {
		resultCh := syntheticWorkerResult(WorkerResult{Err: spawnErr, Elapsed: time.Since(*r.startedAt)})
		r.o.watchWorkerResult(r.id, r.issue, r.attempt, resultCh, *r.startedAt, *r.cancel, *r.entry, *r.workerDone)
		return
	}
	if r.workerTracked != nil && *r.workerTracked {
		r.o.workerWG.Done()
		*r.workerTracked = false
	}
	r.o.recoverSpawnPanic(r.o.runCtx, r.id, r.issue, r.attempt, spawnErr.Error(), r.workspace)
}

func (r spawnPanicRecovery) canFinalizeRunning() bool {
	return r.runningRegistered != nil &&
		*r.runningRegistered &&
		r.entry != nil &&
		*r.entry != nil &&
		r.workerDone != nil &&
		*r.workerDone != nil &&
		r.cancel != nil &&
		*r.cancel != nil
}

func syntheticWorkerResult(result WorkerResult) <-chan WorkerResult {
	ch := make(chan WorkerResult, 1)
	ch <- result
	close(ch)
	return ch
}

func (o *Orchestrator) recoverSpawnPanic(ctx context.Context, id IssueID, issue tracker.Issue, attempt *int, runErr string, workspace Workspace) {
	if ctx == nil {
		return
	}
	identifier := issue.Identifier
	if identifier == "" {
		identifier = issue.ID
	}
	submitErr := o.submit(ctx, opFunc(func(st *OrchestratorState) func() {
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventFailed, IssueID: id, Identifier: identifier, Message: runErr})
		nextAttempt := 1
		if attempt != nil {
			nextAttempt = *attempt + 1
		}
		return func() {
			o.logRescheduleErr(o.scheduleFailureRetry(ctx, issue, identifier, nextAttempt, runErr, workspace), id, identifier)
		}
	}))
	o.logRescheduleErr(submitErr, id, identifier)
}

// awaitWorkerResult collects the dispatcher's single result for a spawned
// worker. On shutdown (runCtx canceled) it cancels the worker and then keeps
// waiting instead of abandoning it: the Dispatcher contract yields exactly one
// result (or a close) once the worker observes the canceled context, so this
// converges — returning early would strand the still-running subprocess for
// WaitForWorkers and skip result collection entirely (#1030). A dispatcher
// close without a result is a cancellation, which becomes an abnormal exit and
// triggers a retry per SPEC §7.3.
func (o *Orchestrator) awaitWorkerResult(resultCh <-chan WorkerResult, startedAt time.Time, cancel context.CancelCauseFunc) WorkerResult {
	var r WorkerResult
	ok := false
	select {
	case r, ok = <-resultCh:
		cancel(nil)
	case <-o.runCtx.Done():
		cancel(nil)
		r, ok = <-resultCh
	}
	if !ok {
		return WorkerResult{Err: context.Canceled, Elapsed: time.Since(startedAt)}
	}
	return r
}
