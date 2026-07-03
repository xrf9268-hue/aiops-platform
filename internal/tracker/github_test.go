package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type githubPathErrorTransport struct {
	base http.RoundTripper
	path string
	err  error
}

func (t githubPathErrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == t.path {
		return nil, t.err
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func TestGitHubClientListIssuesByStatesMapsRepositoryIssues(t *testing.T) {
	var requestedPath string
	var requestedQuery string
	var authHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/api/pulls":
			if got := r.URL.Query().Get("state"); got != "open" {
				t.Fatalf("pull request state query = %q, want open", got)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case "/repos/acme/api/issues":
			requestedPath = r.URL.Path
			requestedQuery = r.URL.RawQuery
			authHeader = r.Header.Get("Authorization")
			_ = json.NewEncoder(w).Encode([]githubIssue{
				{
					ID:        159,
					Number:    159,
					Title:     "Follow up unresolved review thread",
					Body:      "review feedback",
					HTMLURL:   "https://github.com/acme/api/issues/159",
					State:     "open",
					CreatedAt: "2026-05-20T01:02:03Z",
					UpdatedAt: "2026-05-20T02:03:04Z",
					Labels:    []githubLabel{{Name: "priority:p2"}, {Name: "type:chore"}},
				},
				{
					ID:          160,
					Number:      160,
					Title:       "PR disguised as issue",
					State:       "open",
					PullRequest: &githubPullRequest{},
				},
			})
		default:
			t.Fatalf("unexpected request path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	client := NewGitHubClient(workflow.TrackerConfig{
		APIKey:       "test-token",
		ActiveStates: []string{"priority:p2"},
	}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()

	issues, err := client.ListActiveIssues(context.Background())
	if err != nil {
		t.Fatalf("ListActiveIssues: %v", err)
	}

	if requestedPath != "/repos/acme/api/issues" {
		t.Fatalf("request path = %q, want repository issues endpoint", requestedPath)
	}
	for _, want := range []string{"state=open", "labels=priority%3Ap2", "per_page=100", "page=1"} {
		if !strings.Contains(requestedQuery, want) {
			t.Fatalf("query = %q, want %s", requestedQuery, want)
		}
	}
	if authHeader != "Bearer test-token" {
		t.Fatalf("Authorization = %q, want Bearer token", authHeader)
	}
	if len(issues) != 1 {
		t.Fatalf("issues len = %d, want 1 non-PR issue", len(issues))
	}
	got := issues[0]
	if got.ID != "159" || got.Identifier != "#159" || got.Title != "Follow up unresolved review thread" {
		t.Fatalf("mapped issue identity = %+v", got)
	}
	if got.State != "priority:p2" {
		t.Fatalf("mapped state = %q, want requested label state", got.State)
	}
	if got.URL != "https://github.com/acme/api/issues/159" {
		t.Fatalf("URL = %q", got.URL)
	}
	if len(got.Labels) != 2 || got.Labels[0] != "priority:p2" || got.Labels[1] != "type:chore" {
		t.Fatalf("labels = %#v", got.Labels)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Fatalf("timestamps should be parsed: created=%s updated=%s", got.CreatedAt, got.UpdatedAt)
	}
}

func TestGitHubClientListIssuesByStatesPopulatesBlockedByFromNativeAndBodyFallback(t *testing.T) {
	var graphqlRequests int
	var fallbackRequested bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/api/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case "/repos/acme/api/issues":
			switch r.URL.Query().Get("page") {
			case "1", "":
				_ = json.NewEncoder(w).Encode([]githubIssue{
					{
						ID:        100,
						NodeID:    "I_100",
						Number:    10,
						Title:     "native dependency",
						Body:      "Depends on #12",
						HTMLURL:   "https://github.com/acme/api/issues/10",
						State:     "open",
						CreatedAt: "2026-05-20T01:02:03Z",
						UpdatedAt: "2026-05-20T02:03:04Z",
						Labels:    []githubLabel{{Name: "aiops:todo"}},
					},
					{
						ID:        110,
						NodeID:    "I_110",
						Number:    11,
						Title:     "body dependency",
						Body:      "Blocked by #13",
						HTMLURL:   "https://github.com/acme/api/issues/11",
						State:     "open",
						CreatedAt: "2026-05-20T01:02:03Z",
						UpdatedAt: "2026-05-20T02:03:04Z",
						Labels:    []githubLabel{{Name: "aiops:todo"}},
					},
					{
						ID:        140,
						NodeID:    "I_140",
						Number:    14,
						Title:     "foreign dependency",
						Body:      "Depends on other/service#99",
						HTMLURL:   "https://github.com/acme/api/issues/14",
						State:     "open",
						CreatedAt: "2026-05-20T01:02:03Z",
						UpdatedAt: "2026-05-20T02:03:04Z",
						Labels:    []githubLabel{{Name: "aiops:todo"}},
					},
				})
			default:
				_ = json.NewEncoder(w).Encode([]githubIssue{})
			}
		case "/graphql":
			graphqlRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"nodes": []any{
				map[string]any{
					"id":     "I_100",
					"number": float64(10),
					"blockedBy": map[string]any{
						"nodes": []any{map[string]any{
							"id":         "I_120",
							"databaseId": float64(120),
							"number":     float64(12),
							"state":      "OPEN",
							"labels": map[string]any{"nodes": []any{
								map[string]any{"name": "aiops:todo"},
							}},
						}},
						"pageInfo": map[string]any{"hasNextPage": false},
					},
				},
				map[string]any{
					"id":     "I_110",
					"number": float64(11),
					"blockedBy": map[string]any{
						"nodes":    []any{},
						"pageInfo": map[string]any{"hasNextPage": false},
					},
				},
				map[string]any{
					"id":     "I_140",
					"number": float64(14),
					"blockedBy": map[string]any{
						"nodes":    []any{},
						"pageInfo": map[string]any{"hasNextPage": false},
					},
				},
			}}})
		case "/repos/acme/api/issues/13":
			fallbackRequested = true
			_ = json.NewEncoder(w).Encode(githubIssue{
				ID:        130,
				NodeID:    "I_130",
				Number:    13,
				Title:     "done blocker",
				State:     "closed",
				Labels:    []githubLabel{{Name: "Done"}},
				CreatedAt: "2026-05-20T01:02:03Z",
				UpdatedAt: "2026-05-20T02:03:04Z",
			})
		default:
			t.Fatalf("unexpected request path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewGitHubClient(workflow.TrackerConfig{
		APIKey:         "test-token",
		ActiveStates:   []string{"aiops:todo"},
		TerminalStates: []string{"done", "closed"},
	}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()

	issues, err := client.ListActiveIssues(context.Background())
	if err != nil {
		t.Fatalf("ListActiveIssues: %v", err)
	}

	if graphqlRequests != 1 {
		t.Fatalf("graphql requests = %d, want 1", graphqlRequests)
	}
	if !fallbackRequested {
		t.Fatal("body fallback blocker #13 was not resolved")
	}
	if len(issues) != 3 {
		t.Fatalf("issues = %+v, want 3", issues)
	}
	if got := issues[0].BlockedBy; len(got) != 1 || got[0].Identifier != "#12" || got[0].State != "aiops:todo" {
		t.Fatalf("#10 blocked_by = %+v, want one native open blocker #12 in aiops:todo", got)
	}
	if got := issues[1].BlockedBy; len(got) != 1 || got[0].Identifier != "#13" || got[0].State != "done" {
		t.Fatalf("#11 blocked_by = %+v, want one fallback terminal blocker #13 in done", got)
	}
	if got := issues[2].BlockedBy; got == nil || len(got) != 0 {
		t.Fatalf("#14 blocked_by = %+v, want foreign repo body reference ignored", got)
	}
}

