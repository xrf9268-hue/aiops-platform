package main

import (
	"context"
	"testing"

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
	got := sourceEventID(tracker.Issue{ID: "101", State: "Rework", UpdatedAt: "2026-05-17T00:00:00Z"})
	if got != "101|rework|2026-05-17T00:00:00Z" {
		t.Fatalf("sourceEventID = %q", got)
	}
}
