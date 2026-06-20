package runner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type fakeGiteaLabelServer struct {
	mu         sync.Mutex
	authHeader string
	methods    []string
	paths      []string
	bodies     []string
	requests   int
}

func (f *fakeGiteaLabelServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests++
		f.authHeader = r.Header.Get("Authorization")
		f.methods = append(f.methods, r.Method)
		f.paths = append(f.paths, r.URL.String())
		f.bodies = append(f.bodies, string(body))
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `[{"id":101,"name":"aiops/todo"},{"id":202,"name":"bug"}]`)
		case http.MethodDelete:
			_, _ = io.WriteString(w, `{}`)
		case http.MethodPost:
			_, _ = io.WriteString(w, `{"labels":[{"name":"aiops/in-progress"}]}`)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	})
}

func (f *fakeGiteaLabelServer) recorded() (string, string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := ""
	if len(f.paths) > 0 {
		path = f.paths[len(f.paths)-1]
	}
	return f.authHeader, path, f.requests
}

func (f *fakeGiteaLabelServer) recordedSequence() ([]string, []string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...), append([]string(nil), f.paths...), append([]string(nil), f.bodies...)
}

func TestDynamicToolsExposeGiteaIssueLabelsWithTokenIsolation(t *testing.T) {
	token := "gitea_super_secret_token"
	tools := DynamicToolsForWorkflow(workflow.Workflow{Config: workflow.Config{
		Repo: workflow.RepoConfig{Owner: "owner", Name: "repo"},
		Tracker: workflow.TrackerConfig{
			Kind:     "gitea",
			APIKey:   token,
			Endpoint: "https://gitea.example.test/",
		},
	}})

	tool, ok := tools.Lookup("gitea_issue_labels")
	if !ok {
		t.Fatalf("gitea_issue_labels tool not advertised; tools=%#v", tools.Names())
	}
	if strings.Contains(tool.Description, token) {
		t.Fatalf("tool description leaked Gitea token: %q", tool.Description)
	}
	schemaBytes, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("tool input schema is not JSON-marshalable: %v", err)
	}
	if !strings.Contains(string(schemaBytes), `"issue_number"`) || !strings.Contains(string(schemaBytes), `"close_issue"`) || strings.Contains(string(schemaBytes), token) {
		t.Fatalf("tool input schema = %s, want issue_number and close_issue fields with no token leak", schemaBytes)
	}

	server := &fakeGiteaLabelServer{}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()
	proxy := giteaIssueLabelsProxy{token: token, baseURL: httpServer.URL, owner: "owner", repo: "repo", http: httpServer.Client()}
	result, err := proxy.call(context.Background(), ToolCall{
		IssueNumber: 7,
		Labels:      []string{"aiops/in-progress"},
	})
	if err != nil {
		t.Fatalf("gitea_issue_labels call: %v", err)
	}
	if strings.Contains(result, token) {
		t.Fatalf("tool result leaked Gitea token: %q", result)
	}

	auth, _, requests := server.recorded()
	if requests != 3 {
		t.Fatalf("requests = %d, want GET, DELETE, POST", requests)
	}
	if auth != "token "+token {
		t.Fatalf("Authorization = %q, want token auth", auth)
	}
	methods, paths, bodies := server.recordedSequence()
	if strings.Join(methods, ",") != "GET,POST,DELETE" {
		t.Fatalf("methods = %#v, want GET, POST desired label, DELETE stale label", methods)
	}
	if paths[1] != "/api/v1/repos/owner/repo/issues/7/labels" || paths[2] != "/api/v1/repos/owner/repo/issues/7/labels/101" {
		t.Fatalf("paths = %#v", paths)
	}
	body := bodies[1]
	if strings.Contains(body, token) || !strings.Contains(body, "aiops/in-progress") {
		t.Fatalf("unexpected request body: %s", body)
	}
}

func TestDynamicToolsUseGiteaEndpointBeforeProjectSlugAndEnvBaseURL(t *testing.T) {
	t.Setenv("GITEA_BASE_URL", "http://127.0.0.1:1")
	server := &fakeGiteaLabelServer{}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	tools := DynamicToolsForWorkflow(workflow.Workflow{Config: workflow.Config{
		Repo: workflow.RepoConfig{Owner: "owner", Name: "repo"},
		Tracker: workflow.TrackerConfig{
			Kind:        "gitea",
			APIKey:      "token",
			Endpoint:    httpServer.URL + "/",
			ProjectSlug: "http://127.0.0.1:1",
		},
	}})
	tool, ok := tools.Lookup("gitea_issue_labels")
	if !ok {
		t.Fatalf("gitea_issue_labels tool not advertised; tools=%#v", tools.Names())
	}

	result, err := tool.Call(context.Background(), ToolCall{IssueNumber: 7, Labels: []string{"aiops/in-progress"}})
	if err != nil {
		t.Fatalf("gitea_issue_labels call: %v", err)
	}
	if !strings.Contains(result, `"success":true`) {
		t.Fatalf("result = %q, want success from endpoint-backed Gitea server", result)
	}
	_, path, requests := server.recorded()
	if requests != 3 {
		t.Fatalf("requests = %d, want endpoint server to receive GET, POST, DELETE", requests)
	}
	if !strings.HasPrefix(path, "/api/v1/repos/owner/repo/issues/7/labels") {
		t.Fatalf("path = %q, want endpoint-backed Gitea label request", path)
	}
}