func TestGitHubClientListIssuesByStatesFailsClosedWhenNativeBlockersAreIncomplete(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/api/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case "/repos/acme/api/issues":
			_ = json.NewEncoder(w).Encode([]githubIssue{{
				ID:        100,
				NodeID:    "I_100",
				Number:    10,
				Title:     "too many blockers",
				HTMLURL:   "https://github.com/acme/api/issues/10",
				State:     "open",
				CreatedAt: "2026-05-20T01:02:03Z",
				UpdatedAt: "2026-05-20T02:03:04Z",
				Labels:    []githubLabel{{Name: "aiops:todo"}},
			}})
		case "/graphql":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"nodes": []any{
				map[string]any{
					"id":     "I_100",
					"number": float64(10),
					"blockedBy": map[string]any{
						"nodes":    []any{},
						"pageInfo": map[string]any{"hasNextPage": true},
					},
				},
			}}})
		default:
			t.Fatalf("unexpected request path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewGitHubClient(workflow.TrackerConfig{
		APIKey:         "test-token",
		ActiveStates:   []string{"aiops:todo"},
		TerminalStates: []string{"done", "closed"},
	}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()

	issues, err := client.ListActiveIssues(context.Background())
	if err != nil {
		t.Fatalf("ListActiveIssues: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues = %+v, want 1", issues)
	}
	if got := issues[0].BlockedBy; len(got) != 1 || got[0].State != "" {
		t.Fatalf("blocked_by = %+v, want one unknown-state blocker so Todo dispatch fails closed", got)
	}
}

func TestGitHubClientListIssuesByStatesSkipsBlockerLookupForNonTodoStates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/api/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case "/repos/acme/api/issues":
			_ = json.NewEncoder(w).Encode([]githubIssue{{
				ID:        100,
				NodeID:    "I_100",
				Number:    10,
				Title:     "terminal issue",
				Body:      "Blocked by #13",
				HTMLURL:   "https://github.com/acme/api/issues/10",
				State:     "closed",
				CreatedAt: "2026-05-20T01:02:03Z",
				UpdatedAt: "2026-05-20T02:03:04Z",
				Labels:    []githubLabel{{Name: "done"}},
			}})
		case "/graphql", "/repos/acme/api/issues/13":
			t.Fatalf("ListIssuesByStates(done) must not hydrate blockers via %s", r.URL.Path)
		default:
			t.Fatalf("unexpected request path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewGitHubClient(workflow.TrackerConfig{
		APIKey:         "test-token",
		TerminalStates: []string{"done"},
	}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()

	issues, err := client.ListIssuesByStates(context.Background(), []string{"done"})
	if err != nil {
		t.Fatalf("ListIssuesByStates(done): %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues = %+v, want 1", issues)
	}
	if got := issues[0].BlockedBy; got == nil || len(got) != 0 {
		t.Fatalf("done issue BlockedBy = %#v; want non-nil empty blockers without lookup", got)
	}
}

