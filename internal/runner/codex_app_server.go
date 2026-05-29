package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
	"github.com/xrf9268-hue/aiops-platform/internal/workspace"
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
	// directly (no shell wrapper from a custom codex.command), and the
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
	head, tail := headTail(buf.Bytes())
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
type codexAppServerTextInput struct {
	Type         string `json:"type"`
	Text         string `json:"text"`
	TextElements []any  `json:"text_elements"`
}
type codexAppServerTurnStartParams struct {
	ThreadID       string                      `json:"threadId"`
	Input          []codexAppServerTextInput   `json:"input"`
	CWD            string                      `json:"cwd,omitempty"`
	Title          string                      `json:"title,omitempty"`
	ApprovalPolicy any                         `json:"approvalPolicy,omitempty"`
	SandboxPolicy  workflow.CodexSandboxPolicy `json:"sandboxPolicy"`
}

// run drives a full app-server session: cache the per-run config, complete the
// SPEC §10.1 handshake, then loop turns until one stops the run (clean exit /
// §16.5 self-stop) or the continuation cap is hit. The init → startThread →
// per-turn split mirrors upstream start_session / run_turn.
func (c *appServerClient) run(ctx context.Context, in RunInput, prompt string) error {
	c.initSession(in)
	threadID, err := c.startThread(ctx, in)
	if err != nil {
		return err
	}
	maxTurns := in.Workflow.Config.Agent.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 20
	}
	for turn := 1; turn <= maxTurns; turn++ {
		keepGoing, err := c.runSingleTurn(ctx, in, threadID, prompt, turn)
		if err != nil {
			return err
		}
		if !keepGoing {
			return nil
		}
	}
	return fmt.Errorf("codex app-server exceeded agent.max_turns=%d", maxTurns)
}

// initSession records the run's sinks and caches the per-run codex config off
// RunInput before any protocol traffic.
func (c *appServerClient) initSession(in RunInput) {
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
}

// startThread runs the SPEC §10.1 handshake — initialize, initialized,
// thread/start — and returns the started thread id. Mirrors upstream
// start_session.
func (c *appServerClient) startThread(ctx context.Context, in RunInput) (string, error) {
	if _, err := c.request(ctx, "initialize", map[string]any{
		"capabilities": map[string]any{"experimentalApi": true},
		"clientInfo": map[string]any{
			"name":    "aiops-platform",
			"title":   "AIOps Platform",
			"version": "0.1.0",
		},
	}); err != nil {
		c.recordStartupFailed("initialize", err)
		return "", fmt.Errorf("codex app-server initialize: %w", err)
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		c.recordStartupFailed("initialized", err)
		return "", err
	}
	threadResult, err := c.request(ctx, "thread/start", map[string]any{
		"approvalPolicy": c.approvalPolicy,
		"sandbox":        in.Workflow.Config.Codex.ThreadSandbox,
		"cwd":            in.Workdir,
		"dynamicTools":   appServerDynamicToolSpecs(in.Workflow.Config),
	})
	if err != nil {
		c.recordStartupFailed("thread/start", err)
		return "", fmt.Errorf("codex app-server thread/start: %w", err)
	}
	threadID, err := extractString(threadResult, "thread", "id")
	if err != nil {
		c.recordStartupFailed("thread/start", err)
		return "", fmt.Errorf("codex app-server thread/start: %w", err)
	}
	c.threadID = threadID
	c.recordPhaseTransition(task.PhaseLaunchingAgentProcess, task.PhaseInitializingSession)
	return threadID, nil
}

// runSingleTurn starts one turn, awaits its completion, and reports whether the
// loop should continue. Mirrors upstream run_turn: keepGoing=false stops the run
// (a clean turn with continue=false, or a SPEC §16.5 self-stop), a non-nil error
// aborts it.
func (c *appServerClient) runSingleTurn(ctx context.Context, in RunInput, threadID, prompt string, turn int) (bool, error) {
	turnID, err := c.startTurn(ctx, in, threadID, prompt, turn)
	if err != nil {
		return false, err
	}
	c.turnID = turnID
	if turn == 1 {
		c.recordFirstTurnStarted(threadID, turnID)
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
		return false, c.classifyTurnError(ctx, err, turnStarted)
	}
	// SPEC §16.5: refresh tracker state between turns so an operator who
	// cancelled the issue mid-run sees the worker exit after the current turn
	// rather than at the next orchestrator poll tick. Errors here are surfaced
	// verbatim per SPEC ("if refreshed_issue failed: fail"); a nil refresher
	// keeps the legacy continueRun-only path for callers (mock runner, tests)
	// with no tracker hook.
	if c.refreshIssueState != nil {
		active, err := c.refreshIssueState(ctx)
		if err != nil {
			return false, fmt.Errorf("codex app-server refresh issue state: %w", err)
		}
		if !active {
			return false, nil
		}
	}
	return c.continueRun, nil
}

// startTurn issues turn/start and resolves the turn id. A failure on the first
// turn is also recorded as a startup failure (the session never produced a
// turn). Mirrors upstream start_turn.
func (c *appServerClient) startTurn(ctx context.Context, in RunInput, threadID, prompt string, turn int) (string, error) {
	turnResult, err := c.request(ctx, "turn/start", codexAppServerTurnStartParams{
		ThreadID:       threadID,
		Input:          turnInput(in, prompt, turn),
		CWD:            in.Workdir,
		Title:          appServerTurnTitle(in),
		ApprovalPolicy: c.approvalPolicy,
		SandboxPolicy:  in.Workflow.Config.Codex.TurnSandboxPolicy,
	})
	if err != nil {
		if turn == 1 {
			c.recordStartupFailed("turn/start", err)
		}
		return "", fmt.Errorf("codex app-server turn/start: %w", err)
	}
	turnID, err := extractString(turnResult, "turn", "id")
	if err != nil {
		if turn == 1 {
			c.recordStartupFailed("turn/start", err)
		}
		return "", fmt.Errorf("codex app-server turn/start: %w", err)
	}
	return turnID, nil
}

// turnInput builds the turn/start input: the full prompt on the first turn, a
// continuation nudge thereafter.
func turnInput(in RunInput, prompt string, turn int) []codexAppServerTextInput {
	text := prompt
	if turn > 1 {
		text = appServerContinuationPrompt(in, turn)
	}
	return []codexAppServerTextInput{{
		Type:         "text",
		Text:         text,
		TextElements: []any{},
	}}
}

// recordFirstTurnStarted emits the SPEC §10.4 session_started event and the
// initializing→streaming phase transition, once, on the first turn.
func (c *appServerClient) recordFirstTurnStarted(threadID, turnID string) {
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

// classifyTurnError maps an awaitTurnCompletion failure to the run's returned
// error: a *StallError and a generic error pass through unchanged; a per-turn
// deadline that fired while the outer run context is still alive becomes a
// *TurnTimeoutError.
func (c *appServerClient) classifyTurnError(ctx context.Context, err error, turnStarted time.Time) error {
	var stall *StallError
	if errors.As(err, &stall) {
		return err
	}
	if c.turnTimeoutMs > 0 && errors.Is(err, context.DeadlineExceeded) && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &TurnTimeoutError{Timeout: time.Duration(c.turnTimeoutMs) * time.Millisecond, Elapsed: time.Since(turnStarted), Cause: err}
	}
	return err
}
func (c *appServerClient) request(ctx context.Context, method string, params any) (map[string]any, error) {
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
	return remaining, true
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
	_ = workspace.WriteSensitiveArtifact(filepath.Join(workdir, codexAppServerOutputPath), body)
}
