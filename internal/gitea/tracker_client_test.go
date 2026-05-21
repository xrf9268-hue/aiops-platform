package gitea

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestTrackerClientSatisfiesIssueStateRefresher(t *testing.T) {
	var _ tracker.IssueStateRefresher = (*TrackerClient)(nil)
}

func TestTrackerClientListIssuesByStatesReturnsNoIssuesWhenNoStatesRequested(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Issue{
			{ID: 100, Number: 100, Title: "todo", HTMLURL: "https://gitea.local/o/r/issues/100", Labels: []Label{{Name: "aiops/todo"}}},
		})
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	issues, err := client.ListIssuesByStates(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("issues = %#v, want no issues for an empty workflow state filter", issues)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want no unfiltered Gitea request for empty states", requests)
	}
}

func TestTrackerClientListIssuesByStatesMapsAIOpsLabels(t *testing.T) {
	var requestedLabels []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedLabels = append(requestedLabels, r.URL.Query().Get("labels"))
		if r.Header.Get("Authorization") != "token secret" {
			t.Fatalf("Authorization = %q, want token secret", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/api/v1/repos/owner/repo/issues" {
			t.Fatalf("requested path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("labels") == "aiops/todo" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]Issue{
				{ID: 101, Number: 1, Title: "first", Body: "body", HTMLURL: "https://gitea.local/o/r/issues/1", CreatedAt: "2026-05-16T23:59:00Z", UpdatedAt: "2026-05-17T00:00:00Z", Labels: []Label{{Name: "aiops/todo"}}},
			})
			return
		}
		if r.URL.Query().Get("labels") == "aiops/rework" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]Issue{
				{ID: 102, Number: 2, Title: "second", Body: "body", HTMLURL: "https://gitea.local/o/r/issues/2", CreatedAt: "2026-05-17T00:00:30Z", UpdatedAt: "2026-05-17T00:01:00Z", Labels: []Label{{Name: "aiops/rework"}}},
			})
			return
		}
		t.Fatalf("labels query = %q, want one active aiops label per request", r.URL.Query().Get("labels"))
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{
		APIKey:       "secret",
		ActiveStates: []string{"AI Ready", "Rework"},
	}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	issues, err := client.ListActiveIssues(context.Background())
	if err != nil {
		t.Fatalf("ListActiveIssues: %v", err)
	}
	if !slices.Equal(requestedLabels, []string{"aiops/todo", "aiops/rework"}) {
		t.Fatalf("labels queries = %#v, want one request per active aiops label", requestedLabels)
	}
	if len(issues) != 2 {
		t.Fatalf("issues len = %d, want 2", len(issues))
	}
	if issues[0].ID != "101" || issues[0].Identifier != "#1" || issues[0].State != "AI Ready" {
		t.Fatalf("first issue = %#v, want mapped AI Ready state", issues[0])
	}
	if issues[1].State != "Rework" {
		t.Fatalf("second issue state = %q, want Rework", issues[1].State)
	}
	if !issues[0].CreatedAt.Equal(mustTime("2026-05-16T23:59:00Z")) || !issues[1].CreatedAt.Equal(mustTime("2026-05-17T00:00:30Z")) {
		t.Fatalf("issue created_at = %s, %s; want Gitea created_at metadata mapped", issues[0].CreatedAt, issues[1].CreatedAt)
	}
}

