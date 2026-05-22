// Package orchestrator owns the single in-memory authoritative state
// described by Symphony SPEC §4.1.8: the orchestrator's runtime state
// owned by a single authority, mutated via serialized operations.
//
// This file (state.go) defines the data types and pure state-transition
// methods. It deliberately contains no goroutines, timers, or I/O: those
// belong to the actor + scheduler that drive the worker-owned poll path.
//
// Field names mirror SPEC §4.1.8 exactly so future cross-references to
// the protocol contract are mechanical.
package orchestrator

import (
	"context"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// IssueID is the tracker-assigned stable identifier (tracker.Issue.ID).
// SPEC §4.1.1 calls this the issue's id; we alias it here so map keys
// in OrchestratorState are self-documenting.
type IssueID string

// Workspace is the orchestrator's view of an on-disk per-issue workspace.
// SPEC §4.2 / §9.1 want workspaces keyed by sanitized issue identifier;
// today the Manager keys them by task ID (deviation D13, tracked under
// #87). The rekey lands separately; until then Key holds whatever the
// workspace.Manager produced and CreatedNow tells reconciliation
// whether this run created the directory or reused an existing one.
type Workspace struct {
	Path       string
	Key        string
	CreatedNow bool
}

// LiveSession captures the SPEC §4.1.6 live-session fields populated
// from app-server runtime events.
type LiveSession struct {
	SessionID string
	ThreadID  string
	TurnID    string
	TurnCount int
}

// RateLimitSnapshot is the latest SPEC §13.3 rate-limits payload emitted by
// the runner. The payload is implementation-defined, so the orchestrator keeps
// the JSON-like map shape instead of projecting it into provider-specific
// fields.
type RateLimitSnapshot map[string]any

// CodexTotals accumulates the SPEC §4.1.8 / §13.3 codex totals across
// all runs the orchestrator dispatches in this process. Token counts
// and seconds-running are monotonically increasing for the process'
// lifetime; restart resets them per SPEC §14.3 (no persistence).
type CodexTotals struct {
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	SecondsRunning float64
}

// AddTokens folds a per-run codex token delta into the running totals.
// SPEC §13.3 reports input and output separately and the sum as
// `total_tokens`; we keep all three fields rather than recomputing on
// read so the snapshot never drifts from what was observed.
func (c *CodexTotals) AddTokens(input, output int64) {
	c.AddTokenDelta(input, output, input+output)
}

// AddTokenDelta folds a token-accounting delta into the running totals.
// total may be available even when the event did not include separate
// input/output counts.
func (c *CodexTotals) AddTokenDelta(input, output, total int64) {
	c.InputTokens += input
	c.OutputTokens += output
	c.TotalTokens += total
}

// AddSeconds folds elapsed wall-clock time from a single run into the
// running totals. Wall clock is fine here: SPEC §13.3 specifies seconds
// observed by the orchestrator, not monotonic-clock deltas.
func (c *CodexTotals) AddSeconds(d time.Duration) {
	if d < 0 {
		return
	}
	c.SecondsRunning += d.Seconds()
}

// RunningEntry is the per-issue record in OrchestratorState.Running.
// It bundles the data the actor needs to drive a single in-flight run
// from dispatch through normal exit, abnormal exit, or reconciliation
// termination.
//
// CancelWorker and Done are the only termination handles per SPEC §8.5
// Part B; the actor calls CancelWorker(), then waits on Done for the
// worker goroutine to finish before mutating other state fields.
type RunningEntry struct {
	Issue        tracker.Issue
	Identifier   string
	StartedAt    time.Time
	RetryAttempt *int // nil on first run (SPEC §4.1.5)
	// ContinuationAttempt is the number of clean continuation turns already
	// consumed for this issue. It is separate from RetryAttempt because
	// continuation dispatches must not render as failure retries in prompts.
	ContinuationAttempt int
	Workspace           Workspace

	Session     LiveSession
	LastCodexAt time.Time // SPEC §8.5 Part A input (D14)

	CodexInputTokens  int64
	CodexOutputTokens int64
	CodexTotalTokens  int64

	LastReportedInputTokens  int64
	LastReportedOutputTokens int64
	LastReportedTotalTokens  int64

	InputRequired       bool
	InputRequiredAt     time.Time
	InputRequiredMethod string

	CancelWorker context.CancelFunc
	Done         <-chan struct{}

	// ReconcileCancel is set by per-tick reconciliation before it cancels a
	// worker whose tracker issue left the active set. The worker still reports a
	// context cancellation, but finalization must release the run without
	// scheduling a retry because the tracker made the issue ineligible.
	ReconcileCancel bool
}

// BlockedEntry is an input-required run that has stopped executing but remains
// claimed until tracker reconciliation observes the issue leaving active work.
type BlockedEntry struct {
	Issue       tracker.Issue
	Identifier  string
	BlockedAt   time.Time
	Workspace   Workspace
	Session     LiveSession
	LastCodexAt time.Time
	Method      string
	Error       string
}

// OrchestratorState is the single authoritative in-memory state owned
// by the orchestrator (SPEC §4.1.8). Every field name matches the SPEC
// section exactly.
//
// This type is NOT safe for concurrent use on its own: mutation
// discipline (SPEC §7.4 "serialize state mutations through one
// authority") is supplied by the actor goroutine introduced in the
// next migration step. Tests in this package construct it directly
// because they exercise the transitions, not the discipline.
type OrchestratorState struct {
	PollIntervalMs             int64
	MaxConcurrentAgents        int
	MaxConcurrentAgentsByState map[string]int

	Running       map[IssueID]*RunningEntry
	Blocked       map[IssueID]*BlockedEntry
	Claimed       map[IssueID]struct{}
	ClaimedIssues map[IssueID]tracker.Issue
	RetryAttempts map[IssueID]*RetryEntry
	Failed        map[IssueID]FailedEntry
	Completed     map[IssueID]struct{} // bookkeeping only per SPEC §4.1.8

	// completedOrder and failedOrder pin FIFO insertion order so the
	// cap-and-evict policy below has a deterministic "oldest" to drop
	// and Snapshot() can publish entries in observed-order. They
	// mirror Completed / Failed: every id present in the set is also
	// in the slice, and vice versa.
	completedOrder []IssueID
	failedOrder    []IssueID

	// MaxRecentCompleted / MaxRecentFailed cap how many entries the
	// orchestrator retains in Completed / Failed for /api/v1/state
	// publication. SPEC §4.1.8 marks Completed as "bookkeeping only,
	// not dispatch gating" — long-running deployments (#234) were
	// otherwise accumulating tens of thousands of IDs in memory and
	// serializing them on every snapshot. Zero means "no cap"
	// (preserves the prior unbounded behavior for callers that opt
	// out), but NewOrchestratorState sets DefaultMaxRecentCompleted /
	// DefaultMaxRecentFailed so the bounded behavior is the default.
	MaxRecentCompleted int
	MaxRecentFailed    int

	// CumulativeCompletedTotal / CumulativeFailedTotal are monotonic
	// counters of every Completed / Failed transition observed since
	// process start. They survive cap-eviction and ReleaseFailed*
	// removal, so operators can read a true lifetime total from
	// /api/v1/state even when the per-id slices have been trimmed.
	CumulativeCompletedTotal int64
	CumulativeFailedTotal    int64

	CodexTotals     CodexTotals
	CodexRateLimits *RateLimitSnapshot // nil until the runner populates it

	RecentEvents []RuntimeEvent
}

// DefaultMaxRecentCompleted / DefaultMaxRecentFailed cap the per-id
// slices that /api/v1/state and Snapshot() publish. The cap is
// applied at construction (NewOrchestratorState); transitions evict
// the oldest entry when the cap is exceeded. Lifetime totals are
// preserved via the cumulative counters.
const (
	DefaultMaxRecentCompleted = 1000
	DefaultMaxRecentFailed    = 1000
)

// FailedEntry suppresses a deterministic non-retryable failure only while the
// tracker issue is unchanged. A later tracker state/update change means a human
// or agent may have fixed the configuration/input, so the poller may retry.
type FailedEntry struct {
	State     string
	UpdatedAt time.Time
}

// NewOrchestratorState mirrors the SPEC §16.1 reference initializer:
//
//	state = { running: {}, claimed: set(), retry_attempts: {},
//	          completed: set(), codex_totals: {...},
//	          codex_rate_limits: null }
//
// followed by `startup_terminal_workspace_cleanup()` and
// `schedule_tick(delay_ms=0)`. The startup cleanup and first tick are
// orchestrator-level concerns and live with the actor; this
// constructor only produces the empty maps so callers can never panic
// on a nil-map write.
func NewOrchestratorState(pollIntervalMs int64, maxConcurrentAgents int) *OrchestratorState {
	return &OrchestratorState{
		PollIntervalMs:             pollIntervalMs,
		MaxConcurrentAgents:        maxConcurrentAgents,
		MaxConcurrentAgentsByState: map[string]int{},
		Running:                    map[IssueID]*RunningEntry{},
		Blocked:                    map[IssueID]*BlockedEntry{},
		Claimed:                    map[IssueID]struct{}{},
		ClaimedIssues:              map[IssueID]tracker.Issue{},
		RetryAttempts:              map[IssueID]*RetryEntry{},
		Failed:                     map[IssueID]FailedEntry{},
		Completed:                  map[IssueID]struct{}{},
		MaxRecentCompleted:         DefaultMaxRecentCompleted,
		MaxRecentFailed:            DefaultMaxRecentFailed,
	}
}

// IsClaimed reports whether id is currently held by any of Running,
// RetryAttempts, or the "claimed but not yet running" Claimed set. SPEC
// §7.4 REQUIRES this check before launching any worker; making it a
// method on the state keeps the rule discoverable.
//
// All three maps are checked because the invariants are intentionally
// asymmetric: ReleaseClaim (used by reconciliation, SPEC §8.5 Part B)
// removes the Claimed entry immediately, but the Running entry is
// removed asynchronously by the worker goroutine exiting after
// CancelWorker. During that window Running[id] still exists with no
// Claimed[id] backing it, and a second dispatch for the same issue
// would violate SPEC §7.4 unless this check looks at Running too.
func (s *OrchestratorState) IsClaimed(id IssueID) bool {
	if _, ok := s.Claimed[id]; ok {
		return true
	}
	if _, ok := s.Running[id]; ok {
		return true
	}
	if _, ok := s.Blocked[id]; ok {
		return true
	}
	if _, ok := s.RetryAttempts[id]; ok {
		return true
	}
	if _, ok := s.Failed[id]; ok {
		return true
	}
	return false
}

// ReleaseFailedIfIssueChanged clears a non-retryable failure suppression when
// the tracker issue has visibly changed since the failure was recorded.
func (s *OrchestratorState) ReleaseFailedIfIssueChanged(issue tracker.Issue) bool {
	id := IssueID(issue.ID)
	failed, ok := s.Failed[id]
	if !ok {
		return false
	}
	if failed.State == issue.State && failed.UpdatedAt == issue.UpdatedAt {
		return false
	}
	s.removeFailed(id)
	return true
}

// removeFailed deletes id from both Failed and failedOrder. The two
// must stay in sync; release sites elsewhere also call this helper.
// The cumulative counter (CumulativeFailedTotal) is unaffected: it
// records observed transitions and is monotonic.
func (s *OrchestratorState) removeFailed(id IssueID) {
	if _, ok := s.Failed[id]; !ok {
		return
	}
	delete(s.Failed, id)
	for i, v := range s.failedOrder {
		if v == id {
			s.failedOrder = append(s.failedOrder[:i], s.failedOrder[i+1:]...)
			return
		}
	}
}

// recordCompleted adds id to Completed + completedOrder, increments
// CumulativeCompletedTotal, and trims the slice/map to
// MaxRecentCompleted by evicting the oldest entry. A repeat call for
// the same id is a no-op for the slice (FIFO position is preserved
// from the first transition) but still increments the cumulative
// counter — every observed succeeded transition is a real event.
func (s *OrchestratorState) recordCompleted(id IssueID) {
	s.CumulativeCompletedTotal++
	if _, ok := s.Completed[id]; ok {
		return
	}
	s.Completed[id] = struct{}{}
	s.completedOrder = append(s.completedOrder, id)
	if s.MaxRecentCompleted > 0 && len(s.completedOrder) > s.MaxRecentCompleted {
		oldest := s.completedOrder[0]
		s.completedOrder = s.completedOrder[1:]
		delete(s.Completed, oldest)
	}
}

// recordFailed is the Failed-side mirror of recordCompleted. It
// inserts a new FailedEntry, tracks order, increments the cumulative
// counter, and evicts the oldest entry when the cap is exceeded.
func (s *OrchestratorState) recordFailed(id IssueID, entry FailedEntry) {
	s.CumulativeFailedTotal++
	if _, ok := s.Failed[id]; ok {
		// Same id refreshed: update the entry but leave the FIFO
		// position alone. Refresh is a normal flow when a tracker
		// state/update changes mid-cycle and a new failure is
		// recorded before reconciliation rotates the entry out.
		s.Failed[id] = entry
		return
	}
	s.Failed[id] = entry
	s.failedOrder = append(s.failedOrder, id)
	if s.MaxRecentFailed > 0 && len(s.failedOrder) > s.MaxRecentFailed {
		oldest := s.failedOrder[0]
		s.failedOrder = s.failedOrder[1:]
		delete(s.Failed, oldest)
	}
}

// RunningCount reports in-flight work that consumes agent concurrency. Claimed
// entries are included because dispatchOp reserves the slot before its followup
// records Running; without counting those reservations, one poll tick can queue
// more workers than max_concurrent_agents before any Running entry is visible.
func (s *OrchestratorState) RunningCount() int {
	counted := s.runningIssueIDs()
	return len(counted)
}

func (s *OrchestratorState) RunningCountByState(state string) int {
	return s.RunningCountByStateExcluding(state, "")
}

func (s *OrchestratorState) StateCapacityFull(state string) bool {
	return s.StateCapacityFullExcluding(state, "")
}

func (s *OrchestratorState) StateCapacityFullExcluding(state string, excluded IssueID) bool {
	if len(s.MaxConcurrentAgentsByState) == 0 {
		return false
	}
	limit, ok := s.MaxConcurrentAgentsByState[normalizeStateConcurrencyKey(state)]
	if !ok || limit <= 0 {
		return false
	}
	return s.RunningCountByStateExcluding(state, excluded) >= limit
}

func (s *OrchestratorState) RunningCountByStateExcluding(state string, excluded IssueID) int {
	stateKey := normalizeStateConcurrencyKey(state)
	if stateKey == "" {
		return 0
	}
	counted := s.runningIssueIDs()
	if excluded != "" {
		delete(counted, excluded)
	}
	count := 0
	for id := range counted {
		if run, ok := s.Running[id]; ok {
			if normalizeStateConcurrencyKey(run.Issue.State) == stateKey {
				count++
			}
			continue
		}
		if claimed, ok := s.ClaimedIssues[id]; ok && normalizeStateConcurrencyKey(claimed.State) == stateKey {
			count++
		}
	}
	return count
}

func (s *OrchestratorState) runningIssueIDs() map[IssueID]struct{} {
	counted := make(map[IssueID]struct{}, len(s.Running)+len(s.Claimed))
	for id := range s.Running {
		counted[id] = struct{}{}
	}
	for id := range s.Claimed {
		counted[id] = struct{}{}
	}
	for id := range s.RetryAttempts {
		delete(counted, id)
	}
	for id := range s.Blocked {
		delete(counted, id)
	}
	return counted
}

func normalizeStateConcurrencyKey(state string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(state)), " ", "_")
}

