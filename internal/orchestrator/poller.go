package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// ActiveIssueLister is the tracker reader required by the SPEC poll tick.
type ActiveIssueLister interface {
	ListActiveIssues(ctx context.Context) ([]tracker.Issue, error)
}

// Poller connects tracker polling to the orchestrator runtime state. It has no
// durable queue dependency: candidates are read from the tracker and claimed by
// the in-process Orchestrator actor.
type Poller struct {
	tracker      ActiveIssueLister
	orchestrator *Orchestrator
	overflow     []tracker.Issue
}

// NewPoller returns a SPEC-aligned tracker poller backed by orchestrator-owned
// runtime state instead of the legacy Postgres task queue.
func NewPoller(tracker ActiveIssueLister, orchestrator *Orchestrator) *Poller {
	return &Poller{tracker: tracker, orchestrator: orchestrator}
}

// PollOnce performs one tracker tick: fetch active issues and ask the
// orchestrator actor to dispatch each candidate. Duplicate candidates are
// ignored by the actor's runtime claim state.
func (p *Poller) PollOnce(ctx context.Context) error {
	if p == nil || p.tracker == nil {
		return errors.New("orchestrator poller requires tracker")
	}
	if p.orchestrator == nil {
		return errors.New("orchestrator poller requires orchestrator")
	}
	issues, err := p.tracker.ListActiveIssues(ctx)
	if err != nil {
		return err
	}
	candidates := mergeOverflowCandidates(p.overflow, issues)
	p.overflow = nil
	var dispatchErr error
	for _, issue := range candidates {
		if issue.ID == "" {
			continue
		}
		if err := p.orchestrator.RequestDispatch(ctx, issue, nil); err != nil {
			switch {
			case errors.Is(err, ErrNotDispatched):
				continue
			case errors.Is(err, ErrCapacityFull):
				p.overflow = append(p.overflow, issue)
			default:
				dispatchErr = errors.Join(dispatchErr, fmt.Errorf("dispatch %s: %w", issue.ID, err))
			}
		}
	}
	return dispatchErr
}

func mergeOverflowCandidates(overflow, fresh []tracker.Issue) []tracker.Issue {
	if len(overflow) == 0 {
		return fresh
	}
	candidates := make([]tracker.Issue, 0, len(overflow)+len(fresh))
	seen := make(map[string]struct{}, len(overflow)+len(fresh))
	for _, issue := range overflow {
		if issue.ID == "" {
			continue
		}
		seen[issue.ID] = struct{}{}
		candidates = append(candidates, issue)
	}
	for _, issue := range fresh {
		if _, ok := seen[issue.ID]; ok {
			continue
		}
		candidates = append(candidates, issue)
	}
	return candidates
}

// TaskBuilder converts a tracker candidate into the task shape consumed by the
// existing worker runner. This is an adapter between the SPEC poller/runtime
// path and the legacy task execution API; it is intentionally in-memory only.
type TaskBuilder func(issue tracker.Issue) (task.Task, error)

// WorkerTaskDispatcher runs the existing worker task executor for issues
// accepted by the orchestrator actor. It replaces the old Postgres claim loop:
// the orchestrator owns scheduling/claim state, while worker.RunTask
// continues to prepare workspaces and run the configured agent.
type WorkerTaskDispatcher struct {
	BuildTask TaskBuilder
	Config    worker.Config
	Emitter   worker.EventEmitter
}

// Spawn implements Dispatcher.
func (d WorkerTaskDispatcher) Spawn(ctx context.Context, issue tracker.Issue, _ *int) <-chan WorkerResult {
	out := make(chan WorkerResult, 1)
	go func() {
		defer close(out)
		start := time.Now()
		if d.BuildTask == nil {
			out <- WorkerResult{Err: errors.New("worker task dispatcher requires task builder"), NonRetryable: true, Elapsed: time.Since(start)}
			return
		}
		tk, err := d.BuildTask(issue)
		if err != nil {
			out <- WorkerResult{Err: err, NonRetryable: true, Elapsed: time.Since(start)}
			return
		}
		if rterr := worker.RunTask(ctx, d.Emitter, tk, d.Config); rterr != nil {
			out <- WorkerResult{Err: rterr.Err, Elapsed: time.Since(start)}
			return
		}
		out <- WorkerResult{Elapsed: time.Since(start)}
	}()
	return out
}

// TaskFromIssue builds the in-memory task handed to worker execution for a
// tracker candidate. Dedupe/claiming lives in OrchestratorState, not in this
// task ID: the ID is only a stable per-run/workspace identifier.
func TaskFromIssue(issue tracker.Issue, cfg workflow.Config) (task.Task, error) {
	if cfg.Repo.CloneURL == "" {
		return task.Task{}, fmt.Errorf("repo.clone_url missing in WORKFLOW.md")
	}
	sourceType := cfg.Tracker.Kind + "_issue"
	if cfg.Tracker.Kind == "" {
		sourceType = "tracker_issue"
	}
	sourceEventID := issue.ID
	if sourceEventID == "" {
		return task.Task{}, fmt.Errorf("tracker issue id is required")
	}
	if issue.Identifier != "" {
		sourceEventID = issue.Identifier
	}
	return task.Task{
		ID:            string(IssueID(issue.ID)),
		SourceType:    sourceType,
		SourceEventID: sourceEventID,
		RepoOwner:     cfg.Repo.Owner,
		RepoName:      cfg.Repo.Name,
		CloneURL:      cfg.Repo.CloneURL,
		BaseBranch:    cfg.Repo.DefaultBranch,
		WorkBranch:    "ai/" + issue.ID,
		Title:         fmt.Sprintf("%s %s", issue.Identifier, issue.Title),
		Description:   issue.Description + "\n\nTracker: " + issue.URL,
		Actor:         cfg.Tracker.Kind,
		Model:         cfg.Agent.Default,
		Priority:      50,
	}, nil
}

// RunPollLoop repeatedly polls the tracker until ctx is canceled.
func RunPollLoop(ctx context.Context, poller *Poller, interval time.Duration) error {
	if poller == nil {
		return errors.New("orchestrator poll loop requires poller")
	}
	if interval <= 0 {
		interval = 30 * time.Second
	}
	for {
		if err := poller.PollOnce(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("tracker poll error: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
