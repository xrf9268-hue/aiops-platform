package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type recordedToolCall struct {
	Query     string
	Variables map[string]any
}

func TestLinearWorkpadUpsertsExistingWorkpadComment(t *testing.T) {
	calls := []recordedToolCall{}
	linearGraphQL := DynamicTool{
		Name:        "linear_graphql",
		Description: "Execute Linear GraphQL without secrets.",
		Call: func(_ context.Context, call ToolCall) (string, error) {
			calls = append(calls, recordedToolCall{Query: call.Query, Variables: call.Variables})
			switch len(calls) {
			case 1:
				return dynamicToolResult(true, `{"data":{"issue":{"comments":{"nodes":[{"id":"comment-1","body":"<!-- aiops:ai-workpad -->\n# AI Workpad\nold"},{"id":"comment-2","body":"human note"}]}}}}`)
			case 2:
				return dynamicToolResult(true, `{"data":{"commentUpdate":{"success":true,"comment":{"id":"comment-1"}}}}`)
			default:
				t.Fatalf("unexpected extra linear_graphql call %#v", call)
				return "", nil
			}
		},
	}
	tool := NewLinearWorkpadTool(linearGraphQL)

	result, err := tool.Call(context.Background(), ToolCall{Variables: map[string]any{
		"issueId": "LIN-123",
		"branch":  "feat/issue-15-linear-ai-workpad",
		"prUrl":   "https://github.com/xrf9268-hue/aiops-platform/pull/123",
		"summary": "Implemented workpad helper",
		"blocker": "none",
		"next":    "wait for CI",
	}})
	if err != nil {
		t.Fatalf("linear_ai_workpad call: %v", err)
	}
	assertToolSuccess(t, result)
	if len(calls) != 2 {
		t.Fatalf("linear_graphql calls = %d, want query then update", len(calls))
	}
	if !strings.Contains(calls[0].Query, "comments") || !strings.Contains(calls[0].Query, "AIWorkpadFind") {
		t.Fatalf("first call did not query comments for deterministic lookup: %s", calls[0].Query)
	}
	if !strings.Contains(calls[1].Query, "commentUpdate") || strings.Contains(calls[1].Query, "commentCreate") {
		t.Fatalf("second call should update existing workpad only: %s", calls[1].Query)
	}
	if calls[1].Variables["commentId"] != "comment-1" {
		t.Fatalf("commentId = %#v, want comment-1", calls[1].Variables["commentId"])
	}
	body, _ := calls[1].Variables["body"].(string)
	for _, want := range []string{"<!-- aiops:ai-workpad -->", "# AI Workpad", "Current branch", "Pull request", "Run summary", "Last error/blocker", "Next action"} {
		if !strings.Contains(body, want) {
			t.Fatalf("workpad body missing %q:\n%s", want, body)
		}
	}
}

func TestLinearWorkpadIgnoresQuotedMarkerWithoutCanonicalHeading(t *testing.T) {
	calls := []recordedToolCall{}
	linearGraphQL := DynamicTool{
		Name: "linear_graphql",
		Call: func(_ context.Context, call ToolCall) (string, error) {
			calls = append(calls, recordedToolCall{Query: call.Query, Variables: call.Variables})
			switch len(calls) {
			case 1:
				return dynamicToolResult(true, `{"data":{"issue":{"comments":{"nodes":[{"id":"human-comment","body":"I copied this from the bot: <!-- aiops:ai-workpad -->"}]}}}}`)
			case 2:
				return dynamicToolResult(true, `{"data":{"commentCreate":{"success":true,"comment":{"id":"comment-new"}}}}`)
			default:
				t.Fatalf("unexpected extra linear_graphql call %#v", call)
				return "", nil
			}
		},
	}
	tool := NewLinearWorkpadTool(linearGraphQL)

	result, err := tool.Call(context.Background(), ToolCall{Variables: map[string]any{"issueId": "LIN-123"}})
	if err != nil {
		t.Fatalf("linear_ai_workpad call: %v", err)
	}
	assertToolSuccess(t, result)
	if len(calls) != 2 {
		t.Fatalf("linear_graphql calls = %d, want lookup then create", len(calls))
	}
	if !strings.Contains(calls[1].Query, "commentCreate") || strings.Contains(calls[1].Query, "commentUpdate") {
		t.Fatalf("quoted marker should not be updated as managed workpad: %s", calls[1].Query)
	}
}

