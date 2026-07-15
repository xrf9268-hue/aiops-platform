package orchestrator

import (
	"context"
	"fmt"
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
	var cancel context.CancelCauseFunc
	if run := st.Running[r.issueID]; run != nil {
		if st.recordRuntimeEvent(run, r.event, r.now) {
			cancel = run.CancelWorker
		}
	}
	return func() {
		if cancel != nil {
			cancel(nil)
		}
		close(r.done)
	}
}

func (s *OrchestratorState) recordRuntimeEvent(run *RunningEntry, event task.RuntimeEvent, now time.Time) bool {
	if s == nil || run == nil || event.Event == "" {
		return false
	}
	// workflow_resolved is a worker lifecycle event, not a SPEC §10.4 app-server
	// runtime event: fold its profile identity but return before the general
	// fold so it never lands in last_event/last_message or resets the §8.5 stall
	// clock (LastEventAt) — the runner's session/turn events own those.
	if event.Event == task.EventWorkflowResolved {
		s.recordWorkflowFields(run, event)
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	run.LastEventAt = now
	run.LastCodexEvent = event.Event
	if payload, ok := asStringMap(event.Payload); ok {
		if msg, ok := stringField(payload, "message"); ok {
			run.LastCodexMessage = msg
		}
	}
	s.recordSessionFields(run, event)
	s.recordStartupFailureFields(run, event)
	s.recordAgentHandoffFields(run, event)
	s.recordInputRequiredFields(run, event, now)
	if usage, ok := tokenUsageFromEvent(event); ok {
		input, output, total := applyTokenUsage(run, usage)
		s.CodexTotals.AddTokenDelta(input, output, total)
	}
	if limits, ok := rateLimitsFromPayload(event.Payload); ok {
		snap := RateLimitSnapshot(limits)
		s.RecordRateLimits(&snap)
	}
	return s.markBudgetExceededIfNeeded(run, now)
}

func (s *OrchestratorState) markBudgetExceededIfNeeded(run *RunningEntry, now time.Time) bool {
	if s == nil || run == nil || run.BudgetExceeded {
		return false
	}
	message, exceeded := s.budgetExceededMessage(run, now)
	if !exceeded {
		return false
	}
	run.BudgetExceeded = true
	run.BudgetExceededAt = now
	run.BudgetExceededError = message
	run.LastCodexMessage = message
	s.RecordEvent(RuntimeEvent{
		Kind:       RuntimeEventBudgetExceeded,
		IssueID:    IssueID(run.Issue.ID),
		Identifier: run.Identifier,
		Message:    message,
		At:         now,
	})
	return true
}

func (s *OrchestratorState) budgetExceededMessage(run *RunningEntry, now time.Time) (string, bool) {
	guard := s.BudgetGuardrails
	if guard.MaxTokensPerClaim > 0 && run.CodexTotalTokens > guard.MaxTokensPerClaim {
		return fmt.Sprintf(
			"worker-observed, runner-reported Codex claim token budget exceeded: "+
				"current_claim_total_tokens=%d max_tokens_per_claim=%d; external review and otherwise unreported nested or subagent usage are excluded",
			run.CodexTotalTokens,
			guard.MaxTokensPerClaim,
		), true
	}
	if guard.MaxRuntimeSecondsPerClaim > 0 {
		runtimeSeconds := runtimeSecondsSince(now, run.StartedAt)
		if runtimeSeconds > float64(guard.MaxRuntimeSecondsPerClaim) {
			return fmt.Sprintf("claim runtime budget exceeded: runtime_seconds=%.0f max_runtime_seconds_per_claim=%d", runtimeSeconds, guard.MaxRuntimeSecondsPerClaim), true
		}
	}
	return "", false
}

func (s *OrchestratorState) recordStartupFailureFields(run *RunningEntry, event task.RuntimeEvent) {
	if event.Event != task.EventStartupFailed {
		return
	}
	payload, _ := asStringMap(event.Payload)
	phase, ok := stringField(payload, "phase")
	if !ok || phase == "" {
		return
	}
	failure := &task.StartupFailure{Phase: phase}
	if msg, ok := stringField(payload, "error"); ok {
		failure.Error = msg
	}
	run.LastStartupFailure = failure
}

func (s *OrchestratorState) recordAgentHandoffFields(run *RunningEntry, event task.RuntimeEvent) {
	if event.Event != task.EventToolCallMutation {
		return
	}
	payload, _ := asStringMap(event.Payload)
	if !agentHandoffMutationPayload(payload) {
		return
	}
	if handoffBool(payload, "current_issue_non_active_state_update") {
		run.AgentCurrentIssueHandoff = true
	}
	if handoffBool(payload, "current_issue_terminal_state_update") {
		recordCurrentIssueTerminalHandoffState(run, payload)
	}
}

// isAgentHandoffMutationTool names the agent-visible tracker mutation tools
// whose tool_call_mutation events can carry a current-issue handoff
// classification. The Gitea label tool joined the taxonomy in #748 so a Gitea
// run's handoff label flip is counted like a Linear state update.
func isAgentHandoffMutationTool(tool string) bool {
	switch tool {
	case "linear_graphql", "linear_ai_workpad", "gitea_issue_labels":
		return true
	default:
		return false
	}
}

func agentHandoffMutationPayload(payload map[string]any) bool {
	tool, ok := stringField(payload, "tool")
	return ok && isAgentHandoffMutationTool(tool)
}

func handoffBool(payload map[string]any, key string) bool {
	v, ok := boolField(payload, key)
	return ok && v
}

func recordCurrentIssueTerminalHandoffState(run *RunningEntry, payload map[string]any) {
	state, ok := stringField(payload, "current_issue_terminal_state")
	if !ok {
		return
	}
	run.AgentCurrentIssueTerminalHandoffState = strings.TrimSpace(state)
	run.AgentCurrentIssueTerminalHandoff = run.AgentCurrentIssueTerminalHandoffState != ""
}

func (s *OrchestratorState) recordInputRequiredFields(run *RunningEntry, event task.RuntimeEvent, now time.Time) {
	if event.Event != task.EventTurnInputRequired {
		return
	}
	run.InputRequired = true
	run.InputRequiredAt = now
	payload, _ := asStringMap(event.Payload)
	if method, ok := stringField(payload, "method"); ok {
		run.InputRequiredMethod = method
	}
}

// recordWorkflowFields folds the workflow_resolved event's profile identity
// (Resolution.Source / .Path) into the live session so /api/v1/state can report
// which WORKFLOW.md produced a run (#983). The worker omits `path` from the
// payload when Source == default, so WorkflowPath stays empty there while
// WorkflowSource still records "default". The worker emits workflow_resolved
// once per task (ResolveWorkflow), so this is effectively a single write;
// last-write-wins is intentional should a future caller re-resolve mid-run.
func (s *OrchestratorState) recordWorkflowFields(run *RunningEntry, event task.RuntimeEvent) {
	payload, ok := asStringMap(event.Payload)
	if !ok {
		return
	}
	if source, ok := stringField(payload, "source"); ok {
		run.Session.WorkflowSource = source
	}
	if path, ok := stringField(payload, "path"); ok {
		run.Session.WorkflowPath = path
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
	if pid, ok := intField(payload, "codex_app_server_pid"); ok && pid > 0 {
		run.Session.CodexAppServerPID = pid
	}
	if provider, ok := stringField(payload, "agent_provider"); ok {
		run.Session.AgentProvider = provider
	}
	if model, ok := stringField(payload, "agent_model"); ok {
		run.Session.AgentModel = model
	}
	if event.Event == task.EventTurnCompleted {
		run.Session.TurnCount++
	}
}

type tokenUsage struct {
	input, output, total int64
	hasInput, hasOutput  bool
	hasTotal             bool
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
			return usage, true
		}
	}
	if event.Event != task.EventTurnCompleted {
		return tokenUsage{}, false
	}
	for _, path := range [][]string{{"usage"}, {"turn", "usage"}, nil} {
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
	return usage, usage.hasInput || usage.hasOutput || usage.hasTotal
}

func applyTokenUsage(run *RunningEntry, usage tokenUsage) (inputDelta, outputDelta, totalDelta int64) {
	if usage.hasInput {
		inputDelta = positiveDelta(usage.input, run.LastReportedInputTokens)
		run.LastReportedInputTokens = max(run.LastReportedInputTokens, usage.input)
		run.CodexInputTokens = run.LastReportedInputTokens
	}
	if usage.hasOutput {
		outputDelta = positiveDelta(usage.output, run.LastReportedOutputTokens)
		run.LastReportedOutputTokens = max(run.LastReportedOutputTokens, usage.output)
		run.CodexOutputTokens = run.LastReportedOutputTokens
	}
	if usage.hasTotal {
		totalDelta = positiveDelta(usage.total, run.LastReportedTotalTokens)
		run.LastReportedTotalTokens = max(run.LastReportedTotalTokens, usage.total)
		run.CodexTotalTokens = run.LastReportedTotalTokens
	}
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

func boolField(m map[string]any, key string) (bool, bool) {
	v, ok := m[key].(bool)
	return v, ok
}

// intField returns a positive integer from a payload key, accepting Go int,
// int64, or JSON-decoded float64. Returns (0, false) when the value is
// missing, non-numeric, or non-positive.
func intField(m map[string]any, key string) (int, bool) {
	n, ok := integerLike(m[key])
	if !ok || n <= 0 {
		return 0, false
	}
	return int(n), true
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
	// workflow_resolved is forwarded explicitly rather than via runtimeEventKinds
	// membership: that set is the SPEC §10.4 runner app-server vocabulary, and
	// workflow_resolved is a worker lifecycle event. recordRuntimeEvent special-
	// cases it to fold only the profile identity (#983), not the last-event fold.
	_, isRuntimeEvent := runtimeEventKinds[typ]
	if (isRuntimeEvent || typ == task.EventWorkflowResolved) && e.Orchestrator != nil && e.IssueID != "" {
		_ = e.Orchestrator.RecordRuntimeEvent(ctx, e.IssueID, task.RuntimeEvent{Event: typ, Payload: payload})
	}
	// The generic per-notification stream (SPEC §10.4 agent-driven notifications:
	// delta, reasoning, exec output, token_usage, rate_limits, …) is high-frequency
	// and already surfaced live through RecordRuntimeEvent → /api/v1/state + TUI —
	// the same recipient upstream feeds via send_codex_update. Upstream keeps it out
	// of the operator log (Logger.debug, method name only, filtered at the default
	// level); the worker's stdlib log has no levels, so echoing every notification
	// payload here drowned the orchestrator lifecycle events (~80:1 on a trivial
	// issue, #559). Forward it to the live state surface only, not the process log.
	if typ == task.EventNotification {
		return nil
	}
	if e.EventEmitter == nil {
		return nil
	}
	return e.EventEmitter.AddEventWithPayload(ctx, taskID, typ, msg, payload)
}
