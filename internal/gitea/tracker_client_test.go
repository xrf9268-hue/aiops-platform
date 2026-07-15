package gitea

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type giteaPathErrorTransport struct {
	base http.RoundTripper
	path string
	err  error
}

func (t giteaPathErrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Path == t.path {
		return nil, t.err
	}
	return t.base.RoundTrip(req)
}

func TestTrackerClientSatisfiesIssueStateRefresher(t *testing.T) {
	var _ tracker.IssueStateRefresher = (*TrackerClient)(nil)
}

func TestTrackerClientListIssuesByStatesReturnsNoIssuesWhenNoStatesRequested(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Issue{
			{ID: 100, Number: 100, Title: "todo", HTMLURL: "https://gitea.local/o/r/issues/100", Labels: []Label{{Name: "aiops/todo"}}},
		})
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	issues, err := client.ListIssuesByStates(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 0 {
		t.Fatalf("issues = %#v, want no issues for an empty workflow state filter", issues)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want no unfiltered Gitea request for empty states", requests)
	}
}

func TestTrackerClientListIssuesByStatesMapsAIOpsLabels(t *testing.T) {
	var requestedLabels []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedLabels = append(requestedLabels, r.URL.Query().Get("labels"))
		if r.Header.Get("Authorization") != "token secret" {
			t.Fatalf("Authorization = %q, want token secret", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/api/v1/repos/owner/repo/issues" {
			t.Fatalf("requested path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("labels") == "aiops/todo" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]Issue{
				{ID: 101, Number: 1, Title: "first", Body: "body", HTMLURL: "https://gitea.local/o/r/issues/1", CreatedAt: "2026-05-16T23:59:00Z", UpdatedAt: "2026-05-17T00:00:00Z", Labels: []Label{{Name: "aiops/todo"}}},
			})
			return
		}
		if r.URL.Query().Get("labels") == "aiops/rework" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]Issue{
				{ID: 102, Number: 2, Title: "second", Body: "body", HTMLURL: "https://gitea.local/o/r/issues/2", CreatedAt: "2026-05-17T00:00:30Z", UpdatedAt: "2026-05-17T00:01:00Z", Labels: []Label{{Name: "aiops/rework"}}},
			})
			return
		}
		t.Fatalf("labels query = %q, want one active aiops label per request", r.URL.Query().Get("labels"))
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{
		APIKey:       "secret",
		ActiveStates: []string{"Todo", "Rework"},
	}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	issues, err := client.ListActiveIssues(context.Background())
	if err != nil {
		t.Fatalf("ListActiveIssues: %v", err)
	}
	if !slices.Equal(requestedLabels, []string{"aiops/todo", "aiops/rework"}) {
		t.Fatalf("labels queries = %#v, want one request per active aiops label", requestedLabels)
	}
	if len(issues) != 2 {
		t.Fatalf("issues len = %d, want 2", len(issues))
	}
	if issues[0].ID != "101" || issues[0].Identifier != "#1" || issues[0].State != "Todo" {
		t.Fatalf("first issue = %#v, want mapped Todo state", issues[0])
	}
	if issues[1].State != "Rework" {
		t.Fatalf("second issue state = %q, want Rework", issues[1].State)
	}
	if !issues[0].CreatedAt.Equal(mustTime("2026-05-16T23:59:00Z")) || !issues[1].CreatedAt.Equal(mustTime("2026-05-17T00:00:30Z")) {
		t.Fatalf("issue created_at = %s, %s; want Gitea created_at metadata mapped", issues[0].CreatedAt, issues[1].CreatedAt)
	}
}

func TestTrackerClientFetchIssueStatesByIDsUsesCachedIssueNumbers(t *testing.T) {
	var requestedPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPaths = append(requestedPaths, r.URL.Path)
		if r.Header.Get("Authorization") != "token secret" {
			t.Fatalf("Authorization = %q, want token secret", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues":
			if r.URL.Query().Get("labels") != "aiops/todo" {
				t.Fatalf("labels query = %q, want aiops/todo", r.URL.Query().Get("labels"))
			}
			_ = json.NewEncoder(w).Encode([]Issue{
				{ID: 101, Number: 1, Title: "running", HTMLURL: "https://gitea.local/o/r/issues/1", Labels: []Label{{Name: "aiops/todo"}}},
				{ID: 202, Number: 2, Title: "missing later", HTMLURL: "https://gitea.local/o/r/issues/2", Labels: []Label{{Name: "aiops/todo"}}},
			})
		case "/api/v1/repos/owner/repo/issues/1":
			_ = json.NewEncoder(w).Encode(Issue{
				ID: 101, Number: 1, Title: "done", HTMLURL: "https://gitea.local/o/r/issues/1", Labels: []Label{{Name: "aiops/done"}, {Name: "Aiops-Ready"}},
			})
		case "/api/v1/repos/owner/repo/issues/2":
			http.NotFound(w, r)
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{
		APIKey:       "secret",
		ActiveStates: []string{"Todo"},
	}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	if _, err := client.ListActiveIssues(context.Background()); err != nil {
		t.Fatalf("ListActiveIssues: %v", err)
	}
	states, err := client.FetchIssueStatesByIDs(context.Background(), []string{"101", "202"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	if len(states) != 2 || states["101"].Outcome != tracker.IssueStateOutcomeCurrent || states["101"].State != "Done" || states["202"].Outcome != tracker.IssueStateOutcomeAbsent {
		t.Fatalf("states = %#v; want current 101 and absent 202", states)
	}
	if got, want := states["101"].Labels, []string{"aiops/done", "aiops-ready"}; !slices.Equal(got, want) {
		t.Fatalf("FetchIssueStatesByIDs(101).Labels = %v; want %v", got, want)
	}
	if got, want := strings.Join(requestedPaths, ","), "/api/v1/repos/owner/repo/issues,/api/v1/repos/owner/repo/issues/1,/api/v1/repos/owner/repo/issues/2"; got != want {
		t.Fatalf("requested paths = %s, want %s", got, want)
	}
}

func TestTrackerClientFetchIssueStatesByRefsOutcomeMatrixAndPartialError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues/1":
			_ = json.NewEncoder(w).Encode(Issue{ID: 101, Number: 1, Labels: []Label{{Name: "aiops/done"}}})
		case "/api/v1/repos/owner/repo/issues/2":
			http.NotFound(w, r)
		case "/api/v1/repos/owner/repo/issues/3":
			_ = json.NewEncoder(w).Encode(Issue{ID: 999, Number: 3, Labels: []Label{{Name: "aiops/todo"}}})
		case "/api/v1/repos/owner/repo/issues/4":
			_ = json.NewEncoder(w).Encode(Issue{ID: 404, Number: 4, Labels: []Label{{Name: "ready"}}})
		case "/api/v1/repos/owner/repo/issues/5":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/api/v1/repos/owner/repo/issues/6":
			_ = json.NewEncoder(w).Encode(Issue{ID: 606, Number: 6, Labels: []Label{{Name: "aiops/done"}}})
		case "/api/v1/repos/owner/repo/issues/7":
			http.NotFound(w, r)
		default:
			t.Fatalf("request path = %q; want one of issue endpoints #1 through #7", r.URL.Path)
		}
	}))
	defer server.Close()
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	client.issueNumbers.Store("202", 2)
	refs := []tracker.IssueRef{
		{ID: "101", Identifier: "#1"},
		{ID: "202", Identifier: "#2"},
		{ID: "opaque"},
		{ID: "#bad"},
		{ID: "303", Identifier: "#3"},
		{ID: "404", Identifier: "#4"},
		{ID: "505", Identifier: "#5"},
		{ID: "606", Identifier: "#6"},
		{ID: "707", Identifier: "#7"},
		{ID: "101", Identifier: "#1"},
		{},
	}

	states, err := client.FetchIssueStatesByRefs(context.Background(), refs)
	if err == nil {
		t.Fatalf("FetchIssueStatesByRefs error = %v; want HTTP 500", err)
	}
	want := map[string]tracker.IssueStateOutcome{
		"101":    tracker.IssueStateOutcomeCurrent,
		"202":    tracker.IssueStateOutcomeAbsent,
		"opaque": tracker.IssueStateOutcomeUnknown,
		"#bad":   tracker.IssueStateOutcomeUnknown,
		"303":    tracker.IssueStateOutcomeUnknown,
		"404":    tracker.IssueStateOutcomeAbsent,
		"505":    tracker.IssueStateOutcomeUnknown,
		"606":    tracker.IssueStateOutcomeCurrent,
		"707":    tracker.IssueStateOutcomeUnknown,
	}
	if len(states) != len(want) {
		t.Fatalf("states len = %d; want %d: %#v", len(states), len(want), states)
	}
	for id, outcome := range want {
		if got := states[id].Outcome; got != outcome {
			t.Fatalf("states[%q].Outcome = %v; want %v (row=%#v)", id, got, outcome, states[id])
		}
	}
}

