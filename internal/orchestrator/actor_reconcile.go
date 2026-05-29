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

// ReconcileTrackerIssues cancels or releases in-process work that is no longer
// tracker-eligible. It is the per-tick half of SPEC §2.1/#78: each tracker poll
// revalidates active runs against the latest tracker state and cancels workers
// whose issues moved out of active states.
func (o *Orchestrator) ReconcileTrackerIssues(ctx context.Context, issuesByID map[string]tracker.Issue, activeStates map[string]struct{}) error {
	return o.ReconcileTrackerIssuesAndWait(ctx, issuesByID, activeStates, nil, nil, 0)
}

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
// wedges or when a non-Codex runner (mock, codex exec) produces no
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

// ReconcileTrackerIssuesAndWait performs the same reconciliation as
// ReconcileTrackerIssues, then optionally waits for canceled workers to exit.
// This lets poll ticks provide prompt cancellation semantics without making the
// actor itself block on worker goroutines.
func (o *Orchestrator) ReconcileTrackerIssuesAndWait(ctx context.Context, issuesByID map[string]tracker.Issue, activeStates, terminalStates map[string]struct{}, refreshedByID map[string]tracker.Issue, wait time.Duration) error {
	reply := make(chan []*RunningEntry, 1)
	op := &reconcileTrackerIssuesOp{o: o, issuesByID: issuesByID, activeStates: activeStates, terminalStates: terminalStates, refreshedByID: refreshedByID, result: reply}
	if err := o.submit(ctx, op); err != nil {
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
		issue, ok := r.issuesByID[string(id)]
		if !ok || !isActiveTrackerState(issue.State, r.activeStates) || !sameServiceRoute(run.Issue, issue) {
			continue
		}
		refreshRunningIssue(run, issue)
		st.ClaimedIssues[id] = issue
	}
	for id, retry := range st.RetryAttempts {
		issue, ok := r.issuesByID[string(id)]
		if !ok || !isActiveTrackerState(issue.State, r.activeStates) || !sameServiceRoute(retry.Issue, issue) {
			continue
		}
		retry.Issue = issue
		st.ClaimedIssues[id] = issue
	}
	for id, blocked := range st.Blocked {
		issue, ok := r.issuesByID[string(id)]
		if !ok || !isActiveTrackerState(issue.State, r.activeStates) || !sameServiceRoute(blocked.Issue, issue) {
			continue
		}
		blocked.Issue = issue
		st.ClaimedIssues[id] = issue
	}
	done := r.done
	return func() {
		if done != nil {
			close(done)
		}
	}
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
		ref := run.LastEventAt
		if ref.IsZero() {
			ref = run.StartedAt
		}
		if ref.IsZero() {
			// Fresh dispatch without a StartedAt is exceedingly rare (a
			// test fixture). Skip rather than treat the Unix epoch as a
			// stall reference, which would cancel every such entry.
			continue
		}
		if r.now.Sub(ref) <= r.timeout {
			continue
		}
		canceled = append(canceled, run)
		st.RecordEvent(RuntimeEvent{Kind: RuntimeEventFailed, IssueID: id, Identifier: run.Identifier, Message: "stalled"})
	}
	return func() {
		for _, entry := range canceled {
			if entry.CancelWorker != nil {
				entry.CancelWorker()
			}
		}
		if r.result != nil {
			r.result <- canceled
		}
	}
}

type reconcileTrackerIssuesOp struct {
	o            *Orchestrator
	issuesByID   map[string]tracker.Issue
	activeStates map[string]struct{}
	// terminalStates + refreshedByID let this routing-aware pass terminal-gate
	// the SPEC §18.1 active-transition workspace cleanup for the run, blocked, and
	// retry entries it releases. Upstream is single-project and reaches
	// terminate_running_issue (with cleanup) in one pass; the aiops routing
	// extension cancels (and waits for) a routed terminal entry here, before the
	// terminal-aware inactive pass would see it, so the cleanup signal must be
	// computed in this pass. refreshedByID carries the refreshed post-transition
	// state that issuesByID (the active listing) no longer contains for a
	// now-inactive/terminal issue (#340).
	terminalStates map[string]struct{}
	refreshedByID  map[string]tracker.Issue
	result         chan<- []*RunningEntry
}

