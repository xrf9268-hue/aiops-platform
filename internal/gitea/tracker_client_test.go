package gitea

import (
	"context"
	"encoding/json"
	"errors"
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
		ActiveStates: []string{"Todo", "Rework"},
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
	if issues[0].ID != "101" || issues[0].Identifier != "#1" || issues[0].State != "Todo" {
		t.Fatalf("first issue = %#v, want mapped Todo state", issues[0])
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
				ID: 101, Number: 1, Title: "done", HTMLURL: "https://gitea.local/o/r/issues/1", Labels: []Label{{Name: "aiops/done"}, {Name: "Aiops-Ready"}},
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
		ActiveStates: []string{"Todo"},
	}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	if _, err := client.ListActiveIssues(context.Background()); err != nil {
		t.Fatalf("ListActiveIssues: %v", err)
	}
	states, err := client.FetchIssueStatesByIDs(context.Background(), []string{"101", "202"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	if got, want := states, map[string]string{"101": "Done"}; len(got) != len(want) || got["101"].State != want["101"] {
		t.Fatalf("states = %#v, want %#v", got, want)
	}
	if got, want := states["101"].Labels, []string{"aiops/done", "aiops-ready"}; !slices.Equal(got, want) {
		t.Fatalf("FetchIssueStatesByIDs(101).Labels = %v; want %v", got, want)
	}
	if got, want := strings.Join(requestedPaths, ","), "/api/v1/repos/owner/repo/issues,/api/v1/repos/owner/repo/issues/1,/api/v1/repos/owner/repo/issues/2"; got != want {
		t.Fatalf("requested paths = %s, want %s", got, want)
	}
}

func TestTrackerClientFetchIssueStatesByRefsUsesIdentifierFallbackWithoutCache(t *testing.T) {
	var requestedPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPaths = append(requestedPaths, r.URL.Path)
		if r.Header.Get("Authorization") != "token secret" {
			t.Fatalf("Authorization = %q, want token secret", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues/5":
			_ = json.NewEncoder(w).Encode(Issue{
				ID:      555,
				Number:  5,
				Title:   "done",
				HTMLURL: "https://gitea.local/o/r/issues/5",
				Labels:  []Label{{Name: "aiops/done"}},
			})
		case "/api/v1/repos/owner/repo/issues/7":
			_ = json.NewEncoder(w).Encode(Issue{
				ID:      987654,
				Number:  7,
				Title:   "done",
				HTMLURL: "https://gitea.local/o/r/issues/7",
				Labels:  []Label{{Name: "aiops/done"}, {Name: "Aiops-Ready"}},
			})
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	states, err := client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "987654", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs: %v", err)
	}
	if got, want := states, map[string]string{"987654": "Done"}; len(got) != len(want) || got["987654"].State != want["987654"] {
		t.Fatalf("states = %#v, want %#v", got, want)
	}
	if got, want := states["987654"].Labels, []string{"aiops/done", "aiops-ready"}; !slices.Equal(got, want) {
		t.Fatalf("FetchIssueStatesByRefs(987654).Labels = %v; want %v", got, want)
	}
	if got, want := strings.Join(requestedPaths, ","), "/api/v1/repos/owner/repo/issues/7"; got != want {
		t.Fatalf("requested paths = %s, want %s", got, want)
	}
	states, err = client.FetchIssueStatesByIDs(context.Background(), []string{"987654"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs after successful ref refresh: %v", err)
	}
	if got := states["987654"].State; got != "Done" {
		t.Fatalf("successful ref refresh cache state = %q, want Done", got)
	}

	states, err = client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "other-global-id", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs wrong ID: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("states for mismatched issue id = %#v, want empty", states)
	}
	states, err = client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs #number ID: %v", err)
	}
	if got := states["#7"].State; got != "Done" {
		t.Fatalf("#number fallback state = %q, want Done", got)
	}
	states, err = client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "#8", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs mismatched #number ID: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("states for mismatched #number ID = %#v, want empty", states)
	}
	states, err = client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "7", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs numeric fallback ID: %v", err)
	}
	if got := states["7"].State; got != "Done" {
		t.Fatalf("numeric fallback ID state = %q, want Done", got)
	}
	states, err = client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "5", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs mismatched numeric fallback ID: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("states for mismatched numeric fallback ID = %#v, want empty", states)
	}
	client.cacheIssueNumber(Issue{ID: 5, Number: 5})
	states, err = client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "5", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs cached mismatched numeric fallback ID: %v", err)
	}
	if len(states) != 0 {
		t.Fatalf("states for cached mismatched numeric fallback ID = %#v, want empty", states)
	}
	client.cacheIssueNumber(Issue{ID: 555, Number: 5})
	states, err = client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "555", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs cached global ID with stale identifier: %v", err)
	}
	if got := states["555"].State; got != "Done" {
		t.Fatalf("cached global ID state = %q, want Done", got)
	}
	states, err = client.FetchIssueStatesByIDs(context.Background(), []string{"987654"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs after fallback cache: %v", err)
	}
	if got := states["987654"].State; got != "Done" {
		t.Fatalf("cached fallback state = %q, want Done", got)
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
		ActiveStates: []string{"Todo", "Rework"},
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
	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 1 || issues[0].Identifier != "#3" || issues[0].State != "Todo" {
		t.Fatalf("issues = %#v, want only Todo issue", issues)
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
	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
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

// TestTrackerClientListIssuesByStatesErrorsWhenLabelOverflows pins the
// fail-safe contract for #402: if any state-label collection overflows
// pagination, ListIssuesByStates must surface tracker.ErrIssueListingCapped
// (with nil issues) so startup reconciliation does not treat the partial
// result as authoritative and delete workspaces for active issues that fell
// past the page cap.
func TestTrackerClientListIssuesByStatesErrorsWhenLabelOverflows(t *testing.T) {
	var logs []string
	todoPages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			t.Fatalf("page query = %q: %v", r.URL.Query().Get("page"), err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("labels") {
		case "aiops/todo":
			todoPages++
			w.Header().Add("Link", fmt.Sprintf(`<%s%s?page=%d>; rel="next"`, serverURL(r), r.URL.Path, page+1))
			_ = json.NewEncoder(w).Encode([]Issue{{ID: 9001, Number: 9001, Labels: []Label{{Name: "aiops/todo"}}}})
		case "aiops/rework":
			_ = json.NewEncoder(w).Encode([]Issue{{ID: 9001, Number: 9001, Labels: []Label{{Name: "aiops/rework"}}}})
		default:
			t.Fatalf("unexpected labels query %q", r.URL.Query().Get("labels"))
		}
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret", PaginationMaxPages: 1}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	client.Logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	if got := client.PaginationCapHits(); got != 0 {
		t.Fatalf("initial PaginationCapHits = %d, want 0", got)
	}
	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo", "Rework"})
	if !errors.Is(err, tracker.ErrIssueListingCapped) {
		t.Fatalf("ListIssuesByStates err = %v, want tracker.ErrIssueListingCapped", err)
	}
	if issues != nil {
		t.Fatalf("issues = %#v, want nil when listing is partial", issues)
	}
	if todoPages != 2 {
		t.Fatalf("aiops/todo pages = %d, want configured max page plus probe", todoPages)
	}
	if len(logs) == 0 || !strings.Contains(logs[len(logs)-1], "failing this tracker listing") {
		t.Fatalf("logs = %#v, want fail-loud diagnostic", logs)
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
	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
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

// TestTrackerClientListIssuesByStatesNormalizesLabelsAndBlockedBy pins the
// SPEC §4.1.1 / §11.3 Issue normalization for the Gitea tracker: labels are
// lowercased and `Depends on #N` body references become BlockerRef entries
// populated by a follow-up issue lookup (so the §8.2 Todo blocker rule has the
// State it needs).
func TestTrackerClientListIssuesByStatesNormalizesLabelsAndBlockedBy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]Issue{{
				ID:        101,
				Number:    1,
				Title:     "first",
				Body:      "Needs the blocker resolved.\n\nDepends on #42\nAlso Depends on #43 and depends on #42 again.",
				HTMLURL:   "https://gitea.local/o/r/issues/1",
				CreatedAt: "2026-05-16T23:59:00Z",
				UpdatedAt: "2026-05-17T00:00:00Z",
				Labels: []Label{
					{Name: "Aiops/Todo"},
					{Name: "Priority/P1"},
					{Name: "  "},
					{Name: "Type/Bug"},
				},
			}})
		case "/api/v1/repos/owner/repo/issues/42":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Issue{
				ID:     142,
				Number: 42,
				Title:  "blocker still open",
				Labels: []Label{{Name: "aiops/in-progress"}},
			})
		case "/api/v1/repos/owner/repo/issues/43":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Issue{
				ID:     143,
				Number: 43,
				Title:  "blocker done",
				Labels: []Label{{Name: "aiops/done"}},
			})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{
		APIKey:       "secret",
		ActiveStates: []string{"Todo"},
	}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues len = %d, want 1", len(issues))
	}
	got := issues[0]
	wantLabels := []string{"aiops/todo", "priority/p1", "type/bug"}
	if !slices.Equal(got.Labels, wantLabels) {
		t.Fatalf("Labels = %#v, want %#v (lowercased, empty entries dropped)", got.Labels, wantLabels)
	}
	if len(got.BlockedBy) != 2 {
		t.Fatalf("BlockedBy len = %d, want 2 (deduped #42 + #43); got=%#v", len(got.BlockedBy), got.BlockedBy)
	}
	wantBlockers := map[string]string{"#42": "In Progress", "#43": "Done"}
	for _, b := range got.BlockedBy {
		want, ok := wantBlockers[b.Identifier]
		if !ok {
			t.Fatalf("unexpected blocker %#v", b)
		}
		if b.State != want {
			t.Fatalf("blocker %s state = %q, want %q", b.Identifier, b.State, want)
		}
		if b.ID == "" {
			t.Fatalf("blocker %s ID empty; want canonical Gitea ID", b.Identifier)
		}
	}
	if got.Priority != 0 {
		t.Fatalf("Priority = %d, want 0 (Gitea has no native priority; left at zero per dependsOnRegexp comment)", got.Priority)
	}
}

