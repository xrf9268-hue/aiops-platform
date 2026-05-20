package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// CodexAppServerRunner talks to `codex app-server` over JSON-RPC 2.0 stdio.
// It is intentionally separate from CodexRunner so the existing one-shot
// `codex exec` path remains backwards-compatible while the app-server transport
// can grow toward the Symphony protocol.
type CodexAppServerRunner struct{}

const (
	codexAppServerOutputPath = ".aiops/CODEX_APP_SERVER_OUTPUT.txt"
	nonInteractiveInputReply = "This is a non-interactive session. Operator input is unavailable."
)

func (CodexAppServerRunner) Run(ctx context.Context, in RunInput) (Result, error) {
	promptAbs := filepath.Join(in.Workdir, PromptPath)
	prompt, err := os.ReadFile(promptAbs)
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", PromptPath, err)
	}

	cmd, err := buildCodexAppServerCmd(ctx, in)
	if err != nil {
		return Result{}, err
	}
	cmd.Dir = in.Workdir
	cmd, err = applySandbox(ctx, in, cmd)
	if err != nil {
		return Result{}, err
	}
	configurePlatformKill(cmd)
	cmd.WaitDelay = killGrace

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{}, fmt.Errorf("open codex app-server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, fmt.Errorf("open codex app-server stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf("open codex app-server stderr: %w", err)
	}

	buf := &cappedWriter{Cap: CodexOutputCap}
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(buf, stderr)
	}()

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}

	client := &appServerClient{
		stdin:  stdin,
		reader: bufio.NewReader(stdout),
		out:    buf,
	}
	runErr := client.run(ctx, in, string(prompt))
	if runErr != nil && ctx.Err() == nil {
		terminateProcess(cmd)
	}
	_ = stdin.Close()
	waitErr := cmd.Wait()
	<-stderrDone
	elapsed := time.Since(start)

	writeAppServerArtifact(in.Workdir, buf)
	res := Result{
		Summary:       client.summary(),
		RuntimeEvents: client.runtimeEvents,
		OutputBytes:   int64(len(buf.Bytes())),
		OutputDropped: buf.Dropped(),
	}
	head, tail := headTail(buf.Bytes(), CodexEventOutputCap)
	if len(head) > 0 {
		res.OutputHead = string(head)
	}
	res.OutputTail = tail

	if runErr != nil {
		var stall *StallError
		if errors.As(runErr, &stall) {
			return res, runErr
		}
		if isAppServerReadTimeout(runErr) {
			return res, &ReadTimeoutError{Timeout: time.Duration(client.readTimeoutMs) * time.Millisecond, Cause: runErr}
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return res, &TimeoutError{Timeout: deadlineBudget(ctx, start), Elapsed: elapsed, Cause: runErr}
		}
		return res, runErr
	}
	if waitErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return res, &TimeoutError{Timeout: deadlineBudget(ctx, start), Elapsed: elapsed, Cause: waitErr}
		}
		return res, waitErr
	}
	return res, nil
}

func buildCodexAppServerCmd(ctx context.Context, in RunInput) (*exec.Cmd, error) {
	command := strings.TrimSpace(in.Workflow.Config.Codex.Command)
	if command == "" || command == "codex exec" {
		command = "codex app-server"
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return nil, fmt.Errorf("codex app-server command is empty")
	}
	if fields[0] == "codex" {
		if _, err := exec.LookPath("codex"); err != nil {
			return nil, fmt.Errorf("codex binary not found in PATH; install codex CLI or set agent.default to claude/mock")
		}
		return exec.CommandContext(ctx, fields[0], fields[1:]...), nil
	}
	return exec.CommandContext(ctx, "sh", "-lc", command), nil
}

type appServerClient struct {
	stdin               io.Writer
	reader              *bufio.Reader
	out                 io.Writer
	nextID              int
	threadID            string
	turnID              string
	lastMessage         string
	runtimeEvents       []task.RuntimeEvent
	runtimeEventSink    func(task.RuntimeEvent)
	phaseTransitionSink func(from, to task.RunAttemptPhase)
	continueRun         bool
	tools               DynamicToolSet
	turnTimeoutMs       int
	readTimeoutMs       int
	stallTimeoutMs      int
	approvalPolicy      any
	lastTerminal        time.Time
}