func TestLinearWorkpadFindsExistingWorkpadCommentPastFirstPage(t *testing.T) {
	calls := []recordedToolCall{}
	linearGraphQL := DynamicTool{
		Name: "linear_graphql",
		Call: func(_ context.Context, call ToolCall) (string, error) {
			calls = append(calls, recordedToolCall{Query: call.Query, Variables: call.Variables})
			switch len(calls) {
			case 1:
				if _, ok := call.Variables["after"]; ok {
					t.Fatalf("first lookup should not pass an after cursor: %#v", call.Variables)
				}
				return dynamicToolResult(true, `{"data":{"issue":{"comments":{"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"},"nodes":[{"id":"comment-1","body":"human note"}]}}}}`)
			case 2:
				if call.Variables["after"] != "cursor-1" {
					t.Fatalf("second lookup after cursor = %#v, want cursor-1", call.Variables["after"])
				}
				return dynamicToolResult(true, `{"data":{"issue":{"comments":{"pageInfo":{"hasNextPage":false,"endCursor":"cursor-2"},"nodes":[{"id":"comment-2","body":"<!-- aiops:ai-workpad -->\n# AI Workpad\nold"}]}}}}`)
			case 3:
				return dynamicToolResult(true, `{"data":{"commentUpdate":{"success":true,"comment":{"id":"comment-2"}}}}`)
			default:
				t.Fatalf("unexpected extra linear_graphql call %#v", call)
				return "", nil
			}
		},
	}
	tool := NewLinearWorkpadTool(linearGraphQL)

	result, err := tool.Call(context.Background(), ToolCall{Variables: map[string]any{"issueId": "LIN-123"}})
	if err != nil {
		t.Fatalf("linear_ai_workpad call: %v", err)
	}
	assertToolSuccess(t, result)
	if len(calls) != 3 {
		t.Fatalf("linear_graphql calls = %d, want two lookup pages then update", len(calls))
	}
	if !strings.Contains(calls[2].Query, "commentUpdate") || strings.Contains(calls[2].Query, "commentCreate") {
		t.Fatalf("final call should update existing paginated workpad only: %s", calls[2].Query)
	}
	if calls[2].Variables["commentId"] != "comment-2" {
		t.Fatalf("commentId = %#v, want comment-2", calls[2].Variables["commentId"])
	}
}

func TestLinearWorkpadFailsWhenLookupCursorDoesNotAdvance(t *testing.T) {
	calls := []recordedToolCall{}
	linearGraphQL := DynamicTool{
		Name: "linear_graphql",
		Call: func(_ context.Context, call ToolCall) (string, error) {
			calls = append(calls, recordedToolCall{Query: call.Query, Variables: call.Variables})
			if len(calls) > 2 {
				t.Fatalf("lookup continued after cursor stopped advancing: %#v", calls)
			}
			return dynamicToolResult(true, `{"data":{"issue":{"comments":{"pageInfo":{"hasNextPage":true,"endCursor":"same-cursor"},"nodes":[{"id":"comment-1","body":"human note"}]}}}}`)
		},
	}
	tool := NewLinearWorkpadTool(linearGraphQL)

	result, err := tool.Call(context.Background(), ToolCall{Variables: map[string]any{"issueId": "LIN-123"}})

	assertStructuredFailure(t, result, err, "lookup pagination did not advance")
}

func TestLinearWorkpadCreatesCommentWhenMissing(t *testing.T) {
	calls := []recordedToolCall{}
	linearGraphQL := DynamicTool{
		Name:        "linear_graphql",
		Description: "Execute Linear GraphQL without secrets.",
		Call: func(_ context.Context, call ToolCall) (string, error) {
			calls = append(calls, recordedToolCall{Query: call.Query, Variables: call.Variables})
			switch len(calls) {
			case 1:
				return dynamicToolResult(true, `{"data":{"issue":{"comments":{"nodes":[{"id":"comment-2","body":"human note"}]}}}}`)
			case 2:
				return dynamicToolResult(true, `{"data":{"commentCreate":{"success":true,"comment":{"id":"comment-new"}}}}`)
			default:
				t.Fatalf("unexpected extra linear_graphql call %#v", call)
				return "", nil
			}
		},
	}
	tool := NewLinearWorkpadTool(linearGraphQL)

	result, err := tool.Call(context.Background(), ToolCall{Variables: map[string]any{"issueId": "LIN-123"}})
	if err != nil {
		t.Fatalf("linear_ai_workpad call: %v", err)
	}
	assertToolSuccess(t, result)
	if len(calls) != 2 {
		t.Fatalf("linear_graphql calls = %d, want query then create", len(calls))
	}
	if !strings.Contains(calls[1].Query, "commentCreate") || strings.Contains(calls[1].Query, "commentUpdate") {
		t.Fatalf("second call should create missing workpad only: %s", calls[1].Query)
	}
	if calls[1].Variables["issueId"] != "LIN-123" {
		t.Fatalf("issueId = %#v, want LIN-123", calls[1].Variables["issueId"])
	}
}