// TestTrackerClientListIssuesByStatesTolerantOfBlockerLookupFailure: a missing
// blocker (404 or transient failure) silently drops out of BlockedBy rather
// than aborting candidate enumeration. Best-effort semantics per #210.
func TestTrackerClientListIssuesByStatesTolerantOfBlockerLookupFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]Issue{{
				ID:        101,
				Number:    1,
				Title:     "first",
				Body:      "Depends on #999 (deleted)\nDepends on #43",
				HTMLURL:   "https://gitea.local/o/r/issues/1",
				CreatedAt: "2026-05-16T23:59:00Z",
				UpdatedAt: "2026-05-17T00:00:00Z",
				Labels:    []Label{{Name: "aiops/todo"}},
			}})
		case "/api/v1/repos/owner/repo/issues/999":
			http.NotFound(w, r)
		case "/api/v1/repos/owner/repo/issues/43":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Issue{
				ID:     143,
				Number: 43,
				Title:  "still exists",
				Labels: []Label{{Name: "aiops/done"}},
			})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{
		APIKey:       "secret",
		ActiveStates: []string{"Todo"},
	}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues len = %d, want 1", len(issues))
	}
	if len(issues[0].BlockedBy) != 1 || issues[0].BlockedBy[0].Identifier != "#43" {
		t.Fatalf("BlockedBy = %#v, want only #43 (missing #999 silently dropped)", issues[0].BlockedBy)
	}
}