func TestGiteaIssueLabelsPreservesNonAIOpsLabelsWhenReplacingState(t *testing.T) {
	server := &fakeGiteaLabelServer{}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	result, err := giteaIssueLabelsProxy{token: "token", baseURL: httpServer.URL, owner: "owner", repo: "repo", http: httpServer.Client()}.
		call(context.Background(), ToolCall{IssueNumber: 7, Labels: []string{"aiops/in-progress"}})
	if err != nil {
		t.Fatalf("gitea_issue_labels call: %v", err)
	}
	if !strings.Contains(result, `"success":true`) {
		t.Fatalf("result = %q, want success", result)
	}

	methods, paths, bodies := server.recordedSequence()
	if len(methods) != 3 || methods[0] != http.MethodGet || methods[1] != http.MethodPost || methods[2] != http.MethodDelete {
		t.Fatalf("methods = %#v, want GET then POST desired aiops label then DELETE stale label", methods)
	}
	if paths[0] != "/api/v1/repos/owner/repo/issues/7/labels" || paths[1] != "/api/v1/repos/owner/repo/issues/7/labels" || paths[2] != "/api/v1/repos/owner/repo/issues/7/labels/101" {
		t.Fatalf("paths = %#v", paths)
	}
	if strings.Contains(bodies[1], "bug") || !strings.Contains(bodies[1], "aiops/in-progress") || strings.Contains(bodies[1], "aiops/todo") {
		t.Fatalf("POST body = %s, want only desired aiops label added without replacing non-aiops labels", bodies[1])
	}
}

func TestGiteaIssueLabelsDoesNotOverwriteConcurrentNonAIOpsLabelChanges(t *testing.T) {
	var mu sync.Mutex
	var methods, paths, bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.String())
		bodies = append(bodies, string(body))
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `[{"id":101,"name":"aiops/todo"},{"id":202,"name":"bug"}]`)
		case http.MethodDelete:
			_, _ = io.WriteString(w, `{}`)
		case http.MethodPost:
			if strings.Contains(string(body), "bug") || strings.Contains(string(body), "urgent") {
				t.Fatalf("POST body = %s, must not replace a full label snapshot that can drop concurrent labels", body)
			}
			_, _ = io.WriteString(w, `{"labels":[{"name":"aiops/in-progress"},{"name":"bug"},{"name":"urgent"}]}`)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	result, err := giteaIssueLabelsProxy{token: "token", baseURL: server.URL, owner: "owner", repo: "repo", http: server.Client()}.
		call(context.Background(), ToolCall{IssueNumber: 7, Labels: []string{"aiops/in-progress"}})
	if err != nil {
		t.Fatalf("gitea_issue_labels call: %v", err)
	}
	if !strings.Contains(result, `"success":true`) {
		t.Fatalf("result = %q, want success", result)
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(methods, ",") != "GET,POST,DELETE" {
		t.Fatalf("methods = %#v, want GET, POST, DELETE", methods)
	}
	if paths[2] != "/api/v1/repos/owner/repo/issues/7/labels/101" {
		t.Fatalf("DELETE path = %q, want stale aiops label id endpoint", paths[2])
	}
	if !strings.Contains(bodies[1], "aiops/in-progress") {
		t.Fatalf("POST body = %s, want desired aiops label", bodies[1])
	}
}

func TestGiteaIssueLabelsAddsDesiredStateBeforeDeletingStaleState(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `[{"id":101,"name":"aiops/todo"},{"id":202,"name":"bug"}]`)
		case http.MethodPost:
			if !strings.Contains(string(body), "aiops/in-progress") {
				t.Fatalf("POST body = %s, want desired state label", body)
			}
			_, _ = io.WriteString(w, `{"labels":[{"name":"aiops/in-progress"}]}`)
		case http.MethodDelete:
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	result, err := giteaIssueLabelsProxy{token: "token", baseURL: server.URL, owner: "owner", repo: "repo", http: server.Client()}.
		call(context.Background(), ToolCall{IssueNumber: 7, Labels: []string{"aiops/in-progress"}})
	if err != nil {
		t.Fatalf("gitea_issue_labels call: %v", err)
	}
	if !strings.Contains(result, `"success":true`) {
		t.Fatalf("result = %q, want success", result)
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(methods, ",") != "GET,POST,DELETE" {
		t.Fatalf("methods = %#v, want GET, POST desired state, DELETE stale state", methods)
	}
}

func TestGiteaIssueLabelsDoesNotDeleteExistingStateWhenAddingDesiredStateFails(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `[{"id":101,"name":"aiops/todo"}]`)
		case http.MethodPost:
			http.Error(w, `{"message":"temporary failure"}`, http.StatusBadGateway)
		case http.MethodDelete:
			t.Fatalf("DELETE must not run after desired state add fails")
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	result, err := giteaIssueLabelsProxy{token: "token", baseURL: server.URL, owner: "owner", repo: "repo", http: server.Client()}.
		call(context.Background(), ToolCall{IssueNumber: 7, Labels: []string{"aiops/in-progress"}})
	assertStructuredFailure(t, result, err, "Gitea label request failed")

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(methods, ",") != "GET,POST" {
		t.Fatalf("methods = %#v, want GET then failed POST only", methods)
	}
}

func TestGiteaIssueLabelsDoesNotDeleteExistingStateWhenDesiredStateIsMissingAfterAdd(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `[{"id":101,"name":"aiops/todo"}]`)
		case http.MethodPost:
			_, _ = io.WriteString(w, `{"labels":[{"name":"aiops/todo"}]}`)
		case http.MethodDelete:
			t.Fatalf("DELETE must not run until the desired aiops label is confirmed present")
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	result, err := giteaIssueLabelsProxy{token: "token", baseURL: server.URL, owner: "owner", repo: "repo", http: server.Client()}.
		call(context.Background(), ToolCall{IssueNumber: 7, Labels: []string{"aiops/in-progress"}})
	assertStructuredFailure(t, result, err, "Gitea label add response did not include desired aiops label")

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(methods, ",") != "GET,POST" {
		t.Fatalf("methods = %#v, want GET then POST only", methods)
	}
}

