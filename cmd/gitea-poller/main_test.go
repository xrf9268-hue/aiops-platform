package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type fakeStore struct {
	tasks []task.Task
}

func (f *fakeStore) Enqueue(_ context.Context, in task.Task) (task.Task, bool, error) {
	in.ID = "tsk_1"
	f.tasks = append(f.tasks, in)
	return in, false, nil
}

func TestDefaultDatabaseURLIsUsablePostgresDSN(t *testing.T) {
	got := databaseURL()
	if got == "" || got == "postgres://aiops:***@localhost:5432/aiops?sslmode=disable" {
		t.Fatalf("databaseURL default = %q, want usable local Postgres DSN without placeholder credentials", got)
	}
}

func TestProcessIssuesEnqueuesGiteaIssueTasks(t *testing.T) {
	store := &fakeStore{}
	cfg := &workflow.Config{
		Repo:  workflow.RepoConfig{Owner: "owner", Name: "repo", CloneURL: "git@example.com:owner/repo.git", DefaultBranch: "main"},
		Agent: workflow.AgentConfig{Default: "mock"},
	}

	processIssues(context.Background(), store, cfg, []tracker.Issue{{
		ID:          "101",
		Identifier:  "#7",
		Title:       "ship feature",
		Description: "body",
		URL:         "https://gitea.local/owner/repo/issues/7",
		State:       "AI Ready",
	}})

	if len(store.tasks) != 1 {
		t.Fatalf("enqueued tasks = %d, want 1", len(store.tasks))
	}
	got := store.tasks[0]
	if got.SourceType != "gitea_issue" || got.SourceEventID != "101" || got.Actor != "gitea" {
		t.Fatalf("task source = (%q,%q,%q), want gitea issue source", got.SourceType, got.SourceEventID, got.Actor)
	}
	if got.Title != "#7 ship feature" {
		t.Fatalf("title = %q", got.Title)
	}
}

func TestSourceEventIDReworkUsesUpdatedAt(t *testing.T) {
	got := sourceEventID(tracker.Issue{ID: "101", State: "Rework", UpdatedAt: mustTime("2026-05-17T00:00:00Z")})
	if got != "101|rework|2026-05-17T00:00:00Z" {
		t.Fatalf("sourceEventID = %q", got)
	}
}

func TestGiteaMetricsHandlerExposesPaginationCapHits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			t.Fatalf("page query = %q: %v", r.URL.Query().Get("page"), err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Add("Link", fmt.Sprintf(`<%s%s?page=%d>; rel="next"`, serverURL(r), r.URL.Path, page+1))
		issues := make([]gitea.Issue, 50)
		for i := range issues {
			number := (page-1)*50 + i + 1
			issues[i] = gitea.Issue{ID: int64(number), Number: number, Title: "todo", HTMLURL: fmt.Sprintf("https://gitea.local/o/r/issues/%d", number), Labels: []gitea.Label{{Name: "aiops/todo"}}}
		}
		_ = json.NewEncoder(w).Encode(issues)
	}))
	defer server.Close()

	client := gitea.NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	client.Logf = func(string, ...any) {}
	if _, err := client.ListIssuesByStates(context.Background(), []string{"AI Ready"}); err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	writeMetrics(w, req, client)

	if w.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain", got)
	}
	body := w.Body.String()
	if !strings.Contains(body, "aiops_gitea_issue_pagination_cap_hits_total 1") {
		t.Fatalf("metrics body = %q, want pagination cap counter", body)
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
