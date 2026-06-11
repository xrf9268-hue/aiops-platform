package runner

// codex_app_server_approval_test.go holds characterization tests that drive
// (*appServerClient).handleDynamicToolCall directly, without spawning a real
// `codex app-server` subprocess. They pin the dynamic tool-call bridge's
// input→(wire reply, recorded runtime events) behavior at the exact function
// boundary before #499 decomposes the 20-cognitive-complexity handler into
// per-concern helpers; the assertions must hold identically before and after
// that refactor.
//
// The end-to-end subprocess tests in codex_app_server_test.go remain the
// authority for the transport and multi-turn paths; these focus on the
// tool-call demux the decomposition reshapes, especially branches the
// subprocess suite does not exercise (undecodable arguments, tool execution
// errors, the request-id vs. notification reply fork, and the linear_graphql
// mutation audit sink).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

// newDynamicToolCallClient builds an appServerClient whose server->client
// replies (request results and notifications) land in the returned stdin
// buffer. tools is the dynamic tool surface the handler looks names up against;
// a nil entry leaves the set empty so every lookup misses.
func newDynamicToolCallClient(tools map[string]DynamicTool) (*appServerClient, *bytes.Buffer) {
	stdin := &bytes.Buffer{}
	c := &appServerClient{
		stdin: stdin,
		tools: DynamicToolSet{tools: tools},
	}
	return c, stdin
}

// decodeSentMessage parses the single newline-delimited JSON-RPC message the
// handler wrote to stdin.
func decodeSentMessage(t *testing.T, stdin *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(stdin.String())
	if line == "" {
		t.Fatal("handler wrote no message to stdin")
	}
	var msg map[string]any
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		t.Fatalf("decode sent message %q: %v", line, err)
	}
	return msg
}

