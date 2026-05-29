package runner

// codex_app_server_turn.go handles a Codex turn's completion: awaiting the
// terminal notification, classifying turn failures, and deriving quota-backoff
// retry timing from a turn.failed payload. The transport that feeds these
// notifications lives in codex_app_server.go.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
)

// awaitTurnCompletion streams a single Codex turn to its terminal outcome. It
// reads the next protocol message (under the stall budget), classifies read
// failures, and dispatches decoded messages until one ends the turn. The
// read → classify → dispatch split mirrors upstream's receive_loop →
// handle_incoming → handle_turn_method (elixir codex/app_server.ex).
func (c *appServerClient) awaitTurnCompletion(ctx context.Context) error {
	c.lastTerminal = time.Now()
	for {
		msg, raw, stallBudget, err := c.readTurnMessage(ctx)
		if err != nil {
			retErr, retry := c.classifyTurnReadError(ctx, err, raw, stallBudget)
			if retry {
				continue
			}
			return retErr
		}
		if done, derr := c.dispatchTurnMessage(ctx, msg, raw); done {
			return derr
		}
	}
}

// readTurnMessage performs one stall-budget-aware read of the next protocol
// message. With stall detection enabled it derives a per-read deadline from the
// remaining stall budget — returning a *StallError without reading once the
// budget is already spent — and suspends read_timeout_ms for the duration of
// the read: during turn streaming inactivity is governed by stall_timeout_ms,
// while read_timeout_ms stays the per-read transport budget for request/
// response setup and for configurations without stall detection, so it must not
// preempt the longer event-inactivity watchdog and bypass the stalled/
// runner_timeout retry path. The returned stallBudget is 0 when stall detection
// is off.
func (c *appServerClient) readTurnMessage(ctx context.Context) (map[string]any, []byte, time.Duration, error) {
	readCtx := ctx
	var cancel context.CancelFunc
	var stallBudget time.Duration
	if c.stallTimeoutMs > 0 {
		stallBudget = time.Duration(c.stallTimeoutMs) * time.Millisecond
		remaining := stallBudget - time.Since(c.lastTerminal)
		if remaining <= 0 {
			return nil, nil, stallBudget, &StallError{Timeout: stallBudget, Elapsed: time.Since(c.lastTerminal)}
		}
		readCtx, cancel = context.WithTimeout(ctx, remaining)
	}
	readTimeoutMs := c.readTimeoutMs
	if stallBudget > 0 {
		c.readTimeoutMs = 0
	}
	msg, raw, err := c.readProtocolMessage(readCtx)
	c.readTimeoutMs = readTimeoutMs
	if cancel != nil {
		cancel()
	}
	return msg, raw, stallBudget, err
}

// classifyTurnReadError decides what a failed read means. A protocol-like line
// that failed to decode is recorded and skipped (retry=true); a line that is
// not even a JSON object is a hard decode failure. When stall detection is
// active and the read deadline elapsed without the outer context being
// cancelled, the timeout is reclassified as a *StallError so the stalled/
// runner_timeout retry path fires instead of a bare deadline error.
//
// ctx must be the outer loop context, not the per-read deadline context that
// readTurnMessage builds: a fired per-read stall deadline leaves the outer ctx
// uncancelled, and that `ctx.Err() == nil` is exactly what distinguishes a
// stall from a caller-driven cancellation. The pre-read budget-exhausted
// *StallError readTurnMessage returns (raw==nil, Cause==nil) is neither
// DeadlineExceeded nor a read timeout, so it falls through the reclassification
// guard and propagates unchanged.
func (c *appServerClient) classifyTurnReadError(ctx context.Context, err error, raw []byte, stallBudget time.Duration) (error, bool) {
	if raw != nil {
		if !protocolMessageCandidate(raw) {
			return fmt.Errorf("decode codex app-server message: %w", err), false
		}
		c.recordMalformedRuntimeLine(raw, err)
		c.lastTerminal = time.Now()
		return nil, true
	}
	elapsed := time.Since(c.lastTerminal)
	if stallBudget > 0 && ctx.Err() == nil && elapsed >= stallBudget {
		if errors.Is(err, context.DeadlineExceeded) || isAppServerReadTimeout(err) {
			return &StallError{Timeout: stallBudget, Elapsed: elapsed, Cause: err}, false
		}
	}
	return err, false
}

