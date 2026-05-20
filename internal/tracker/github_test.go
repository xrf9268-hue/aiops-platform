package tracker

import (
	"context"
	"encoding/json"
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
		requestedPath = r.URL.Path
		requestedQuery = r.URL.RawQuery
		authHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
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
