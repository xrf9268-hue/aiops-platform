package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		Codex: workflow.CommandConfig{
			LinearGraphQL: workflow.LinearGraphQLConfig{AllowMutations: true},
		},
	}})

	tool, ok := tools.Lookup("linear_graphql")
	if !ok {
		t.Fatalf("linear_graphql tool not advertised; tools=%#v", tools.Names())
	}
	if strings.Contains(tool.Description, token) {
		t.Fatalf("tool description leaked Linear token: %q", tool.Description)
	}
	schemaBytes, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("tool input schema is not JSON-marshalable: %v", err)
	}
	if !strings.Contains(string(schemaBytes), `"query"`) || strings.Contains(string(schemaBytes), token) {
		t.Fatalf("tool input schema = %s, want query field and no token leak", schemaBytes)
	}

	proxy := linearGraphQLProxy{apiKey: token, baseURL: httpServer.URL, http: httpServer.Client(), allowMutations: true}
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
		t.Fatalf("Authorization = %q, want raw Linear API key matching tracker client auth", auth)
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

func TestLinearAuthorizationHeaderUsesLinearAPIKeyFormat(t *testing.T) {
	if got := linearAuthorizationHeader("  lin_api_secret  "); got != "lin_api_secret" {
		t.Fatalf("linearAuthorizationHeader(raw key) = %q, want raw trimmed key", got)
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

func TestDynamicToolsDoNotExposeLinearToolsWithoutLinearToken(t *testing.T) {
	for _, wf := range []workflow.Workflow{
		{},
		{Config: workflow.Config{Tracker: workflow.TrackerConfig{Kind: "linear"}}},
		{Config: workflow.Config{Tracker: workflow.TrackerConfig{Kind: "gitea", APIKey: "token"}}},
	} {
		tools := DynamicToolsForWorkflow(wf)
		for _, name := range []string{"linear_graphql", "linear_ai_workpad"} {
			if _, ok := tools.Lookup(name); ok {
				t.Fatalf("%s advertised without configured Linear token: %#v", name, wf.Config.Tracker)
			}
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
			apiKey:         "token",
			baseURL:        httpServer.URL,
			http:           httpServer.Client(),
			allowMutations: true,
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

func TestLinearGraphQLRejectsMultipleOperationsWithoutHTTPRequest(t *testing.T) {
	server := &fakeLinearGraphQLServer{}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	result, err := linearGraphQLProxy{apiKey: "token", baseURL: httpServer.URL, http: httpServer.Client()}.
		call(context.Background(), ToolCall{Query: "query Viewer { viewer { id } } mutation Update { issueUpdate(id: \"1\", input: {}) { success } }"})
	assertStructuredFailure(t, result, err, "linear_graphql query must contain exactly one operation")
	_, _, requests := server.recorded()
	if requests != 0 {
		t.Fatalf("server received %d requests, want 0", requests)
	}
}

func TestLinearGraphQLRejectsMultipleAnonymousOperationsWithoutHTTPRequest(t *testing.T) {
	server := &fakeLinearGraphQLServer{}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	result, err := linearGraphQLProxy{apiKey: "token", baseURL: httpServer.URL, http: httpServer.Client()}.
		call(context.Background(), ToolCall{Query: `{ viewer { id } }
{ issue(id: "1") { title } }`})
	assertStructuredFailure(t, result, err, "linear_graphql query must contain exactly one operation")
	_, _, requests := server.recorded()
	if requests != 0 {
		t.Fatalf("server received %d requests, want 0", requests)
	}
}

func TestCountGraphQLOperationsIgnoresFragmentDefinitions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  int
	}{
		{
			name:  "named query with fragment",
			query: `query Q { ...F } fragment F on T { id }`,
			want:  1,
		},
		{
			name:  "named mutation with fragment",
			query: `mutation M { issueUpdate(id: "1", input: {}) { success } } fragment F on T { id title }`,
			want:  1,
		},
		{
			name:  "anonymous operation with fragment",
			query: `{ viewer { ...F } } fragment F on T { id }`,
			want:  1,
		},
		{
			name:  "fragment directive input object",
			query: `query Q { ...F } fragment F on T @cache(config: { ttl: 60 }) { id }`,
			want:  1,
		},
		{
			name:  "multiple operations still counted",
			query: `query A { viewer { id } } query B { issue(id: "1") { id } }`,
			want:  2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := countGraphQLOperations(tc.query); got != tc.want {
				t.Fatalf("countGraphQLOperations(%q) = %d, want %d", tc.query, got, tc.want)
			}
		})
	}
}

func TestLinearGraphQLAllowsSingleAnonymousOperation(t *testing.T) {
	server := &fakeLinearGraphQLServer{}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	result, err := linearGraphQLProxy{apiKey: "token", baseURL: httpServer.URL, http: httpServer.Client()}.
		call(context.Background(), ToolCall{Query: `{ viewer { id } }`})
	if err != nil {
		t.Fatalf("linear_graphql call: %v", err)
	}
	var payload struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		t.Fatalf("result is not structured JSON: %v", err)
	}
	if !payload.Success {
		t.Fatalf("success = false, want true; result=%s", result)
	}
	_, _, requests := server.recorded()
	if requests != 1 {
		t.Fatalf("server received %d requests, want 1", requests)
	}
}

func TestLinearGraphQLAllowsSingleOperationWithFragments(t *testing.T) {
	tests := []struct {
		name           string
		query          string
		allowMutations bool
	}{
		{
			name:  "named query",
			query: `query Q { ...F } fragment F on Issue { id }`,
		},
		{
			name:           "named mutation",
			query:          `mutation M { issueUpdate(id: "1", input: {}) { issue { ...F } } } fragment F on Issue { id }`,
			allowMutations: true,
		},
		{
			name: "anonymous query",
			query: `{ viewer { id } }
fragment F on Issue { id }`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &fakeLinearGraphQLServer{}
			httpServer := httptest.NewServer(server.handler())
			defer httpServer.Close()

			result, err := linearGraphQLProxy{apiKey: "token", baseURL: httpServer.URL, http: httpServer.Client(), allowMutations: tt.allowMutations}.
				call(context.Background(), ToolCall{Query: tt.query})
			if err != nil {
				t.Fatalf("linear_graphql call: %v", err)
			}
			var payload struct {
				Success bool `json:"success"`
			}
			if err := json.Unmarshal([]byte(result), &payload); err != nil {
				t.Fatalf("result is not structured JSON: %v", err)
			}
			if !payload.Success {
				t.Fatalf("success = false, want true; result=%s", result)
			}
			_, _, requests := server.recorded()
			if requests != 1 {
				t.Fatalf("server received %d requests, want 1", requests)
			}
		})
	}
}

