package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// These tests walk the state-transition table from
// docs/design/d21-d6-orchestrator-state.md ("State transitions") row by
// row. Each test is intentionally small — the design separates "what
// the state must look like after operation X" (covered here) from "who
// decides to call X and in what order" (covered by the actor tests in
// the next migration step).

func issue(id string) tracker.Issue {
	return tracker.Issue{ID: id, Identifier: id, Title: "t-" + id}
}

func runningEntry(t *testing.T, iss tracker.Issue) *RunningEntry {
	t.Helper()
	_, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	return &RunningEntry{
		Issue:        iss,
		Identifier:   iss.Identifier,
		StartedAt:    time.Now(),
		Workspace:    Workspace{Path: "/tmp/" + iss.ID, Key: iss.Identifier, CreatedNow: true},
		CancelWorker: cancel,
		Done:         done,
	}
}

func TestNewOrchestratorState_MatchesSpec16_1Initializer(t *testing.T) {
	// SPEC §16.1: state = { running: {}, claimed: set(),
	// retry_attempts: {}, completed: set(), codex_totals: {...},
	// codex_rate_limits: null }.
	st := NewOrchestratorState(15000, 4)

	if st.PollIntervalMs != 15000 {
		t.Errorf("PollIntervalMs = %d, want 15000", st.PollIntervalMs)
	}
	if st.MaxConcurrentAgents != 4 {
		t.Errorf("MaxConcurrentAgents = %d, want 4", st.MaxConcurrentAgents)
	}
	if st.Running == nil || len(st.Running) != 0 {
		t.Errorf("Running not initialized to empty map: %v", st.Running)
	}
	if st.Claimed == nil || len(st.Claimed) != 0 {
		t.Errorf("Claimed not initialized to empty set: %v", st.Claimed)
	}
	if st.RetryAttempts == nil || len(st.RetryAttempts) != 0 {
		t.Errorf("RetryAttempts not initialized to empty map: %v", st.RetryAttempts)
	}
	if st.Completed == nil || len(st.Completed) != 0 {
		t.Errorf("Completed not initialized to empty set: %v", st.Completed)
	}
	if st.CodexRateLimits != nil {
		t.Errorf("CodexRateLimits expected nil per SPEC §16.1, got %v", st.CodexRateLimits)
	}
	if (st.CodexTotals != CodexTotals{}) {
		t.Errorf("CodexTotals not zero-initialized: %+v", st.CodexTotals)
	}
}

// Row 1: poll tick dispatches an eligible candidate (SPEC §16.4).
func TestBeginDispatch_AddsClaimAndRunningAndClearsRetry(t *testing.T) {
	st := NewOrchestratorState(15000, 4)
	iss := issue("ENG-1")
	id := IssueID(iss.ID)

	// Seed a stale retry entry to prove dispatch consumes it. The
	// caller is expected to stop the timer before calling
	// BeginDispatch; we don't start one in the test.
	st.RetryAttempts[id] = &RetryEntry{IssueID: id, Identifier: iss.Identifier, Attempt: 2}

	entry := runningEntry(t, iss)
	st.BeginDispatch(id, entry)

	if !st.IsClaimed(id) {
		t.Errorf("expected %s claimed after BeginDispatch", id)
	}
	if got, ok := st.Running[id]; !ok || got != entry {
		t.Errorf("Running[%s] = %v ok=%v, want entry %p", id, got, ok, entry)
	}
	if _, ok := st.RetryAttempts[id]; ok {
		t.Errorf("RetryAttempts[%s] should be cleared by BeginDispatch", id)
	}
}

// Row 2: worker exits normally (SPEC §7.3 normal exit + §8.4 continuation).
//
// FinishRunSucceeded handles the state-mutation half: Running cleared,
// claim released, Completed marked, seconds folded. The continuation
// retry is scheduled by a separate call (verified in TestScheduleRetry).
func TestFinishRunSucceeded_RemovesRunningAddsCompletedFoldsSeconds(t *testing.T) {
	st := NewOrchestratorState(15000, 4)
	iss := issue("ENG-1")
	id := IssueID(iss.ID)
	st.BeginDispatch(id, runningEntry(t, iss))

	st.FinishRunSucceeded(id, 750*time.Millisecond)

	if _, ok := st.Running[id]; ok {
		t.Errorf("Running[%s] should be removed after FinishRunSucceeded", id)
	}
	if st.IsClaimed(id) {
		t.Errorf("Claim for %s should be released after FinishRunSucceeded", id)
	}
	if _, ok := st.Completed[id]; !ok {
		t.Errorf("Completed[%s] should be set after FinishRunSucceeded", id)
	}
	// 750ms == 0.75s; floating-point comparison is exact here because
	// 0.75 has a finite binary representation.
	if st.CodexTotals.SecondsRunning != 0.75 {
		t.Errorf("CodexTotals.SecondsRunning = %v, want 0.75", st.CodexTotals.SecondsRunning)
	}
}

