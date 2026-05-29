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

func (c *appServerClient) awaitTurnCompletion(ctx context.Context) error {
	c.lastTerminal = time.Now()
	for {
		readCtx := ctx
		var cancel context.CancelFunc
		var stallBudget time.Duration
		if c.stallTimeoutMs > 0 {
			stallBudget = time.Duration(c.stallTimeoutMs) * time.Millisecond
			remaining := stallBudget - time.Since(c.lastTerminal)
			if remaining <= 0 {
				return &StallError{Timeout: stallBudget, Elapsed: time.Since(c.lastTerminal)}
			}
			readCtx, cancel = context.WithTimeout(ctx, remaining)
		}
		readTimeoutMs := c.readTimeoutMs
		if stallBudget > 0 {
			// During turn streaming, inactivity is governed by stall_timeout_ms.
			// read_timeout_ms remains the per-read transport budget for request/
			// response setup and for configurations without stall detection, but it
			// must not preempt the longer event-inactivity watchdog and bypass the
			// stalled/runner_timeout retry path.
			c.readTimeoutMs = 0
		}
		msg, raw, err := c.readProtocolMessage(readCtx)
		c.readTimeoutMs = readTimeoutMs
		if cancel != nil {
			cancel()
		}
		if err != nil {
			if raw != nil {
				if !protocolMessageCandidate(raw) {
					return fmt.Errorf("decode codex app-server message: %w", err)
				}
				c.recordMalformedRuntimeLine(raw, err)
				c.lastTerminal = time.Now()
				continue
			}
			elapsed := time.Since(c.lastTerminal)
			if stallBudget > 0 && ctx.Err() == nil && elapsed >= stallBudget {
				if errors.Is(err, context.DeadlineExceeded) || isAppServerReadTimeout(err) {
					return &StallError{Timeout: stallBudget, Elapsed: elapsed, Cause: err}
				}
			}
			return err
		}
		if method, _ := msg["method"].(string); method != "" {
			switch method {
			case "turn/completed":
				if err := completedTurnError(msg); err != nil {
					params, _ := msg["params"].(map[string]any)
					c.recordSafeTurnFailure(task.EventTurnEndedWithError, params)
					return err
				}
				c.handleNotification(msg)
				c.recordRuntimeMessage(task.EventTurnCompleted, msg)
				return nil
			case "turn/failed", "turn/cancelled":
				params, _ := msg["params"].(map[string]any)
				reason := safeTurnReason(params)
				if method == "turn/failed" {
					c.recordSafeTurnFailure(task.EventTurnFailed, params)
					return NewError(CategoryTurnFailed, fmt.Sprintf("%s: %s", method, reason), nil)
				}
				c.recordSafeTurnFailure(task.EventTurnCancelled, params)
				return NewError(CategoryTurnCancelled, fmt.Sprintf("%s: %s", method, reason), nil)
			case "item/tool/call":
				if err := c.handleDynamicToolCall(ctx, msg); err != nil {
					return err
				}
				c.lastTerminal = time.Now()
			default:
				if _, ok := msg["id"]; ok {
					if err := c.replyServerRequest(msg); err != nil {
						return err
					}
					if inputRequiredServerRequest(method) {
						c.recordInputRequiredMessage(method, msg)
						return &InputRequiredError{Method: method}
					}
					// SPEC §10.4 turn_input_required also fires for an
					// approval request that is not auto-approved — the
					// runner's wire response is a decline / empty
					// permissions / deny per protocolServerRequestResult,
					// which is itself a signal that the operator must
					// act. Without the event the orchestrator cannot
					// distinguish this from a transient turn failure.
					if approvalDeclinedServerRequest(method, c.approvalPolicy) {
						c.recordInputRequiredMessage(method, msg)
						return &InputRequiredError{Method: method}
					}
					c.lastTerminal = time.Now()
					continue
				}
				if inputRequiredNotification(method, msg) {
					c.recordInputRequiredMessage(method, msg)
					return &InputRequiredError{Method: method}
				}
				if c.stallTimeoutMs > 0 {
					elapsed := time.Since(c.lastTerminal)
					if elapsed > time.Duration(c.stallTimeoutMs)*time.Millisecond {
						return &StallError{Timeout: time.Duration(c.stallTimeoutMs) * time.Millisecond, Elapsed: elapsed}
					}
				}
				c.lastTerminal = time.Now()
				c.handleNotification(msg)
				c.recordRuntimeMessage(task.EventNotification, msg)
			}
		} else {
			c.lastTerminal = time.Now()
			c.recordOtherRuntimeMessage(msg, raw)
		}
	}
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
	}
	if match := retryAtPattern.FindStringSubmatch(message); len(match) == 4 {
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
		if meridiem == "pm" && hour != 12 {
			hour += 12
		}
		if meridiem == "am" && hour == 12 {
			hour = 0
		}
		retryAt := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
		if !retryAt.After(now) {
			retryAt = retryAt.Add(24 * time.Hour)
		}
		return retryAt.Sub(now)
	}
	return 0
}
func (c *appServerClient) recordSafeTurnFailure(event string, params map[string]any) {
	c.recordRuntimeEvent(event, c.withRuntimeContext(safeTurnFailurePayload(params)))
}
