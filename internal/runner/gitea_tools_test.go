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

func (f *fakeGiteaLabelServer) recorded() (string, string, string, string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	method, path, body := "", "", ""
	if len(f.methods) > 0 {
		method = f.methods[len(f.methods)-1]
	}
	if len(f.paths) > 0 {
		path = f.paths[len(f.paths)-1]
	}
	if len(f.bodies) > 0 {
		body = f.bodies[len(f.bodies)-1]
	}
	return f.authHeader, method, path, body, f.requests
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
			Kind:        "gitea",
			APIKey:      token,
			ProjectSlug: "https://gitea.example.test/",
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
	if !strings.Contains(string(schemaBytes), `"issue_number"`) || strings.Contains(string(schemaBytes), token) {
		t.Fatalf("tool input schema = %s, want issue_number field and no token leak", schemaBytes)
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

	auth, _, _, body, requests := server.recorded()
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
	body = bodies[1]
	if strings.Contains(body, token) || !strings.Contains(body, "aiops/in-progress") {
		t.Fatalf("unexpected request body: %s", body)
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
	_, _, _, _, requests := server.recorded()
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
	assertStructuredFailure(t, result, err, "gitea_issue_labels label must be one of: aiops/canceled, aiops/done, aiops/human-review, aiops/in-progress, aiops/rework, aiops/todo")
	_, _, _, _, requests := server.recorded()
	if requests != 0 {
		t.Fatalf("server received %d requests, want 0", requests)
	}
}

func TestGiteaIssueLabelsRejectsMissingIssueNumberWithoutHTTPRequest(t *testing.T) {
	server := &fakeGiteaLabelServer{}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	result, err := giteaIssueLabelsProxy{token: "token", baseURL: httpServer.URL, owner: "owner", repo: "repo", http: httpServer.Client()}.
		call(context.Background(), ToolCall{Labels: []string{"aiops/in-progress"}})
	assertStructuredFailure(t, result, err, "gitea_issue_labels issue_number is required")
	_, _, _, _, requests := server.recorded()
	if requests != 0 {
		t.Fatalf("server received %d requests, want 0", requests)
	}
}
