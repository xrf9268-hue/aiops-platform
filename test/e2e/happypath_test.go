//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/orchestrator"
	"github.com/xrf9268-hue/aiops-platform/internal/queue"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestGiteaMockLoop_HappyPath(t *testing.T) {
	testStart := time.Now()
	t.Cleanup(func() { bed.resetState(t, testStart) })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	taskID, owner, repo := runGiteaPollerWorkerTask(t, ctx, "demo-happy", "first task", "Make a tiny change.", "mock-happy.md")

	events, err := queue.New(bed.pg.pool).TaskEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("TaskEvents: %v", err)
	}
	seen := map[string]bool{}
	for _, ev := range events {
		seen[ev.EventType] = true
	}
	for _, want := range []string{task.EventWorkflowResolved, task.EventRunnerStart, task.EventRunnerEnd} {
		if !seen[want] {
			t.Fatalf("missing event %s in %+v", want, events)
		}
	}

	var workBranch string
	if err := bed.pg.pool.QueryRow(ctx, `SELECT work_branch FROM tasks WHERE id=$1`, taskID).Scan(&workBranch); err != nil {
		t.Fatalf("query work_branch: %v", err)
	}
	if !regexp.MustCompile(`^ai/[0-9]+$`).MatchString(workBranch) {
		t.Fatalf("unexpected poller work branch %q", workBranch)
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

func runGiteaPollerWorkerTask(t *testing.T, ctx context.Context, repo, title, body, fixture string) (taskID, owner, repoName string) {
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

	store := queue.New(bed.pg.pool)
	dispatcher := orchestrator.WorkerTaskDispatcher{
		BuildRecordedTask: func(issue tracker.Issue) (orchestrator.BuiltTask, error) {
			tk, err := orchestrator.TaskFromIssue(issue, cfg)
			if err != nil {
				return orchestrator.BuiltTask{}, err
			}
			recorded, _, err := store.Enqueue(ctx, tk)
			if err != nil {
				return orchestrator.BuiltTask{}, fmt.Errorf("record task row: %w", err)
			}
			return orchestrator.BuiltTask{Task: tk, RecordedQueueID: recorded.ID}, nil
		},
		Config: worker.Config{
			WorkspaceRoot: tmpDir(),
			MirrorRoot:    tmpDir(),
			Workflow:      serviceWorkflow,
		},
		Emitter: store,
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
		row := bed.pg.pool.QueryRow(ctx, `SELECT id, status FROM tasks WHERE source_type=$1 AND source_event_id=$2`, "gitea_issue", fmt.Sprintf("#%d", issueNum))
		var id, status string
		if err := row.Scan(&id, &status); err != nil {
			if err == sql.ErrNoRows {
				return false, nil
			}
			return false, err
		}
		if status != string(task.StatusSucceeded) {
			return false, nil
		}
		taskID = id
		return true, nil
	})
	if taskID == "" {
		t.Fatalf("poller worker task did not complete")
	}
	return taskID, owner, repo
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