func TestGiteaIssueLabelsAcceptsGiteaAddLabelArrayResponse(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `[{"id":101,"name":"aiops/todo"}]`)
		case http.MethodPost:
			_, _ = io.WriteString(w, `[{"id":303,"name":"aiops/in-progress"},{"id":202,"name":"bug"}]`)
		case http.MethodDelete:
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	result, err := giteaIssueLabelsProxy{token: "token", baseURL: server.URL, owner: "owner", repo: "repo", http: server.Client()}.
		call(context.Background(), ToolCall{IssueNumber: 7, Labels: []string{"aiops/in-progress"}})
	if err != nil {
		t.Fatalf("gitea_issue_labels call: %v", err)
	}
	if !strings.Contains(result, `"success":true`) {
		t.Fatalf("result = %q, want success", result)
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(methods, ",") != "GET,POST,DELETE" {
		t.Fatalf("methods = %#v, want GET, POST, DELETE after array add response confirms desired label", methods)
	}
}

func TestGiteaIssueLabelsRequestHelperReturnsStructuredFailureForHTTPStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token trimmed-token" {
			t.Fatalf("Authorization = %q, want trimmed token auth", got)
		}
		http.Error(w, `{"message":"temporary failure"}`, http.StatusBadGateway)
	}))
	defer server.Close()

	status, body, failure := giteaIssueLabelsProxy{token: "  trimmed-token  "}.
		doGiteaRequest(context.Background(), server.Client(), http.MethodGet, server.URL, nil)

	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", status, http.StatusBadGateway)
	}
	if !strings.Contains(string(body), "temporary failure") {
		t.Fatalf("body = %q, want upstream failure body", body)
	}
	assertStructuredFailure(t, failure, nil, "Gitea label request failed")
	if !strings.Contains(failure, "502 Bad Gateway") || !strings.Contains(failure, "temporary failure") {
		t.Fatalf("failure = %s, want status and bounded response body", failure)
	}
}

func TestGiteaIssueLabelsTreatsMissingStaleStateAsSuccess(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `[{"id":101,"name":"aiops/todo"}]`)
		case http.MethodPost:
			_, _ = io.WriteString(w, `[{"id":303,"name":"aiops/in-progress"}]`)
		case http.MethodDelete:
			http.Error(w, `{"message":"label not found"}`, http.StatusNotFound)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	result, err := giteaIssueLabelsProxy{token: "token", baseURL: server.URL, owner: "owner", repo: "repo", http: server.Client()}.
		call(context.Background(), ToolCall{IssueNumber: 7, Labels: []string{"aiops/in-progress"}})
	if err != nil {
		t.Fatalf("gitea_issue_labels call: %v", err)
	}
	if !strings.Contains(result, `"success":true`) {
		t.Fatalf("result = %q, want success when stale aiops label is already missing", result)
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(methods, ",") != "GET,POST,DELETE" {
		t.Fatalf("methods = %#v, want GET, POST, DELETE", methods)
	}
}

func TestDynamicToolsDoNotExposeGiteaToolsWithoutGiteaToken(t *testing.T) {
	for _, wf := range []workflow.Workflow{
		{},
		{Config: workflow.Config{Tracker: workflow.TrackerConfig{Kind: "gitea"}}},
		{Config: workflow.Config{Tracker: workflow.TrackerConfig{Kind: "linear", APIKey: "token"}}},
	} {
		tools := DynamicToolsForWorkflow(wf)
		if _, ok := tools.Lookup("gitea_issue_labels"); ok {
			t.Fatalf("gitea_issue_labels advertised without configured Gitea token: %#v", wf.Config.Tracker)
		}
	}
}

func TestDynamicToolsDoNotExposeGiteaToolsWithoutBaseURL(t *testing.T) {
	t.Setenv("GITEA_BASE_URL", "")
	tools := DynamicToolsForWorkflow(workflow.Workflow{Config: workflow.Config{
		Repo: workflow.RepoConfig{Owner: "owner", Name: "repo"},
		Tracker: workflow.TrackerConfig{
			Kind:   "gitea",
			APIKey: "token",
		},
	}})
	if _, ok := tools.Lookup("gitea_issue_labels"); ok {
		t.Fatalf("gitea_issue_labels advertised without configured Gitea base URL")
	}
}

func TestDynamicToolsExposeGiteaIssueLabelsWithEnvBaseURL(t *testing.T) {
	t.Setenv("GITEA_BASE_URL", "https://gitea.env.example/")
	tools := DynamicToolsForWorkflow(workflow.Workflow{Config: workflow.Config{
		Repo: workflow.RepoConfig{Owner: "owner", Name: "repo"},
		Tracker: workflow.TrackerConfig{
			Kind:   "gitea",
			APIKey: "token",
		},
	}})
	if _, ok := tools.Lookup("gitea_issue_labels"); !ok {
		t.Fatalf("gitea_issue_labels not advertised with env Gitea base URL; tools=%#v", tools.Names())
	}
}

func TestGiteaIssueLabelsRejectsMultipleAIOpsStateLabelsWithoutHTTPRequest(t *testing.T) {
	server := &fakeGiteaLabelServer{}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	result, err := giteaIssueLabelsProxy{token: "token", baseURL: httpServer.URL, owner: "owner", repo: "repo", http: httpServer.Client()}.
		call(context.Background(), ToolCall{IssueNumber: 7, Labels: []string{"aiops/in-progress", "aiops/done"}})
	assertStructuredFailure(t, result, err, "gitea_issue_labels labels must contain exactly one aiops/* state label")
	_, _, requests := server.recorded()
	if requests != 0 {
		t.Fatalf("server received %d requests, want 0", requests)
	}
}