// dispatchTurnMessage routes one decoded protocol message. It returns done=true
// with the turn's terminal error (nil on success) when the turn ends, and
// done=false to keep streaming. Mirrors upstream handle_incoming.
func (c *appServerClient) dispatchTurnMessage(ctx context.Context, msg map[string]any, raw []byte) (bool, error) {
	method, _ := msg["method"].(string)
	if method == "" {
		c.lastTerminal = time.Now()
		c.recordOtherRuntimeMessage(msg, raw)
		return false, nil
	}
	switch method {
	case "turn/completed":
		if err := completedTurnError(msg); err != nil {
			params, _ := msg["params"].(map[string]any)
			c.recordSafeTurnFailure(task.EventTurnEndedWithError, params)
			return true, err
		}
		c.handleNotification(msg)
		c.recordRuntimeMessage(task.EventTurnCompleted, msg)
		return true, nil
	case "turn/failed", "turn/cancelled":
		params, _ := msg["params"].(map[string]any)
		reason := safeTurnReason(params)
		if method == "turn/failed" {
			c.recordSafeTurnFailure(task.EventTurnFailed, params)
			return true, NewError(CategoryTurnFailed, fmt.Sprintf("%s: %s", method, reason), nil)
		}
		c.recordSafeTurnFailure(task.EventTurnCancelled, params)
		return true, NewError(CategoryTurnCancelled, fmt.Sprintf("%s: %s", method, reason), nil)
	case "item/tool/call":
		if err := c.handleDynamicToolCall(ctx, msg); err != nil {
			return true, err
		}
		c.lastTerminal = time.Now()
		return false, nil
	default:
		return c.handleTurnMethod(msg, method)
	}
}

// handleTurnMethod handles a non-terminal method message: a server->client
// request (carries an id) is answered through handleServerRequest, anything
// else is an agent-driven notification. Mirrors upstream handle_turn_method.
// Returns done=true with the terminal error when the message ends the turn,
// done=false to keep streaming.
func (c *appServerClient) handleTurnMethod(msg map[string]any, method string) (bool, error) {
	if _, ok := msg["id"]; ok {
		return c.handleServerRequest(msg, method)
	}
	return c.handleTurnNotification(msg, method)
}

// handleServerRequest answers a server->client request and surfaces a declined
// approval or explicit input request as operator-required input. Mirrors
// upstream maybe_handle_approval_request.
func (c *appServerClient) handleServerRequest(msg map[string]any, method string) (bool, error) {
	if err := c.replyServerRequest(msg); err != nil {
		return true, err
	}
	if inputRequiredServerRequest(method) {
		c.recordInputRequiredMessage(method, msg)
		return true, &InputRequiredError{Method: method}
	}
	// SPEC §10.4 turn_input_required also fires for an approval request that is
	// not auto-approved — the runner's wire response is a decline / empty
	// permissions / deny per protocolServerRequestResult, which is itself a
	// signal that the operator must act. Without the event the orchestrator
	// cannot distinguish this from a transient turn failure.
	if approvalDeclinedServerRequest(method, c.approvalPolicy) {
		c.recordInputRequiredMessage(method, msg)
		return true, &InputRequiredError{Method: method}
	}
	c.lastTerminal = time.Now()
	return false, nil
}

// handleTurnNotification handles an agent-driven notification (no request id):
// an explicit input request ends the turn, a notification arriving after the
// stall budget has elapsed surfaces a *StallError, otherwise the message
// refreshes the stall clock and is recorded. Mirrors upstream
// handle_turn_method's :unhandled branch.
func (c *appServerClient) handleTurnNotification(msg map[string]any, method string) (bool, error) {
	if inputRequiredNotification(method, msg) {
		c.recordInputRequiredMessage(method, msg)
		return true, &InputRequiredError{Method: method}
	}
	if c.stallTimeoutMs > 0 {
		elapsed := time.Since(c.lastTerminal)
		if elapsed > time.Duration(c.stallTimeoutMs)*time.Millisecond {
			return true, &StallError{Timeout: time.Duration(c.stallTimeoutMs) * time.Millisecond, Elapsed: elapsed}
		}
	}
	c.lastTerminal = time.Now()
	c.handleNotification(msg)
	c.recordRuntimeMessage(task.EventNotification, msg)
	return false, nil
}
func (c *appServerClient) handleNotification(msg map[string]any) {
	params, _ := msg["params"].(map[string]any)
	if v, ok := params["continue"].(bool); ok {
		c.continueRun = v
	}
	for _, key := range []string{"lastAssistantMessage", "last_message", "message", "summary"} {
		if v, _ := params[key].(string); strings.TrimSpace(v) != "" {
			c.lastMessage = strings.TrimSpace(v)
			return
		}
	}
}
func completedTurnError(msg map[string]any) error {
	params, _ := msg["params"].(map[string]any)
	status, _ := params["status"].(string)
	if status == "" {
		turn, _ := params["turn"].(map[string]any)
		status, _ = turn["status"].(string)
	}
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" || status == "completed" || status == "succeeded" || status == "success" || status == "interrupted" {
		return nil
	}
	if quota := quotaBackoffFromTurnParams(params); quota != nil {
		return quota
	}
	return NewError(CategoryTurnFailed, fmt.Sprintf("turn/completed failed with status %q: %s", status, safeTurnReason(params)), nil)
}