func (r *reconcileTrackerIssuesOp) apply(st *OrchestratorState) func() {
	var cancelEntries []*RunningEntry
	var blockedCleanups []reconciledCleanup
	for id, run := range st.Running {
		issue, ok := r.issuesByID[string(id)]
		if ok && isActiveTrackerState(issue.State, r.activeStates) && sameServiceRoute(run.Issue, issue) {
			// Refresh stored issue metadata so per-state capacity gates
			// (RunningCountByState, StateCapacityFull) see the latest tracker
			// state. Without this, an issue that moved between active states
			// keeps counting toward its dispatch-time bucket and a later poll
			// can exceed max_concurrent_agents_by_state for the new state.
			refreshRunningIssue(run, issue)
			st.ClaimedIssues[id] = issue
			continue
		}
		st.ReleaseClaim(id)
		run.ReconcileCancel = true
		r.flagTerminalRunCleanup(run, id)
		cancelEntries = append(cancelEntries, run)
	}
	for id, retry := range st.RetryAttempts {
		issue, ok := r.issuesByID[string(id)]
		if ok && isActiveTrackerState(issue.State, r.activeStates) && sameServiceRoute(retry.Issue, issue) {
			retry.Issue = issue
			st.ClaimedIssues[id] = issue
			continue
		}
		// A routed terminal retry must clean its workspace through the §18.1 seam
		// here — this pass releases it before the terminal-aware inactive pass
		// (reconcileInactiveTrackerIssuesOp) would see it, so deferring the
		// cleanup leaks the directory until the next startup sweep. continuationForRetry
		// carries the queued continuation so the deletion-time recheck resumes it
		// if the issue flips back active, matching the inactive pass.
		if w, okw := r.terminalRetryCleanup(id, retry); okw {
			blockedCleanups = append(blockedCleanups, reconciledCleanup{workspace: w, continuation: continuationForRetry(retry)})
		}
		st.ReleaseClaim(id)
	}
	for id, blocked := range st.Blocked {
		issue, ok := r.issuesByID[string(id)]
		if ok && isActiveTrackerState(issue.State, r.activeStates) && sameServiceRoute(blocked.Issue, issue) {
			blocked.Issue = issue
			st.ClaimedIssues[id] = issue
			continue
		}
		if w, okw := r.terminalBlockedCleanup(id, blocked); okw {
			blockedCleanups = append(blockedCleanups, reconciledCleanup{workspace: w})
		}
		st.ReleaseClaim(id)
	}
	return r.o.reconcileCancelFollowup(cancelEntries, blockedCleanups, r.result)
}

// flagTerminalRunCleanup sets ReconcileCleanupWorkspace on a routed run this
// pass is cancelling, based on its refreshed post-transition state. A terminal
// transition flags the cleanup so finalize fires before_remove +
// reconcile_workspace reason=terminal (SPEC §18.1), matching the non-routing
// inactive pass; a route-change or non-terminal inactive cancel leaves it false
// so the workspace is preserved for reuse. Assigned unconditionally so a flag
// left by an earlier terminal blip is cleared once the issue is no longer
// terminal, and false when the refresh did not observe this run (no terminal
// evidence → defer to the startup sweep rather than remove) (#340, Codex P2).
func (r *reconcileTrackerIssuesOp) flagTerminalRunCleanup(run *RunningEntry, id IssueID) {
	refreshed, ok := r.refreshedByID[string(id)]
	if !ok {
		run.ReconcileCleanupWorkspace = false
		return
	}
	run.Issue = refreshed
	run.ReconcileCleanupWorkspace = isTerminalTrackerState(refreshed.State, r.terminalStates)
}

// terminalBlockedCleanup builds the WorkspaceCleaner removal for a routed
// blocked entry this pass is releasing, but only when its refreshed state is
// terminal — so a terminal blocked transition fires before_remove +
// reconcile_workspace reason=terminal (mirroring reconcileInactiveTrackerIssuesOp
// and upstream reconcile_blocked_issue_state) while a route-change or
// non-terminal inactive transition keeps the workspace (#340).
func (r *reconcileTrackerIssuesOp) terminalBlockedCleanup(id IssueID, blocked *BlockedEntry) (ReconciledWorkspace, bool) {
	if blocked == nil {
		return ReconciledWorkspace{}, false
	}
	refreshed, ok := r.refreshedByID[string(id)]
	if !ok || !isTerminalTrackerState(refreshed.State, r.terminalStates) {
		return ReconciledWorkspace{}, false
	}
	return terminalWorkspaceForCleanup(id, blocked.Identifier, blocked.Workspace.Path, blocked.Workspace.Root, refreshed.State)
}

// terminalRetryCleanup builds the WorkspaceCleaner removal for a routed retry
// this pass is releasing, but only when its refreshed state is terminal — so a
// terminal continuation/failure retry fires before_remove + reconcile_workspace
// reason=terminal (mirroring reconcileInactiveTrackerIssuesOp's retry loop and
// upstream handle_retry_issue_lookup) while a route-change or non-terminal
// inactive transition keeps the directory for reuse (#341). Without it the
// routing pass released terminal retries with no §18.1 cleanup, leaking the
// workspace until the next startup sweep.
func (r *reconcileTrackerIssuesOp) terminalRetryCleanup(id IssueID, retry *RetryEntry) (ReconciledWorkspace, bool) {
	if retry == nil {
		return ReconciledWorkspace{}, false
	}
	refreshed, ok := r.refreshedByID[string(id)]
	if !ok || !isTerminalTrackerState(refreshed.State, r.terminalStates) {
		return ReconciledWorkspace{}, false
	}
	return terminalWorkspaceForCleanup(id, retry.Identifier, retry.Workspace.Path, retry.Workspace.Root, refreshed.State)
}
func sameServiceRoute(previous, current tracker.Issue) bool {
	return strings.TrimSpace(previous.ServiceName) == strings.TrimSpace(current.ServiceName)
}