func TestTrackerClientFetchIssueStatesPayloadNumberMismatchStaysUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Issue{ID: 101, Number: 2, Labels: []Label{{Name: "aiops/done"}}})
	}))
	defer server.Close()
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	client.issueNumbers.Store("101", 1)

	states, err := client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "101", Identifier: "#1"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs() error = %v; want nil", err)
	}
	if got := states["101"].Outcome; got != tracker.IssueStateOutcomeUnknown {
		t.Fatalf("outcome = %v; want unknown for payload number mismatch", got)
	}
	if number, ok := client.cachedIssueNumber("101"); !ok || number != 1 {
		t.Fatalf("cached issue number = %d, %v; want original 1 preserved", number, ok)
	}
}

func TestTrackerClientFetchIssueStatesPartialPayloadAndEmptyLabels(t *testing.T) {
	tests := []struct {
		name        string
		payload     string
		wantOutcome tracker.IssueStateOutcome
		wantErr     bool
	}{
		{name: "missing id", payload: `{"number":1,"body":"","labels":[]}`, wantOutcome: tracker.IssueStateOutcomeUnknown, wantErr: true},
		{name: "zero id", payload: `{"id":0,"number":1,"body":"","labels":[]}`, wantOutcome: tracker.IssueStateOutcomeUnknown, wantErr: true},
		{name: "missing number", payload: `{"id":101,"body":"","labels":[]}`, wantOutcome: tracker.IssueStateOutcomeUnknown, wantErr: true},
		{name: "zero number", payload: `{"id":101,"number":0,"body":"","labels":[]}`, wantOutcome: tracker.IssueStateOutcomeUnknown, wantErr: true},
		{name: "missing labels", payload: `{"id":101,"number":1,"body":""}`, wantOutcome: tracker.IssueStateOutcomeUnknown, wantErr: true},
		{name: "null labels", payload: `{"id":101,"number":1,"body":"","labels":null}`, wantOutcome: tracker.IssueStateOutcomeUnknown, wantErr: true},
		{name: "label missing name", payload: `{"id":101,"number":1,"body":"","labels":[{}]}`, wantOutcome: tracker.IssueStateOutcomeUnknown, wantErr: true},
		{name: "label null name", payload: `{"id":101,"number":1,"body":"","labels":[{"name":null}]}`, wantOutcome: tracker.IssueStateOutcomeUnknown, wantErr: true},
		{name: "label empty name", payload: `{"id":101,"number":1,"body":"","labels":[{"name":"  "}]}`, wantOutcome: tracker.IssueStateOutcomeUnknown, wantErr: true},
		{name: "empty labels", payload: `{"id":101,"number":1,"body":"","labels":[]}`, wantOutcome: tracker.IssueStateOutcomeAbsent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.payload)
			}))
			defer server.Close()
			client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
			client.HTTP = server.Client()
			client.issueNumbers.Store("101", 1)

			states, err := client.FetchIssueStatesByIDs(context.Background(), []string{"101"})
			if tc.wantErr && !errors.Is(err, tracker.ErrIssueStateRefreshIncomplete) {
				t.Fatalf("FetchIssueStatesByIDs error = %v; want typed incomplete response", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("FetchIssueStatesByIDs() error = %v; want nil", err)
			}
			if got := states["101"].Outcome; got != tc.wantOutcome {
				t.Fatalf("outcome = %v; want %v", got, tc.wantOutcome)
			}
		})
	}
}

func TestTrackerClientFetchIssueStatesTodoRequiresBodyKey(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantOutcome tracker.IssueStateOutcome
		wantErr     bool
	}{
		{name: "missing body", body: ``, wantOutcome: tracker.IssueStateOutcomeUnknown, wantErr: true},
		{name: "null body", body: `,"body":null`, wantOutcome: tracker.IssueStateOutcomeUnknown, wantErr: true},
		{name: "empty body", body: `,"body":""`, wantOutcome: tracker.IssueStateOutcomeCurrent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":101,"number":1,"labels":[{"name":"aiops/todo"}]`+tc.body+`}`)
			}))
			defer server.Close()
			client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
			client.HTTP = server.Client()

			states, err := client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "101", Identifier: "#1"}})
			if tc.wantErr && !errors.Is(err, tracker.ErrIssueStateRefreshIncomplete) {
				t.Fatalf("FetchIssueStatesByRefs error = %v; want typed incomplete body", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("FetchIssueStatesByRefs() error = %v; want nil", err)
			}
			if got := states["101"].Outcome; got != tc.wantOutcome {
				t.Fatalf("outcome = %v; want %v", got, tc.wantOutcome)
			}
		})
	}
}

func TestTrackerClientFetchIssueStatesIncompletePayloadPreservesLaterCurrent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues/1":
			_, _ = io.WriteString(w, `{"id":101,"number":1}`)
		case "/api/v1/repos/owner/repo/issues/2":
			_, _ = io.WriteString(w, `{"id":202,"number":2,"labels":[{"name":"aiops/done"}]}`)
		default:
			t.Fatalf("request path = %q; want issue endpoint #1 or #2", r.URL.Path)
		}
	}))
	defer server.Close()
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	states, err := client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "101", Identifier: "#1"}, {ID: "202", Identifier: "#2"}})
	if !errors.Is(err, tracker.ErrIssueStateRefreshIncomplete) {
		t.Fatalf("FetchIssueStatesByRefs error = %v; want typed incomplete response", err)
	}
	if states["101"].Outcome != tracker.IssueStateOutcomeUnknown || states["202"].Outcome != tracker.IssueStateOutcomeCurrent {
		t.Fatalf("states = %#v; want incomplete row unknown and later clean row current", states)
	}
}

func TestTrackerClientFetchIssueStatesRateLimitStopsLaterRefs(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	states, err := client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "101", Identifier: "#1"}, {ID: "202", Identifier: "#2"}})
	if !errors.Is(err, tracker.ErrRateLimited) {
		t.Fatalf("FetchIssueStatesByRefs error = %v; want typed rate limit", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d; want stop after first rate limit", requests)
	}
	if states["101"].Outcome != tracker.IssueStateOutcomeUnknown || states["202"].Outcome != tracker.IssueStateOutcomeUnknown {
		t.Fatalf("states = %#v; want both refs unknown", states)
	}
}

func TestTrackerClientFetchIssueStatesRequestDeadlineStopsLaterRefs(t *testing.T) {
	serverRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serverRequests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":202,"number":2,"labels":[{"name":"aiops/done"}]}`)
	}))
	defer server.Close()
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = &http.Client{Transport: giteaPathErrorTransport{
		base: server.Client().Transport,
		path: "/api/v1/repos/owner/repo/issues/1",
		err:  context.DeadlineExceeded,
	}}

	states, err := client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "101", Identifier: "#1"}, {ID: "202", Identifier: "#2"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FetchIssueStatesByRefs error = %v; want request deadline", err)
	}
	if serverRequests != 0 {
		t.Fatalf("server requests = %d; want no second lookup after request deadline", serverRequests)
	}
	if states["101"].Outcome != tracker.IssueStateOutcomeUnknown || states["202"].Outcome != tracker.IssueStateOutcomeUnknown {
		t.Fatalf("states = %#v; want both refs unknown", states)
	}
}

func TestTrackerClientFetchIssueStatesUnknownForUnknownAiopsLabel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/owner/repo/issues/8" {
			t.Fatalf("request path = %q; want issue 8", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Issue{ID: 808, Number: 8, Labels: []Label{{Name: "aiops/future-state"}}})
	}))
	defer server.Close()
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	states, err := client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "808", Identifier: "#8"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs() error = %v; want nil", err)
	}
	if got := states["808"].Outcome; got != tracker.IssueStateOutcomeUnknown {
		t.Fatalf("outcome = %v; want unknown for unrecognized aiops/* workflow label", got)
	}
}

