package runner

// codex_app_server_approval.go answers app-server server->client requests: the
// approval-policy gate that auto-approves or declines exec/patch/tool requests,
// and the dynamic (linear_graphql) tool-call bridge. The message loop that
// dispatches these lives in codex_app_server.go.

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

func (c *appServerClient) replyServerRequest(msg map[string]any) error {
	method, _ := msg["method"].(string)
	if result, ok := protocolServerRequestResult(method, msg, c.approvalPolicy); ok {
		if err := c.send(map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": result}); err != nil {
			return err
		}
		if protocolServerRequestAutoApproved(method, c.approvalPolicy) {
			c.recordApprovalAutoApproved(method, msg, result)
		}
		return nil
	}
	return c.sendJSONRPCError(msg["id"], -32601, "Method not found: "+method)
}
func (c *appServerClient) recordApprovalAutoApproved(method string, msg map[string]any, result map[string]any) {
	params, _ := msg["params"].(map[string]any)
	payload := normalizeRuntimePayload(params)
	if payload == nil {
		payload = map[string]any{}
	}
	payload["method"] = method
	payload["result"] = normalizeRuntimeValue(result)
	c.recordRuntimeEvent(task.EventApprovalAutoApproved, c.withRuntimeContext(payload))
}
func (c *appServerClient) sendJSONRPCError(id any, code int, message string) error {
	return c.send(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}
func inputRequiredServerRequest(method string) bool {
	switch method {
	case "item/tool/requestUserInput", "mcpServer/elicitation/request":
		return true
	default:
		return false
	}
}

// approvalDeclinedServerRequest reports whether a server request method goes
// through the decline / deny / empty-permissions branch in
// protocolServerRequestResult under the current approval policy. SPEC §10.4
// treats a declined approval as operator-required input.
func approvalDeclinedServerRequest(method string, approvalPolicy any) bool {
	switch method {
	case "item/commandExecution/requestApproval",
		"item/fileChange/requestApproval",
		"item/permissions/requestApproval",
		"execCommandApproval",
		"applyPatchApproval":
		return !autoApproveRequest(method, approvalPolicy)
	default:
		return false
	}
}
func inputRequiredNotification(method string, msg map[string]any) bool {
	if method == "mcpServer/elicitation/request" {
		return true
	}
	if !strings.HasPrefix(method, "turn/") {
		return false
	}
	switch method {
	case "turn/input_required", "turn/needs_input", "turn/need_input", "turn/request_input", "turn/request_response", "turn/provide_input", "turn/approval_required":
		return true
	}
	params, _ := msg["params"].(map[string]any)
	return inputRequiredField(msg) || inputRequiredField(params)
}
func inputRequiredField(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	return payload["requiresInput"] == true ||
		payload["needsInput"] == true ||
		payload["input_required"] == true ||
		payload["inputRequired"] == true ||
		payload["type"] == "input_required" ||
		payload["type"] == "needs_input"
}
func protocolServerRequestResult(method string, msg map[string]any, approvalPolicy any) (map[string]any, bool) {
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		if autoApproveRequest(method, approvalPolicy) {
			return map[string]any{"decision": "acceptForSession"}, true
		}
		return map[string]any{"decision": "decline"}, true
	case "item/permissions/requestApproval":
		if autoApproveRequest(method, approvalPolicy) {
			params, _ := msg["params"].(map[string]any)
			return map[string]any{"permissions": params["permissions"]}, true
		}
		return map[string]any{"permissions": map[string]any{}}, true
	case "execCommandApproval", "applyPatchApproval":
		if autoApproveRequest(method, approvalPolicy) {
			return map[string]any{"decision": "allow"}, true
		}
		return map[string]any{"decision": "deny"}, true
	case "item/tool/requestUserInput":
		return protocolUserInputResult(msg), true
	case "mcpServer/elicitation/request":
		return map[string]any{"action": "decline", "content": nil}, true
	default:
		return nil, false
	}
}
func protocolServerRequestAutoApproved(method string, approvalPolicy any) bool {
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval", "execCommandApproval", "applyPatchApproval":
		return autoApproveRequest(method, approvalPolicy)
	default:
		return false
	}
}

