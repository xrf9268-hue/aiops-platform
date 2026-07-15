package orchestrator

// actor_cleanup.go holds the reconciled-workspace cleanup machinery: the
// begin/end cleanup guard ops and the followups that remove a terminal
// issue's workspace (or hand off to a continuation). See actor.go for the
// actor's mutation discipline.

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
)

// beginReconcileWorkspaceCleanup atomically reserves issue id for an
// active-transition workspace removal. It returns false — abort the cleanup —
// when the issue is already claimed (re-dispatched since it went terminal, so
// a new run now owns the deterministic workspace path) or another cleanup is
// already in flight for it. On success the issue is marked cleaning, which
// dispatchOp treats like a claim, so no run can be dispatched onto the path
// until endReconcileWorkspaceCleanup clears the mark. Because both this check
// and the dispatch claim run on the single actor goroutine, there is no
// check-then-delete race (the deletion happens entirely within the marked
// window). Returns false if the actor is unreachable (shutdown), leaving the
// directory for the next startup sweep.
func (o *Orchestrator) beginReconcileWorkspaceCleanup(id IssueID) bool {
	reply := make(chan bool, 1)
	if err := o.submit(o.runCtx, &beginWorkspaceCleanupOp{id: id, result: reply}); err != nil {
		return false
	}
	select {
	case ok := <-reply:
		return ok
	case <-o.runCtx.Done():
		return false
	}
}
func (o *Orchestrator) endReconcileWorkspaceCleanup(id IssueID) {
	done := make(chan struct{}, 1)
	if err := o.submit(o.runCtx, &endWorkspaceCleanupOp{id: id, done: done}); err != nil {
		return
	}
	select {
	case <-done:
	case <-o.runCtx.Done():
	}
}

type beginWorkspaceCleanupOp struct {
	id     IssueID
	result chan<- bool
}

func (o *beginWorkspaceCleanupOp) apply(st *OrchestratorState) func() {
	if st.IsClaimed(o.id) || st.IsCleaningWorkspace(o.id) {
		o.result <- false
		return nil
	}
	st.MarkCleaningWorkspace(o.id)
	o.result <- true
	return nil
}

type endWorkspaceCleanupOp struct {
	id   IssueID
	done chan<- struct{}
}

func (o *endWorkspaceCleanupOp) apply(st *OrchestratorState) func() {
	st.UnmarkCleaningWorkspace(o.id)
	o.done <- struct{}{}
	return nil
}

// reconciledCleanup pairs a terminal-transition workspace removal with the
// optional continuation to resume when the deletion-time recheck finds the
// issue active again. Blocked and failure/quota/external-blocker retry cleanups
// carry a nil continuation — they have no queued continuation attempt to
// preserve; a continuation retry carries its attempt + workspace so a terminal
// blip that flips back active reschedules the continuation rather than losing
// the attempt and max-turn budget (Codex review, PR #455).
type reconciledCleanup struct {
	workspace    ReconciledWorkspace
	continuation *continuationAfterSkippedCleanup
}

// reconcileCancelFollowup builds the off-actor followup both reconcile passes
// return: cancel each worker, then run any terminal workspace cleanups through
// the WorkspaceCleaner (before_remove + reconcile_workspace event), then deliver
// the cancelled entries to the waiting caller. Cleanup runs here, off the actor
// loop, so a slow before_remove hook cannot block state mutation. A cleanup
// carrying a continuation resumes it when the deletion-time recheck finds the
// issue active again instead of removing the workspace.
func (o *Orchestrator) reconcileCancelFollowup(cancelEntries []*RunningEntry, cleanups []reconciledCleanup, result chan<- []*RunningEntry) func() {
	return func() {
		for _, entry := range cancelEntries {
			if entry.CancelWorker != nil {
				// Cause = ErrReconcileCancel so the worker records this as a
				// supervised stop (runner_stopped), not a runner failure, for a
				// superseded run (#543).
				entry.CancelWorker(worker.ErrReconcileCancel)
			}
		}
		for _, c := range cleanups {
			o.runReconciledWorkspaceCleanup(c.workspace, c.continuation)
		}
		if result != nil {
			result <- cancelEntries
		}
	}
}

