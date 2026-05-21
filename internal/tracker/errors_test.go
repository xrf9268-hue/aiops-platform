package tracker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestLinearClientSurfacesTrackerErrorCategories(t *testing.T) {
	t.Run("missing api key", func(t *testing.T) {
		_, err := NewLinearClient(workflow.TrackerConfig{ProjectSlug: "aiops"}).ListIssuesByStates(context.Background(), []string{"Todo"})
		if !errors.Is(err, ErrMissingTrackerAPIKey) {
			t.Fatalf("ListIssuesByStates missing key error = %T %[1]v, want ErrMissingTrackerAPIKey", err)
		}
	})

	t.Run("missing project slug", func(t *testing.T) {
		_, err := NewLinearClient(workflow.TrackerConfig{APIKey: "key"}).ListIssuesByStates(context.Background(), []string{"Todo"})
		if !errors.Is(err, ErrMissingTrackerProjectSlug) {
			t.Fatalf("ListIssuesByStates missing slug error = %T %[1]v, want ErrMissingTrackerProjectSlug", err)
		}
	})

	t.Run("graphql errors", func(t *testing.T) {
		client := linearTestClientForCategory(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"errors":[{"message":"bad token"}]}`)
		}))
		_, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
		if !errors.Is(err, ErrLinearGraphQLErrors) {
			t.Fatalf("ListIssuesByStates GraphQL error = %T %[1]v, want ErrLinearGraphQLErrors", err)
		}
	})

	t.Run("non-200 status", func(t *testing.T) {
		client := linearTestClientForCategory(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope", http.StatusBadGateway)
		}))
		_, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
		if !errors.Is(err, ErrLinearAPIStatus) {
			t.Fatalf("ListIssuesByStates status error = %T %[1]v, want ErrLinearAPIStatus", err)
		}
	})

	t.Run("unknown payload", func(t *testing.T) {
		client := linearTestClientForCategory(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{`)
		}))
		_, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
		if !errors.Is(err, ErrLinearUnknownPayload) {
			t.Fatalf("ListIssuesByStates payload error = %T %[1]v, want ErrLinearUnknownPayload", err)
		}
	})

	t.Run("missing end cursor", func(t *testing.T) {
		client := linearTestClientForCategory(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"data":{"issues":{"nodes":[],"pageInfo":{"hasNextPage":true,"endCursor":""}}}}`)
		}))
		_, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
		if !errors.Is(err, ErrLinearMissingEndCursor) {
			t.Fatalf("ListIssuesByStates cursor error = %T %[1]v, want ErrLinearMissingEndCursor", err)
		}
		if got, ok := ErrorCategory(err); !ok || got != CategoryLinearMissingEndCursor {
			t.Fatalf("ErrorCategory = %q, %v; want %q, true", got, ok, CategoryLinearMissingEndCursor)
		}
	})
}

func TestTrackerSentinelsCoverSpecCategories(t *testing.T) {
	err := NewError(CategoryUnsupportedTrackerKind, "unsupported tracker.kind jira", nil)
	if !errors.Is(err, ErrUnsupportedTrackerKind) {
		t.Fatalf("unsupported tracker error = %T %[1]v, want ErrUnsupportedTrackerKind", err)
	}
	err = NewError(CategoryLinearAPIRequest, "request failed", context.Canceled)
	if !errors.Is(err, ErrLinearAPIRequest) {
		t.Fatalf("request error = %T %[1]v, want ErrLinearAPIRequest", err)
	}
}

func linearTestClientForCategory(t *testing.T, handler http.Handler) *LinearClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := NewLinearClient(workflow.TrackerConfig{APIKey: "key", ProjectSlug: "aiops"})
	client.BaseURL = srv.URL
	client.HTTP = srv.Client()
	return client
}
