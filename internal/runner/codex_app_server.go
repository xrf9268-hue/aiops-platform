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

const (
	// appServerScannerInitialBuf is the Scanner's starting buffer size; it grows
	// as needed up to maxAppServerLineBytes.
	appServerScannerInitialBuf = 64 << 10
	// maxAppServerLineBytes caps each Codex app-server stdio line per SPEC §10.1
	// ("Max line size: 10 MB"). Lines exceeding this surface as bufio.ErrTooLong
	// instead of growing the buffer unbounded and OOMing the worker.
	maxAppServerLineBytes = 10 * 1024 * 1024
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
	if err := validateAppServerWorkdir(in.Workdir); err != nil {
		return Result{}, err
	}
	promptAbs := filepath.Join(in.Workdir, PromptPath)
	prompt, err := os.ReadFile(promptAbs)
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", PromptPath, err)
	}

	env := agentEnv(in.Workflow.Config.Codex.EnvPassthrough, in.Workflow.Config)
	cmd, directCodexExec, err := buildCodexAppServerCmd(ctx, in, env)
	if err != nil {
		return Result{}, err
	}
	cmd.Dir = in.Workdir
	cmd.Env = env
	if err := validateAgentCommandWorkdir(in, cmd); err != nil {
		return Result{}, err
	}
	sandboxEnabled := in.Workflow.Config.Sandbox.Enabled && in.Workflow.Config.Sandbox.Backend != "" && in.Workflow.Config.Sandbox.Backend != "none"
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

	sc := bufio.NewScanner(stdout)
	// Scanner returns ErrTooLong when the buffer fills with no token boundary,
	// so passing maxAppServerLineBytes+1 as the cap allows tokens up to (and
	// including) maxAppServerLineBytes and rejects anything strictly larger.
	sc.Buffer(make([]byte, 0, appServerScannerInitialBuf), maxAppServerLineBytes+1)
	// Only emit codex_app_server_pid when we can guarantee cmd.Process.Pid is
	// the actual codex process: the command must launch the codex binary
	// directly (no `sh -c` wrapper from a custom codex.command), and the
	// sandbox must not have wrapped cmd in firejail/bwrap. In any wrapper
	// scenario the PID belongs to the wrapper, which would mislead operators
	// trying to map `/api/v1/state` rows to a host process. omitempty makes
	// the JSON field absent in those cases.
	pid := 0
	if directCodexExec && !sandboxEnabled && cmd.Process != nil {
		pid = cmd.Process.Pid
	}
	client := &appServerClient{
		stdin:             stdin,
		scanner:           sc,
		out:               buf,
		codexAppServerPID: pid,
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
		if _, ok := ErrorCategory(runErr); ok {
			return res, runErr
		}
		if waitErr != nil {
			return res, NewError(CategoryPortExit, "codex app-server process exited", errors.Join(runErr, waitErr))
		}
		return res, runErr
	}
	if waitErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return res, &TimeoutError{Timeout: deadlineBudget(ctx, start), Elapsed: elapsed, Cause: waitErr}
		}
		return res, NewError(CategoryPortExit, "codex app-server process exited", waitErr)
	}
	return res, nil
}

func validateAppServerWorkdir(workdir string) error {
	if strings.TrimSpace(workdir) == "" {
		return NewError(CategoryInvalidWorkspaceCWD, "codex app-server workspace cwd is empty", nil)
	}
	info, err := os.Stat(workdir)
	if err != nil {
		return NewError(CategoryInvalidWorkspaceCWD, "codex app-server workspace cwd is invalid", err)
	}
	if !info.IsDir() {
		return NewError(CategoryInvalidWorkspaceCWD, "codex app-server workspace cwd is not a directory", nil)
	}
	return nil
}

// buildCodexAppServerCmd returns the codex app-server *exec.Cmd plus a
// directCodexExec flag. The flag is true only when the command launches the
// `codex` binary directly (so `cmd.Process.Pid` after Start() is the actual
// app-server pid). Custom `codex.command` configurations route through
// `sh -c <command>`, in which case `cmd.Process.Pid` is the shell wrapper,
// not codex — see codex_app_server.go's session_started emit for the resulting
// PID-emission guard.
func buildCodexAppServerCmd(ctx context.Context, in RunInput, env []string) (*exec.Cmd, bool, error) {
	command := strings.TrimSpace(in.Workflow.Config.Codex.Command)
	if command == "" || command == "codex exec" {
		command = "codex app-server"
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return nil, false, fmt.Errorf("codex app-server command is empty")
	}
	if fields[0] == "codex" {
		codexPath, err := lookPathInEnv("codex", env)
		if err != nil {
			return nil, false, NewError(CategoryCodexNotFound, "codex binary not found in PATH; install codex CLI or set agent.default to claude/mock", err)
		}
		return exec.CommandContext(ctx, codexPath, fields[1:]...), true, nil
	}
	return exec.CommandContext(ctx, "sh", "-c", command), false, nil
}