// reconciledWorkspaceCleanup returns a followup that removes the workspace of
// a terminal-state run whose worker has now exited (SPEC §18.1 active
// transition). It returns nil — leaving cleanup to the startup sweep — when
// the entry was not flagged for terminal cleanup, no cleaner is wired, or the
// run never recorded a workspace path. The returned func runs on the actor's
// followup goroutine, off the actor loop, so the before_remove hook and
// remove cannot block state mutation.
func (o *Orchestrator) reconciledWorkspaceCleanup(id IssueID, entry *RunningEntry) func() {
	return o.reconciledWorkspaceCleanupFollowup(id, entry, nil)
}
func (o *Orchestrator) reconciledWorkspaceCleanupOrContinuation(id IssueID, entry *RunningEntry, attempt int) func() {
	continuation := &continuationAfterSkippedCleanup{
		issue:                 entry.Issue,
		identifier:            entry.Identifier,
		attempt:               attempt,
		continuationTurnCount: entry.ContinuationTurnCount + continuationTurnDelta(entry),
		workspace:             entry.Workspace,
	}
	return o.reconciledWorkspaceCleanupFollowup(id, entry, continuation)
}
func (o *Orchestrator) reconciledWorkspaceCleanupFollowup(id IssueID, entry *RunningEntry, continuation *continuationAfterSkippedCleanup) func() {
	if entry == nil || !entry.ReconcileCleanupWorkspace {
		return nil
	}
	w, ok := terminalWorkspaceForCleanup(id, entry.Identifier, entry.Workspace.Path, entry.Workspace.Root, entry.Issue.State)
	if !ok || o.workspaceCleaner == nil {
		return nil
	}
	return func() { o.runReconciledWorkspaceCleanup(w, continuation) }
}

// terminalWorkspaceForCleanup builds the ReconciledWorkspace for a terminal
// active-transition removal, returning ok=false when there is no workspace
// path to remove. Shared by the running (finalize) and blocked (immediate)
// cleanup paths so both label the reconcile_workspace event identically.
func terminalWorkspaceForCleanup(id IssueID, identifier, path, root, state string) (ReconciledWorkspace, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return ReconciledWorkspace{}, false
	}
	return ReconciledWorkspace{
		IssueID:    id,
		Identifier: identifier,
		Path:       path,
		Root:       strings.TrimSpace(root),
		State:      state,
		Reason:     "terminal",
	}, true
}

// runReconciledWorkspaceCleanup invokes the WorkspaceCleaner under a bounded
// context. It is a no-op when no cleaner is wired (unit tests / legacy
// callers); the startup sweep then reclaims the directory on next boot. The
// before_remove hook enforces its own per-command timeout; the outer deadline
// here guards against a hook that ignores cancellation (AGENTS.md Go-runtime
// hardening). Callers must invoke it from a followup goroutine, never inside
// an apply.
func (o *Orchestrator) runReconciledWorkspaceCleanup(w ReconciledWorkspace, continuation *continuationAfterSkippedCleanup) {
	o.runReconciledWorkspaceCleanupAttempt(w, continuation, 0)
}