func TestGiteaIssueLabelsRejectsUnknownAIOpsStateLabelWithoutHTTPRequest(t *testing.T) {
	server := &fakeGiteaLabelServer{}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	result, err := giteaIssueLabelsProxy{token: "token", baseURL: httpServer.URL, owner: "owner", repo: "repo", http: httpServer.Client()}.
		call(context.Background(), ToolCall{IssueNumber: 7, Labels: []string{"aiops/inprogress"}})
	assertStructuredFailure(t, result, err, "gitea_issue_labels label must be one of: aiops/canceled, aiops/done, aiops/human-review, aiops/in-progress, aiops/merging, aiops/rework, aiops/todo")
	_, _, requests := server.recorded()
	if requests != 0 {
		t.Fatalf("server received %d requests, want 0", requests)
	}
}

func TestGiteaIssueLabelsAcceptsMergingStateLabel(t *testing.T) {
	server := &fakeGiteaLabelServer{}
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		server.mu.Lock()
		server.requests++
		server.methods = append(server.methods, r.Method)
		server.paths = append(server.paths, r.URL.String())
		server.bodies = append(server.bodies, string(body))
		server.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `[{"id":101,"name":"aiops/todo"},{"id":202,"name":"bug"}]`)
		case http.MethodPost:
			_, _ = io.WriteString(w, `{"labels":[{"name":"aiops/merging"}]}`)
		case http.MethodDelete:
			_, _ = io.WriteString(w, `{}`)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
	defer httpServer.Close()

	result, err := giteaIssueLabelsProxy{token: "token", baseURL: httpServer.URL, owner: "owner", repo: "repo", http: httpServer.Client()}.
		call(context.Background(), ToolCall{IssueNumber: 7, Labels: []string{"aiops/merging"}})
	if err != nil {
		t.Fatalf("gitea_issue_labels call: %v", err)
	}
	if !strings.Contains(result, `"success":true`) {
		t.Fatalf("result = %q, want success", result)
	}

	methods, _, bodies := server.recordedSequence()
	if strings.Join(methods, ",") != "GET,POST,DELETE" {
		t.Fatalf("methods = %#v, want GET, POST desired label, DELETE stale label", methods)
	}
	if !strings.Contains(bodies[1], "aiops/merging") {
		t.Fatalf("POST body = %s, want aiops/merging desired label", bodies[1])
	}
}

func TestGiteaIssueLabelsRejectsMissingIssueNumberWithoutHTTPRequest(t *testing.T) {
	server := &fakeGiteaLabelServer{}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	result, err := giteaIssueLabelsProxy{token: "token", baseURL: httpServer.URL, owner: "owner", repo: "repo", http: httpServer.Client()}.
		call(context.Background(), ToolCall{Labels: []string{"aiops/in-progress"}})
	assertStructuredFailure(t, result, err, "gitea_issue_labels issue_number is required")
	_, _, requests := server.recorded()
	if requests != 0 {
		t.Fatalf("server received %d requests, want 0", requests)
	}
}

// TestGiteaIssueLabelsRejectsNonAIOpsPrefixedLabelWithoutHTTPRequest pins the
// prefix/empty guard (label == "" || !HasPrefix lower "aiops/") that is distinct
// from the allowlist guard: a "bug" label is rejected for failing the prefix
// check before it ever reaches validGiteaStateLabels.
func TestGiteaIssueLabelsRejectsNonAIOpsPrefixedLabelWithoutHTTPRequest(t *testing.T) {
	server := &fakeGiteaLabelServer{}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	result, err := giteaIssueLabelsProxy{token: "token", baseURL: httpServer.URL, owner: "owner", repo: "repo", http: httpServer.Client()}.
		call(context.Background(), ToolCall{IssueNumber: 7, Labels: []string{"bug"}})
	assertStructuredFailure(t, result, err, "gitea_issue_labels only accepts aiops/* labels")
	_, _, requests := server.recorded()
	if requests != 0 {
		t.Fatalf("call(%q) made %d HTTP requests; want 0", "bug", requests)
	}
}

// TestGiteaIssueLabelsAcceptsLabelWithSurroundingWhitespace pins the
// strings.TrimSpace reassignment: a " aiops/in-progress " input is trimmed and
// accepted, and the trimmed value (not the padded original) is sent on the wire.
func TestGiteaIssueLabelsAcceptsLabelWithSurroundingWhitespace(t *testing.T) {
	server := &fakeGiteaLabelServer{}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	const padded = " aiops/in-progress "
	result, err := giteaIssueLabelsProxy{token: "token", baseURL: httpServer.URL, owner: "owner", repo: "repo", http: httpServer.Client()}.
		call(context.Background(), ToolCall{IssueNumber: 7, Labels: []string{padded}})
	if err != nil {
		t.Fatalf("call(%q) returned Go error %v; want nil", padded, err)
	}
	if !strings.Contains(result, `"success":true`) {
		t.Fatalf("call(%q) = %q; want success after trimming", padded, result)
	}

	methods, _, bodies := server.recordedSequence()
	if strings.Join(methods, ",") != "GET,POST,DELETE" {
		t.Fatalf("call(%q) methods = %#v; want GET,POST,DELETE", padded, methods)
	}
	if !strings.Contains(bodies[1], `"aiops/in-progress"`) || strings.Contains(bodies[1], padded) {
		t.Fatalf("call(%q) POST body = %s; want trimmed %q on the wire, not the padded input", padded, bodies[1], "aiops/in-progress")
	}
}