func TestLinearGraphQLAllowsOperationWordsInsideSingleOperationBody(t *testing.T) {
	server := &fakeLinearGraphQLServer{}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	result, err := linearGraphQLProxy{apiKey: "token", baseURL: httpServer.URL, http: httpServer.Client()}.
		call(context.Background(), ToolCall{Query: `query Viewer(query: String, $query: String!, $mutation: String!) {
  # mutation and subscription are words inside this operation body, not operations.
  mutation: viewer { id }
  search(term: "query mutation subscription") { id }
  issues(filter: { title: { contains: $query }, description: { contains: $mutation } }) { nodes { id } }
}`})
	if err != nil {
		t.Fatalf("linear_graphql call: %v", err)
	}
	var payload struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		t.Fatalf("result is not structured JSON: %v", err)
	}
	if !payload.Success {
		t.Fatalf("success = false, want true; result=%s", result)
	}
	_, _, requests := server.recorded()
	if requests != 1 {
		t.Fatalf("server received %d requests, want 1", requests)
	}
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

func TestLinearGraphQLReturnsStructuredFailureForOversizedResponse(t *testing.T) {
	oversized := strings.Repeat("a", maxLinearGraphQLResponseBytes+1)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, oversized)
	}))
	defer httpServer.Close()

	result, err := linearGraphQLProxy{apiKey: "token", baseURL: httpServer.URL, http: httpServer.Client()}.
		call(context.Background(), ToolCall{Query: "query { viewer { id } }"})
	assertStructuredFailure(t, result, err, "Linear GraphQL response exceeded maximum size")
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