func TestTrackerClientFetchIssueStatesUnknownForInconsistentCachedRefOutcome(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.NotFound(w, r)
	}))
	defer server.Close()
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	tests := []struct {
		name string
		ref  tracker.IssueRef
	}{
		{name: "conflicting identifier", ref: tracker.IssueRef{ID: "101", Identifier: "#2"}},
		{name: "malformed identifier", ref: tracker.IssueRef{ID: "101", Identifier: "not-a-number"}},
		{name: "number id conflicts with identifier", ref: tracker.IssueRef{ID: "#1", Identifier: "#2"}},
		{name: "number id conflicts with cache", ref: tracker.IssueRef{ID: "#2"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client.issueNumbers.Store(tc.ref.ID, 1)
			states, err := client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{tc.ref})
			if err != nil {
				t.Fatalf("FetchIssueStatesByRefs(%+v) error = %v; want nil", tc.ref, err)
			}
			if got := states[tc.ref.ID].Outcome; got != tracker.IssueStateOutcomeUnknown {
				t.Fatalf("outcome = %v; want unknown for inconsistent cached ref", got)
			}
		})
	}
	if requests != 0 {
		t.Fatalf("requests = %d; want inconsistent refs rejected before network lookup", requests)
	}
}

func TestTrackerClientFetchIssueStatesUnknownOnConfigurationFailure(t *testing.T) {
	client := NewTrackerClient(workflow.TrackerConfig{}, "", "", "")
	states, err := client.FetchIssueStatesByIDs(context.Background(), []string{"1", "2", "1", ""})
	if err == nil {
		t.Fatal("FetchIssueStatesByIDs error = nil; want configuration error")
	}
	if len(states) != 2 || states["1"].Outcome != tracker.IssueStateOutcomeUnknown || states["2"].Outcome != tracker.IssueStateOutcomeUnknown {
		t.Fatalf("states = %#v; want explicit unknown rows for configuration failure", states)
	}
}

func TestTrackerClientFetchIssueStatesByRefsUsesIdentifierFallbackWithoutCache(t *testing.T) {
	var requestedPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPaths = append(requestedPaths, r.URL.Path)
		if r.Header.Get("Authorization") != "token secret" {
			t.Fatalf("Authorization = %q, want token secret", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues/5":
			_ = json.NewEncoder(w).Encode(Issue{
				ID:      555,
				Number:  5,
				Title:   "done",
				HTMLURL: "https://gitea.local/o/r/issues/5",
				Labels:  []Label{{Name: "aiops/done"}},
			})
		case "/api/v1/repos/owner/repo/issues/7":
			_ = json.NewEncoder(w).Encode(Issue{
				ID:      987654,
				Number:  7,
				Title:   "done",
				HTMLURL: "https://gitea.local/o/r/issues/7",
				Labels:  []Label{{Name: "aiops/done"}, {Name: "Aiops-Ready"}},
			})
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	states, err := client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "987654", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs: %v", err)
	}
	if got, want := states, map[string]string{"987654": "Done"}; len(got) != len(want) || got["987654"].State != want["987654"] {
		t.Fatalf("states = %#v, want %#v", got, want)
	}
	if got, want := states["987654"].Labels, []string{"aiops/done", "aiops-ready"}; !slices.Equal(got, want) {
		t.Fatalf("FetchIssueStatesByRefs(987654).Labels = %v; want %v", got, want)
	}
	if got, want := strings.Join(requestedPaths, ","), "/api/v1/repos/owner/repo/issues/7"; got != want {
		t.Fatalf("requested paths = %s, want %s", got, want)
	}
	states, err = client.FetchIssueStatesByIDs(context.Background(), []string{"987654"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs after successful ref refresh: %v", err)
	}
	if got := states["987654"].State; got != "Done" {
		t.Fatalf("successful ref refresh cache state = %q, want Done", got)
	}

	states, err = client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "other-global-id", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs wrong ID: %v", err)
	}
	if len(states) != 1 || states["other-global-id"].Outcome != tracker.IssueStateOutcomeUnknown {
		t.Fatalf("states for mismatched issue id = %#v; want explicit unknown", states)
	}
	states, err = client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs #number ID: %v", err)
	}
	if got := states["#7"].State; got != "Done" {
		t.Fatalf("#number fallback state = %q, want Done", got)
	}
	states, err = client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "#8", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs mismatched #number ID: %v", err)
	}
	if len(states) != 1 || states["#8"].Outcome != tracker.IssueStateOutcomeUnknown {
		t.Fatalf("states for mismatched #number ID = %#v; want explicit unknown", states)
	}
	states, err = client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "7", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs numeric fallback ID: %v", err)
	}
	if got := states["7"].State; got != "Done" {
		t.Fatalf("numeric fallback ID state = %q, want Done", got)
	}
	states, err = client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "5", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs mismatched numeric fallback ID: %v", err)
	}
	if len(states) != 1 || states["5"].Outcome != tracker.IssueStateOutcomeUnknown {
		t.Fatalf("states for mismatched numeric fallback ID = %#v; want explicit unknown", states)
	}
	client.cacheIssueNumber(Issue{ID: 5, Number: 5})
	states, err = client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "5", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs cached mismatched numeric fallback ID: %v", err)
	}
	if len(states) != 1 || states["5"].Outcome != tracker.IssueStateOutcomeUnknown {
		t.Fatalf("states for cached mismatched numeric fallback ID = %#v; want explicit unknown", states)
	}
	client.cacheIssueNumber(Issue{ID: 555, Number: 5})
	states, err = client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "555", Identifier: "#7"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs cached global ID with stale identifier: %v", err)
	}
	if got := states["555"].Outcome; got != tracker.IssueStateOutcomeUnknown {
		t.Fatalf("cached global ID outcome = %v; want unknown for stale identifier", got)
	}
	states, err = client.FetchIssueStatesByIDs(context.Background(), []string{"987654"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs after fallback cache: %v", err)
	}
	if got := states["987654"].State; got != "Done" {
		t.Fatalf("cached fallback state = %q, want Done", got)
	}
}

func TestParseGiteaIssueTimeErrorsOnMalformedTimestamp(t *testing.T) {
	_, err := parseGiteaIssueTime("updated_at", "not-a-timestamp")
	if err == nil {
		t.Fatal("parseGiteaIssueTime malformed timestamp should error")
	}
	if !strings.Contains(err.Error(), "updated_at") || !strings.Contains(err.Error(), "not-a-timestamp") {
		t.Fatalf("error = %q, want field name and bad value", err.Error())
	}
}

func TestTrackerClientListIssuesByStatesDeduplicatesIssuesReturnedForMultipleLabels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Issue{
			{ID: 101, Number: 1, Title: "conflict", Body: "body", HTMLURL: "https://gitea.local/o/r/issues/1", UpdatedAt: "2026-05-17T00:00:00Z", Labels: []Label{{Name: "aiops/todo"}, {Name: "aiops/rework"}}},
		})
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{
		APIKey:       "secret",
		ActiveStates: []string{"Todo", "Rework"},
	}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	issues, err := client.ListActiveIssues(context.Background())
	if err != nil {
		t.Fatalf("ListActiveIssues: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues len = %d, want deduplicated issue", len(issues))
	}
	if issues[0].ID != "101" || issues[0].State != "Rework" {
		t.Fatalf("issue = %#v, want conflict resolved once", issues[0])
	}
}

func TestTrackerClientListIssuesByStatesFiltersTerminalAndMissingStates(t *testing.T) {
	var requestedState string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedState = r.URL.Query().Get("state")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Issue{
			{ID: 201, Number: 1, Title: "done", HTMLURL: "https://gitea.local/o/r/issues/1", Labels: []Label{{Name: "aiops/done"}}},
			{ID: 202, Number: 2, Title: "missing", HTMLURL: "https://gitea.local/o/r/issues/2", Labels: []Label{{Name: "bug"}}},
			{ID: 203, Number: 3, Title: "todo", HTMLURL: "https://gitea.local/o/r/issues/3", Labels: []Label{{Name: "aiops/todo"}}},
		})
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 1 || issues[0].Identifier != "#3" || issues[0].State != "Todo" {
		t.Fatalf("issues = %#v, want only Todo issue", issues)
	}
	if requestedState != "open" {
		t.Fatalf("state query = %q, want open for active states", requestedState)
	}
}