// TestGiteaIssueLabelsRejectsStaleAIOpsLabelMissingID pins the label.ID == 0
// guard in the stale-delete loop: a stale aiops label whose GET response omitted
// its id yields the "omitted id" failure and no DELETE is attempted.
func TestGiteaIssueLabelsRejectsStaleAIOpsLabelMissingID(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			// Stale aiops label "aiops/todo" present with id 0 (omitted).
			_, _ = io.WriteString(w, `[{"id":0,"name":"aiops/todo"}]`)
		case http.MethodPost:
			_, _ = io.WriteString(w, `[{"id":303,"name":"aiops/in-progress"}]`)
		case http.MethodDelete:
			t.Fatalf("DELETE must not run for a stale aiops label with omitted id")
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	result, err := giteaIssueLabelsProxy{token: "token", baseURL: server.URL, owner: "owner", repo: "repo", http: server.Client()}.
		call(context.Background(), ToolCall{IssueNumber: 7, Labels: []string{"aiops/in-progress"}})
	assertStructuredFailure(t, result, err, "Gitea label response omitted id for stale aiops label")

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(methods, ",") != "GET,POST" {
		t.Fatalf("methods = %#v; want GET,POST then failure before DELETE", methods)
	}
}

// TestGiteaIssueLabelsNoOpWhenDesiredAlreadyPresentAndNoStale pins the else
// branch: when the desired aiops label is already present (labelsToAdd empty)
// and no stale aiops label exists, the call returns the synthetic {"labels":[]}
// result with only the GET request and no add/delete writes.
func TestGiteaIssueLabelsNoOpWhenDesiredAlreadyPresentAndNoStale(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			// Desired label already present; only non-aiops "bug" otherwise.
			_, _ = io.WriteString(w, `[{"id":101,"name":"aiops/in-progress"},{"id":202,"name":"bug"}]`)
		case http.MethodPost:
			t.Fatalf("POST must not run when the desired aiops label is already present")
		case http.MethodDelete:
			t.Fatalf("DELETE must not run when there is no stale aiops label")
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()

	result, err := giteaIssueLabelsProxy{token: "token", baseURL: server.URL, owner: "owner", repo: "repo", http: server.Client()}.
		call(context.Background(), ToolCall{IssueNumber: 7, Labels: []string{"aiops/in-progress"}})
	if err != nil {
		t.Fatalf("no-op call returned Go error %v; want nil", err)
	}
	if !strings.Contains(result, `"success":true`) || !strings.Contains(result, `{\"labels\":[]}`) {
		t.Fatalf("no-op call result = %q; want synthetic success {\"labels\":[]}", result)
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(methods, ",") != "GET" {
		t.Fatalf("methods = %#v; want GET only for an already-satisfied issue", methods)
	}
}

func TestGiteaIssueLabelsCloseIssueAfterTerminalLabelReplace(t *testing.T) {
	var mu sync.Mutex
	var methods, paths, bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.String())
		bodies = append(bodies, string(body))
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `[{"id":101,"name":"aiops/in-progress"}]`)
		case http.MethodPost:
			if !strings.Contains(string(body), "aiops/done") {
				t.Fatalf("POST body = %s, want terminal aiops/done label", body)
			}
			_, _ = io.WriteString(w, `{"labels":[{"id":303,"name":"aiops/done"}]}`)
		case http.MethodDelete:
			_, _ = io.WriteString(w, `{}`)
		case http.MethodPatch:
			if r.URL.String() != "/api/v1/repos/owner/repo/issues/7" {
				t.Fatalf("PATCH path = %q, want issue endpoint", r.URL.String())
			}
			if string(body) != `{"state":"closed"}` {
				t.Fatalf("PATCH body = %s; want closed state payload", body)
			}
			_, _ = io.WriteString(w, `{"state":"closed"}`)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()
	proxy := giteaClassificationProxy(server.URL)
	proxy.http = server.Client()

	var audits []ToolMutationAudit
	ctx := WithToolMutationSink(context.Background(), func(audit ToolMutationAudit) {
		audits = append(audits, audit)
	})
	result, err := proxy.call(ctx, ToolCall{IssueNumber: 7, Labels: []string{"aiops/done"}, CloseIssue: true})
	if err != nil {
		t.Fatalf("call error = %v; want nil", err)
	}
	if !toolResultSucceeded(result) || !strings.Contains(result, `\"closed\":true`) || !strings.Contains(result, `\"label_result\":{\"labels\"`) {
		t.Fatalf("result = %s; want successful close_issue result with label_result", result)
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(methods, ",") != "GET,POST,DELETE,PATCH" {
		t.Fatalf("methods = %#v; want GET, POST terminal label, DELETE stale label, PATCH close", methods)
	}
	if paths[3] != "/api/v1/repos/owner/repo/issues/7" {
		t.Fatalf("paths = %#v; want PATCH to issue endpoint", paths)
	}
	if bodies[3] != `{"state":"closed"}` {
		t.Fatalf("PATCH body = %s; want closed state payload", bodies[3])
	}
	want := ToolMutationAudit{
		CurrentIssueNonActiveStateUpdate: true,
		CurrentIssueTerminalStateUpdate:  true,
		CurrentIssueTerminalState:        "Done",
	}
	if len(audits) != 1 || audits[0] != want {
		t.Fatalf("audits = %+v; want exactly [%+v]", audits, want)
	}
}

func TestGiteaIssueLabelsCloseIssueRepairsAlreadyTerminalOpenIssue(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `[{"id":303,"name":"aiops/done"},{"id":202,"name":"bug"}]`)
		case http.MethodPatch:
			if string(body) != `{"state":"closed"}` {
				t.Fatalf("PATCH body = %s; want closed state payload", body)
			}
			_, _ = io.WriteString(w, `{"state":"closed"}`)
		case http.MethodPost:
			t.Fatalf("POST must not run when terminal label is already present")
		case http.MethodDelete:
			t.Fatalf("DELETE must not run when there is no stale aiops label")
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()
	proxy := giteaClassificationProxy(server.URL)
	proxy.http = server.Client()

	var audits []ToolMutationAudit
	ctx := WithToolMutationSink(context.Background(), func(audit ToolMutationAudit) {
		audits = append(audits, audit)
	})
	result, err := proxy.call(ctx, ToolCall{IssueNumber: 7, Labels: []string{"aiops/done"}, CloseIssue: true})
	if err != nil {
		t.Fatalf("call error = %v; want nil", err)
	}
	if !toolResultSucceeded(result) || !strings.Contains(result, `\"closed\":true`) || !strings.Contains(result, `\"label_result\":{\"labels\":[]`) {
		t.Fatalf("result = %s; want successful close_issue result with label_result", result)
	}

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(methods, ",") != "GET,PATCH" {
		t.Fatalf("methods = %#v; want GET then PATCH close for already-terminal issue", methods)
	}
	if len(audits) != 0 {
		t.Fatalf("audits = %+v; want none because no label handoff write landed", audits)
	}
}