func TestTrackerClientFetchIssueStatesByIDsUsesCachedIssueNumbers(t *testing.T) {
	var requestedPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPaths = append(requestedPaths, r.URL.Path)
		if r.Header.Get("Authorization") != "token secret" {
			t.Fatalf("Authorization = %q, want token secret", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues":
			if r.URL.Query().Get("labels") != "aiops/todo" {
				t.Fatalf("labels query = %q, want aiops/todo", r.URL.Query().Get("labels"))
			}
			_ = json.NewEncoder(w).Encode([]Issue{
				{ID: 101, Number: 1, Title: "running", HTMLURL: "https://gitea.local/o/r/issues/1", Labels: []Label{{Name: "aiops/todo"}}},
				{ID: 202, Number: 2, Title: "missing later", HTMLURL: "https://gitea.local/o/r/issues/2", Labels: []Label{{Name: "aiops/todo"}}},
			})
		case "/api/v1/repos/owner/repo/issues/1":
			_ = json.NewEncoder(w).Encode(Issue{
				ID: 101, Number: 1, Title: "done", HTMLURL: "https://gitea.local/o/r/issues/1", Labels: []Label{{Name: "aiops/done"}},
			})
		case "/api/v1/repos/owner/repo/issues/2":
			http.NotFound(w, r)
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{
		APIKey:       "secret",
		ActiveStates: []string{"AI Ready"},
	}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	if _, err := client.ListActiveIssues(context.Background()); err != nil {
		t.Fatalf("ListActiveIssues: %v", err)
	}
	states, err := client.FetchIssueStatesByIDs(context.Background(), []string{"101", "202"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	if got, want := states, map[string]string{"101": "Done"}; len(got) != len(want) || got["101"] != want["101"] {
		t.Fatalf("states = %#v, want %#v", got, want)
	}
	if got, want := strings.Join(requestedPaths, ","), "/api/v1/repos/owner/repo/issues,/api/v1/repos/owner/repo/issues/1,/api/v1/repos/owner/repo/issues/2"; got != want {
		t.Fatalf("requested paths = %s, want %s", got, want)
	}
}

func TestParseGiteaIssueTimeErrorsOnMalformedTimestamp(t *testing.T) {
	_, err := parseGiteaIssueTime("updated_at", "not-a-timestamp")
	if err == nil {
		t.Fatal("parseGiteaIssueTime malformed timestamp should error")
	}
	if !strings.Contains(err.Error(), "updated_at") || !strings.Contains(err.Error(), "not-a-timestamp") {
		t.Fatalf("error = %q, want field name and bad value", err.Error())
	}
}

func TestTrackerClientListIssuesByStatesDeduplicatesIssuesReturnedForMultipleLabels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Issue{
			{ID: 101, Number: 1, Title: "conflict", Body: "body", HTMLURL: "https://gitea.local/o/r/issues/1", UpdatedAt: "2026-05-17T00:00:00Z", Labels: []Label{{Name: "aiops/todo"}, {Name: "aiops/rework"}}},
		})
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{
		APIKey:       "secret",
		ActiveStates: []string{"AI Ready", "Rework"},
	}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	issues, err := client.ListActiveIssues(context.Background())
	if err != nil {
		t.Fatalf("ListActiveIssues: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues len = %d, want deduplicated issue", len(issues))
	}
	if issues[0].ID != "101" || issues[0].State != "Rework" {
		t.Fatalf("issue = %#v, want conflict resolved once", issues[0])
	}
}

func TestTrackerClientListIssuesByStatesFiltersTerminalAndMissingStates(t *testing.T) {
	var requestedState string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedState = r.URL.Query().Get("state")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Issue{
			{ID: 201, Number: 1, Title: "done", HTMLURL: "https://gitea.local/o/r/issues/1", Labels: []Label{{Name: "aiops/done"}}},
			{ID: 202, Number: 2, Title: "missing", HTMLURL: "https://gitea.local/o/r/issues/2", Labels: []Label{{Name: "bug"}}},
			{ID: 203, Number: 3, Title: "todo", HTMLURL: "https://gitea.local/o/r/issues/3", Labels: []Label{{Name: "aiops/todo"}}},
		})
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	issues, err := client.ListIssuesByStates(context.Background(), []string{"AI Ready"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 1 || issues[0].Identifier != "#3" || issues[0].State != "AI Ready" {
		t.Fatalf("issues = %#v, want only AI Ready issue", issues)
	}
	if requestedState != "open" {
		t.Fatalf("state query = %q, want open for active states", requestedState)
	}
}

func TestTrackerClientListIssuesByStatesQueriesAllForTerminalStates(t *testing.T) {
	var requestedState string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedState = r.URL.Query().Get("state")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Issue{
			{ID: 211, Number: 11, Title: "closed done", HTMLURL: "https://gitea.local/o/r/issues/11", Labels: []Label{{Name: "aiops/done"}}},
		})
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	issues, err := client.ListIssuesByStates(context.Background(), []string{"Done"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if requestedState != "all" {
		t.Fatalf("state query = %q, want all for terminal states", requestedState)
	}
	if len(issues) != 1 || issues[0].Identifier != "#11" || issues[0].State != "Done" {
		t.Fatalf("issues = %#v, want Done issue", issues)
	}
}

func TestTrackerClientListIssuesByStatesUsesConfiguredTerminalStatesForGiteaState(t *testing.T) {
	var requestedState string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedState = r.URL.Query().Get("state")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Issue{
			{ID: 212, Number: 12, Title: "custom closed", HTMLURL: "https://gitea.local/o/r/issues/12", Labels: []Label{{Name: "aiops/done"}}},
		})
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret", TerminalStates: []string{"Shipped"}}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	_, err := client.ListIssuesByStates(context.Background(), []string{"Shipped"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if requestedState != "all" {
		t.Fatalf("state query = %q, want all for configured terminal state", requestedState)
	}
}

func TestTrackerClientListIssuesByStatesUsesDeterministicConflictState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Issue{
			{ID: 301, Number: 1, Title: "conflict", HTMLURL: "https://gitea.local/o/r/issues/1", Labels: []Label{{Name: "aiops/done"}, {Name: "aiops/rework"}}},
		})
	}))
	defer server.Close()

	var diagnostics []string
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	client.Logf = func(format string, args ...any) {
		diagnostics = append(diagnostics, fmt.Sprintf(format, args...))
	}
	issues, err := client.ListIssuesByStates(context.Background(), []string{"Rework"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 1 || issues[0].State != "Rework" {
		t.Fatalf("issues = %#v, want conflict resolved to Rework", issues)
	}
	if len(diagnostics) == 0 || !strings.Contains(diagnostics[0], "gitea issue #1 label diagnostic") {
		t.Fatalf("diagnostics = %#v, want logged conflict diagnostic", diagnostics)
	}
}

func TestTrackerClientListIssuesByStatesAllowsExactlyFullMaxPages(t *testing.T) {
	var requestedPages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPages = append(requestedPages, r.URL.Query().Get("page"))
		w.Header().Set("Content-Type", "application/json")
		page := r.URL.Query().Get("page")
		if page == strconv.Itoa(listIssuesMaxPages+1) {
			_ = json.NewEncoder(w).Encode([]Issue{})
			return
		}
		issues := make([]Issue, listIssuesPageSize)
		for i := range issues {
			number := (len(requestedPages)-1)*listIssuesPageSize + i + 1
			issues[i] = Issue{ID: int64(number), Number: number, Title: "todo", HTMLURL: fmt.Sprintf("https://gitea.local/o/r/issues/%d", number), Labels: []Label{{Name: "aiops/todo"}}}
		}
		_ = json.NewEncoder(w).Encode(issues)
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	issues, err := client.ListIssuesByStates(context.Background(), []string{"AI Ready"})
	if err != nil {
		t.Fatalf("ListIssuesByStates returned error for exactly full max pages: %v", err)
	}
	if len(issues) != listIssuesMaxPages*listIssuesPageSize {
		t.Fatalf("issues len = %d, want %d", len(issues), listIssuesMaxPages*listIssuesPageSize)
	}
	if got, want := requestedPages[len(requestedPages)-1], strconv.Itoa(listIssuesMaxPages+1); got != want {
		t.Fatalf("last requested page = %s, want probe page %s", got, want)
	}
}

func TestTrackerClientListIssuesByStatesReturnsCappedResultsInsteadOfFailingWhenPageLimitExceeded(t *testing.T) {
	var logs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			t.Fatalf("page query = %q: %v", r.URL.Query().Get("page"), err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("Link", fmt.Sprintf(`<%s%s?page=%d>; rel="next"`, serverURL(r), r.URL.Path, page+1))
		issues := make([]Issue, listIssuesPageSize)
		for i := range issues {
			number := (page-1)*listIssuesPageSize + i + 1
			issues[i] = Issue{ID: int64(number), Number: number, Title: "todo", HTMLURL: fmt.Sprintf("https://gitea.local/o/r/issues/%d", number), Labels: []Label{{Name: "aiops/todo"}}}
		}
		_ = json.NewEncoder(w).Encode(issues)
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	client.Logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	if got := client.PaginationCapHits(); got != 0 {
		t.Fatalf("initial PaginationCapHits = %d, want 0", got)
	}
	issues, err := client.ListIssuesByStates(context.Background(), []string{"AI Ready"})
	if err != nil {
		t.Fatalf("ListIssuesByStates must return capped results instead of failing the poll cycle: %v", err)
	}
	if len(issues) != listIssuesMaxPages*listIssuesPageSize {
		t.Fatalf("issues len = %d, want capped %d issues", len(issues), listIssuesMaxPages*listIssuesPageSize)
	}
	if len(logs) == 0 || !strings.Contains(logs[len(logs)-1], "gitea issue pagination exceeded") {
		t.Fatalf("logs = %#v, want pagination cap diagnostic", logs)
	}
	if got := client.PaginationCapHits(); got != 1 {
		t.Fatalf("PaginationCapHits = %d, want 1", got)
	}
}

func TestTrackerClientListIssuesByStatesContinuesWhenServerCapsPageBelowRequestedLimit(t *testing.T) {
	var requestedPages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPages = append(requestedPages, r.URL.Query().Get("page"))
		w.Header().Set("Content-Type", "application/json")
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			t.Fatalf("page query = %q: %v", r.URL.Query().Get("page"), err)
		}
		if page < 2 {
			w.Header().Add("Link", fmt.Sprintf(`<%s%s?page=%d>; rel="next"`, serverURL(r), r.URL.Path, page+1))
		}
		issues := make([]Issue, 2)
		for i := range issues {
			number := (page-1)*2 + i + 1
			issues[i] = Issue{ID: int64(number), Number: number, Title: "todo", HTMLURL: fmt.Sprintf("https://gitea.local/o/r/issues/%d", number), Labels: []Label{{Name: "aiops/todo"}}}
		}
		_ = json.NewEncoder(w).Encode(issues)
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	issues, err := client.ListIssuesByStates(context.Background(), []string{"AI Ready"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 4 {
		t.Fatalf("issues len = %d, want all 4 issues across server-capped pages", len(issues))
	}
	if !slices.Equal(requestedPages, []string{"1", "2"}) {
		t.Fatalf("requested pages = %#v, want pagination to follow Link rel=next", requestedPages)
	}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}

func mustTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		panic(err)
	}
	return parsed
}