func TestTrackerClientListIssuesByStatesQueriesAllForTerminalStates(t *testing.T) {
	var requestedState string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedState = r.URL.Query().Get("state")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Issue{
			{ID: 211, Number: 11, Title: "closed done", HTMLURL: "https://gitea.local/o/r/issues/11", Labels: []Label{{Name: "aiops/done"}}},
		})
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	issues, err := client.ListIssuesByStates(context.Background(), []string{"Done"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if requestedState != "all" {
		t.Fatalf("state query = %q, want all for terminal states", requestedState)
	}
	if len(issues) != 1 || issues[0].Identifier != "#11" || issues[0].State != "Done" {
		t.Fatalf("issues = %#v, want Done issue", issues)
	}
}

func TestTrackerClientListIssuesByStatesUsesConfiguredTerminalStatesForGiteaState(t *testing.T) {
	var requestedState string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedState = r.URL.Query().Get("state")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Issue{
			{ID: 212, Number: 12, Title: "custom closed", HTMLURL: "https://gitea.local/o/r/issues/12", Labels: []Label{{Name: "aiops/done"}}},
		})
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret", TerminalStates: []string{"Shipped"}}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	_, err := client.ListIssuesByStates(context.Background(), []string{"Shipped"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if requestedState != "all" {
		t.Fatalf("state query = %q, want all for configured terminal state", requestedState)
	}
}

func TestTrackerClientListIssuesByStatesUsesDeterministicConflictState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Issue{
			{ID: 301, Number: 1, Title: "conflict", HTMLURL: "https://gitea.local/o/r/issues/1", Labels: []Label{{Name: "aiops/done"}, {Name: "aiops/rework"}}},
		})
	}))
	defer server.Close()

	var diagnostics []string
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	client.Logf = func(format string, args ...any) {
		diagnostics = append(diagnostics, fmt.Sprintf(format, args...))
	}
	issues, err := client.ListIssuesByStates(context.Background(), []string{"Rework"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 1 || issues[0].State != "Rework" {
		t.Fatalf("issues = %#v, want conflict resolved to Rework", issues)
	}
	if len(diagnostics) == 0 || !strings.Contains(diagnostics[0], "gitea issue #1 label diagnostic") {
		t.Fatalf("diagnostics = %#v, want logged conflict diagnostic", diagnostics)
	}
}

func TestTrackerClientListIssuesByStatesAllowsExactlyFullMaxPages(t *testing.T) {
	var requestedPages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPages = append(requestedPages, r.URL.Query().Get("page"))
		w.Header().Set("Content-Type", "application/json")
		page := r.URL.Query().Get("page")
		if page == strconv.Itoa(listIssuesMaxPages+1) {
			_ = json.NewEncoder(w).Encode([]Issue{})
			return
		}
		issues := make([]Issue, listIssuesPageSize)
		for i := range issues {
			number := (len(requestedPages)-1)*listIssuesPageSize + i + 1
			issues[i] = Issue{ID: int64(number), Number: number, Title: "todo", HTMLURL: fmt.Sprintf("https://gitea.local/o/r/issues/%d", number), Labels: []Label{{Name: "aiops/todo"}}}
		}
		_ = json.NewEncoder(w).Encode(issues)
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates returned error for exactly full max pages: %v", err)
	}
	if len(issues) != listIssuesMaxPages*listIssuesPageSize {
		t.Fatalf("issues len = %d, want %d", len(issues), listIssuesMaxPages*listIssuesPageSize)
	}
	if got, want := requestedPages[len(requestedPages)-1], strconv.Itoa(listIssuesMaxPages+1); got != want {
		t.Fatalf("last requested page = %s, want probe page %s", got, want)
	}
}

// TestTrackerClientListIssuesByStatesErrorsWhenLabelOverflows pins the
// fail-safe contract for #402: if any state-label collection overflows
// pagination, ListIssuesByStates must surface tracker.ErrIssueListingCapped
// (with nil issues) so startup reconciliation does not treat the partial
// result as authoritative and delete workspaces for active issues that fell
// past the page cap.
func TestTrackerClientListIssuesByStatesErrorsWhenLabelOverflows(t *testing.T) {
	var logs []string
	todoPages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			t.Fatalf("page query = %q: %v", r.URL.Query().Get("page"), err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("labels") {
		case "aiops/todo":
			todoPages++
			w.Header().Add("Link", fmt.Sprintf(`<%s%s?page=%d>; rel="next"`, serverURL(r), r.URL.Path, page+1))
			_ = json.NewEncoder(w).Encode([]Issue{{ID: 9001, Number: 9001, Labels: []Label{{Name: "aiops/todo"}}}})
		case "aiops/rework":
			_ = json.NewEncoder(w).Encode([]Issue{{ID: 9001, Number: 9001, Labels: []Label{{Name: "aiops/rework"}}}})
		default:
			t.Fatalf("unexpected labels query %q", r.URL.Query().Get("labels"))
		}
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret", PaginationMaxPages: 1}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	client.Logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	if got := client.PaginationCapHits(); got != 0 {
		t.Fatalf("initial PaginationCapHits = %d, want 0", got)
	}
	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo", "Rework"})
	if !errors.Is(err, tracker.ErrIssueListingCapped) {
		t.Fatalf("ListIssuesByStates err = %v, want tracker.ErrIssueListingCapped", err)
	}
	if issues != nil {
		t.Fatalf("issues = %#v, want nil when listing is partial", issues)
	}
	if todoPages != 2 {
		t.Fatalf("aiops/todo pages = %d, want configured max page plus probe", todoPages)
	}
	if len(logs) == 0 || !strings.Contains(logs[len(logs)-1], "failing this tracker listing") {
		t.Fatalf("logs = %#v, want fail-loud diagnostic", logs)
	}
	if got := client.PaginationCapHits(); got != 1 {
		t.Fatalf("PaginationCapHits = %d, want 1", got)
	}
}

func TestTrackerClientListIssuesByStatesContinuesWhenServerCapsPageBelowRequestedLimit(t *testing.T) {
	var requestedPages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPages = append(requestedPages, r.URL.Query().Get("page"))
		w.Header().Set("Content-Type", "application/json")
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			t.Fatalf("page query = %q: %v", r.URL.Query().Get("page"), err)
		}
		if page < 2 {
			w.Header().Add("Link", fmt.Sprintf(`<%s%s?page=%d>; rel="next"`, serverURL(r), r.URL.Path, page+1))
		}
		issues := make([]Issue, 2)
		for i := range issues {
			number := (page-1)*2 + i + 1
			issues[i] = Issue{ID: int64(number), Number: number, Title: "todo", HTMLURL: fmt.Sprintf("https://gitea.local/o/r/issues/%d", number), Labels: []Label{{Name: "aiops/todo"}}}
		}
		_ = json.NewEncoder(w).Encode(issues)
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 4 {
		t.Fatalf("issues len = %d, want all 4 issues across server-capped pages", len(issues))
	}
	if !slices.Equal(requestedPages, []string{"1", "2"}) {
		t.Fatalf("requested pages = %#v, want pagination to follow Link rel=next", requestedPages)
	}
}

// TestTrackerClientListIssuesByStatesNormalizesLabelsAndBlockedBy pins the
// SPEC §4.1.1 / §11.3 Issue normalization for the Gitea tracker: labels are
// lowercased and `Depends on #N` body references become BlockerRef entries
// populated by a follow-up issue lookup (so the §8.2 Todo blocker rule has the
// State it needs).
func TestTrackerClientListIssuesByStatesNormalizesLabelsAndBlockedBy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]Issue{{
				ID:        101,
				Number:    1,
				Title:     "first",
				Body:      "Needs the blocker resolved.\n\nDepends on #42\nAlso Depends on #43 and depends on #42 again.",
				HTMLURL:   "https://gitea.local/o/r/issues/1",
				CreatedAt: "2026-05-16T23:59:00Z",
				UpdatedAt: "2026-05-17T00:00:00Z",
				Labels: []Label{
					{Name: "Aiops/Todo"},
					{Name: "Priority/P1"},
					{Name: "  "},
					{Name: "Type/Bug"},
				},
			}})
		case "/api/v1/repos/owner/repo/issues/42":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Issue{
				ID:     142,
				Number: 42,
				Title:  "blocker still open",
				Labels: []Label{{Name: "aiops/in-progress"}},
			})
		case "/api/v1/repos/owner/repo/issues/43":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Issue{
				ID:     143,
				Number: 43,
				Title:  "blocker done",
				Labels: []Label{{Name: "aiops/done"}},
			})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{
		APIKey:       "secret",
		ActiveStates: []string{"Todo"},
	}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues len = %d, want 1", len(issues))
	}
	got := issues[0]
	wantLabels := []string{"aiops/todo", "priority/p1", "type/bug"}
	if !slices.Equal(got.Labels, wantLabels) {
		t.Fatalf("Labels = %#v, want %#v (lowercased, empty entries dropped)", got.Labels, wantLabels)
	}
	if len(got.BlockedBy) != 2 {
		t.Fatalf("BlockedBy len = %d, want 2 (deduped #42 + #43); got=%#v", len(got.BlockedBy), got.BlockedBy)
	}
	wantBlockers := map[string]string{"#42": "In Progress", "#43": "Done"}
	for _, b := range got.BlockedBy {
		want, ok := wantBlockers[b.Identifier]
		if !ok {
			t.Fatalf("unexpected blocker %#v", b)
		}
		if b.State != want {
			t.Fatalf("blocker %s state = %q, want %q", b.Identifier, b.State, want)
		}
		if b.ID == "" {
			t.Fatalf("blocker %s ID empty; want canonical Gitea ID", b.Identifier)
		}
	}
	if got.Priority != 0 {
		t.Fatalf("Priority = %d, want 0 (Gitea has no native priority; left at zero per dependsOnRegexp comment)", got.Priority)
	}
}

