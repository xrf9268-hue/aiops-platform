package tracker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"delta seconds", "30", 30 * time.Second},
		{"delta seconds with whitespace", " 30 ", 30 * time.Second},
		{"http date in the future", now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{"http date already elapsed", now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"missing header", "", 0},
		{"unparseable value", "soon", 0},
		{"negative delta", "-5", 0},
		{"overflowing delta saturates", "10000000000", maxRetryAfter},
		{"delta beyond int64 saturates", "99999999999999999999999", maxRetryAfter},
		{"negative delta beyond int64", "-99999999999999999999999", 0},
		{"zero delta", "0", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfter(tc.value, now); got != tc.want {
				t.Fatalf("parseRetryAfter(%q) = %v; want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestParseRateLimitReset(t *testing.T) {
	now := time.Date(2026, 6, 12, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"future epoch", "1781258490", 90 * time.Second}, // now is epoch 1781258400
		{"elapsed epoch", "1781258340", 0},
		{"missing header", "", 0},
		{"unparseable value", "soon", 0},
		{"zero epoch", "0", 0},
		{"negative epoch", "-1", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRateLimitReset(tc.value, now); got != tc.want {
				t.Fatalf("parseRateLimitReset(%q) = %v; want %v", tc.value, got, tc.want)
			}
		})
	}
}

// TestGitHubSecondaryLimitBody pins each discriminator of the documented
// headerless secondary-limit payload independently: the message branch, the
// documentation_url branch, and the case fold. The endpoint-level fixture
// carries both signals at once, so deleting a single branch (or the fold)
// would survive it — these rows are what kill those mutations.
func TestGitHubSecondaryLimitBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"message only", `{"message":"You have exceeded a secondary rate limit. Please wait."}`, true},
		{"documentation_url only", `{"message":"Forbidden","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`, true},
		{"mixed-case message", `{"message":"You have exceeded a Secondary Rate Limit, please slow down."}`, true},
		{"unrelated message", `{"message":"Resource not accessible by integration"}`, false},
		{"non-JSON body", `<html>Forbidden</html>`, false},
		{"empty body", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := githubSecondaryLimitBody(strings.NewReader(tc.body)); got != tc.want {
				t.Fatalf("githubSecondaryLimitBody(%q) = %v; want %v", tc.body, got, tc.want)
			}
		})
	}
}

// assertRateLimited pins the typed 429 contract: errors.Is classification via
// ErrRateLimited and the exact parsed Retry-After extracted with errors.As.
func assertRateLimited(t *testing.T, err error, wantRetryAfter time.Duration) {
	t.Helper()
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %T %[1]v; want errors.Is(err, ErrRateLimited)", err)
	}
	var rateLimited *RateLimitedError
	if !errors.As(err, &rateLimited) {
		t.Fatalf("errors.As(%v, **RateLimitedError) = false; want true", err)
	}
	if rateLimited.RetryAfter != wantRetryAfter {
		t.Fatalf("RetryAfter = %v; want %v (err = %v)", rateLimited.RetryAfter, wantRetryAfter, err)
	}
}

// assertRateLimitedHTTPDate asserts classification for an HTTP-date
// Retry-After: the parsed duration is computed against the wall clock, so it
// is asserted as a (lower, upper] window around the expected delta instead of
// an exact value.
func assertRateLimitedHTTPDate(t *testing.T, err error, lower, upper time.Duration) {
	t.Helper()
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %T %[1]v; want errors.Is(err, ErrRateLimited)", err)
	}
	var rateLimited *RateLimitedError
	if !errors.As(err, &rateLimited) {
		t.Fatalf("errors.As(%v, **RateLimitedError) = false; want true", err)
	}
	if rateLimited.RetryAfter <= lower || rateLimited.RetryAfter > upper {
		t.Fatalf("RetryAfter = %v; want in (%v, %v] for an HTTP-date Retry-After", rateLimited.RetryAfter, lower, upper)
	}
}

