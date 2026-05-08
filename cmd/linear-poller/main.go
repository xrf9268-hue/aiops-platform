package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xrf9268-hue/aiops-platform/internal/queue"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// reworkStateName is the Linear state that should re-enqueue a task each
// time the issue moves into it. The poller treats this state specially when
// composing source_event_id so re-runs are not deduped against the original
// task.
const reworkStateName = "Rework"

// enqueuer is the subset of queue.Store used by the poller. Defined as an
// interface so the poll loop can be exercised in unit tests without a real
// Postgres pool.
type enqueuer interface {
	Enqueue(ctx context.Context, t task.Task) (task.Task, bool, error)
}

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
		processIssues(ctx, store, &wf.Config, issues)
		time.Sleep(interval)
	}
}

// processIssues enqueues each polled Linear issue once. It is split out from
// main so unit tests can drive the dedupe and Rework re-enqueue behavior
// without standing up Postgres or the Linear API.
func processIssues(ctx context.Context, store enqueuer, cfg *workflow.Config, issues []tracker.Issue) {
	for _, issue := range issues {
		if cfg.Repo.CloneURL == "" {
			log.Printf("skip %s: repo.clone_url missing in WORKFLOW.md", issue.Identifier)
			continue
		}
		out, deduped, err := store.Enqueue(ctx, task.Task{
			SourceType:    "linear_issue",
			SourceEventID: sourceEventID(issue),
			RepoOwner:     cfg.Repo.Owner,
			RepoName:      cfg.Repo.Name,
			CloneURL:      cfg.Repo.CloneURL,
			BaseBranch:    cfg.Repo.DefaultBranch,
			Title:         fmt.Sprintf("%s %s", issue.Identifier, issue.Title),
			Description:   issue.Description + "\n\nLinear: " + issue.URL,
			Actor:         "linear",
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

// sourceEventID builds the dedupe key the poller hands to queue.Store.Enqueue.
//
// For non-Rework states (e.g. AI Ready, In Progress) the key is just the
// Linear issue.ID. queue.Enqueue uses ON CONFLICT (source_type,
// source_event_id) DO UPDATE, so repeated polls of the same issue collapse
// into the original task row, which is what we want while a task is in
// flight or being iterated on.
//
// For Rework, we want each transition into Rework to produce a fresh task
// even though earlier polls of the same issue.ID already created a row.
// Linear's GraphQL exposes issue.updatedAt, which advances whenever the
// issue's state (or fields) change. Composing the key as
//
//	<issue.ID>|rework|<updatedAt>
//
// makes the first poll after the issue lands in Rework a brand-new
// source_event_id (so Postgres INSERTs a new task), while subsequent polls
// during the same Rework dwell stay deduped because updatedAt does not
// change. A later move into Rework (e.g. Human Review -> Rework after the
// agent retried) advances updatedAt again, producing the next fresh task.
//
// This deliberately uses issue.UpdatedAt as a fallback for Linear's
// state-transition history: the GraphQL ListIssues query in
// internal/tracker/linear.go does not currently fetch issue history nodes,
// and updatedAt is sufficient because state changes are the dominant
// trigger for re-polling a Rework issue.
func sourceEventID(issue tracker.Issue) string {
	if strings.EqualFold(issue.State, reworkStateName) && issue.UpdatedAt != "" {
		return issue.ID + "|rework|" + issue.UpdatedAt
	}
	return issue.ID
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
