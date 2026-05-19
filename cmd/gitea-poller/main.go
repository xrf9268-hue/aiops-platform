package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/queue"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type enqueuer interface {
	Enqueue(ctx context.Context, t task.Task) (task.Task, bool, error)
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: gitea-poller /path/to/WORKFLOW.md")
	}
	ctx := context.Background()
	wf, err := workflow.Load(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	if wf.Config.Tracker.Kind != "gitea" {
		log.Fatalf("tracker.kind must be gitea, got %q", wf.Config.Tracker.Kind)
	}
	dsn := databaseURL()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	store := queue.New(pool)
	baseURL := giteaBaseURL(wf.Config.Tracker)
	client := gitea.NewTrackerClient(wf.Config.Tracker, baseURL, wf.Config.Repo.Owner, wf.Config.Repo.Name)
	client.Logf = log.Printf
	interval := time.Duration(wf.Config.Tracker.PollIntervalMs) * time.Millisecond

	for {
		issues, err := client.ListActiveIssues(ctx)
		if err != nil {
			log.Printf("gitea poll error: %v", err)
			time.Sleep(interval)
			continue
		}
		processIssues(ctx, store, &wf.Config, issues)
		time.Sleep(interval)
	}
}

func processIssues(ctx context.Context, store enqueuer, cfg *workflow.Config, issues []tracker.Issue) {
	for _, issue := range issues {
		if cfg.Repo.CloneURL == "" {
			log.Printf("skip %s: repo.clone_url missing in WORKFLOW.md", issue.Identifier)
			continue
		}
		out, deduped, err := store.Enqueue(ctx, task.Task{
			SourceType:    "gitea_issue",
			SourceEventID: sourceEventID(issue),
			RepoOwner:     cfg.Repo.Owner,
			RepoName:      cfg.Repo.Name,
			CloneURL:      cfg.Repo.CloneURL,
			BaseBranch:    cfg.Repo.DefaultBranch,
			Title:         fmt.Sprintf("%s %s", issue.Identifier, issue.Title),
			Description:   issue.Description + "\n\nGitea: " + issue.URL,
			Actor:         "gitea",
			Model:         cfg.Agent.Default,
			Priority:      50,
		})
		if err != nil {
			log.Printf("enqueue %s error: %v", issue.Identifier, err)
			continue
		}
		log.Printf("issue %s -> task %s deduped=%v", issue.Identifier, out.ID, deduped)
	}
}

func sourceEventID(issue tracker.Issue) string {
	if strings.EqualFold(issue.State, "Rework") && !issue.UpdatedAt.IsZero() {
		return issue.ID + "|rework|" + tracker.TimeString(issue.UpdatedAt)
	}
	return issue.ID
}

func giteaBaseURL(cfg workflow.TrackerConfig) string {
	if cfg.ProjectSlug != "" {
		return strings.TrimRight(cfg.ProjectSlug, "/")
	}
	return env("GITEA_BASE_URL", "http://localhost:3000")
}

func databaseURL() string {
	return env("DATABASE_URL", "postgres://aiops:aiops@localhost:5432/aiops?sslmode=disable")
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
