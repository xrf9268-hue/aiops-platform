package orchestrator

// actor_retry.go holds the retry subsystem: the RetryScheduler seam (SPEC
// §8.4 backoff / §16.6 continuation), the Orchestrator schedule* entry
// points, and the retry stateOps that fire and re-defer retries. See
// actor.go for the actor's mutation discipline.

import (
	"context"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// RetryKind identifies why an issue is waiting in the retry queue.
type RetryKind string

const (
	RetryKindFailure      RetryKind = "failure"
	RetryKindContinuation RetryKind = "continuation"
	RetryKindQuotaBackoff RetryKind = "quota_backoff"
)

// RetryRequest describes the retry being scheduled. Attempt is the 1-based
// failure retry attempt for RetryKindFailure. Continuation retries ignore it
// and always use the short SPEC §16.6 delay.
type RetryRequest struct {
	Kind          RetryKind
	Attempt       int
	DelayOverride time.Duration
}

// RetryScheduler implements the SPEC retry delays: clean continuation retries
// use one second; failure retries use delay=min(10s*2^(attempt-1), MaxBackoff).
type RetryScheduler struct {
	MaxBackoff time.Duration
}

const continuationRetryDelay = time.Second

// retryFetchTimeout caps the SPEC §16.6 candidate-fetch call a fired
// failure-retry timer makes. Tracker clients enforce per-request
// network timeouts on their own (Linear / Gitea / GitHub all 30s per
// PR #303), but a defensive ceiling here means the orchestrator does
// not depend on every adapter to do so: if a future tracker client
// (or a transport bug) silently drops cancellation, the fetch still
// returns within this bound and the SPEC's "retry poll failed"
// reschedule path takes over instead of leaving the issue stuck in
// Claimed/RetryAttempts forever. The value is comfortably larger
// than any adapter's own deadline so an honest slow tracker is not
// punished. A package-level var (not const) so tests can shrink the
// bound; runtime callers must not mutate it.
var retryFetchTimeout = 45 * time.Second

// NextDelay implements Scheduler.
func (s RetryScheduler) NextDelay(req RetryRequest) time.Duration {
	if req.DelayOverride > 0 {
		return req.DelayOverride
	}
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

type continuationAfterSkippedCleanup struct {
	issue                 tracker.Issue
	identifier            string
	attempt               int
	continuationTurnCount int
	workspace             Workspace
}

// continuationForRetry returns the continuation to resume when a queued
// continuation retry's terminal-cleanup recheck (verifyReconciledWorkspaceStillTerminal)
// finds the issue active again. Only RetryKindContinuation entries carry a
// continuation attempt worth preserving; for every other retry kind it returns
// nil so the recheck falls back to a plain poll wake (their re-dispatch does not
// depend on a preserved ContinuationAttempt). The
// reconcile pass builds this before ReleaseClaim drops the entry so the
// off-actor cleanup can reschedule the same attempt + workspace instead of
// resetting ContinuationAttempt to 0 on the next poll (Codex review, PR #455).
func continuationForRetry(retry *RetryEntry) *continuationAfterSkippedCleanup {
	if retry == nil || retry.Kind != RetryKindContinuation {
		return nil
	}
	return &continuationAfterSkippedCleanup{
		issue:                 retry.Issue,
		identifier:            retry.Identifier,
		attempt:               retry.Attempt,
		continuationTurnCount: retry.ContinuationTurnCount,
		workspace:             retry.Workspace,
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
	return o.scheduleFailureRetry(ctx, issue, identifier, attempt, runErr, Workspace{})
}

// scheduleFailureRetry is the internal failure-retry entry point that also
// carries the prior run's workspace forward. A failure retry whose issue is
// later observed terminal (by the reconcile pass or the SPEC §16.6 retry-fire
// resolution) cleans this directory through the §18.1 seam rather than leaking
// it until the next startup sweep (#341). The reschedule paths (capacity defer,
// retry-poll-failed) thread the existing entry's workspace through so it
// survives across attempts.
func (o *Orchestrator) scheduleFailureRetry(ctx context.Context, issue tracker.Issue, identifier string, attempt int, runErr string, workspace Workspace) error {
	return o.scheduleRetry(ctx, issue, identifier, RetryRequest{Kind: RetryKindFailure, Attempt: attempt}, attempt, runErr, workspace, 0)
}

func (o *Orchestrator) scheduleFailureRetryWithStartupFailure(ctx context.Context, issue tracker.Issue, identifier string, attempt int, runErr string, workspace Workspace, startupFailure *task.StartupFailure) error {
	return o.scheduleRetryWithStartupFailure(ctx, issue, identifier, RetryRequest{Kind: RetryKindFailure, Attempt: attempt}, attempt, runErr, workspace, startupFailure, 0)
}

func (o *Orchestrator) scheduleQuotaBackoffRetry(ctx context.Context, issue tracker.Issue, identifier string, attempt int, runErr string, retryAfter time.Duration, workspace Workspace) error {
	return o.scheduleRetry(ctx, issue, identifier, RetryRequest{Kind: RetryKindQuotaBackoff, Attempt: attempt, DelayOverride: retryAfter}, attempt, runErr, workspace, 0)
}

// logRescheduleErr records a dropped reschedule submit from a deferred
// followup closure. submit returns context.Canceled when the actor is tearing
// down (the benign shutdown path), so that case is intentionally silent. Any
// other error means the issue silently fell off the retry chain, so it is
// surfaced as a structured log line rather than discarded (#636).
func (o *Orchestrator) logRescheduleErr(err error, id IssueID, identifier string) {
	if err == nil || errors.Is(err, context.Canceled) {
		return
	}
	if strings.TrimSpace(identifier) == "" {
		log.Printf("event=reschedule_submit_failed issue_id=%s error=%q", id, err)
		return
	}
	log.Printf("event=reschedule_submit_failed issue_id=%s issue_identifier=%s error=%q", id, identifier, err)
}

// scheduleContinuationRetry queues the short SPEC §16.6 wake after a clean
// turn. workspace carries the finalized run's directory so a continuation
// whose issue is later seen terminal can be cleaned through the §18.1 seam
// (#341); pass the finalized RunningEntry.Workspace.
func (o *Orchestrator) scheduleContinuationRetry(ctx context.Context, issue tracker.Issue, identifier string, attempt, continuationTurnCount int, workspace Workspace) error {
	return o.scheduleRetry(ctx, issue, identifier, RetryRequest{Kind: RetryKindContinuation, Attempt: attempt}, attempt, "", workspace, continuationTurnCount)
}
func (o *Orchestrator) scheduleRetry(ctx context.Context, issue tracker.Issue, identifier string, req RetryRequest, attempt int, runErr string, workspace Workspace, continuationTurnCount int) error {
	return o.scheduleRetryWithStartupFailure(ctx, issue, identifier, req, attempt, runErr, workspace, nil, continuationTurnCount)
}

func (o *Orchestrator) scheduleRetryWithStartupFailure(ctx context.Context, issue tracker.Issue, identifier string, req RetryRequest, attempt int, runErr string, workspace Workspace, startupFailure *task.StartupFailure, continuationTurnCount int) error {
	op := &scheduleRetryOp{
		o:                     o,
		issue:                 issue,
		identifier:            identifier,
		attempt:               attempt,
		runErr:                runErr,
		kind:                  req.Kind,
		req:                   req,
		workspace:             workspace,
		startupFailure:        task.CopyStartupFailure(startupFailure),
		continuationTurnCount: continuationTurnCount,
	}
	return o.submit(ctx, op)
}

// scheduleRetryOp is the actor-side half of ScheduleRetry: it stores
// the RetryEntry through OrchestratorState.ScheduleRetry (which stops
// any prior timer for the same id) and starts a new timer whose
// callback submits a retryFireOp.
type scheduleRetryOp struct {
	o                     *Orchestrator
	issue                 tracker.Issue
	identifier            string
	attempt               int
	runErr                string
	kind                  RetryKind
	req                   RetryRequest
	workspace             Workspace
	startupFailure        *task.StartupFailure
	continuationTurnCount int
}

func (s *scheduleRetryOp) apply(st *OrchestratorState) func() {
	id := IssueID(s.issue.ID)
	o := s.o
	issue := s.issue
	attempt := s.attempt
	kind := s.kind
	delay := o.currentScheduler().NextDelay(s.req)
	dueAt := time.Now().Add(delay)
	var quotaBackoffDueAt time.Time
	if kind == RetryKindQuotaBackoff && s.req.DelayOverride > 0 {
		quotaBackoffDueAt = dueAt
	}
	// time.AfterFunc schedules immediately and is cheap (no goroutine
	// until fire), so we can safely create the timer on the actor
	// without blocking. ScheduleRetry needs the Timer set on the entry
	// before storing so a stale prior timer is stopped atomically.
	entry := &RetryEntry{
		Issue:                 s.issue,
		IssueID:               id,
		Identifier:            s.identifier,
		Attempt:               attempt,
		DueAt:                 dueAt,
		Error:                 s.runErr,
		Kind:                  s.kind,
		QuotaBackoffDueAt:     quotaBackoffDueAt,
		StartupFailure:        task.CopyStartupFailure(s.startupFailure),
		ContinuationTurnCount: s.continuationTurnCount,
		Workspace:             s.workspace,
	}
	entry.Timer = time.AfterFunc(delay, func() {
		defer recoverPanic("orchestrator.retry_timer")
		_ = o.submit(o.runCtx, &retryFireOp{
			o:       o,
			id:      id,
			issue:   issue,
			attempt: attempt,
			kind:    kind,
		})
	})
	st.ScheduleRetry(entry)
	return nil
}

// retryFireOp is the actor-side handler for a fired retry timer. The
// SPEC §16.6 retry path is "if the entry is still queued, re-dispatch;
// otherwise drop the fire." Two timers may race here in pathological
// cases (a ScheduleRetry replace where the prior timer's Stop missed
// because the callback was already queued); the attempt/kind equality
// guard makes the stale fire a no-op.
type retryFireOp struct {
	o       *Orchestrator
	id      IssueID
	issue   tracker.Issue
	attempt int
	kind    RetryKind
}

// matchingRetryEntry returns the queued RetryEntry for id only while it still
// matches this fire's (attempt, kind) identity, and false otherwise. The two
// false cases both mean "drop this fire as a no-op": (1) the entry is absent —
// already consumed by reconciliation's ReleaseClaim or by an earlier fire of
// the same retry; and (2) a newer ScheduleRetry replaced the entry with a
// different attempt/kind, so an older timer fired late (the Stop()-missed race,
// SPEC §16.6). It mirrors upstream pop_retry_attempt_state's :missing result
// (orchestrator.ex:1045-1060) minus the pop — the Go port keeps the entry in
// RetryAttempts across the async candidate fetch and re-checks this guard at
// every actor re-entry (retryFireOp, retryPollFailedOp, retryFireAfterFetchOp).
// The returned *RetryEntry is the live pointer from the map, so callers'
// in-place mutations (entry.Timer, entry.Issue, entry.Identifier) still reach
// the stored struct.
func matchingRetryEntry(st *OrchestratorState, id IssueID, attempt int, kind RetryKind) (*RetryEntry, bool) {
	entry, ok := st.RetryAttempts[id]
	if !ok {
		return nil, false
	}
	if entry.Attempt != attempt || entry.Kind != kind {
		return nil, false
	}
	return entry, true
}

func (r *retryFireOp) apply(st *OrchestratorState) func() {
	entry, ok := matchingRetryEntry(st, r.id, r.attempt, r.kind)
	if !ok {
		return nil
	}
	if entry.Kind == RetryKindContinuation {
		return r.fireWakeSignal(entry)
	}
	o := r.o
	if lister := o.currentCandidateLister(); lister != nil {
		return r.fireWithCandidateFetch(entry, lister)
	}
	// No CandidateLister wired (unit-test fallback): dispatch directly from the
	// cached entry.Issue. Production always wires one via RuntimePoller.
	return retryFireDispatchTail(st, entry, r.id, r.attempt, o)
}

// fireWakeSignal handles a fired continuation retry. A continuation is a
// wake-up signal, not a dispatch: it must not spawn from the cached issue
// snapshot or carry failure-retry accounting. The entry stays in RetryAttempts
// (timer cleared) and the followup wakes the poll loop; a poll then observes the
// issue still active and calls RequestDispatchAfterTrackerRecheck, which consumes
// the entry before spawning the next normal turn.
func (r *retryFireOp) fireWakeSignal(entry *RetryEntry) func() {
	entry.Timer = nil
	o := r.o
	return func() { o.wakeRetryPollLoop() }
}

// fireWithCandidateFetch builds the SPEC §16.6 on_retry_timer followup for a
// fired failure/quota retry when a CandidateLister is wired: (1) fetch active
// candidates, (2) reschedule with "retry poll failed" on fetch error, (3)
// release the claim if the issue is absent, and only then (4/5) dispatch from
// the refreshed tracker state. The actor-side timer clear happens here (before
// the followup is returned) so a subsequent ScheduleRetry's Stop() is a no-op
// on the already-fired timer; the entry is NOT popped — it stays in
// RetryAttempts so the post-fetch ops re-validate it under actor serialization.
// id/attempt/kind/identifier are captured by value (not the op or the entry
// pointer) because the returned closure runs off-actor.
func (r *retryFireOp) fireWithCandidateFetch(entry *RetryEntry, lister ActiveIssueLister) func() {
	entry.Timer = nil
	o := r.o
	id := r.id
	attempt := r.attempt
	kind := r.kind
	identifier := entry.Identifier
	return func() {
		// Per-fetch timeout. The followup runs on a fresh goroutine outside the
		// actor, and o.runCtx has no deadline of its own. A tracker client that
		// ignores ctx cancellation would otherwise pin this goroutine
		// indefinitely — entry.Timer is already cleared and no retryFireOp would
		// be resubmitted, leaving the issue stuck in Claimed/RetryAttempts
		// forever. Surfacing the timeout as a "retry poll failed" reschedule
		// keeps the SPEC §16.6 backoff window the only source of forward progress.
		fetchCtx, cancel := context.WithTimeout(o.runCtx, retryFetchTimeout)
		defer cancel()
		issues, fetchErr := lister.ListActiveIssues(fetchCtx)
		found := findIssueByID(issues, id)
		if fetchErr != nil && found == nil {
			// Either the whole fetch failed (including timeout), or a
			// multi-tracker partial failure happened on the tracker that owns
			// this issue. We can't tell "absent" from "tracker down" — treat as
			// fetch failure per SPEC §16.6 and reschedule with the typed error.
			_ = o.submit(o.runCtx, &retryPollFailedOp{
				o:        o,
				id:       id,
				attempt:  attempt,
				kind:     kind,
				fetchErr: fetchErr,
			})
			return
		}
		// found==nil means the issue is not in the active candidate set:
		// terminal, merely-inactive, or deleted. The active-only fetch cannot
		// tell them apart, so resolve the actual state (the way the reconcile
		// pass does) to recover upstream handle_retry_issue_lookup's terminal
		// branch — terminal → clean the workspace + release, every other absence
		// → release only (#341).
		terminal := false
		terminalState := ""
		if found == nil {
			terminal, terminalState = resolveRetryTerminalState(o, id, identifier)
		}
		_ = o.submit(o.runCtx, &retryFireAfterFetchOp{
			o:             o,
			id:            id,
			attempt:       attempt,
			kind:          kind,
			found:         found,
			terminal:      terminal,
			terminalState: terminalState,
		})
	}
}

// resolveRetryTerminalState reproduces the reconcile pass's terminal-vs-absent
// classification for a fired failure-retry whose issue is absent from the active
// candidate set, recovering upstream handle_retry_issue_lookup's terminal branch
// that the active-only fetch collapses into plain absence (#341). It returns
// (true, state) only when a resolver is wired and reports a terminal state;
// every other outcome (no resolver, fetch error, empty/non-terminal state)
// returns (false, "") so the caller keeps the release-only default. Runs
// off-actor; the lookup gets its OWN timeout budget rather than reusing the
// already-consumed candidate-fetch ctx, because a slow-but-successful fetch near
// the deadline would otherwise fail this call immediately with
// context-deadline-exceeded, dropping a terminal issue onto the release-only
// path and leaking its workspace (Codex P2). Derived from runCtx so actor
// shutdown still cancels it.
func resolveRetryTerminalState(o *Orchestrator, id IssueID, identifier string) (terminal bool, terminalState string) {
	resolver, terminalStates := o.currentRetryTerminalResolver()
	if resolver == nil || len(terminalStates) == 0 {
		return false, ""
	}
	resolveCtx, resolveCancel := context.WithTimeout(o.runCtx, retryFetchTimeout)
	statesByID, rerr := fetchIssueStates(resolveCtx, resolver, []tracker.IssueRef{{
		ID:         string(id),
		Identifier: identifier,
	}})
	resolveCancel()
	if rerr != nil {
		return false, ""
	}
	s := strings.TrimSpace(statesByID[string(id)].State)
	if s == "" || !isTerminalTrackerState(s, terminalStates) {
		return false, ""
	}
	return true, s
}

// retryFireDispatchTail runs the post-fetch tail of a failure-retry fire:
// honor global + per-state capacity gates, then either spawn or reschedule
// via the configured backoff. Shared between the legacy direct-dispatch
// path (no CandidateLister) and the SPEC §16.6 post-fetch path.
func retryFireDispatchTail(st *OrchestratorState, entry *RetryEntry, id IssueID, attempt int, o *Orchestrator) func() {
	// Use entry.Issue rather than any timer-captured snapshot: reconciliation
	// (and the SPEC §16.6 candidate fetch) may have refreshed the tracker
	// state, and both the per-state capacity gate and the spawned worker
	// must see the live state.
	issue := entry.Issue
	if suppressRetryFireAfterOperatorStop(st, entry, id) {
		return nil
	}
	if st.RunningCount() >= st.MaxConcurrentAgents {
		// Retry timers must obey the same capacity gate as fresh dispatch.
		// Mirror upstream handle_active_retry (orchestrator.ex:1142-1161):
		// reschedule through the configured backoff with attempt+1 and a
		// typed "no available orchestrator slots" error instead of arming a
		// short 100ms re-fire timer.
		return capacityDeferRetry(st, id, entry, attempt, o)
	}
	if st.StateCapacityFull(issue.State) {
		// Retry timers must also obey per-state capacity gates. Same
		// upstream-aligned reschedule shape as the global-cap branch.
		return capacityDeferRetry(st, id, entry, attempt, o)
	}
	// Consume the retry entry but keep Claimed: the re-dispatch
	// immediately re-adds Running, and dropping Claimed in between
	// would let a concurrent tick race in.
	delete(st.RetryAttempts, id)
	return func() {
		var retryAttempt *int
		if entry.Kind != RetryKindQuotaBackoff || attempt > 0 {
			a := attempt
			retryAttempt = &a
		}
		o.spawn(id, issue, retryAttempt, 0, 0, 0, entry.Workspace)
	}
}

func suppressRetryFireAfterOperatorStop(st *OrchestratorState, entry *RetryEntry, id IssueID) bool {
	if !st.IsOperatorTerminalStopped(id) {
		return false
	}
	if stop, first := st.RecordOperatorTerminalDispatchSuppressed(id, entry.Issue, "retry_fire_after_operator_terminal_stop"); first {
		st.RecordEvent(RuntimeEvent{
			Kind:       RuntimeEventOperatorTerminalStopDispatchSuppressed,
			IssueID:    id,
			Identifier: stop.Identifier,
			Message:    "retry dispatch suppressed after Operator Terminal Stop",
		})
	}
	st.ReleaseClaim(id)
	return true
}

// capacityDeferRetry mirrors upstream handle_active_retry's no-slots
// branch (elixir/lib/symphony_elixir/orchestrator.ex:1142-1161): when a
// fired failure-retry observes a full global or per-state capacity gate,
// reschedule the retry through the configured backoff (SPEC §8.4), bump
// the attempt counter, and stamp the entry with the upstream-canonical
// "no available orchestrator slots" error so operators can observe
// sustained capacity pressure in the runtime event stream. The prior
// 100ms re-fire loop bypassed the backoff formula, left the attempt
// counter frozen across thousands of re-fires, and produced no runtime
// event for the cap-pressure case.
func capacityDeferRetry(st *OrchestratorState, id IssueID, entry *RetryEntry, attempt int, o *Orchestrator) func() {
	if o.runCtx.Err() != nil {
		// Mirror retryPollFailedOp's shutdown guard (actor.go above):
		// the followup's ScheduleRetry would fail submit anyway, so
		// recording a cap-pressure event during shutdown would only
		// leak a misleading line into shutdown logs.
		return nil
	}
	issue := entry.Issue
	identifier := entry.Identifier
	kind := entry.Kind
	workspace := entry.Workspace
	const runErr = "no available orchestrator slots"
	nextAttempt := attempt + 1
	if kind == RetryKindQuotaBackoff {
		nextAttempt = attempt
	}
	startupFailure := task.CopyStartupFailure(entry.StartupFailure)
	quotaRetryAfter := entry.quotaBackoffDelayOverride(time.Now())
	st.RecordEvent(RuntimeEvent{
		Kind:       RuntimeEventFailed,
		IssueID:    id,
		Identifier: identifier,
		Message:    runErr,
	})
	return func() {
		// Carry the workspace across the reschedule so the §18.1 terminal
		// cleanup gate still has a path on a later attempt (#341).
		if kind == RetryKindQuotaBackoff {
			o.logRescheduleErr(o.scheduleQuotaBackoffRetry(o.runCtx, issue, identifier, nextAttempt, runErr, quotaRetryAfter, workspace), id, identifier)
			return
		}
		o.logRescheduleErr(o.scheduleFailureRetryWithStartupFailure(o.runCtx, issue, identifier, nextAttempt, runErr, workspace, startupFailure), id, identifier)
	}
}
func findIssueByID(issues []tracker.Issue, id IssueID) *tracker.Issue {
	for i := range issues {
		if IssueID(issues[i].ID) == id {
			out := issues[i]
			return &out
		}
	}
	return nil
}

// retryPollFailedOp implements SPEC §16.6 step 1 alt: when a fired
// failure-retry timer's candidate fetch fails, reschedule the same
// retry (attempt+1) with a typed "retry poll failed" error so the
// next backoff window can try the fetch again.
type retryPollFailedOp struct {
	o        *Orchestrator
	id       IssueID
	attempt  int
	kind     RetryKind
	fetchErr error
}

func (r *retryPollFailedOp) apply(st *OrchestratorState) func() {
	entry, ok := matchingRetryEntry(st, r.id, r.attempt, r.kind)
	if !ok {
		// Entry absent (reconciliation released the claim between fetch and
		// apply) or replaced by a newer ScheduleRetry that owns the re-dispatch.
		return nil
	}
	o := r.o
	if o.runCtx.Err() != nil {
		// Orchestrator is shutting down between the fetch and this apply.
		// The followup's ScheduleRetry would fail anyway; recording the
		// event and then dropping the followup would only leak a
		// "retry poll failed" line into shutdown logs while the entry
		// goes nowhere. Drop silently and let process exit reclaim the
		// entry along with everything else.
		return nil
	}
	issue := entry.Issue
	identifier := entry.Identifier
	// Carry the workspace across the reschedule so the §18.1 terminal cleanup
	// gate still has a path on a later attempt (#341).
	workspace := entry.Workspace
	startupFailure := task.CopyStartupFailure(entry.StartupFailure)
	quotaRetryAfter := entry.quotaBackoffDelayOverride(time.Now())
	nextAttempt := r.attempt + 1
	if r.kind == RetryKindQuotaBackoff {
		nextAttempt = r.attempt
	}
	runErr := "retry poll failed"
	if r.fetchErr != nil {
		runErr = "retry poll failed: " + r.fetchErr.Error()
	}
	st.RecordEvent(RuntimeEvent{
		Kind:       RuntimeEventFailed,
		IssueID:    r.id,
		Identifier: identifier,
		Message:    runErr,
	})
	return func() {
		if r.kind == RetryKindQuotaBackoff {
			o.logRescheduleErr(o.scheduleQuotaBackoffRetry(o.runCtx, issue, identifier, nextAttempt, runErr, quotaRetryAfter, workspace), r.id, identifier)
			return
		}
		o.logRescheduleErr(o.scheduleFailureRetryWithStartupFailure(o.runCtx, issue, identifier, nextAttempt, runErr, workspace, startupFailure), r.id, identifier)
	}
}

// retryFireAfterFetchOp implements SPEC §16.6 steps 3-5 after the
// candidate fetch completes. found == nil means the issue is absent
// from the active candidate set (step 3 / step 5: release the claim);
// otherwise refresh entry.Issue with the live tracker state and proceed
// to capacity check + dispatch.
//
// terminal/terminalState are resolved by the followup only when found == nil:
// they recover upstream handle_retry_issue_lookup's terminal branch that the
// active-only candidate fetch collapses into plain absence, so a terminal
// issue's workspace is cleaned through the §18.1 seam rather than leaked (#341).
type retryFireAfterFetchOp struct {
	o             *Orchestrator
	id            IssueID
	attempt       int
	kind          RetryKind
	found         *tracker.Issue
	terminal      bool
	terminalState string
}

func (r *retryFireAfterFetchOp) apply(st *OrchestratorState) func() {
	entry, ok := matchingRetryEntry(st, r.id, r.attempt, r.kind)
	if !ok {
		return nil
	}
	if r.found == nil {
		// SPEC §16.6 steps 3 + 5: issue no longer in the active candidate
		// set (either absent or moved to a non-active state). Drop the
		// retry and release the claim.
		identifier := entry.Identifier
		if r.terminal {
			issue := entry.Issue
			issue.State = r.terminalState
			recordOperatorTerminalStop(st, r.id, issue)
			// Upstream handle_retry_issue_lookup's terminal branch
			// (orchestrator.ex:1082-1090): a retry whose issue went terminal
			// cleans its workspace + releases. The worker already exited before
			// the retry was scheduled, so no live worker holds the directory —
			// route the removal through the same WorkspaceCleaner seam the
			// running/blocked terminal paths use (#341). The actor-serialized
			// re-claim guard inside runReconciledWorkspaceCleanup prevents a
			// re-dispatch from racing the removal onto the same path.
			if w, okw := terminalWorkspaceForCleanup(r.id, identifier, entry.Workspace.Path, entry.Workspace.Root, r.terminalState); okw {
				st.ReleaseClaim(r.id)
				st.RecordEvent(RuntimeEvent{
					Kind:       RuntimeEventFailed,
					IssueID:    r.id,
					Identifier: identifier,
					Message:    "retry released: issue terminal, removing workspace",
				})
				o := r.o
				return func() { o.runReconciledWorkspaceCleanup(w, nil) }
			}
		}
		st.ReleaseClaim(r.id)
		st.RecordEvent(RuntimeEvent{
			Kind:       RuntimeEventFailed,
			IssueID:    r.id,
			Identifier: identifier,
			Message:    "retry released: issue absent from active candidates",
		})
		return nil
	}
	// SPEC §16.6 step 4: refresh entry.Issue from the live tracker state
	// before proceeding to capacity check + dispatch. The per-state cap
	// must see the latest state, not the dispatch-time snapshot. Upstream
	// handle_active_retry (orchestrator.ex:1142-1161) uses issue.identifier
	// from the refreshed issue on every subsequent reschedule, so a
	// Linear identifier rename between schedule and fire would otherwise
	// leave runtime events stamped with the stale identifier.
	entry.Issue = *r.found
	st.ClaimedIssues[r.id] = *r.found
	if id := strings.TrimSpace(r.found.Identifier); id != "" {
		entry.Identifier = id
	}
	return retryFireDispatchTail(st, entry, r.id, r.attempt, r.o)
}
