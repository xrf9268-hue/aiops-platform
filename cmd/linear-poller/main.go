package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xrf9268-hue/aiops-platform/internal/queue"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: linear-poller /path/to/WORKFLOW.md")
	}
	ctx := context.Background()
	wf, err := workflow.Load(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	if wf.Config.Tracker.Kind != "linear" {
		log.Fatalf("tracker.kind must be linear, got %q", wf.Config.Tracker.Kind)
	}
	dsn := env("DATABASE_URL", "postgres://aiops:aiops@localhost:5432/aiops?sslmode=disable")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	store := queue.New(pool)
	client := tracker.NewLinearClient(wf.Config.Tracker)
	interval := time.Duration(wf.Config.Tracker.PollIntervalMs) * time.Millisecond

	for {
		issues, err := client.ListActiveIssues(ctx)
		if err != nil {
			log.Printf("linear poll error: %v", err)
			time.Sleep(interval)
			continue
		}
		for _, issue := range issues {
			if wf.Config.Repo.CloneURL == "" {
				log.Printf("skip %s: repo.clone_url missing in WORKFLOW.md", issue.Identifier)
				continue
			}
			out, deduped, err := store.Enqueue(ctx, task.Task{
				SourceType:    "linear_issue",
				SourceEventID: issue.ID,
				RepoOwner:     wf.Config.Repo.Owner,
				RepoName:      wf.Config.Repo.Name,
				CloneURL:      wf.Config.Repo.CloneURL,
				BaseBranch:    wf.Config.Repo.DefaultBranch,
				Title:         fmt.Sprintf("%s %s", issue.Identifier, issue.Title),
				Description:   issue.Description + "\n\nLinear: " + issue.URL,
				Actor:         "linear",
				Model:         wf.Config.Agent.Default,
				Priority:      50,
			})
			if err != nil {
				log.Printf("enqueue %s error: %v", issue.Identifier, err)
				continue
			}
			log.Printf("issue %s -> task %s deduped=%v", issue.Identifier, out.ID, deduped)
		}
		time.Sleep(interval)
	}
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
