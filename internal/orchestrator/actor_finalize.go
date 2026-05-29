package orchestrator

// actor_finalize.go holds finalizeRunOp, the stateOp that applies a worker's
// terminal RunResult — routing it to completion, failure backoff, quota
// backoff, or a continuation retry. See actor.go for the mutation discipline.

import (
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
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

func (f *finalizeRunOp) apply(st *OrchestratorState) func() {
	elapsed := f.result.Elapsed
	if elapsed == 0 {
		elapsed = time.Since(f.started)
	}
	if f.entry.InputRequired || f.result.InputRequired || runner.IsInputRequired(f.result.Err) {
		runErr := "input required"
		if f.result.Err != nil {
			runErr = f.result.Err.Error()
		}
		if !st.BlockRun(f.id, f.entry, f.entry.InputRequiredAt, runErr, elapsed) {
			close(f.done)
			return nil
		}
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventInputBlocked, IssueID: f.id, Identifier: f.identifier, Message: runErr})
		close(f.done)
		return nil
	}
	if f.result.ExternalBlocked {
		reason := strings.TrimSpace(f.result.BlockerReason)
		if reason == "" && f.result.Err != nil {
			reason = f.result.Err.Error()
		}
		if reason == "" {
			reason = "external dependency blocked run"
		}
		if !st.FinishRunExternalBlocked(f.id, f.entry, elapsed) {
			close(f.done)
			return nil
		}
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventCandidateBlocked, IssueID: f.id, Identifier: f.identifier, Message: reason})
		issue := f.entry.Issue
		workspace := f.entry.Workspace
		close(f.done)
		o := f.o
		identifier := f.identifier
		delay := f.result.BlockerRetryAfter
		return func() {
			_ = o.scheduleExternalBlockerRetry(o.runCtx, issue, identifier, reason, delay, workspace)
		}
	}
	if f.result.Err == nil {
		nextContinuationAttempt := f.entry.ContinuationAttempt + 1
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
		// SPEC §7.1 leaves continuation worker spawns unbounded; only apply
		// the orchestrator-side cap for runners that cannot enforce
		// agent.max_turns inside their own session. See issue #216.
		if !f.o.runnerEnforcesMaxTurns && nextContinuationAttempt >= f.o.maxTurns {
			if !st.FinishRunNonRetryableFailed(f.id, f.entry, elapsed) {
				close(f.done)
				return nil
			}
			msg := "clean continuation budget exhausted after " + strconv.Itoa(f.o.maxTurns) + " turns while tracker issue remained active"
			st.RecordEvent(RuntimeEvent{Kind: RuntimeEventFailed, IssueID: f.id, Identifier: f.identifier, Message: msg})
			// SPEC §13.1: the failed outcome must be expressible on stderr,
			// not only in the in-memory recent_events ring. Operators tailing
			// stderr otherwise see a run of "Succeeded" lines then silence.
			log.Printf("event=run_failed issue_id=%s issue_identifier=%s reason=continuation_budget_exhausted budget=%d", f.id, f.identifier, f.o.maxTurns)
			close(f.done)
			return nil
		}
		if !st.FinishRunSucceeded(f.id, f.entry, elapsed) {
			close(f.done)
			return nil
		}
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventCompleted, IssueID: f.id, Identifier: f.identifier, Message: "worker exited cleanly"})
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
		// A clean continuation is a new normal turn. Keep its retry entry
		// 1-based for the continuation budget, but do not carry it into future
		// failure backoff; otherwise many successful turns inflate the next
		// transient failure straight to the max backoff.
		nextAttempt := nextContinuationAttempt
		o := f.o
		identifier := f.identifier
		return func() {
			_ = o.scheduleContinuationRetry(o.runCtx, issue, identifier, nextAttempt, workspace)
		}
	}
	if f.entry.ReconcileCleanupWorkspace && runHasCompletedTurn(f.entry) {
		cleanup := f.o.reconciledWorkspaceCleanup(f.id, f.entry)
		if !st.FinishRunReconciledCancelled(f.id, f.entry, elapsed) {
			close(f.done)
			return nil
		}
		close(f.done)
		return cleanup
	}
	if f.result.NonRetryable {
		if !st.FinishRunNonRetryableFailed(f.id, f.entry, elapsed) {
			close(f.done)
			return nil
		}
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventFailed, IssueID: f.id, Identifier: f.identifier, Message: f.result.Err.Error()})
		// SPEC §13.1 failed outcome on stderr (see continuation-budget site).
		log.Printf("event=run_failed issue_id=%s issue_identifier=%s reason=non_retryable_runner_error error=%q", f.id, f.identifier, f.result.Err.Error())
		close(f.done)
		return nil
	}
	if cleanup, ok := f.applyQuotaBackoff(st, elapsed); ok {
		return cleanup
	}
	if f.entry.ReconcileCancel {
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
			return nil
		}
		close(f.done)
		return cleanup
	}
	// Schedule a retry with attempt+1. Per SPEC §4.1.5 the first run's
	// RetryAttempt is nil; the first retry is attempt 1, the second 2,
	// and so on. SPEC §8.4 / §16.6 expect the orchestrator to keep
	// scheduling retries indefinitely (with the exponential-backoff
	// ceiling) — only an explicit harness-hardening opt-in (SPEC §15.5)
	// caps the count. A negative maxFailureRetries leaves the SPEC
	// behavior in place; non-negative opts into the cap.
	nextAttempt := 1
	if f.attempt != nil {
		nextAttempt = *f.attempt + 1
	}
	runErr := f.result.Err.Error()
	if f.o.maxFailureRetries >= 0 && nextAttempt > f.o.maxFailureRetries {
		if !st.FinishRunNonRetryableFailed(f.id, f.entry, elapsed) {
			close(f.done)
			return nil
		}
		msg := "failure retry budget exhausted after " + strconv.Itoa(f.o.maxFailureRetries) + " retries: " + runErr
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventFailed, IssueID: f.id, Identifier: f.identifier, Message: msg})
		// SPEC §13.1 failed outcome on stderr (see continuation-budget site).
		log.Printf("event=run_failed issue_id=%s issue_identifier=%s reason=failure_retry_budget_exhausted attempts=%d error=%q", f.id, f.identifier, f.o.maxFailureRetries, runErr)
		close(f.done)
		return nil
	}
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
	identifier := f.identifier
	return func() {
		_ = o.scheduleFailureRetry(o.runCtx, issue, identifier, nextAttempt, runErr, workspace)
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
	identifier := f.identifier
	return func() {
		_ = o.scheduleQuotaBackoffRetry(o.runCtx, issue, identifier, attempt, runErr, quota.RetryAfter, workspace)
	}, true
}
