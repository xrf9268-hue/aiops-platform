package orchestrator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// These tests drive a real tracker.GitHubClient — not an in-memory fake —
// through the full poll -> dispatch -> reconcile loop against canned-response
// httptest servers (no containers), mirroring the Gitea e2e loop shape. They
// close the gap the unit tests leave: github_test.go exercises the client in
// isolation and poller_test.go exercises the loop with fakeIssueStateTracker,
// but nothing composes the real client with NewPollerWithReconciliation (#794).

// githubLoopServer serves the GitHub REST endpoints the poll loop touches:
// the open-PR claim scan (/pulls), the active-issue listing (/issues), and the
// per-issue narrow state refresh (/issues/{number}). list/refresh return the
// JSON the caller supplies for the current phase so a test can flip an issue
// from active to terminal between ticks.
func githubLoopServer(t *testing.T, listBody, refreshBody func() any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/api/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case "/repos/acme/api/issues":
			_ = json.NewEncoder(w).Encode(listBody())
		case "/repos/acme/api/issues/159":
			_ = json.NewEncoder(w).Encode(refreshBody())
		case "/graphql":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"nodes": []any{
				map[string]any{
					"id": "I_159", "number": 159,
					"blockedBy": map[string]any{
						"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false},
					},
				},
			}}})
		default:
			t.Errorf("unexpected GitHub request path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func githubIssueJSON(label string) map[string]any {
	return map[string]any{
		"id": 159, "node_id": "I_159", "number": 159, "title": "loop integration", "body": "", "state": "open",
		"created_at": "2026-05-20T01:02:03Z", "updated_at": "2026-05-20T02:03:04Z",
		"labels": []map[string]any{{"name": label}},
	}
}

// waitForIssueNotRunningOrRetrying waits until the specific issue id leaves the
// running/retrying sets. The package's waitForNoRunningOrRetrying hard-codes
// "issue-1", so it can't be used for these tests' real issue ids (#794).
func waitForIssueNotRunningOrRetrying(t *testing.T, ctx context.Context, orch *Orchestrator, id IssueID) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		view, err := orch.Snapshot(ctx)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		if !hasRunningOrRetrying(view, id) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	view, err := orch.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	t.Fatalf("issue %s still running/retrying after reconcile cancel: running=%v retrying=%v", id, view.Running, view.Retrying)
}

func TestPollerLoopGitHubClientDispatchesActiveIssue(t *testing.T) {
	srv := githubLoopServer(t,
		func() any { return []map[string]any{githubIssueJSON("Todo")} },
		func() any { return githubIssueJSON("Todo") },
	)
	defer srv.Close()
	client := tracker.NewGitHubClient(workflow.TrackerConfig{
		APIKey: "test-token", ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"},
	}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dispatcher := &recordingDispatcher{releaseCh: make(chan struct{})}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPollerWithReconciliation(client, orch, ReconciliationConfig{
		ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}, WorkerExitTimeout: time.Second,
	})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll once: %v", err)
	}
	waitForDispatcherCount(t, dispatcher, 1)

	got := dispatcher.issueAt(0)
	if got.ID != "159" || got.Identifier != "#159" || got.State != "todo" {
		t.Fatalf("dispatched issue = %+v; want ID 159 identifier #159 state todo (mapped from the active label)", got)
	}
}

func TestPollerLoopGitHubClientReconcileCancelsIssueLeavingActive(t *testing.T) {
	// phase 0: the issue carries the active Todo label; phase 1: it has moved to
	// the terminal Done label and dropped out of the active listing, so the
	// reconcile pass must cancel the in-flight run on the next tick.
	// The dispatch path revalidates each candidate via the narrow state refresh
	// (/issues/{number}) before dispatching, so both the listing and the refresh
	// must report the active Todo label in phase 0 for the run to start; phase 1
	// flips both to the terminal Done label so the reconcile pass cancels it.
	var phase atomic.Int32
	label := func() string {
		if phase.Load() == 0 {
			return "Todo"
		}
		return "Done"
	}
	srv := githubLoopServer(t,
		func() any {
			if phase.Load() == 0 {
				return []map[string]any{githubIssueJSON("Todo")}
			}
			return []map[string]any{}
		},
		func() any { return githubIssueJSON(label()) },
	)
	defer srv.Close()
	client := tracker.NewGitHubClient(workflow.TrackerConfig{
		APIKey: "test-token", ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"},
	}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dispatcher := &cancellationDispatcher{}
	orch := New(NewOrchestratorState(30000, 1), Deps{
		Dispatcher: dispatcher,
		Scheduler:  RetryScheduler{MaxBackoff: time.Hour},
	})
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("wait for orchestrator: %v", err)
	}

	poller := NewPollerWithReconciliation(client, orch, ReconciliationConfig{
		ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}, WorkerExitTimeout: time.Second,
	})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll once: %v", err)
	}
	waitForCancellationDispatcherCount(t, dispatcher)

	phase.Store(1)
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("reconcile poll once: %v", err)
	}
	waitForContextCanceled(t, dispatcher.contextAt(0))
	waitForIssueNotRunningOrRetrying(t, ctx, orch, "159")
}
