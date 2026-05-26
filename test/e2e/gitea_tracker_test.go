//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/orchestrator"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestGiteaTrackerDispatchesLabeledIssueAndDedupesRepeatedPolls(t *testing.T) {
	owner, repo := bed.gitea.botUser, "demo-gitea-tracker"
	cloneURL, err := bed.gitea.createRepo(context.Background(), repo)
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := bed.gitea.putFile(context.Background(), owner, repo, "WORKFLOW.md", fixtureContent(t, "gitea-worker.md"), "add workflow"); err != nil {
		t.Fatalf("put workflow: %v", err)
	}
	issueNumber, err := bed.gitea.createIssue(context.Background(), owner, repo, "poll me", "dispatch through polling")
	if err != nil {
		t.Fatalf("create issue: %v", err)
	}
	if err := bed.gitea.ensureLabels(context.Background(), owner, repo, []string{"aiops/todo"}); err != nil {
		t.Fatalf("ensure label: %v", err)
	}
	if err := bed.gitea.addIssueLabels(context.Background(), owner, repo, issueNumber, []string{"aiops/todo"}); err != nil {
		t.Fatalf("add label: %v", err)
	}

	client := gitea.NewTrackerClient(workflow.TrackerConfig{
		APIKey:         bed.gitea.botToken,
		ActiveStates:   []string{"AI Ready", "Rework"},
		TerminalStates: []string{"Done", "Canceled"},
	}, bed.gitea.baseURL, owner, repo)
	client.HTTP = httpClientForE2E()

	disp := &fakeE2EDispatcher{}
	orch := orchestrator.New(orchestrator.NewOrchestratorState(15000, 1), orchestrator.Deps{
		Dispatcher: disp,
		Scheduler:  orchestrator.RetryScheduler{MaxBackoff: time.Minute},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("start orchestrator: %v", err)
	}

	poller := orchestrator.NewPoller(client, orch)
	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	waitForE2E(t, func() bool { return disp.count() == 1 }, time.Second)
	got := disp.issueAt(0)
	if got.Identifier != "#1" || got.Title != "poll me" || got.State != "AI Ready" {
		t.Fatalf("unexpected dispatched issue: %+v", got)
	}
	if cloneURL == "" {
		t.Fatalf("fixture sanity check: repo clone URL should not be empty")
	}

	if err := poller.PollOnce(ctx); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := disp.count(); got != 1 {
		t.Fatalf("repeated poll should dedupe running issue, spawn count = %d", got)
	}
}

func TestGiteaTrackerIgnoresBacklogAndTerminalIssues(t *testing.T) {
	owner, repo := bed.gitea.botUser, "demo-gitea-tracker-filter"
	if _, err := bed.gitea.createRepo(context.Background(), repo); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if err := bed.gitea.putFile(context.Background(), owner, repo, "WORKFLOW.md", fixtureContent(t, "gitea-worker.md"), "add workflow"); err != nil {
		t.Fatalf("put workflow: %v", err)
	}
	backlogIssue, err := bed.gitea.createIssue(context.Background(), owner, repo, "backlog", "must not dispatch")
	if err != nil {
		t.Fatalf("create backlog issue: %v", err)
	}
	if err := bed.gitea.ensureLabels(context.Background(), owner, repo, []string{"aiops/backlog", "aiops/done"}); err != nil {
		t.Fatalf("ensure labels: %v", err)
	}
	if err := bed.gitea.addIssueLabels(context.Background(), owner, repo, backlogIssue, []string{"aiops/backlog"}); err != nil {
		t.Fatalf("label backlog issue: %v", err)
	}
	terminalIssue, err := bed.gitea.createIssue(context.Background(), owner, repo, "done", "must not dispatch")
	if err != nil {
		t.Fatalf("create terminal issue: %v", err)
	}
	if err := bed.gitea.addIssueLabels(context.Background(), owner, repo, terminalIssue, []string{"aiops/done"}); err != nil {
		t.Fatalf("label terminal issue: %v", err)
	}

	client := gitea.NewTrackerClient(workflow.TrackerConfig{
		APIKey:         bed.gitea.botToken,
		ActiveStates:   []string{"AI Ready", "Rework"},
		TerminalStates: []string{"Done", "Canceled"},
	}, bed.gitea.baseURL, owner, repo)
	client.HTTP = httpClientForE2E()

	disp := &fakeE2EDispatcher{}
	orch := orchestrator.New(orchestrator.NewOrchestratorState(15000, 1), orchestrator.Deps{
		Dispatcher: disp,
		Scheduler:  orchestrator.RetryScheduler{MaxBackoff: time.Minute},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go orch.Run(ctx)
	if err := orch.WaitStarted(ctx); err != nil {
		t.Fatalf("start orchestrator: %v", err)
	}

	if err := orchestrator.NewPoller(client, orch).PollOnce(ctx); err != nil {
		t.Fatalf("poll: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := disp.count(); got != 0 {
		t.Fatalf("backlog/terminal issues should not dispatch, spawn count = %d", got)
	}
}

type fakeE2EDispatcher struct {
	mu     sync.Mutex
	issues []tracker.Issue
}

func (f *fakeE2EDispatcher) Spawn(ctx context.Context, issue tracker.Issue, attempt *int) <-chan orchestrator.WorkerResult {
	f.mu.Lock()
	f.issues = append(f.issues, issue)
	f.mu.Unlock()
	return make(chan orchestrator.WorkerResult)
}

func (f *fakeE2EDispatcher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.issues)
}

func (f *fakeE2EDispatcher) issueAt(i int) tracker.Issue {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issues[i]
}

func waitForE2E(t *testing.T, pred func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !pred() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

func httpClientForE2E() *http.Client {
	return http.DefaultClient
}