// BeginDispatch records the SPEC §16.4 dispatch step: an eligible
// candidate transitions from "fetched" to "claimed and running".
//
//   - Adds id to Claimed (SPEC §4.1.8 set membership).
//   - Adds entry to Running.
//   - Clears any RetryAttempts record (the scheduled retry is being
//     consumed; its timer must already be stopped by the caller, since
//     RetryEntry.Timer is owned outside this struct).
//
// Per SPEC §7.4 the caller MUST first verify the issue is not already
// in Claimed; BeginDispatch does not perform that check because the
// duplicate-dispatch guard is a property of the actor, not of pure
// state mutation.
func (s *OrchestratorState) BeginDispatch(id IssueID, entry *RunningEntry) {
	s.Claimed[id] = struct{}{}
	s.ClaimedIssues[id] = entry.Issue
	s.Running[id] = entry
	delete(s.Blocked, id)
	delete(s.RetryAttempts, id)
	s.removeFailed(id)
}

// FinishRunSucceeded is the transition for a worker that exited cleanly
// (SPEC §7.3 normal exit).
//
//   - Removes from Running.
//   - Releases Claimed (the §8.4 continuation retry, if any, is
//     scheduled by a separate call to ScheduleRetry which re-adds the
//     claim).
//   - Adds id to Completed for bookkeeping (SPEC §4.1.8 says completed
//     is NOT consulted for dispatch gating; it exists so operator views
//     and §13.7's /api/v1/state can report it).
//   - Folds elapsed into CodexTotals.SecondsRunning.
func (s *OrchestratorState) FinishRunSucceeded(id IssueID, run *RunningEntry, elapsed time.Duration) bool {
	if current, ok := s.Running[id]; !ok || current != run {
		return false
	}
	delete(s.Running, id)
	delete(s.Claimed, id)
	delete(s.ClaimedIssues, id)
	s.recordCompleted(id)
	s.CodexTotals.AddSeconds(elapsed)
	return true
}

