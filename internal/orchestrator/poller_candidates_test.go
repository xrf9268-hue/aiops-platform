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

	// An empty refresh result is upstream's {:skip, :missing}: the issue was
	// deleted, or (Gitea) its aiops/* state labels were stripped so no state
	// can be derived.
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

func TestPollOnceReleasesDueContinuationWhenRevalidationOmitsIssue(t *testing.T) {
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}}
	poller, dispatcher, ctx := startRevalidationHarness(t, trackerClient, revalidationReconcileConfig())
	seedDueContinuation(t, ctx, poller, tracker.Issue{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"})

	// The refresh omits the issue entirely (deleted / Gitea aiops/* labels
	// stripped). Reconcile treats absence as no-information forever, so the
	// revalidation pass must release the queued continuation or the issue
	// wedges in retrying (#740 review P2).
	trackerClient.setFetchIDStates(map[string]string{})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("PollOnce() = %v; want nil", err)
	}
	if got := dispatcher.count(); got != 0 {
		t.Fatalf("dispatched %d issues (%v); want 0", got, dispatcher.issueIDs())
	}
	retained, claimed := continuationClaimState(t, ctx, poller, "issue-1")
	if retained || claimed {
		t.Fatalf("continuation after missing revalidation: retained=%v claimed=%v; want released", retained, claimed)
	}
}

func TestPollOnceRetainsDueContinuationWhenRevalidationFetchFails(t *testing.T) {
	trackerClient := &fakeIssueStateTracker{issues: []tracker.Issue{{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"}}}
	poller, dispatcher, ctx := startRevalidationHarness(t, trackerClient, revalidationReconcileConfig())
	seedDueContinuation(t, ctx, poller, tracker.Issue{ID: "issue-1", Identifier: "LIN-1", State: "In Progress"})

	// With the fetch failing, a missing row is indistinguishable from tracker
	// downtime — the continuation must survive (upstream {:error} leaves state
	// untouched).
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
