package gitea

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestTrackerClientListIssuesByStatesMapsAIOpsLabels(t *testing.T) {
	var requestedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.String()
		if r.Header.Get("Authorization") != "token secret" {
			t.Fatalf("Authorization = %q, want token secret", r.Header.Get("Authorization"))
		}
		if !strings.Contains(r.URL.Query().Get("labels"), "aiops/todo") || !strings.Contains(r.URL.Query().Get("labels"), "aiops/rework") {
			t.Fatalf("labels query = %q, want active aiops labels", r.URL.Query().Get("labels"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Issue{
			{ID: 101, Number: 1, Title: "first", Body: "body", HTMLURL: "https://gitea.local/o/r/issues/1", UpdatedAt: "2026-05-17T00:00:00Z", Labels: []Label{{Name: "aiops/todo"}}},
			{ID: 102, Number: 2, Title: "second", Body: "body", HTMLURL: "https://gitea.local/o/r/issues/2", UpdatedAt: "2026-05-17T00:01:00Z", Labels: []Label{{Name: "aiops/rework"}}},
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
	if !strings.HasPrefix(requestedPath, "/api/v1/repos/owner/repo/issues?") {
		t.Fatalf("requested path = %q", requestedPath)
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
}

func TestTrackerClientListIssuesByStatesFiltersTerminalAndMissingStates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