// FinishRunFailed is the transition for a worker that exited abnormally
// (SPEC §7.3 abnormal exit). The retry policy itself (exponential
// backoff per SPEC §8.4, currently D16) is owned by the scheduler in
// the next migration step; this method only releases the run slot and
// folds elapsed time so the caller can decide whether to enqueue a
// retry via ScheduleRetry.
func (s *OrchestratorState) FinishRunFailed(id IssueID, run *RunningEntry, elapsed time.Duration) bool {
	return s.finishRunAborted(id, run, elapsed)
}

// FinishRunReconciledCancelled releases a run that was stopped because the
// tracker made it ineligible. It is intentionally separate from
// FinishRunFailed so reconciliation cancellations are not counted as worker
// failures and cannot consume retry budget.
func (s *OrchestratorState) FinishRunReconciledCancelled(id IssueID, run *RunningEntry, elapsed time.Duration) bool {
	return s.finishRunAborted(id, run, elapsed)
}

func (s *OrchestratorState) BlockRun(id IssueID, run *RunningEntry, blockedAt time.Time, runErr string, elapsed time.Duration) bool {
	if current, ok := s.Running[id]; !ok || current != run {
		return false
	}
	if blockedAt.IsZero() {
		blockedAt = time.Now().UTC()
	}
	delete(s.Running, id)
	delete(s.RetryAttempts, id)
	s.removeFailed(id)
	s.Claimed[id] = struct{}{}
	s.ClaimedIssues[id] = run.Issue
	s.Blocked[id] = &BlockedEntry{
		Issue:       run.Issue,
		Identifier:  run.Identifier,
		BlockedAt:   blockedAt,
		Workspace:   run.Workspace,
		Session:     run.Session,
		LastCodexAt: run.LastCodexAt,
		Method:      run.InputRequiredMethod,
		Error:       runErr,
	}
	s.CodexTotals.AddSeconds(elapsed)
	return true
}