func TestGiteaIssueLabelsRejectsInvalidCloseIssueWithoutHTTPRequest(t *testing.T) {
	cases := []struct {
		name   string
		call   ToolCall
		reason string
	}{
		{
			name:   "different issue",
			call:   ToolCall{IssueNumber: 8, Labels: []string{"aiops/done"}, CloseIssue: true},
			reason: "gitea_issue_labels close_issue is only supported for the current issue",
		},
		{
			name:   "non-terminal label",
			call:   ToolCall{IssueNumber: 7, Labels: []string{"aiops/human-review"}, CloseIssue: true},
			reason: "gitea_issue_labels close_issue requires a configured terminal aiops/* label",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := &fakeGiteaLabelServer{}
			httpServer := httptest.NewServer(server.handler())
			defer httpServer.Close()
			proxy := giteaClassificationProxy(httpServer.URL)
			proxy.http = httpServer.Client()

			result, err := proxy.call(context.Background(), tc.call)
			assertStructuredFailure(t, result, err, tc.reason)
			_, _, requests := server.recorded()
			if requests != 0 {
				t.Fatalf("server received %d requests; want 0", requests)
			}
		})
	}
}

func TestGiteaIssueLabelsRestoresActiveStateWhenCloseIssueFails(t *testing.T) {
	var mu sync.Mutex
	var methods, bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		methods = append(methods, r.Method)
		bodies = append(bodies, string(body))
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, `[{"id":101,"name":"aiops/in-progress"}]`)
		case http.MethodPost:
			switch {
			case strings.Contains(string(body), "aiops/done"):
				_, _ = io.WriteString(w, `{"labels":[{"id":303,"name":"aiops/done"}]}`)
			case strings.Contains(string(body), "aiops/in-progress"):
				_, _ = io.WriteString(w, `{"labels":[{"id":101,"name":"aiops/in-progress"}]}`)
			default:
				t.Fatalf("POST body = %s; want terminal add or active-state restore", body)
			}
		case http.MethodDelete:
			_, _ = io.WriteString(w, `{}`)
		case http.MethodPatch:
			http.Error(w, `{"message":"temporary failure"}`, http.StatusBadGateway)
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer server.Close()
	proxy := giteaClassificationProxy(server.URL)
	proxy.http = server.Client()

	var audits []ToolMutationAudit
	ctx := WithToolMutationSink(context.Background(), func(audit ToolMutationAudit) {
		audits = append(audits, audit)
	})
	result, err := proxy.call(ctx, ToolCall{IssueNumber: 7, Labels: []string{"aiops/done"}, CloseIssue: true})
	assertStructuredFailure(t, result, err, "Gitea label request failed", "502 Bad Gateway")

	mu.Lock()
	defer mu.Unlock()
	if strings.Join(methods, ",") != "GET,POST,DELETE,PATCH,POST" {
		t.Fatalf("methods = %#v; want close failure followed by active-state restore", methods)
	}
	if !strings.Contains(bodies[4], "aiops/in-progress") {
		t.Fatalf("rollback POST body = %s; want original active state restored", bodies[4])
	}
	if len(audits) != 0 {
		t.Fatalf("audits = %+v; want none because close failure restored retryable active state", audits)
	}
}

// giteaEchoLabelServer serves the labels endpoint for handoff-classification
// tests: GET returns currentLabels JSON, POST echoes the requested labels with
// synthetic ids, DELETE succeeds unless the trailing label id is listed in
// failDeleteIDs.
func giteaEchoLabelServer(currentLabels string, failDeleteIDs ...string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w, currentLabels)
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			var payload struct {
				Labels []string `json:"labels"`
			}
			_ = json.Unmarshal(body, &payload)
			echoed := make([]map[string]any, 0, len(payload.Labels))
			for i, name := range payload.Labels {
				echoed = append(echoed, map[string]any{"id": 900 + i, "name": name})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"labels": echoed})
		case http.MethodDelete:
			parts := strings.Split(strings.TrimRight(r.URL.Path, "/"), "/")
			deleteID := parts[len(parts)-1]
			for _, failID := range failDeleteIDs {
				if deleteID == failID {
					http.Error(w, "boom", http.StatusInternalServerError)
					return
				}
			}
			_, _ = io.WriteString(w, `{}`)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	}))
}

func giteaClassificationProxy(serverURL string) giteaIssueLabelsProxy {
	return giteaIssueLabelsProxy{
		token:              "token",
		baseURL:            serverURL,
		owner:              "owner",
		repo:               "repo",
		currentIssueNumber: 7,
		activeStates:       []string{"Todo", "In Progress"},
		terminalStates:     []string{"Done", "Canceled"},
	}
}

