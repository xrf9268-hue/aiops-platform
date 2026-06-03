package orchestrator

// actor_reconcile.go holds the tracker-reconciliation path: the public
// Reconcile* entry points and the reconcile stateOps that bring actor state
// in line with a fresh tracker fetch. Workspace-cleanup machinery the
// reconcile ops defer to lives in actor_cleanup.go. See actor.go for the
// actor's mutation discipline.

import (
	"context"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// ReconciledWorkspace identifies the per-issue workspace that SPEC §18.1
// active-transition cleanup removes after a terminal-state run is cancelled
// and its worker has exited.
type ReconciledWorkspace struct {
	IssueID    IssueID
	Identifier string
	Path       string
	// Root is the workspace root Path was created under (captured at dispatch
	// time). The cleaner removes Path via SafeRemove against this root, so a
	// hot-reloaded workspace.root cannot make the removal fail the containment
	// check and silently skip cleanup. Empty falls back to the live snapshot
	// root.
	Root   string
	State  string
	Reason string
}

// WorkspaceCleaner removes a per-issue workspace through the same
// before_remove hook → remove → reconcile_workspace event sequence the
// startup sweep uses (SPEC §18.1). The orchestrator invokes it from the
// reconcile-cancel finalize path on a followup goroutine — after the worker
// goroutine has exited — so the directory is no longer in use. A nil cleaner
// disables active-transition cleanup (unit tests / legacy callers); the
// startup sweep still reclaims the directory on the next boot.
type WorkspaceCleaner interface {
	CleanupReconciledWorkspace(ctx context.Context, w ReconciledWorkspace)
}

// reconcileWorkspaceCleanupTimeout bounds the post-exit cleanup followup so a
// wedged before_remove hook cannot pin the followup goroutine forever. The
// before_remove hook enforces its own per-command timeout; this is the outer
// guard required of every followup that does external I/O (AGENTS.md
// "Replicate Elixir's implicit runtime guarantees explicitly in Go"). A
// package var so tests can shrink it.
var reconcileWorkspaceCleanupTimeout = 60 * time.Second

// terminalCleanupStateFetchTimeout bounds the deletion-time state recheck that
// prevents a stale terminal observation from deleting a workspace after the
// issue has already returned to active work.
var terminalCleanupStateFetchTimeout = 45 * time.Second
var terminalCleanupStateRetryDelay = continuationRetryDelay

// ReconcileStalledRuns implements SPEC §8.5 Part A / §16.3
// reconcile_stalled_runs: for each running issue compute elapsed time
// since the last observed runtime event (RunningEntry.LastEventAt,
// falling back to StartedAt before any event has been seen) and, if it
// exceeds stallTimeoutMs, cancel the worker so the finalize path
// schedules a retry. stallTimeoutMs <= 0 skips detection entirely (SPEC
// §6.4 default).
//
// The Codex app-server runner has its own self-stall detection; this
// orchestrator-side path closes the gap when the runner goroutine itself
// wedges or when a non-Codex runner (mock, shell-based claude) produces no
// StallError. Without this an issue with `LastEventAt` long in the past
// would stay claimed forever.
//
// wait is the per-tick budget for waiting on cancelled worker
// goroutines to exit (mirrors ReconcileTrackerIssuesAndWait). Use 0 for
// fire-and-forget cancel.
func (o *Orchestrator) ReconcileStalledRuns(ctx context.Context, stallTimeoutMs int, wait time.Duration) error {
	if stallTimeoutMs <= 0 {
		return nil
	}
	reply := make(chan []*RunningEntry, 1)
	if err := o.submit(ctx, &reconcileStalledRunsOp{
		timeout: time.Duration(stallTimeoutMs) * time.Millisecond,
		now:     time.Now(),
		result:  reply,
	}); err != nil {
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

// RefreshActiveTrackerIssues updates stored issue metadata for in-process runs
// and queued retries whose issues are still observed in the active set, without
// canceling any work. Use this when the active listing may be partial (so
// absence from issuesByID must not imply inactivity) but per-state capacity
// gates still need the latest tracker state.
func (o *Orchestrator) RefreshActiveTrackerIssues(ctx context.Context, issuesByID map[string]tracker.Issue, activeStates map[string]struct{}) error {
	done := make(chan struct{}, 1)
	if err := o.submit(ctx, &refreshActiveTrackerIssuesOp{issuesByID: issuesByID, activeStates: activeStates, done: done}); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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

// ReconcileInactiveTrackerIssuesAndWait cancels only issues explicitly observed
// in a terminal or configured inactive tracker state. Missing issues are
// treated as unknown instead of inactive because tracker adapters may return
// partial state listings under pagination caps.
//
// terminalStates is the lowercased terminal-state set. A running entry whose
// refreshed issue sits in a terminal state is flagged for workspace cleanup
// after its worker exits (SPEC §18.1); a merely-inactive cancel keeps the
// workspace. Pass an empty set to disable the cleanup gating (every cancel
// then behaves as inactive — workspace preserved).
func (o *Orchestrator) ReconcileInactiveTrackerIssuesAndWait(ctx context.Context, issuesByID map[string]tracker.Issue, terminalStates map[string]struct{}, workerExitTimeout time.Duration) error {
	reply := make(chan []*RunningEntry, 1)
	op := &reconcileInactiveTrackerIssuesOp{o: o, issuesByID: issuesByID, terminalStates: terminalStates, result: reply}
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

// refreshActiveTrackerIssuesOp refreshes RunningEntry.Issue and the matching
// ClaimedIssues snapshot for every in-process issue observed in the active set,
// without canceling anything. It is safe to call when the active listing may be
// partial, because absence from issuesByID is treated as "no information," not
// "now inactive."
type refreshActiveTrackerIssuesOp struct {
	issuesByID   map[string]tracker.Issue
	activeStates map[string]struct{}
	done         chan<- struct{}
}

func (r *refreshActiveTrackerIssuesOp) apply(st *OrchestratorState) func() {
	for id, run := range st.Running {
		if issue, ok := r.activeMatch(st, id); ok {
			refreshRunningIssue(run, issue)
		}
	}
	for id, retry := range st.RetryAttempts {
		if issue, ok := r.activeMatch(st, id); ok {
			retry.Issue = issue
		}
	}
	for id, blocked := range st.Blocked {
		if issue, ok := r.activeMatch(st, id); ok {
			blocked.Issue = issue
		}
	}
	return r.doneFollowup()
}

// doneFollowup returns the no-op off-actor followup that signals completion by
// closing the reply channel (the refresh pass cancels nothing).
func (r *refreshActiveTrackerIssuesOp) doneFollowup() func() {
	done := r.done
	return func() {
		if done != nil {
			close(done)
		}
	}
}

// activeMatch reports whether id's entry is still observed in the active
// listing. On a match it refreshes the ClaimedIssues snapshot (so per-state
// capacity gates see the latest state) and returns the fresh issue for the
// caller to store on the entry; a miss (absent / non-active) leaves the entry
// untouched — absence from a possibly-partial listing is "no information," not
// "now inactive."
func (r *refreshActiveTrackerIssuesOp) activeMatch(st *OrchestratorState, id IssueID) (tracker.Issue, bool) {
	issue, ok := r.issuesByID[string(id)]
	if !ok || !isActiveTrackerState(issue.State, r.activeStates) {
		return tracker.Issue{}, false
	}
	st.ClaimedIssues[id] = issue
	return issue, true
}

// reconcileStalledRunsOp is the actor-side handler for SPEC §8.5 Part A.
// It scans st.Running for entries whose LastEventAt (or StartedAt when no
// runtime event has been observed yet) is older than the configured
// stall budget and cancels the worker. The entry stays Claimed: the
// finalize op observes the canceled ctx as a worker failure and schedules
// a normal failure retry (no ReconcileCancel — operator cancellation and
// stall recovery have different semantics).
type reconcileStalledRunsOp struct {
	timeout time.Duration
	now     time.Time
	result  chan<- []*RunningEntry
}

func (r *reconcileStalledRunsOp) apply(st *OrchestratorState) func() {
	var canceled []*RunningEntry
	for id, run := range st.Running {
		if !r.isStalled(run) {
			continue
		}
		canceled = append(canceled, run)
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventFailed, IssueID: id, Identifier: run.Identifier, Message: "stalled"})
	}
	return r.cancelStalledFollowup(canceled)
}

// isStalled reports whether run has gone longer than the stall budget since its
// last observed runtime event, falling back to StartedAt before any event has
// been seen. A run with neither timestamp is treated as not-stalled rather than
// anchoring the stall window at the zero time, which would cancel every such
// (test-fixture) entry.
func (r *reconcileStalledRunsOp) isStalled(run *RunningEntry) bool {
	ref := run.LastEventAt
	if ref.IsZero() {
		ref = run.StartedAt
	}
	if ref.IsZero() {
		return false
	}
	return r.now.Sub(ref) > r.timeout
}

// cancelStalledFollowup returns the off-actor followup that cancels each stalled
// worker and publishes the canceled set to the reply channel.
func (r *reconcileStalledRunsOp) cancelStalledFollowup(canceled []*RunningEntry) func() {
	return func() {
		for _, entry := range canceled {
			if entry.CancelWorker != nil {
				// A stall cancel is a genuine failure (the run is stuck), not an
				// eligibility stop, so it carries no ErrReconcileCancel cause and
				// keeps its existing failure classification (#543).
				entry.CancelWorker(nil)
			}
		}
		if r.result != nil {
			r.result <- canceled
		}
	}
}

type reconcileInactiveTrackerIssuesOp struct {
	o              *Orchestrator
	issuesByID     map[string]tracker.Issue
	terminalStates map[string]struct{}
	result         chan<- []*RunningEntry
}

// apply releases every in-process entry whose issue is observed in the inactive
// listing, gating SPEC §18.1 workspace cleanup on a terminal transition. Each
// per-collection helper returns what to collect: running entries are cancelled
// (their workspace cleanup deferred to finalize), while blocked (input-required)
// and retry-queued (a clean §16.5 self-stop finalized the run) entries hold no
// live worker, so a terminal one's workspace is removed now via the shared
// WorkspaceCleaner. An entry whose issue is absent from the (possibly partial)
// listing is left untouched.
func (r *reconcileInactiveTrackerIssuesOp) apply(st *OrchestratorState) func() {
	var cancelEntries []*RunningEntry
	var terminalCleanups []reconciledCleanup
	for id, run := range st.Running {
		if entry := r.reconcileInactiveRun(st, id, run); entry != nil {
			cancelEntries = append(cancelEntries, entry)
		}
	}
	for id, retry := range st.RetryAttempts {
		if cleanup, ok := r.reconcileInactiveRetry(st, id, retry); ok {
			terminalCleanups = append(terminalCleanups, cleanup)
		}
	}
	for id, blocked := range st.Blocked {
		if cleanup, ok := r.reconcileInactiveBlocked(st, id, blocked); ok {
			terminalCleanups = append(terminalCleanups, cleanup)
		}
	}
	return r.o.reconcileCancelFollowup(cancelEntries, terminalCleanups, r.result)
}

// reconcileInactiveRun releases a running entry observed inactive, refreshes its
// stored issue, and (re)evaluates the SPEC §18.1 terminal cleanup gate against
// the CURRENT observation: a terminal transition flags the entry so finalize
// fires before_remove + remove with the terminal state labeling the
// reconcile_workspace event, while a merely-inactive cancel leaves it false to
// keep the workspace for reuse (upstream terminate_running_issue gating).
// Assigning the flag unconditionally clears one left by an earlier terminal blip
// when the issue has since flipped back to a non-terminal inactive state before
// the worker exits (Codex P2 follow-up). Returns nil when the issue is absent
// from the (possibly partial) listing — unknown, not inactive — so it is left
// untouched.
func (r *reconcileInactiveTrackerIssuesOp) reconcileInactiveRun(st *OrchestratorState, id IssueID, run *RunningEntry) *RunningEntry {
	issue, ok := r.issuesByID[string(id)]
	if !ok {
		return nil
	}
	st.ReleaseClaim(id)
	run.Issue = issue
	run.ReconcileCleanupWorkspace = isTerminalTrackerState(issue.State, r.terminalStates)
	run.ReconcileCancel = true
	return run
}

// reconcileInactiveRetry releases a retry-queued entry observed inactive and,
// when its issue is terminal, returns the workspace cleanup. Mirrors upstream
// handle_retry_issue_lookup (orchestrator.ex:1082-1100): a terminal retry cleans
// its workspace + releases, carrying the queued continuation so the
// deletion-time recheck can resume it (preserving the attempt + max-turn budget)
// if the issue flips back active before removal (#455); a merely-inactive one
// releases only, keeping the directory for reuse. A failure retry without a
// workspace (#341) yields no path and is released only. An issue absent from the
// (possibly partial) listing is unknown, not inactive, so the claim is left
// intact.
func (r *reconcileInactiveTrackerIssuesOp) reconcileInactiveRetry(st *OrchestratorState, id IssueID, retry *RetryEntry) (reconciledCleanup, bool) {
	issue, ok := r.issuesByID[string(id)]
	if !ok {
		return reconciledCleanup{}, false
	}
	defer st.ReleaseClaim(id)
	if !isTerminalTrackerState(issue.State, r.terminalStates) {
		return reconciledCleanup{}, false
	}
	w, okw := terminalWorkspaceForCleanup(id, retry.Identifier, retry.Workspace.Path, retry.Workspace.Root, issue.State)
	if !okw {
		return reconciledCleanup{}, false
	}
	return reconciledCleanup{workspace: w, continuation: continuationForRetry(retry)}, true
}

// reconcileInactiveBlocked releases a blocked entry observed inactive and, when
// its issue is terminal, returns the workspace cleanup. Mirrors upstream
// reconcile_blocked_issue_state's terminal branch (cleanup + release) vs. its
// non-terminal inactive branch (release only). An issue absent from the
// (possibly partial) listing is unknown, not inactive, so the claim is left
// intact.
func (r *reconcileInactiveTrackerIssuesOp) reconcileInactiveBlocked(st *OrchestratorState, id IssueID, blocked *BlockedEntry) (reconciledCleanup, bool) {
	issue, ok := r.issuesByID[string(id)]
	if !ok {
		return reconciledCleanup{}, false
	}
	defer st.ReleaseClaim(id)
	if !isTerminalTrackerState(issue.State, r.terminalStates) {
		return reconciledCleanup{}, false
	}
	w, okw := terminalWorkspaceForCleanup(id, blocked.Identifier, blocked.Workspace.Path, blocked.Workspace.Root, issue.State)
	if !okw {
		return reconciledCleanup{}, false
	}
	return reconciledCleanup{workspace: w}, true
}
func isActiveTrackerState(state string, activeStates map[string]struct{}) bool {
	if len(activeStates) == 0 {
		return false
	}
	_, ok := activeStates[strings.ToLower(strings.TrimSpace(state))]
	return ok
}

// isTerminalTrackerState reports whether state is in the lowercased terminal
// set. Used by reconciliation to gate SPEC §18.1 workspace cleanup on a
// terminal transition only (an empty set disables the gating).
func isTerminalTrackerState(state string, terminalStates map[string]struct{}) bool {
	if len(terminalStates) == 0 {
		return false
	}
	_, ok := terminalStates[strings.ToLower(strings.TrimSpace(state))]
	return ok
}
func runHasCompletedTurn(run *RunningEntry) bool {
	return run != nil && run.Session.TurnCount > 0
}

func runHasAgentHandoffWithoutCompletedTurn(run *RunningEntry) bool {
	return run != nil && run.AgentHandoffActivity && !runHasCompletedTurn(run)
}

// refreshRunningIssue updates a still-active run's stored issue and clears any
// terminal-cleanup flag left by an earlier terminal observation. The issue is
// active again, so its workspace must be preserved: without this reset a
// transient terminal blip (terminal seen on one tick, back to active before
// the worker exits) would still trigger removal at finalize (Codex P2).
func refreshRunningIssue(run *RunningEntry, issue tracker.Issue) {
	run.Issue = issue
	run.ReconcileCleanupWorkspace = false
}
