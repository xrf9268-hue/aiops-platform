package orchestrator

import "time"

// RetryEntry is the SPEC §4.1.7 record stored under
// OrchestratorState.RetryAttempts while an issue is in the retry-queued
// substate (SPEC §7.1). Fields match SPEC §4.1.7 exactly:
//
//   - IssueID, Identifier — which issue this retry is for.
//   - Attempt — 1-based retry counter; SPEC §8.4 bounds it via
//     `max_retry_attempts`.
//   - DueAt — absolute deadline at which the retry should fire. Go's
//     time.Time carries a monotonic reading on construction so SPEC's
//     "due_at_ms (monotonic)" requirement is satisfied without a
//     separate field.
//   - Timer — the scheduling handle SPEC calls `timer_handle`. The
//     scheduler that owns the timer (introduced in the next migration
//     step) is responsible for calling Stop when an entry is replaced
//     or released; OrchestratorState.ReleaseClaim performs that stop
//     for the reconciliation/release paths.
//   - Error — the abnormal-exit message that caused the retry, surfaced
//     through SPEC §13.3's retrying view so operators can see why an
//     issue is in backoff.
type RetryEntry struct {
	IssueID    IssueID
	Identifier string
	Attempt    int
	DueAt      time.Time
	Timer      *time.Timer
	Error      string
}

// IsDue reports whether the retry's DueAt has passed relative to now.
// Useful for the actor's tick path when it needs to walk RetryAttempts
// independently of timer callbacks (e.g. after a long pause where
// multiple timers may have piled up).
func (r *RetryEntry) IsDue(now time.Time) bool {
	if r == nil {
		return false
	}
	return !now.Before(r.DueAt)
}
