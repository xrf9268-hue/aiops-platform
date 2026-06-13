package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// These tests drive a real tracker.LinearClient through the full
// poll -> dispatch -> reconcile loop against a canned GraphQL httptest server
// (no containers), the Linear counterpart to poller_github_loop_test.go (#794).
// Linear, unlike the GitHub adapter, resolves blocker relations for Todo
// issues, so the SPEC §8.2 blocker gate is exercised here end-to-end.

// linearLoopServer routes the three GraphQL operations the loop issues — the
// ListIssues active listing, the IssueStatesByIDs narrow refresh, and the
// Todo-only ListIssuesInverseRelations blocker query — to per-operation
// response builders so a test can vary them by phase. IssueStatesByIDs is
// matched before the broader ListIssues check because both contain "Issue".
func linearLoopServer(t *testing.T, listNodes, refreshNodes, blockerNodes func() []any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		var nodes []any
		switch {
		case strings.Contains(req.Query, "IssueStatesByIDs"):
			nodes = refreshNodes()
		case strings.Contains(req.Query, "InverseRelations"):
			nodes = blockerNodes()
		case strings.Contains(req.Query, "ListIssues"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issues": map[string]any{
				"nodes":    listNodes(),
				"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
			}}})
			return
		default:
			t.Errorf("unexpected Linear GraphQL query: %s", req.Query)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"issues": map[string]any{"nodes": nodes}}})
	}))
}

func linearListNode(id, identifier, state string) any {
	return map[string]any{
		"id": id, "identifier": identifier, "title": "loop integration",
		"description": "", "url": "https://linear.app/x/issue/" + identifier,
		"priority": 2, "branchName": "", "createdAt": "2026-05-20T01:02:03.000Z",
		"updatedAt": "2026-05-20T02:03:04.000Z",
		"labels":    map[string]any{"nodes": []any{}},
		"state":     map[string]any{"name": state},
	}
}

func linearRefreshNode(id, state string) any {
	return map[string]any{
		"id": id, "state": map[string]any{"name": state},
		"labels": map[string]any{"nodes": []any{}},
	}
}

// linearBlockerNode is one ListIssuesInverseRelations node: issueID is blocked
// by blockerID (a "blocks" relation) which sits in blockerState.
func linearBlockerNode(issueID, blockerID, blockerState string) any {
	return map[string]any{
		"id": issueID,
		"inverseRelations": map[string]any{
			"nodes": []any{map[string]any{
				"type":  "blocks",
				"issue": map[string]any{"id": blockerID, "identifier": blockerID, "state": map[string]any{"name": blockerState}},
			}},
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
		},
	}
}

func newLinearLoopClient(t *testing.T, srv *httptest.Server) *tracker.LinearClient {
	t.Helper()
	client := tracker.NewLinearClient(workflow.TrackerConfig{
		APIKey:         "test-key",
		ProjectSlug:    "aiops-loop",
		ActiveStates:   []string{"Todo", "In Progress"},
		TerminalStates: []string{"Done", "Canceled"},
	})
	client.BaseURL = srv.URL
	client.HTTP = srv.Client()
	return client
}

func linearLoopReconcileConfig() ReconciliationConfig {
	return ReconciliationConfig{
		ActiveStates:      []string{"Todo", "In Progress"},
		TerminalStates:    []string{"Done", "Canceled"},
		WorkerExitTimeout: time.Second,
	}
}

func TestPollerLoopLinearClientDispatchesActiveIssue(t *testing.T) {
	noNodes := func() []any { return nil }
	srv := linearLoopServer(t,
		func() []any { return []any{linearListNode("lin-1", "AIS-1", "In Progress")} },
		func() []any { return []any{linearRefreshNode("lin-1", "In Progress")} },
		noNodes,
	)
	defer srv.Close()
	client := newLinearLoopClient(t, srv)

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

	poller := NewPollerWithReconciliation(client, orch, linearLoopReconcileConfig())
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll once: %v", err)
	}
	waitForDispatcherCount(t, dispatcher, 1)
	if got := dispatcher.issueAt(0); got.ID != "lin-1" || got.Identifier != "AIS-1" || got.State != "In Progress" {
		t.Fatalf("dispatched issue = %+v; want lin-1 AIS-1 In Progress", got)
	}
}

func TestPollerLoopLinearClientBlockerGateSuppressesTodoDispatch(t *testing.T) {
	// A Todo issue blocked by a non-terminal dependency must not dispatch. The
	// Linear listing resolves Todo blockers inline (fetchLinearIssuesPage ->
	// attachLinearBlockers), so the open "blocks" relation the inverse-relations
	// query reports lands on the candidate and the SPEC §8.2 eligibility gate
	// drops it before dispatch. (Dispatch-time revalidation re-applies the same
	// gate on refreshed data; that path is covered by the client unit tests.)
	srv := linearLoopServer(t,
		func() []any { return []any{linearListNode("lin-1", "AIS-1", "Todo")} },
		func() []any { return []any{linearRefreshNode("lin-1", "Todo")} },
		func() []any { return []any{linearBlockerNode("lin-1", "AIS-2", "In Progress")} },
	)
	defer srv.Close()
	client := newLinearLoopClient(t, srv)

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

	poller := NewPollerWithReconciliation(client, orch, linearLoopReconcileConfig())
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("poll once: %v", err)
	}
	// Give any erroneous dispatch a chance to register before asserting absence.
	time.Sleep(100 * time.Millisecond)
	if got := dispatcher.count(); got != 0 {
		t.Fatalf("dispatched %d issues; want 0 (Todo blocked by a non-terminal dependency must be gated)", got)
	}
}

func TestPollerLoopLinearClientReconcileCancelsIssueLeavingActive(t *testing.T) {
	var phase atomic.Int32
	state := func() string {
		if phase.Load() == 0 {
			return "In Progress"
		}
		return "Done"
	}
	srv := linearLoopServer(t,
		func() []any {
			if phase.Load() == 0 {
				return []any{linearListNode("lin-1", "AIS-1", "In Progress")}
			}
			return nil
		},
		func() []any { return []any{linearRefreshNode("lin-1", state())} },
		func() []any { return nil },
	)
	defer srv.Close()
	client := newLinearLoopClient(t, srv)

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

	poller := NewPollerWithReconciliation(client, orch, linearLoopReconcileConfig())
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll once: %v", err)
	}
	waitForCancellationDispatcherCount(t, dispatcher)

	phase.Store(1)
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("reconcile poll once: %v", err)
	}
	waitForContextCanceled(t, dispatcher.contextAt(0))
	waitForIssueNotRunningOrRetrying(t, ctx, orch, "lin-1")
}
