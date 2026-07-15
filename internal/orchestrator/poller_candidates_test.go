package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
)

// startRevalidationHarness builds the PollOnce fixture the #740 dispatch
// revalidation tests share: a reconciling poller over a narrow-refresh-capable
// tracker fake and a recording dispatcher, with the orchestrator actor running.
func startRevalidationHarness(t *testing.T, trackerClient *fakeIssueStateTracker, cfg ReconciliationConfig) (*Poller, *recordingDispatcher, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dispatcher := &recordingDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}
	return NewPollerWithReconciliation(trackerClient, orch, cfg), dispatcher, ctx
}

func revalidationReconcileConfig() ReconciliationConfig {
	return ReconciliationConfig{
		ActiveStates:      []string{"In Progress", "Rework"},
		TerminalStates:    []string{"Cancelled", "Done"},
		WorkerExitTimeout: time.Second,
	}
}

func TestPollOnceSkipsDispatchWhenRevalidationShowsInactiveState(t *testing.T) {
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}}
	poller, dispatcher, ctx := startRevalidationHarness(t, trackerClient, revalidationReconcileConfig())

	// The listing (tick start) still shows the issue active; the pre-dispatch
	// revalidation observes it already moved to a terminal state (#740).
	trackerClient.setFetchIDStates(map[string]string{"issue-1": "Done"})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce() = %v; want nil", err)
	}
	if got := dispatcher.count(); got != 0 {
		t.Fatalf("dispatched %d issues (%v); want 0 after stale revalidation", got, dispatcher.issueIDs())
	}
}

func TestPollOnceSkipsDispatchWhenRevalidationOmitsIssue(t *testing.T) {
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}}
	poller, dispatcher, ctx := startRevalidationHarness(t, trackerClient, revalidationReconcileConfig())

	// An omitted result normalizes to Unknown. Dispatch fails closed, but the
	// omission is not treated as confirmed tracker absence.
	trackerClient.setFetchIDStates(map[string]string{})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce() = %v; want nil", err)
	}
	if got := dispatcher.count(); got != 0 {
		t.Fatalf("dispatched %d issues (%v); want 0 when refresh omits the candidate", got, dispatcher.issueIDs())
	}
}

func TestPollOnceDispatchesRevalidatedCandidateWithRefreshedState(t *testing.T) {
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}}
	poller, dispatcher, ctx := startRevalidationHarness(t, trackerClient, revalidationReconcileConfig())

	// Still active, but in a different active state than the listing showed:
	// the dispatch must carry the refreshed state (per-state capacity gates and
	// the spawned worker read it), mirroring upstream's refreshed_issue dispatch.
	trackerClient.setFetchIDStates(map[string]string{"issue-1": "Rework"})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce() = %v; want nil", err)
	}
	if got := dispatcher.count(); got != 1 {
		t.Fatalf("dispatched %d issues (%v); want 1", got, dispatcher.issueIDs())
	}
	if got := dispatcher.issueAt(0).State; got != "Rework" {
		t.Fatalf("dispatched issue state = %q; want refreshed state %q", got, "Rework")
	}
}

func TestRevalidatedCandidateRequiresCurrentOutcome(t *testing.T) {
	issue := tracker.Issue{ID: "issue-1", Identifier: "LIN-1", Title: "work", State: "In Progress"}
	active := normalizedStates([]string{"In Progress", "Rework"})
	terminal := normalizedStates([]string{"Done"})
	tests := []struct {
		name    string
		states  map[string]tracker.IssueState
		want    bool
		wantOut string
	}{
		{
			name: "current active",
			states: map[string]tracker.IssueState{
				issue.ID: {Outcome: tracker.IssueStateOutcomeCurrent, State: "Rework"},
			},
			want: true, wantOut: "Rework",
		},
		{
			name: "state-bearing unknown",
			states: map[string]tracker.IssueState{
				issue.ID: {Outcome: tracker.IssueStateOutcomeUnknown, State: "Rework"},
			},
		},
		{
			name: "state-bearing absent",
			states: map[string]tracker.IssueState{
				issue.ID: {Outcome: tracker.IssueStateOutcomeAbsent, State: "Rework"},
			},
		},
		{name: "missing row", states: map[string]tracker.IssueState{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, keep := revalidatedCandidate(issue, tt.states, active, terminal, nil)
			if keep != tt.want {
				t.Fatalf("revalidatedCandidate keep = %v; want %v (got=%+v)", keep, tt.want, got)
			}
			if keep && got.State != tt.wantOut {
				t.Fatalf("revalidatedCandidate state = %q; want %q", got.State, tt.wantOut)
			}
		})
	}
}

