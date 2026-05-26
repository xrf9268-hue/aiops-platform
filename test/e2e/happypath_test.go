//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/orchestrator"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestGiteaMockLoop_HappyPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	taskID, owner, repo, events := runGiteaWorkerTask(t, ctx, "demo-happy", "first task", "Make a tiny change.", "mock-happy.md")

	seen := map[string]bool{}
	for _, ev := range events.byTask(taskID) {
		seen[ev.Kind] = true
	}
	for _, want := range []string{task.EventWorkflowResolved, task.EventRunnerStart, task.EventRunnerEnd} {
		if !seen[want] {
			t.Fatalf("missing event %s in %+v", want, events)
		}
	}

	workBranch := events.task(taskID).WorkBranch
	if !regexp.MustCompile(`^ai/[0-9]+$`).MatchString(workBranch) {
		t.Fatalf("unexpected worker task branch %q", workBranch)
	}
	branchExists, err := bed.gitea.getBranch(ctx, owner, repo, workBranch)
	if err != nil {
		t.Fatalf("getBranch: %v", err)
	}
	if branchExists {
		t.Fatalf("worker must not push work branch %q", workBranch)
	}
	prs, err := bed.gitea.listOpenPRs(ctx, owner, repo)
	if err != nil {
		t.Fatalf("listOpenPRs: %v", err)
	}
	if len(prs) != 0 {
		t.Fatalf("worker must not open PRs; got %d open PR(s): %+v", len(prs), prs)
	}
}

func TestGiteaWorkerReconciliationStopsRunMovedToDone(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	owner, repo := bed.gitea.botUser, "demo-gitea-reconcile"
	if _, err := bed.gitea.createRepo(ctx, repo); err != nil {
		t.Fatalf("createRepo: %v", err)
	}
	issueNum, err := bed.gitea.createIssue(ctx, owner, repo, "stop me", "move to done while running")
	if err != nil {
		t.Fatalf("createIssue: %v", err)
	}
	if err := bed.gitea.ensureLabels(ctx, owner, repo, []string{"aiops/todo", "aiops/done"}); err != nil {
		t.Fatalf("ensure labels: %v", err)
	}
	if err := bed.gitea.addIssueLabels(ctx, owner, repo, issueNum, []string{"aiops/todo"}); err != nil {
		t.Fatalf("add todo label: %v", err)
	}

	cfg := workflow.DefaultConfig()
	cfg.Repo.Owner = owner
	cfg.Repo.Name = repo
	cfg.Repo.DefaultBranch = "main"
	cfg.Tracker.Kind = "gitea"
	cfg.Tracker.APIKey = bed.gitea.botToken
	cfg.Tracker.ActiveStates = []string{"AI Ready"}
	cfg.Tracker.TerminalStates = []string{"Done", "Canceled"}
	client := gitea.NewTrackerClient(cfg.Tracker, bed.gitea.baseURL, owner, repo)
	client.HTTP = httpClientForE2E()

	dispatcher := &giteaReconcileBlockingDispatcher{
		started:  make(chan tracker.Issue, 1),
		canceled: make(chan error, 1),
	}
	orch := orchestrator.New(orchestrator.NewOrchestratorState(15000, 1), orchestrator.Deps{
		Dispatcher: dispatcher,
		Scheduler:  orchestrator.RetryScheduler{MaxBackoff: time.Minute},
	})
	orchCtx, orchCancel := context.WithCancel(ctx)
	t.Cleanup(orchCancel)
	go orch.Run(orchCtx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("start orchestrator: %v", err)
	}

	poller := orchestrator.NewPollerWithReconciliation(client, orch, orchestrator.ReconciliationConfig{
		ActiveStates:      cfg.Tracker.ActiveStates,
		TerminalStates:    cfg.Tracker.TerminalStates,
		WorkerExitTimeout: 10 * time.Second,
	})
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("initial poll: %v", err)
	}
	select {
	case issue := <-dispatcher.started:
		if issue.Identifier != fmt.Sprintf("#%d", issueNum) || issue.State != "AI Ready" {
			t.Fatalf("started issue = %#v, want Gitea issue #%d in AI Ready", issue, issueNum)
		}
	case <-ctx.Done():
		t.Fatalf("worker did not start: %v", ctx.Err())
	}

	if err := bed.gitea.replaceIssueLabels(ctx, owner, repo, issueNum, []string{"aiops/done"}); err != nil {
		t.Fatalf("replace issue labels: %v", err)
	}
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("reconcile poll: %v", err)
	}
	select {
	case <-dispatcher.canceled:
	case <-ctx.Done():
		t.Fatalf("worker was not canceled after moving issue to Done: %v", ctx.Err())
	}
	pollUntil(t, 10*time.Second, 100*time.Millisecond, func(ctx context.Context) (bool, error) {
		view, err := orch.Snapshot(ctx)
		if err != nil {
			return false, err
		}
		return len(view.Running) == 0, nil
	})
}