func TestLinearClientClassifiesRateLimitedResponses(t *testing.T) {
	listIssues := func(t *testing.T, handler http.HandlerFunc) error {
		t.Helper()
		_, err := linearTestClientForCategory(t, handler).ListIssuesByStates(context.Background(), []string{"Todo"})
		return err
	}

	t.Run("429 with delta-seconds retry-after", func(t *testing.T) {
		err := listIssues(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		})
		assertRateLimited(t, err, 30*time.Second)
	})

	t.Run("429 with http-date retry-after", func(t *testing.T) {
		err := listIssues(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", time.Now().Add(2*time.Minute).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusTooManyRequests)
		})
		assertRateLimitedHTTPDate(t, err, time.Minute, 2*time.Minute)
	})

	t.Run("429 without retry-after", func(t *testing.T) {
		err := listIssues(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		})
		assertRateLimited(t, err, 0)
	})

	t.Run("non-429 is not rate limited and carries the numeric status", func(t *testing.T) {
		err := listIssues(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		if errors.Is(err, ErrRateLimited) {
			t.Fatalf("err = %v; want HTTP 500 not classified as ErrRateLimited", err)
		}
		if want := "linear request failed: status 500"; err == nil || err.Error() != want {
			t.Fatalf("err = %v; want %q", err, want)
		}
	})
}

func githubRateLimitTestClient(t *testing.T, handler http.Handler) *GitHubClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := NewGitHubClient(workflow.TrackerConfig{APIKey: "test-token"}, srv.URL, "acme", "api")
	client.HTTP = srv.Client()
	return client
}