func TestGitHubClientListIssuesByStatesReturnsErrorWhenNativeBlockerLookupIsRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/api/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case "/repos/acme/api/issues":
			_ = json.NewEncoder(w).Encode([]githubIssue{{
				ID:        100,
				NodeID:    "I_100",
				Number:    10,
				Title:     "unknown native dependency state",
				HTMLURL:   "https://github.com/acme/api/issues/10",
				State:     "open",
				CreatedAt: "2026-05-20T01:02:03Z",
				UpdatedAt: "2026-05-20T02:03:04Z",
				Labels:    []githubLabel{{Name: "aiops:todo"}},
			}})
		case "/graphql":
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"message":"rate limited"}`)
		default:
			t.Fatalf("unexpected request path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewGitHubClient(workflow.TrackerConfig{
		APIKey:       "test-token",
		ActiveStates: []string{"aiops:todo"},
	}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()

	issues, err := client.ListActiveIssues(context.Background())
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("ListActiveIssues error = %v; want errors.Is ErrRateLimited from native blockedBy lookup", err)
	}
	if issues != nil {
		t.Fatalf("ListActiveIssues issues = %#v; want nil when native blockedBy lookup is not authoritative", issues)
	}
}

func TestGitHubClientListIssuesByStatesOmitsBodyFallbackWhenLookupTransportFails(t *testing.T) {
	var fallbackRequested bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/api/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case "/repos/acme/api/issues":
			_ = json.NewEncoder(w).Encode([]githubIssue{{
				ID:        100,
				NodeID:    "I_100",
				Number:    10,
				Title:     "best effort body dependency",
				Body:      "Blocked by #13",
				HTMLURL:   "https://github.com/acme/api/issues/10",
				State:     "open",
				CreatedAt: "2026-05-20T01:02:03Z",
				UpdatedAt: "2026-05-20T02:03:04Z",
				Labels:    []githubLabel{{Name: "aiops:todo"}},
			}})
		case "/graphql":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"nodes": []any{
				map[string]any{
					"id":     "I_100",
					"number": float64(10),
					"blockedBy": map[string]any{
						"nodes":    []any{},
						"pageInfo": map[string]any{"hasNextPage": false},
					},
				},
			}}})
		case "/repos/acme/api/issues/13":
			fallbackRequested = true
			t.Fatalf("transport should fail before handler receives %s", r.URL.Path)
		default:
			t.Fatalf("unexpected request path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewGitHubClient(workflow.TrackerConfig{
		APIKey:       "test-token",
		ActiveStates: []string{"aiops:todo"},
	}, srv.URL, "acme", "api")
	base := srv.Client().Transport
	client.HTTP = &http.Client{Transport: githubPathErrorTransport{
		base: base,
		path: "/repos/acme/api/issues/13",
		err:  errors.New("body fallback transport failed"),
	}}

	issues, err := client.ListActiveIssues(context.Background())
	if err != nil {
		t.Fatalf("ListActiveIssues: %v", err)
	}
	if fallbackRequested {
		t.Fatal("body fallback handler was reached; want injected transport failure")
	}
	if len(issues) != 1 {
		t.Fatalf("issues = %+v, want 1", issues)
	}
	if got := issues[0].BlockedBy; got == nil || len(got) != 0 {
		t.Fatalf("blocked_by = %+v, want no synthetic body fallback blocker after transport failure", got)
	}
}

func TestGitHubClientListIssuesByStatesChunksNativeBlockerLookups(t *testing.T) {
	var graphqlBatchSizes []int
	issueRows := make([]githubIssue, 0, 51)
	for number := 1; number <= 51; number++ {
		issueRows = append(issueRows, githubIssue{
			ID:        int64(1000 + number),
			NodeID:    fmt.Sprintf("I_%d", number),
			Number:    number,
			Title:     fmt.Sprintf("issue %d", number),
			HTMLURL:   fmt.Sprintf("https://github.com/acme/api/issues/%d", number),
			State:     "open",
			CreatedAt: "2026-05-20T01:02:03Z",
			UpdatedAt: "2026-05-20T02:03:04Z",
			Labels:    []githubLabel{{Name: "aiops:todo"}},
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/api/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case "/repos/acme/api/issues":
			_ = json.NewEncoder(w).Encode(issueRows)
		case "/graphql":
			var req struct {
				Variables struct {
					IDs []string `json:"ids"`
				} `json:"variables"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode graphql request: %v", err)
			}
			graphqlBatchSizes = append(graphqlBatchSizes, len(req.Variables.IDs))
			nodes := make([]any, 0, len(req.Variables.IDs))
			for _, id := range req.Variables.IDs {
				nodes = append(nodes, map[string]any{
					"id":     id,
					"number": float64(1),
					"blockedBy": map[string]any{
						"nodes":    []any{},
						"pageInfo": map[string]any{"hasNextPage": false},
					},
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"nodes": nodes}})
		default:
			t.Fatalf("unexpected request path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewGitHubClient(workflow.TrackerConfig{
		APIKey:       "test-token",
		ActiveStates: []string{"aiops:todo"},
	}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()

	issues, err := client.ListActiveIssues(context.Background())
	if err != nil {
		t.Fatalf("ListActiveIssues: %v", err)
	}

	if got, want := graphqlBatchSizes, []int{25, 25, 1}; !slices.Equal(got, want) {
		t.Fatalf("graphql batch sizes = %v; want %v", got, want)
	}
	if len(issues) != len(issueRows) {
		t.Fatalf("issues len = %d, want %d", len(issues), len(issueRows))
	}
	for _, issue := range issues {
		if issue.BlockedBy == nil || len(issue.BlockedBy) != 0 {
			t.Fatalf("issue %s BlockedBy = %#v, want non-nil empty blockers", issue.Identifier, issue.BlockedBy)
		}
	}
}

func TestGitHubClientListIssuesByStatesSkipsIssuesClaimedByOpenPR(t *testing.T) {
	var pullsRequested bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/api/pulls":
			pullsRequested = true
			if got := r.URL.Query().Get("state"); got != "open" {
				t.Fatalf("pull request state query = %q, want open", got)
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number":   200,
				"title":    "Fix duplicate dispatch",
				"body":     "Issue #159\n\nSee also #161.",
				"state":    "open",
				"html_url": "https://github.com/acme/api/pull/200",
			}})
		case "/repos/acme/api/issues":
			_ = json.NewEncoder(w).Encode([]githubIssue{
				{
					ID:        159,
					Number:    159,
					Title:     "Already has PR",
					HTMLURL:   "https://github.com/acme/api/issues/159",
					State:     "open",
					CreatedAt: "2026-05-20T01:02:03Z",
					UpdatedAt: "2026-05-20T02:03:04Z",
					Labels:    []githubLabel{{Name: "priority:p2"}},
				},
				{
					ID:        160,
					Number:    160,
					Title:     "Needs work",
					HTMLURL:   "https://github.com/acme/api/issues/160",
					State:     "open",
					CreatedAt: "2026-05-20T01:02:03Z",
					UpdatedAt: "2026-05-20T02:03:04Z",
					Labels:    []githubLabel{{Name: "priority:p2"}},
				},
				{
					ID:        161,
					Number:    161,
					Title:     "Only casually mentioned by PR",
					HTMLURL:   "https://github.com/acme/api/issues/161",
					State:     "open",
					CreatedAt: "2026-05-20T01:02:03Z",
					UpdatedAt: "2026-05-20T02:03:04Z",
					Labels:    []githubLabel{{Name: "priority:p2"}},
				},
			})
		default:
			t.Fatalf("unexpected request path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	client := NewGitHubClient(workflow.TrackerConfig{
		APIKey:       "test-token",
		ActiveStates: []string{"priority:p2"},
	}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()

	issues, err := client.ListActiveIssues(context.Background())
	if err != nil {
		t.Fatalf("ListActiveIssues: %v", err)
	}

	if !pullsRequested {
		t.Fatal("expected open pull request lookup before dispatching GitHub issues")
	}
	if len(issues) != 2 {
		t.Fatalf("issues len = %d, want issues not claimed by open PRs", len(issues))
	}
	if issues[0].Identifier != "#160" {
		t.Fatalf("dispatched issue = %s, want #160", issues[0].Identifier)
	}
	if issues[1].Identifier != "#161" {
		t.Fatalf("second dispatched issue = %s, want #161", issues[1].Identifier)
	}
}