type giteaReconcileBlockingDispatcher struct {
	started  chan tracker.Issue
	canceled chan error
}

func (d *giteaReconcileBlockingDispatcher) Spawn(ctx context.Context, issue tracker.Issue, _ *int) <-chan orchestrator.WorkerResult {
	select {
	case d.started <- issue:
	default:
	}
	ch := make(chan orchestrator.WorkerResult, 1)
	go func() {
		<-ctx.Done()
		err := ctx.Err()
		d.canceled <- err
		ch <- orchestrator.WorkerResult{Err: err, Elapsed: time.Millisecond}
		close(ch)
	}()
	return ch
}

func runGiteaWorkerTask(t *testing.T, ctx context.Context, repo, title, body, fixture string) (taskID, owner, repoName string, events *e2eEventRecorder) {
	t.Helper()

	owner = bed.gitea.botUser
	cloneURL, err := bed.gitea.createRepo(ctx, repo)
	if err != nil {
		t.Fatalf("createRepo: %v", err)
	}
	if err := bed.gitea.putFile(ctx, owner, repo, "WORKFLOW.md", fixtureContent(t, fixture), "seed workflow"); err != nil {
		t.Fatalf("putFile workflow: %v", err)
	}
	issueNum, err := bed.gitea.createIssue(ctx, owner, repo, title, body)
	if err != nil {
		t.Fatalf("createIssue: %v", err)
	}
	if err := bed.gitea.ensureLabels(ctx, owner, repo, []string{"aiops/todo"}); err != nil {
		t.Fatalf("ensure label: %v", err)
	}
	if err := bed.gitea.addIssueLabels(ctx, owner, repo, issueNum, []string{"aiops/todo"}); err != nil {
		t.Fatalf("add label: %v", err)
	}

	cfg := workflow.DefaultConfig()
	cfg.Repo.Owner = owner
	cfg.Repo.Name = repo
	cfg.Repo.CloneURL = cloneURL
	cfg.Repo.DefaultBranch = "main"
	cfg.Tracker.Kind = "gitea"
	cfg.Tracker.APIKey = bed.gitea.botToken
	cfg.Tracker.ActiveStates = []string{"AI Ready"}
	cfg.Tracker.TerminalStates = []string{"Done", "Canceled"}
	serviceWorkflow, err := workflow.Load(writeE2EServiceWorkflow(t, string(fixtureContent(t, fixture)), cloneURL))
	if err != nil {
		t.Fatalf("load service workflow: %v", err)
	}
	client := gitea.NewTrackerClient(cfg.Tracker, bed.gitea.baseURL, owner, repo)
	client.HTTP = httpClientForE2E()

	events = &e2eEventRecorder{}
	dispatcher := orchestrator.WorkerTaskDispatcher{
		BuildTask: func(issue tracker.Issue) (task.Task, error) {
			tk, err := orchestrator.TaskFromIssue(issue, cfg)
			if err != nil {
				return task.Task{}, err
			}
			events.recordTask(tk)
			return tk, nil
		},
		Config: worker.Config{
			WorkspaceRoot: tmpDir(),
			MirrorRoot:    tmpDir(),
			Workflow:      serviceWorkflow,
		},
		Emitter: events,
	}
	orch := orchestrator.New(orchestrator.NewOrchestratorState(15000, 1), orchestrator.Deps{
		Dispatcher: dispatcher,
		Scheduler:  orchestrator.RetryScheduler{MaxBackoff: time.Minute},
	})
	orchCtx, orchCancel := context.WithCancel(ctx)
	t.Cleanup(orchCancel)
	go orch.Run(orchCtx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("start orchestrator: %v", err)
	}

	if err := orchestrator.NewPoller(client, orch).PollOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}

	pollUntil(t, 90*time.Second, 250*time.Millisecond, func(ctx context.Context) (bool, error) {
		tk, ok := events.taskBySource("gitea_issue", fmt.Sprintf("#%d", issueNum))
		if !ok {
			return false, nil
		}
		succeeded, err := events.taskSucceeded(tk.ID)
		if err != nil {
			return false, err
		}
		if !succeeded {
			return false, nil
		}
		taskID = tk.ID
		return true, nil
	})
	if taskID == "" {
		t.Fatalf("worker task did not complete")
	}
	return taskID, owner, repo, events
}

