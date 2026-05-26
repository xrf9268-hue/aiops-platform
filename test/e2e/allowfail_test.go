//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

func TestVerifyAllowFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	taskID, owner, repo, events := runGiteaWorkerTask(t, ctx, "demo-allow-fail", "allow-failure task", "Try.", "mock-allow-fail.md")

	var foundFailedAllowed bool
	for _, ev := range events.byTask(taskID) {
		if ev.Kind != task.EventVerifyEnd {
			continue
		}
		status, ok := payloadString(ev.Payload, "status")
		if !ok {
			t.Fatalf("verify_end event payload missing string status: %#v", ev.Payload)
		}
		if status == "failed_allowed" {
			foundFailedAllowed = true
			break
		}
	}
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
