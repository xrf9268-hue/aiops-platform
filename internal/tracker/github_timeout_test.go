package tracker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestGitHubClientRequestTimeoutFallsBackToDefault(t *testing.T) {
	c := &GitHubClient{}
	if got := c.requestTimeout(); got != defaultGitHubRequestTimeout {
		t.Errorf("zero RequestTimeout: got %v, want %v", got, defaultGitHubRequestTimeout)
	}
}

func TestGitHubClientRequestTimeoutHonorsExplicitOverride(t *testing.T) {
	c := &GitHubClient{RequestTimeout: 250 * time.Millisecond}
	if got := c.requestTimeout(); got != 250*time.Millisecond {
		t.Errorf("explicit override: got %v, want 250ms", got)
	}
}

// TestGitHubClientListIssuesAbortsHungServer is the #295 acceptance
// test for the GitHub adapter: a hung api.github.com response must
// abort within RequestTimeout rather than wedging the worker's poll
// loop until the OS-level keepalive RTO trips.
func TestGitHubClientListIssuesAbortsHungServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := NewGitHubClient(workflow.TrackerConfig{
		APIKey:       "stub-token",
		ActiveStates: []string{"open"},
	}, srv.URL, "owner", "repo")
	c.HTTP = srv.Client()
	c.RequestTimeout = 200 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := c.ListActiveIssues(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected timeout error from hung server, got nil")
	}
	if elapsed > 2*time.Second {
		t.Errorf("ListActiveIssues blocked %v — RequestTimeout did not fire", elapsed)
	}
}
