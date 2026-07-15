package orchestrator

// actor_finalize.go holds finalizeRunOp, the stateOp that applies a worker's
// terminal RunResult — routing it to completion, failure backoff, quota
// backoff, or a continuation retry. See actor.go for the mutation discipline.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

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

// apply routes a worker's terminal RunResult. Each branch is a single-concern
// handler that, once its guard matches, owns the state transition, the
// close(f.done), and the returned followup; the guard order is the SPEC routing
// priority (input-required block → clean exit → reconciled cancel cleanup →
// quota backoff → reconcile cancel → failure retry). Handlers
// that may schedule async work report (followup, handled); the pure terminal
// input-required block reports only handled since it never
// schedules a followup. applyCleanExit and applyFailureRetry are reached
// unconditionally on their branch and always handle, so they return the followup
// directly.
func (f *finalizeRunOp) apply(st *OrchestratorState) func() {
	elapsed := f.result.Elapsed
	if elapsed == 0 {
		elapsed = time.Since(f.started)
	}
	if f.applyInputRequiredBlock(st, elapsed) {
		return nil
	}
	if f.applyBudgetExceededBlock(st, elapsed) {
		return nil
	}
	if f.result.Err == nil {
		return f.applyCleanExit(st, elapsed)
	}
	if followup, ok := f.applyReconciledCancelCleanup(st, elapsed); ok {
		return followup
	}
	if followup, ok := f.applyQuotaBackoff(st, elapsed); ok {
		return followup
	}
	if followup, ok := f.applyReconcileCancel(st, elapsed); ok {
		return followup
	}
	return f.applyFailureRetry(st, elapsed)
}

// applyInputRequiredBlock handles SPEC §7.3 input-required exits: the run is
// parked in the blocked substate awaiting operator input, never retried. It
// never schedules a followup, so it reports only whether it handled the exit.
func (f *finalizeRunOp) applyInputRequiredBlock(st *OrchestratorState, elapsed time.Duration) bool {
	if !f.entry.InputRequired && !f.result.InputRequired && !runner.IsInputRequired(f.result.Err) {
		return false
	}
	runErr := "input required"
	if f.result.Err != nil {
		runErr = f.result.Err.Error()
	}
	if !st.BlockRun(f.id, f.entry, f.entry.InputRequiredAt, runErr, elapsed) {
		close(f.done)
		return true
	}
	st.RecordEvent(RuntimeEvent{Kind: RuntimeEventInputBlocked, IssueID: f.id, Identifier: f.identifier, Message: runErr})
	close(f.done)
	return true
}

const continuationBudgetBlockMethod = "continuation_budget"
const budgetExceededBlockMethod = "budget_exceeded"

func (f *finalizeRunOp) applyBudgetExceededBlock(st *OrchestratorState, elapsed time.Duration) bool {
	if !f.entry.BudgetExceeded {
		return false
	}
	blockedAt := f.entry.BudgetExceededAt
	runErr := f.entry.BudgetExceededError
	if strings.TrimSpace(runErr) == "" {
		guard := st.BudgetGuardrails
		runErr = fmt.Sprintf(
			"worker-observed, runner-reported Codex claim budget exceeded: "+
				"current_claim_total_tokens=%d max_tokens_per_claim=%d "+
				"current_claim_runtime_seconds=%.0f max_runtime_seconds_per_claim=%d; "+
				"recorded exceedance reason missing; external GitHub @codex review and otherwise unreported nested or subagent usage are excluded from token totals",
			f.entry.CodexTotalTokens, guard.MaxTokensPerClaim, elapsedSeconds(elapsed), guard.MaxRuntimeSecondsPerClaim,
		)
	}
	if !st.BlockRunWithReason(f.id, f.entry, blockedAt, budgetExceededBlockMethod, runErr, elapsed) {
		close(f.done)
		return true
	}
	close(f.done)
	return true
}

