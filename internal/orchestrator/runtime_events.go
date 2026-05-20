package orchestrator

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/worker"
)

var runtimeEventKinds = func() map[string]struct{} {
	kinds := map[string]struct{}{}
	for _, kind := range task.RuntimeEvents() {
		kinds[kind] = struct{}{}
	}
	return kinds
}()

// RecordRuntimeEvent folds a SPEC §10.4 app-server event into the
// orchestrator-owned runtime state. Task-event persistence remains the
// worker's responsibility.
func (o *Orchestrator) RecordRuntimeEvent(ctx context.Context, issueID string, event task.RuntimeEvent) error {
	if o == nil || issueID == "" || event.Event == "" {
		return nil
	}
	done := make(chan struct{})
	if err := o.submit(ctx, &recordRuntimeEventOp{
		issueID: IssueID(issueID),
		event:   event,
		now:     time.Now().UTC(),
		done:    done,
	}); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type recordRuntimeEventOp struct {
	issueID IssueID
	event   task.RuntimeEvent
	now     time.Time
	done    chan<- struct{}
}

func (r *recordRuntimeEventOp) apply(st *OrchestratorState) func() {
	if run := st.Running[r.issueID]; run != nil {
		st.recordRuntimeEvent(run, r.event, r.now)
	}
	return func() { close(r.done) }
}

func (s *OrchestratorState) recordRuntimeEvent(run *RunningEntry, event task.RuntimeEvent, now time.Time) {
	if s == nil || run == nil || event.Event == "" {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	run.LastCodexAt = now
	s.recordSessionFields(run, event)
	if usage, ok := tokenUsageFromEvent(event); ok {
		input, output, total := applyTokenUsage(run, usage)
		s.CodexTotals.AddTokenDelta(input, output, total)
	}
	if limits, ok := rateLimitsFromPayload(event.Payload); ok {
		snap := RateLimitSnapshot(limits)
		s.RecordRateLimits(&snap)
	}
}

func (s *OrchestratorState) recordSessionFields(run *RunningEntry, event task.RuntimeEvent) {
	payload, _ := asStringMap(event.Payload)
	if sessionID, ok := stringField(payload, "session_id"); ok {
		run.Session.SessionID = sessionID
	}
	if threadID, ok := stringField(payload, "thread_id"); ok {
		run.Session.ThreadID = threadID
	}
	if turnID, ok := stringField(payload, "turn_id"); ok {
		run.Session.TurnID = turnID
	}
	if event.Event == task.EventTurnCompleted {
		run.Session.TurnCount++
	}
}

type tokenUsage struct {
	input, output, total int64
	hasInput, hasOutput  bool
	hasTotal             bool
	absolute             bool
}

func tokenUsageFromEvent(event task.RuntimeEvent) (tokenUsage, bool) {
	payload, ok := asStringMap(event.Payload)
	if !ok {
		return tokenUsage{}, false
	}
	absolutePaths := [][]string{
		{"total_token_usage"},
		{"token_usage", "total"},
		{"turn", "token_usage", "total"},
		{"msg", "payload", "info", "total_token_usage"},
		{"msg", "info", "total_token_usage"},
	}
	for _, path := range absolutePaths {
		if usage, ok := tokenUsageFromMap(mapAtPath(payload, path)); ok {
			usage.absolute = true
			return usage, true
		}
	}
	paths := [][]string{{"usage"}, {"turn", "usage"}}
	if event.Event == task.EventTurnCompleted {
		paths = append(paths, nil)
	}
	for _, path := range paths {
		if usage, ok := tokenUsageFromMap(mapAtPath(payload, path)); ok {
			return usage, true
		}
	}
	return tokenUsage{}, false
}

func tokenUsageFromMap(m map[string]any) (tokenUsage, bool) {
	var usage tokenUsage
	if v, ok := numberField(m, "input_tokens", "prompt_tokens", "input", "inputTokens", "promptTokens"); ok {
		usage.input, usage.hasInput = v, true
	}
	if v, ok := numberField(m, "output_tokens", "completion_tokens", "output", "completion", "outputTokens", "completionTokens"); ok {
		usage.output, usage.hasOutput = v, true
	}
	if v, ok := numberField(m, "total_tokens", "total", "totalTokens"); ok {
		usage.total, usage.hasTotal = v, true
	}
	if !usage.hasTotal && (usage.hasInput || usage.hasOutput) {
		usage.total, usage.hasTotal = usage.input+usage.output, true
	}
	return usage, usage.hasInput || usage.hasOutput || usage.hasTotal
}

func applyTokenUsage(run *RunningEntry, usage tokenUsage) (inputDelta, outputDelta, totalDelta int64) {
	if usage.hasInput {
		inputDelta = usage.input
		if usage.absolute {
			inputDelta = positiveDelta(usage.input, run.CodexInputTokens)
			run.LastReportedInputTokens += inputDelta
		}
	}
	if usage.hasOutput {
		outputDelta = usage.output
		if usage.absolute {
			outputDelta = positiveDelta(usage.output, run.CodexOutputTokens)
			run.LastReportedOutputTokens += outputDelta
		}
	}
	if usage.hasTotal {
		totalDelta = usage.total
		if usage.absolute {
			totalDelta = positiveDelta(usage.total, run.CodexTotalTokens)
			run.LastReportedTotalTokens += totalDelta
		}
	} else {
		totalDelta = inputDelta + outputDelta
	}
	run.CodexInputTokens += inputDelta
	run.CodexOutputTokens += outputDelta
	run.CodexTotalTokens += totalDelta
	return inputDelta, outputDelta, totalDelta
}

func positiveDelta(next, prev int64) int64 {
	if next <= prev {
		return 0
	}
	return next - prev
}

func rateLimitsFromPayload(payload any) (map[string]any, bool) {
	root, ok := asStringMap(payload)
	if !ok {
		return nil, false
	}
	for _, path := range [][]string{
		{"rate_limits"},
		{"msg", "payload", "info", "rate_limits"},
		{"msg", "info", "rate_limits"},
	} {
		if limits := mapAtPath(root, path); limits != nil {
			return copyAnyMap(limits), true
		}
	}
	return nil, false
}

func asStringMap(v any) (map[string]any, bool) {
	switch typed := v.(type) {
	case map[string]any:
		return typed, true
	case RateLimitSnapshot:
		return map[string]any(typed), true
	case *RateLimitSnapshot:
		if typed == nil {
			return nil, false
		}
		return map[string]any(*typed), true
	default:
		return nil, false
	}
}

func mapAtPath(m map[string]any, path []string) map[string]any {
	var current any = m
	for _, key := range path {
		next, ok := asStringMap(current)
		if !ok {
			return nil
		}
		current = next[key]
	}
	out, _ := asStringMap(current)
	return out
}

func stringField(m map[string]any, key string) (string, bool) {
	v, ok := m[key].(string)
	return v, ok && v != ""
}

func numberField(m map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		if n, ok := integerLike(m[key]); ok {
			return n, true
		}
	}
	return 0, false
}

func integerLike(v any) (int64, bool) {
	switch typed := v.(type) {
	case int:
		return int64(typed), typed >= 0
	case int64:
		return typed, typed >= 0
	case float64:
		n := int64(typed)
		return n, typed >= 0
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return n, err == nil && n >= 0
	default:
		return 0, false
	}
}

func copyAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = copyAnyValue(value)
	}
	return out
}

func copyAnyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return copyAnyMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = copyAnyValue(item)
		}
		return out
	case RateLimitSnapshot:
		return copyAnyMap(map[string]any(typed))
	default:
		return value
	}
}

type runtimeEventForwardingEmitter struct {
	worker.EventEmitter
	Orchestrator *Orchestrator
	IssueID      string
}

func (e runtimeEventForwardingEmitter) AddEvent(ctx context.Context, taskID, typ, msg string) error {
	return e.AddEventWithPayload(ctx, taskID, typ, msg, nil)
}

func (e runtimeEventForwardingEmitter) AddEventWithPayload(ctx context.Context, taskID, typ, msg string, payload any) error {
	if _, ok := runtimeEventKinds[typ]; ok && e.Orchestrator != nil && e.IssueID != "" {
		_ = e.Orchestrator.RecordRuntimeEvent(ctx, e.IssueID, task.RuntimeEvent{Event: typ, Payload: payload})
	}
	if e.EventEmitter == nil {
		return nil
	}
	return e.EventEmitter.AddEventWithPayload(ctx, taskID, typ, msg, payload)
}
