//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"
)

func TestWebhookBadSignature(t *testing.T) {
	testStart := time.Now()
	t.Cleanup(func() { bed.resetState(t, testStart) })

	body := []byte(`{"action":"created"}`)
	req, err := http.NewRequest("POST", bed.triggerSrv.URL+"/v1/events/gitea", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitea-Event", "issue_comment")
	req.Header.Set("X-Gitea-Delivery", "deadbeef")
	req.Header.Set("X-Gitea-Signature", "sha256=00000000000000000000000000000000")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}

	ctx := context.Background()
	var n int
	if err := bed.pg.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM tasks WHERE created_at >= $1`, testStart).Scan(&n); err != nil {
		t.Fatalf("count tasks: %v", err)
	}
	if n != 0 {
		t.Errorf("bad-signature post should not enqueue; got %d tasks", n)
	}
}