// runReconciledWorkspaceCleanupAttempt is one pass of the deletion-time recheck.
// attempt is 0 for the initial pass and N for the Nth backed-off retry after a
// failed or unknown tracker state refresh; on an unknown refresh it schedules the
// next attempt through retryReconciledWorkspaceCleanup (exponential backoff +
// give-up bound) instead of re-probing on a fixed interval forever (#675).
func (o *Orchestrator) runReconciledWorkspaceCleanupAttempt(w ReconciledWorkspace, continuation *continuationAfterSkippedCleanup, attempt int) {
	if o.workspaceCleaner == nil || strings.TrimSpace(w.Path) == "" {
		return
	}
	// Reserve the issue for cleanup on the actor. This aborts if the issue was
	// re-claimed since it went terminal (a new run owns the path), and while
	// reserved dispatchOp denies dispatch — so the removal below cannot race a
	// re-dispatch onto the same deterministic workspace path. Pairs with
	// endReconcileWorkspaceCleanup via defer so the mark never leaks.
	if !o.beginReconcileWorkspaceCleanup(w.IssueID) {
		return
	}
	defer o.endReconcileWorkspaceCleanup(w.IssueID)
	currentState, verdict := o.verifyReconciledWorkspaceStillTerminal(w)
	switch verdict {
	case reconciledWorkspaceStateUnknown:
		o.retryReconciledWorkspaceCleanup(w, continuation, attempt+1)
		return
	case reconciledWorkspaceStateAbsent:
		return
	case reconciledWorkspaceStateCurrentNonTerminal:
		if o.hasOperatorTerminalStop(w.IssueID) {
			return
		}
		o.continueAfterSkippedTerminalCleanup(continuation)
		return
	case reconciledWorkspaceStateCurrentTerminal:
	default:
		o.retryReconciledWorkspaceCleanup(w, continuation, attempt+1)
		return
	}
	if strings.TrimSpace(currentState) != "" {
		w.State = currentState
	}
	ctx, cancel := context.WithTimeout(o.runCtx, reconcileWorkspaceCleanupTimeout)
	defer cancel()
	o.workspaceCleaner.CleanupReconciledWorkspace(ctx, w)
}

func (o *Orchestrator) hasOperatorTerminalStop(id IssueID) bool {
	_, ok, err := o.LookupOperatorTerminalStop(o.runCtx, id)
	// Cleanup runs off-actor after the worker has already stopped. If the actor
	// is shutting down and cannot answer, fail closed: do not resume continuation
	// onto a workspace whose terminal stop status is unknown.
	return err != nil || ok
}

func (o *Orchestrator) continueAfterSkippedTerminalCleanup(continuation *continuationAfterSkippedCleanup) {
	if continuation == nil {
		o.queuePollWake()
		return
	}
	o.logRescheduleErr(o.scheduleContinuationRetry(o.runCtx, continuation.issue, continuation.identifier, continuation.attempt, continuation.continuationTurnCount, continuation.workspace), IssueID(continuation.issue.ID), continuation.identifier)
}

// retryReconciledWorkspaceCleanup reschedules the deletion-time state recheck
// after a failed or unknown tracker refresh. It uses the same exponential
// backoff as the failure-retry path (RetryScheduler.NextDelay with
// RetryKindFailure) so a persistently unavailable tracker is probed on a growing
// interval rather than every fixed second, and it gives up after
// maxReconciledCleanupStateRetries attempts: the orphaned workspace is then left
// for the next worker start's reconcile sweep to re-discover, which bounds both
// the goroutine churn and the extra load on an already-unhealthy tracker (#675).
// attempt is 1-based (the first retry after the initial pass).
func (o *Orchestrator) retryReconciledWorkspaceCleanup(w ReconciledWorkspace, continuation *continuationAfterSkippedCleanup, attempt int) {
	if attempt > maxReconciledCleanupStateRetries {
		// Give up rather than probe an unavailable tracker forever. A carried
		// continuation is deliberately NOT rescheduled here: resuming it would
		// bypass the deletion-time state recheck this loop could not complete,
		// defeating the §18.1 stale-terminal-deletion guard and the D35
		// operator-terminal-stop fail-closed gate. The clean-turn budget is
		// instead re-derived on the next poll re-dispatch — the same non-durable
		// recovery as a worker restart (SPEC §14.3). The drop is logged (not
		// silent) so the budget reset is observable (#675, AGENTS.md "no silent
		// caps").
		droppedTurns := 0
		if continuation != nil {
			droppedTurns = continuation.continuationTurnCount
		}
		log.Printf("event=reconcile_workspace_cleanup_giveup issue_id=%s issue_identifier=%s attempts=%d continuation_dropped=%t continuation_turns=%d reason=state_refresh_unavailable", w.IssueID, w.Identifier, attempt-1, continuation != nil, droppedTurns)
		return
	}
	delay := o.currentScheduler().NextDelay(RetryRequest{Kind: RetryKindFailure, Attempt: attempt})
	safeGo("orchestrator.reconcile_cleanup_retry", func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
			o.runReconciledWorkspaceCleanupAttempt(w, continuation, attempt)
		case <-o.runCtx.Done():
		}
	})
}