func TestLinearWorkpadFailsWhenLinearMutationReportsFailure(t *testing.T) {
	linearGraphQL := DynamicTool{
		Name: "linear_graphql",
		Call: func(_ context.Context, call ToolCall) (string, error) {
			if strings.Contains(call.Query, "AIWorkpadFind") {
				return dynamicToolResult(true, `{"data":{"issue":{"comments":{"nodes":[]}}}}`)
			}
			return dynamicToolResult(true, `{"data":{"commentCreate":{"success":false,"comment":null}}}`)
		},
	}
	tool := NewLinearWorkpadTool(linearGraphQL)

	result, err := tool.Call(context.Background(), ToolCall{Variables: map[string]any{"issueId": "LIN-123"}})

	assertStructuredFailure(t, result, err, "AI Workpad mutation did not succeed")
}

func TestLinearWorkpadAcceptsSchemaNestedVariables(t *testing.T) {
	calls := []recordedToolCall{}
	linearGraphQL := DynamicTool{
		Name: "linear_graphql",
		Call: func(_ context.Context, call ToolCall) (string, error) {
			calls = append(calls, recordedToolCall{Query: call.Query, Variables: call.Variables})
			switch len(calls) {
			case 1:
				return dynamicToolResult(true, `{"data":{"issue":{"comments":{"nodes":[]}}}}`)
			case 2:
				return dynamicToolResult(true, `{"data":{"commentCreate":{"success":true,"comment":{"id":"comment-new"}}}}`)
			default:
				t.Fatalf("unexpected extra linear_graphql call %#v", call)
				return "", nil
			}
		},
	}
	tool := NewLinearWorkpadTool(linearGraphQL)

	result, err := tool.Call(context.Background(), ToolCall{Variables: map[string]any{
		"variables": map[string]any{
			"issueId": "LIN-456",
			"summary": "nested schema input",
		},
	}})
	if err != nil {
		t.Fatalf("linear_ai_workpad call: %v", err)
	}
	assertToolSuccess(t, result)
	if len(calls) != 2 {
		t.Fatalf("linear_graphql calls = %d, want query then create", len(calls))
	}
	if calls[0].Variables["issueId"] != "LIN-456" || calls[1].Variables["issueId"] != "LIN-456" {
		t.Fatalf("nested variables were not normalized into GraphQL variables: %#v", calls)
	}
	body, _ := calls[1].Variables["body"].(string)
	if !strings.Contains(body, "nested schema input") {
		t.Fatalf("workpad body did not include nested summary: %s", body)
	}
}

func TestLinearWorkpadToolMetadataDoesNotExposeLinearToken(t *testing.T) {
	const token = "lin_super_secret_workpad_token"
	tools := DynamicToolsForWorkflow(workflow.Workflow{Config: workflow.Config{Tracker: workflow.TrackerConfig{Kind: "linear", APIKey: token}}})
	tool, ok := tools.Lookup("linear_ai_workpad")
	if !ok {
		t.Fatalf("linear_ai_workpad tool not advertised; tools=%#v", tools.Names())
	}
	metadata, err := json.Marshal(map[string]any{
		"name":        tool.Name,
		"description": tool.Description,
		"schema":      tool.InputSchema,
	})
	if err != nil {
		t.Fatalf("marshal tool metadata: %v", err)
	}
	if strings.Contains(string(metadata), token) {
		t.Fatalf("workpad metadata leaked Linear token: %s", metadata)
	}
	if strings.Contains(string(metadata), "apiKey") || strings.Contains(string(metadata), "Authorization") {
		t.Fatalf("workpad metadata exposes orchestrator auth details: %s", metadata)
	}
}

func TestLinearWorkpadFailureWhenIssueIDMissing(t *testing.T) {
	called := false
	tool := DynamicTool{
		Name: "linear_ai_workpad",
		Call: NewLinearWorkpadTool(DynamicTool{Name: "linear_graphql", Call: func(context.Context, ToolCall) (string, error) {
			called = true
			return dynamicToolResult(true, `{}`)
		}}).Call,
	}
	result, err := tool.Call(context.Background(), ToolCall{Variables: map[string]any{"branch": "feat/example"}})
	assertStructuredFailure(t, result, err, "issueId is required")
	if called {
		t.Fatalf("linear_graphql was called even though issueId was missing")
	}
}

func assertToolSuccess(t *testing.T, result string) {
	t.Helper()
	var payload struct {
		Success bool   `json:"success"`
		Output  string `json:"output"`
	}
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		t.Fatalf("result is not structured JSON: %v\n%s", err, result)
	}
	if !payload.Success {
		t.Fatalf("success = false, want true; output=%s", payload.Output)
	}
}