func (s *OrchestratorState) finishRunAborted(id IssueID, run *RunningEntry, elapsed time.Duration) bool {
	if current, ok := s.Running[id]; !ok || current != run {
		return false
	}
	delete(s.Running, id)
	delete(s.Claimed, id)
	delete(s.ClaimedIssues, id)
	s.CodexTotals.AddSeconds(elapsed)
	return true
}

// FinishRunNonRetryableFailed records a deterministic failure that should not
// be re-dispatched while the issue remains active in the same process. This is
// used for task-construction/configuration failures that would otherwise spin
// on every tracker poll tick.
func (s *OrchestratorState) FinishRunNonRetryableFailed(id IssueID, run *RunningEntry, elapsed time.Duration) bool {
	if !s.FinishRunFailed(id, run, elapsed) {
		return false
	}
	s.recordFailed(id, FailedEntry{State: run.Issue.State, UpdatedAt: run.Issue.UpdatedAt})
	return true
}

// ScheduleRetry records a RetryEntry under RetryAttempts and keeps the
// issue Claimed so a parallel tick cannot dispatch it again. The actual
// timer lives on entry.Timer and is started by the caller; this method
// is the pure-state half of "enter the retry-queued substate" from
// SPEC §7.1.
//
// If a prior retry entry exists for the same issue (e.g. backoff
// rescheduled) ScheduleRetry stops the old timer before replacing it
// so we never leak a goroutine in time.AfterFunc.
func (s *OrchestratorState) ScheduleRetry(entry *RetryEntry) {
	if prev, ok := s.RetryAttempts[entry.IssueID]; ok && prev.Timer != nil {
		prev.Timer.Stop()
	}
	s.RetryAttempts[entry.IssueID] = entry
	s.Claimed[entry.IssueID] = struct{}{}
	s.ClaimedIssues[entry.IssueID] = entry.Issue
}