type e2eRecordedEvent struct {
	TaskID  string
	Kind    string
	Message string
	Payload any
}

type e2eEventRecorder struct {
	mu     sync.Mutex
	tasks  map[string]task.Task
	events []e2eRecordedEvent
}

func (r *e2eEventRecorder) recordTask(tk task.Task) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tasks == nil {
		r.tasks = map[string]task.Task{}
	}
	r.tasks[tk.ID] = tk
}

func (r *e2eEventRecorder) task(id string) task.Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tasks[id]
}

func (r *e2eEventRecorder) taskBySource(sourceType, sourceEventID string) (task.Task, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, tk := range r.tasks {
		if tk.SourceType == sourceType && tk.SourceEventID == sourceEventID {
			return tk, true
		}
	}
	return task.Task{}, false
}

func (r *e2eEventRecorder) byTask(taskID string) []e2eRecordedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []e2eRecordedEvent
	for _, ev := range r.events {
		if ev.TaskID == taskID {
			out = append(out, ev)
		}
	}
	return out
}

func (r *e2eEventRecorder) taskSucceeded(taskID string) (bool, error) {
	var sawSucceededPhase, sawRunnerEndOK bool
	for _, ev := range r.byTask(taskID) {
		switch ev.Kind {
		case task.EventRunPhaseTransition:
			to, ok := payloadString(ev.Payload, "to")
			if !ok {
				return false, fmt.Errorf("run phase transition missing string to payload: %#v", ev.Payload)
			}
			if to == string(task.PhaseSucceeded) {
				sawSucceededPhase = true
			}
		case task.EventRunnerEnd:
			okValue, ok := payloadBool(ev.Payload, "ok")
			if !ok {
				return false, fmt.Errorf("runner_end missing bool ok payload: %#v", ev.Payload)
			}
			if okValue {
				sawRunnerEndOK = true
			}
		}
	}
	return sawSucceededPhase && sawRunnerEndOK, nil
}

func payloadString(payload any, key string) (string, bool) {
	switch p := payload.(type) {
	case map[string]any:
		if v, ok := p[key].(string); ok {
			return v, true
		}
	case map[string]string:
		v, ok := p[key]
		return v, ok
	case task.PhaseTransition:
		if key == "to" {
			return string(p.To), p.To != ""
		}
		if key == "from" {
			return string(p.From), p.From != ""
		}
	}
	return "", false
}

func payloadBool(payload any, key string) (bool, bool) {
	if p, ok := payload.(map[string]any); ok {
		if v, ok := p[key].(bool); ok {
			return v, true
		}
	}
	return false, false
}

func (r *e2eEventRecorder) AddEvent(_ context.Context, taskID, kind, msg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e2eRecordedEvent{TaskID: taskID, Kind: kind, Message: msg})
	return nil
}

func (r *e2eEventRecorder) AddEventWithPayload(_ context.Context, taskID, kind, msg string, payload any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e2eRecordedEvent{TaskID: taskID, Kind: kind, Message: msg, Payload: payload})
	return nil
}

func writeE2EServiceWorkflow(t *testing.T, body, cloneURL string) string {
	t.Helper()
	body = strings.ReplaceAll(body, "http://localhost:3000/aiops-bot/demo-happy.git", cloneURL)
	body = strings.ReplaceAll(body, "http://localhost:3000/aiops-bot/demo-allow-fail.git", cloneURL)
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write service workflow: %v", err)
	}
	return path
}