// autoApproveRequest reports whether protocolServerRequestResult should
// auto-approve a server-side approval prompt that codex sent the harness.
// The decision tracks codex's own `AskForApproval` semantics (codex-rs
// protocol/src/protocol.rs):
//
//   - "never"                     — codex never asks; if a prompt still
//     surfaces, auto-approve it (matches codex's "failures returned to the
//     model" intent).
//   - "on-failure"                — codex asks only on failure; the harness
//     auto-approves to keep the unattended loop moving.
//   - "untrusted" / "on-request"  — operator-supervised modes; decline.
//   - {"granular": {...}}         — per-method bool flags where TRUE means
//     ALLOW (codex semantics since #14516, b7dba72db, 2026-03-12).
func autoApproveRequest(method string, approvalPolicy any) bool {
	if policy, ok := approvalPolicy.(string); ok {
		switch strings.ToLower(strings.TrimSpace(policy)) {
		case "never", "on-failure":
			return true
		default:
			return false
		}
	}
	policy, ok := approvalPolicy.(map[string]any)
	if !ok {
		return false
	}
	if granular, ok := policy["granular"].(map[string]any); ok {
		return approvalRuleAllowsRequest(granular, method)
	}
	return false
}
func approvalRuleAllowsRequest(rules map[string]any, method string) bool {
	if method == "item/permissions/requestApproval" {
		return approvalRuleEnabled(rules, "request_permissions")
	}
	return approvalRuleEnabled(rules, "sandbox_approval") || approvalRuleEnabled(rules, "rules")
}
func approvalRuleEnabled(rules map[string]any, key string) bool {
	enabled, _ := rules[key].(bool)
	return enabled
}

// codexWireApprovalPolicy maps aiops-platform's ApprovalPolicy value to the
// codex app-server JSON-RPC wire format. Codex's `AskForApproval` enum
// (codex-rs/protocol/src/protocol.rs and
// codex-rs/app-server-protocol/src/protocol/v2/shared.rs) accepts exactly
// {"untrusted", "on-failure", "on-request", "granular": {...}, "never"}.
// Sending anything else makes thread/start return JSON-RPC -32600
// `Invalid request: unknown variant ...` and breaks startup (#329).
//
// The obsolete `{"reject": {...}}` shape — which used to be valid in codex
// up to PR #14516 (commit b7dba72db, renamed to `granular` and field
// polarity inverted on 2026-03-12) — is translated here for back-compat
// with WORKFLOW.md files written against the old protocol. The polarity
// flip (`reject.sandbox_approval=true` meant "reject"; the new
// `granular.sandbox_approval=true` means "allow") is preserved so the
// translated payload retains the operator's original intent.
//
// All codex-recognized values (`"untrusted"`/`"on-failure"`/`"on-request"`/
// `"never"` strings, and the `{"granular": {...}}` map) pass through
// unchanged. Unrecognized shapes also pass through so codex's own
// validation error reaches the operator verbatim rather than getting
// masked behind a translator silently rewriting their payload.
func codexWireApprovalPolicy(internal any) any {
	policy, ok := internal.(map[string]any)
	if !ok {
		return internal
	}
	rejectShape, hasReject := policy["reject"].(map[string]any)
	if !hasReject {
		return internal
	}
	// Translate obsolete reject:{flag:true} → granular:{flag:false} per the
	// codex #14516 polarity flip. Pass through any extra keys verbatim so a
	// future codex addition doesn't get lost.
	granular := make(map[string]any, len(rejectShape))
	for k, v := range rejectShape {
		if b, ok := v.(bool); ok {
			granular[k] = !b
			continue
		}
		granular[k] = v
	}
	translated := make(map[string]any, len(policy))
	for k, v := range policy {
		if k == "reject" {
			continue
		}
		translated[k] = v
	}
	translated["granular"] = granular
	return translated
}
func protocolUserInputResult(msg map[string]any) map[string]any {
	params, _ := msg["params"].(map[string]any)
	questions, _ := params["questions"].([]any)
	answers := make(map[string]any)
	for _, question := range questions {
		q, _ := question.(map[string]any)
		id, _ := q["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		answers[id] = map[string]any{"answers": []string{nonInteractiveInputReply}}
	}
	return map[string]any{"answers": answers}
}
func (c *appServerClient) handleDynamicToolCall(ctx context.Context, msg map[string]any) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	params, _ := msg["params"].(map[string]any)
	name := appServerToolCallName(params)
	arguments := appServerToolCallArguments(params)
	result, err := c.resolveDynamicToolResult(ctx, name, arguments)
	if err != nil {
		return err
	}
	return c.replyDynamicToolOutput(msg, params, result)
}

