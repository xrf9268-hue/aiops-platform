package tracker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// fakeLinearServer is a single-handler GraphQL fake. Tests script its
// responses by name (the GraphQL operationName, derived from the first
// `query|mutation NAME(` token in the body) so a single test can answer
// both the workflowStates lookup and the issueUpdate mutation that
// MoveIssueToState issues. Recorded requests are exposed for
// assertions on payload shape and Authorization headers.
type fakeLinearServer struct {
	mu        sync.Mutex
	requests  []fakeLinearRequest
	responses map[string]string // op name -> JSON body to return
}

type fakeLinearRequest struct {
	OpName     string
	Variables  map[string]any
	AuthHeader string
}

func newFakeLinearServer() *fakeLinearServer {
	return &fakeLinearServer{responses: map[string]string{}}
}

func (f *fakeLinearServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &payload)
		op := opNameFromQuery(payload.Query)
		f.mu.Lock()
		f.requests = append(f.requests, fakeLinearRequest{
			OpName:     op,
			Variables:  payload.Variables,
			AuthHeader: r.Header.Get("Authorization"),
		})
		resp, ok := f.responses[op]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			// Default: succeed-shaped response so tests that don't pre-script
			// every op (e.g. lookup-only) don't have to.
			http.Error(w, `{"errors":[{"message":"unscripted op `+op+`"}]}`, http.StatusOK)
			return
		}
		_, _ = io.WriteString(w, resp)
	})
}

// opNameFromQuery extracts the GraphQL operation name from a query
// string. The Linear client formats queries as `query NAME(...)` or
// `mutation NAME(...)`, so a small token scan is enough; we avoid
// pulling in a real GraphQL parser to keep the test dependency-free.
func opNameFromQuery(q string) string {
	q = strings.TrimSpace(q)
	for _, prefix := range []string{"query ", "mutation "} {
		if strings.HasPrefix(q, prefix) {
			rest := q[len(prefix):]
			end := strings.IndexAny(rest, "( {")
			if end < 0 {
				return strings.TrimSpace(rest)
			}
			return strings.TrimSpace(rest[:end])
		}
	}
	return ""
}

func newTestClient(t *testing.T, srv *httptest.Server, cfg workflow.TrackerConfig) *LinearClient {
	t.Helper()
	if cfg.APIKey == "" {
		cfg.APIKey = "test-key"
	}
	c := NewLinearClient(cfg)
	c.BaseURL = srv.URL
	c.HTTP = srv.Client()
	return c
}

// TestMoveIssueToState_LooksUpStateThenMutates verifies the two-step
// flow: a workflowStates lookup (scoped to TeamKey when present)
// resolves the human-readable state name to its UUID, then issueUpdate
// mutates the issue. Both calls must carry the orchestrator-held Linear token.
func TestMoveIssueToState_LooksUpStateThenMutates(t *testing.T) {
	srv := newFakeLinearServer()
	srv.responses["StateByName"] = `{"data":{"workflowStates":{"nodes":[{"id":"state-uuid-1","name":"In Progress"}]}}}`
	srv.responses["IssueUpdate"] = `{"data":{"issueUpdate":{"success":true}}}`

	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{TeamKey: "ENG"})

	if err := client.MoveIssueToState(context.Background(), "issue-1", "In Progress"); err != nil {
		t.Fatalf("MoveIssueToState: %v", err)
	}

	if got := len(srv.requests); got != 2 {
		t.Fatalf("requests = %d, want 2 (lookup + mutate)", got)
	}
	lookup := srv.requests[0]
	if lookup.OpName != "StateByName" {
		t.Fatalf("first op = %q, want StateByName", lookup.OpName)
	}
	if lookup.Variables["name"] != "In Progress" {
		t.Fatalf("lookup name = %v, want \"In Progress\"", lookup.Variables["name"])
	}
	if lookup.Variables["teamKey"] != "ENG" {
		t.Fatalf("lookup teamKey = %v, want \"ENG\"", lookup.Variables["teamKey"])
	}
	if lookup.AuthHeader != "test-key" {
		t.Fatalf("lookup Authorization = %q, want raw Linear token", lookup.AuthHeader)
	}

	mutate := srv.requests[1]
	if mutate.OpName != "IssueUpdate" {
		t.Fatalf("second op = %q, want IssueUpdate", mutate.OpName)
	}
	if mutate.Variables["id"] != "issue-1" {
		t.Fatalf("mutate id = %v, want \"issue-1\"", mutate.Variables["id"])
	}
	if mutate.Variables["stateId"] != "state-uuid-1" {
		t.Fatalf("mutate stateId = %v, want \"state-uuid-1\"", mutate.Variables["stateId"])
	}
}