// applyCleanExit handles a normal (Err==nil) worker exit: a reconciled-cleanup
// continuation, a D34 continuation-budget block, or a normal continuation
// retry. Upstream/SPEC §7.1 leaves the clean continuation loop unbounded; D34
// caps clean still-active turns locally because #621/PR #625 proved the upstream behavior
// can burn quota forever on impossible issues. It always handles the exit, so it
// returns the followup directly.
func (f *finalizeRunOp) applyCleanExit(st *OrchestratorState, elapsed time.Duration) func() {
	nextContinuationAttempt := f.entry.ContinuationAttempt + 1
	nextContinuationTurnCount := f.entry.ContinuationTurnCount + continuationTurnDelta(f.entry)
	if f.result.IssueExitState != nil && f.result.IssueExitState.Terminal {
		recordStop := f.result.IssueExitState.OperatorTerminalStop || !agentOwnedTerminalStateMatches(f.entry, f.result.IssueExitState.State)
		return f.applyTerminalSelfStop(st, elapsed, recordStop)
	}
	if f.entry.ReconcileCleanupWorkspace && runHasCompletedTurn(f.entry) {
		cleanup := f.o.reconciledWorkspaceCleanupOrContinuation(f.id, f.entry, nextContinuationAttempt)
		if !st.FinishRunSucceeded(f.id, f.entry, elapsed) {
			close(f.done)
			return nil
		}
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventCompleted, IssueID: f.id, Identifier: f.identifier, Message: "worker exited cleanly"})
		close(f.done)
		return cleanup
	}
	if f.shouldBlockContinuationBudget(st, nextContinuationTurnCount) {
		return f.applyContinuationBudgetBlock(st, elapsed, nextContinuationTurnCount)
	}
	// SPEC §7.1 leaves the clean continuation loop unbounded upstream: an active
	// issue keeps getting fresh sessions until tracker state changes (reconcile /
	// §16.5 self-stop). D34 keeps that loop bounded locally by carrying
	// nextContinuationTurnCount through the retry entry.
	if !f.finishCleanContinuation(st, elapsed) {
		close(f.done)
		return nil
	}
	// Use f.entry.Issue, not f.issue: reconciliation may have refreshed
	// the tracker state mid-run, and per-state capacity gates must see
	// the live state, not the dispatch-time snapshot.
	issue := f.entry.Issue
	// Carry the finalized run's workspace onto the continuation retry. A
	// clean exit can be a SPEC §16.5 self-stop (the per-turn refresher saw
	// the issue leave the active set); if the issue is later observed
	// terminal while this continuation is queued, the reconcile pass uses
	// this path to clean the directory through the §18.1 seam instead of
	// leaking it until the next startup sweep (#341).
	workspace := f.entry.Workspace
	st.Claimed[f.id] = struct{}{}
	st.ClaimedIssues[f.id] = issue
	close(f.done)
	// A clean continuation queues a follow-on dispatch. Keep the dispatch
	// attempt 1-based for prompt/retry identity, but do not carry it into
	// future failure backoff; otherwise many successful turns inflate the next
	// transient failure straight to the max backoff.
	nextAttempt := nextContinuationAttempt
	o := f.o
	id := f.id
	identifier := f.identifier
	return func() {
		o.logRescheduleErr(o.scheduleContinuationRetry(o.runCtx, issue, identifier, nextAttempt, nextContinuationTurnCount, workspace), id, identifier)
	}
}

func (f *finalizeRunOp) finishCleanContinuation(st *OrchestratorState, elapsed time.Duration) bool {
	if f.shouldRecordActiveSuccessNoHandoff() {
		if !st.FinishRunActiveSuccessNoHandoff(f.id, f.entry, elapsed) {
			return false
		}
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventCompleted, IssueID: f.id, Identifier: f.identifier, Message: "worker exited cleanly"})
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventActiveSuccessNoHandoff, IssueID: f.id, Identifier: f.identifier, Message: "worker exited cleanly while issue remained active with no agent handoff"})
		return true
	}
	if !st.FinishRunSucceeded(f.id, f.entry, elapsed) {
		return false
	}
	st.RecordEvent(RuntimeEvent{Kind: RuntimeEventCompleted, IssueID: f.id, Identifier: f.identifier, Message: "worker exited cleanly"})
	return true
}

func (f *finalizeRunOp) shouldRecordActiveSuccessNoHandoff() bool {
	return f.result.IssueExitState == nil &&
		!runHasCurrentIssueHandoff(f.entry) &&
		strings.TrimSpace(f.entry.Issue.State) != ""
}

func (f *finalizeRunOp) shouldBlockContinuationBudget(st *OrchestratorState, turnCount int) bool {
	return f.result.IssueExitState == nil && continuationBudgetExceeded(st.MaxContinuationTurns, turnCount)
}

func (f *finalizeRunOp) applyContinuationBudgetBlock(st *OrchestratorState, elapsed time.Duration, turnCount int) func() {
	runErr := continuationBudgetError(turnCount, st.MaxContinuationTurns)
	if !st.BlockRunWithReason(f.id, f.entry, time.Now().UTC(), continuationBudgetBlockMethod, runErr, elapsed) {
		close(f.done)
		return nil
	}
	st.RecordEvent(RuntimeEvent{Kind: RuntimeEventContinuationBudgetBlocked, IssueID: f.id, Identifier: f.identifier, Message: runErr})
	close(f.done)
	return nil
}

