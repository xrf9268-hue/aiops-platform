// Package orchestrator owns the single in-memory authoritative state
// described by Symphony SPEC §4.1.8: the orchestrator's runtime state
// owned by a single authority, mutated via serialized operations.
//
// This file (state.go) defines the data types and pure state-transition
// methods. It deliberately contains no goroutines, timers, or I/O: those
// belong to the actor + scheduler that arrive in a later step of the
// D21+D6 migration plan (docs/design/d21-d6-orchestrator-state.md).
// During this step the package has no production callers; the worker
// still claims tasks through the Postgres queue.
//
// Field names mirror SPEC §4.1.8 exactly so future cross-references to
// the protocol contract are mechanical.
package orchestrator

import (
	"context"
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
// by the app-server runner once deviation D1 (#64) lands. PR 1 ships
// it as an empty struct so the field exists on RunningEntry without
// pulling the app-server protocol forward.
type LiveSession struct{}

// RateLimitSnapshot is the SPEC §13.3 rate-limits view emitted by the
// runner. Same deferral story as LiveSession: shape lands with D1.
type RateLimitSnapshot struct{}

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
	c.InputTokens += input
	c.OutputTokens += output
	c.TotalTokens += input + output
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
	Workspace    Workspace

	Session     LiveSession
	LastCodexAt time.Time // SPEC §8.5 Part A input (D14)

	CancelWorker context.CancelFunc
	Done         <-chan struct{}
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
	PollIntervalMs      int64
	MaxConcurrentAgents int

	Running       map[IssueID]*RunningEntry
	Claimed       map[IssueID]struct{}
	RetryAttempts map[IssueID]*RetryEntry
	Completed     map[IssueID]struct{} // bookkeeping only per SPEC §4.1.8

	CodexTotals     CodexTotals
	CodexRateLimits *RateLimitSnapshot // nil until the runner populates it
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
		PollIntervalMs:      pollIntervalMs,
		MaxConcurrentAgents: maxConcurrentAgents,
		Running:             map[IssueID]*RunningEntry{},
		Claimed:             map[IssueID]struct{}{},
		RetryAttempts:       map[IssueID]*RetryEntry{},
		Completed:           map[IssueID]struct{}{},
	}
}

// IsClaimed reports whether id is currently held by any of Running,
// RetryAttempts, or the "claimed but not yet running" window. SPEC §7.4
// REQUIRES this check before launching any worker; making it a method
// on the state keeps the rule discoverable.
func (s *OrchestratorState) IsClaimed(id IssueID) bool {
	_, ok := s.Claimed[id]
	return ok
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
	s.Running[id] = entry
	delete(s.RetryAttempts, id)
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
func (s *OrchestratorState) FinishRunSucceeded(id IssueID, elapsed time.Duration) {
	delete(s.Running, id)
	delete(s.Claimed, id)
	s.Completed[id] = struct{}{}
	s.CodexTotals.AddSeconds(elapsed)
}

// FinishRunFailed is the transition for a worker that exited abnormally
// (SPEC §7.3 abnormal exit). The retry policy itself (exponential
// backoff per SPEC §8.4, currently D16) is owned by the scheduler in
// the next migration step; this method only releases the run slot and
// folds elapsed time so the caller can decide whether to enqueue a
// retry via ScheduleRetry.
func (s *OrchestratorState) FinishRunFailed(id IssueID, elapsed time.Duration) {
	delete(s.Running, id)
	delete(s.Claimed, id)
	s.CodexTotals.AddSeconds(elapsed)
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
	s.CodexRateLimits = snap
}

// StateView is the SPEC §13.3 / §13.7 shape the orchestrator publishes
// for /api/v1/state, CLI status, and tests. It is intentionally a
// value type with slices so callers cannot mutate the orchestrator's
// internal maps through it. The HTTP wrapper is deferred (see the
// design doc's "HTTP surface" section) but Snapshot() ships now so
// tests have a stable read API.
type StateView struct {
	PollIntervalMs      int64
	MaxConcurrentAgents int

	Running         []RunningView
	Retrying        []RetryView
	Completed       []IssueID
	CodexTotals     CodexTotals
	CodexRateLimits *RateLimitSnapshot
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
		PollIntervalMs:      s.PollIntervalMs,
		MaxConcurrentAgents: s.MaxConcurrentAgents,
		Running:             make([]RunningView, 0, len(s.Running)),
		Retrying:            make([]RetryView, 0, len(s.RetryAttempts)),
		Completed:           make([]IssueID, 0, len(s.Completed)),
		CodexTotals:         s.CodexTotals,
		CodexRateLimits:     s.CodexRateLimits,
	}
	for id, r := range s.Running {
		view.Running = append(view.Running, RunningView{
			IssueID:       id,
			Identifier:    r.Identifier,
			StartedAt:     r.StartedAt,
			RetryAttempt:  r.RetryAttempt,
			WorkspacePath: r.Workspace.Path,
			LastCodexAt:   r.LastCodexAt,
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
	for id := range s.Completed {
		view.Completed = append(view.Completed, id)
	}
	return view
}