// TestTrackerClientListIssuesByStatesSurfacesMalformedTimestamp pins coverage
// gap (a) for #521: a malformed created_at/updated_at routed THROUGH the
// listing (not just parseGiteaIssueTime in isolation) must surface the parse
// error from listIssuesByStateLabel. created_at is parsed before updated_at,
// so a malformed created_at wins; the error names the field and bad value.
func TestTrackerClientListIssuesByStatesSurfacesMalformedTimestamp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Issue{
			{ID: 401, Number: 1, Title: "todo", HTMLURL: "https://gitea.local/o/r/issues/1", CreatedAt: "not-a-timestamp", UpdatedAt: "2026-05-17T00:00:00Z", Labels: []Label{{Name: "aiops/todo"}}},
		})
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err == nil {
		t.Fatalf("ListIssuesByStates(malformed created_at) = %#v, nil; want parse error", issues)
	}
	if !strings.Contains(err.Error(), "created_at") || !strings.Contains(err.Error(), "not-a-timestamp") {
		t.Fatalf("ListIssuesByStates err = %q; want created_at field name and bad value", err.Error())
	}
	if issues != nil {
		t.Fatalf("ListIssuesByStates(malformed created_at) issues = %#v; want nil on parse error", issues)
	}
}

// TestTrackerClientListIssuesByStatesDeduplicatesWithinSingleLabelScope pins
// coverage gap (b) for #521: when the SAME issueKey appears twice across the
// pages of ONE label scope, the collectionSeen dedup (marked on the include
// path) must keep it once. Existing coverage only exercises cross-label dedup
// seeded from seenIssues; this exercises intra-scope dedup across pages.
func TestTrackerClientListIssuesByStatesDeduplicatesWithinSingleLabelScope(t *testing.T) {
	var requestedPages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPages = append(requestedPages, r.URL.Query().Get("page"))
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			t.Fatalf("page query = %q: %v", r.URL.Query().Get("page"), err)
		}
		w.Header().Set("Content-Type", "application/json")
		if page < 2 {
			w.Header().Add("Link", fmt.Sprintf(`<%s%s?page=%d>; rel="next"`, serverURL(r), r.URL.Path, page+1))
		}
		// Same issue (ID 501) returned on both page 1 and page 2.
		_ = json.NewEncoder(w).Encode([]Issue{
			{ID: 501, Number: 1, Title: "todo", HTMLURL: "https://gitea.local/o/r/issues/1", CreatedAt: "2026-05-16T23:59:00Z", UpdatedAt: "2026-05-17T00:00:00Z", Labels: []Label{{Name: "aiops/todo"}}},
		})
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if !slices.Equal(requestedPages, []string{"1", "2"}) {
		t.Fatalf("requested pages = %#v; want pages 1 and 2 (same issue repeated)", requestedPages)
	}
	if len(issues) != 1 || issues[0].ID != "501" {
		t.Fatalf("ListIssuesByStates(duplicate across pages) = %#v; want issue 501 once", issues)
	}
}

