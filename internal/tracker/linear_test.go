package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	Query      string
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
			Query:      payload.Query,
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
		t.Fatalf("lookup Authorization = %q, want raw Linear API key", lookup.AuthHeader)
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

func TestLinearClient_SatisfiesIssueStateRefresher(t *testing.T) {
	var _ IssueStateRefresher = (*LinearClient)(nil)
}

func TestListIssuesByStatesRequiresProjectSlugAndUsesProjectFilter(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &payload)
		if !strings.Contains(payload.Query, "project: { slugId: { eq: $projectSlug } }") {
			t.Fatalf("ListIssues query = %s, want project slugId filter", payload.Query)
		}
		if payload.Variables["projectSlug"] != "aiops" {
			t.Fatalf("projectSlug variable = %v, want aiops", payload.Variables["projectSlug"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`)
	}))
	defer httpSrv.Close()

	missingSlug := newTestClient(t, httpSrv, workflow.TrackerConfig{})
	_, err := missingSlug.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err == nil || !strings.Contains(err.Error(), "Linear project slug is required") {
		t.Fatalf("ListIssuesByStates without project slug error = %v, want missing project slug", err)
	}

	client := newTestClient(t, httpSrv, workflow.TrackerConfig{ProjectSlug: "aiops"})
	if _, err := client.ListIssuesByStates(context.Background(), []string{"Todo"}); err != nil {
		t.Fatalf("ListIssuesByStates with project slug: %v", err)
	}
}

func TestListIssuesByStatesMapsSpecDomainFields(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &payload)
		for _, fragment := range []string{"priority", "branchName", "createdAt", "updatedAt", "project { slugId }", "team { key }", "labels(first: 50)", "customFieldValues(first: 50)", "customField { name }"} {
			if !strings.Contains(payload.Query, fragment) {
				t.Fatalf("ListIssues query = %s, want fragment %q", payload.Query, fragment)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[{"id":"issue-1","identifier":"LIN-1","title":"One","description":"","url":"https://linear.app/acme/issue/LIN-1","priority":1,"branchName":"agent/lin-1","createdAt":"2026-05-15T00:00:00Z","updatedAt":"2026-05-16T00:00:00Z","project":{"slugId":"api-platform"},"team":{"key":"ENG"},"labels":{"nodes":[{"name":"Backend"},{"name":"Customer"}]},"customFieldValues":{"nodes":[{"customField":{"name":"Runtime"},"value":"go"}]},"state":{"name":"In Progress"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`)
	}))
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{ProjectSlug: "api-platform"})

	issues, err := client.ListIssuesByStates(context.Background(), []string{"In Progress"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues = %d, want 1", len(issues))
	}
	issue := issues[0]
	if issue.ProjectSlug != "api-platform" || issue.TeamKey != "ENG" {
		t.Fatalf("issue route = project %q team %q, want api-platform/ENG", issue.ProjectSlug, issue.TeamKey)
	}
	if issue.Priority != 1 || issue.BranchName != "agent/lin-1" {
		t.Fatalf("issue priority/branch = %d/%q, want 1/agent/lin-1", issue.Priority, issue.BranchName)
	}
	createdAt := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	updatedAt := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)
	if !issue.CreatedAt.Equal(createdAt) || !issue.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("issue timestamps = %s/%s, want %s/%s", issue.CreatedAt, issue.UpdatedAt, createdAt, updatedAt)
	}
	if got := strings.Join(issue.Labels, ","); got != "backend,customer" {
		t.Fatalf("labels = %q, want lower-cased backend,customer", got)
	}
	if issue.CustomFields["Runtime"] != "go" {
		t.Fatalf("custom field Runtime = %q, want go", issue.CustomFields["Runtime"])
	}
}

func TestParseLinearIssueTimeErrorsOnMalformedTimestamp(t *testing.T) {
	_, err := parseLinearIssueTime("updatedAt", "not-a-timestamp")
	if err == nil {
		t.Fatal("parseLinearIssueTime malformed timestamp should error")
	}
	if !strings.Contains(err.Error(), "updatedAt") || !strings.Contains(err.Error(), "not-a-timestamp") {
		t.Fatalf("error = %q, want field name and bad value", err.Error())
	}
}