// TestLinearGraphQLDefaultRejectsMutationsBeforeHTTPRequest exercises the
// SPEC §15.5 default-deny path (#298): with the zero value of the
// LinearGraphQL config, the proxy refuses every mutation with a typed
// error and never dispatches an HTTP request, so prompt-injected
// `issueDelete` / `commentDelete` mutations cannot reach Linear.
func TestLinearGraphQLDefaultRejectsMutationsBeforeHTTPRequest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
	}{
		{name: "issue_delete", query: `mutation { issueDelete(id: "1") { success } }`},
		{name: "comment_delete", query: `mutation Delete { commentDelete(id: "c1") { success } }`},
		{name: "team_update", query: `mutation { teamUpdate(id: "t1", input: {}) { success } }`},
		{name: "anonymous_mutation_with_directive", query: `mutation @auth(token: "x") { issueArchive(id: "1") { success } }`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &fakeLinearGraphQLServer{}
			httpServer := httptest.NewServer(server.handler())
			defer httpServer.Close()

			proxy := linearGraphQLProxy{apiKey: "token", baseURL: httpServer.URL, http: httpServer.Client()}
			result, err := proxy.call(context.Background(), ToolCall{Query: tc.query})
			assertStructuredFailure(t, result, err, "mutations are disabled by this workflow", "codex.linear_graphql.allow_mutations")
			if _, _, requests := server.recorded(); requests != 0 {
				t.Fatalf("server received %d requests, want 0 for blocked mutation", requests)
			}
		})
	}
}

// TestLinearGraphQLAllowsMutationsWhenOpted verifies the workflow opt-in
// turns the gate off: with AllowMutations=true the proxy dispatches the
// mutation to Linear exactly once.
func TestLinearGraphQLAllowsMutationsWhenOpted(t *testing.T) {
	server := &fakeLinearGraphQLServer{}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	proxy := linearGraphQLProxy{apiKey: "token", baseURL: httpServer.URL, http: httpServer.Client(), allowMutations: true}
	result, err := proxy.call(context.Background(), ToolCall{Query: `mutation IssueUpdate { issueUpdate(id: "1", input: {}) { success } }`})
	if err != nil {
		t.Fatalf("linear_graphql call: %v", err)
	}
	if !toolResultSucceeded(result) {
		t.Fatalf("success = false, want true; result=%s", result)
	}
	if _, _, requests := server.recorded(); requests != 1 {
		t.Fatalf("server received %d requests, want 1", requests)
	}
}

// TestLinearGraphQLAllowListRestrictsMutationFieldNames covers the
// per-operation allow-list (#298 Layer 2). issueUpdate is allowed;
// issueDelete is not, even though AllowMutations is true.
func TestLinearGraphQLAllowListRestrictsMutationFieldNames(t *testing.T) {
	server := &fakeLinearGraphQLServer{}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	proxy := linearGraphQLProxy{
		apiKey:           "token",
		baseURL:          httpServer.URL,
		http:             httpServer.Client(),
		allowMutations:   true,
		allowedMutations: linearGraphQLAllowSet([]string{"issueUpdate", "commentCreate"}),
	}

	allowed, err := proxy.call(context.Background(), ToolCall{Query: `mutation { issueUpdate(id: "1", input: {}) { success } }`})
	if err != nil {
		t.Fatalf("allowed mutation: %v", err)
	}
	if !toolResultSucceeded(allowed) {
		t.Fatalf("allowed mutation did not succeed: %s", allowed)
	}

	blocked, err := proxy.call(context.Background(), ToolCall{Query: `mutation { issueDelete(id: "1") { success } }`})
	assertStructuredFailure(t, blocked, err, "not in the workflow's allowed_mutations list", "issueDelete")
	if _, _, requests := server.recorded(); requests != 1 {
		t.Fatalf("server received %d requests, want 1 (only the allowed mutation should have been dispatched)", requests)
	}
}