func TestTrackerClientListIssuesByStatesTreatsMergingBlockerAsNonTerminal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]Issue{{
				ID:     101,
				Number: 1,
				Title:  "dependent",
				Body:   "Depends on #42",
				Labels: []Label{{Name: "aiops/todo"}},
			}})
		case "/api/v1/repos/owner/repo/issues/42":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Issue{
				ID:     142,
				Number: 42,
				Title:  "blocker landing",
				Labels: []Label{{Name: "aiops/merging"}},
			})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{
		APIKey:       "secret",
		ActiveStates: []string{"Todo"},
	}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues len = %d, want 1", len(issues))
	}
	blockedBy := issues[0].BlockedBy
	if len(blockedBy) != 1 || blockedBy[0].Identifier != "#42" || blockedBy[0].State != "Merging" {
		t.Fatalf("BlockedBy = %#v, want #42 in Merging state", blockedBy)
	}
	terminal := map[string]struct{}{"done": {}, "canceled": {}}
	if !tracker.BlockedByNonTerminal(blockedBy, terminal) {
		t.Fatalf("BlockedByNonTerminal(%#v) = false, want true because Merging is non-terminal", blockedBy)
	}
}

// TestTrackerClientListIssuesByStatesTolerantOfBlockerLookupFailure: a
// definitively deleted blocker (404) silently drops out of BlockedBy rather
// than aborting candidate enumeration — it can never become terminal, so
// keeping it would starve the candidate forever. (A transient lookup failure
// fails closed as an empty-state placeholder instead; see
// TestTrackerClientFetchIssueStatesByRefsCarriesRefreshedBlockers.)
func TestTrackerClientListIssuesByStatesTolerantOfBlockerLookupFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]Issue{{
				ID:        101,
				Number:    1,
				Title:     "first",
				Body:      "Depends on #999 (deleted)\nDepends on #43",
				HTMLURL:   "https://gitea.local/o/r/issues/1",
				CreatedAt: "2026-05-16T23:59:00Z",
				UpdatedAt: "2026-05-17T00:00:00Z",
				Labels:    []Label{{Name: "aiops/todo"}},
			}})
		case "/api/v1/repos/owner/repo/issues/999":
			http.NotFound(w, r)
		case "/api/v1/repos/owner/repo/issues/43":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Issue{
				ID:     143,
				Number: 43,
				Title:  "still exists",
				Labels: []Label{{Name: "aiops/done"}},
			})
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{
		APIKey:       "secret",
		ActiveStates: []string{"Todo"},
	}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("issues len = %d, want 1", len(issues))
	}
	if len(issues[0].BlockedBy) != 1 || issues[0].BlockedBy[0].Identifier != "#43" {
		t.Fatalf("BlockedBy = %#v, want only #43 (missing #999 silently dropped)", issues[0].BlockedBy)
	}
}

// TestTrackerClientListIssuesByStatesSurfacesMalformedTimestamp pins coverage
// gap (a) for #521: a malformed created_at/updated_at routed THROUGH the
// listing (not just parseGiteaIssueTime in isolation) must surface the parse
// error from listIssuesByStateLabel. created_at is parsed before updated_at,
// so a malformed created_at wins; the error names the field and bad value.
func TestTrackerClientListIssuesByStatesSurfacesMalformedTimestamp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Issue{
			{ID: 401, Number: 1, Title: "todo", HTMLURL: "https://gitea.local/o/r/issues/1", CreatedAt: "not-a-timestamp", UpdatedAt: "2026-05-17T00:00:00Z", Labels: []Label{{Name: "aiops/todo"}}},
		})
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err == nil {
		t.Fatalf("ListIssuesByStates(malformed created_at) = %#v, nil; want parse error", issues)
	}
	if !strings.Contains(err.Error(), "created_at") || !strings.Contains(err.Error(), "not-a-timestamp") {
		t.Fatalf("ListIssuesByStates err = %q; want created_at field name and bad value", err.Error())
	}
	if issues != nil {
		t.Fatalf("ListIssuesByStates(malformed created_at) issues = %#v; want nil on parse error", issues)
	}
}

// TestTrackerClientListIssuesByStatesDeduplicatesWithinSingleLabelScope pins
// coverage gap (b) for #521: when the SAME issueKey appears twice across the
// pages of ONE label scope, the collectionSeen dedup (marked on the include
// path) must keep it once. Existing coverage only exercises cross-label dedup
// seeded from seenIssues; this exercises intra-scope dedup across pages.
func TestTrackerClientListIssuesByStatesDeduplicatesWithinSingleLabelScope(t *testing.T) {
	var requestedPages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPages = append(requestedPages, r.URL.Query().Get("page"))
		page, err := strconv.Atoi(r.URL.Query().Get("page"))
		if err != nil {
			t.Fatalf("page query = %q: %v", r.URL.Query().Get("page"), err)
		}
		w.Header().Set("Content-Type", "application/json")
		if page < 2 {
			w.Header().Add("Link", fmt.Sprintf(`<%s%s?page=%d>; rel="next"`, serverURL(r), r.URL.Path, page+1))
		}
		// Same issue (ID 501) returned on both page 1 and page 2.
		_ = json.NewEncoder(w).Encode([]Issue{
			{ID: 501, Number: 1, Title: "todo", HTMLURL: "https://gitea.local/o/r/issues/1", CreatedAt: "2026-05-16T23:59:00Z", UpdatedAt: "2026-05-17T00:00:00Z", Labels: []Label{{Name: "aiops/todo"}}},
		})
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if !slices.Equal(requestedPages, []string{"1", "2"}) {
		t.Fatalf("requested pages = %#v; want pages 1 and 2 (same issue repeated)", requestedPages)
	}
	if len(issues) != 1 || issues[0].ID != "501" {
		t.Fatalf("ListIssuesByStates(duplicate across pages) = %#v; want issue 501 once", issues)
	}
}

// TestTrackerClientListIssuesByStateLabelSkipsEmptyStateWithoutWantedStatesFilter
// pins coverage gap (c) for #521: an issue whose labels yield an empty state
// must be dropped by the state=="" guard INDEPENDENTLY of the wantedStates
// filter. It drives listIssuesByStateLabel directly with an empty wantedStates
// set, so the wantedStates filter is a no-op (len==0) and cannot drop anything;
// the unlabelled issue can only be removed by the state=="" guard, while the
// labelled issue (which would survive the wantedStates filter regardless) is
// kept.
func TestTrackerClientListIssuesByStateLabelSkipsEmptyStateWithoutWantedStatesFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Issue{
			{ID: 601, Number: 1, Title: "no aiops label", HTMLURL: "https://gitea.local/o/r/issues/1", CreatedAt: "2026-05-16T23:59:00Z", UpdatedAt: "2026-05-17T00:00:00Z", Labels: []Label{{Name: "bug"}}},
			{ID: 602, Number: 2, Title: "todo", HTMLURL: "https://gitea.local/o/r/issues/2", CreatedAt: "2026-05-16T23:59:00Z", UpdatedAt: "2026-05-17T00:00:00Z", Labels: []Label{{Name: "aiops/todo"}}},
		})
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	issues, capped, err := client.listIssuesByStateLabel(context.Background(), "aiops/todo", "open", map[string]struct{}{}, map[string]struct{}{})
	if err != nil {
		t.Fatalf("listIssuesByStateLabel: %v", err)
	}
	if capped {
		t.Fatalf("listIssuesByStateLabel capped = true; want false")
	}
	if len(issues) != 1 || issues[0].ID != "602" || issues[0].State != "Todo" {
		t.Fatalf("listIssuesByStateLabel(empty wantedStates) = %#v; want only issue 602 (unlabelled issue 601 dropped by state==\"\" guard, not wantedStates)", issues)
	}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}

func mustTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		panic(err)
	}
	return parsed
}

// newSharedBlockerTrackerClient builds a Gitea mock whose issue-list endpoint
// returns the given source issues and whose per-issue endpoint serves a single
// shared blocker (#3, "Todo"), counting how many times the blocker is
// fetched. It is the harness for the #677 per-poll-tick blocker-cache tests.
func newSharedBlockerTrackerClient(t *testing.T, sources []Issue, blockerFetches *int) *TrackerClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues":
			_ = json.NewEncoder(w).Encode(sources)
		case "/api/v1/repos/owner/repo/issues/3":
			*blockerFetches++
			_ = json.NewEncoder(w).Encode(Issue{ID: 103, Number: 3, Title: "blocker", HTMLURL: "https://gitea.local/o/r/issues/3", Labels: []Label{{Name: "aiops/todo"}}})
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret", ActiveStates: []string{"Todo"}}, server.URL, "owner", "repo")
	client.HTTP = server.Client()
	return client
}

func TestTrackerClientListIssuesByStatesFetchesSharedBlockerOncePerTick(t *testing.T) {
	var blockerFetches int
	client := newSharedBlockerTrackerClient(t, []Issue{
		{ID: 101, Number: 1, Title: "a", Body: "Depends on #3", HTMLURL: "https://gitea.local/o/r/issues/1", Labels: []Label{{Name: "aiops/todo"}}},
		{ID: 102, Number: 2, Title: "b", Body: "Depends on #3", HTMLURL: "https://gitea.local/o/r/issues/2", Labels: []Label{{Name: "aiops/todo"}}},
	}, &blockerFetches)

	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("ListIssuesByStates returned %d issues; want 2", len(issues))
	}
	if blockerFetches != 1 {
		t.Fatalf("shared blocker #3 fetched %d times; want 1 (cached across both source issues in one tick)", blockerFetches)
	}
	for _, iss := range issues {
		if len(iss.BlockedBy) != 1 || iss.BlockedBy[0].Identifier != "#3" || iss.BlockedBy[0].State != "Todo" {
			t.Fatalf("issue %s BlockedBy = %#v; want one blocker #3 in state Todo", iss.Identifier, iss.BlockedBy)
		}
	}
}

func TestTrackerClientListIssuesByStatesRereadsBlockerOnNextTick(t *testing.T) {
	var blockerFetches int
	blockerLabel := "aiops/todo" // "Todo" on tick 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues":
			_ = json.NewEncoder(w).Encode([]Issue{
				{ID: 101, Number: 1, Title: "a", Body: "Depends on #3", HTMLURL: "https://gitea.local/o/r/issues/1", Labels: []Label{{Name: "aiops/todo"}}},
			})
		case "/api/v1/repos/owner/repo/issues/3":
			blockerFetches++
			_ = json.NewEncoder(w).Encode(Issue{ID: 103, Number: 3, Title: "blocker", HTMLURL: "https://gitea.local/o/r/issues/3", Labels: []Label{{Name: blockerLabel}}})
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer server.Close()
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret", ActiveStates: []string{"Todo"}}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	tick1, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates tick 1: %v", err)
	}
	blockerLabel = "aiops/done" // blocker transitions to "Done" between ticks
	tick2, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates tick 2: %v", err)
	}

	if blockerFetches != 2 {
		t.Fatalf("blocker #3 fetched %d times across two ticks; want 2 (once per tick, no cross-tick cache leak)", blockerFetches)
	}
	if len(tick1) != 1 || len(tick1[0].BlockedBy) != 1 || tick1[0].BlockedBy[0].State != "Todo" {
		t.Fatalf("tick 1 BlockedBy = %#v; want blocker #3 in state Todo", tick1)
	}
	if len(tick2) != 1 || len(tick2[0].BlockedBy) != 1 || tick2[0].BlockedBy[0].State != "Done" {
		t.Fatalf("tick 2 BlockedBy = %#v; want blocker #3 re-read in state Done", tick2)
	}
}

// TestCachedIssueByNumberNilCacheFallsBackToDirectFetch pins the defensive
// fallback for callers that reach buildBlockedBy without ListIssuesByStates
// installing a cache: with a nil cache, each lookup is a direct getIssueByNumber
// (no memoization, no nil-map write panic) (#677).
func TestCachedIssueByNumberNilCacheFallsBackToDirectFetch(t *testing.T) {
	var blockerFetches int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/v1/repos/owner/repo/issues/3" {
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
		blockerFetches++
		_ = json.NewEncoder(w).Encode(Issue{ID: 103, Number: 3, Title: "blocker", HTMLURL: "https://gitea.local/o/r/issues/3", Labels: []Label{{Name: "aiops/todo"}}})
	}))
	defer server.Close()
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	issue, found, resolved, err := client.cachedIssueByNumber(context.Background(), nil, 3)
	if err != nil {
		t.Fatalf("cachedIssueByNumber(nil cache, 3) error = %v; want nil", err)
	}
	if !found || !resolved || issue.Number != 3 {
		t.Fatalf("cachedIssueByNumber(nil cache, 3) = (%#v, %v, %v); want issue #3, true, true", issue, found, resolved)
	}
	if _, found, _, err = client.cachedIssueByNumber(context.Background(), nil, 3); err != nil || !found {
		t.Fatalf("cachedIssueByNumber(nil cache, 3) second call found = false; want true")
	}
	if blockerFetches != 2 {
		t.Fatalf("blocker #3 fetched %d times with a nil cache; want 2 (no memoization without a cache)", blockerFetches)
	}
}

func TestTrackerClientListIssuesByStatesRetriesBlockerAfterTransientError(t *testing.T) {
	var blockerFetches int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues":
			_ = json.NewEncoder(w).Encode([]Issue{
				{ID: 101, Number: 1, Title: "a", Body: "Depends on #3", HTMLURL: "https://gitea.local/o/r/issues/1", Labels: []Label{{Name: "aiops/todo"}}},
				{ID: 102, Number: 2, Title: "b", Body: "Depends on #3", HTMLURL: "https://gitea.local/o/r/issues/2", Labels: []Label{{Name: "aiops/todo"}}},
			})
		case "/api/v1/repos/owner/repo/issues/3":
			blockerFetches++
			if blockerFetches == 1 {
				// Transient failure on the first source issue's fetch: must NOT be
				// cached, so the second source issue still retries (#677).
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode(Issue{ID: 103, Number: 3, Title: "blocker", HTMLURL: "https://gitea.local/o/r/issues/3", Labels: []Label{{Name: "aiops/todo"}}})
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer server.Close()
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret", ActiveStates: []string{"Todo"}}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	issues, err := client.ListIssuesByStates(context.Background(), []string{"Todo"})
	if err != nil {
		t.Fatalf("ListIssuesByStates: %v; a best-effort blocker error must not abort listing", err)
	}
	if blockerFetches != 2 {
		t.Fatalf("blocker #3 fetched %d times; want 2 (transient error not cached, retried for the second source issue)", blockerFetches)
	}
	// The first source's transient failure fails closed as an empty-state
	// placeholder (#750 / PR #752 review); the second source's retry resolves
	// the real blocker state. Both carry #3 — the difference is the State.
	resolved, placeholders := 0, 0
	for _, iss := range issues {
		if len(iss.BlockedBy) != 1 || iss.BlockedBy[0].Identifier != "#3" {
			continue
		}
		if iss.BlockedBy[0].State == "" {
			placeholders++
		} else {
			resolved++
		}
	}
	if placeholders != 1 || resolved != 1 {
		t.Fatalf("blocker #3 carried as placeholder=%d resolved=%d; want 1 fail-closed placeholder (errored source) and 1 resolved (retried source)", placeholders, resolved)
	}
}

func TestTrackerClientListActiveIssuesKeepsIssueOnBlockerGlobalFailure(t *testing.T) {
	tests := []struct {
		name           string
		blockerStatus  int
		blockerRequest error
	}{
		{name: "rate limit", blockerStatus: http.StatusTooManyRequests},
		{name: "request deadline", blockerRequest: context.DeadlineExceeded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/repos/owner/repo/issues":
					_ = json.NewEncoder(w).Encode([]Issue{{
						ID: 101, Number: 1, Title: "blocked todo", Body: "Depends on #9",
						HTMLURL: "https://gitea.local/owner/repo/issues/1",
						Labels:  []Label{{Name: "aiops/todo"}},
					}})
				case "/api/v1/repos/owner/repo/issues/9":
					w.WriteHeader(tc.blockerStatus)
				default:
					t.Fatalf("request path for %q = %q; want listing or blocker endpoint", tc.name, r.URL.Path)
				}
			}))
			defer server.Close()

			client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret", ActiveStates: []string{"Todo"}}, server.URL, "owner", "repo")
			client.HTTP = server.Client()
			if tc.blockerRequest != nil {
				client.HTTP = &http.Client{Transport: giteaPathErrorTransport{
					base: server.Client().Transport,
					path: "/api/v1/repos/owner/repo/issues/9",
					err:  tc.blockerRequest,
				}}
			}

			issues, err := client.ListActiveIssues(context.Background())
			if err != nil {
				t.Fatalf("ListActiveIssues(%q) error = %v; want nil", tc.name, err)
			}
			if len(issues) != 1 {
				t.Fatalf("ListActiveIssues(%q) issue count = %d; want 1", tc.name, len(issues))
			}
			want := []tracker.BlockerRef{{Identifier: "#9"}}
			if !slices.Equal(issues[0].BlockedBy, want) {
				t.Fatalf("ListActiveIssues(%q) BlockedBy = %#v; want %#v", tc.name, issues[0].BlockedBy, want)
			}
		})
	}
}