// ReleaseClaim removes id from Claimed and tears down any pending
// retry. It does NOT touch Running because reconciliation termination
// (SPEC §8.5 Part B) handles the Running entry separately via
// CancelWorker + Done. The two cases ReleaseClaim serves:
//
//   - Retry timer expired and the candidate is no longer eligible.
//   - Reconciliation found the tracker state non-active, non-terminal
//     (row 6 in the design's state-transition table).
func (s *OrchestratorState) ReleaseClaim(id IssueID) {
	delete(s.Claimed, id)
	delete(s.ClaimedIssues, id)
	delete(s.Blocked, id)
	if entry, ok := s.RetryAttempts[id]; ok {
		if entry.Timer != nil {
			entry.Timer.Stop()
		}
		delete(s.RetryAttempts, id)
	}
}

// RecordCodexTokens folds a per-event token delta into CodexTotals.
// Equivalent to s.CodexTotals.AddTokens but discoverable on the state
// type itself so callers don't need to reach through the field name.
func (s *OrchestratorState) RecordCodexTokens(input, output int64) {
	s.CodexTotals.AddTokens(input, output)
}

// RecordRateLimits replaces the current rate-limit snapshot. SPEC
// §13.3 treats it as last-write-wins because the runner emits the full
// view, not a delta.
func (s *OrchestratorState) RecordRateLimits(snap *RateLimitSnapshot) {
	s.CodexRateLimits = copyRateLimitSnapshot(snap)
}