func TestPollOnceSkipsDispatchWhenRevalidationDropsRequiredLabels(t *testing.T) {
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{
		ID:         "issue-1",
		Identifier: "LIN-1",
		State:      "In Progress",
		Labels:     []string{"aiops"},
	}}}
	cfg := revalidationReconcileConfig()
	cfg.RequiredLabels = []string{"aiops"}
	poller, dispatcher, ctx := startRevalidationHarness(t, trackerClient, cfg)

	// The listing carries the required label; the revalidation refresh returns
	// the issue still active but without it (SPEC §6.4 gate re-applied on
	// refreshed labels).
	trackerClient.setFetchIDStates(map[string]string{"issue-1": "In Progress"})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce() = %v; want nil", err)
	}
	if got := dispatcher.count(); got != 0 {
		t.Fatalf("dispatched %d issues (%v); want 0 after required label removal", got, dispatcher.issueIDs())
	}
}

// seedDueContinuation installs a due continuation retry entry (with its claim)
// through the actor, the state a clean worker exit leaves behind while the
// next dispatch is pending.
func seedDueContinuation(t *testing.T, ctx context.Context, poller *Poller, issue tracker.Issue) {
	t.Helper()
	id := IssueID(issue.ID)
	done := make(chan struct{})
	if err := poller.orchestrator.submit(ctx, opFunc(func(st *OrchestratorState) func() {
		st.Claimed[id] = struct{}{}
		st.ClaimedIssues[id] = issue
		st.RetryAttempts[id] = &RetryEntry{
			IssueID:               id,
			Identifier:            issue.Identifier,
			Kind:                  RetryKindContinuation,
			Attempt:               1,
			DueAt:                 time.Now().Add(-time.Minute),
			ContinuationTurnCount: 2,
		}
		close(done)
		return nil
	})); err != nil {
		t.Fatalf("seed continuation: %v", err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("seed continuation: %v", ctx.Err())
	}
}

// continuationClaimState reads whether the issue still holds a retry entry and
// a claim, serialized through the actor.
func continuationClaimState(t *testing.T, ctx context.Context, poller *Poller, id IssueID) (retained, claimed bool) {
	t.Helper()
	done := make(chan struct{})
	if err := poller.orchestrator.submit(ctx, opFunc(func(st *OrchestratorState) func() {
		_, retained = st.RetryAttempts[id]
		_, claimed = st.Claimed[id]
		close(done)
		return nil
	})); err != nil {
		t.Fatalf("read claim state: %v", err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("read claim state: %v", ctx.Err())
	}
	return retained, claimed
}

func TestPollOnceReleasesDueContinuationWhenRefreshConfirmsAbsent(t *testing.T) {
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}}
	poller, dispatcher, ctx := startRevalidationHarness(t, trackerClient, revalidationReconcileConfig())
	seedDueContinuation(t, ctx, poller, tracker.Issue{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"})

	// Only the explicit per-ref Absent outcome proves deletion / removal from
	// the tracker workflow. A missing or Unknown row remains no-information.
	trackerClient.setFetchIDIssueStates(map[string]tracker.IssueState{
		"issue-1": {Outcome: tracker.IssueStateOutcomeAbsent},
	})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce() = %v; want nil", err)
	}
	if got := dispatcher.count(); got != 0 {
		t.Fatalf("dispatched %d issues (%v); want 0", got, dispatcher.issueIDs())
	}
	retained, claimed := continuationClaimState(t, ctx, poller, "issue-1")
	if retained || claimed {
		t.Fatalf("continuation after confirmed absence: retained=%v claimed=%v; want released", retained, claimed)
	}
}

func TestPollOnceReleasesDueContinuationConfirmedAbsentFromActiveListing(t *testing.T) {
	// The between-tick variant: the issue vanished BEFORE this tick's listing,
	// so it is never a dispatch candidate at all. The reconcile pass still
	// narrow-refreshes it as a claimed (retrying) ref; an explicit Absent
	// outcome must release the continuation (#740 review HIGH).
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{}}
	poller, dispatcher, ctx := startRevalidationHarness(t, trackerClient, revalidationReconcileConfig())
	seedDueContinuation(t, ctx, poller, tracker.Issue{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"})

	trackerClient.setFetchIDIssueStates(map[string]tracker.IssueState{
		"issue-1": {Outcome: tracker.IssueStateOutcomeAbsent},
	})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce() = %v; want nil", err)
	}
	if got := dispatcher.count(); got != 0 {
		t.Fatalf("dispatched %d issues (%v); want 0", got, dispatcher.issueIDs())
	}
	retained, claimed := continuationClaimState(t, ctx, poller, "issue-1")
	if retained || claimed {
		t.Fatalf("continuation absent from listing: retained=%v claimed=%v; want released", retained, claimed)
	}
}

func TestPollOnceRetainsAbsentFailureRetry(t *testing.T) {
	// A failure retry whose narrow refresh confirms absence is NOT released by
	// reconciliation: its own timer fire path runs the SPEC §16.6 candidate
	// fetch and releases on absence with terminal-state resolution (#341).
	// Releasing it here would double-own that contract.
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{}}
	poller, dispatcher, ctx := startRevalidationHarness(t, trackerClient, revalidationReconcileConfig())
	id := IssueID("issue-1")
	done := make(chan struct{})
	if err := poller.orchestrator.submit(ctx, opFunc(func(st *OrchestratorState) func() {
		st.Claimed[id] = struct{}{}
		st.ClaimedIssues[id] = tracker.Issue{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}
		st.RetryAttempts[id] = &RetryEntry{
			IssueID:    id,
			Identifier: "LIN-1",
			Kind:       RetryKindFailure,
			Attempt:    1,
			DueAt:      time.Now().Add(time.Hour),
		}
		close(done)
		return nil
	})); err != nil {
		t.Fatalf("seed failure retry: %v", err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("seed failure retry: %v", ctx.Err())
	}

	trackerClient.setFetchIDIssueStates(map[string]tracker.IssueState{
		"issue-1": {Outcome: tracker.IssueStateOutcomeAbsent},
	})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce() = %v; want nil", err)
	}
	if got := dispatcher.count(); got != 0 {
		t.Fatalf("dispatched %d issues (%v); want 0", got, dispatcher.issueIDs())
	}
	retained, claimed := continuationClaimState(t, ctx, poller, id)
	if !retained || !claimed {
		t.Fatalf("failure retry after absent refresh: retained=%v claimed=%v; want retained", retained, claimed)
	}
}

