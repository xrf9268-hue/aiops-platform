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

type fakeLinearGraphQLServer struct {
	mu         sync.Mutex
	authHeader string
	body       string
	requests   int
}

func (f *fakeLinearGraphQLServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests++
		f.authHeader = r.Header.Get("Authorization")
		f.body = string(body)
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"issueUpdate":{"success":true}}}`)
	})
}

func (f *fakeLinearGraphQLServer) recorded() (string, string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authHeader, f.body, f.requests
}

func TestDynamicToolsExposeLinearGraphQLWithTokenIsolation(t *testing.T) {
	server := &fakeLinearGraphQLServer{}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	token := "lin_super_secret_test_token"
	tools := DynamicToolsForWorkflow(workflow.Workflow{Config: workflow.Config{
		Tracker: workflow.TrackerConfig{
			Kind:   "linear",
			APIKey: token,
		},
	}})

	tool, ok := tools.Lookup("linear_graphql")
	if !ok {
		t.Fatalf("linear_graphql tool not advertised; tools=%#v", tools.Names())
	}
	if strings.Contains(tool.Description, token) {
		t.Fatalf("tool description leaked Linear token: %q", tool.Description)
	}

	result, err := tool.Call(context.Background(), ToolCall{
		Query:     "mutation IssueUpdate($id: String!) { issueUpdate(id: $id, input: {}) { success } }",
		Variables: map[string]any{"id": "issue-1"},
		Endpoint:  httpServer.URL,
	})
	if err != nil {
		t.Fatalf("linear_graphql call: %v", err)
	}
	if strings.Contains(result, token) {
		t.Fatalf("tool result leaked Linear token: %q", result)
	}

	auth, body, _ := server.recorded()
	if auth != "Bearer "+token {
		t.Fatalf("Authorization = %q, want bearer token held by orchestrator", auth)
	}
	if strings.Contains(body, token) {
		t.Fatalf("GraphQL request body leaked token to agent-controlled payload: %s", body)
	}
	var payload struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	if payload.Query == "" || payload.Variables["id"] != "issue-1" {
		t.Fatalf("unexpected GraphQL payload: %#v", payload)
	}
}

func TestDynamicToolsDoNotExposeLinearGraphQLWithoutLinearToken(t *testing.T) {
	for _, wf := range []workflow.Workflow{
		{},
		{Config: workflow.Config{Tracker: workflow.TrackerConfig{Kind: "linear"}}},
		{Config: workflow.Config{Tracker: workflow.TrackerConfig{Kind: "gitea", APIKey: "token"}}},
	} {
		tools := DynamicToolsForWorkflow(wf)
		if _, ok := tools.Lookup("linear_graphql"); ok {
			t.Fatalf("linear_graphql advertised without configured Linear token: %#v", wf.Config.Tracker)
		}
	}
}

func TestLinearGraphQLReturnsFailurePayloadForGraphQLErrors(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errors":[{"message":"bad mutation"}]}`)
	}))
	defer httpServer.Close()

	tool := DynamicTool{
		Name: "linear_graphql",
		Call: linearGraphQLProxy{
			apiKey:  "token",
			baseURL: httpServer.URL,
		}.call,
	}

	result, err := tool.Call(context.Background(), ToolCall{Query: "mutation { issueUpdate(id: \"1\", input: {}) { success } }"})
	if err != nil {
		t.Fatalf("linear_graphql transport returned error; want structured failure payload: %v", err)
	}
	var payload struct {
		Success bool   `json:"success"`
		Output  string `json:"output"`
	}
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		t.Fatalf("result is not structured JSON: %v\n%s", err, result)
	}
	if payload.Success {
		t.Fatalf("success = true, want false for GraphQL errors; result=%s", result)
	}
	if !strings.Contains(payload.Output, "bad mutation") {
		t.Fatalf("failure output did not preserve GraphQL response: %s", payload.Output)
	}
}

func TestLinearGraphQLRejectsInvalidVariablesWithoutHTTPRequest(t *testing.T) {
	server := &fakeLinearGraphQLServer{}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	tool := DynamicTool{
		Name: "linear_graphql",
		Call: linearGraphQLProxy{
			apiKey:  "token",
			baseURL: httpServer.URL,
		}.call,
	}

	result, err := tool.Call(context.Background(), ToolCall{
		Query:     "query { viewer { id } }",
		Variables: map[string]any{"bad": func() {}},
	})
	if err != nil {
		t.Fatalf("invalid input returned Go error; want structured tool failure: %v", err)
	}
	_, _, requests := server.recorded()
	if requests != 0 {
		t.Fatalf("HTTP requests = %d, want 0 for invalid input", requests)
	}
	if !strings.Contains(result, "success") || !strings.Contains(result, "false") {
		t.Fatalf("result did not look like structured failure payload: %s", result)
	}
}