// StateView is the SPEC §13.3 / §13.7 shape the orchestrator publishes
// for /api/v1/state, CLI status, and tests. It is intentionally a
// value type with slices so callers cannot mutate the orchestrator's
// internal maps through it. The HTTP wrapper is deferred (see the
// design doc's "HTTP surface" section) but Snapshot() ships now so
// tests have a stable read API.
type StateView struct {
	GeneratedAt                time.Time
	PollIntervalMs             int64
	MaxConcurrentAgents        int
	MaxConcurrentAgentsByState map[string]int

	Running  []RunningView
	Blocked  []BlockedView
	Retrying []RetryView
	// Failed and Completed are bounded by MaxRecentFailed /
	// MaxRecentCompleted on the source state; the slices here are
	// FIFO-ordered (oldest first). For lifetime totals that survive
	// eviction, read CumulativeFailedTotal / CumulativeCompletedTotal.
	Failed                   []IssueID
	Completed                []IssueID
	CumulativeCompletedTotal int64
	CumulativeFailedTotal    int64
	CodexTotals              CodexTotals
	CodexRateLimits          *RateLimitSnapshot
}

// RunningView is the per-running-entry projection in StateView. It
// omits unexported / non-serializable fields like CancelWorker and Done.
type RunningView struct {
	IssueID       IssueID
	Identifier    string
	StartedAt     time.Time
	RetryAttempt  *int
	WorkspacePath string
	LastCodexAt   time.Time
}

