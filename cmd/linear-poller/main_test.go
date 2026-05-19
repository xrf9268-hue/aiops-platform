package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// fakeStore mimics queue.Store.Enqueue with the same dedupe semantics as the
// Postgres ON CONFLICT (source_type, source_event_id) DO UPDATE clause: the
// first call for a given key INSERTs and returns deduped=false, repeats
// return the original row with deduped=true.
type fakeStore struct {
	calls    []task.Task
	bySource map[string]task.Task
	nextID   int
}

func newFakeStore() *fakeStore {
	return &fakeStore{bySource: map[string]task.Task{}}
}

func (f *fakeStore) Enqueue(_ context.Context, t task.Task) (task.Task, bool, error) {
	f.calls = append(f.calls, t)
	key := t.SourceType + "|" + t.SourceEventID
	if existing, ok := f.bySource[key]; ok {
		return existing, true, nil
	}
	f.nextID++
	t.ID = fmt.Sprintf("tsk_%d", f.nextID)
	t.Status = task.StatusQueued
	f.bySource[key] = t
	return t, false, nil
}

func (f *fakeStore) inserts() int {
	return len(f.bySource)
}

func baseConfig() *workflow.Config {
	cfg := workflow.DefaultConfig()
	cfg.Tracker.Kind = "linear"
	cfg.Repo.Owner = "octo"
	cfg.Repo.Name = "demo"
	cfg.Repo.CloneURL = "git@example.com:octo/demo.git"
	cfg.Repo.DefaultBranch = "main"
	cfg.Agent.Default = "mock"
	return &cfg
}

func TestSourceEventIDNonReworkUsesIssueID(t *testing.T) {
	for _, state := range []string{"AI Ready", "In Progress"} {
		issue := tracker.Issue{ID: "abc-123", State: state, UpdatedAt: "2026-05-08T10:00:00Z"}
		got := sourceEventID(issue)
		if got != "abc-123" {
			t.Fatalf("state=%q sourceEventID = %q, want %q", state, got, "abc-123")
		}
	}
}

func TestSourceEventIDReworkComposesUpdatedAt(t *testing.T) {
	issue := tracker.Issue{ID: "abc-123", State: "Rework", UpdatedAt: "2026-05-08T10:00:00Z"}
	got := sourceEventID(issue)
	want := "abc-123|rework|2026-05-08T10:00:00Z"
	if got != want {
		t.Fatalf("sourceEventID = %q, want %q", got, want)
	}
}

func TestProcessIssuesDedupesSameStateRepoll(t *testing.T) {
	store := newFakeStore()
	cfg := baseConfig()
	issue := tracker.Issue{
		ID: "abc-123", Identifier: "ENG-1", Title: "do thing",
		State: "AI Ready", UpdatedAt: "2026-05-08T10:00:00Z",
	}

	// First poll inserts.
	processIssues(context.Background(), store, cfg, []tracker.Issue{issue})
	if store.inserts() != 1 {
		t.Fatalf("inserts after first poll = %d, want 1", store.inserts())
	}

	// Second poll of same AI Ready issue dedupes — no new task row.
	processIssues(context.Background(), store, cfg, []tracker.Issue{issue})
	if store.inserts() != 1 {
		t.Fatalf("inserts after repeat poll = %d, want 1 (deduped)", store.inserts())
	}
	if len(store.calls) != 2 {
		t.Fatalf("Enqueue calls = %d, want 2", len(store.calls))
	}
}

func TestProcessIssuesReEnqueuesOnReworkTransition(t *testing.T) {
	store := newFakeStore()
	cfg := baseConfig()

	// Issue starts in AI Ready, gets enqueued, then moves to Rework with
	// a new updatedAt. The Rework transition must produce a fresh task.
	aiReady := tracker.Issue{
		ID: "abc-123", Identifier: "ENG-1", Title: "do thing",
		State: "AI Ready", UpdatedAt: "2026-05-08T10:00:00Z",
	}
	rework := tracker.Issue{
		ID: "abc-123", Identifier: "ENG-1", Title: "do thing",
		State: "Rework", UpdatedAt: "2026-05-08T11:30:00Z",
	}

	processIssues(context.Background(), store, cfg, []tracker.Issue{aiReady})
	processIssues(context.Background(), store, cfg, []tracker.Issue{rework})

	if store.inserts() != 2 {
		t.Fatalf("inserts = %d, want 2 (one for AI Ready, one for Rework)", store.inserts())
	}

	// Confirm the two source_event_ids actually differ — the Rework one is
	// composed with the rework marker and updatedAt.
	if _, ok := store.bySource["linear_issue|abc-123"]; !ok {
		t.Fatalf("missing AI Ready task keyed by issue.ID")
	}
	reworkKey := "linear_issue|abc-123|rework|2026-05-08T11:30:00Z"
	if _, ok := store.bySource[reworkKey]; !ok {
		t.Fatalf("missing Rework task keyed by %q; got keys=%v", reworkKey, mapKeys(store.bySource))
	}
}