func TestHandleDynamicToolCall_ContextCancelledReturnsErrWithoutReply(t *testing.T) {
	c, stdin := newDynamicToolCallClient(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.handleDynamicToolCall(ctx, map[string]any{
		"id":     "call-1",
		"params": map[string]any{"tool": "linear_graphql"},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("handleDynamicToolCall(cancelled ctx) = %v; want context.Canceled", err)
	}
	if stdin.Len() != 0 {
		t.Errorf("handleDynamicToolCall(cancelled ctx) wrote %q to stdin; want no reply", stdin.String())
	}
}

func TestHandleDynamicToolCall_UnsupportedToolRecordsEventAndReplies(t *testing.T) {
	c, stdin := newDynamicToolCallClient(nil) // empty set: every lookup misses

	err := c.handleDynamicToolCall(context.Background(), map[string]any{
		"id":     "call-7",
		"params": map[string]any{"tool": "missing_tool", "arguments": map[string]any{"query": "q"}},
	})
	if err != nil {
		t.Fatalf("handleDynamicToolCall() = %v; want nil", err)
	}
	if names := runtimeEventNames(c); !slices.Contains(names, task.EventUnsupportedToolCall) {
		t.Errorf("runtime events = %v; want to include %q", names, task.EventUnsupportedToolCall)
	}
	sent := decodeSentMessage(t, stdin)
	if sent["id"] != "call-7" {
		t.Errorf("reply id = %v; want call-7", sent["id"])
	}
	result, _ := sent["result"].(map[string]any)
	if result["success"] != false {
		t.Errorf("result.success = %v; want false for an unsupported tool", result["success"])
	}
	if out, _ := result["output"].(string); !strings.Contains(out, "unsupported dynamic tool: missing_tool") {
		t.Errorf("result.output = %q; want the unsupported-tool message", out)
	}
}

func TestHandleDynamicToolCall_SuccessfulToolResultDecodesArgsAndReplies(t *testing.T) {
	c, stdin := newDynamicToolCallClient(map[string]DynamicTool{
		"linear_graphql": {Name: "linear_graphql", Call: func(_ context.Context, call ToolCall) (string, error) {
			return `{"success":true,"output":"ok","query":"` + call.Query + `"}`, nil
		}},
	})

	err := c.handleDynamicToolCall(context.Background(), map[string]any{
		"id":     "call-2",
		"params": map[string]any{"tool": "linear_graphql", "arguments": map[string]any{"query": "viewer"}},
	})
	if err != nil {
		t.Fatalf("handleDynamicToolCall() = %v; want nil", err)
	}
	sent := decodeSentMessage(t, stdin)
	result, _ := sent["result"].(map[string]any)
	if result["success"] != true {
		t.Errorf("result.success = %v; want true", result["success"])
	}
	if result["query"] != "viewer" {
		t.Errorf("result.query = %v; want viewer (tool must receive decoded arguments)", result["query"])
	}
	if names := runtimeEventNames(c); slices.Contains(names, task.EventUnsupportedToolCall) {
		t.Errorf("runtime events = %v; want no unsupported_tool_call for a present tool", names)
	}
}

func TestHandleDynamicToolCall_UndecodableArgumentsSkipCallAndReplyFailure(t *testing.T) {
	var called bool
	c, stdin := newDynamicToolCallClient(map[string]DynamicTool{
		"linear_graphql": {Name: "linear_graphql", Call: func(_ context.Context, _ ToolCall) (string, error) {
			called = true
			return "{}", nil
		}},
	})

	// A string arguments value marshals to a JSON string, which cannot decode
	// into ToolCall — exercising the json.Unmarshal failure branch.
	err := c.handleDynamicToolCall(context.Background(), map[string]any{
		"id":     "call-3",
		"params": map[string]any{"tool": "linear_graphql", "arguments": "not-an-object"},
	})
	if err != nil {
		t.Fatalf("handleDynamicToolCall() = %v; want nil", err)
	}
	if called {
		t.Error("tool.Call was invoked despite undecodable arguments; want skipped")
	}
	sent := decodeSentMessage(t, stdin)
	result, _ := sent["result"].(map[string]any)
	if result["success"] != false {
		t.Errorf("result.success = %v; want false on argument decode failure", result["success"])
	}
	out, _ := result["output"].(string)
	if !strings.Contains(out, "unmarshal") {
		t.Errorf("result.output = %q; want it to surface the decode error", out)
	}
	if strings.Contains(out, "unsupported dynamic tool") {
		t.Errorf("result.output = %q; want a decode failure, not the unsupported-tool message (tool was present)", out)
	}
}

func TestHandleDynamicToolCall_ToolErrorRepliesStructuredFailure(t *testing.T) {
	c, stdin := newDynamicToolCallClient(map[string]DynamicTool{
		"linear_graphql": {Name: "linear_graphql", Call: func(_ context.Context, _ ToolCall) (string, error) {
			return "", errors.New("boom from tool")
		}},
	})

	err := c.handleDynamicToolCall(context.Background(), map[string]any{
		"id":     "call-4",
		"params": map[string]any{"tool": "linear_graphql", "arguments": map[string]any{"query": "x"}},
	})
	if err != nil {
		t.Fatalf("handleDynamicToolCall() = %v; want nil", err)
	}
	sent := decodeSentMessage(t, stdin)
	result, _ := sent["result"].(map[string]any)
	if result["success"] != false {
		t.Errorf("result.success = %v; want false on tool execution error", result["success"])
	}
	if out, _ := result["output"].(string); !strings.Contains(out, "boom from tool") {
		t.Errorf("result.output = %q; want it to carry the tool error reason", out)
	}
}

func TestHandleDynamicToolCall_NotificationUsesToolOutputNotify(t *testing.T) {
	c, stdin := newDynamicToolCallClient(map[string]DynamicTool{
		"linear_graphql": {Name: "linear_graphql", Call: func(_ context.Context, _ ToolCall) (string, error) {
			return `{"success":true}`, nil
		}},
	})

	// No "id" key → the message is a notification, so the reply must go out as
	// an item/tool/call/output notification keyed by call_id rather than a
	// JSON-RPC result.
	err := c.handleDynamicToolCall(context.Background(), map[string]any{
		"params": map[string]any{"tool": "linear_graphql", "call_id": "cid-9", "arguments": map[string]any{"query": "x"}},
	})
	if err != nil {
		t.Fatalf("handleDynamicToolCall() = %v; want nil", err)
	}
	sent := decodeSentMessage(t, stdin)
	if sent["method"] != "item/tool/call/output" {
		t.Errorf("notify method = %v; want item/tool/call/output", sent["method"])
	}
	if _, ok := sent["id"]; ok {
		t.Errorf("notification reply carried an id %v; want none", sent["id"])
	}
	params, _ := sent["params"].(map[string]any)
	if params["call_id"] != "cid-9" {
		t.Errorf("notify call_id = %v; want cid-9", params["call_id"])
	}
	if _, ok := params["output"]; !ok {
		t.Errorf("notify params missing output; got %v", params)
	}
}

func TestHandleDynamicToolCall_MutationSinkRecordsToolCallMutationEvent(t *testing.T) {
	c, _ := newDynamicToolCallClient(map[string]DynamicTool{
		"linear_graphql": {Name: "linear_graphql", Call: func(ctx context.Context, _ ToolCall) (string, error) {
			// A real proxy fires the sink installed on the tool context when it
			// dispatches a mutation; emulate that here.
			if sink := toolMutationSinkFrom(ctx); sink != nil {
				sink(ToolMutationAudit{OperationField: "issueUpdate"})
			}
			return `{"success":true}`, nil
		}},
	})

	err := c.handleDynamicToolCall(context.Background(), map[string]any{
		"id":     "call-5",
		"params": map[string]any{"tool": "linear_graphql", "arguments": map[string]any{"query": "mutation"}},
	})
	if err != nil {
		t.Fatalf("handleDynamicToolCall() = %v; want nil", err)
	}
	var payload map[string]any
	for _, ev := range c.runtimeEvents {
		if ev.Event == task.EventToolCallMutation {
			payload, _ = ev.Payload.(map[string]any)
		}
	}
	if payload == nil {
		t.Fatalf("runtime events = %v; want a %q event fired by the mutation sink", runtimeEventNames(c), task.EventToolCallMutation)
	}
	if payload["tool"] != "linear_graphql" {
		t.Errorf("mutation event tool = %v; want linear_graphql", payload["tool"])
	}
	if payload["operation_field"] != "issueUpdate" {
		t.Errorf("mutation event operation_field = %v; want issueUpdate", payload["operation_field"])
	}
}