func (f *finalizeRunOp) applyTerminalSelfStop(st *OrchestratorState, elapsed time.Duration, recordStop bool) func() {
	snapshot := f.result.IssueExitState
	issue := f.entry.Issue
	if strings.TrimSpace(snapshot.State) != "" {
		issue.State = snapshot.State
		f.entry.Issue = issue
	}
	if recordStop {
		recordOperatorTerminalStop(st, f.id, issue)
	}
	f.entry.ReconcileCleanupWorkspace = true
	cleanup := f.o.reconciledWorkspaceCleanup(f.id, f.entry)
	if !st.FinishRunTerminalSelfStop(f.id, f.entry, elapsed) {
		close(f.done)
		return nil
	}
	close(f.done)
	return cleanup
}

func continuationTurnDelta(run *RunningEntry) int {
	if run != nil && run.Session.TurnCount > 0 {
		return run.Session.TurnCount
	}
	return 1
}

func continuationBudgetExceeded(maxContinuationTurns, cumulativeTurns int) bool {
	return maxContinuationTurns > 0 && cumulativeTurns >= maxContinuationTurns
}

func continuationBudgetError(cumulativeTurns, maxContinuationTurns int) string {
	return fmt.Sprintf("continuation budget exhausted: cumulative_turns=%d max_continuation_turns=%d", cumulativeTurns, maxContinuationTurns)
}

// applyReconciledCancelCleanup cleans the workspace of a run that reconciliation
// marked for cleanup and that has a completed turn, on an abnormal exit (#341).
func (f *finalizeRunOp) applyReconciledCancelCleanup(st *OrchestratorState, elapsed time.Duration) (func(), bool) {
	if !f.entry.ReconcileCleanupWorkspace || !runHasCompletedTurn(f.entry) {
		return nil, false
	}
	cleanup := f.o.reconciledWorkspaceCleanup(f.id, f.entry)
	if !st.FinishRunReconciledCancelled(f.id, f.entry, elapsed) {
		close(f.done)
		return nil, true
	}
	// The run completed ≥1 turn (gate above) before reconcile reaped it, so it made
	// progress — surface it in /api/v1/state instead of leaving it absent from
	// completed (#557). turn_completed fires after every turn, so this is
	// "reaped after progress" (usually the agent's handoff; inspect to confirm), not
	// a guaranteed success. The runtime event carries the identifier so the run is
	// drillable by identifier via /api/v1/<issue>, like completed runs.
	st.recordReconcileStoppedWithProgress(f.id)
	st.RecordEvent(RuntimeEvent{Kind: RuntimeEventReconcileStopped, IssueID: f.id, Identifier: f.identifier, Message: "reconcile stopped run after ≥1 completed turn"})
	if runHasCurrentIssueHandoff(f.entry) {
		f.recordAgentHandoffReconcileStopped(st)
	}
	close(f.done)
	return cleanup, true
}

// applyReconcileCancel cleans the workspace of a run reconciliation cancelled
// mid-run (the issue left the active set), now that the worker has exited.
func (f *finalizeRunOp) applyReconcileCancel(st *OrchestratorState, elapsed time.Duration) (func(), bool) {
	if !f.entry.ReconcileCancel {
		return nil, false
	}
	// Capture the cleanup followup before FinishRunReconciledCancelled
	// drops the entry from state: the worker has now exited, so the
	// workspace dir is free to remove (SPEC §18.1 active transition).
	// Done stays tied to worker exit (closed here), so the reconcile wait
	// keeps its worker_exit_timeout meaning and a slow before_remove hook
	// cannot surface as a spurious "deadline exceeded" poll error (Codex
	// P2). Cleanup runs asynchronously, bounded by its own hook timeout;
	// the re-dispatch data-loss race — a re-opened issue dispatched to the
	// same deterministic path while cleanup is still running — is prevented
	// inside the cleaner, which skips removal when the issue has been
	// re-claimed (Codex P1), rather than by gating the wait on cleanup.
	cleanup := f.o.reconciledWorkspaceCleanup(f.id, f.entry)
	if !st.FinishRunReconciledCancelled(f.id, f.entry, elapsed) {
		close(f.done)
		return nil, true
	}
	// If the run completed ≥1 turn before reconcile reaped it mid-finalization, it
	// made progress — surface it distinctly so a progressed run is visible in
	// /api/v1/state instead of absent from completed (#557). It is
	// usually the agent's handoff, but turn_completed fires after every turn, so it
	// can also be a run an operator stopped after an intermediate turn (inspect to
	// confirm) — not a guaranteed success. A 0-turn cancel is a genuine no-progress
	// stop and stays unrecorded. The runtime event carries the identifier so the run
	// is drillable by identifier via /api/v1/<issue>, like completed/failed.
	if runHasCompletedTurn(f.entry) {
		st.recordReconcileStoppedWithProgress(f.id)
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventReconcileStopped, IssueID: f.id, Identifier: f.identifier, Message: "reconcile stopped run after ≥1 completed turn"})
		if runHasCurrentIssueHandoff(f.entry) {
			f.recordAgentHandoffReconcileStopped(st)
		}
	} else if runHasCurrentIssueHandoff(f.entry) {
		f.recordAgentHandoffReconcileStopped(st)
	}
	close(f.done)
	return cleanup, true
}