func TestProcessIssuesDoesNotLoopWhileStuckInRework(t *testing.T) {
	store := newFakeStore()
	cfg := baseConfig()

	// Issue is parked in Rework. Multiple polls happen before the user
	// touches the issue, so updatedAt does not advance. We must not create
	// a fresh task on every poll.
	rework := tracker.Issue{
		ID: "abc-123", Identifier: "ENG-1", Title: "do thing",
		State: "Rework", UpdatedAt: "2026-05-08T11:30:00Z",
	}

	for i := 0; i < 5; i++ {
		processIssues(context.Background(), store, cfg, []tracker.Issue{rework})
	}

	if store.inserts() != 1 {
		t.Fatalf("inserts after 5 Rework polls with same updatedAt = %d, want 1", store.inserts())
	}
}

func TestProcessIssuesSkipsWhenCloneURLMissing(t *testing.T) {
	store := newFakeStore()
	cfg := baseConfig()
	cfg.Repo.CloneURL = ""

	issue := tracker.Issue{
		ID: "abc-123", Identifier: "ENG-1", State: "AI Ready",
		UpdatedAt: "2026-05-08T10:00:00Z",
	}
	processIssues(context.Background(), store, cfg, []tracker.Issue{issue})

	if len(store.calls) != 0 {
		t.Fatalf("Enqueue called %d times, want 0 when clone_url missing", len(store.calls))
	}
}

func TestProcessIssuesRoutesServiceOnlyLinearWorkflow(t *testing.T) {
	store := newFakeStore()
	cfg := baseConfig()
	cfg.Repo = workflow.RepoConfig{}
	cfg.Tracker.ProjectSlug = "platform"
	cfg.Services = []workflow.ServiceConfig{
		{
			Name:    "api",
			Repo:    workflow.RepoConfig{Owner: "octo", Name: "api", CloneURL: "git@example.com:octo/api.git", DefaultBranch: "main"},
			Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "platform", Labels: []string{"api"}},
		},
	}

	issue := tracker.Issue{
		ID: "abc-123", Identifier: "ENG-1", Title: "route me", State: "AI Ready",
		ProjectSlug: "platform", Labels: []string{"api"}, UpdatedAt: "2026-05-08T10:00:00Z",
	}
	processIssues(context.Background(), store, cfg, []tracker.Issue{issue})

	if len(store.calls) != 1 {
		t.Fatalf("Enqueue calls = %d, want 1 for matched service route", len(store.calls))
	}
	got := store.calls[0]
	if got.RepoOwner != "octo" || got.RepoName != "api" || got.CloneURL != "git@example.com:octo/api.git" {
		t.Fatalf("task repo = %s/%s %s, want octo/api service repo", got.RepoOwner, got.RepoName, got.CloneURL)
	}
}

func TestProcessIssuesSkipsUnmatchedServiceOnlyLinearWorkflow(t *testing.T) {
	store := newFakeStore()
	cfg := baseConfig()
	cfg.Repo = workflow.RepoConfig{}
	cfg.Tracker.ProjectSlug = "platform"
	cfg.Services = []workflow.ServiceConfig{
		{
			Name:    "api",
			Repo:    workflow.RepoConfig{Owner: "octo", Name: "api", CloneURL: "git@example.com:octo/api.git", DefaultBranch: "main"},
			Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "platform", Labels: []string{"api"}},
		},
	}

	issue := tracker.Issue{
		ID: "abc-123", Identifier: "ENG-1", Title: "skip me", State: "AI Ready",
		ProjectSlug: "platform", Labels: []string{"docs"}, UpdatedAt: "2026-05-08T10:00:00Z",
	}
	processIssues(context.Background(), store, cfg, []tracker.Issue{issue})

	if len(store.calls) != 0 {
		t.Fatalf("Enqueue called %d times, want 0 for unmatched service route", len(store.calls))
	}
}