// TestMoveIssueToState_OmitsTeamKeyFilterWhenUnset confirms the lookup
// query drops the team filter when TeamKey is empty so single-team
// boards (the personal profile) work without operators having to fill
// in a teamKey they don't otherwise need.
func TestMoveIssueToState_OmitsTeamKeyFilterWhenUnset(t *testing.T) {
	srv := newFakeLinearServer()
	srv.responses["StateByName"] = `{"data":{"workflowStates":{"nodes":[{"id":"state-uuid","name":"Rework"}]}}}`
	srv.responses["IssueUpdate"] = `{"data":{"issueUpdate":{"success":true}}}`

	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{}) // no TeamKey

	if err := client.MoveIssueToState(context.Background(), "issue-1", "Rework"); err != nil {
		t.Fatalf("MoveIssueToState: %v", err)
	}
	if got := len(srv.requests); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	if _, present := srv.requests[0].Variables["teamKey"]; present {
		t.Fatalf("lookup variables should omit teamKey when unset, got %#v", srv.requests[0].Variables)
	}
}

// TestMoveIssueToState_NoMatchingStateReturnsError pins that a
// configured status name with no Linear workflow state behind it
// surfaces a clear error rather than silently no-op'ing. This is the
// signal OnFailure relies on to fall back to the comment path.
func TestMoveIssueToState_NoMatchingStateReturnsError(t *testing.T) {
	srv := newFakeLinearServer()
	srv.responses["StateByName"] = `{"data":{"workflowStates":{"nodes":[]}}}`

	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{})

	err := client.MoveIssueToState(context.Background(), "issue-1", "Mystery")
	if err == nil {
		t.Fatalf("expected error when no state matches, got nil")
	}
	if !strings.Contains(err.Error(), "Mystery") {
		t.Fatalf("error should name the missing state, got %q", err.Error())
	}
	// Lookup happens; mutation must not.
	if got := len(srv.requests); got != 1 {
		t.Fatalf("requests = %d, want 1 (lookup only)", got)
	}
}

// TestMoveIssueToState_GraphQLErrorBubblesUp pins that a Linear-side
// `errors` field is treated as a hard failure rather than absorbed.
// Without this assertion, a misconfigured TeamKey or revoked API key
// could leak through as a silent no-op.
func TestMoveIssueToState_GraphQLErrorBubblesUp(t *testing.T) {
	srv := newFakeLinearServer()
	srv.responses["StateByName"] = `{"data":null,"errors":[{"message":"unauthenticated"}]}`

	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{})

	if err := client.MoveIssueToState(context.Background(), "issue-1", "In Progress"); err == nil {
		t.Fatalf("expected error on GraphQL errors field, got nil")
	}
}

// TestMoveIssueToState_RejectsEmptyArgs guards the small input contract
// so callers (the worker hooks) get a deterministic error rather than a
// confusing 400 from Linear when invariants are missed.
func TestMoveIssueToState_RejectsEmptyArgs(t *testing.T) {
	client := NewLinearClient(workflow.TrackerConfig{APIKey: "k"})
	if err := client.MoveIssueToState(context.Background(), "", "In Progress"); err == nil {
		t.Fatal("empty issue id should error")
	}
	if err := client.MoveIssueToState(context.Background(), "issue-1", ""); err == nil {
		t.Fatal("empty state name should error")
	}

	noKey := NewLinearClient(workflow.TrackerConfig{})
	if err := noKey.MoveIssueToState(context.Background(), "issue-1", "In Progress"); err == nil {
		t.Fatal("missing API key should error")
	}
}

// TestAddComment_SendsCommentCreateMutation locks the GraphQL shape
// the worker's failure-fallback path depends on.
func TestAddComment_SendsCommentCreateMutation(t *testing.T) {
	srv := newFakeLinearServer()
	srv.responses["CommentCreate"] = `{"data":{"commentCreate":{"success":true}}}`

	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{})

	if err := client.AddComment(context.Background(), "issue-1", "AI run failed"); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if got := len(srv.requests); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
	req := srv.requests[0]
	if req.OpName != "CommentCreate" {
		t.Fatalf("op = %q, want CommentCreate", req.OpName)
	}
	if req.Variables["issueId"] != "issue-1" {
		t.Fatalf("variables.issueId = %v, want \"issue-1\"", req.Variables["issueId"])
	}
	if req.Variables["body"] != "AI run failed" {
		t.Fatalf("variables.body = %v, want \"AI run failed\"", req.Variables["body"])
	}
}