func TestPollOnceRetainsDueContinuationWhenRefreshOmitsOutcome(t *testing.T) {
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{}}
	poller, dispatcher, ctx := startRevalidationHarness(t, trackerClient, revalidationReconcileConfig())
	seedDueContinuation(t, ctx, poller, tracker.Issue{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"})

	// A non-compliant or partial adapter may omit a requested row. The poller
	// boundary totalizes that omission to Unknown; it must never infer absence.
	trackerClient.setFetchIDIssueStates(map[string]tracker.IssueState{})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce() = %v; want nil", err)
	}
	if got := dispatcher.count(); got != 0 {
		t.Fatalf("dispatched %d issues (%v); want 0", got, dispatcher.issueIDs())
	}
	retained, claimed := continuationClaimState(t, ctx, poller, "issue-1")
	if !retained || !claimed {
		t.Fatalf("continuation after omitted refresh outcome: retained=%v claimed=%v; want retained", retained, claimed)
	}
}

func TestPollOnceRetainsDueContinuationWhenRevalidationFetchFails(t *testing.T) {
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}}
	poller, dispatcher, ctx := startRevalidationHarness(t, trackerClient, revalidationReconcileConfig())
	seedDueContinuation(t, ctx, poller, tracker.Issue{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"})

	// With the fetch failing, a missing row is indistinguishable from tracker
	// downtime. The normalized Unknown outcome must preserve the continuation
	// (upstream {:error} leaves state untouched).
	trackerClient.setFetchIDStates(map[string]string{})
	trackerClient.setFetchIDErr(errors.New("tracker briefly down"))
	if err := poller.PollOnce(ctx); err == nil || !strings.Contains(err.Error(), "revalidate dispatch candidates") {
		t.Fatalf("PollOnce() = %v; want wrapped revalidation fetch error", err)
	}
	if got := dispatcher.count(); got != 0 {
		t.Fatalf("dispatched %d issues (%v); want 0", got, dispatcher.issueIDs())
	}
	retained, claimed := continuationClaimState(t, ctx, poller, "issue-1")
	if !retained || !claimed {
		t.Fatalf("continuation after failed revalidation fetch: retained=%v claimed=%v; want retained", retained, claimed)
	}
}

func TestPollOncePartialRevalidationErrorStillDispatchesFreshCandidates(t *testing.T) {
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{
		{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"},
		{ID: "issue-2", Identifier: "LIN-2", State: "In Progress"},
	}}
	poller, dispatcher, ctx := startRevalidationHarness(t, trackerClient, revalidationReconcileConfig())

	// A partial refresh (one row plus an error) dispatches the row it did
	// confirm and skips the rest; the error surfaces as a non-fatal poll error.
	trackerClient.setFetchIDStates(map[string]string{"issue-1": "In Progress"})
	trackerClient.setFetchIDErr(errors.New("revalidation fetch interrupted"))
	err := poller.PollOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "revalidate dispatch candidates") || !strings.Contains(err.Error(), "revalidation fetch interrupted") {
		t.Fatalf("PollOnce() = %v; want wrapped revalidation fetch error", err)
	}
	if got := dispatcher.issueIDs(); len(got) != 1 || got[0] != "issue-1" {
		t.Fatalf("dispatched issues = %v; want [issue-1]", got)
	}
}