// resolveDynamicToolResult runs the named dynamic tool and returns its wire
// result string. A name not in the advertised set records the SPEC §10.4
// unsupported_tool_call event — so the orchestrator/state surface can tell
// "agent invoked an unadvertised tool" apart from "advertised tool failed" —
// and returns the structured "unsupported dynamic tool" failure; a present
// tool's argument-decode and execution failures are wrapped as structured
// dynamicToolFailure results. The returned error is non-nil only when a failure
// result itself cannot be marshaled, mirroring the original handler's fatal
// early-return.
func (c *appServerClient) resolveDynamicToolResult(ctx context.Context, name string, arguments json.RawMessage) (string, error) {
	tool, ok := c.tools.Lookup(name)
	if !ok {
		c.recordUnsupportedToolCall(name, arguments)
		return dynamicToolResult(false, "unsupported dynamic tool: "+name)
	}
	call := ToolCall{}
	if err := json.Unmarshal(arguments, &call); err != nil {
		return dynamicToolFailure(err.Error())
	}
	result, err := tool.Call(c.withMutationAuditSink(ctx, name), call)
	if err != nil {
		return dynamicToolFailure(err.Error())
	}
	return result, nil
}

// withMutationAuditSink derives the tool-execution context carrying the audit
// sink that fires for any tool routing through the Linear GraphQL proxy.
// linear_ai_workpad composes deterministic commentCreate/commentUpdate
// mutations through the same token-isolated callRaw transport, so operators
// need the tool_call_mutation runtime event for those harness-attributable
// writes too. Only the proxy itself fires the sink, so installing it on
// unrelated tools is a no-op.
func (c *appServerClient) withMutationAuditSink(ctx context.Context, name string) context.Context {
	ctx = WithLinearGraphQLMutationSink(ctx, func(audit LinearGraphQLMutationAudit) {
		payload := map[string]any{"tool": name}
		if audit.OperationField != "" {
			payload["operation_field"] = audit.OperationField
		}
		if audit.CurrentIssueNonActiveStateUpdate {
			payload["current_issue_non_active_state_update"] = true
		}
		c.recordRuntimeEvent(task.EventToolCallMutation, c.withRuntimeContext(payload))
	})
	ctx = WithLinearGraphQLMutationRejectedSink(ctx, func(rejection linearGraphQLMutationRejected) {
		payload := map[string]any{
			"tool":     name,
			"reason":   rejection.Reason,
			"found":    rejection.Found,
			"terminal": rejection.Terminal,
		}
		if rejection.OperationField != "" {
			payload["operation_field"] = rejection.OperationField
		}
		if rejection.State != "" {
			payload["state"] = rejection.State
		}
		c.recordRuntimeEvent(task.EventToolCallMutationRejected, c.withRuntimeContext(payload))
	})
	return WithLinearGraphQLPostStopMutationSink(ctx, func(operationField string) {
		payload := map[string]any{"tool": name}
		if operationField != "" {
			payload["operation_field"] = operationField
		}
		c.recordRuntimeEvent(task.EventToolCallMutationPostOperatorTerminalStop, c.withRuntimeContext(payload))
	})
}

// replyDynamicToolOutput returns result to codex: parsed as JSON (falling back
// to a {success:false, output} envelope when result is not valid JSON), sent as
// the JSON-RPC result when the inbound message carries an id, or emitted as an
// item/tool/call/output notification keyed by call_id otherwise.
func (c *appServerClient) replyDynamicToolOutput(msg, params map[string]any, result string) error {
	var payload any
	if err := json.Unmarshal([]byte(result), &payload); err != nil {
		payload = map[string]any{"success": false, "output": result}
	}
	if id, ok := msg["id"]; ok {
		return c.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": payload})
	}
	return c.notify("item/tool/call/output", map[string]any{"call_id": params["call_id"], "output": payload})
}
func appServerToolCallName(params map[string]any) string {
	for _, key := range []string{"tool", "name"} {
		if v, _ := params[key].(string); strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
func appServerToolCallArguments(params map[string]any) json.RawMessage {
	if raw, ok := params["arguments"]; ok && raw != nil {
		b, err := json.Marshal(raw)
		if err == nil {
			return b
		}
	}
	return json.RawMessage(`{}`)
}