// TestLinearGraphQLRejectsSubscriptions ensures subscription operations
// never reach Linear regardless of mutation gate state — the runner has
// no streaming surface for them.
func TestLinearGraphQLRejectsSubscriptions(t *testing.T) {
	server := &fakeLinearGraphQLServer{}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	proxy := linearGraphQLProxy{apiKey: "token", baseURL: httpServer.URL, http: httpServer.Client(), allowMutations: true}
	result, err := proxy.call(context.Background(), ToolCall{Query: `subscription { issues { id } }`})
	assertStructuredFailure(t, result, err, "subscription")
	if _, _, requests := server.recorded(); requests != 0 {
		t.Fatalf("server received %d requests, want 0 for subscription", requests)
	}
}

// TestLinearGraphQLEmitsAuditEventForSuccessfulMutation covers the
// audit-trail layer (#298 Layer 3): when the context carries a mutation
// sink, the proxy fires it exactly once per successful mutation, with
// the top-level mutation field name and never with the query body.
func TestLinearGraphQLEmitsAuditEventForSuccessfulMutation(t *testing.T) {
	server := &fakeLinearGraphQLServer{}
	httpServer := httptest.NewServer(server.handler())
	defer httpServer.Close()

	proxy := linearGraphQLProxy{apiKey: "token", baseURL: httpServer.URL, http: httpServer.Client(), allowMutations: true}

	var sinkCalls []string
	sink := func(operationField string) {
		sinkCalls = append(sinkCalls, operationField)
	}
	ctx := WithLinearGraphQLMutationSink(context.Background(), sink)

	if _, err := proxy.call(ctx, ToolCall{Query: `mutation { issueUpdate(id: "1", input: {}) { success } }`}); err != nil {
		t.Fatalf("mutation: %v", err)
	}
	// Query operations must NOT fire the audit sink.
	if _, err := proxy.call(ctx, ToolCall{Query: `query { viewer { id } }`}); err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(sinkCalls) != 1 {
		t.Fatalf("sink fired %d times, want 1; calls=%v", len(sinkCalls), sinkCalls)
	}
	if sinkCalls[0] != "issueUpdate" {
		t.Fatalf("sink received %q, want issueUpdate", sinkCalls[0])
	}
}

// TestLinearGraphQLDoesNotEmitAuditOnGraphQLErrors makes sure the sink
// fires only when Linear actually accepted the mutation. A 200 OK with a
// `errors` envelope (Linear's typical GraphQL error shape) must not
// produce a tool_call_mutation event because the mutation did not run.
func TestLinearGraphQLDoesNotEmitAuditOnGraphQLErrors(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errors":[{"message":"forbidden"}]}`)
	}))
	defer httpServer.Close()

	proxy := linearGraphQLProxy{apiKey: "token", baseURL: httpServer.URL, http: httpServer.Client(), allowMutations: true}
	var sinkCalls []string
	ctx := WithLinearGraphQLMutationSink(context.Background(), func(name string) {
		sinkCalls = append(sinkCalls, name)
	})

	if _, err := proxy.call(ctx, ToolCall{Query: `mutation { issueUpdate(id: "1", input: {}) { success } }`}); err != nil {
		t.Fatalf("mutation: %v", err)
	}
	if len(sinkCalls) != 0 {
		t.Fatalf("audit fired on Linear-reported error: %v", sinkCalls)
	}
}