func TestGiteaIssueLabelsClassifiesCurrentIssueHandoffMutation(t *testing.T) {
	cases := []struct {
		name          string
		issueNumber   int
		label         string
		currentLabels string
		want          ToolMutationAudit
	}{
		{
			name:        "non-active flip on current issue classifies handoff",
			issueNumber: 7,
			label:       "aiops/human-review",
			want:        ToolMutationAudit{CurrentIssueNonActiveStateUpdate: true},
		},
		{
			name:        "terminal flip on current issue classifies terminal handoff",
			issueNumber: 7,
			label:       "aiops/done",
			want: ToolMutationAudit{
				CurrentIssueNonActiveStateUpdate: true,
				CurrentIssueTerminalStateUpdate:  true,
				CurrentIssueTerminalState:        "Done",
			},
		},
		{
			name:        "active flip on current issue does not classify",
			issueNumber: 7,
			label:       "aiops/todo",
			want:        ToolMutationAudit{},
		},
		{
			name:        "non-active flip on another issue does not classify",
			issueNumber: 8,
			label:       "aiops/human-review",
			want:        ToolMutationAudit{},
		},
		{
			// The issue was already terminal before the write (e.g. an
			// operator's manual aiops/done); relabeling it must not be
			// misattributed as an agent handoff out of the active set.
			name:          "terminal pre-state does not classify",
			issueNumber:   7,
			label:         "aiops/human-review",
			currentLabels: `[{"id":101,"name":"aiops/done"}]`,
			want:          ToolMutationAudit{},
		},
		{
			// Pre-state derives to Human Review (non-active wins over the
			// stale terminal label by mapping priority): cleaning up the
			// stale label is a write but not a flip out of the active set.
			name:          "non-active pre-state stale cleanup does not classify",
			issueNumber:   7,
			label:         "aiops/human-review",
			currentLabels: `[{"id":102,"name":"aiops/human-review"},{"id":103,"name":"aiops/canceled"}]`,
			want:          ToolMutationAudit{},
		},
		{
			// No aiops/* state label at all before the write: the pre-state
			// is unknown, so the conservative verdict is "not a handoff".
			name:          "missing pre-state does not classify",
			issueNumber:   7,
			label:         "aiops/human-review",
			currentLabels: `[{"id":202,"name":"bug"}]`,
			want:          ToolMutationAudit{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			currentLabels := tc.currentLabels
			if currentLabels == "" {
				currentLabels = `[{"id":101,"name":"aiops/in-progress"}]`
			}
			server := giteaEchoLabelServer(currentLabels)
			defer server.Close()
			proxy := giteaClassificationProxy(server.URL)
			proxy.http = server.Client()

			var audits []ToolMutationAudit
			ctx := WithToolMutationSink(context.Background(), func(audit ToolMutationAudit) {
				audits = append(audits, audit)
			})
			result, err := proxy.call(ctx, ToolCall{IssueNumber: tc.issueNumber, Labels: []string{tc.label}})
			if err != nil {
				t.Fatalf("call(%d, %q) error = %v; want nil", tc.issueNumber, tc.label, err)
			}
			if !toolResultSucceeded(result) {
				t.Fatalf("call(%d, %q) result = %s; want success", tc.issueNumber, tc.label, result)
			}
			if len(audits) != 1 {
				t.Fatalf("mutation sink fired %d times for (%d, %q); want 1", len(audits), tc.issueNumber, tc.label)
			}
			if audits[0] != tc.want {
				t.Fatalf("classifyLabelMutation(%d, %q) audit = %+v; want %+v", tc.issueNumber, tc.label, audits[0], tc.want)
			}
		})
	}
}