// Row 3: worker exits abnormally (SPEC §7.3 abnormal exit). State
// releases the slot; the retry decision is the scheduler's job.
func TestFinishRunFailed_RemovesRunningReleasesClaimDoesNotMarkCompleted(t *testing.T) {
	st := NewOrchestratorState(15000, 4)
	iss := issue("ENG-1")
	id := IssueID(iss.ID)
	st.BeginDispatch(id, runningEntry(t, iss))

	st.FinishRunFailed(id, 2*time.Second)

	if _, ok := st.Running[id]; ok {
		t.Errorf("Running[%s] should be removed after FinishRunFailed", id)
	}
	if st.IsClaimed(id) {
		t.Errorf("Claim for %s should be released after FinishRunFailed", id)
	}
	if _, ok := st.Completed[id]; ok {
		t.Errorf("Completed[%s] must NOT be set on abnormal exit (SPEC §4.1.8)", id)
	}
	if st.CodexTotals.SecondsRunning != 2.0 {
		t.Errorf("CodexTotals.SecondsRunning = %v, want 2.0", st.CodexTotals.SecondsRunning)
	}
}

// Row 4: retry-queued substate. ScheduleRetry must claim the issue so
// concurrent ticks cannot dispatch it again before the timer fires.
func TestScheduleRetry_RecordsEntryAndKeepsClaim(t *testing.T) {
	st := NewOrchestratorState(15000, 4)
	iss := issue("ENG-1")
	id := IssueID(iss.ID)

	entry := &RetryEntry{
		IssueID:    id,
		Identifier: iss.Identifier,
		Attempt:    1,
		DueAt:      time.Now().Add(1 * time.Second),
		Error:      "boom",
	}
	st.ScheduleRetry(entry)

	if got, ok := st.RetryAttempts[id]; !ok || got != entry {
		t.Errorf("RetryAttempts[%s] = %v ok=%v, want entry %p", id, got, ok, entry)
	}
	if !st.IsClaimed(id) {
		t.Errorf("ScheduleRetry must hold the claim so SPEC §7.4 duplicate-dispatch guard works")
	}
}

// ScheduleRetry replacing an existing entry must stop the old timer to
// avoid leaking the goroutine inside time.AfterFunc.
func TestScheduleRetry_ReplacingEntryStopsOldTimer(t *testing.T) {
	st := NewOrchestratorState(15000, 4)
	iss := issue("ENG-1")
	id := IssueID(iss.ID)

	fired := make(chan struct{}, 1)
	oldTimer := time.AfterFunc(50*time.Millisecond, func() { fired <- struct{}{} })
	st.ScheduleRetry(&RetryEntry{IssueID: id, Identifier: iss.Identifier, Attempt: 1, Timer: oldTimer})
	st.ScheduleRetry(&RetryEntry{IssueID: id, Identifier: iss.Identifier, Attempt: 2})

	select {
	case <-fired:
		t.Errorf("old timer fired even though ScheduleRetry should have stopped it")
	case <-time.After(150 * time.Millisecond):
	}
}

