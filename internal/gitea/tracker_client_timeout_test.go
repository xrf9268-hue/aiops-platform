package gitea

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestTrackerClientRequestTimeoutFallsBackToDefault(t *testing.T) {
	c := &TrackerClient{}
	if got := c.requestTimeout(); got != defaultGiteaRequestTimeout {
		t.Errorf("zero RequestTimeout: got %v, want %v", got, defaultGiteaRequestTimeout)
	}
}

func TestTrackerClientRequestTimeoutHonorsExplicitOverride(t *testing.T) {
	c := &TrackerClient{RequestTimeout: 250 * time.Millisecond}
	if got := c.requestTimeout(); got != 250*time.Millisecond {
		t.Errorf("explicit override: got %v, want 250ms", got)
	}
}

func TestTrackerClientFallbackHTTPClientUsesRequestTimeout(t *testing.T) {
	c := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, "https://gitea.example", "owner", "repo")
	c.RequestTimeout = 250 * time.Millisecond
	client := c.httpClient()
	if client == nil {
		t.Fatal("fallback HTTP client is nil")
	}
	if got := client.Timeout; got != 250*time.Millisecond {
		t.Fatalf("HTTP.Timeout = %v, want 250ms", got)
	}
}

func TestTrackerClientListIssuesAbortsHungServer(t *testing.T) {
	srv := hungGiteaServer(t)
	client := NewTrackerClient(workflow.TrackerConfig{
		APIKey:       "secret",
		ActiveStates: []string{"AI Ready"},
	}, srv.URL, "owner", "repo")
	client.HTTP = srv.Client()
	client.RequestTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := client.ListActiveIssues(ctx)
	assertRequestTimeout(t, "ListActiveIssues", err, time.Since(start))
}

func TestTrackerClientFetchIssueStateAbortsHungServer(t *testing.T) {
	srv := hungGiteaServer(t)
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, srv.URL, "owner", "repo")
	client.HTTP = srv.Client()
	client.RequestTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err := client.FetchIssueStatesByRefs(ctx, []tracker.IssueRef{{ID: "#1"}})
	assertRequestTimeout(t, "FetchIssueStatesByRefs", err, time.Since(start))
}

func hungGiteaServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	}))
	srv.Config.SetKeepAlivesEnabled(false)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
	})
	return srv
}

func assertRequestTimeout(t *testing.T, name string, err error, elapsed time.Duration) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected timeout error from hung server, got nil", name)
	}
	if !strings.Contains(err.Error(), "deadline exceeded") && !strings.Contains(err.Error(), "context") {
		t.Fatalf("%s: expected context-deadline error, got %v", name, err)
	}
	if elapsed > time.Second {
		t.Fatalf("%s blocked %v; RequestTimeout did not fire", name, elapsed)
	}
}