// TestTrackerClientListIssuesByStateLabelSkipsEmptyStateWithoutWantedStatesFilter
// pins coverage gap (c) for #521: an issue whose labels yield an empty state
// must be dropped by the state=="" guard INDEPENDENTLY of the wantedStates
// filter. It drives listIssuesByStateLabel directly with an empty wantedStates
// set, so the wantedStates filter is a no-op (len==0) and cannot drop anything;
// the unlabelled issue can only be removed by the state=="" guard, while the
// labelled issue (which would survive the wantedStates filter regardless) is
// kept.
func TestTrackerClientListIssuesByStateLabelSkipsEmptyStateWithoutWantedStatesFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Issue{
			{ID: 601, Number: 1, Title: "no aiops label", HTMLURL: "https://gitea.local/o/r/issues/1", CreatedAt: "2026-05-16T23:59:00Z", UpdatedAt: "2026-05-17T00:00:00Z", Labels: []Label{{Name: "bug"}}},
			{ID: 602, Number: 2, Title: "todo", HTMLURL: "https://gitea.local/o/r/issues/2", CreatedAt: "2026-05-16T23:59:00Z", UpdatedAt: "2026-05-17T00:00:00Z", Labels: []Label{{Name: "aiops/todo"}}},
		})
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	issues, capped, err := client.listIssuesByStateLabel(context.Background(), "aiops/todo", "open", map[string]struct{}{}, map[string]struct{}{})
	if err != nil {
		t.Fatalf("listIssuesByStateLabel: %v", err)
	}
	if capped {
		t.Fatalf("listIssuesByStateLabel capped = true; want false")
	}
	if len(issues) != 1 || issues[0].ID != "602" || issues[0].State != "Todo" {
		t.Fatalf("listIssuesByStateLabel(empty wantedStates) = %#v; want only issue 602 (unlabelled issue 601 dropped by state==\"\" guard, not wantedStates)", issues)
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

// newSharedBlockerTrackerClient builds a Gitea mock whose issue-list endpoint
// returns the given source issues and whose per-issue endpoint serves a single
// shared blocker (#3, "Todo"), counting how many times the blocker is
// fetched. It is the harness for the #677 per-poll-tick blocker-cache tests.
func newSharedBlockerTrackerClient(t *testing.T, sources []Issue, blockerFetches *int) *TrackerClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues":
			_ = json.NewEncoder(w).Encode(sources)
		case "/api/v1/repos/owner/repo/issues/3":
			*blockerFetches++
			_ = json.NewEncoder(w).Encode(Issue{ID: 103, Number: 3, Title: "blocker", HTMLURL: "https://gitea.local/o/r/issues/3", Labels: []Label{{Name: "aiops/todo"}}})
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret", ActiveStates: []string{"Todo"}}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	return client
}

