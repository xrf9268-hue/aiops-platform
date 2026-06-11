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