func TestGitHubClientClassifiesRateLimitedResponses(t *testing.T) {
	// listClosedIssues exercises listIssuesPage without the open-PR claims
	// scan (a "closed" state never requires it), so the handler sees exactly
	// one issues-listing request.
	listClosedIssues := func(t *testing.T, handler http.HandlerFunc) error {
		t.Helper()
		_, err := githubRateLimitTestClient(t, handler).ListIssuesByStates(context.Background(), []string{"closed"})
		return err
	}

	t.Run("429 with delta-seconds retry-after", func(t *testing.T) {
		err := listClosedIssues(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		})
		assertRateLimited(t, err, 30*time.Second)
	})

	t.Run("429 with http-date retry-after", func(t *testing.T) {
		err := listClosedIssues(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", time.Now().Add(2*time.Minute).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusTooManyRequests)
		})
		assertRateLimitedHTTPDate(t, err, time.Minute, 2*time.Minute)
	})

	t.Run("429 without retry-after", func(t *testing.T) {
		err := listClosedIssues(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		})
		assertRateLimited(t, err, 0)
	})

	t.Run("non-429 is not rate limited and carries the numeric status", func(t *testing.T) {
		err := listClosedIssues(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		if errors.Is(err, ErrRateLimited) {
			t.Fatalf("err = %v; want HTTP 500 not classified as ErrRateLimited", err)
		}
		if want := "list GitHub issues failed: status 500"; err == nil || err.Error() != want {
			t.Fatalf("err = %v; want %q", err, want)
		}
	})

	// GitHub's documented primary/secondary rate limits surface as 403 as
	// well as 429 (codex P2 on PR #768): exhausted X-RateLimit-Remaining or a
	// Retry-After header distinguishes them from ordinary permission 403s.
	t.Run("403 primary limit with exhausted remaining and reset hint", func(t *testing.T) {
		err := listClosedIssues(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(2*time.Minute).Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
		})
		assertRateLimitedHTTPDate(t, err, time.Minute, 2*time.Minute)
	})

	t.Run("403 secondary limit with retry-after", func(t *testing.T) {
		err := listClosedIssues(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusForbidden)
		})
		assertRateLimited(t, err, 30*time.Second)
	})

	t.Run("403 headerless secondary limit classified by documented body", func(t *testing.T) {
		err := listClosedIssues(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`))
		})
		// No header and no machine-readable wait in the body: the response
		// carries no hint, so RetryAfter stays zero while the classification
		// holds.
		assertRateLimited(t, err, 0)
	})

	t.Run("403 with unrelated body message stays a generic status error", func(t *testing.T) {
		err := listClosedIssues(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration","documentation_url":"https://docs.github.com/rest"}`))
		})
		if errors.Is(err, ErrRateLimited) {
			t.Fatalf("err = %v; want a permission 403 with unrelated body not classified as ErrRateLimited", err)
		}
	})

	t.Run("403 with remaining quota stays a generic status error", func(t *testing.T) {
		err := listClosedIssues(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-RateLimit-Remaining", "41")
			w.WriteHeader(http.StatusForbidden)
		})
		if errors.Is(err, ErrRateLimited) {
			t.Fatalf("err = %v; want a 403 with remaining quota not classified as ErrRateLimited", err)
		}
	})

	t.Run("plain 403 stays a generic status error", func(t *testing.T) {
		err := listClosedIssues(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		})
		if errors.Is(err, ErrRateLimited) {
			t.Fatalf("err = %v; want a permission 403 (no rate-limit headers) not classified as ErrRateLimited", err)
		}
		if want := "list GitHub issues failed: status 403"; err == nil || err.Error() != want {
			t.Fatalf("err = %v; want %q", err, want)
		}
	})

	t.Run("403 rate limit reports its real status code", func(t *testing.T) {
		err := listClosedIssues(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusForbidden)
		})
		var typed *Error
		if !errors.As(err, &typed) {
			t.Fatalf("errors.As(%v, **Error) = false; want typed tracker error", err)
		}
		if want := "list GitHub issues failed: status 403"; typed.Message != want {
			t.Fatalf("typed.Message = %q; want %q (the message must not claim 429 for a 403 limit)", typed.Message, want)
		}
	})

	t.Run("open pull request claims scan surfaces 429", func(t *testing.T) {
		client := githubRateLimitTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/acme/api/pulls" {
				t.Fatalf("request path = %q; want only the open-PR claims scan before the 429 aborts the listing", r.URL.Path)
			}
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		_, err := client.ListIssuesByStates(context.Background(), []string{"open"})
		assertRateLimited(t, err, 30*time.Second)
	})

	// The 403 shape must classify on every GitHub site, not only the issue
	// listing: each remaining endpoint gets its own 403 subtest so reverting
	// any single site to a 429-only check fails here.
	t.Run("open pull request claims scan surfaces 403 rate limit", func(t *testing.T) {
		client := githubRateLimitTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/acme/api/pulls" {
				t.Fatalf("request path = %q; want only the open-PR claims scan before the 403 aborts the listing", r.URL.Path)
			}
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusForbidden)
		}))
		_, err := client.ListIssuesByStates(context.Background(), []string{"open"})
		assertRateLimited(t, err, 30*time.Second)
	})

	t.Run("issue state refresh surfaces 429", func(t *testing.T) {
		client := githubRateLimitTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/acme/api/issues/1" {
				t.Fatalf("request path = %q; want the per-issue state refresh", r.URL.Path)
			}
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		}))
		_, err := client.FetchIssueStatesByRefs(context.Background(), []IssueRef{{ID: "500", Identifier: "#1"}})
		assertRateLimited(t, err, 30*time.Second)
	})

	t.Run("issue state refresh surfaces 403 rate limit", func(t *testing.T) {
		client := githubRateLimitTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/repos/acme/api/issues/1" {
				t.Fatalf("request path = %q; want the per-issue state refresh", r.URL.Path)
			}
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
		}))
		_, err := client.FetchIssueStatesByRefs(context.Background(), []IssueRef{{ID: "500", Identifier: "#1"}})
		assertRateLimited(t, err, 0)
	})
}