func TestProcessIssuesSkipsAmbiguousServiceOnlyLinearWorkflow(t *testing.T) {
	store := newFakeStore()
	cfg := baseConfig()
	cfg.Repo = workflow.RepoConfig{}
	cfg.Tracker.ProjectSlug = "platform"
	cfg.Services = []workflow.ServiceConfig{
		{Name: "api", Repo: workflow.RepoConfig{Owner: "octo", Name: "api", CloneURL: "git@example.com:octo/api.git", DefaultBranch: "main"}, Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "platform", Labels: []string{"backend"}}},
		{Name: "worker", Repo: workflow.RepoConfig{Owner: "octo", Name: "worker", CloneURL: "git@example.com:octo/worker.git", DefaultBranch: "main"}, Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "platform", Labels: []string{"backend"}}},
	}

	issue := tracker.Issue{
		ID: "abc-123", Identifier: "ENG-1", Title: "ambiguous", State: "AI Ready",
		ProjectSlug: "platform", Labels: []string{"backend"}, UpdatedAt: "2026-05-08T10:00:00Z",
	}
	processIssues(context.Background(), store, cfg, []tracker.Issue{issue})

	if len(store.calls) != 0 {
		t.Fatalf("Enqueue called %d times, want 0 for ambiguous service route", len(store.calls))
	}
}

func TestProcessIssuesIgnoresServiceWithoutLinearRoute(t *testing.T) {
	store := newFakeStore()
	cfg := baseConfig()
	cfg.Repo = workflow.RepoConfig{}
	cfg.Tracker.ProjectSlug = "platform"
	cfg.Services = []workflow.ServiceConfig{
		{Name: "api", Repo: workflow.RepoConfig{Owner: "octo", Name: "api", CloneURL: "git@example.com:octo/api.git", DefaultBranch: "main"}, Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "platform", Labels: []string{"api"}}},
		{Name: "unrouted", Repo: workflow.RepoConfig{Owner: "octo", Name: "unrouted", CloneURL: "git@example.com:octo/unrouted.git", DefaultBranch: "main"}},
	}

	issue := tracker.Issue{
		ID: "abc-123", Identifier: "ENG-1", Title: "route me", State: "AI Ready",
		ProjectSlug: "platform", Labels: []string{"api"}, UpdatedAt: "2026-05-08T10:00:00Z",
	}
	processIssues(context.Background(), store, cfg, []tracker.Issue{issue})

	if len(store.calls) != 1 {
		t.Fatalf("Enqueue calls = %d, want 1; unrouted service must not create ambiguity", len(store.calls))
	}
	if got := store.calls[0].RepoName; got != "api" {
		t.Fatalf("enqueued repo name = %q, want api", got)
	}
}

func TestProcessIssuesFallsBackToRootRepoWhenNoServiceRouteMatches(t *testing.T) {
	store := newFakeStore()
	cfg := baseConfig()
	cfg.Services = []workflow.ServiceConfig{
		{Name: "api", Repo: workflow.RepoConfig{Owner: "octo", Name: "api", CloneURL: "git@example.com:octo/api.git", DefaultBranch: "main"}, Tracker: workflow.ServiceTrackerRouteConfig{Labels: []string{"api"}}},
	}

	issue := tracker.Issue{
		ID: "abc-123", Identifier: "ENG-1", Title: "root repo", State: "AI Ready",
		Labels: []string{"docs"}, UpdatedAt: "2026-05-08T10:00:00Z",
	}
	processIssues(context.Background(), store, cfg, []tracker.Issue{issue})

	if len(store.calls) != 1 {
		t.Fatalf("Enqueue calls = %d, want 1 for root repo fallback", len(store.calls))
	}
	got := store.calls[0]
	if got.RepoOwner != "octo" || got.RepoName != "demo" || got.CloneURL != "git@example.com:octo/demo.git" {
		t.Fatalf("task repo = %s/%s %s, want octo/demo root repo", got.RepoOwner, got.RepoName, got.CloneURL)
	}
}

func mapKeys(m map[string]task.Task) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