func TestListIssuesByStatesMapsNonStringCustomFieldValues(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[{"id":"issue-1","identifier":"LIN-1","title":"One","description":"","url":"https://linear.app/acme/issue/LIN-1","priority":1,"createdAt":"2026-05-15T00:00:00Z","updatedAt":"2026-05-16T00:00:00Z","project":{"slugId":"api-platform"},"team":{"key":"ENG"},"labels":{"nodes":[]},"customFieldValues":{"nodes":[{"customField":{"name":"Risk"},"value":3},{"customField":{"name":"CustomerFacing"},"value":true},{"customField":{"name":"Option"},"value":{"name":"gold"}}]},"state":{"name":"In Progress"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`)
	}))
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{ProjectSlug: "api-platform"})

	issues, err := client.ListIssuesByStates(context.Background(), []string{"In Progress"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues = %d, want 1", len(issues))
	}
	fields := issues[0].CustomFields
	if fields["Risk"] != "3" || fields["CustomerFacing"] != "true" || fields["Option"] != `{"name":"gold"}` {
		t.Fatalf("custom fields = %#v, want stringified non-string values", fields)
	}
}

func TestListIssuesByStatesUsesDefaultPageSizeAndAggregatesMoreThanFiftyIssues(t *testing.T) {
	var mu sync.Mutex
	var requests []fakeLinearRequest
	page := 0
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &payload)
		mu.Lock()
		requests = append(requests, fakeLinearRequest{OpName: opNameFromQuery(payload.Query), Query: payload.Query, Variables: payload.Variables})
		idx := page
		page++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if idx == 0 {
			_, _ = io.WriteString(w, linearIssuesPageJSON(1, 50, true, "cursor-50"))
			return
		}
		if idx == 1 {
			_, _ = io.WriteString(w, linearIssuesPageJSON(51, 55, false, ""))
			return
		}
		t.Fatalf("unexpected extra ListIssues request %d", idx+1)
	}))
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{ProjectSlug: "aiops"})

	issues, err := client.ListIssuesByStates(context.Background(), []string{"In Progress"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if got, want := len(issues), 55; got != want {
		t.Fatalf("issues = %d, want %d", got, want)
	}
	if issues[0].Identifier != "LIN-1" || issues[54].Identifier != "LIN-55" {
		t.Fatalf("issue range = %s..%s, want LIN-1..LIN-55", issues[0].Identifier, issues[54].Identifier)
	}
	if got, want := requests[0].Variables["first"], float64(50); got != want {
		t.Fatalf("first page size variable = %#v, want %#v", got, want)
	}
	if requests[0].Variables["after"] != nil || requests[1].Variables["after"] != "cursor-50" {
		t.Fatalf("after variables = %#v then %#v, want nil then cursor-50", requests[0].Variables["after"], requests[1].Variables["after"])
	}
}

func TestFetchIssueStatesByIDsUsesIDListQuery(t *testing.T) {
	var recorded fakeLinearRequest
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &payload)
		recorded = fakeLinearRequest{OpName: opNameFromQuery(payload.Query), Query: payload.Query, Variables: payload.Variables}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[{"id":"issue-1","state":{"name":"Todo"}},{"id":"issue-2","state":{"name":"Done"}}]}}}`)
	}))
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{ProjectSlug: "aiops"})

	states, err := client.FetchIssueStatesByIDs(context.Background(), []string{"issue-1", "issue-2"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	if got, want := states, map[string]string{"issue-1": "Todo", "issue-2": "Done"}; len(got) != len(want) || got["issue-1"] != want["issue-1"] || got["issue-2"] != want["issue-2"] {
		t.Fatalf("states = %#v, want %#v", got, want)
	}
	if recorded.OpName != "IssueStatesByIDs" {
		t.Fatalf("op = %q, want IssueStatesByIDs", recorded.OpName)
	}
	if !strings.Contains(recorded.Query, "$ids: [ID!]!") || !strings.Contains(recorded.Query, "id: { in: $ids }") || !strings.Contains(recorded.Query, "state { name }") {
		t.Fatalf("state refresh query = %s, want [ID!] id filter and state name", recorded.Query)
	}
	ids, ok := recorded.Variables["ids"].([]any)
	if !ok || len(ids) != 2 || ids[0] != "issue-1" || ids[1] != "issue-2" {
		t.Fatalf("ids variable = %#v, want []string{issue-1, issue-2}", recorded.Variables["ids"])
	}
}

func TestFetchIssueStatesByIDsChunksLargeBatches(t *testing.T) {
	var mu sync.Mutex
	var requests []fakeLinearRequest
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &payload)
		mu.Lock()
		requests = append(requests, fakeLinearRequest{OpName: opNameFromQuery(payload.Query), Query: payload.Query, Variables: payload.Variables})
		mu.Unlock()
		ids := payload.Variables["ids"].([]any)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, linearIssueStatesJSON(ids))
	}))
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{ProjectSlug: "aiops"})
	ids := make([]string, 0, linearIssuePageSize+5)
	for i := 1; i <= linearIssuePageSize+5; i++ {
		ids = append(ids, fmt.Sprintf("issue-%d", i))
	}

	states, err := client.FetchIssueStatesByIDs(context.Background(), ids)
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	if got, want := len(states), linearIssuePageSize+5; got != want {
		t.Fatalf("states = %d, want %d", got, want)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	if got, want := requests[0].Variables["first"], float64(linearIssuePageSize); got != want {
		t.Fatalf("first chunk size = %#v, want %#v", got, want)
	}
	firstIDs := requests[0].Variables["ids"].([]any)
	secondIDs := requests[1].Variables["ids"].([]any)
	if len(firstIDs) != linearIssuePageSize || len(secondIDs) != 5 {
		t.Fatalf("chunk lengths = %d, %d; want %d, 5", len(firstIDs), len(secondIDs), linearIssuePageSize)
	}
}

