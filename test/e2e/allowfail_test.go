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

	taskID, owner, repo := runGiteaPollerWorkerTask(t, ctx, "demo-allow-fail", "allow-failure task", "Try.", "mock-allow-fail.md")

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
