package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xrf9268-hue/aiops-platform/internal/orchestrator"
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
	listers := linearIssueListers(wf.Config)
	interval := time.Duration(wf.Config.Tracker.PollIntervalMs) * time.Millisecond

	for {
		if err := pollOnce(ctx, store, &wf.Config, listers); err != nil {
			log.Printf("linear poll error: %v", err)
		}
		time.Sleep(interval)
	}
}

type activeIssueLister interface {
	ListActiveIssues(ctx context.Context) ([]tracker.Issue, error)
}

type linearIssueSource struct {
	lister      activeIssueLister
	projectSlug string
}

func linearIssueListers(cfg workflow.Config) []linearIssueSource {
	projectConfigs := orchestrator.TrackerProjectConfigs(cfg)
	listers := make([]linearIssueSource, 0, len(projectConfigs))
	for _, projectCfg := range projectConfigs {
		listers = append(listers, linearIssueSource{
			lister:      tracker.NewLinearClient(projectCfg.Tracker),
			projectSlug: strings.TrimSpace(projectCfg.Tracker.ProjectSlug),
		})
	}
	return listers
}

func pollOnce(ctx context.Context, store enqueuer, cfg *workflow.Config, sources []linearIssueSource) error {
	var pollErr error
	for _, source := range sources {
		issues, err := source.lister.ListActiveIssues(ctx)
		if err != nil {
			pollErr = errors.Join(pollErr, err)
			continue
		}
		processIssues(ctx, store, cfg, filterIssuesForPollSource(*cfg, issues, source.projectSlug))
	}
	return pollErr
}

func filterIssuesForPollSource(cfg workflow.Config, issues []tracker.Issue, projectSlug string) []tracker.Issue {
	rootProject := strings.TrimSpace(cfg.Tracker.ProjectSlug)
	if rootProject != "" && strings.EqualFold(rootProject, strings.TrimSpace(projectSlug)) {
		return issues
	}
	out := make([]tracker.Issue, 0, len(issues))
	for _, issue := range issues {
		for _, service := range cfg.Services {
			if serviceMatchesIssue(service, cfg.Tracker, issue) {
				out = append(out, issue)
				break
			}
		}
	}
	return out
}

// processIssues enqueues each polled Linear issue once. It is split out from
// main so unit tests can drive the dedupe and Rework re-enqueue behavior
// without standing up Postgres or the Linear API.
func processIssues(ctx context.Context, store enqueuer, cfg *workflow.Config, issues []tracker.Issue) {
	for _, issue := range issues {
		repo, serviceName, ok := repoForIssue(*cfg, issue)
		if !ok {
			continue
		}
		if repo.CloneURL == "" {
			log.Printf("skip %s: repo.clone_url missing in WORKFLOW.md", issue.Identifier)
			continue
		}
		description := issue.Description + "\n\nLinear: " + issue.URL
		if serviceName != "" {
			description += "\n\nService: " + serviceName
		}
		out, deduped, err := store.Enqueue(ctx, task.Task{
			SourceType:    "linear_issue",
			SourceEventID: sourceEventIDForService(issue, serviceName),
			RepoOwner:     repo.Owner,
			RepoName:      repo.Name,
			CloneURL:      repo.CloneURL,
			BaseBranch:    repo.DefaultBranch,
			Title:         fmt.Sprintf("%s %s", issue.Identifier, issue.Title),
			Description:   description,
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

func repoForIssue(cfg workflow.Config, issue tracker.Issue) (workflow.RepoConfig, string, bool) {
	if len(cfg.Services) == 0 {
		return cfg.Repo, "", true
	}
	matches := make([]workflow.ServiceConfig, 0, len(cfg.Services))
	for _, service := range cfg.Services {
		if serviceMatchesIssue(service, cfg.Tracker, issue) {
			matches = append(matches, service)
		}
	}
	switch len(matches) {
	case 0:
		if strings.TrimSpace(cfg.Repo.CloneURL) != "" {
			return cfg.Repo, "", true
		}
		log.Printf("skip %s: no configured service matched Linear route", issue.Identifier)
		return workflow.RepoConfig{}, "", false
	case 1:
		return matches[0].Repo, matches[0].Name, true
	default:
		names := make([]string, 0, len(matches))
		for _, service := range matches {
			names = append(names, service.Name)
		}
		log.Printf("skip %s: ambiguous Linear route matched services %s", issue.Identifier, strings.Join(names, ", "))
		return workflow.RepoConfig{}, "", false
	}
}

func serviceMatchesIssue(service workflow.ServiceConfig, defaults workflow.TrackerConfig, issue tracker.Issue) bool {
	route := service.Tracker
	if !hasExplicitServiceRoute(route) {
		return false
	}
	projectSlug := strings.TrimSpace(route.ProjectSlug)
	if projectSlug == "" {
		projectSlug = strings.TrimSpace(defaults.ProjectSlug)
	}
	if projectSlug != "" && !strings.EqualFold(projectSlug, strings.TrimSpace(issue.ProjectSlug)) {
		return false
	}
	if route.TeamKey != "" && !strings.EqualFold(strings.TrimSpace(route.TeamKey), strings.TrimSpace(issue.TeamKey)) {
		return false
	}
	issueLabels := make(map[string]struct{}, len(issue.Labels))
	for _, label := range issue.Labels {
		if label = strings.ToLower(strings.TrimSpace(label)); label != "" {
			issueLabels[label] = struct{}{}
		}
	}
	for _, label := range route.Labels {
		if _, ok := issueLabels[strings.ToLower(strings.TrimSpace(label))]; !ok {
			return false
		}
	}
	for key, want := range route.CustomFields {
		got, ok := issue.CustomFields[key]
		if !ok || strings.TrimSpace(got) != strings.TrimSpace(want) {
			return false
		}
	}
	return true
}

func hasExplicitServiceRoute(route workflow.ServiceTrackerRouteConfig) bool {
	return strings.TrimSpace(route.ProjectSlug) != "" ||
		strings.TrimSpace(route.TeamKey) != "" ||
		len(route.Labels) > 0 ||
		len(route.CustomFields) > 0
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
	return sourceEventIDForService(issue, "")
}

func sourceEventIDForService(issue tracker.Issue, serviceName string) string {
	key := issue.ID
	if serviceName != "" {
		key += "|service|" + serviceName
	}
	if strings.EqualFold(issue.State, reworkStateName) && !issue.UpdatedAt.IsZero() {
		return key + "|rework|" + tracker.TimeString(issue.UpdatedAt)
	}
	return key
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
