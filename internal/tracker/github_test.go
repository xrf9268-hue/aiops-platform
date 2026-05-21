package tracker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

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

func TestGitHubClientListIssuesByStatesErrorsWhenIssuePaginationOverflows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/api/pulls":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[]`)
		case "/repos/acme/api/issues":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Link", `<`+r.URL.String()+`>; rel="next"`)
			_, _ = io.WriteString(w, `[]`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewGitHubClient(workflow.TrackerConfig{APIKey: "token"}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()
	_, err := client.ListIssuesByStates(context.Background(), []string{"open"})
	if err == nil || !strings.Contains(err.Error(), "github issue pagination exceeded") {
		t.Fatalf("ListIssuesByStates error = %v, want pagination overflow", err)
	}
	if got := client.PaginationCapHits(); got != 1 {
		t.Fatalf("PaginationCapHits = %d, want 1", got)
	}
}

func TestGitHubClientListIssuesByStatesErrorsWhenOpenPRPaginationOverflows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/api/pulls":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Link", `<`+r.URL.String()+`>; rel="next"`)
			_, _ = io.WriteString(w, `[]`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := NewGitHubClient(workflow.TrackerConfig{APIKey: "token"}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()
	_, err := client.ListIssuesByStates(context.Background(), []string{"open"})
	if err == nil || !strings.Contains(err.Error(), "github open pull request pagination exceeded") {
		t.Fatalf("ListIssuesByStates error = %v, want PR pagination overflow", err)
	}
	if got := client.PaginationCapHits(); got != 1 {
		t.Fatalf("PaginationCapHits = %d, want 1", got)
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
	var _ Client = (*GitHubClient)(nil)
	var _ StateIssueLister = (*GitHubClient)(nil)
}