// BlockedView is the public projection of an input-required blocked run.
type BlockedView struct {
	IssueID       IssueID
	Identifier    string
	State         string
	BlockedAt     time.Time
	WorkspacePath string
	SessionID     string
	LastCodexAt   time.Time
	Method        string
	Error         string
}

// RetryView is the per-retry-entry projection in StateView. Omits the
// *time.Timer handle because it is not meaningful outside the process.
type RetryView struct {
	IssueID    IssueID
	Identifier string
	Attempt    int
	DueAt      time.Time
	Error      string
}

// Snapshot returns a read-only view of the orchestrator state. The
// returned slices are freshly allocated so the caller may sort or
// truncate them without affecting future snapshots. Map iteration
// order is unspecified in Go, but downstream consumers (the §13.7 HTTP
// handler, CLI status) sort by IssueID before display.
func (s *OrchestratorState) Snapshot() StateView {
	view := StateView{
		GeneratedAt:                time.Now().UTC(),
		PollIntervalMs:             s.PollIntervalMs,
		MaxConcurrentAgents:        s.MaxConcurrentAgents,
		MaxConcurrentAgentsByState: copyStateConcurrencyLimits(s.MaxConcurrentAgentsByState),
		Running:                    make([]RunningView, 0, len(s.Running)),
		Blocked:                    make([]BlockedView, 0, len(s.Blocked)),
		Retrying:                   make([]RetryView, 0, len(s.RetryAttempts)),
		Failed:                     make([]IssueID, 0, len(s.failedOrder)),
		Completed:                  make([]IssueID, 0, len(s.completedOrder)),
		CumulativeCompletedTotal:   s.CumulativeCompletedTotal,
		CumulativeFailedTotal:      s.CumulativeFailedTotal,
		CodexTotals:                s.CodexTotals,
		CodexRateLimits:            copyRateLimitSnapshot(s.CodexRateLimits),
	}
	for id, r := range s.Running {
		// Deep-copy RetryAttempt so a snapshot consumer mutating the
		// pointee cannot reach back into orchestrator state. The
		// pointer-vs-nil distinction matters (SPEC §4.1.5 first-run
		// semantic) so we cannot flatten to an int.
		var retryAttempt *int
		if r.RetryAttempt != nil {
			n := *r.RetryAttempt
			retryAttempt = &n
		}
		view.Running = append(view.Running, RunningView{
			IssueID:       id,
			Identifier:    r.Identifier,
			StartedAt:     r.StartedAt,
			RetryAttempt:  retryAttempt,
			WorkspacePath: r.Workspace.Path,
			LastCodexAt:   r.LastCodexAt,
		})
	}
	for id, b := range s.Blocked {
		view.Blocked = append(view.Blocked, BlockedView{
			IssueID:       id,
			Identifier:    b.Identifier,
			State:         b.Issue.State,
			BlockedAt:     b.BlockedAt,
			WorkspacePath: b.Workspace.Path,
			SessionID:     b.Session.SessionID,
			LastCodexAt:   b.LastCodexAt,
			Method:        b.Method,
			Error:         b.Error,
		})
	}
	for id, r := range s.RetryAttempts {
		view.Retrying = append(view.Retrying, RetryView{
			IssueID:    id,
			Identifier: r.Identifier,
			Attempt:    r.Attempt,
			DueAt:      r.DueAt,
			Error:      r.Error,
		})
	}
	// Iterate the FIFO order slices, not the map. The map's iteration
	// order is undefined in Go; using the slices preserves observed
	// insertion order so /api/v1/state consumers see "oldest first"
	// stably, and the bounded slice matches MaxRecent* exactly.
	view.Failed = append(view.Failed, s.failedOrder...)
	view.Completed = append(view.Completed, s.completedOrder...)
	return view
}

func copyRateLimitSnapshot(in *RateLimitSnapshot) *RateLimitSnapshot {
	if in == nil {
		return nil
	}
	copied := RateLimitSnapshot(copyAnyMap(map[string]any(*in)))
	return &copied
}