func TestTrackerClientListIssuesByStatesFetchesSharedBlockerOncePerTick(t *testing.T) {
	var blockerFetches int
	client := newSharedBlockerTrackerClient(t, []Issue{
		{ID: 101, Number: 1, Title: "a", Body: "Depends on #3", HTMLURL: "https://gitea.local/o/r/issues/1", Labels: []Label{{Name: "aiops/todo"}}},
		{ID: 102, Number: 2, Title: "b", Body: "Depends on #3", HTMLURL: "https://gitea.local/o/r/issues/2", Labels: []Label{{Name: "aiops/todo"}}},
	}, &blockerFetches)

	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("ListIssuesByStates returned %d issues; want 2", len(issues))
	}
	if blockerFetches != 1 {
		t.Fatalf("shared blocker #3 fetched %d times; want 1 (cached across both source issues in one tick)", blockerFetches)
	}
	for _, iss := range issues {
		if len(iss.BlockedBy) != 1 || iss.BlockedBy[0].Identifier != "#3" || iss.BlockedBy[0].State != "Todo" {
			t.Fatalf("issue %s BlockedBy = %#v; want one blocker #3 in state Todo", iss.Identifier, iss.BlockedBy)
		}
	}
}

func TestTrackerClientListIssuesByStatesRereadsBlockerOnNextTick(t *testing.T) {
	var blockerFetches int
	blockerLabel := "aiops/todo" // "Todo" on tick 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues":
			_ = json.NewEncoder(w).Encode([]Issue{
				{ID: 101, Number: 1, Title: "a", Body: "Depends on #3", HTMLURL: "https://gitea.local/o/r/issues/1", Labels: []Label{{Name: "aiops/todo"}}},
			})
		case "/api/v1/repos/owner/repo/issues/3":
			blockerFetches++
			_ = json.NewEncoder(w).Encode(Issue{ID: 103, Number: 3, Title: "blocker", HTMLURL: "https://gitea.local/o/r/issues/3", Labels: []Label{{Name: blockerLabel}}})
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer server.Close()
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret", ActiveStates: []string{"Todo"}}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	tick1, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates tick 1: %v", err)
	}
	blockerLabel = "aiops/done" // blocker transitions to "Done" between ticks
	tick2, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates tick 2: %v", err)
	}

	if blockerFetches != 2 {
		t.Fatalf("blocker #3 fetched %d times across two ticks; want 2 (once per tick, no cross-tick cache leak)", blockerFetches)
	}
	if len(tick1) != 1 || len(tick1[0].BlockedBy) != 1 || tick1[0].BlockedBy[0].State != "Todo" {
		t.Fatalf("tick 1 BlockedBy = %#v; want blocker #3 in state Todo", tick1)
	}
	if len(tick2) != 1 || len(tick2[0].BlockedBy) != 1 || tick2[0].BlockedBy[0].State != "Done" {
		t.Fatalf("tick 2 BlockedBy = %#v; want blocker #3 re-read in state Done", tick2)
	}
}

