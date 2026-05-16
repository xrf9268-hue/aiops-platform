//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

func TestVerifyAllowFailure(t *testing.T) {
	testStart := time.Now()
	t.Cleanup(func() { bed.resetState(t, testStart) })

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	repo := "demo-allow-fail"
	owner := bed.gitea.botUser

	if _, err := bed.gitea.createRepo(ctx, repo); err != nil {
		t.Fatalf("createRepo: %v", err)
	}
	if err := bed.gitea.putFile(ctx, owner, repo, "WORKFLOW.md",
		fixtureContent(t, "mock-allow-fail.md"), "seed workflow"); err != nil {
		t.Fatalf("putFile: %v", err)
	}
	if err := bed.gitea.createWebhook(ctx, owner, repo, bed.webhookURL, bed.secret); err != nil {
		t.Fatalf("createWebhook: %v", err)
	}
	issueNum, err := bed.gitea.createIssue(ctx, owner, repo, "allow-failure task", "Try.")
	if err != nil {
		t.Fatalf("createIssue: %v", err)
	}
	if err := bed.gitea.commentIssue(ctx, owner, repo, issueNum, "/ai-run"); err != nil {
		t.Fatalf("commentIssue: %v", err)
	}

	var taskID string
	pollUntil(t, 180*time.Second, 250*time.Millisecond, func(ctx context.Context) (bool, error) {
		row := bed.pg.pool.QueryRow(ctx,
			`SELECT id, status FROM tasks WHERE created_at >= $1 ORDER BY created_at DESC LIMIT 1`, testStart)
		var id, status string
		if err := row.Scan(&id, &status); err != nil {
			return false, nil
		}
		if status != string(task.StatusSucceeded) {
			return false, nil
		}
		taskID = id
		return true, nil
	})

	var foundFailedAllowed bool
	rows, err := bed.pg.pool.Query(ctx,
		`SELECT payload FROM task_events WHERE task_id=$1 AND event_type=$2`,
		taskID, task.EventVerifyEnd)
	if err != nil {
		t.Fatalf("query verify_end: %v", err)
	}
	for rows.Next() {
		var payload string
		_ = rows.Scan(&payload)
		if strings.Contains(payload, "failed_allowed") {
			foundFailedAllowed = true
			break
		}
	}
	rows.Close()
	if !foundFailedAllowed {
		t.Errorf("expected verify_end event with failed_allowed status for task %s", taskID)
	}

	prs, err := bed.gitea.listOpenPRs(ctx, owner, repo)
	if err != nil {
		t.Fatalf("listOpenPRs: %v", err)
	}
	if len(prs) != 0 {
		t.Fatalf("worker must not open PRs; got %d open PR(s): %+v", len(prs), prs)
	}
}
