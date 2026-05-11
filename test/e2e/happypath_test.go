//go:build e2e

package e2e

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

func TestGiteaMockLoop_HappyPath(t *testing.T) {
	testStart := time.Now()
	t.Cleanup(func() { bed.resetState(t, testStart) })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	repo := "demo-happy"
	owner := bed.gitea.botUser

	if _, err := bed.gitea.createRepo(ctx, repo); err != nil {
		t.Fatalf("createRepo: %v", err)
	}
	if err := bed.gitea.putFile(ctx, owner, repo, "WORKFLOW.md",
		fixtureContent(t, "mock-happy.md"), "seed workflow"); err != nil {
		t.Fatalf("putFile workflow: %v", err)
	}
	if err := bed.gitea.createWebhook(ctx, owner, repo, bed.webhookURL, bed.secret); err != nil {
		t.Fatalf("createWebhook: %v", err)
	}
	issueNum, err := bed.gitea.createIssue(ctx, owner, repo, "first task", "Make a tiny change.")
	if err != nil {
		t.Fatalf("createIssue: %v", err)
	}

	if err := bed.gitea.commentIssue(ctx, owner, repo, issueNum, "/ai-run"); err != nil {
		t.Fatalf("commentIssue: %v", err)
	}

	var workBranch string
	pollUntil(t, 180*time.Second, 250*time.Millisecond, func(ctx context.Context) (bool, error) {
		row := bed.pg.pool.QueryRow(ctx,
			`SELECT id, status, work_branch FROM tasks WHERE created_at >= $1 ORDER BY created_at DESC LIMIT 1`,
			testStart)
		var id, status, branch string
		if err := row.Scan(&id, &status, &branch); err != nil {
			return false, nil
		}
		if status != string(task.StatusSucceeded) {
			return false, nil
		}
		workBranch = branch
		return true, nil
	})

	if !regexp.MustCompile(`^ai/tsk_`).MatchString(workBranch) {
		t.Fatalf("work_branch %q does not match ^ai/tsk_", workBranch)
	}

	taskID := func() string {
		row := bed.pg.pool.QueryRow(ctx,
			`SELECT id FROM tasks WHERE created_at >= $1 ORDER BY created_at DESC LIMIT 1`, testStart)
		var id string
		_ = row.Scan(&id)
		return id
	}()
	wantEvents := []string{
		task.EventWorkflowResolved,
		task.EventRunnerStart,
		task.EventPRCreated,
	}
	for _, want := range wantEvents {
		var n int
		if err := bed.pg.pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM task_events WHERE task_id=$1 AND event_type=$2`,
			taskID, want).Scan(&n); err != nil {
			t.Fatalf("count event %s: %v", want, err)
		}
		if n == 0 {
			t.Errorf("expected at least one %q event for task %s", want, taskID)
		}
	}

	exists, err := bed.gitea.getBranch(ctx, owner, repo, workBranch)
	if err != nil || !exists {
		t.Fatalf("getBranch %s: exists=%v err=%v", workBranch, exists, err)
	}

	prs, err := bed.gitea.listOpenPRs(ctx, owner, repo)
	if err != nil {
		t.Fatalf("listOpenPRs: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("want 1 open PR, got %d: %+v", len(prs), prs)
	}
	pr := prs[0]
	if !pr.Draft {
		t.Errorf("PR should be draft (workflow says draft:true); got draft=%v title=%q", pr.Draft, pr.Title)
	}
	if !strings.Contains(pr.Body, ".aiops/") {
		t.Errorf("PR body should reference .aiops/ artifacts; got: %s", pr.Body)
	}
}