func (c *appServerClient) run(ctx context.Context, in RunInput, prompt string) error {
	c.runtimeEventSink = in.RuntimeEventSink
	c.phaseTransitionSink = in.PhaseTransitionSink
	c.nextID = 1
	c.continueRun = false
	c.tools = DynamicToolsForWorkflow(workflow.Workflow{Config: in.Workflow.Config})
	c.turnTimeoutMs = in.Workflow.Config.Codex.TurnTimeoutMs
	c.readTimeoutMs = in.Workflow.Config.Codex.ReadTimeoutMs
	c.stallTimeoutMs = in.Workflow.Config.Codex.StallTimeoutMs
	c.approvalPolicy = in.Workflow.Config.Codex.ApprovalPolicy
	if _, err := c.request(ctx, "initialize", map[string]any{
		"capabilities": map[string]any{"experimentalApi": true},
		"clientInfo": map[string]any{
			"name":    "aiops-platform",
			"title":   "AIOps Platform",
			"version": "0.1.0",
		},
	}); err != nil {
		return fmt.Errorf("codex app-server initialize: %w", err)
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		return err
	}

	threadResult, err := c.request(ctx, "thread/start", map[string]any{
		"approvalPolicy": in.Workflow.Config.Codex.ApprovalPolicy,
		"sandbox":        in.Workflow.Config.Codex.ThreadSandbox,
		"cwd":            in.Workdir,
		"dynamicTools":   appServerDynamicToolSpecs(in.Workflow.Config),
	})
	if err != nil {
		return fmt.Errorf("codex app-server thread/start: %w", err)
	}
	threadID, err := extractString(threadResult, "thread", "id")
	if err != nil {
		return fmt.Errorf("codex app-server thread/start: %w", err)
	}
	c.threadID = threadID
	c.recordPhaseTransition(task.PhaseLaunchingAgentProcess, task.PhaseInitializingSession)

	maxTurns := in.Workflow.Config.Agent.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 20
	}
	for turn := 1; turn <= maxTurns; turn++ {
		input := []map[string]any{{
			"type": "text",
			"text": appServerContinuationPrompt(in, turn),
		}}
		if turn == 1 {
			input = []map[string]any{{
				"type": "text",
				"text": prompt,
			}}
		}
		turnResult, err := c.request(ctx, "turn/start", map[string]any{
			"threadId":       threadID,
			"input":          input,
			"cwd":            in.Workdir,
			"title":          appServerTurnTitle(in),
			"approvalPolicy": in.Workflow.Config.Codex.ApprovalPolicy,
			"sandboxPolicy":  in.Workflow.Config.Codex.TurnSandboxPolicy,
		})
		if err != nil {
			return fmt.Errorf("codex app-server turn/start: %w", err)
		}
		turnID, err := extractString(turnResult, "turn", "id")
		if err != nil {
			return fmt.Errorf("codex app-server turn/start: %w", err)
		}
		c.turnID = turnID
		if turn == 1 {
			c.recordRuntimeEvent(task.EventSessionStarted, map[string]any{
				"session_id": threadID + "-" + turnID,
				"thread_id":  threadID,
				"turn_id":    turnID,
			})
			c.recordPhaseTransition(task.PhaseInitializingSession, task.PhaseStreamingTurn)
		}
		c.continueRun = false
		turnCtx := ctx
		var cancel context.CancelFunc
		if c.turnTimeoutMs > 0 {
			turnCtx, cancel = context.WithTimeout(ctx, time.Duration(c.turnTimeoutMs)*time.Millisecond)
		}
		turnStarted := time.Now()
		err = c.awaitTurnCompletion(turnCtx)
		if cancel != nil {
			cancel()
		}
		if err != nil {
			var stall *StallError
			if errors.As(err, &stall) {
				return err
			}
			if c.turnTimeoutMs > 0 && errors.Is(err, context.DeadlineExceeded) && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return &TurnTimeoutError{Timeout: time.Duration(c.turnTimeoutMs) * time.Millisecond, Elapsed: time.Since(turnStarted), Cause: err}
			}
			return err
		}
		if !c.continueRun {
			return nil
		}
	}
	return fmt.Errorf("codex app-server exceeded agent.max_turns=%d", maxTurns)
}

func (c *appServerClient) request(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	id := c.nextID
	c.nextID++
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		msg, err := c.readMessage(ctx)
		if err != nil {
			return nil, err
		}
		if gotID, ok := numberID(msg["id"]); ok && gotID == id {
			if e, ok := msg["error"]; ok {
				return nil, fmt.Errorf("rpc error: %v", e)
			}
			result, _ := msg["result"].(map[string]any)
			if result == nil {
				result = map[string]any{}
			}
			return result, nil
		}
		c.handleNotification(msg)
	}
}

