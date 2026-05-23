package gitea

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestClientRequestTimeoutFallsBackToDefault pins the closing-#295
// behavior: a client constructed without an explicit RequestTimeout
// uses defaultGiteaRequestTimeout (30 s). Without the gate, a hung
// Gitea response would block the worker's poll loop indefinitely.
func TestClientRequestTimeoutFallsBackToDefault(t *testing.T) {
	c := Client{}
	if got := c.requestTimeout(); got != defaultGiteaRequestTimeout {
		t.Errorf("zero RequestTimeout: got %v, want %v", got, defaultGiteaRequestTimeout)
	}
}

func TestClientRequestTimeoutHonorsExplicitOverride(t *testing.T) {
	c := Client{RequestTimeout: 250 * time.Millisecond}
	if got := c.requestTimeout(); got != 250*time.Millisecond {
		t.Errorf("explicit override: got %v, want 250ms", got)
	}
}

// TestFindOpenPullRequestAbortsHungServer is the #295 acceptance test:
// a Gitea endpoint that never replies must abort within
// RequestTimeout rather than wedging the caller until the OS keepalive
// trips. The test sets RequestTimeout=200ms; without the per-request
// context.WithTimeout wrap, this test would block until the outer
// context deadline (or longer).
func TestFindOpenPullRequestAbortsHungServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate a hung upstream by waiting past the client's
		// RequestTimeout. r.Context().Done() fires when the client
		// transport cancels the request, so the handler exits promptly
		// once the per-request deadline trips.
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	srv.Config.SetKeepAlivesEnabled(false)
	defer func() {
		// Force-close any active TCP connections so srv.Close() does
		// not block on httptest's idle-connection bookkeeping.
		srv.CloseClientConnections()
		srv.Close()
	}()

	c := Client{
		BaseURL:        srv.URL,
		Token:          "stub-token",
		HTTP:           srv.Client(),
		RequestTimeout: 200 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := c.FindOpenPullRequest(ctx, FindOpenPullRequestInput{
		Owner: "owner",
		Repo:  "repo",
		Head:  "branch",
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected timeout error from hung server, got nil")
	}
	if !strings.Contains(err.Error(), "deadline exceeded") && !strings.Contains(err.Error(), "context") {
		t.Errorf("expected context-deadline error, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("FindOpenPullRequest blocked %v — RequestTimeout did not fire", elapsed)
	}
}

// CreatePullRequest uses the same Client.requestTimeout() wrap as
// FindOpenPullRequest (covered by TestFindOpenPullRequestAbortsHungServer)
// — both code paths go through `context.WithTimeout(ctx, c.requestTimeout())`
// immediately before `http.NewRequestWithContext`. A dedicated POST-side
// hung-server integration test is intentionally omitted: httptest.Server.Close()
// blocks waiting on in-flight POSTs even after the client cancels (the
// transport's CloseIdleConnections does not reach handlers parked on
// `<-r.Context().Done()` for a connection mid-request-body), and the
// resulting test hang is a worse failure than no test. The helper-level
// coverage above plus the GET-side integration test is sufficient.