type reconciledWorkspaceStateVerdict uint8

const (
	reconciledWorkspaceStateUnknown reconciledWorkspaceStateVerdict = iota
	reconciledWorkspaceStateCurrentTerminal
	reconciledWorkspaceStateCurrentNonTerminal
	reconciledWorkspaceStateAbsent
)

func (o *Orchestrator) verifyReconciledWorkspaceStillTerminal(w ReconciledWorkspace) (string, reconciledWorkspaceStateVerdict) {
	resolver, terminalStates := o.currentRetryTerminalResolver()
	if resolver == nil || len(terminalStates) == 0 {
		return w.State, reconciledWorkspaceStateCurrentTerminal
	}
	ctx, cancel := context.WithTimeout(o.runCtx, terminalCleanupStateFetchTimeout)
	defer cancel()
	states, err := fetchIssueStates(ctx, resolver, []tracker.IssueRef{{ID: string(w.IssueID), Identifier: w.Identifier}})
	if err != nil {
		log.Printf("event=reconcile_workspace_skip issue_id=%s issue_identifier=%s reason=state_refresh_failed error=%q", w.IssueID, w.Identifier, err.Error())
		return "", reconciledWorkspaceStateUnknown
	}
	st, ok := states[string(w.IssueID)]
	if !ok {
		log.Printf("event=reconcile_workspace_skip issue_id=%s issue_identifier=%s reason=state_missing", w.IssueID, w.Identifier)
		return "", reconciledWorkspaceStateUnknown
	}
	switch st.Outcome {
	case tracker.IssueStateOutcomeUnknown:
		log.Printf("event=reconcile_workspace_skip issue_id=%s issue_identifier=%s reason=state_unknown", w.IssueID, w.Identifier)
		return "", reconciledWorkspaceStateUnknown
	case tracker.IssueStateOutcomeAbsent:
		log.Printf("event=reconcile_workspace_skip issue_id=%s issue_identifier=%s reason=issue_absent", w.IssueID, w.Identifier)
		return "", reconciledWorkspaceStateAbsent
	case tracker.IssueStateOutcomeCurrent:
	default:
		log.Printf("event=reconcile_workspace_skip issue_id=%s issue_identifier=%s reason=state_outcome_invalid outcome=%d", w.IssueID, w.Identifier, st.Outcome)
		return "", reconciledWorkspaceStateUnknown
	}
	if strings.TrimSpace(st.State) == "" {
		log.Printf("event=reconcile_workspace_skip issue_id=%s issue_identifier=%s reason=state_missing", w.IssueID, w.Identifier)
		return "", reconciledWorkspaceStateUnknown
	}
	if !isTerminalTrackerState(st.State, terminalStates) {
		log.Printf("event=reconcile_workspace_skip issue_id=%s issue_identifier=%s reason=state_not_terminal state=%q", w.IssueID, w.Identifier, st.State)
		return st.State, reconciledWorkspaceStateCurrentNonTerminal
	}
	return st.State, reconciledWorkspaceStateCurrentTerminal
}