func linearIssueStatesJSON(ids []any) string {
	var b strings.Builder
	b.WriteString(`{"data":{"issues":{"nodes":[`)
	for i, id := range ids {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":"`)
		b.WriteString(id.(string))
		b.WriteString(`","state":{"name":"Todo"}}`)
	}
	b.WriteString(`]}}}`)
	return b.String()
}

func linearIssuesPageJSON(start, end int, hasNext bool, cursor string) string {
	var b strings.Builder
	b.WriteString(`{"data":{"issues":{"nodes":[`)
	for i := start; i <= end; i++ {
		if i > start {
			b.WriteByte(',')
		}
		b.WriteString(`{"id":"issue-`)
		b.WriteString(fmt.Sprint(i))
		b.WriteString(`","identifier":"LIN-`)
		b.WriteString(fmt.Sprint(i))
		b.WriteString(`","title":"Issue","description":"","url":"https://linear.app/acme/issue/LIN-`)
		b.WriteString(fmt.Sprint(i))
		b.WriteString(`","priority":1,"createdAt":"2026-05-15T00:00:00Z","updatedAt":"2026-05-16T00:00:00Z","state":{"name":"In Progress"}}`)
	}
	b.WriteString(`],"pageInfo":{"hasNextPage":`)
	b.WriteString(fmt.Sprint(hasNext))
	b.WriteString(`,"endCursor":"`)
	b.WriteString(cursor)
	b.WriteString(`"}}}}`)
	return b.String()
}

func TestListIssuesByStatesPaginates(t *testing.T) {
	var mu sync.Mutex
	var requests []fakeLinearRequest
	pages := []string{
		`{"data":{"issues":{"nodes":[{"id":"issue-1","identifier":"LIN-1","title":"One","description":"","url":"https://linear.app/acme/issue/LIN-1","priority":1,"createdAt":"2026-05-15T00:00:00Z","updatedAt":"2026-05-16T00:00:00Z","state":{"name":"Todo"}}],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"}}}}`,
		`{"data":{"issues":{"nodes":[{"id":"issue-2","identifier":"LIN-2","title":"Two","description":"","url":"https://linear.app/acme/issue/LIN-2","priority":2,"createdAt":"2026-05-15T00:01:00Z","updatedAt":"2026-05-16T00:01:00Z","state":{"name":"In Progress"}}],"pageInfo":{"hasNextPage":false,"endCursor":"cursor-2"}}}}`,
	}
	page := 0
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &payload)
		op := opNameFromQuery(payload.Query)
		mu.Lock()
		requests = append(requests, fakeLinearRequest{OpName: op, Query: payload.Query, Variables: payload.Variables, AuthHeader: r.Header.Get("Authorization")})
		idx := page
		if op == "ListIssues" {
			page++
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if op == "ListIssueInverseRelations" {
			_, _ = io.WriteString(w, `{"data":{"issue":{"inverseRelations":{"nodes":[{"type":"blocks","issue":{"id":"blocker-1","identifier":"LIN-0","state":{"name":"In Progress"}}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`)
			return
		}
		if idx >= len(pages) {
			t.Fatalf("unexpected extra ListIssues request")
		}
		_, _ = io.WriteString(w, pages[idx])
	}))
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{ProjectSlug: "aiops"})

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
	if issues[0].Priority != 1 || !issues[0].CreatedAt.Equal(time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("issue metadata = priority %d createdAt %s, want priority 1 createdAt 2026-05-15T00:00:00Z", issues[0].Priority, issues[0].CreatedAt)
	}
	if got := len(issues[0].BlockedBy); got != 1 {
		t.Fatalf("issue blockers = %d, want 1", got)
	}
	if blocker := issues[0].BlockedBy[0]; blocker.Identifier != "LIN-0" || blocker.State != "In Progress" {
		t.Fatalf("issue blocker = %#v, want LIN-0 in In Progress", blocker)
	}
	if got, want := len(requests), 3; got != want {
		t.Fatalf("requests = %d, want %d", got, want)
	}
	if requests[0].Variables["after"] != nil {
		t.Fatalf("first request after = %v, want nil", requests[0].Variables["after"])
	}
	if requests[1].Variables["id"] != "issue-1" {
		t.Fatalf("blocker request id = %v, want issue-1", requests[1].Variables["id"])
	}
	if requests[2].Variables["after"] != "cursor-1" {
		t.Fatalf("second ListIssues request after = %v, want cursor-1", requests[2].Variables["after"])
	}
	if strings.Contains(requests[0].Query, "blockedBy") {
		t.Fatalf("ListIssues query uses unsupported blockedBy field: %s", requests[0].Query)
	}
	if strings.Contains(requests[0].Query, "\n      relations") {
		t.Fatalf("ListIssues query uses outgoing relations for blockers: %s", requests[0].Query)
	}
	if strings.Contains(requests[0].Query, "inverseRelations") {
		t.Fatalf("ListIssues query should not fetch relation metadata for every candidate: %s", requests[0].Query)
	}
	if !strings.Contains(requests[1].Query, "inverseRelations") || !strings.Contains(requests[1].Query, "issue { id identifier state") {
		t.Fatalf("ListIssueInverseRelations query = %s, want inverse relation blocker issue fields", requests[1].Query)
	}
}