// todoBlockerRevalidationConfig mirrors revalidationReconcileConfig but keeps
// Todo active so the SPEC §8.2 blocker gate (Todo-only) is exercisable.
func todoBlockerRevalidationConfig() ReconciliationConfig {
	return ReconciliationConfig{
		ActiveStates:      []string{"Todo", "In Progress"},
		TerminalStates:    []string{"Cancelled", "Done"},
		WorkerExitTimeout: time.Second,
	}
}

// TestPollOnceSkipsDispatchWhenRevalidationShowsReopenedBlocker pins #750: a
// Todo candidate that passed the tick-start blocker gate (blocker terminal at
// listing time) is dropped when the pre-dispatch refresh shows the blocker
// back in a non-terminal state, matching upstream retry_candidate_issue?'s
// !todo_issue_blocked_by_non_terminal? on the refreshed issue
// (orchestrator.ex:1602-1604).
func TestPollOnceSkipsDispatchWhenRevalidationShowsReopenedBlocker(t *testing.T) {
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{
		ID:         "issue-1",
		Identifier: "GT-1",
		State:      "Todo",
		BlockedBy:  []tracker.BlockerRef{{ID: "blocker-1", Identifier: "GT-9", State: "Done"}},
	}}}
	poller, dispatcher, ctx := startRevalidationHarness(t, trackerClient, todoBlockerRevalidationConfig())

	trackerClient.setFetchIDIssueStates(map[string]tracker.IssueState{
		"issue-1": {Outcome: tracker.IssueStateOutcomeCurrent, State: "Todo", BlockedBy: []tracker.BlockerRef{{ID: "blocker-1", Identifier: "GT-9", State: "In Progress"}}},
	})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce() = %v; want nil", err)
	}
	if got := dispatcher.count(); got != 0 {
		t.Fatalf("dispatched %d issues (%v); want 0 after the refreshed blocker reopened", got, dispatcher.issueIDs())
	}
}