// TestCachedIssueByNumberNilCacheFallsBackToDirectFetch pins the defensive
// fallback for callers that reach buildBlockedBy without ListIssuesByStates
// installing a cache: with a nil cache, each lookup is a direct getIssueByNumber
// (no memoization, no nil-map write panic) (#677).
func TestCachedIssueByNumberNilCacheFallsBackToDirectFetch(t *testing.T) {
	var blockerFetches int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v1/repos/owner/repo/issues/3" {
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
		blockerFetches++
		_ = json.NewEncoder(w).Encode(Issue{ID: 103, Number: 3, Title: "blocker", HTMLURL: "https://gitea.local/o/r/issues/3", Labels: []Label{{Name: "aiops/todo"}}})
	}))
	defer server.Close()
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	issue, found := client.cachedIssueByNumber(context.Background(), nil, 3)
	if !found || issue.Number != 3 {
		t.Fatalf("cachedIssueByNumber(nil cache, 3) = (%#v, %v); want issue #3, true", issue, found)
	}
	if _, found = client.cachedIssueByNumber(context.Background(), nil, 3); !found {
		t.Fatalf("cachedIssueByNumber(nil cache, 3) second call found = false; want true")
	}
	if blockerFetches != 2 {
		t.Fatalf("blocker #3 fetched %d times with a nil cache; want 2 (no memoization without a cache)", blockerFetches)
	}
}

func TestTrackerClientListIssuesByStatesRetriesBlockerAfterTransientError(t *testing.T) {
	var blockerFetches int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues":
			_ = json.NewEncoder(w).Encode([]Issue{
				{ID: 101, Number: 1, Title: "a", Body: "Depends on #3", HTMLURL: "https://gitea.local/o/r/issues/1", Labels: []Label{{Name: "aiops/todo"}}},
				{ID: 102, Number: 2, Title: "b", Body: "Depends on #3", HTMLURL: "https://gitea.local/o/r/issues/2", Labels: []Label{{Name: "aiops/todo"}}},
			})
		case "/api/v1/repos/owner/repo/issues/3":
			blockerFetches++
			if blockerFetches == 1 {
				// Transient failure on the first source issue's fetch: must NOT be
				// cached, so the second source issue still retries (#677).
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(Issue{ID: 103, Number: 3, Title: "blocker", HTMLURL: "https://gitea.local/o/r/issues/3", Labels: []Label{{Name: "aiops/todo"}}})
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer server.Close()
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret", ActiveStates: []string{"Todo"}}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v; a best-effort blocker error must not abort listing", err)
	}
	if blockerFetches != 2 {
		t.Fatalf("blocker #3 fetched %d times; want 2 (transient error not cached, retried for the second source issue)", blockerFetches)
	}
	withBlocker := 0
	for _, iss := range issues {
		if len(iss.BlockedBy) == 1 && iss.BlockedBy[0].Identifier == "#3" {
			withBlocker++
		}
	}
	if withBlocker != 1 {
		t.Fatalf("issues carrying blocker #3 = %d; want 1 (first source errored+skipped, second retried+found)", withBlocker)
	}
}

// TestIssueNumberFromRef pins the "#N"-only contract (#748): a bare numeric ID
// is the Gitea-internal int64 id, not the issue number, and must not parse.
func TestIssueNumberFromRef(t *testing.T) {
	cases := []struct {
		id, identifier string
		want           int
		ok             bool
	}{
		{id: "8842", identifier: "#12", want: 12, ok: true},
		{id: "#12", identifier: "", want: 12, ok: true},
		{id: "8842", identifier: "", want: 0, ok: false},
		{id: "", identifier: "", want: 0, ok: false},
		{id: "#0", identifier: "", want: 0, ok: false},
		{id: "#x", identifier: "", want: 0, ok: false},
		{id: "8842", identifier: " #12 ", want: 12, ok: true},
	}
	for _, tc := range cases {
		got, ok := IssueNumberFromRef(tc.id, tc.identifier)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("IssueNumberFromRef(%q, %q) = (%d, %t); want (%d, %t)", tc.id, tc.identifier, got, ok, tc.want, tc.ok)
		}
	}
}