func TestListIssuesByStatesPaginatesLinearInverseRelationsBeforeMappingBlockers(t *testing.T) {
	var mu sync.Mutex
	var requests []fakeLinearRequest
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &payload)
		mu.Lock()
		requests = append(requests, fakeLinearRequest{OpName: opNameFromQuery(payload.Query), Query: payload.Query, Variables: payload.Variables})
		idx := len(requests)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch idx {
		case 1:
			_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[{"id":"issue-1","identifier":"LIN-1","title":"One","description":"","url":"https://linear.app/acme/issue/LIN-1","priority":1,"createdAt":"2026-05-15T00:00:00Z","updatedAt":"2026-05-16T00:00:00Z","state":{"name":"Todo"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`)
		case 2:
			_, _ = io.WriteString(w, `{"data":{"issue":{"inverseRelations":{"nodes":[{"type":"blocks","issue":{"id":"blocker-1","identifier":"LIN-0","state":{"name":"Done"}}}],"pageInfo":{"hasNextPage":true,"endCursor":"relation-cursor"}}}}}`)
		case 3:
			_, _ = io.WriteString(w, `{"data":{"issue":{"inverseRelations":{"nodes":[{"type":"blocks","issue":{"id":"blocker-2","identifier":"LIN-2","state":{"name":"In Progress"}}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}}`)
		default:
			t.Fatalf("unexpected extra Linear request %d", idx)
		}
	}))
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{ProjectSlug: "aiops"})

	issues, err := client.ListIssuesByStates(context.Background(), []string{"AI Ready"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if got := len(issues[0].BlockedBy); got != 2 {
		t.Fatalf("issue blockers = %d, want blockers from both inverse relation pages", got)
	}
	if blocker := issues[0].BlockedBy[1]; blocker.Identifier != "LIN-2" || blocker.State != "In Progress" {
		t.Fatalf("second-page blocker = %#v, want LIN-2 in In Progress", blocker)
	}
	if got := len(requests); got != 3 {
		t.Fatalf("requests = %d, want candidate page plus inverse relation pages", got)
	}
	if requests[1].Variables["id"] != "issue-1" || requests[1].Variables["after"] != nil {
		t.Fatalf("first relation request variables = %#v, want issue id and nil cursor", requests[1].Variables)
	}
	if requests[2].Variables["id"] != "issue-1" || requests[2].Variables["after"] != "relation-cursor" {
		t.Fatalf("second relation request variables = %#v, want issue id and relation cursor", requests[2].Variables)
	}
}

func TestListIssuesByStatesErrorsWhenNextPageCursorMissing(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":""}}}}`)
	}))
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{ProjectSlug: "aiops"})

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
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{ProjectSlug: "aiops"})

	_, err := client.ListIssuesByStates(context.Background(), []string{"AI Ready"})
	if err == nil || !strings.Contains(err.Error(), "linear pagination exceeded") {
		t.Fatalf("ListIssuesByStates error = %v, want max pages error", err)
	}
}

func TestListIssuesByStatesErrorsWhenInverseRelationMaxPagesExceeded(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &payload)
		w.Header().Set("Content-Type", "application/json")
		if opNameFromQuery(payload.Query) == "ListIssues" {
			_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[{"id":"issue-1","identifier":"LIN-1","title":"One","description":"","url":"https://linear.app/acme/issue/LIN-1","priority":1,"createdAt":"2026-05-15T00:00:00Z","updatedAt":"2026-05-16T00:00:00Z","state":{"name":"Todo"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"issue":{"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"same-relation-cursor"}}}}}`)
	}))
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{ProjectSlug: "aiops"})

	_, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err == nil || !strings.Contains(err.Error(), "linear inverse relation pagination exceeded") {
		t.Fatalf("ListIssuesByStates error = %v, want inverse relation max pages error", err)
	}
}

func TestLinearClient_SatisfiesStateIssueLister(t *testing.T) {
	var _ StateIssueLister = (*LinearClient)(nil)
}

func TestLinearClient_EnforcesRequestTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(block)
		srv.Close()
	})

	client := newTestClient(t, srv, workflow.TrackerConfig{ProjectSlug: "aiops"})
	client.RequestTimeout = 50 * time.Millisecond

	_, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want wrapping context.DeadlineExceeded", err)
	}
}

