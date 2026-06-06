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

// fakeLinearRequest captures one GraphQL request the ListIssues fakes record
// for assertions on operation name, query shape, and variables.
type fakeLinearRequest struct {
	OpName    string
	Query     string
	Variables map[string]any
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
		for _, fragment := range []string{"priority", "branchName", "createdAt", "updatedAt", "labels(first: 50)"} {
			if !strings.Contains(payload.Query, fragment) {
				t.Fatalf("ListIssues query = %s, want fragment %q", payload.Query, fragment)
			}
		}
		// Per #326, customFieldValues is NOT in Linear's GraphQL schema; the
		// query must omit it so the request stops 400'ing.
		for _, banned := range []string{"customFieldValues", "customField {"} {
			if strings.Contains(payload.Query, banned) {
				t.Fatalf("ListIssues query must not request %q (Linear schema rejects it): %s", banned, payload.Query)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[{"id":"issue-1","identifier":"LIN-1","title":"One","description":"","url":"https://linear.app/acme/issue/LIN-1","priority":1,"branchName":"agent/lin-1","createdAt":"2026-05-15T00:00:00Z","updatedAt":"2026-05-16T00:00:00Z","labels":{"nodes":[{"name":"Backend"},{"name":"Customer"}]},"state":{"name":"In Progress"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`)
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

// TestListIssuesByStatesUsesLinearSupportedQueryShape pins #326: the query
// must not ask Linear for `customFieldValues` (the GraphQL schema does not
// expose that field on Issue; every poll 400'd with GRAPHQL_VALIDATION_FAILED
// before this regression was caught). Verified against live Linear
// introspection 2026-05-23.
func TestListIssuesByStatesUsesLinearSupportedQueryShape(t *testing.T) {
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &payload)
		for _, banned := range []string{"customFieldValues", "customField {"} {
			if strings.Contains(payload.Query, banned) {
				t.Fatalf("ListIssues query must not request %q (Linear schema rejects it): %s", banned, payload.Query)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[{"id":"issue-1","identifier":"LIN-1","title":"One","description":"","url":"https://linear.app/acme/issue/LIN-1","priority":1,"createdAt":"2026-05-15T00:00:00Z","updatedAt":"2026-05-16T00:00:00Z","labels":{"nodes":[]},"state":{"name":"In Progress"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`)
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
		fmt.Fprint(&b, i)
		b.WriteString(`","identifier":"LIN-`)
		fmt.Fprint(&b, i)
		b.WriteString(`","title":"Issue","description":"","url":"https://linear.app/acme/issue/LIN-`)
		fmt.Fprint(&b, i)
		b.WriteString(`","priority":1,"createdAt":"2026-05-15T00:00:00Z","updatedAt":"2026-05-16T00:00:00Z","state":{"name":"In Progress"}}`)
	}
	b.WriteString(`],"pageInfo":{"hasNextPage":`)
	fmt.Fprint(&b, hasNext)
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
		requests = append(requests, fakeLinearRequest{OpName: op, Query: payload.Query, Variables: payload.Variables})
		idx := page
		if op == "ListIssues" {
			page++
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if op == "ListIssuesInverseRelations" {
			_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[{"id":"issue-1","inverseRelations":{"nodes":[{"type":"blocks","issue":{"id":"blocker-1","identifier":"LIN-0","state":{"name":"In Progress"}}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}]}}}`)
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
	if ids, ok := requests[1].Variables["ids"].([]any); !ok || len(ids) != 1 || ids[0] != "issue-1" {
		t.Fatalf("blocker request ids = %v, want [issue-1]", requests[1].Variables["ids"])
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
		t.Fatalf("ListIssuesInverseRelations query = %s, want inverse relation blocker issue fields", requests[1].Query)
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
			// Batched first page (one request for all Todo ids): blocker-1 with a
			// next page so the per-issue overflow query (case 3) must still run.
			_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[{"id":"issue-1","inverseRelations":{"nodes":[{"type":"blocks","issue":{"id":"blocker-1","identifier":"LIN-0","state":{"name":"Done"}}}],"pageInfo":{"hasNextPage":true,"endCursor":"relation-cursor"}}}]}}}`)
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
	if ids, ok := requests[1].Variables["ids"].([]any); !ok || len(ids) != 1 || ids[0] != "issue-1" {
		t.Fatalf("first relation request ids = %#v, want batched [issue-1]", requests[1].Variables["ids"])
	}
	if requests[2].Variables["id"] != "issue-1" || requests[2].Variables["after"] != "relation-cursor" {
		t.Fatalf("overflow relation request variables = %#v, want issue id and relation cursor", requests[2].Variables)
	}
}

// TestListIssuesByStatesBatchesBlockerLookupsForManyTodoIssues pins #672: three
// Todo issues on one page resolve their blockers in a single batched query
// (2 requests total) rather than one query per issue (the prior N+1 = 4
// requests), while every issue still receives its own correct blockers.
func TestListIssuesByStatesBatchesBlockerLookupsForManyTodoIssues(t *testing.T) {
	var mu sync.Mutex
	var requests []fakeLinearRequest
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &payload)
		op := opNameFromQuery(payload.Query)
		mu.Lock()
		requests = append(requests, fakeLinearRequest{OpName: op, Query: payload.Query, Variables: payload.Variables})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch op {
		case "ListIssues":
			_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[`+
				`{"id":"issue-1","identifier":"LIN-1","title":"One","description":"","url":"u1","priority":1,"createdAt":"2026-05-15T00:00:00Z","updatedAt":"2026-05-16T00:00:00Z","state":{"name":"Todo"}},`+
				`{"id":"issue-2","identifier":"LIN-2","title":"Two","description":"","url":"u2","priority":1,"createdAt":"2026-05-15T00:00:00Z","updatedAt":"2026-05-16T00:00:00Z","state":{"name":"Todo"}},`+
				`{"id":"issue-3","identifier":"LIN-3","title":"Three","description":"","url":"u3","priority":1,"createdAt":"2026-05-15T00:00:00Z","updatedAt":"2026-05-16T00:00:00Z","state":{"name":"Todo"}}`+
				`],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`)
		case "ListIssuesInverseRelations":
			_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[`+
				`{"id":"issue-1","inverseRelations":{"nodes":[{"type":"blocks","issue":{"id":"b1","identifier":"LIN-91","state":{"name":"In Progress"}}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}},`+
				`{"id":"issue-2","inverseRelations":{"nodes":[{"type":"blocks","issue":{"id":"b2","identifier":"LIN-92","state":{"name":"Done"}}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}},`+
				`{"id":"issue-3","inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":""}}}`+
				`]}}}`)
		default:
			t.Fatalf("unexpected op %q", op)
		}
	}))
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{ProjectSlug: "aiops"})

	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if got, want := len(requests), 2; got != want {
		t.Fatalf("requests = %d, want %d (one candidate page + one batched blocker query, not N+1)", got, want)
	}
	if requests[1].OpName != "ListIssuesInverseRelations" {
		t.Fatalf("second op = %q, want ListIssuesInverseRelations", requests[1].OpName)
	}
	ids, ok := requests[1].Variables["ids"].([]any)
	if !ok || len(ids) != 3 || ids[0] != "issue-1" || ids[1] != "issue-2" || ids[2] != "issue-3" {
		t.Fatalf("batched blocker ids = %#v, want [issue-1 issue-2 issue-3]", requests[1].Variables["ids"])
	}
	if got := len(issues[0].BlockedBy); got != 1 || issues[0].BlockedBy[0].Identifier != "LIN-91" {
		t.Fatalf("issue-1 blockers = %#v, want one LIN-91", issues[0].BlockedBy)
	}
	if got := len(issues[1].BlockedBy); got != 1 || issues[1].BlockedBy[0].Identifier != "LIN-92" {
		t.Fatalf("issue-2 blockers = %#v, want one LIN-92", issues[1].BlockedBy)
	}
	if got := len(issues[2].BlockedBy); got != 0 {
		t.Fatalf("issue-3 blockers = %d, want 0", got)
	}
}