// TestParseLinearGraphQLOperationIdentifiesFieldNames is the parser-only
// unit test the gate relies on: it must surface the first top-level
// Mutation root field across normal whitespace, named/anonymous
// operations, header arguments, and leading fragment definitions.
func TestParseLinearGraphQLOperationIdentifiesFieldNames(t *testing.T) {
	for _, tc := range []struct {
		name      string
		query     string
		kind      linearGraphQLOperationKind
		fieldName string
	}{
		{
			name:      "named mutation",
			query:     `mutation Update { issueUpdate(id: "1", input: {}) { success } }`,
			kind:      linearGraphQLOperationMutation,
			fieldName: "issueUpdate",
		},
		{
			name:      "anonymous mutation",
			query:     `mutation { issueDelete(id: "1") { success } }`,
			kind:      linearGraphQLOperationMutation,
			fieldName: "issueDelete",
		},
		{
			name:      "named mutation with variables",
			query:     `mutation M($id: String!) { commentCreate(input: { issueId: $id, body: "" }) { success } }`,
			kind:      linearGraphQLOperationMutation,
			fieldName: "commentCreate",
		},
		{
			name:      "shorthand query",
			query:     `{ viewer { id } }`,
			kind:      linearGraphQLOperationQuery,
			fieldName: "viewer",
		},
		{
			name:      "subscription",
			query:     `subscription { issues { id } }`,
			kind:      linearGraphQLOperationSubscription,
			fieldName: "issues",
		},
		{
			name:      "mutation after fragment",
			query:     `fragment F on Issue { id } mutation { issueArchive(id: "1") { success } }`,
			kind:      linearGraphQLOperationMutation,
			fieldName: "issueArchive",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLinearGraphQLOperation(tc.query)
			if got.Kind != tc.kind {
				t.Fatalf("kind = %q, want %q", got.Kind, tc.kind)
			}
			if got.FieldName != tc.fieldName {
				t.Fatalf("fieldName = %q, want %q", got.FieldName, tc.fieldName)
			}
		})
	}
}

