package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type fakeIssueLister struct {
	issues []tracker.Issue
	err    error
	calls  int
}

func (l *fakeIssueLister) ListActiveIssues(ctx context.Context) ([]tracker.Issue, error) {
	l.calls++
	if l.err != nil {
		return nil, l.err
	}
	return l.issues, nil
}

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

func TestLinearIssueListersFanOutServiceOnlyLinearWorkflowProjects(t *testing.T) {
	cfg := baseConfig()
	cfg.Repo = workflow.RepoConfig{}
	cfg.Tracker.ProjectSlug = ""
	cfg.Services = []workflow.ServiceConfig{
		{
			Name:    "api",
			Repo:    workflow.RepoConfig{Owner: "octo", Name: "api", CloneURL: "git@example.com:octo/api.git", DefaultBranch: "main"},
			Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "api-platform"},
		},
		{
			Name:    "web",
			Repo:    workflow.RepoConfig{Owner: "octo", Name: "web", CloneURL: "git@example.com:octo/web.git", DefaultBranch: "main"},
			Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "web-platform"},
		},
	}

	listers := linearIssueListers(*cfg)
	if len(listers) != 2 {
		t.Fatalf("linear issue listers = %d, want one per service project", len(listers))
	}
	projects := make([]string, 0, len(listers))
	for _, source := range listers {
		client, ok := source.lister.(*tracker.LinearClient)
		if !ok {
			t.Fatalf("lister type = %T, want *tracker.LinearClient", source.lister)
		}
		projects = append(projects, client.Config.ProjectSlug)
	}
	want := []string{"api-platform", "web-platform"}
	for i := range want {
		if projects[i] != want[i] {
			t.Fatalf("lister projects = %v, want %v", projects, want)
		}
	}
}

func TestPollOnceFansOutProvidedLinearListers(t *testing.T) {
	store := newFakeStore()
	cfg := baseConfig()
	cfg.Tracker.ProjectSlug = "platform"
	apiLister := &fakeIssueLister{issues: []tracker.Issue{{
		ID: "api-1", Identifier: "API-1", Title: "api", State: "AI Ready", ProjectSlug: "platform",
	}}}
	webLister := &fakeIssueLister{issues: []tracker.Issue{{
		ID: "web-1", Identifier: "WEB-1", Title: "web", State: "AI Ready", ProjectSlug: "platform",
	}}}

	err := pollOnce(context.Background(), store, cfg, []linearIssueSource{
		{lister: apiLister, projectSlug: "platform"},
		{lister: webLister, projectSlug: "platform"},
	})
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if apiLister.calls != 1 || webLister.calls != 1 {
		t.Fatalf("lister calls = api:%d web:%d, want both service project listers called once", apiLister.calls, webLister.calls)
	}
	if len(store.calls) != 2 {
		t.Fatalf("Enqueue calls = %d, want one issue per lister", len(store.calls))
	}
}

func TestPollOnceDoesNotFallbackServiceProjectIssuesToRootRepo(t *testing.T) {
	store := newFakeStore()
	cfg := baseConfig()
	cfg.Tracker.ProjectSlug = "platform"
	cfg.Services = []workflow.ServiceConfig{
		{
			Name:    "api",
			Repo:    workflow.RepoConfig{Owner: "octo", Name: "api", CloneURL: "git@example.com:octo/api.git", DefaultBranch: "main"},
			Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "api-platform", Labels: []string{"api"}},
		},
	}
	rootLister := &fakeIssueLister{issues: []tracker.Issue{{
		ID: "root-1", Identifier: "ROOT-1", Title: "root", State: "AI Ready", ProjectSlug: "platform",
	}}}
	serviceLister := &fakeIssueLister{issues: []tracker.Issue{{
		ID: "api-1", Identifier: "API-1", Title: "unmatched api project issue", State: "AI Ready", ProjectSlug: "api-platform",
		Labels: []string{"docs"},
	}}}

	err := pollOnce(context.Background(), store, cfg, []linearIssueSource{
		{lister: rootLister, projectSlug: "platform"},
		{lister: serviceLister, projectSlug: "api-platform"},
	})
	if err != nil {
		t.Fatalf("pollOnce: %v", err)
	}
	if len(store.calls) != 1 {
		t.Fatalf("Enqueue calls = %d, want only root project issue enqueued", len(store.calls))
	}
	if got := store.calls[0].SourceEventID; got != "root-1" {
		t.Fatalf("enqueued source event = %q, want only root-1", got)
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