// Rows 5 and 6: reconciliation releases a claim. ReleaseClaim must stop
// any pending retry timer and clear both Claimed and RetryAttempts; it
// must not touch Running (rows 5/6 cover the non-running case — running
// termination is the worker's CancelWorker + Done).
func TestReleaseClaim_ClearsClaimAndRetryAndStopsTimer(t *testing.T) {
	st := NewOrchestratorState(15000, 4)
	iss := issue("ENG-1")
	id := IssueID(iss.ID)

	fired := make(chan struct{}, 1)
	timer := time.AfterFunc(50*time.Millisecond, func() { fired <- struct{}{} })
	st.ScheduleRetry(&RetryEntry{IssueID: id, Identifier: iss.Identifier, Attempt: 1, Timer: timer})

	st.ReleaseClaim(id)

	if st.IsClaimed(id) {
		t.Errorf("Claimed[%s] not cleared", id)
	}
	if _, ok := st.RetryAttempts[id]; ok {
		t.Errorf("RetryAttempts[%s] not cleared", id)
	}
	select {
	case <-fired:
		t.Errorf("retry timer fired after ReleaseClaim — timer.Stop missed")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestReleaseClaim_DoesNotTouchRunning(t *testing.T) {
	st := NewOrchestratorState(15000, 4)
	iss := issue("ENG-1")
	id := IssueID(iss.ID)
	entry := runningEntry(t, iss)
	st.BeginDispatch(id, entry)

	st.ReleaseClaim(id)

	// SPEC §8.5 Part B says running termination uses CancelWorker +
	// Done; ReleaseClaim is the non-running half of reconciliation.
	if _, ok := st.Running[id]; !ok {
		t.Errorf("ReleaseClaim must not remove Running[%s]; that's the worker goroutine's job", id)
	}
}

func TestCodexTotals_AddTokensAndSeconds(t *testing.T) {
	var c CodexTotals
	c.AddTokens(100, 250)
	c.AddTokens(50, 0)
	c.AddSeconds(1500 * time.Millisecond)
	c.AddSeconds(-time.Second) // negative deltas must be ignored

	if c.InputTokens != 150 || c.OutputTokens != 250 || c.TotalTokens != 400 {
		t.Errorf("token totals wrong: %+v", c)
	}
	if c.SecondsRunning != 1.5 {
		t.Errorf("SecondsRunning = %v, want 1.5", c.SecondsRunning)
	}
}

func TestRecordRateLimits_NilToValueAndBack(t *testing.T) {
	st := NewOrchestratorState(15000, 4)
	if st.CodexRateLimits != nil {
		t.Fatalf("initial CodexRateLimits should be nil")
	}
	// RateLimitSnapshot is currently a zero-sized struct (D1 fills it
	// in). Go reserves the right to alias pointers to zero-sized
	// allocations (runtime.zerobase), so pointer-identity comparisons
	// between two `&RateLimitSnapshot{}` values are a no-op assertion.
	// Until D1 adds fields, exercise the nil-vs-non-nil transition
	// instead, which is what callers actually depend on.
	snap := &RateLimitSnapshot{}
	st.RecordRateLimits(snap)
	if st.CodexRateLimits == nil {
		t.Errorf("RecordRateLimits did not store snapshot (still nil)")
	}
	st.RecordRateLimits(nil)
	if st.CodexRateLimits != nil {
		t.Errorf("RecordRateLimits(nil) should clear the field")
	}
}

func TestSnapshot_ShapeMatches13_3(t *testing.T) {
	st := NewOrchestratorState(15000, 4)
	iss1 := issue("ENG-1")
	iss2 := issue("ENG-2")
	iss3 := issue("ENG-3")
	id1, id2, id3 := IssueID(iss1.ID), IssueID(iss2.ID), IssueID(iss3.ID)

	st.BeginDispatch(id1, runningEntry(t, iss1))
	st.ScheduleRetry(&RetryEntry{
		IssueID:    id2,
		Identifier: iss2.Identifier,
		Attempt:    3,
		DueAt:      time.Unix(1_700_000_000, 0),
		Error:      "bad",
	})
	// Completed is fed via FinishRunSucceeded.
	st.BeginDispatch(id3, runningEntry(t, iss3))
	st.FinishRunSucceeded(id3, 500*time.Millisecond)

	view := st.Snapshot()

	if view.PollIntervalMs != 15000 || view.MaxConcurrentAgents != 4 {
		t.Errorf("config not surfaced in snapshot: %+v", view)
	}
	if len(view.Running) != 1 || view.Running[0].IssueID != id1 {
		t.Errorf("Running view wrong: %+v", view.Running)
	}
	if len(view.Retrying) != 1 || view.Retrying[0].IssueID != id2 || view.Retrying[0].Attempt != 3 {
		t.Errorf("Retrying view wrong: %+v", view.Retrying)
	}
	if len(view.Completed) != 1 || view.Completed[0] != id3 {
		t.Errorf("Completed view wrong: %+v", view.Completed)
	}
	if view.CodexTotals.SecondsRunning != 0.5 {
		t.Errorf("CodexTotals not surfaced: %+v", view.CodexTotals)
	}

	// Mutating the returned slice must not affect future snapshots.
	view.Completed[0] = "tampered"
	again := st.Snapshot()
	if again.Completed[0] != id3 {
		t.Errorf("Snapshot returned a slice aliased to internal state")
	}
}

// Regression for the IsClaimed window: after ReleaseClaim (used by
// reconciliation) the Running entry persists until the worker goroutine
// exits and the actor processes the FinishRun* op. During that window
// IsClaimed must still report true, otherwise SPEC §7.4's
// duplicate-dispatch guard fails and a second tick dispatches a second
// worker for the same issue.
func TestIsClaimed_CoversRunningAfterReleaseClaim(t *testing.T) {
	st := NewOrchestratorState(15000, 4)
	iss := issue("ENG-1")
	id := IssueID(iss.ID)

	st.BeginDispatch(id, runningEntry(t, iss))
	st.ReleaseClaim(id)

	if !st.IsClaimed(id) {
		t.Errorf("IsClaimed must remain true while Running[%s] still holds the entry", id)
	}
}

// Same idea, but for the retry-queued window: ScheduleRetry adds to
// Claimed + RetryAttempts; if a future change ever drops the Claimed
// add (or the actor races on ReleaseClaim mid-retry), IsClaimed must
// still report claimed via RetryAttempts.
func TestIsClaimed_CoversRetryAttemptsWhenClaimedMissing(t *testing.T) {
	st := NewOrchestratorState(15000, 4)
	id := IssueID("ENG-1")
	st.RetryAttempts[id] = &RetryEntry{IssueID: id, Identifier: "ENG-1", Attempt: 1}
	// Deliberately do not seed Claimed[id]; the test asserts the
	// fallback through RetryAttempts.
	if !st.IsClaimed(id) {
		t.Errorf("IsClaimed must report true when only RetryAttempts[%s] is set", id)
	}
}

// Snapshot must deep-copy RetryAttempt so a consumer mutating the
// pointee cannot reach back into orchestrator state.
func TestSnapshot_DeepCopiesRetryAttemptPointer(t *testing.T) {
	st := NewOrchestratorState(15000, 4)
	iss := issue("ENG-1")
	id := IssueID(iss.ID)
	attempt := 3
	entry := runningEntry(t, iss)
	entry.RetryAttempt = &attempt
	st.BeginDispatch(id, entry)

	view := st.Snapshot()
	if len(view.Running) != 1 {
		t.Fatalf("expected one Running entry in snapshot, got %d", len(view.Running))
	}
	got := view.Running[0].RetryAttempt
	if got == nil || *got != 3 {
		t.Fatalf("RetryAttempt not surfaced: %v", got)
	}
	if got == entry.RetryAttempt {
		t.Errorf("Snapshot aliased RetryAttempt pointer; consumer mutation would leak into state")
	}
	*got = 999
	if *entry.RetryAttempt != 3 {
		t.Errorf("mutating snapshot's RetryAttempt mutated live state: %d", *entry.RetryAttempt)
	}
}

// Nil RetryAttempt (first-run, SPEC §4.1.5) must survive the deep copy
// as still-nil — flattening to a zero int would silently turn a first
// run into "attempt 0".
func TestSnapshot_NilRetryAttemptStaysNil(t *testing.T) {
	st := NewOrchestratorState(15000, 4)
	iss := issue("ENG-1")
	id := IssueID(iss.ID)
	st.BeginDispatch(id, runningEntry(t, iss))

	view := st.Snapshot()
	if len(view.Running) != 1 {
		t.Fatalf("expected one Running entry, got %d", len(view.Running))
	}
	if view.Running[0].RetryAttempt != nil {
		t.Errorf("first-run RetryAttempt must stay nil, got %d", *view.Running[0].RetryAttempt)
	}
}

func TestRetryEntry_IsDue(t *testing.T) {
	now := time.Now()
	r := &RetryEntry{DueAt: now.Add(-time.Second)}
	if !r.IsDue(now) {
		t.Errorf("retry due in the past should be IsDue=true")
	}
	r2 := &RetryEntry{DueAt: now.Add(time.Second)}
	if r2.IsDue(now) {
		t.Errorf("retry due in the future should be IsDue=false")
	}
	var nilEntry *RetryEntry
	if nilEntry.IsDue(now) {
		t.Errorf("nil retry entry should not be due")
	}
}