// TestAddComment_FailureSurfacesError pins the failure path so the
// worker's OnFailure can record a tracker_transition_error event when
// even the comment fallback can't be delivered.
func TestAddComment_FailureSurfacesError(t *testing.T) {
	srv := newFakeLinearServer()
	srv.responses["CommentCreate"] = `{"data":{"commentCreate":{"success":false}}}`

	httpSrv := httptest.NewServer(srv.handler())
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{})

	if err := client.AddComment(context.Background(), "issue-1", "msg"); err == nil {
		t.Fatal("AddComment with success=false should error")
	}
}

// TestLinearClient_SatisfiesTransitioner is a compile-time check that
// *LinearClient implements tracker.Transitioner. Worker.NewTransitioner
// returns the interface; this assertion catches a future signature
// drift before it manifests as a runtime nil interface assignment.
func TestLinearClient_SatisfiesTransitioner(t *testing.T) {
	var _ Transitioner = (*LinearClient)(nil)
}

func TestListIssuesByStatesPaginates(t *testing.T) {
	var mu sync.Mutex
	var requests []fakeLinearRequest
	pages := []string{
		`{"data":{"issues":{"nodes":[{"id":"issue-1","identifier":"LIN-1","title":"One","description":"","url":"https://linear.app/acme/issue/LIN-1","updatedAt":"2026-05-16T00:00:00Z","state":{"name":"AI Ready"}}],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"}}}}`,
		`{"data":{"issues":{"nodes":[{"id":"issue-2","identifier":"LIN-2","title":"Two","description":"","url":"https://linear.app/acme/issue/LIN-2","updatedAt":"2026-05-16T00:01:00Z","state":{"name":"In Progress"}}],"pageInfo":{"hasNextPage":false,"endCursor":"cursor-2"}}}}`,
	}
	page := 0
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &payload)
		mu.Lock()
		requests = append(requests, fakeLinearRequest{OpName: opNameFromQuery(payload.Query), Variables: payload.Variables, AuthHeader: r.Header.Get("Authorization")})
		idx := page
		page++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if idx >= len(pages) {
			t.Fatalf("unexpected extra ListIssues request")
		}
		_, _ = io.WriteString(w, pages[idx])
	}))
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{})

	issues, err := client.ListIssuesByStates(context.Background(), []string{"AI Ready", "In Progress"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if got, want := len(issues), 2; got != want {
		t.Fatalf("issues = %d, want %d", got, want)
	}
	if issues[0].Identifier != "LIN-1" || issues[1].Identifier != "LIN-2" {
		t.Fatalf("issue identifiers = %q, %q; want LIN-1, LIN-2", issues[0].Identifier, issues[1].Identifier)
	}
	if got, want := len(requests), 2; got != want {
		t.Fatalf("requests = %d, want %d", got, want)
	}
	if requests[0].Variables["after"] != nil {
		t.Fatalf("first request after = %v, want nil", requests[0].Variables["after"])
	}
	if requests[1].Variables["after"] != "cursor-1" {
		t.Fatalf("second request after = %v, want cursor-1", requests[1].Variables["after"])
	}
}

func TestListIssuesByStatesErrorsWhenNextPageCursorMissing(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":""}}}}`)
	}))
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{})

	_, err := client.ListIssuesByStates(context.Background(), []string{"AI Ready"})
	if err == nil || !strings.Contains(err.Error(), "linear pagination missing endCursor") {
		t.Fatalf("ListIssuesByStates error = %v, want missing cursor error", err)
	}
}

func TestListIssuesByStatesErrorsWhenMaxPagesExceeded(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"same-cursor"}}}}`)
	}))
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{})

	_, err := client.ListIssuesByStates(context.Background(), []string{"AI Ready"})
	if err == nil || !strings.Contains(err.Error(), "linear pagination exceeded") {
		t.Fatalf("ListIssuesByStates error = %v, want max pages error", err)
	}
}

func TestLinearClient_SatisfiesStateIssueLister(t *testing.T) {
	var _ StateIssueLister = (*LinearClient)(nil)
}