// A refresh that positively reports "no blockers" (non-nil empty) dispatches,
// and a refresh without blocker knowledge (nil BlockedBy) keeps the
// listing-time verdict instead of clearing it.
func TestPollOnceBlockerRevalidationHonorsNilVersusEmptyContract(t *testing.T) {
	t.Run("non-nil empty refreshed blockers dispatch", func(t *testing.T) {
		trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{
			ID:         "issue-1",
			Identifier: "GT-1",
			State:      "Todo",
			BlockedBy:  []tracker.BlockerRef{{ID: "blocker-1", Identifier: "GT-9", State: "Done"}},
		}}}
		poller, dispatcher, ctx := startRevalidationHarness(t, trackerClient, todoBlockerRevalidationConfig())

		trackerClient.setFetchIDIssueStates(map[string]tracker.IssueState{
			"issue-1": {Outcome: tracker.IssueStateOutcomeCurrent, State: "Todo", BlockedBy: []tracker.BlockerRef{}},
		})
		if err := poller.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce() = %v; want nil", err)
		}
		if got := dispatcher.count(); got != 1 {
			t.Fatalf("dispatched %d issues (%v); want 1 when the refresh positively reports no blockers", got, dispatcher.issueIDs())
		}
	})
	t.Run("nil refreshed blockers keep the listing verdict", func(t *testing.T) {
		// Listing blockers are all terminal (the candidate passed the
		// tick-start gate); a nil-BlockedBy refresh must not clear them, and
		// re-running the gate on them stays a pass — the candidate
		// dispatches with its listing-time blocker data intact.
		trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{
			ID:         "issue-1",
			Identifier: "GT-1",
			State:      "Todo",
			BlockedBy:  []tracker.BlockerRef{{ID: "blocker-1", Identifier: "GT-9", State: "Done"}},
		}}}
		poller, dispatcher, ctx := startRevalidationHarness(t, trackerClient, todoBlockerRevalidationConfig())

		trackerClient.setFetchIDIssueStates(map[string]tracker.IssueState{
			"issue-1": {Outcome: tracker.IssueStateOutcomeCurrent, State: "Todo"},
		})
		if err := poller.PollOnce(ctx); err != nil {
			t.Fatalf("PollOnce() = %v; want nil", err)
		}
		if got := dispatcher.count(); got != 1 {
			t.Fatalf("dispatched %d issues (%v); want 1 when the refresh has no blocker knowledge", got, dispatcher.issueIDs())
		}
		if got := dispatcher.issueAt(0).BlockedBy; len(got) != 1 || got[0].ID != "blocker-1" {
			t.Fatalf("dispatched issue BlockedBy = %#v; want listing-time blocker kept", got)
		}
	})
}

// A non-Todo refreshed state leaves the blocker gate inert (SPEC §8.2 gates
// only Todo issues), even when the refresh carries non-terminal blockers.
func TestPollOnceBlockerRevalidationGatesTodoOnly(t *testing.T) {
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{
		ID:         "issue-1",
		Identifier: "GT-1",
		State:      "Todo",
	}}}
	poller, dispatcher, ctx := startRevalidationHarness(t, trackerClient, todoBlockerRevalidationConfig())

	trackerClient.setFetchIDIssueStates(map[string]tracker.IssueState{
		"issue-1": {Outcome: tracker.IssueStateOutcomeCurrent, State: "In Progress", BlockedBy: []tracker.BlockerRef{{ID: "blocker-1", Identifier: "GT-9", State: "In Progress"}}},
	})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce() = %v; want nil", err)
	}
	if got := dispatcher.count(); got != 1 {
		t.Fatalf("dispatched %d issues (%v); want 1 — the blocker gate applies to Todo only", got, dispatcher.issueIDs())
	}
}