func TestGitHubClaimedIssueNumbersMatchesPRContract(t *testing.T) {
	got := githubClaimedIssueNumbers("Closes #10\nFixes acme/api#11\nIssue #13\nGitHub issue: #14\nAssigned issue #15\nSee also #12\nresolved #10")
	want := []int{10, 11, 13, 14, 15}
	if len(got) != len(want) {
		t.Fatalf("claimed issue numbers = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("claimed issue numbers = %#v, want %#v", got, want)
		}
	}
	for _, text := range []string{"See also #10", "Related to #11", "Not a claim"} {
		if got := githubClaimedIssueNumbers(text); len(got) != 0 {
			t.Fatalf("claimed issue numbers = %#v, want none", got)
		}
	}
}

// TestGitHubClientListIssuesByStatesErrorsWhenStateCollectionOverflows pins
// the fail-safe contract for #401: if any state collection overflows pagination,
// ListIssuesByStates must surface ErrIssueListingCapped (with nil issues) so
// startup reconciliation does not treat the partial result as authoritative and
// delete workspaces for active issues that fell past the page cap.
func TestGitHubClientListIssuesByStatesErrorsWhenStateCollectionOverflows(t *testing.T) {
	var logs []string
	openIssuePages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/api/pulls":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[]`)
		case "/repos/acme/api/issues":
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Query().Get("state") {
			case "open":
				openIssuePages++
				w.Header().Set("Link", `<`+r.URL.String()+`>; rel="next"`)
				if r.URL.Query().Get("page") == "1" {
					_ = json.NewEncoder(w).Encode([]githubIssue{{ID: 42, Number: 42, State: "open"}})
					return
				}
				_, _ = io.WriteString(w, `[]`)
			case "closed":
				_ = json.NewEncoder(w).Encode([]githubIssue{{ID: 42, Number: 42, State: "closed"}})
			default:
				t.Fatalf("unexpected issue state query %q", r.URL.Query().Get("state"))
			}
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewGitHubClient(workflow.TrackerConfig{APIKey: "token", PaginationMaxPages: 1}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()
	client.Logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	issues, err := client.ListIssuesByStates(context.Background(), []string{"open", "closed"})
	if !errors.Is(err, ErrIssueListingCapped) {
		t.Fatalf("ListIssuesByStates err = %v, want ErrIssueListingCapped", err)
	}
	if issues != nil {
		t.Fatalf("issues = %#v, want nil when listing is partial", issues)
	}
	if openIssuePages != 2 {
		t.Fatalf("open issue pages = %d, want configured max page plus probe", openIssuePages)
	}
	if got := client.PaginationCapHits(); got != 1 {
		t.Fatalf("PaginationCapHits = %d, want 1", got)
	}
	if len(logs) == 0 || !strings.Contains(logs[len(logs)-1], "failing this tracker listing") {
		t.Fatalf("logs = %#v, want fail-loud diagnostic", logs)
	}
}

// TestGitHubClientListIssuesByStatesErrorsWhenOpenPRPaginationOverflows pins
// the fail-safe contract for #401 on the open-PR-claim-cap path: if the open PR
// listing overflows, any state collection that depends on a complete claim set
// is skipped AND ListIssuesByStates returns ErrIssueListingCapped so reconcile
// does not treat the resulting partial issue set as authoritative.
func TestGitHubClientListIssuesByStatesErrorsWhenOpenPRPaginationOverflows(t *testing.T) {
	for _, tc := range []struct {
		name              string
		states            []string
		skippedIssueState string
		skippedLabel      string
	}{
		{name: "open", states: []string{"open", "closed"}, skippedIssueState: "open"},
		{name: "all", states: []string{"all", "closed"}, skippedIssueState: "all"},
		{name: "label", states: []string{"priority:p2", "closed"}, skippedIssueState: "open", skippedLabel: "priority:p2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pullPages := 0
			skippedCollectionRequests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/acme/api/pulls":
					pullPages++
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Link", `<`+r.URL.String()+`>; rel="next"`)
					_ = json.NewEncoder(w).Encode([]githubPullRequestSummary{{Number: 7, State: "open", Title: "claim", Body: "Closes #43"}})
				case "/repos/acme/api/issues":
					w.Header().Set("Content-Type", "application/json")
					state := r.URL.Query().Get("state")
					label := r.URL.Query().Get("labels")
					if state == tc.skippedIssueState && label == tc.skippedLabel {
						skippedCollectionRequests++
						_ = json.NewEncoder(w).Encode([]githubIssue{{ID: 43, Number: 43, State: "open"}})
						return
					}
					if state == "closed" && label == "" {
						_ = json.NewEncoder(w).Encode([]githubIssue{{ID: 45, Number: 45, State: "closed"}})
						return
					}
					t.Fatalf("unexpected issue query state=%q labels=%q", state, label)
				default:
					t.Fatalf("unexpected path %s", r.URL.Path)
				}
			}))
			defer srv.Close()

			client := NewGitHubClient(workflow.TrackerConfig{APIKey: "token", PaginationMaxPages: 1}, srv.URL, "acme", "api")
			client.HTTP = srv.Client()
			issues, err := client.ListIssuesByStates(context.Background(), tc.states)
			if !errors.Is(err, ErrIssueListingCapped) {
				t.Fatalf("ListIssuesByStates err = %v, want ErrIssueListingCapped", err)
			}
			if issues != nil {
				t.Fatalf("issues = %#v, want nil when listing is partial", issues)
			}
			if pullPages != 2 {
				t.Fatalf("open pull request pages = %d, want configured max page plus cap probe", pullPages)
			}
			if skippedCollectionRequests != 0 {
				t.Fatalf("skipped collection requests = %d, want none when PR claims are incomplete", skippedCollectionRequests)
			}
			if got := client.PaginationCapHits(); got != 1 {
				t.Fatalf("PaginationCapHits = %d, want 1", got)
			}
		})
	}
}