func TestNewLinearClient_DefaultsRequestTimeoutTo30s(t *testing.T) {
	client := NewLinearClient(workflow.TrackerConfig{APIKey: "k"})
	if client.RequestTimeout != 30*time.Second {
		t.Fatalf("default RequestTimeout = %v, want 30s", client.RequestTimeout)
	}
}

// TestNewLinearClientHonorsEndpointOverride pins SPEC §5.3.1 (#242): an
// explicit `tracker.endpoint` configures the Linear client's BaseURL.
// Workflows pointing at a httptest mock, a regional Linear endpoint, or a
// proxy can express the override in WORKFLOW.md without code changes.
func TestNewLinearClientHonorsEndpointOverride(t *testing.T) {
	client := NewLinearClient(workflow.TrackerConfig{APIKey: "k", Endpoint: "https://linear.example/graphql"})
	if client.BaseURL != "https://linear.example/graphql" {
		t.Fatalf("BaseURL = %q, want override from tracker.endpoint", client.BaseURL)
	}
}

func TestNewLinearClientDefaultsToSpecEndpoint(t *testing.T) {
	client := NewLinearClient(workflow.TrackerConfig{APIKey: "k"})
	if client.BaseURL != DefaultLinearEndpoint {
		t.Fatalf("BaseURL = %q, want DefaultLinearEndpoint when override absent", client.BaseURL)
	}
}

// TestNewLinearClientEndpointActuallyUsedForRequests verifies the override
// reaches the wire — `cmd.Process.Pid`-style "the field exists" tests caught
// the pre-#242 bug where BaseURL was set but immediately overwritten, so this
// test issues a real HTTP request through the override.
func TestNewLinearClientEndpointActuallyUsedForRequests(t *testing.T) {
	var observed string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`)
	}))
	defer srv.Close()

	endpoint := srv.URL + "/custom-graphql"
	client := NewLinearClient(workflow.TrackerConfig{APIKey: "k", ProjectSlug: "aiops", Endpoint: endpoint, ActiveStates: []string{"AI Ready"}})
	client.HTTP = srv.Client()
	if _, err := client.ListIssuesByStates(context.Background(), []string{"AI Ready"}); err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if observed != "/custom-graphql" {
		t.Fatalf("request path = %q, want /custom-graphql (Endpoint override reached the wire)", observed)
	}
}

func TestListIssuesByStatesEmptyShortCircuitsWithoutAPICall(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`)
	}))
	defer srv.Close()

	client := newTestClient(t, srv, workflow.TrackerConfig{ProjectSlug: "aiops"})

	cases := []struct {
		name   string
		states []string
	}{
		{"nil", nil},
		{"empty-slice", []string{}},
		{"single-empty-string", []string{""}},
		{"whitespace-only", []string{"  ", "\t"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			issues, err := client.ListIssuesByStates(context.Background(), c.states)
			if err != nil {
				t.Fatalf("ListIssuesByStates(%v) err = %v, want nil", c.states, err)
			}
			if len(issues) != 0 {
				t.Fatalf("ListIssuesByStates(%v) len = %d, want 0", c.states, len(issues))
			}
		})
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("server received %d requests, want 0 (SPEC §17.3: empty fetch_issues_by_states returns empty without API call)", got)
	}
}