func (c *appServerClient) notify(method string, params map[string]any) error {
	return c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *appServerClient) send(msg map[string]any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = c.stdin.Write(b)
	return err
}

func (c *appServerClient) readMessage(ctx context.Context) (map[string]any, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	line, err := c.readLine(ctx)
	if err != nil {
		return nil, err
	}
	_, _ = c.out.Write(line)
	var msg map[string]any
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, fmt.Errorf("decode codex app-server message: %w", err)
	}
	return msg, nil
}

type readResult struct {
	line []byte
	err  error
}

func (c *appServerClient) readLine(ctx context.Context) ([]byte, error) {
	ch := make(chan readResult, 1)
	go func() {
		line, err := c.reader.ReadBytes('\n')
		ch <- readResult{line: line, err: err}
	}()

	readTimeout := time.Duration(c.readTimeoutMs) * time.Millisecond
	deadlineTimeout, hasDeadline := deadlineDuration(ctx)
	if c.readTimeoutMs <= 0 || (hasDeadline && deadlineTimeout < readTimeout) {
		return c.readLineOnce(ctx, ch, nil)
	}
	return c.readLineOnce(ctx, ch, time.After(readTimeout))
}

func (c *appServerClient) readLineOnce(ctx context.Context, ch <-chan readResult, timeout <-chan time.Time) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timeout:
		return nil, fmt.Errorf("codex app-server read timeout after %dms", c.readTimeoutMs)
	case res := <-ch:
		if res.err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ctx.Err()
			}
			return nil, res.err
		}
		return res.line, nil
	}
}

func deadlineDuration(ctx context.Context) (time.Duration, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0, true
	}
	return time.Duration(math.Ceil(float64(remaining))), true
}

func isAppServerReadTimeout(err error) bool {
	return err != nil && strings.Contains(err.Error(), "codex app-server read timeout")
}

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
		msg, err := c.readMessage(readCtx)
		c.readTimeoutMs = readTimeoutMs
		if cancel != nil {
			cancel()
		}
		if err != nil {
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
					return err
				}
				c.handleNotification(msg)
				c.recordRuntimeMessage(task.EventTurnCompleted, msg)
				return nil
			case "turn/failed", "turn/cancelled":
				if method == "turn/failed" {
					c.recordRuntimeMessage(task.EventTurnFailed, msg)
				} else {
					c.recordRuntimeMessage(task.EventTurnCancelled, msg)
				}
				return fmt.Errorf("%s: %v", method, msg["params"])
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
					c.lastTerminal = time.Now()
					continue
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
		}
	}
}

func (c *appServerClient) recordRuntimeMessage(event string, msg map[string]any) {
	params, _ := msg["params"].(map[string]any)
	payload := normalizeRuntimePayload(params)
	if payload == nil {
		payload = map[string]any{}
	}
	if _, ok := payload["thread_id"]; !ok && c.threadID != "" {
		payload["thread_id"] = c.threadID
	}
	if _, ok := payload["turn_id"]; !ok && c.turnID != "" {
		payload["turn_id"] = c.turnID
	}
	c.recordRuntimeEvent(event, payload)
}