// TestTrackerClientFetchIssueStatesByRefsCarriesRefreshedBlockers pins #750:
// the narrow refresh re-derives `Depends on #N` blockers from the refreshed
// body (with the blocker's current state) so dispatch revalidation can re-run
// the SPEC §8.2 Todo blocker gate, and a dependency-free issue carries the
// positive non-nil empty answer rather than nil ("no knowledge").
func TestTrackerClientFetchIssueStatesByRefsCarriesRefreshedBlockers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues/5":
			_ = json.NewEncoder(w).Encode(Issue{
				ID: 555, Number: 5, Title: "blocked todo", Body: "Depends on #9",
				HTMLURL: "https://gitea.local/o/r/issues/5",
				Labels:  []Label{{Name: "aiops/todo"}},
			})
		case "/api/v1/repos/owner/repo/issues/9":
			_ = json.NewEncoder(w).Encode(Issue{
				ID: 999, Number: 9, Title: "reopened blocker",
				HTMLURL: "https://gitea.local/o/r/issues/9",
				Labels:  []Label{{Name: "aiops/in-progress"}},
			})
		case "/api/v1/repos/owner/repo/issues/6":
			_ = json.NewEncoder(w).Encode(Issue{
				ID: 666, Number: 6, Title: "free todo", Body: "no dependencies here",
				HTMLURL: "https://gitea.local/o/r/issues/6",
				Labels:  []Label{{Name: "aiops/todo"}},
			})
		case "/api/v1/repos/owner/repo/issues/7":
			_ = json.NewEncoder(w).Encode(Issue{
				ID: 777, Number: 7, Title: "todo with unresolvable dep", Body: "Depends on #99",
				HTMLURL: "https://gitea.local/o/r/issues/7",
				Labels:  []Label{{Name: "aiops/todo"}},
			})
		case "/api/v1/repos/owner/repo/issues/99":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/api/v1/repos/owner/repo/issues/8":
			_ = json.NewEncoder(w).Encode(Issue{
				ID: 888, Number: 8, Title: "mixed deps, one reopened", Body: "Depends on #9\nDepends on #99",
				HTMLURL: "https://gitea.local/o/r/issues/8",
				Labels:  []Label{{Name: "aiops/todo"}},
			})
		case "/api/v1/repos/owner/repo/issues/4":
			_ = json.NewEncoder(w).Encode(Issue{
				ID: 444, Number: 4, Title: "mixed deps, resolved one terminal", Body: "Depends on #10\nDepends on #99",
				HTMLURL: "https://gitea.local/o/r/issues/4",
				Labels:  []Label{{Name: "aiops/todo"}},
			})
		case "/api/v1/repos/owner/repo/issues/10":
			_ = json.NewEncoder(w).Encode(Issue{
				ID: 1010, Number: 10, Title: "done blocker",
				HTMLURL: "https://gitea.local/o/r/issues/10",
				Labels:  []Label{{Name: "aiops/done"}},
			})
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret", ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done", "Canceled"}}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	states, err := client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{
		{ID: "555", Identifier: "#5"},
		{ID: "666", Identifier: "#6"},
		{ID: "777", Identifier: "#7"},
		{ID: "888", Identifier: "#8"},
		{ID: "444", Identifier: "#4"},
	})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs() error = %v; want nil", err)
	}
	blocked := states["555"].BlockedBy
	if len(blocked) != 1 || blocked[0].ID != "999" || blocked[0].Identifier != "#9" || blocked[0].State != "In Progress" {
		t.Fatalf("FetchIssueStatesByRefs(555).BlockedBy = %#v; want #9 in In Progress", blocked)
	}
	free := states["666"].BlockedBy
	if free == nil || len(free) != 0 {
		t.Fatalf("FetchIssueStatesByRefs(666).BlockedBy = %#v; want non-nil empty for a dependency-free body", free)
	}
	// A `Depends on #N` reference whose lookup transiently fails must fail
	// closed: an empty-state placeholder the gate treats as open, never a
	// positive "no blockers" — otherwise a transient blocker-lookup failure
	// would let the candidate dispatch past a blocker the refresh could not
	// see (PR #752 review).
	failed := states["777"].BlockedBy
	if len(failed) != 1 || failed[0].Identifier != "#99" || failed[0].State != "" {
		t.Fatalf("FetchIssueStatesByRefs(777).BlockedBy = %#v; want one empty-state placeholder for the unresolvable #99", failed)
	}
	// Mixed resolution keeps the resolved reopened blocker AND the
	// fail-closed placeholder; either alone blocks at the consumer's gate.
	mixed := states["888"].BlockedBy
	if len(mixed) != 2 || mixed[0].Identifier != "#9" || mixed[0].State != "In Progress" || mixed[1].Identifier != "#99" || mixed[1].State != "" {
		t.Fatalf("FetchIssueStatesByRefs(888).BlockedBy = %#v; want resolved #9 (In Progress) plus the #99 placeholder", mixed)
	}
	// Resolved-terminal blockers do not unblock an unresolved sibling: the
	// placeholder still fails the gate closed.
	terminalMixed := states["444"].BlockedBy
	if len(terminalMixed) != 2 || terminalMixed[0].Identifier != "#10" || terminalMixed[0].State != "Done" || terminalMixed[1].Identifier != "#99" || terminalMixed[1].State != "" {
		t.Fatalf("FetchIssueStatesByRefs(444).BlockedBy = %#v; want resolved #10 (Done) plus the #99 placeholder", terminalMixed)
	}
}