// TestParseLinearGraphQLOperationRejectsAdversarialFraming exercises the
// shapes a prompt-injection attempt would use to confuse the gate into
// mis-classifying a mutation as a query or hiding the mutation field
// name. The parser must see through GraphQL comments, single/triple
// string literals containing GraphQL-shaped text, operation-header
// directives, and leading fragment definitions.
func TestParseLinearGraphQLOperationRejectsAdversarialFraming(t *testing.T) {
	for _, tc := range []struct {
		name      string
		query     string
		kind      linearGraphQLOperationKind
		fieldName string
	}{
		{
			name:      "leading comment then mutation",
			query:     "# this comment mentions mutation { issueDelete } but is just text\nmutation { issueUpdate(id: \"1\", input: {}) { success } }",
			kind:      linearGraphQLOperationMutation,
			fieldName: "issueUpdate",
		},
		{
			name:      "leading comment then shorthand query",
			query:     "# mutation { issueDelete }\n{ viewer { id } }",
			kind:      linearGraphQLOperationQuery,
			fieldName: "viewer",
		},
		{
			name:      "default-value string contains mutation keyword",
			query:     `mutation M($x: String = "mutation { issueDelete }") { issueUpdate(id: "1", input: {}) { success } }`,
			kind:      linearGraphQLOperationMutation,
			fieldName: "issueUpdate",
		},
		{
			name:      "default-value string contains query shorthand",
			query:     `mutation M($x: String = "{ viewer { id } }") { commentCreate(input: { issueId: "1", body: "" }) { success } }`,
			kind:      linearGraphQLOperationMutation,
			fieldName: "commentCreate",
		},
		{
			name:      "triple-quoted block-string with braces",
			query:     `mutation { commentCreate(input: { issueId: "1", body: """{ inner } mutation { fake }""" }) { success } }`,
			kind:      linearGraphQLOperationMutation,
			fieldName: "commentCreate",
		},
		{
			name:      "operation-header directive",
			query:     `mutation M @auth(token: "x") { issueArchive(id: "1") { success } }`,
			kind:      linearGraphQLOperationMutation,
			fieldName: "issueArchive",
		},
		{
			name:      "leading fragment then mutation",
			query:     `fragment F on Issue { id title } mutation { issueDelete(id: "1") { success } }`,
			kind:      linearGraphQLOperationMutation,
			fieldName: "issueDelete",
		},
		{
			name:      "fragment with directive then mutation",
			query:     `fragment F on Issue @cache(ttl: 60) { id } mutation { issueUpdate(id: "1", input: {}) { success } }`,
			kind:      linearGraphQLOperationMutation,
			fieldName: "issueUpdate",
		},
		{
			name:      "mutation keyword inside escaped string",
			query:     `mutation { issueUpdate(id: "1", input: { description: "a \"mutation\" inside" }) { success } }`,
			kind:      linearGraphQLOperationMutation,
			fieldName: "issueUpdate",
		},
		{
			name:      "interior block does not shadow first field",
			query:     `mutation { issueDelete(id: "1") { success deletedIssue { id } } }`,
			kind:      linearGraphQLOperationMutation,
			fieldName: "issueDelete",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLinearGraphQLOperation(tc.query)
			if got.Kind != tc.kind {
				t.Fatalf("kind = %q, want %q", got.Kind, tc.kind)
			}
			if got.FieldName != tc.fieldName {
				t.Fatalf("fieldName = %q, want %q", got.FieldName, tc.fieldName)
			}
		})
	}
}

// TestLinearGraphQLAdversarialMutationsAreRejected end-to-ends the
// adversarial-framing cases through the gate: every mutation shape in
// the table must produce a structured failure and zero HTTP requests
// under default-deny.
func TestLinearGraphQLAdversarialMutationsAreRejected(t *testing.T) {
	queries := []string{
		"# innocuous comment\nmutation { issueDelete(id: \"1\") { success } }",
		`mutation M($x: String = "{ viewer { id } }") { issueDelete(id: "1") { success } }`,
		`fragment F on Issue { id } mutation { issueDelete(id: "1") { success } }`,
		`mutation M @auth(token: "x") { issueDelete(id: "1") { success } }`,
		`mutation { issueDelete(id: "1") { success deletedIssue { id } } }`,
	}
	for i, q := range queries {
		t.Run(fmt.Sprintf("query_%d", i), func(t *testing.T) {
			server := &fakeLinearGraphQLServer{}
			httpServer := httptest.NewServer(server.handler())
			defer httpServer.Close()

			proxy := linearGraphQLProxy{apiKey: "token", baseURL: httpServer.URL, http: httpServer.Client()}
			result, err := proxy.call(context.Background(), ToolCall{Query: q})
			assertStructuredFailure(t, result, err, "mutations are disabled by this workflow")
			if _, _, requests := server.recorded(); requests != 0 {
				t.Fatalf("server received %d requests for adversarial query %q, want 0", requests, q)
			}
		})
	}
}

