//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
)

func TestWebhookDeliveryUUID_Deduped(t *testing.T) {
	testStart := time.Now()
	t.Cleanup(func() { bed.resetState(t, testStart) })

	payload := gitea.IssueCommentPayload{Action: "created"}
	payload.Repository.Name = "demo-dedup"
	payload.Repository.FullName = bed.gitea.botUser + "/demo-dedup"
	payload.Repository.CloneURL = "http://localhost:3000/" + bed.gitea.botUser + "/demo-dedup.git"
	payload.Repository.DefaultBranch = "main"
	payload.Issue.Number = 1
	payload.Issue.Title = "test"
	payload.Comment.ID = 9999
	payload.Comment.Body = "/ai-run"
	payload.Sender.Login = "tester"

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	delivery := "test-delivery-12345"

	post := func() (status int, parsed map[string]any) {
		req, _ := http.NewRequest("POST", bed.triggerSrv.URL+"/v1/events/gitea", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Gitea-Event", "issue_comment")
		req.Header.Set("X-Gitea-Delivery", delivery)
		req.Header.Set("X-Gitea-Signature", gitea.Sign(bed.secret, body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(raw, &parsed)
		return resp.StatusCode, parsed
	}

	st1, body1 := post()
	if st1 != http.StatusAccepted {
		t.Fatalf("first response status want 202 got %d body %v", st1, body1)
	}
	if body1["deduped"] != false {
		t.Fatalf("first response should be deduped:false; got %v", body1)
	}
	taskID1, _ := body1["task_id"].(string)
	if taskID1 == "" {
		t.Fatalf("first response missing task_id: %v", body1)
	}

	st2, body2 := post()
	if st2 != http.StatusAccepted {
		t.Fatalf("second response status want 202 got %d body %v", st2, body2)
	}
	if body2["deduped"] != true {
		t.Fatalf("second response should be deduped:true; got %v", body2)
	}
	if body2["task_id"] != taskID1 {
		t.Fatalf("dedup should reuse task id; got %v vs %v", body2["task_id"], taskID1)
	}

	ctx := context.Background()
	var n int
	if err := bed.pg.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tasks WHERE source_event_id=$1`, delivery).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("want exactly 1 task with source_event_id=%s, got %d", delivery, n)
	}
}