func (c *appServerClient) recordRuntimeEvent(event string, payload map[string]any) {
	runtimeEvent := task.RuntimeEvent{Event: event, Payload: payload}
	c.runtimeEvents = append(c.runtimeEvents, runtimeEvent)
	if c.runtimeEventSink != nil {
		c.runtimeEventSink(runtimeEvent)
	}
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
		nextIsLower := false
		if i+1 < len(s) {
			next := rune(s[i+1])
			nextIsLower = next >= 'a' && next <= 'z'
		}
		if r >= 'A' && r <= 'Z' {
			prevIsLowerOrDigit := (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9')
			prevIsUpper := prev >= 'A' && prev <= 'Z'
			if i > 0 && (prevIsLowerOrDigit || (prevIsUpper && nextIsLower)) {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
		prev = rune(s[i])
	}
	return b.String()
}

func (c *appServerClient) replyServerRequest(msg map[string]any) error {
	method, _ := msg["method"].(string)
	if result, ok := protocolServerRequestResult(method, msg, c.approvalPolicy); ok {
		return c.send(map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": result})
	}
	return c.sendJSONRPCError(msg["id"], -32601, "Method not found: "+method)
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
	if reject, ok := policy["reject"].(map[string]any); ok {
		return !approvalRuleRequiresReview(reject, method)
	}
	return false
}

func approvalRuleRequiresReview(rules map[string]any, method string) bool {
	if method == "item/permissions/requestApproval" {
		return approvalRuleEnabled(rules, "request_permissions") ||
			approvalRuleEnabled(rules, "sandbox_approval") ||
			approvalRuleEnabled(rules, "rules")
	}
	return approvalRuleEnabled(rules, "sandbox_approval") || approvalRuleEnabled(rules, "rules")
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
	reason := ""
	reasonSources := []map[string]any{params}
	if turn, _ := params["turn"].(map[string]any); turn != nil {
		reasonSources = append(reasonSources, turn)
	}
	for _, source := range reasonSources {
		for _, key := range []string{"reason", "error", "message"} {
			if v, _ := source[key].(string); strings.TrimSpace(v) != "" {
				reason = strings.TrimSpace(v)
				break
			}
		}
		if reason != "" {
			break
		}
	}
	if reason == "" {
		for _, key := range []string{"reason", "error", "message"} {
			if v, _ := params[key].(string); strings.TrimSpace(v) != "" {
				reason = strings.TrimSpace(v)
				break
			}
		}
	}
	if reason == "" {
		reason = fmt.Sprint(params)
	}
	return fmt.Errorf("turn/completed failed with status %q: %s", status, reason)
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
	result, err := dynamicToolResult(false, "unsupported dynamic tool: "+name)
	if err != nil {
		return err
	}
	if tool, ok := c.tools.Lookup(name); ok {
		call := ToolCall{}
		if err := json.Unmarshal(arguments, &call); err != nil {
			result, err = dynamicToolFailure(err.Error())
		} else {
			result, err = tool.Call(ctx, call)
			if err != nil {
				result, err = dynamicToolFailure(err.Error())
			}
		}
		if err != nil {
			return err
		}
	}
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

func (c *appServerClient) summary() string {
	if strings.TrimSpace(c.lastMessage) != "" {
		return strings.TrimSpace(c.lastMessage)
	}
	return "codex app-server completed"
}

func appServerDynamicToolSpecs(cfg workflow.Config) []map[string]any {
	toolSet := DynamicToolsForWorkflow(workflow.Workflow{Config: cfg})
	names := toolSet.Names()
	specs := make([]map[string]any, 0, len(names))
	for _, name := range names {
		tool, ok := toolSet.Lookup(name)
		if !ok {
			continue
		}
		schema := tool.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		specs = append(specs, map[string]any{"name": tool.Name, "description": tool.Description, "inputSchema": schema})
	}
	return specs
}

func appServerTurnTitle(in RunInput) string {
	if in.Task.ID != "" && in.Task.Title != "" {
		return in.Task.ID + ": " + in.Task.Title
	}
	if in.Task.Title != "" {
		return in.Task.Title
	}
	return in.Task.ID
}

func appServerContinuationPrompt(in RunInput, turn int) string {
	subject := appServerTurnTitle(in)
	if strings.TrimSpace(subject) == "" {
		subject = "the current task"
	}
	return fmt.Sprintf("Continue working on %s. This is continuation turn %d; use the existing thread context, address any remaining requirements, and finish only when the task is complete.", subject, turn)
}

func extractString(m map[string]any, outer, inner string) (string, error) {
	o, _ := m[outer].(map[string]any)
	if o == nil {
		return "", fmt.Errorf("missing %s", outer)
	}
	v, _ := o[inner].(string)
	if v == "" {
		return "", fmt.Errorf("missing %s.%s", outer, inner)
	}
	return v, nil
}

func numberID(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	default:
		return 0, false
	}
}

func writeAppServerArtifact(workdir string, buf *cappedWriter) {
	dir := filepath.Join(workdir, ".aiops")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	body := append([]byte{}, buf.Bytes()...)
	if buf.Dropped() > 0 {
		footer := fmt.Sprintf("\n...output truncated at %d bytes\n", CodexOutputCap)
		body = append(body, []byte(footer)...)
	}
	_ = os.WriteFile(filepath.Join(workdir, codexAppServerOutputPath), body, 0o644)
}
