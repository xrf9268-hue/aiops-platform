package runner

// codex_app_server_events.go records SPEC runtime events from the app-server
// message stream onto the run's attempt log, including payload normalization
// to snake_case. The turn message loop that calls these recorders lives in
// codex_app_server_turn.go; the session driver in codex_app_server.go records
// the startup-failure and phase-transition events directly.

import (
	"encoding/json"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

func (c *appServerClient) recordRuntimeMessage(event string, msg map[string]any) {
	params, _ := msg["params"].(map[string]any)
	payload := normalizeRuntimePayload(params)
	if payload == nil {
		payload = map[string]any{}
	}
	c.recordRuntimeEvent(event, c.withRuntimeContext(payload))
}

// codexModelReroutedMethod is the SPEC §10.4 notification (ModelReroutedNotification
// in the app-server schema) codex emits when it falls back mid-claim from one
// model to another — e.g. the requested model is unavailable. Its toModel is the
// model now actually running the turn.
const codexModelReroutedMethod = "model/rerouted"

// recordModelRerouted folds a model/rerouted notification's toModel into the
// canonical agent_model the orchestrator reads (recordSessionFields). The
// thread/start model is captured once on session_started; a later reroute must
// update it, or /api/v1/state, the web dashboard, and the TUI keep reporting the
// originally requested model for the rest of the claim — wrong in exactly the
// fallback/reroute case #977 must surface. The raw notification is still recorded
// as EventNotification (so the reason/from_model stay observable); agent_model is
// added so the existing session-field fold tracks the new model without a second
// event kind.
func (c *appServerClient) recordModelRerouted(msg map[string]any) {
	params, _ := msg["params"].(map[string]any)
	if to, _ := params["toModel"].(string); strings.TrimSpace(to) != "" {
		c.agentModel = strings.TrimSpace(to)
	}
	payload := normalizeRuntimePayload(params)
	if payload == nil {
		payload = map[string]any{}
	}
	if c.agentModel != "" {
		payload["agent_model"] = c.agentModel
	}
	c.recordRuntimeEvent(task.EventNotification, c.withRuntimeContext(payload))
}
func (c *appServerClient) recordOtherRuntimeMessage(msg map[string]any, raw []byte) {
	payload := normalizeRuntimePayload(msg)
	if payload == nil {
		payload = map[string]any{}
	}
	payload["raw"] = trimProtocolLine(raw)
	c.recordRuntimeEvent(task.EventOtherMessage, c.withRuntimeContext(payload))
}
func (c *appServerClient) recordMalformedRuntimeLine(raw []byte, err error) {
	if !protocolMessageCandidate(raw) {
		return
	}
	payload := map[string]any{
		"raw":   trimProtocolLine(raw),
		"error": err.Error(),
	}
	c.recordRuntimeEvent(task.EventMalformed, c.withRuntimeContext(payload))
}
func (c *appServerClient) withRuntimeContext(payload map[string]any) map[string]any {
	if _, ok := payload["thread_id"]; !ok && c.threadID != "" {
		payload["thread_id"] = c.threadID
	}
	if _, ok := payload["turn_id"]; !ok && c.turnID != "" {
		payload["turn_id"] = c.turnID
	}
	return payload
}
func (c *appServerClient) recordInputRequiredMessage(method string, msg map[string]any) {
	payload := map[string]any{"method": method}
	if params, _ := msg["params"].(map[string]any); params != nil {
		payload["params"] = normalizeRuntimePayload(params)
	}
	if c.threadID != "" {
		payload["thread_id"] = c.threadID
	}
	if c.turnID != "" {
		payload["turn_id"] = c.turnID
		if c.threadID != "" {
			payload["session_id"] = c.threadID + "-" + c.turnID
		}
	}
	c.recordRuntimeEvent(task.EventTurnInputRequired, payload)
}
func (c *appServerClient) recordRuntimeEvent(event string, payload map[string]any) {
	runtimeEvent := task.RuntimeEvent{Event: event, Payload: payload}
	c.runtimeEvents = append(c.runtimeEvents, runtimeEvent)
	c.lastRuntimeEvent = event
	if c.runtimeEventSink != nil {
		c.runtimeEventSink(runtimeEvent)
	}
}

// recordUnsupportedToolCall emits task.EventUnsupportedToolCall (SPEC §10.4)
// with the tool name and the (already JSON-marshaled) arguments slice the
// agent supplied. Arguments come from codex over JSON-RPC, so we surface them
// verbatim — they were never going to be a secret-leak surface since the
// agent chose them.
func (c *appServerClient) recordUnsupportedToolCall(name string, arguments json.RawMessage) {
	payload := map[string]any{"tool": name}
	if len(arguments) > 0 {
		var parsed any
		if err := json.Unmarshal(arguments, &parsed); err == nil {
			payload["arguments"] = parsed
		} else {
			payload["arguments_raw"] = string(arguments)
		}
	}
	c.recordRuntimeEvent(task.EventUnsupportedToolCall, c.withRuntimeContext(payload))
}

// recordStartupFailed emits task.EventStartupFailed (SPEC §10.4) tagged with
// the startup phase (initialize / initialized / thread/start / turn/start)
// that just failed. The payload `error` carries the Go error's Error()
// string; the upstream errors come from JSON-RPC framing or extractString,
// neither of which echoes user-controlled params, so it is safe to surface
// without the safeTurnReason redaction pass.
func (c *appServerClient) recordStartupFailed(phase string, err error) {
	payload := map[string]any{"phase": phase}
	if err != nil {
		payload["error"] = err.Error()
	}
	c.recordRuntimeEvent(task.EventStartupFailed, c.withRuntimeContext(payload))
}
func (c *appServerClient) recordPhaseTransition(from, to task.RunAttemptPhase) {
	if c.phaseTransitionSink != nil {
		c.phaseTransitionSink(from, to)
	}
}
func normalizeRuntimePayload(params map[string]any) map[string]any {
	if params == nil {
		return nil
	}
	out := make(map[string]any, len(params))
	for k, v := range params {
		out[toSnakeCase(k)] = normalizeRuntimeValue(v)
	}
	return out
}
func normalizeRuntimeValue(v any) any {
	switch typed := v.(type) {
	case map[string]any:
		return normalizeRuntimePayload(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = normalizeRuntimeValue(item)
		}
		return out
	default:
		return v
	}
}
func toSnakeCase(s string) string {
	var b strings.Builder
	var prev rune
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 && snakeCaseWordBoundary(prev, asciiByteIsLower(s, i+1)) {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
		prev = rune(s[i])
	}
	return b.String()
}

// asciiByteIsLower reports whether the byte at index i in s is an ASCII
// lowercase letter. Codex payload keys are ASCII camelCase, so toSnakeCase scans
// bytes rather than runes; this is the one-byte lookahead its acronym-boundary
// rule needs.
func asciiByteIsLower(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	return s[i] >= 'a' && s[i] <= 'z'
}

// snakeCaseWordBoundary reports whether an uppercase rune starts a new
// snake_case word given the previous rune and whether the next byte is
// lowercase. Two boundaries insert an underscore: after a lowercase/digit (the
// camelCase boundary, fooB -> foo_b) and at the tail of an acronym run that is
// followed by a word (an uppercase prev with a lowercase next, HTTPServer ->
// http_server).
func snakeCaseWordBoundary(prev rune, nextIsLower bool) bool {
	prevIsLowerOrDigit := (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9')
	prevIsUpper := prev >= 'A' && prev <= 'Z'
	return prevIsLowerOrDigit || (prevIsUpper && nextIsLower)
}