func TestTrackerClientFetchIssueStatesNonTodoSkipsBlockerLookupFailure(t *testing.T) {
	tests := []struct {
		name           string
		blockerStatus  int
		blockerRequest error
	}{
		{name: "rate limit", blockerStatus: http.StatusTooManyRequests},
		{name: "request deadline", blockerRequest: context.DeadlineExceeded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var requestedPaths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestedPaths = append(requestedPaths, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/repos/owner/repo/issues/5":
					_ = json.NewEncoder(w).Encode(Issue{ID: 555, Number: 5, Body: "Depends on #9", Labels: []Label{{Name: "aiops/in-progress"}}})
				case "/api/v1/repos/owner/repo/issues/9":
					w.WriteHeader(tc.blockerStatus)
				case "/api/v1/repos/owner/repo/issues/6":
					_ = json.NewEncoder(w).Encode(Issue{ID: 666, Number: 6, Labels: []Label{{Name: "aiops/done"}}})
				default:
					t.Fatalf("request path = %q; want source issue #5/#6 or blocker #9", r.URL.Path)
				}
			}))
			defer server.Close()
			client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
			client.HTTP = server.Client()
			if tc.blockerRequest != nil {
				client.HTTP = &http.Client{Transport: giteaPathErrorTransport{
					base: server.Client().Transport,
					path: "/api/v1/repos/owner/repo/issues/9",
					err:  tc.blockerRequest,
				}}
			}

			states, err := client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{
				{ID: "555", Identifier: "#5"},
				{ID: "666", Identifier: "#6"},
			})
			if err != nil {
				t.Fatalf("FetchIssueStatesByRefs() error = %v; want nil without non-Todo blocker lookup", err)
			}
			if slices.Contains(requestedPaths, "/api/v1/repos/owner/repo/issues/9") {
				t.Fatalf("requested paths = %v; want no blocker lookup for non-Todo source", requestedPaths)
			}
			if got := states["555"]; got.Outcome != tracker.IssueStateOutcomeCurrent || got.State != "In Progress" || got.BlockedBy != nil {
				t.Fatalf("states[555] = %#v; want current In Progress with nil blocker knowledge", got)
			}
			if got := states["666"]; got.Outcome != tracker.IssueStateOutcomeCurrent || got.State != "Done" {
				t.Fatalf("states[666] = %#v; want later Done row current", got)
			}
		})
	}
}

func TestTrackerClientFetchIssueStatesBlockerRateLimitStopsLaterRefs(t *testing.T) {
	var requestedPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPaths = append(requestedPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues/5":
			_ = json.NewEncoder(w).Encode(Issue{ID: 555, Number: 5, Body: "Depends on #9", Labels: []Label{{Name: "aiops/todo"}}})
		case "/api/v1/repos/owner/repo/issues/9":
			w.WriteHeader(http.StatusTooManyRequests)
		case "/api/v1/repos/owner/repo/issues/6":
			_ = json.NewEncoder(w).Encode(Issue{ID: 666, Number: 6, Labels: []Label{{Name: "aiops/done"}}})
		default:
			t.Fatalf("request path = %q; want issue endpoint #5, #6, or blocker #9", r.URL.Path)
		}
	}))
	defer server.Close()
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	states, err := client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{
		{ID: "555", Identifier: "#5"},
		{ID: "666", Identifier: "#6"},
	})
	if !errors.Is(err, tracker.ErrRateLimited) {
		t.Fatalf("FetchIssueStatesByRefs error = %v; want blocker rate limit", err)
	}
	if slices.Contains(requestedPaths, "/api/v1/repos/owner/repo/issues/6") {
		t.Fatalf("requested paths = %v; want later source #6 skipped after blocker rate limit", requestedPaths)
	}
	if states["555"].Outcome != tracker.IssueStateOutcomeUnknown || states["666"].Outcome != tracker.IssueStateOutcomeUnknown {
		t.Fatalf("states = %#v; want current and later refs unknown after blocker rate limit", states)
	}
}

func TestTrackerClientFetchIssueStatesBlockerDeadlineStopsLaterRefs(t *testing.T) {
	var requestedPaths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPaths = append(requestedPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues/5":
			_ = json.NewEncoder(w).Encode(Issue{ID: 555, Number: 5, Body: "Depends on #9", Labels: []Label{{Name: "aiops/todo"}}})
		case "/api/v1/repos/owner/repo/issues/6":
			_ = json.NewEncoder(w).Encode(Issue{ID: 666, Number: 6, Labels: []Label{{Name: "aiops/done"}}})
		default:
			t.Fatalf("request path = %q; want source issue endpoint #5 or #6", r.URL.Path)
		}
	}))
	defer server.Close()
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = &http.Client{Transport: giteaPathErrorTransport{
		base: server.Client().Transport,
		path: "/api/v1/repos/owner/repo/issues/9",
		err:  context.DeadlineExceeded,
	}}

	states, err := client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{
		{ID: "555", Identifier: "#5"},
		{ID: "666", Identifier: "#6"},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("FetchIssueStatesByRefs error = %v; want blocker request deadline", err)
	}
	if len(requestedPaths) != 1 || requestedPaths[0] != "/api/v1/repos/owner/repo/issues/5" {
		t.Fatalf("requested paths = %v; want only source #5 before blocker deadline", requestedPaths)
	}
	if states["555"].Outcome != tracker.IssueStateOutcomeUnknown || states["666"].Outcome != tracker.IssueStateOutcomeUnknown {
		t.Fatalf("states = %#v; want current and later refs unknown after blocker deadline", states)
	}
}

func TestTrackerClientFetchIssueStatesBlockerNumberMismatchFailsClosedWithoutCaching(t *testing.T) {
	blockerFetches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues/5":
			_ = json.NewEncoder(w).Encode(Issue{ID: 555, Number: 5, Body: "Depends on #9", Labels: []Label{{Name: "aiops/todo"}}})
		case "/api/v1/repos/owner/repo/issues/6":
			_ = json.NewEncoder(w).Encode(Issue{ID: 666, Number: 6, Body: "Depends on #9", Labels: []Label{{Name: "aiops/todo"}}})
		case "/api/v1/repos/owner/repo/issues/9":
			blockerFetches++
			_ = json.NewEncoder(w).Encode(Issue{ID: 999, Number: 99, Labels: []Label{{Name: "aiops/done"}}})
		default:
			t.Fatalf("request path = %q; want issue endpoint #5, #6, or blocker #9", r.URL.Path)
		}
	}))
	defer server.Close()
	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret"}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	states, err := client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{
		{ID: "555", Identifier: "#5"},
		{ID: "666", Identifier: "#6"},
	})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs() error = %v; want nil", err)
	}
	if blockerFetches != 2 {
		t.Fatalf("blocker #9 fetches = %d; want 2 because mismatched #99 payload must not be cached", blockerFetches)
	}
	for _, id := range []string{"555", "666"} {
		got := states[id]
		if got.Outcome != tracker.IssueStateOutcomeCurrent || len(got.BlockedBy) != 1 || got.BlockedBy[0].Identifier != "#9" || got.BlockedBy[0].State != "" {
			t.Fatalf("states[%s] = %#v; want current Todo with fail-closed #9 placeholder", id, got)
		}
	}
}

// A definitively deleted blocker (404) is skipped — it can never become
// terminal, so blocking on it would starve the candidate forever; this
// matches the listing path's handling of deleted references.
func TestTrackerClientFetchIssueStatesByRefsSkipsDeletedBlockers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/repos/owner/repo/issues/5":
			_ = json.NewEncoder(w).Encode(Issue{
				ID: 555, Number: 5, Title: "todo with deleted dep", Body: "Depends on #404",
				HTMLURL: "https://gitea.local/o/r/issues/5",
				Labels:  []Label{{Name: "aiops/todo"}},
			})
		case "/api/v1/repos/owner/repo/issues/404":
			http.NotFound(w, r)
		default:
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewTrackerClient(workflow.TrackerConfig{APIKey: "secret", ActiveStates: []string{"Todo"}}, server.URL, "owner", "repo")
	client.HTTP = server.Client()

	states, err := client.FetchIssueStatesByRefs(context.Background(), []tracker.IssueRef{{ID: "555", Identifier: "#5"}})
	if err != nil {
		t.Fatalf("FetchIssueStatesByRefs: %v", err)
	}
	if got := states["555"].BlockedBy; got == nil || len(got) != 0 {
		t.Fatalf("FetchIssueStatesByRefs(555).BlockedBy = %#v; want non-nil empty when the only dependency is deleted", got)
	}
}

// TestIssueNumberFromRef pins the "#N"-only contract (#748): a bare numeric ID
// is the Gitea-internal int64 id, not the issue number, and must not parse.
func TestIssueNumberFromRef(t *testing.T) {
	cases := []struct {
		id, identifier string
		want           int
		ok             bool
	}{
		{id: "8842", identifier: "#12", want: 12, ok: true},
		{id: "#12", identifier: "", want: 12, ok: true},
		{id: "8842", identifier: "", want: 0, ok: false},
		{id: "", identifier: "", want: 0, ok: false},
		{id: "#0", identifier: "", want: 0, ok: false},
		{id: "#x", identifier: "", want: 0, ok: false},
		{id: "8842", identifier: " #12 ", want: 12, ok: true},
	}
	for _, tc := range cases {
		got, ok := IssueNumberFromRef(tc.id, tc.identifier)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("IssueNumberFromRef(%q, %q) = (%d, %t); want (%d, %t)", tc.id, tc.identifier, got, ok, tc.want, tc.ok)
		}
	}
}