// TestLinearGraphQLWorkpadEmitsAuditEvent verifies the should-fix from
// the review: harness-driven mutations dispatched via the workpad must
// also surface as tool_call_mutation events so operators see
// harness-attributable Linear writes alongside agent-driven ones. Both
// branches of the workpad's create/update fork are exercised so a
// future refactor that breaks field-name detection on either path is
// caught.
func TestLinearGraphQLWorkpadEmitsAuditEvent(t *testing.T) {
	tests := []struct {
		name      string
		findReply string
		wantField string
	}{
		{
			name:      "create_branch_when_no_existing_workpad",
			findReply: `{"data":{"issue":{"comments":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}}}}`,
			wantField: "commentCreate",
		},
		{
			name:      "update_branch_when_workpad_exists",
			findReply: `{"data":{"issue":{"comments":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[{"id":"comment-existing","body":"<!-- aiops:ai-workpad -->\n# AI Workpad\nold"}]}}}}`,
			wantField: "commentUpdate",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.Contains(string(body), "AIWorkpadFind"):
					_, _ = io.WriteString(w, tt.findReply)
				case strings.Contains(string(body), "commentUpdate"):
					_, _ = io.WriteString(w, `{"data":{"commentUpdate":{"success":true,"comment":{"id":"comment-existing"}}}}`)
				default:
					_, _ = io.WriteString(w, `{"data":{"commentCreate":{"success":true,"comment":{"id":"c-1"}}}}`)
				}
			}))
			defer httpServer.Close()

			proxy := linearGraphQLProxy{apiKey: "token", baseURL: httpServer.URL, http: httpServer.Client()}
			harnessTool := DynamicTool{Name: "linear_graphql", Call: proxy.callRaw}
			workpad := NewLinearWorkpadTool(harnessTool)

			var sinkCalls []string
			ctx := WithLinearGraphQLMutationSink(context.Background(), func(name string) {
				sinkCalls = append(sinkCalls, name)
			})

			result, err := workpad.Call(ctx, ToolCall{Variables: map[string]any{
				"issueId": "LIN-123",
				"summary": "test",
			}})
			if err != nil {
				t.Fatalf("workpad call: %v", err)
			}
			if !toolResultSucceeded(result) {
				t.Fatalf("workpad mutation failed: %s", result)
			}
			if len(sinkCalls) != 1 || sinkCalls[0] != tt.wantField {
				t.Fatalf("workpad audit sink calls = %v, want [%s]", sinkCalls, tt.wantField)
			}
		})
	}
}

// TestLinearGraphQLWorkpadCallsBypassMutationGate confirms the
// harness-owned linear_ai_workpad helper is not blocked by the gate,
// because the workpad composes deterministic comment mutations rather
// than executing agent-supplied GraphQL.
func TestLinearGraphQLWorkpadCallsBypassMutationGate(t *testing.T) {
	calls := 0
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls++
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(string(body), "AIWorkpadFind"):
			_, _ = io.WriteString(w, `{"data":{"issue":{"comments":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[]}}}}`)
		default:
			_, _ = io.WriteString(w, `{"data":{"commentCreate":{"success":true,"comment":{"id":"c-1"}}}}`)
		}
	}))
	defer httpServer.Close()

	oldEndpoint := defaultLinearGraphQLEndpoint
	defaultLinearGraphQLEndpoint = httpServer.URL
	t.Cleanup(func() { defaultLinearGraphQLEndpoint = oldEndpoint })

	tools := DynamicToolsForWorkflow(workflow.Workflow{Config: workflow.Config{
		Tracker: workflow.TrackerConfig{Kind: "linear", APIKey: "token"},
		// Deliberately leave LinearGraphQL at its zero value: mutations
		// stay blocked for the agent-visible tool but the workpad must
		// still post comments through the harness-internal path.
	}})
	workpad, ok := tools.Lookup("linear_ai_workpad")
	if !ok {
		t.Fatalf("linear_ai_workpad not advertised")
	}

	result, err := workpad.Call(context.Background(), ToolCall{Variables: map[string]any{
		"issueId": "LIN-123",
		"summary": "test",
	}})
	if err != nil {
		t.Fatalf("workpad call: %v", err)
	}
	if !toolResultSucceeded(result) {
		t.Fatalf("workpad mutation blocked by gate (regressed harness-internal bypass): %s", result)
	}
	if calls != 2 {
		t.Fatalf("HTTP calls = %d, want 2 (find + create)", calls)
	}
}

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