// safeTurnReason extracts a human-readable reason string from a Codex
// `turn/failed`, `turn/cancelled`, or failed `turn/completed` params payload,
// pulling only from an explicit allow-list of fields. Returns
// `"reason unavailable"` when none are populated. This is a defense-in-depth
// guard so that arbitrary protocol fields (which may carry tool output,
// elicitation snippets, or secrets) never end up in returned error strings or
// the structured log/event surface. See docs/security-posture.md.
func safeTurnReason(params map[string]any) string {
	const fallback = "reason unavailable"
	if params == nil {
		return fallback
	}
	sources := []map[string]any{params}
	if turn, _ := params["turn"].(map[string]any); turn != nil {
		sources = append(sources, turn)
	}
	for _, source := range sources {
		if v := extractReasonFields(source); v != "" {
			return v
		}
	}
	return fallback
}

// extractReasonFields walks the allow-listed reason keys in a single map and
// returns the first non-empty string. `error` is special: Codex may serialize
// it as a string or as a nested object like
// `{"message": "...", "code": "...", "error_code": "..."}`. Both shapes are
// supported; nested object lookup only allows the same allow-listed scalar
// fields.
func extractReasonFields(source map[string]any) string {
	for _, key := range []string{"reason", "error", "message", "error_code"} {
		raw, ok := source[key]
		if !ok {
			continue
		}
		switch v := raw.(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case map[string]any:
			for _, nestedKey := range []string{"message", "reason", "error_code", "code"} {
				if s, _ := v[nestedKey].(string); strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
	}
	return ""
}

// safeTurnFailurePayload returns a runtime-event payload built from only the
// allow-listed status/reason fields of a Codex failure params map. Mirrors the
// redaction discipline of safeTurnReason so the JSON event surface
// (`/api/v1/state`, runtime event log) never persists the raw protocol payload.
func safeTurnFailurePayload(params map[string]any) map[string]any {
	out := map[string]any{}
	if params == nil {
		out["reason"] = "reason unavailable"
		return out
	}
	if v, _ := params["status"].(string); strings.TrimSpace(v) != "" {
		out["status"] = strings.TrimSpace(v)
	}
	copyAllowlistedReasonFields(params, out)
	if turn, _ := params["turn"].(map[string]any); turn != nil {
		nested := map[string]any{}
		if v, _ := turn["status"].(string); strings.TrimSpace(v) != "" {
			nested["status"] = strings.TrimSpace(v)
		}
		copyAllowlistedReasonFields(turn, nested)
		if len(nested) > 0 {
			out["turn"] = nested
		}
	}
	if info, ok := codexErrorInfoValue(params); ok {
		out["codex_error_info"] = info
	}
	if retryAfter := quotaRetryAfterFromParams(params, time.Now()); retryAfter > 0 {
		out["retry_after_seconds"] = int64(retryAfter.Round(time.Second) / time.Second)
	}
	if _, ok := out["reason"]; !ok {
		out["reason"] = safeTurnReason(params)
	}
	return out
}

// copyAllowlistedReasonFields copies allow-listed reason fields from src to
// dst. String values are trimmed; structured `error` objects are flattened to a
// fresh allow-listed sub-map so non-allow-listed siblings are never persisted.
func copyAllowlistedReasonFields(src, dst map[string]any) {
	for _, key := range []string{"reason", "error", "message", "error_code"} {
		raw, ok := src[key]
		if !ok {
			continue
		}
		switch v := raw.(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				dst[key] = s
			}
		case map[string]any:
			scrubbed := map[string]any{}
			for _, nestedKey := range []string{"message", "reason", "error_code", "code"} {
				if s, _ := v[nestedKey].(string); strings.TrimSpace(s) != "" {
					scrubbed[nestedKey] = strings.TrimSpace(s)
				}
			}
			if len(scrubbed) > 0 {
				dst[key] = scrubbed
			}
		}
	}
}
func quotaBackoffFromTurnParams(params map[string]any) *QuotaBackoffError {
	info, ok := codexErrorInfoValue(params)
	if !ok || !isUsageLimitExceeded(info) {
		return nil
	}
	return &QuotaBackoffError{
		Message:    safeTurnReason(params),
		RetryAfter: quotaRetryAfterFromParams(params, time.Now()),
	}
}
func codexErrorInfoValue(params map[string]any) (string, bool) {
	turn, _ := params["turn"].(map[string]any)
	if turn == nil {
		return "", false
	}
	errorPayload, _ := turn["error"].(map[string]any)
	if errorPayload == nil {
		return "", false
	}
	info, _ := errorPayload["codexErrorInfo"].(string)
	info = strings.TrimSpace(info)
	return info, info != ""
}
func isUsageLimitExceeded(info string) bool {
	return info == "usageLimitExceeded"
}
func quotaRetryAfterFromParams(params map[string]any, now time.Time) time.Duration {
	turn, _ := params["turn"].(map[string]any)
	errorPayload, _ := turn["error"].(map[string]any)
	for _, key := range []string{"message", "additionalDetails"} {
		if s, _ := errorPayload[key].(string); strings.TrimSpace(s) != "" {
			if d := parseRetryAfterText(s, now); d > 0 {
				return d
			}
		}
	}
	return 0
}

var (
	retryInPattern = regexp.MustCompile(`(?i)(?:try again|retry)[^.]*?\bin\s+(\d+(?:\.\d+)?)\s*(second|seconds|sec|secs|minute|minutes|min|mins|hour|hours|hr|hrs)\b`)
	retryAtPattern = regexp.MustCompile(`(?i)(?:try again|retry)[^.]*?\bat\s+(\d{1,2})(?::(\d{2}))?\s*([ap]\.?m\.?)`)
)

func parseRetryAfterText(message string, now time.Time) time.Duration {
	if match := retryInPattern.FindStringSubmatch(message); len(match) == 3 {
		return parseRelativeRetryDelay(match)
	}
	if match := retryAtPattern.FindStringSubmatch(message); len(match) == 4 {
		return parseAbsoluteRetryDelay(match, now)
	}
	return 0
}

// parseRelativeRetryDelay converts a retryInPattern submatch ("in N <unit>")
// into a delay. N is regex-constrained to a non-negative decimal, and the unit
// group is exhaustive over the switch cases, so the only non-positive result is
// an explicit N<=0 (treated as "no usable hint").
func parseRelativeRetryDelay(match []string) time.Duration {
	n, err := strconv.ParseFloat(match[1], 64)
	if err != nil || n <= 0 {
		return 0
	}
	switch strings.ToLower(match[2]) {
	case "second", "seconds", "sec", "secs":
		return time.Duration(n * float64(time.Second))
	case "minute", "minutes", "min", "mins":
		return time.Duration(n * float64(time.Minute))
	case "hour", "hours", "hr", "hrs":
		return time.Duration(n * float64(time.Hour))
	}
	return 0
}

// parseAbsoluteRetryDelay converts a retryAtPattern submatch ("at H[:MM] am/pm")
// into the delay until that clock time relative to now, rolling to the next day
// when the time has already passed today. An out-of-range hour (>12) or minute
// (>59) yields 0.
func parseAbsoluteRetryDelay(match []string, now time.Time) time.Duration {
	hour, err := strconv.Atoi(match[1])
	if err != nil || hour < 1 || hour > 12 {
		return 0
	}
	minute := 0
	if match[2] != "" {
		minute, err = strconv.Atoi(match[2])
		if err != nil || minute > 59 {
			return 0
		}
	}
	meridiem := strings.ToLower(strings.ReplaceAll(match[3], ".", ""))
	hour = to24Hour(hour, meridiem)
	retryAt := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !retryAt.After(now) {
		retryAt = retryAt.Add(24 * time.Hour)
	}
	return retryAt.Sub(now)
}

// to24Hour converts a 1-12 clock hour plus a normalized meridiem ("am"/"pm")
// to a 0-23 hour: 12 PM stays noon, 12 AM becomes midnight (0), every other PM
// hour is shifted by 12.
func to24Hour(hour int, meridiem string) int {
	if meridiem == "pm" && hour != 12 {
		return hour + 12
	}
	if meridiem == "am" && hour == 12 {
		return 0
	}
	return hour
}
func (c *appServerClient) recordSafeTurnFailure(event string, params map[string]any) {
	c.recordRuntimeEvent(event, c.withRuntimeContext(safeTurnFailurePayload(params)))
}