func (f *finalizeRunOp) recordAgentHandoffReconcileStopped(st *OrchestratorState) {
	st.recordAgentHandoffReconcileStopped(f.id)
	st.RecordEvent(RuntimeEvent{Kind: RuntimeEventAgentHandoffReconcileStopped, IssueID: f.id, Identifier: f.identifier, Message: "reconcile stopped run after agent-side current-issue state handoff"})
}

// applyFailureRetry is the default terminal route for a retryable runner error:
// schedule a backoff retry with attempt+1. SPEC §8.4 / §16.6 retry unboundedly
// until the tracker takes the issue out of active work (the opt-in failure-retry
// cap was removed in #577). It always handles the exit, so it returns the
// followup directly.
func (f *finalizeRunOp) applyFailureRetry(st *OrchestratorState, elapsed time.Duration) func() {
	// Schedule a retry with attempt+1. Per SPEC §4.1.5 the first run's
	// RetryAttempt is nil; the first retry is attempt 1, the second 2,
	// and so on. SPEC §8.4 / §16.6 keep scheduling retries indefinitely with
	// the exponential-backoff ceiling until the tracker takes the issue out of
	// active work — there is no retry-count cap (the opt-in max_retry_attempts
	// cap was removed in #577 / DEVIATIONS D29).
	nextAttempt := 1
	if f.attempt != nil {
		nextAttempt = *f.attempt + 1
	}
	runErr := f.result.Err.Error()
	if !st.FinishRunFailed(f.id, f.entry, elapsed) {
		close(f.done)
		return nil
	}
	st.RecordEvent(RuntimeEvent{Kind: RuntimeEventFailed, IssueID: f.id, Identifier: f.identifier, Message: runErr})
	// Hold the Claimed slot across the gap between this apply (which
	// returns control to the actor's select loop) and the
	// scheduleRetryOp that the followup enqueues. Without this re-set,
	// any RequestDispatch op already queued behind finalizeRunOp would
	// observe IsClaimed=false (Running gone, Claimed gone, RetryAttempts
	// not yet set) and dispatch the issue immediately — bypassing
	// backoff and racing a phantom retry timer against a live worker.
	// scheduleRetryOp's call to OrchestratorState.ScheduleRetry re-sets
	// Claimed idempotently, so this is safe.
	// Use f.entry.Issue, not f.issue: reconciliation may have refreshed
	// the tracker state mid-run, and the retry must carry the live state.
	issue := f.entry.Issue
	// Carry the workspace so a failure retry whose issue later goes terminal
	// is cleaned through the §18.1 seam instead of leaking (#341).
	workspace := f.entry.Workspace
	st.Claimed[f.id] = struct{}{}
	st.ClaimedIssues[f.id] = issue
	close(f.done)
	o := f.o
	id := f.id
	identifier := f.identifier
	startupFailure := task.CopyStartupFailure(f.entry.LastStartupFailure)
	return func() {
		o.logRescheduleErr(o.scheduleFailureRetryWithStartupFailure(o.runCtx, issue, identifier, nextAttempt, runErr, workspace, startupFailure), id, identifier)
	}
}

func (f *finalizeRunOp) applyQuotaBackoff(st *OrchestratorState, elapsed time.Duration) (func(), bool) {
	var quota *runner.QuotaBackoffError
	if !errors.As(f.result.Err, &quota) {
		return nil, false
	}
	attempt := 0
	if f.attempt != nil && *f.attempt > 0 {
		attempt = *f.attempt
	}
	runErr := quota.Error()
	if !st.FinishRunFailed(f.id, f.entry, elapsed) {
		close(f.done)
		return nil, true
	}
	st.RecordEvent(RuntimeEvent{Kind: RuntimeEventFailed, IssueID: f.id, Identifier: f.identifier, Message: runErr})
	issue := f.entry.Issue
	workspace := f.entry.Workspace
	st.Claimed[f.id] = struct{}{}
	st.ClaimedIssues[f.id] = issue
	close(f.done)
	o := f.o
	id := f.id
	identifier := f.identifier
	return func() {
		o.logRescheduleErr(o.scheduleQuotaBackoffRetry(o.runCtx, issue, identifier, attempt, runErr, quota.RetryAfter, workspace), id, identifier)
	}, true
}
