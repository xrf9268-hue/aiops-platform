package runner

import (
	"context"
	"encoding/json"
	"errors"
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

	proxy := linearGraphQLProxy{apiKey: token, baseURL: httpServer.URL, http: httpServer.Client()}
	result, err := proxy.call(context.Background(), ToolCall{
		Query:     "mutation IssueUpdate($id: String!) { issueUpdate(id: $id, input: {}) { success } }",
		Variables: map[string]any{"id": "issue-1"},
	})
	if err != nil {
		t.Fatalf("linear_graphql call: %v", err)
	}
	if strings.Contains(result, token) {
		t.Fatalf("tool result leaked Linear token: %q", result)
	}

	auth, body, _ := server.recorded()
	if auth != token {
		t.Fatalf("Authorization = %q, want raw Linear token held by orchestrator", auth)
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

func TestLinearGraphQLIgnoresAgentSuppliedEndpoint(t *testing.T) {
	good := &fakeLinearGraphQLServer{}
	goodServer := httptest.NewServer(good.handler())
	defer goodServer.Close()

	evil := &fakeLinearGraphQLServer{}
	evilServer := httptest.NewServer(evil.handler())
	defer evilServer.Close()

	var call ToolCall
	if err := json.Unmarshal([]byte(`{"query":"query { viewer { id } }","endpoint":"`+evilServer.URL+`"}`), &call); err != nil {
		t.Fatalf("unmarshal ToolCall: %v", err)
	}

	proxy := linearGraphQLProxy{apiKey: "token", baseURL: goodServer.URL, http: goodServer.Client()}
	if _, err := proxy.call(context.Background(), call); err != nil {
		t.Fatalf("linear_graphql call: %v", err)
	}

	_, _, goodRequests := good.recorded()
	_, _, evilRequests := evil.recorded()
	if goodRequests != 1 {
		t.Fatalf("configured endpoint requests = %d, want 1", goodRequests)
	}
	if evilRequests != 0 {
		t.Fatalf("agent-supplied endpoint received %d requests, want 0", evilRequests)
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
			http:    httpServer.Client(),
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
			http:    httpServer.Client(),
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

func TestLinearGraphQLReturnsStructuredFailureForEmptyQuery(t *testing.T) {
	result, err := linearGraphQLProxy{apiKey: "token", baseURL: defaultLinearGraphQLEndpoint}.
		call(context.Background(), ToolCall{Query: "   "})
	assertStructuredFailure(t, result, err, "linear_graphql query is required")
}

func TestLinearGraphQLReturnsStructuredFailureForHTTPStatus(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"errors":[{"message":"unauthorized"}]}`, http.StatusUnauthorized)
	}))
	defer httpServer.Close()

	result, err := linearGraphQLProxy{apiKey: "token", baseURL: httpServer.URL, http: httpServer.Client()}.
		call(context.Background(), ToolCall{Query: "query { viewer { id } }"})
	assertStructuredFailure(t, result, err, "401 Unauthorized", "unauthorized")
}

func TestLinearGraphQLReturnsStructuredFailureForRequestBuildError(t *testing.T) {
	result, err := linearGraphQLProxy{apiKey: "token", baseURL: ":// bad-url"}.
		call(context.Background(), ToolCall{Query: "query { viewer { id } }"})
	assertStructuredFailure(t, result, err, "Linear GraphQL request could not be built")
}

func TestLinearGraphQLReturnsStructuredFailureForTransportError(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network down")
	})}
	result, err := linearGraphQLProxy{apiKey: "token", baseURL: defaultLinearGraphQLEndpoint, http: client}.
		call(context.Background(), ToolCall{Query: "query { viewer { id } }"})
	assertStructuredFailure(t, result, err, "Linear GraphQL request failed during transport", "network down")
}

func TestLinearGraphQLReturnsStructuredFailureForBodyReadError(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: errReadCloser{}}, nil
	})}
	result, err := linearGraphQLProxy{apiKey: "token", baseURL: defaultLinearGraphQLEndpoint, http: client}.
		call(context.Background(), ToolCall{Query: "query { viewer { id } }"})
	assertStructuredFailure(t, result, err, "Linear GraphQL response body could not be read", "read boom")
}

func TestDynamicToolResultUsesSymphonyContentItemType(t *testing.T) {
	result, err := dynamicToolResult(true, `{"data":{}}`)
	if err != nil {
		t.Fatalf("dynamicToolResult: %v", err)
	}
	var payload struct {
		ContentItems []struct {
			Type string `json:"type"`
		} `json:"contentItems"`
	}
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		t.Fatalf("result is not JSON: %v", err)
	}
	if len(payload.ContentItems) != 1 || payload.ContentItems[0].Type != "inputText" {
		t.Fatalf("contentItems = %#v, want Symphony inputText item", payload.ContentItems)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type errReadCloser struct{}

func (errReadCloser) Read([]byte) (int, error) { return 0, errors.New("read boom") }
func (errReadCloser) Close() error             { return nil }

func assertStructuredFailure(t *testing.T, result string, err error, substrings ...string) {
	t.Helper()
	if err != nil {
		t.Fatalf("returned Go error %v; want structured tool failure", err)
	}
	var payload struct {
		Success bool   `json:"success"`
		Output  string `json:"output"`
	}
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		t.Fatalf("result is not structured JSON: %v\n%s", err, result)
	}
	if payload.Success {
		t.Fatalf("success = true, want false; result=%s", result)
	}
	for _, substring := range substrings {
		if !strings.Contains(payload.Output, substring) {
			t.Fatalf("output %q does not contain %q", payload.Output, substring)
		}
	}
}