type appServerClient struct {
	stdin               io.Writer
	scanner             *bufio.Scanner
	out                 io.Writer
	nextID              int
	codexAppServerPID   int
	threadID            string
	turnID              string
	lastMessage         string
	runtimeEvents       []task.RuntimeEvent
	runtimeEventSink    func(task.RuntimeEvent)
	phaseTransitionSink func(from, to task.RunAttemptPhase)
	// continueRun is the agent-emitted "should we keep going?" signal
	// from turn/completed notifications (`params.continue`). It only
	// gates the legacy path: when refreshIssueState is wired, SPEC §16.5
	// per-turn tracker refresh is the authoritative continuation gate
	// and continueRun is consulted only as the agent's secondary opinion.
	// Keeping both lets cooperative agents end early (continueRun=false)
	// while still letting the operator cancel an otherwise-productive
	// worker by moving the issue out of the active states.
	continueRun       bool
	refreshIssueState IssueStateRefresher
	tools             DynamicToolSet
	turnTimeoutMs     int
	readTimeoutMs     int
	stallTimeoutMs    int
	approvalPolicy    any
	lastTerminal      time.Time
}

func (c *appServerClient) run(ctx context.Context, in RunInput, prompt string) error {
	c.runtimeEventSink = in.RuntimeEventSink
	c.phaseTransitionSink = in.PhaseTransitionSink
	c.nextID = 1
	c.continueRun = false
	c.refreshIssueState = in.RefreshIssueState
	c.tools = DynamicToolsForWorkflow(workflow.Workflow{Config: in.Workflow.Config})
	c.turnTimeoutMs = in.Workflow.Config.Codex.TurnTimeoutMs
	c.readTimeoutMs = in.Workflow.Config.Codex.ReadTimeoutMs
	c.stallTimeoutMs = in.Workflow.Config.Codex.StallTimeoutMs
	// Translate once at this boundary and reuse the same value for both the
	// codex wire payload (thread/start, turn/start) and the harness-side
	// auto-approve decisions (autoApproveRequest). Storing the raw workflow
	// value here while sending the translated one to codex desyncs the two:
	// a legacy reject:{...} config would reach codex as granular:{...} (codex
	// emits approval prompts) but autoApproveRequest, which no longer handles
	// reject:, would decline them unconditionally and flip behavior (#335).
	c.approvalPolicy = codexWireApprovalPolicy(in.Workflow.Config.Codex.ApprovalPolicy)
	if _, err := c.request(ctx, "initialize", map[string]any{
		"capabilities": map[string]any{"experimentalApi": true},
		"clientInfo": map[string]any{
			"name":    "aiops-platform",
			"title":   "AIOps Platform",
			"version": "0.1.0",
		},
	}); err != nil {
		c.recordStartupFailed("initialize", err)
		return fmt.Errorf("codex app-server initialize: %w", err)
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		c.recordStartupFailed("initialized", err)
		return err
	}

	threadResult, err := c.request(ctx, "thread/start", map[string]any{
		"approvalPolicy": c.approvalPolicy,
		"sandbox":        in.Workflow.Config.Codex.ThreadSandbox,
		"cwd":            in.Workdir,
		"dynamicTools":   appServerDynamicToolSpecs(in.Workflow.Config),
	})
	if err != nil {
		c.recordStartupFailed("thread/start", err)
		return fmt.Errorf("codex app-server thread/start: %w", err)
	}
	threadID, err := extractString(threadResult, "thread", "id")
	if err != nil {
		c.recordStartupFailed("thread/start", err)
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
			"approvalPolicy": c.approvalPolicy,
			"sandboxPolicy":  in.Workflow.Config.Codex.TurnSandboxPolicy,
		})
		if err != nil {
			if turn == 1 {
				c.recordStartupFailed("turn/start", err)
			}
			return fmt.Errorf("codex app-server turn/start: %w", err)
		}
		turnID, err := extractString(turnResult, "turn", "id")
		if err != nil {
			if turn == 1 {
				c.recordStartupFailed("turn/start", err)
			}
			return fmt.Errorf("codex app-server turn/start: %w", err)
		}
		c.turnID = turnID
		if turn == 1 {
			payload := map[string]any{
				"session_id": threadID + "-" + turnID,
				"thread_id":  threadID,
				"turn_id":    turnID,
			}
			if c.codexAppServerPID > 0 {
				payload["codex_app_server_pid"] = c.codexAppServerPID
			}
			c.recordRuntimeEvent(task.EventSessionStarted, payload)
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
		// SPEC §16.5: refresh tracker state between turns so an
		// operator who cancelled the issue mid-run sees the worker
		// exit after the current turn rather than at the next
		// orchestrator poll tick. Errors here are surfaced verbatim per
		// SPEC ("if refreshed_issue failed: fail"); a nil refresher
		// keeps the legacy continueRun-only path for callers (mock
		// runner, tests) with no tracker hook.
		if c.refreshIssueState != nil {
			active, err := c.refreshIssueState(ctx)
			if err != nil {
				return fmt.Errorf("codex app-server refresh issue state: %w", err)
			}
			if !active {
				return nil
			}
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
				return nil, NewError(CategoryResponseError, fmt.Sprintf("rpc error: %v", e), nil)
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
	msg, raw, err := c.readProtocolMessage(ctx)
	if err != nil {
		if raw != nil {
			return nil, fmt.Errorf("decode codex app-server message: %w", err)
		}
		return nil, err
	}
	return msg, nil
}

func (c *appServerClient) readProtocolMessage(ctx context.Context) (map[string]any, []byte, error) {
	select {
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	default:
	}
	line, err := c.readLine(ctx)
	if err != nil {
		return nil, nil, err
	}
	// Scanner strips the line terminator; restore one in the transcript so
	// successive JSON-RPC messages remain visually separated.
	_, _ = c.out.Write(line)
	_, _ = c.out.Write([]byte{'\n'})
	var msg map[string]any
	if err := json.Unmarshal(line, &msg); err != nil {
		return nil, line, NewError(CategoryResponseError, "decode codex app-server message", err)
	}
	return msg, line, nil
}

type readResult struct {
	line []byte
	err  error
}

func (c *appServerClient) readLine(ctx context.Context) ([]byte, error) {
	ch := make(chan readResult, 1)
	go func() {
		if c.scanner.Scan() {
			// scanner.Bytes() is invalidated by the next Scan; copy before
			// crossing the goroutine boundary.
			line := append([]byte(nil), c.scanner.Bytes()...)
			ch <- readResult{line: line, err: nil}
			return
		}
		err := c.scanner.Err()
		if err == nil {
			err = io.EOF
		}
		if errors.Is(err, bufio.ErrTooLong) {
			err = fmt.Errorf("codex app-server line exceeded %d bytes: %w", maxAppServerLineBytes, err)
		}
		ch <- readResult{err: err}
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

func protocolMessageCandidate(raw []byte) bool {
	return strings.HasPrefix(strings.TrimLeft(string(raw), " \t\r\n"), "{")
}

func trimProtocolLine(raw []byte) string {
	return strings.TrimRight(string(raw), "\r\n")
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

func (c *appServerClient) recordRuntimeMessage(event string, msg map[string]any) {
	params, _ := msg["params"].(map[string]any)
	payload := normalizeRuntimePayload(params)
	if payload == nil {
		payload = map[string]any{}
	}
	c.recordRuntimeEvent(event, c.withRuntimeContext(payload))
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

func (c *appServerClient) recordSafeTurnFailure(event string, params map[string]any) {
	c.recordRuntimeEvent(event, c.withRuntimeContext(safeTurnFailurePayload(params)))
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
			// Install the audit sink for any tool that may route
			// through the Linear GraphQL proxy. linear_ai_workpad
			// composes deterministic commentCreate/commentUpdate
			// mutations through the same token-isolated transport
			// (via callRaw); operators need the runtime event for
			// those harness-attributable writes too. Only the
			// proxy itself fires the sink, so installing it on
			// unrelated tools is a no-op.
			toolCtx := WithLinearGraphQLMutationSink(ctx, func(operationField string) {
				payload := map[string]any{"tool": name}
				if operationField != "" {
					payload["operation_field"] = operationField
				}
				c.recordRuntimeEvent(task.EventToolCallMutation, c.withRuntimeContext(payload))
			})
			result, err = tool.Call(toolCtx, call)
			if err != nil {
				result, err = dynamicToolFailure(err.Error())
			}
		}
		if err != nil {
			return err
		}
	} else {
		// SPEC §10.4 unsupported_tool_call: the wire still carries the
		// structured failure result, but the orchestrator/state surface
		// needs a typed event to distinguish "agent invoked an
		// unadvertised tool" from "advertised tool failed". The structured
		// failure path above already emits its own error; this branch is
		// reached only when the tool name is not in c.tools.
		c.recordUnsupportedToolCall(name, arguments)
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

// appServerTurnTitle builds the Codex turn title following SPEC §10.2:
// "<issue.identifier>: <issue.title>". Task.SourceEventID is the
// tracker identifier (e.g. "AIOPS-64", "MT-649"); Task.ID is an
// internal queue nonce that means nothing to operators reading a
// Codex session log. Prefer the identifier; fall back to title alone
// if the identifier is unset; fall back to the task nonce only as a
// last resort so prompt-only tests still get something to dispatch.
func appServerTurnTitle(in RunInput) string {
	identifier := strings.TrimSpace(in.Task.SourceEventID)
	title := strings.TrimSpace(in.Task.Title)
	switch {
	case identifier != "" && title != "":
		return identifier + ": " + title
	case identifier != "":
		return identifier
	case title != "":
		return title
	default:
		return in.Task.ID
	}
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
		return "", NewError(CategoryResponseError, "missing "+outer, nil)
	}
	v, _ := o[inner].(string)
	if v == "" {
		return "", NewError(CategoryResponseError, fmt.Sprintf("missing %s.%s", outer, inner), nil)
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
