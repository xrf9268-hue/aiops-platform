package gitea

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func giteaRateLimitTestClient(t *testing.T, handler http.Handler) *TrackerClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, srv.URL, "owner", "repo")
	client.HTTP = srv.Client()
	return client
}

// assertGiteaRateLimited pins the typed 429 contract for this package's
// tracker client: classification via the shared tracker.ErrRateLimited
// sentinel and the exact parsed Retry-After extracted with errors.As.
func assertGiteaRateLimited(t *testing.T, err error, wantRetryAfter time.Duration) {
	t.Helper()
	if !errors.Is(err, tracker.ErrRateLimited) {
		t.Fatalf("err = %T %[1]v; want errors.Is(err, tracker.ErrRateLimited)", err)
	}
	var rateLimited *tracker.RateLimitedError
	if !errors.As(err, &rateLimited) {
		t.Fatalf("errors.As(%v, **tracker.RateLimitedError) = false; want true", err)
	}
	if rateLimited.RetryAfter != wantRetryAfter {
		t.Fatalf("RetryAfter = %v; want %v (err = %v)", rateLimited.RetryAfter, wantRetryAfter, err)
	}
}

func TestTrackerClientClassifiesRateLimitedResponses(t *testing.T) {
	listIssues := func(t *testing.T, handler http.HandlerFunc) error {
		t.Helper()
		_, err := giteaRateLimitTestClient(t, handler).ListIssuesByStates(context.Background(), []string{"Todo"})
		return err
	}

	t.Run("429 with delta-seconds retry-after", func(t *testing.T) {
		err := listIssues(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		})
		assertGiteaRateLimited(t, err, 30*time.Second)
	})

	t.Run("429 with http-date retry-after", func(t *testing.T) {
		err := listIssues(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", time.Now().Add(2*time.Minute).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusTooManyRequests)
		})
		if !errors.Is(err, tracker.ErrRateLimited) {
			t.Fatalf("err = %T %[1]v; want errors.Is(err, tracker.ErrRateLimited)", err)
		}
		var rateLimited *tracker.RateLimitedError
		if !errors.As(err, &rateLimited) {
			t.Fatalf("errors.As(%v, **tracker.RateLimitedError) = false; want true", err)
		}
		if rateLimited.RetryAfter <= time.Minute || rateLimited.RetryAfter > 2*time.Minute {
			t.Fatalf("RetryAfter = %v; want in (%v, %v] for an HTTP-date Retry-After", rateLimited.RetryAfter, time.Minute, 2*time.Minute)
		}
	})

	t.Run("429 without retry-after", func(t *testing.T) {
		err := listIssues(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		})
		assertGiteaRateLimited(t, err, 0)
	})

	t.Run("non-429 is not rate limited and carries the numeric status", func(t *testing.T) {
		err := listIssues(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		if errors.Is(err, tracker.ErrRateLimited) {
			t.Fatalf("err = %v; want HTTP 500 not classified as tracker.ErrRateLimited", err)
		}
		if want := "list Gitea issues failed: status 500"; err == nil || err.Error() != want {
			t.Fatalf("err = %v; want %q", err, want)
		}
	})

	t.Run("issue state refresh surfaces 429", func(t *testing.T) {
		client := giteaRateLimitTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/v1/repos/owner/repo/issues/7" {
				t.Fatalf("request path = %q; want the per-issue state refresh", r.URL.Path)
			}
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		_, err := client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "#7", Identifier: "#7"}})
		assertGiteaRateLimited(t, err, 30*time.Second)
	})
}