// TestListIssuesByStatesChunksBlockerBatchAcrossPageSize pins the #672 chunk
// loop: when a page carries more Todo issues than linearIssuePageSize, the
// blocker batch is split into linearIssuePageSize-sized ListIssuesInverseRelations
// requests (mirroring FetchIssueStatesByIDs), and an issue in the trailing chunk
// still receives its blocker.
func TestListIssuesByStatesChunksBlockerBatchAcrossPageSize(t *testing.T) {
	const todoCount = 55 // > linearIssuePageSize (50): forces a second chunk
	var mu sync.Mutex
	var batchChunkSizes []int
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &payload)
		w.Header().Set("Content-Type", "application/json")
		switch opNameFromQuery(payload.Query) {
		case "ListIssues":
			var b strings.Builder
			b.WriteString(`{"data":{"issues":{"nodes":[`)
			for i := 1; i <= todoCount; i++ {
				if i > 1 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"id":"issue-%d","identifier":"LIN-%d","title":"T","description":"","url":"u","priority":1,"createdAt":"2026-05-15T00:00:00Z","updatedAt":"2026-05-16T00:00:00Z","state":{"name":"Todo"}}`, i, i)
			}
			b.WriteString(`],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`)
			_, _ = io.WriteString(w, b.String())
		case "ListIssuesInverseRelations":
			ids, _ := payload.Variables["ids"].([]any)
			mu.Lock()
			batchChunkSizes = append(batchChunkSizes, len(ids))
			mu.Unlock()
			var b strings.Builder
			b.WriteString(`{"data":{"issues":{"nodes":[`)
			for i, raw := range ids {
				id, _ := raw.(string)
				if i > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, `{"id":%q,"inverseRelations":{"nodes":[{"type":"blocks","issue":{"id":"b-%s","identifier":"BLK-%s","state":{"name":"In Progress"}}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}`, id, id, id)
			}
			b.WriteString(`]}}}`)
			_, _ = io.WriteString(w, b.String())
		default:
			t.Fatalf("unexpected op %q", opNameFromQuery(payload.Query))
		}
	}))
	defer httpSrv.Close()
	client := newTestClient(t, httpSrv, workflow.TrackerConfig{ProjectSlug: "aiops"})

	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if got, want := len(issues), todoCount; got != want {
		t.Fatalf("issues = %d, want %d", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	wantChunks := []int{linearIssuePageSize, todoCount - linearIssuePageSize}
	if len(batchChunkSizes) != len(wantChunks) || batchChunkSizes[0] != wantChunks[0] || batchChunkSizes[1] != wantChunks[1] {
		t.Fatalf("blocker batch chunk sizes = %v, want %v (one query per linearIssuePageSize chunk)", batchChunkSizes, wantChunks)
	}
	// An issue in the trailing chunk (issue-55) must still receive its blocker.
	if got := issues[todoCount-1]; len(got.BlockedBy) != 1 || got.BlockedBy[0].Identifier != "BLK-issue-55" {
		t.Fatalf("issues[%d].BlockedBy = %#v, want one BLK-issue-55", todoCount-1, got.BlockedBy)
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
		switch opNameFromQuery(payload.Query) {
		case "ListIssues":
			_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[{"id":"issue-1","identifier":"LIN-1","title":"One","description":"","url":"https://linear.app/acme/issue/LIN-1","priority":1,"createdAt":"2026-05-15T00:00:00Z","updatedAt":"2026-05-16T00:00:00Z","state":{"name":"Todo"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`)
		case "ListIssuesInverseRelations":
			// Batched first page reports a next page, forcing per-issue overflow.
			_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[{"id":"issue-1","inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"same-relation-cursor"}}}]}}}`)
		default:
			// Per-issue overflow query keeps reporting the same cursor until the cap.
			_, _ = io.WriteString(w, `{"data":{"issue":{"inverseRelations":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":"same-relation-cursor"}}}}}`)
		}
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

// TestListIssuesByStatesSkipsBlockerFetchForNonTodoIssue pins the isTodoState
// gate (#521 decomposition characterization): blockers are fetched ONLY for
// Todo-state issues, so a non-Todo issue must end with empty BlockedBy AND must
// not trigger a ListIssuesInverseRelations request. The existing pagination test
// only asserts the positive (Todo) branch; flipping the gate would survive it.
func TestListIssuesByStatesSkipsBlockerFetchForNonTodoIssue(t *testing.T) {
	var mu sync.Mutex
	var ops []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &payload)
		mu.Lock()
		ops = append(ops, opNameFromQuery(payload.Query))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		// Single In Progress (non-Todo) issue; if the gate is flipped and a
		// blocker fetch fires, the inverse-relations op falls through to this
		// same handler and is recorded below.
		_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[{"id":"issue-1","identifier":"LIN-1","title":"One","description":"","url":"https://linear.app/acme/issue/LIN-1","priority":1,"createdAt":"2026-05-15T00:00:00Z","updatedAt":"2026-05-16T00:00:00Z","state":{"name":"In Progress"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`)
	}))
	defer srv.Close()
	client := newTestClient(t, srv, workflow.TrackerConfig{ProjectSlug: "aiops"})

	issues, err := client.ListIssuesByStates(context.Background(), []string{"In Progress"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues = %d, want 1", len(issues))
	}
	if got := len(issues[0].BlockedBy); got != 0 {
		t.Fatalf("issues[0].BlockedBy = %d, want 0 (non-Todo issue must not fetch blockers)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, op := range ops {
		// Post-#672 a flipped gate fetches blockers via the batched op
		// ListIssuesInverseRelations (plural); a non-Todo issue must emit none.
		if op == "ListIssuesInverseRelations" {
			t.Fatalf("server ops = %v, want no ListIssuesInverseRelations (blockers fetched only for Todo state)", ops)
		}
	}
	if got, want := len(ops), 1; got != want {
		t.Fatalf("server ops = %v (len %d), want %d (single ListIssues call only)", ops, got, want)
	}
}

// TestListIssuesByStatesSkipsBlankLabelNames pins the empty-label skip branch
// (#521 decomposition characterization): a node label whose name is blank or
// whitespace-only is dropped from the mapped Labels slice; surviving names are
// trimmed and lower-cased. No existing test feeds a blank label name.
func TestListIssuesByStatesSkipsBlankLabelNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[{"id":"issue-1","identifier":"LIN-1","title":"One","description":"","url":"https://linear.app/acme/issue/LIN-1","priority":1,"createdAt":"2026-05-15T00:00:00Z","updatedAt":"2026-05-16T00:00:00Z","labels":{"nodes":[{"name":"  "},{"name":""},{"name":" Backend "}]},"state":{"name":"In Progress"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`)
	}))
	defer srv.Close()
	client := newTestClient(t, srv, workflow.TrackerConfig{ProjectSlug: "aiops"})

	issues, err := client.ListIssuesByStates(context.Background(), []string{"In Progress"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues = %d, want 1", len(issues))
	}
	if got := strings.Join(issues[0].Labels, ","); got != "backend" {
		t.Fatalf("issues[0].Labels = %q, want %q (blank/whitespace label names skipped, survivor trimmed+lowered)", got, "backend")
	}
}

// TestListIssuesByStatesReturnsParseErrorForMalformedTimestamp pins the
// parse-error propagation branch end-to-end (#521 decomposition
// characterization): a malformed createdAt/updatedAt routed THROUGH
// ListIssuesByStates must surface the parse error. parseLinearIssueTime is unit
// tested standalone, but no test exercises it via the list path; createdAt is
// parsed before updatedAt, so the first malformed field wins.
func TestListIssuesByStatesReturnsParseErrorForMalformedTimestamp(t *testing.T) {
	cases := []struct {
		name      string
		createdAt string
		updatedAt string
		wantField string
		wantValue string
	}{
		{"malformed-createdAt", "not-a-time", "2026-05-16T00:00:00Z", "createdAt", "not-a-time"},
		{"malformed-updatedAt", "2026-05-15T00:00:00Z", "also-bad", "updatedAt", "also-bad"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, fmt.Sprintf(`{"data":{"issues":{"nodes":[{"id":"issue-1","identifier":"LIN-1","title":"One","description":"","url":"https://linear.app/acme/issue/LIN-1","priority":1,"createdAt":%q,"updatedAt":%q,"state":{"name":"In Progress"}}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`, c.createdAt, c.updatedAt))
			}))
			defer srv.Close()
			client := newTestClient(t, srv, workflow.TrackerConfig{ProjectSlug: "aiops"})

			issues, err := client.ListIssuesByStates(context.Background(), []string{"In Progress"})
			if err == nil {
				t.Fatalf("ListIssuesByStates(%s) = %v, nil; want parse error", c.name, issues)
			}
			if !strings.Contains(err.Error(), c.wantField) || !strings.Contains(err.Error(), c.wantValue) {
				t.Fatalf("ListIssuesByStates(%s) err = %q; want field %q and value %q", c.name, err.Error(), c.wantField, c.wantValue)
			}
		})
	}
}