// The retry that only deletes a stale active label (the desired label landed in
// an earlier partial replace) is the write completing the handoff and must
// classify; a replace whose stale-label delete fails must fire no audit at all,
// because the surviving active label keeps the derived state active.
func TestGiteaIssueLabelsStaleDeleteRetryAndPartialFailureAuditContract(t *testing.T) {
	t.Run("stale-delete-only retry classifies handoff", func(t *testing.T) {
		server := giteaEchoLabelServer(`[{"id":101,"name":"aiops/in-progress"},{"id":102,"name":"aiops/human-review"}]`)
		defer server.Close()
		proxy := giteaClassificationProxy(server.URL)
		proxy.http = server.Client()

		var audits []ToolMutationAudit
		ctx := WithToolMutationSink(context.Background(), func(audit ToolMutationAudit) {
			audits = append(audits, audit)
		})
		result, err := proxy.call(ctx, ToolCall{IssueNumber: 7, Labels: []string{"aiops/human-review"}})
		if err != nil {
			t.Fatalf("call error = %v; want nil", err)
		}
		if !toolResultSucceeded(result) {
			t.Fatalf("call result = %s; want success", result)
		}
		want := ToolMutationAudit{CurrentIssueNonActiveStateUpdate: true}
		if len(audits) != 1 || audits[0] != want {
			t.Fatalf("audits = %+v; want exactly [%+v]", audits, want)
		}
	})
	t.Run("zero-write no-op fires no audit", func(t *testing.T) {
		// The label is already in the desired shape, so the call makes no HTTP
		// write. The flip happened elsewhere (an earlier audited call or an
		// operator's manual edit) and must not be attributed to the agent.
		server := giteaEchoLabelServer(`[{"id":102,"name":"aiops/human-review"}]`)
		defer server.Close()
		proxy := giteaClassificationProxy(server.URL)
		proxy.http = server.Client()

		var audits []ToolMutationAudit
		ctx := WithToolMutationSink(context.Background(), func(audit ToolMutationAudit) {
			audits = append(audits, audit)
		})
		result, err := proxy.call(ctx, ToolCall{IssueNumber: 7, Labels: []string{"aiops/human-review"}})
		if err != nil {
			t.Fatalf("call error = %v; want nil", err)
		}
		if !toolResultSucceeded(result) {
			t.Fatalf("call result = %s; want success", result)
		}
		if len(audits) != 0 {
			t.Fatalf("audits = %+v; want none for a zero-write no-op", audits)
		}
	})
	t.Run("failed stale delete with surviving higher-priority active label fires no audit", func(t *testing.T) {
		// aiops/in-progress outranks aiops/human-review in the mapping
		// priority, so the surviving stale label keeps the derived state
		// active: the issue never left the active set and the agent's retry
		// will carry the signal.
		server := giteaEchoLabelServer(`[{"id":101,"name":"aiops/in-progress"}]`, "101")
		defer server.Close()
		proxy := giteaClassificationProxy(server.URL)
		proxy.http = server.Client()

		var audits []ToolMutationAudit
		ctx := WithToolMutationSink(context.Background(), func(audit ToolMutationAudit) {
			audits = append(audits, audit)
		})
		result, err := proxy.call(ctx, ToolCall{IssueNumber: 7, Labels: []string{"aiops/human-review"}})
		if err != nil {
			t.Fatalf("call error = %v; want nil (structured failure result)", err)
		}
		if toolResultSucceeded(result) {
			t.Fatalf("call result = %s; want failure when stale delete fails", result)
		}
		if len(audits) != 0 {
			t.Fatalf("audits = %+v; want none for a partial replace that stays active", audits)
		}
	})
	t.Run("failed aiops/todo delete after successful add still fires handoff", func(t *testing.T) {
		// Codex P2 on #751: aiops/human-review outranks aiops/todo, so the
		// successful add alone flips the derived state to Human Review —
		// reconcile will stop the run on the next poll even though the
		// stale delete failed. Skipping the audit here would lose the
		// handoff exactly like the bug #748 fixes.
		server := giteaEchoLabelServer(`[{"id":103,"name":"aiops/todo"}]`, "103")
		defer server.Close()
		proxy := giteaClassificationProxy(server.URL)
		proxy.http = server.Client()

		var audits []ToolMutationAudit
		ctx := WithToolMutationSink(context.Background(), func(audit ToolMutationAudit) {
			audits = append(audits, audit)
		})
		result, err := proxy.call(ctx, ToolCall{IssueNumber: 7, Labels: []string{"aiops/human-review"}})
		if err != nil {
			t.Fatalf("call error = %v; want nil (structured failure result)", err)
		}
		if toolResultSucceeded(result) {
			t.Fatalf("call result = %s; want failure when stale delete fails", result)
		}
		want := ToolMutationAudit{CurrentIssueNonActiveStateUpdate: true}
		if len(audits) != 1 || audits[0] != want {
			t.Fatalf("audits = %+v; want exactly [%+v] (landed add already left the active set)", audits, want)
		}
	})
	t.Run("partial delete accounting derives post state from landed writes", func(t *testing.T) {
		// Two stale active labels; the in-progress delete lands, the todo
		// delete fails. The landed writes leave {todo, human-review}, which
		// derives Human Review (non-active) → handoff fires. If the audit
		// ignored which deletes landed and used the full pre-write set, the
		// surviving in-progress would wrongly keep the verdict active.
		server := giteaEchoLabelServer(`[{"id":101,"name":"aiops/in-progress"},{"id":103,"name":"aiops/todo"}]`, "103")
		defer server.Close()
		proxy := giteaClassificationProxy(server.URL)
		proxy.http = server.Client()

		var audits []ToolMutationAudit
		ctx := WithToolMutationSink(context.Background(), func(audit ToolMutationAudit) {
			audits = append(audits, audit)
		})
		result, err := proxy.call(ctx, ToolCall{IssueNumber: 7, Labels: []string{"aiops/human-review"}})
		if err != nil {
			t.Fatalf("call error = %v; want nil (structured failure result)", err)
		}
		if toolResultSucceeded(result) {
			t.Fatalf("call result = %s; want failure when a stale delete fails", result)
		}
		want := ToolMutationAudit{CurrentIssueNonActiveStateUpdate: true}
		if len(audits) != 1 || audits[0] != want {
			t.Fatalf("audits = %+v; want exactly [%+v] (post state from landed writes)", audits, want)
		}
	})
}

// TestDynamicToolsWireGiteaCurrentIssueClassification drives the production
// construction seam (#748, clean-code rule 11): DynamicToolsForWorkflow +
// WithCurrentIssueToolGuard must thread the "#N" identifier and the tracker
// state sets into the Gitea proxy. The bare-numeric task ID is deliberately a
// different number than the issue, pinning that classification keys on the
// "#N" identifier, never on the Gitea-internal id.
func TestDynamicToolsWireGiteaCurrentIssueClassification(t *testing.T) {
	server := giteaEchoLabelServer(`[{"id":101,"name":"aiops/in-progress"}]`)
	defer server.Close()

	tools := DynamicToolsForWorkflow(workflow.Workflow{Config: workflow.Config{
		Repo: workflow.RepoConfig{Owner: "owner", Name: "repo"},
		Tracker: workflow.TrackerConfig{
			Kind:           "gitea",
			APIKey:         "token",
			Endpoint:       server.URL,
			ActiveStates:   []string{"Todo", "In Progress"},
			TerminalStates: []string{"Done", "Canceled"},
		},
	}}, WithCurrentIssueToolGuard("99999", "#7", nil))
	tool, ok := tools.Lookup("gitea_issue_labels")
	if !ok {
		t.Fatalf("gitea_issue_labels tool not advertised; tools=%#v", tools.Names())
	}

	var audits []ToolMutationAudit
	ctx := WithToolMutationSink(context.Background(), func(audit ToolMutationAudit) {
		audits = append(audits, audit)
	})
	result, err := tool.Call(ctx, ToolCall{IssueNumber: 7, Labels: []string{"aiops/human-review"}})
	if err != nil {
		t.Fatalf("tool.Call error = %v; want nil", err)
	}
	if !toolResultSucceeded(result) {
		t.Fatalf("tool.Call result = %s; want success", result)
	}
	want := ToolMutationAudit{CurrentIssueNonActiveStateUpdate: true}
	if len(audits) != 1 || audits[0] != want {
		t.Fatalf("audits = %+v; want exactly [%+v]", audits, want)
	}
}