type reconcileInactiveTrackerIssuesOp struct {
	o              *Orchestrator
	issuesByID     map[string]tracker.Issue
	terminalStates map[string]struct{}
	result         chan<- []*RunningEntry
}

func (r *reconcileInactiveTrackerIssuesOp) apply(st *OrchestratorState) func() {
	var cancelEntries []*RunningEntry
	// terminalCleanups collects terminal-state entries that hold no live worker
	// — blocked (input-required, stopped executing) and retry-queued (a clean
	// SPEC §16.5 self-stop finalized the run before scheduling the continuation
	// retry) — whose workspace must be removed now rather than deferred to the
	// finalize path the way running entries are. Every path goes through the
	// same WorkspaceCleaner so before_remove fires and a reconcile_workspace
	// event is emitted — mirroring upstream reconcile_blocked_issue_state and
	// handle_retry_issue_lookup, which clean the workspace only on a terminal
	// transition. A continuation retry also carries its continuation so the
	// deletion-time recheck can resume it (preserving the queued attempt and
	// max-turn budget) if the issue flips back active before removal.
	var terminalCleanups []reconciledCleanup
	for id, run := range st.Running {
		issue, ok := r.issuesByID[string(id)]
		if !ok {
			continue
		}
		st.ReleaseClaim(id)
		// Refresh the stored issue and (re)evaluate the terminal cleanup gate
		// against the CURRENT observation every tick. A terminal transition
		// flags the entry so finalize fires before_remove + remove (SPEC §18.1
		// active transition) with the terminal state labeling the
		// reconcile_workspace event; a merely-inactive cancel leaves it false,
		// keeping the workspace for reuse (upstream terminate_running_issue
		// cleanup gating). Assigning unconditionally — rather than only setting
		// true — clears a flag left by an earlier terminal blip when the issue
		// has since flipped back to a non-terminal inactive state before the
		// worker exits (Codex P2 follow-up).
		run.Issue = issue
		run.ReconcileCleanupWorkspace = isTerminalTrackerState(issue.State, r.terminalStates)
		run.ReconcileCancel = true
		cancelEntries = append(cancelEntries, run)
	}
	for id, retry := range st.RetryAttempts {
		issue, ok := r.issuesByID[string(id)]
		if !ok {
			continue
		}
		// Mirror upstream handle_retry_issue_lookup (orchestrator.ex:1082-1100):
		// a retry whose issue is now terminal cleans its workspace + releases;
		// a merely-inactive (non-terminal) one releases only, keeping the
		// directory for possible reuse. The continuation retry carries the
		// finalized run's workspace (#341); a failure retry without one yields
		// no path and is released only. continuationForRetry threads the queued
		// continuation attempt + workspace through the cleanup so a terminal
		// observation the deletion-time recheck finds active again resumes the
		// continuation instead of resetting ContinuationAttempt to 0 on the next
		// poll (Codex review, PR #455).
		if isTerminalTrackerState(issue.State, r.terminalStates) {
			if w, okw := terminalWorkspaceForCleanup(id, retry.Identifier, retry.Workspace.Path, retry.Workspace.Root, issue.State); okw {
				terminalCleanups = append(terminalCleanups, reconciledCleanup{workspace: w, continuation: continuationForRetry(retry)})
			}
		}
		st.ReleaseClaim(id)
	}
	for id, blocked := range st.Blocked {
		issue, ok := r.issuesByID[string(id)]
		if !ok {
			continue
		}
		if isTerminalTrackerState(issue.State, r.terminalStates) {
			if w, okw := terminalWorkspaceForCleanup(id, blocked.Identifier, blocked.Workspace.Path, blocked.Workspace.Root, issue.State); okw {
				terminalCleanups = append(terminalCleanups, reconciledCleanup{workspace: w})
			}
		}
		st.ReleaseClaim(id)
	}
	return r.o.reconcileCancelFollowup(cancelEntries, terminalCleanups, r.result)
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

// refreshRunningIssue updates a still-active run's stored issue and clears any
// terminal-cleanup flag left by an earlier terminal observation. The issue is
// active again, so its workspace must be preserved: without this reset a
// transient terminal blip (terminal seen on one tick, back to active before
// the worker exits) would still trigger removal at finalize (Codex P2).
func refreshRunningIssue(run *RunningEntry, issue tracker.Issue) {
	run.Issue = issue
	run.ReconcileCleanupWorkspace = false
}