func TestGitHubClientOpenPRClaimScanAllowsEmptyProbePage(t *testing.T) {
	pullPages := 0
	openIssueRequests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/api/pulls":
			pullPages++
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("Link", `<`+r.URL.String()+`>; rel="next"`)
				_ = json.NewEncoder(w).Encode([]githubPullRequestSummary{{Number: 7, State: "open", Title: "claim", Body: "Closes #43"}})
				return
			}
			_ = json.NewEncoder(w).Encode([]githubPullRequestSummary{})
		case "/repos/acme/api/issues":
			openIssueRequests++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]githubIssue{
				{ID: 43, Number: 43, State: "open"},
				{ID: 44, Number: 44, State: "open"},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewGitHubClient(workflow.TrackerConfig{APIKey: "token", PaginationMaxPages: 1}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()
	issues, err := client.ListIssuesByStates(context.Background(), []string{"open"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 1 || issues[0].Identifier != "#44" {
		t.Fatalf("issues = %#v, want unclaimed open issue after empty PR probe page", issues)
	}
	if pullPages != 2 {
		t.Fatalf("open pull request pages = %d, want max page plus empty probe", pullPages)
	}
	if openIssueRequests != 1 {
		t.Fatalf("open issue requests = %d, want open collection to proceed after complete PR claim scan", openIssueRequests)
	}
	if got := client.PaginationCapHits(); got != 0 {
		t.Fatalf("PaginationCapHits = %d, want 0", got)
	}
}

func TestGitHubClientListIssuesByStatesMapsClosedState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("state"); got != "closed" {
			t.Fatalf("state query = %q, want closed", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]githubIssue{{
			ID:        42,
			Number:    42,
			Title:     "closed issue",
			HTMLURL:   "https://github.com/acme/api/issues/42",
			State:     "closed",
			CreatedAt: "2026-05-20T01:02:03Z",
			UpdatedAt: "2026-05-20T02:03:04Z",
		}})
	}))
	defer srv.Close()
	client := NewGitHubClient(workflow.TrackerConfig{APIKey: "test-token"}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()

	issues, err := client.ListIssuesByStates(context.Background(), []string{"closed"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 1 || issues[0].State != "closed" {
		t.Fatalf("issues = %+v, want one closed issue", issues)
	}
}

func TestGitHubClientRequiresTokenOwnerAndRepo(t *testing.T) {
	client := NewGitHubClient(workflow.TrackerConfig{}, "https://api.github.test", "acme", "api")
	if _, err := client.ListIssuesByStates(context.Background(), []string{"priority:p2"}); err == nil || !strings.Contains(err.Error(), "GitHub tracker api_key") {
		t.Fatalf("missing token error = %v", err)
	}
	client = NewGitHubClient(workflow.TrackerConfig{APIKey: "token"}, "https://api.github.test", "", "api")
	if _, err := client.ListIssuesByStates(context.Background(), []string{"priority:p2"}); err == nil || !strings.Contains(err.Error(), "repo.owner and repo.name") {
		t.Fatalf("missing owner error = %v", err)
	}
}

func TestGitHubClientSatisfiesTrackerInterfaces(t *testing.T) {
	var _ IssueStateRefresher = (*GitHubClient)(nil)
}

func TestMapGitHubIssueLowercasesLabels(t *testing.T) {
	got, err := mapGitHubIssue(githubIssue{
		ID:        1,
		Number:    1,
		Title:     "case test",
		State:     "open",
		CreatedAt: "2026-05-20T01:02:03Z",
		UpdatedAt: "2026-05-20T01:02:03Z",
		Labels:    []githubLabel{{Name: "Important"}, {Name: " Priority:P1 "}, {Name: ""}},
	}, "open")
	if err != nil {
		t.Fatalf("mapGitHubIssue: %v", err)
	}
	want := []string{"important", "priority:p1"}
	if len(got.Labels) != len(want) {
		t.Fatalf("labels = %#v, want %#v", got.Labels, want)
	}
	for i := range want {
		if got.Labels[i] != want[i] {
			t.Fatalf("labels[%d] = %q, want %q", i, got.Labels[i], want[i])
		}
	}
}

func TestGitHubClientFetchIssueStatesByIDsUsesCachedIssueNumbers(t *testing.T) {
	var requestedPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		requestedPaths = append(requestedPaths, r.URL.Path)
		switch r.URL.Path {
		case "/repos/acme/api/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case "/repos/acme/api/issues":
			_ = json.NewEncoder(w).Encode([]githubIssue{
				{ID: 101, Number: 1, Title: "running", HTMLURL: "https://github.com/acme/api/issues/1", State: "open", CreatedAt: "2026-05-20T01:02:03Z", UpdatedAt: "2026-05-20T01:02:03Z", Labels: []githubLabel{{Name: "priority:p2"}}},
				{ID: 202, Number: 2, Title: "second", HTMLURL: "https://github.com/acme/api/issues/2", State: "open", CreatedAt: "2026-05-20T01:02:03Z", UpdatedAt: "2026-05-20T01:02:03Z", Labels: []githubLabel{{Name: "priority:p2"}}},
				{ID: 303, Number: 3, Title: "third", HTMLURL: "https://github.com/acme/api/issues/3", State: "open", CreatedAt: "2026-05-20T01:02:03Z", UpdatedAt: "2026-05-20T01:02:03Z", Labels: []githubLabel{{Name: "priority:p2"}}},
			})
		case "/repos/acme/api/issues/1":
			_ = json.NewEncoder(w).Encode(githubIssue{ID: 101, Number: 1, State: "closed", CreatedAt: "2026-05-20T01:02:03Z", UpdatedAt: "2026-05-20T02:03:04Z", Labels: []githubLabel{{Name: "Done"}, {Name: "Aiops-Ready"}}})
		case "/repos/acme/api/issues/2":
			http.NotFound(w, r)
		case "/repos/acme/api/issues/3":
			w.WriteHeader(http.StatusGone)
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewGitHubClient(workflow.TrackerConfig{
		APIKey:         "test-token",
		ActiveStates:   []string{"priority:p2"},
		TerminalStates: []string{"done"},
	}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()

	if _, err := client.ListActiveIssues(context.Background()); err != nil {
		t.Fatalf("ListActiveIssues: %v", err)
	}
	states, err := client.FetchIssueStatesByIDs(context.Background(), []string{"101", "202", "303", "999"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	if got, want := states, map[string]string{"101": "done"}; len(got) != len(want) || got["101"].State != want["101"] {
		t.Fatalf("states = %#v, want %#v", got, want)
	}
	if got, want := states["101"].Labels, []string{"done", "aiops-ready"}; !slices.Equal(got, want) {
		t.Fatalf("FetchIssueStatesByIDs(101).Labels = %v; want %v", got, want)
	}
	wantPathPrefix := "/repos/acme/api/issues/1,/repos/acme/api/issues/2,/repos/acme/api/issues/3"
	gotJoined := strings.Join(requestedPaths[2:], ",")
	if gotJoined != wantPathPrefix {
		t.Fatalf("requested paths after listing = %s, want %s", gotJoined, wantPathPrefix)
	}
}

func TestGitHubClientFetchIssueStatesByRefsUsesIdentifierFallbackWithoutCache(t *testing.T) {
	var requestedPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		requestedPaths = append(requestedPaths, r.URL.Path)
		switch r.URL.Path {
		case "/repos/acme/api/issues/5":
			_ = json.NewEncoder(w).Encode(githubIssue{
				ID:        555,
				Number:    5,
				State:     "open",
				CreatedAt: "2026-05-20T01:02:03Z",
				UpdatedAt: "2026-05-20T02:03:04Z",
				Labels:    []githubLabel{{Name: "Done"}},
			})
		case "/repos/acme/api/issues/7":
			_ = json.NewEncoder(w).Encode(githubIssue{
				ID:        987654,
				Number:    7,
				State:     "open",
				CreatedAt: "2026-05-20T01:02:03Z",
				UpdatedAt: "2026-05-20T02:03:04Z",
				Labels:    []githubLabel{{Name: "Done"}, {Name: "Aiops-Ready"}},
			})
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewGitHubClient(workflow.TrackerConfig{
		APIKey:         "test-token",
		TerminalStates: []string{"Done"},
	}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()

	states, err := client.FetchIssueStatesByRefs(context.Background(), []IssueRef{{ID: "987654", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs: %v", err)
	}
	if got, want := states, map[string]string{"987654": "done"}; len(got) != len(want) || got["987654"].State != want["987654"] {
		t.Fatalf("states = %#v, want %#v", got, want)
	}
	if got, want := states["987654"].Labels, []string{"done", "aiops-ready"}; !slices.Equal(got, want) {
		t.Fatalf("FetchIssueStatesByRefs(987654).Labels = %v; want %v", got, want)
	}
	if got, want := strings.Join(requestedPaths, ","), "/repos/acme/api/issues/7"; got != want {
		t.Fatalf("requested paths = %s, want %s", got, want)
	}

	states, err = client.FetchIssueStatesByRefs(context.Background(), []IssueRef{{ID: "other-global-id", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs wrong ID: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("states for mismatched issue id = %#v, want empty", states)
	}
	states, err = client.FetchIssueStatesByRefs(context.Background(), []IssueRef{{ID: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs #number ID: %v", err)
	}
	if got := states["#7"].State; got != "done" {
		t.Fatalf("#number fallback state = %q, want done", got)
	}
	states, err = client.FetchIssueStatesByRefs(context.Background(), []IssueRef{{ID: "#8", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs mismatched #number ID: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("states for mismatched #number ID = %#v, want empty", states)
	}
	states, err = client.FetchIssueStatesByRefs(context.Background(), []IssueRef{{ID: "7", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs numeric fallback ID: %v", err)
	}
	if got := states["7"].State; got != "done" {
		t.Fatalf("numeric fallback ID state = %q, want done", got)
	}
	states, err = client.FetchIssueStatesByRefs(context.Background(), []IssueRef{{ID: "5", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs mismatched numeric fallback ID: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("states for mismatched numeric fallback ID = %#v, want empty", states)
	}
	client.cacheIssueNumber("5", 5)
	states, err = client.FetchIssueStatesByRefs(context.Background(), []IssueRef{{ID: "5", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs cached mismatched numeric fallback ID: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("states for cached mismatched numeric fallback ID = %#v, want empty", states)
	}
	client.cacheIssueNumber("555", 5)
	states, err = client.FetchIssueStatesByRefs(context.Background(), []IssueRef{{ID: "555", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs cached global ID with stale identifier: %v", err)
	}
	if got := states["555"].State; got != "done" {
		t.Fatalf("cached global ID state = %q, want done", got)
	}
	states, err = client.FetchIssueStatesByIDs(context.Background(), []string{"987654"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs after fallback cache: %v", err)
	}
	if got := states["987654"].State; got != "done" {
		t.Fatalf("cached fallback state = %q, want done", got)
	}
}

func TestGitHubClientFetchIssueStatesByIDsRequiresToken(t *testing.T) {
	client := NewGitHubClient(workflow.TrackerConfig{}, "https://api.github.test", "acme", "api")
	if _, err := client.FetchIssueStatesByIDs(context.Background(), []string{"1"}); err == nil || !strings.Contains(err.Error(), "GitHub tracker api_key") {
		t.Fatalf("missing token error = %v", err)
	}
}

func TestGitHubClientFetchIssueStatesByIDsEmptyInputReturnsEmptyMap(t *testing.T) {
	client := NewGitHubClient(workflow.TrackerConfig{APIKey: "token"}, "https://api.github.test", "acme", "api")
	states, err := client.FetchIssueStatesByIDs(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs nil: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("states = %#v, want empty map", states)
	}
}

func TestGitHubClientFetchIssueStatesByRefsPopulatesBlockedByAndDropsDeletedFallback(t *testing.T) {
	var graphqlRequests int
	var deletedFallbackRequested bool
	var openFallbackRequested bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/api/issues/20":
			_ = json.NewEncoder(w).Encode(githubIssue{
				ID:        200,
				NodeID:    "I_200",
				Number:    20,
				Title:     "refresh blocker",
				Body:      "Blocked by #21\nDepends on #22",
				State:     "open",
				CreatedAt: "2026-05-20T01:02:03Z",
				UpdatedAt: "2026-05-20T02:03:04Z",
				Labels:    []githubLabel{{Name: "aiops:todo"}},
			})
		case "/repos/acme/api/issues/23":
			_ = json.NewEncoder(w).Encode(githubIssue{
				ID:        230,
				NodeID:    "I_230",
				Number:    23,
				Title:     "no blockers",
				Body:      "Depends on other/service#24",
				State:     "open",
				CreatedAt: "2026-05-20T01:02:03Z",
				UpdatedAt: "2026-05-20T02:03:04Z",
				Labels:    []githubLabel{{Name: "aiops:todo"}},
			})
		case "/graphql":
			graphqlRequests++
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"nodes": []any{
				map[string]any{
					"id":     "I_200",
					"number": float64(20),
					"blockedBy": map[string]any{
						"nodes":    []any{},
						"pageInfo": map[string]any{"hasNextPage": false},
					},
				},
				map[string]any{
					"id":     "I_230",
					"number": float64(23),
					"blockedBy": map[string]any{
						"nodes":    []any{},
						"pageInfo": map[string]any{"hasNextPage": false},
					},
				},
			}}})
		case "/repos/acme/api/issues/21":
			deletedFallbackRequested = true
			http.NotFound(w, r)
		case "/repos/acme/api/issues/22":
			openFallbackRequested = true
			_ = json.NewEncoder(w).Encode(githubIssue{
				ID:        220,
				NodeID:    "I_220",
				Number:    22,
				Title:     "open fallback",
				State:     "open",
				Labels:    []githubLabel{{Name: "aiops:todo"}},
				CreatedAt: "2026-05-20T01:02:03Z",
				UpdatedAt: "2026-05-20T02:03:04Z",
			})
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewGitHubClient(workflow.TrackerConfig{
		APIKey:         "test-token",
		ActiveStates:   []string{"aiops:todo"},
		TerminalStates: []string{"closed", "done"},
	}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()

	states, err := client.FetchIssueStatesByRefs(context.Background(), []IssueRef{
		{ID: "200", Identifier: "#20"},
		{ID: "230", Identifier: "#23"},
	})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs: %v", err)
	}

	if graphqlRequests != 1 {
		t.Fatalf("graphql requests = %d, want 1", graphqlRequests)
	}
	if !deletedFallbackRequested || !openFallbackRequested {
		t.Fatalf("fallback resolution requested deleted=%t open=%t, want both", deletedFallbackRequested, openFallbackRequested)
	}
	if got := states["200"].BlockedBy; len(got) != 1 || got[0].Identifier != "#22" || got[0].State != "aiops:todo" {
		t.Fatalf("FetchIssueStatesByRefs(#20).BlockedBy = %+v, want deleted #21 dropped and open #22 retained", got)
	}
	if got := states["230"].BlockedBy; got == nil || len(got) != 0 {
		t.Fatalf("FetchIssueStatesByRefs(#23).BlockedBy = %#v, want non-nil empty slice for confirmed no blockers", got)
	}
}

func TestGitHubClientFetchIssueStatesWithoutBlockersByRefsSkipsBlockerHydration(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/api/issues/20":
			_ = json.NewEncoder(w).Encode(githubIssue{
				ID:        200,
				NodeID:    "I_200",
				Number:    20,
				Title:     "runner refresh",
				Body:      "Blocked by #21",
				State:     "open",
				CreatedAt: "2026-05-20T01:02:03Z",
				UpdatedAt: "2026-05-20T02:03:04Z",
				Labels:    []githubLabel{{Name: "aiops:todo"}, {Name: "backend"}},
			})
		case "/graphql":
			t.Fatal("FetchIssueStatesWithoutBlockersByRefs must not hydrate GitHub blockers")
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewGitHubClient(workflow.TrackerConfig{
		APIKey:       "test-token",
		ActiveStates: []string{"aiops:todo"},
	}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()

	states, err := client.FetchIssueStatesWithoutBlockersByRefs(context.Background(), []IssueRef{{ID: "200", Identifier: "#20"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesWithoutBlockersByRefs: %v", err)
	}
	got, ok := states["200"]
	if !ok {
		t.Fatalf("states = %#v; want row 200", states)
	}
	if got.State != "aiops:todo" {
		t.Fatalf("states[200].State = %q; want aiops:todo", got.State)
	}
	if len(got.Labels) != 2 || got.Labels[0] != "aiops:todo" || got.Labels[1] != "backend" {
		t.Fatalf("states[200].Labels = %#v; want aiops:todo/backend", got.Labels)
	}
	if got.BlockedBy != nil {
		t.Fatalf("states[200].BlockedBy = %#v; want nil no-blocker knowledge for runner current-issue refresh", got.BlockedBy)
	}
}

func TestGitHubClientFetchIssueStatesByRefsSkipsBlockerLookupForNonTodoStates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/api/issues/20":
			_ = json.NewEncoder(w).Encode(githubIssue{
				ID:        200,
				NodeID:    "I_200",
				Number:    20,
				Title:     "done issue",
				Body:      "Depends on #21",
				HTMLURL:   "https://github.com/acme/api/issues/20",
				State:     "closed",
				CreatedAt: "2026-05-20T01:02:03Z",
				UpdatedAt: "2026-05-20T02:03:04Z",
				Labels:    []githubLabel{{Name: "done"}},
			})
		case "/graphql", "/repos/acme/api/issues/21":
			t.Fatalf("FetchIssueStatesByRefs(done) must not hydrate blockers via %s", r.URL.Path)
		default:
			t.Fatalf("unexpected request path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewGitHubClient(workflow.TrackerConfig{
		APIKey:         "test-token",
		TerminalStates: []string{"done"},
	}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()

	states, err := client.FetchIssueStatesByRefs(context.Background(), []IssueRef{{ID: "200", Identifier: "#20"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs: %v", err)
	}
	got, ok := states["200"]
	if !ok {
		t.Fatalf("FetchIssueStatesByRefs missing issue 200: %+v", states)
	}
	if got.State != "done" {
		t.Fatalf("FetchIssueStatesByRefs state = %q, want done", got.State)
	}
	if got.BlockedBy == nil || len(got.BlockedBy) != 0 {
		t.Fatalf("FetchIssueStatesByRefs BlockedBy = %#v; want non-nil empty blockers without lookup", got.BlockedBy)
	}
}

func TestGitHubClientFetchIssueStatesByIDsSurfacesNon404Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/api/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case "/repos/acme/api/issues":
			_ = json.NewEncoder(w).Encode([]githubIssue{
				{ID: 500, Number: 1, Title: "fails later", HTMLURL: "https://github.com/acme/api/issues/1", State: "open", CreatedAt: "2026-05-20T01:02:03Z", UpdatedAt: "2026-05-20T01:02:03Z", Labels: []githubLabel{{Name: "priority:p2"}}},
			})
		case "/repos/acme/api/issues/1":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewGitHubClient(workflow.TrackerConfig{APIKey: "test-token", ActiveStates: []string{"priority:p2"}}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()

	if _, err := client.ListActiveIssues(context.Background()); err != nil {
		t.Fatalf("ListActiveIssues: %v", err)
	}
	_, err := client.FetchIssueStatesByIDs(context.Background(), []string{"500"})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("FetchIssueStatesByIDs error = %v, want HTTP 500 surfaced", err)
	}
}

// TestGitHubClientListIssuesForStateKeepsClaimedNonOpenIssue characterizes the
// compound claimed-skip guard in listIssuesForState before the #521
// decomposition: an issue claimed by an open PR is only skipped while it is
// itself open. A claimed issue in a non-open state must still be collected
// (e.g. a "closed" issue surfaced by an "all" state query). Every other fixture
// in this suite uses state:"open", so dropping the `&& EqualFold(...,"open")`
// half of the guard would go undetected without this case.
func TestGitHubClientListIssuesForStateKeepsClaimedNonOpenIssue(t *testing.T) {
	// An open PR claims BOTH #77 and #78, so both enter claimedIssueNumbers.
	// The "all" state query returns #77 (closed) and #78 (open). The compound
	// guard must skip #78 (claimed AND open) but keep #77 (claimed but closed).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/api/pulls":
			_ = json.NewEncoder(w).Encode([]githubPullRequestSummary{{
				Number:  300,
				State:   "open",
				Title:   "Fix it",
				Body:    "Closes #77\nCloses #78",
				HTMLURL: "https://github.com/acme/api/pull/300",
			}})
		case "/repos/acme/api/issues":
			if got := r.URL.Query().Get("state"); got != "all" {
				t.Fatalf("issue state query = %q, want all", got)
			}
			_ = json.NewEncoder(w).Encode([]githubIssue{
				{
					ID:        77,
					Number:    77,
					Title:     "claimed but closed",
					HTMLURL:   "https://github.com/acme/api/issues/77",
					State:     "closed",
					CreatedAt: "2026-05-20T01:02:03Z",
					UpdatedAt: "2026-05-20T02:03:04Z",
				},
				{
					ID:        78,
					Number:    78,
					Title:     "claimed and open",
					HTMLURL:   "https://github.com/acme/api/issues/78",
					State:     "open",
					CreatedAt: "2026-05-20T01:02:03Z",
					UpdatedAt: "2026-05-20T02:03:04Z",
				},
			})
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewGitHubClient(workflow.TrackerConfig{APIKey: "test-token"}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()

	issues, err := client.ListIssuesByStates(context.Background(), []string{"all"})
	if err != nil {
		t.Fatalf("ListIssuesByStates(all) = %v; want nil error", err)
	}
	if len(issues) != 1 {
		t.Fatalf("ListIssuesByStates(all) returned %d issues (%+v); want 1 (claimed-but-closed #77 kept, claimed-and-open #78 skipped)", len(issues), issues)
	}
	if got := issues[0].Identifier; got != "#77" {
		t.Fatalf("ListIssuesByStates(all)[0].Identifier = %q; want %q (claimed non-open issue must be kept)", got, "#77")
	}
}

// TestGitHubClientListIssuesForStateSurfacesMapError characterizes that a
// mapGitHubIssue failure on the list path (here a malformed created_at) is
// surfaced as an error rather than silently dropped, and that the accumulated
// partial result is discarded.
func TestGitHubClientListIssuesForStateSurfacesMapError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/api/pulls":
			_ = json.NewEncoder(w).Encode([]githubPullRequestSummary{})
		case "/repos/acme/api/issues":
			_ = json.NewEncoder(w).Encode([]githubIssue{
				{
					ID:        90,
					Number:    90,
					Title:     "malformed timestamp",
					HTMLURL:   "https://github.com/acme/api/issues/90",
					State:     "open",
					CreatedAt: "not-a-timestamp",
					UpdatedAt: "2026-05-20T02:03:04Z",
				},
			})
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewGitHubClient(workflow.TrackerConfig{APIKey: "test-token"}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()

	issues, err := client.ListIssuesByStates(context.Background(), []string{"open"})
	if err == nil || !strings.Contains(err.Error(), "created_at") {
		t.Fatalf("ListIssuesByStates(open) err = %v; want mapGitHubIssue created_at parse error surfaced", err)
	}
	if issues != nil {
		t.Fatalf("ListIssuesByStates(open) issues = %#v; want nil when a mapping error aborts the listing", issues)
	}
}

// TestGitHubClientListIssuesForStateDeduplicatesAcrossStates characterizes that
// a single global mapped.ID appearing in two different state collections is
// collected exactly once. The dedup carries across states because each
// per-state collection seeds collectionSeen from the shared seen set.
func TestGitHubClientListIssuesForStateDeduplicatesAcrossStates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/api/pulls":
			_ = json.NewEncoder(w).Encode([]githubPullRequestSummary{})
		case "/repos/acme/api/issues":
			// ID 42 is returned for BOTH the open and closed state queries.
			_ = json.NewEncoder(w).Encode([]githubIssue{{
				ID:        42,
				Number:    42,
				Title:     "appears in two states",
				HTMLURL:   "https://github.com/acme/api/issues/42",
				State:     r.URL.Query().Get("state"),
				CreatedAt: "2026-05-20T01:02:03Z",
				UpdatedAt: "2026-05-20T02:03:04Z",
			}})
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewGitHubClient(workflow.TrackerConfig{APIKey: "test-token"}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()

	issues, err := client.ListIssuesByStates(context.Background(), []string{"open", "closed"})
	if err != nil {
		t.Fatalf("ListIssuesByStates(open,closed) = %v; want nil error", err)
	}
	if len(issues) != 1 {
		t.Fatalf("ListIssuesByStates(open,closed) returned %d issues (%+v); want 1 deduplicated by global ID", len(issues), issues)
	}
	if got := issues[0].ID; got != "42" {
		t.Fatalf("ListIssuesByStates(open,closed)[0].ID = %q; want %q", got, "42")
	}
}
