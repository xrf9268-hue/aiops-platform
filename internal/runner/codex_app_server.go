package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
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
// This is the SPEC §10 agent runner: a long-running app-server session that
// drives multiple coding-agent turns within one worker session.
type CodexAppServerRunner struct{}

const (
	codexAppServerOutputPath = ".aiops/CODEX_APP_SERVER_OUTPUT.txt"
	nonInteractiveInputReply = "This is a non-interactive session. Operator input is unavailable."
)

// PromptPath is the workdir-relative location of the rendered prompt the
// worker writes before invoking the runner.
const PromptPath = ".aiops/PROMPT.md"

func (CodexAppServerRunner) Run(ctx context.Context, in RunInput) (Result, error) {
	releaseGoBuildCache := markActiveGoBuildCache(in.Workdir)
	defer releaseGoBuildCache()
	if err := validateAppServerWorkdir(in.Workdir); err != nil {
		return Result{}, err
	}
	promptAbs := filepath.Join(in.Workdir, PromptPath)
	prompt, err := os.ReadFile(promptAbs)
	if err != nil {
		return Result{}, fmt.Errorf("read %s: %w", PromptPath, err)
	}

	cmd, directCodexExec, sandboxEnabled, err := setupAppServerCommand(ctx, in)
	if err != nil {
		return Result{}, err
	}
	stdin, stdout, stderr, err := openAppServerPipes(cmd)
	if err != nil {
		return Result{}, err
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
	client := &appServerClient{
		stdin:             stdin,
		scanner:           sc,
		out:               buf,
		codexAppServerPID: appServerProcessPID(cmd, directCodexExec, sandboxEnabled),
	}
	client.startStdoutReader()
	runErr := client.run(ctx, in, string(prompt))
	if runErr != nil && ctx.Err() == nil {
		terminateProcess(cmd)
	}
	_ = stdin.Close()
	waitErr := cmd.Wait()
	<-stderrDone
	// Join the stdout reader so no goroutine outlives this call (Go Code Review
	// Comments, "Goroutine Lifetimes"). cmd.Wait above has closed the process'
	// stdout, so a reader parked in scanner.Scan now observes EOF and exits;
	// close(readDone) releases one parked handing off a line; draining readCh to
	// its deferred close receives any in-flight line and blocks until the reader
	// has returned.
	close(client.readDone)
	for range client.readCh { //nolint:revive // drain-to-close joins the reader goroutine
	}
	elapsed := time.Since(start)

	writeAppServerArtifact(in.Workdir, buf)
	res := Result{
		Summary:        client.summary(),
		RuntimeEvents:  client.runtimeEvents,
		IssueExitState: client.issueExitState,
		OutputBytes:    int64(len(buf.Bytes())),
		OutputDropped:  buf.Dropped(),
	}
	head, tail := headTail(buf.Bytes())
	if len(head) > 0 {
		res.OutputHead = string(head)
	}
	res.OutputTail = tail
	res, outcomeErr := classifyAppServerOutcome(ctx, res, runErr, waitErr, client.readTimeoutMs, start, elapsed)
	// Re-tag a recurring codex/bwrap sandbox-startup denial so the worker parks
	// it on a cooldown instead of hot-retrying every poll (#550). Done here, not
	// inside classifyAppServerOutcome, because the captured output (OutputHead/
	// Tail, where bwrap's stderr lands) is only assembled above.
	return res, classifySandboxStartupFailure(outcomeErr, res)
}

// setupAppServerCommand builds the `codex app-server` *exec.Cmd ready to start:
// it resolves the agent env, constructs the command, pins its workdir/env,
// validates the command's workdir, applies the sandbox wrapper, and configures
// the platform kill + WaitDelay. directCodexExec reports whether the command
// launches codex directly (vs a shell wrapper) and sandboxEnabled whether the
// sandbox wraps it — both feed appServerProcessPID's PID-emission guard.
func setupAppServerCommand(ctx context.Context, in RunInput) (cmd *exec.Cmd, directCodexExec, sandboxEnabled bool, err error) {
	env := agentEnv(in.Workflow.Config.Codex.EnvPassthrough, in.Workflow.Config)
	// Pin the agent's Go toolchain caches to a sandbox-writable, per-workspace
	// path so its first `go test` does not fail on the default $HOME-based
	// caches that lie outside codex's workspace-write sandbox (#544).
	if err := reapSandboxGoBuildCaches(); err != nil {
		log.Printf("event=go_build_cache_reap_failed error=%q", err)
	}
	env = withSandboxGoToolchainCaches(env, in.Workdir)
	cmd, directCodexExec, err = buildCodexAppServerCmd(ctx, in, env)
	if err != nil {
		return nil, false, false, err
	}
	cmd.Dir = in.Workdir
	cmd.Env = env
	if err := validateAgentCommandWorkdir(in, cmd); err != nil {
		return nil, false, false, err
	}
	sandboxEnabled = in.Workflow.Config.Sandbox.Enabled && in.Workflow.Config.Sandbox.Backend != "" && in.Workflow.Config.Sandbox.Backend != "none"
	cmd, err = applySandbox(ctx, in, cmd)
	if err != nil {
		return nil, false, false, err
	}
	configurePlatformKill(cmd)
	cmd.WaitDelay = killGrace
	return cmd, directCodexExec, sandboxEnabled, nil
}

// openAppServerPipes wires the subprocess's stdio. Each pipe error carries the
// stream name so a setup failure is attributable; callers return the wrapped
// error verbatim. If a later pipe fails after an earlier one succeeded, the
// already-opened pipes are closed so their fds do not leak (the process is never
// started on this path, so cmd.Wait never runs to close them for us).
func openAppServerPipes(cmd *exec.Cmd) (stdin io.WriteCloser, stdout, stderr io.ReadCloser, err error) {
	stdin, err = cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open codex app-server stdin: %w", err)
	}
	defer closeOnError(&err, stdin)
	stdout, err = cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open codex app-server stdout: %w", err)
	}
	defer closeOnError(&err, stdout)
	stderr, err = cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open codex app-server stderr: %w", err)
	}
	return stdin, stdout, stderr, nil
}

// closeOnError closes c when *errp is non-nil at defer time. A setup function
// that opens several resources defers one of these per resource so a later
// failure releases the ones already opened, without each early return unwinding
// by hand. The closer value is captured when the defer is registered, so it
// survives the failing return that resets the named result to nil.
func closeOnError(errp *error, c io.Closer) {
	if *errp != nil {
		_ = c.Close()
	}
}

// appServerProcessPID returns cmd.Process.Pid only when it is guaranteed to be
// the actual codex process: the command must launch the codex binary directly
// (no shell wrapper from a custom codex.command) and the sandbox must not have
// wrapped cmd in firejail/bwrap. In any wrapper scenario the PID belongs to the
// wrapper, which would mislead operators trying to map `/api/v1/state` rows to a
// host process; returning 0 makes the omitempty JSON field absent. Call after
// cmd.Start() so cmd.Process is populated.
func appServerProcessPID(cmd *exec.Cmd, directCodexExec, sandboxEnabled bool) int {
	if directCodexExec && !sandboxEnabled && cmd.Process != nil {
		return cmd.Process.Pid
	}
	return 0
}

// classifyAppServerOutcome maps a completed app-server run to the (Result, error)
// the worker finalize path consumes. The precedence is load-bearing: the worker
// (internal/worker/runtask.go) switches on the error TYPE to pick the terminal
// phase (stall, read/run timeout, port exit, generic failure), so a run-loop
// error is classified StallError → read-timeout → outer-deadline TimeoutError →
// already-categorized error → process-exit PortExit → bare runErr, in that order.
// res is returned on every path so output telemetry survives the error.
func classifyAppServerOutcome(ctx context.Context, res Result, runErr, waitErr error, readTimeoutMs int, start time.Time, elapsed time.Duration) (Result, error) {
	if runErr == nil {
		return classifyAppServerProcessExit(ctx, res, waitErr, start, elapsed)
	}
	var stall *StallError
	if errors.As(runErr, &stall) {
		return res, runErr
	}
	if isAppServerReadTimeout(runErr) {
		return res, &ReadTimeoutError{Timeout: time.Duration(readTimeoutMs) * time.Millisecond, Cause: runErr}
	}
	// This outer-deadline check intentionally precedes the ErrorCategory check
	// below: when the run ctx deadline has genuinely fired, a coinciding
	// *TurnTimeoutError (categorized CategoryTurnTimeout) is reported as a
	// *TimeoutError carrying the run budget. The worker (internal/worker/
	// runtask.go) routes both to task.PhaseTimedOut, so this only changes the
	// reported budget/elapsed, not the terminal phase; reporting the run budget
	// is defensible once the run deadline has elapsed. Kept deliberately (#507
	// item 3) — flipping the order would be a behavior change with no phase-level
	// payoff. TestClassifyAppServerOutcome_TurnTimeoutUnderDeadlineIsTimeout pins
	// this; the worker-routing equivalence is covered by
	// TestRunRunnerWithTimeoutEmitsTerminalErrorPhases.
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

// classifyAppServerProcessExit handles the runErr==nil tail of
// classifyAppServerOutcome: a clean run loop whose subprocess still exited
// non-zero is an outer-deadline TimeoutError (if the run ctx deadline fired) or
// a PortExit, and an exitless clean run is success.
func classifyAppServerProcessExit(ctx context.Context, res Result, waitErr error, start time.Time, elapsed time.Duration) (Result, error) {
	if waitErr == nil {
		return res, nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return res, &TimeoutError{Timeout: deadlineBudget(ctx, start), Elapsed: elapsed, Cause: waitErr}
	}
	return res, NewError(CategoryPortExit, "codex app-server process exited", waitErr)
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
	stdin   io.Writer
	scanner *bufio.Scanner
	// readCh/readDone/readErr connect the single long-lived stdout reader
	// (startStdoutReader, which documents the lifecycle) to the request/response
	// consumer. readCh carries lines and is closed only by the reader; readDone is
	// the go.dev/blog/pipelines "done" channel closed at shutdown; readErr is the
	// sticky terminal scan error, published before close(readCh) so the close is
	// the happens-before edge that lets the consumer read it race-free.
	readCh              chan []byte
	readDone            chan struct{}
	readErr             error
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
	lastRuntimeEvent  string
	issueExitState    *IssueStateSnapshot
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
	turnLimit, cleanBudgetStop := effectiveTurnLimit(in)
	for turn := 1; turn <= turnLimit; turn++ {
		keepGoing, err := c.runSingleTurn(ctx, in, threadID, prompt, turn)
		if err != nil {
			return err
		}
		if !keepGoing {
			return nil
		}
	}
	if cleanBudgetStop {
		return nil
	}
	return fmt.Errorf("codex app-server exceeded agent.max_turns=%d", turnLimit)
}

func effectiveTurnLimit(in RunInput) (limit int, cleanBudgetStop bool) {
	maxTurns := in.Workflow.Config.Agent.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 20
	}
	if in.CleanTurnBudget > 0 && in.CleanTurnBudget <= maxTurns {
		return in.CleanTurnBudget, true
	}
	return maxTurns, false
}

// initSession records the run's sinks and caches the per-run codex config off
// RunInput before any protocol traffic.
func (c *appServerClient) initSession(in RunInput) {
	c.runtimeEventSink = in.RuntimeEventSink
	c.phaseTransitionSink = in.PhaseTransitionSink
	c.nextID = 1
	c.continueRun = false
	c.refreshIssueState = in.RefreshIssueState
	c.tools = DynamicToolsForWorkflow(
		workflow.Workflow{Config: in.Workflow.Config},
		WithCurrentIssueToolGuard(in.Task.ID, in.Task.SourceEventID, in.RefreshIssueState),
		WithCurrentIssueOperatorTerminalStopLookup(in.LookupOperatorTerminalStop),
	)
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
	if _, err := c.request(ctx, "initialize", buildInitializeParams()); err != nil {
		c.recordStartupFailed("initialize", err)
		return "", fmt.Errorf("codex app-server initialize: %w", err)
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		c.recordStartupFailed("initialized", err)
		return "", err
	}
	threadResult, err := c.request(ctx, "thread/start", buildThreadStartParams(in, c.approvalPolicy))
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
	c.recordTurnStarted(threadID, turnID, turn)
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
		snapshot, err := c.refreshIssueState(ctx)
		if err != nil {
			return false, fmt.Errorf("codex app-server refresh issue state: %w", err)
		}
		if !snapshot.Active {
			c.issueExitState = &snapshot
			return false, nil
		}
	}
	return c.continueRun, nil
}

// startTurn issues turn/start and resolves the turn id. A failure on the first
// turn is also recorded as a startup failure (the session never produced a
// turn). Mirrors upstream start_turn.
func (c *appServerClient) startTurn(ctx context.Context, in RunInput, threadID, prompt string, turn int) (string, error) {
	turnResult, err := c.request(ctx, "turn/start", buildTurnStartParams(in, threadID, prompt, turn, c.approvalPolicy))
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

// buildInitializeParams is the SPEC §10.1 initialize payload. It is extracted
// (rather than inlined at the call site) so the schema contract test validates
// the exact bytes startThread sends. capabilities.experimentalApi opts into the
// experimental protocol surface, which is what makes thread/start dynamicTools
// (also experimental) take effect.
func buildInitializeParams() map[string]any {
	return map[string]any{
		"capabilities": map[string]any{"experimentalApi": true},
		"clientInfo": map[string]any{
			"name":    "aiops-platform",
			"title":   "AIOps Platform",
			"version": "0.1.0",
		},
	}
}

// buildThreadStartParams is the thread/start payload. dynamicTools is an
// experimental field gated by the experimentalApi capability set in
// buildInitializeParams; it advertises the SPEC §10.5 client-side tool surface
// (e.g. linear_graphql) to the agent.
func buildThreadStartParams(in RunInput, approvalPolicy any) map[string]any {
	return map[string]any{
		"approvalPolicy": approvalPolicy,
		"config":         appServerThreadConfig(),
		"sandbox":        in.Workflow.Config.Codex.ThreadSandbox,
		"cwd":            in.Workdir,
		"dynamicTools":   appServerDynamicToolSpecs(in.Workflow.Config),
	}
}

func appServerThreadConfig() map[string]any {
	return map[string]any{
		"apps": map[string]any{
			"_default": map[string]any{
				// Unattended worker sessions own PR/tracker handoff through
				// WORKFLOW-prescribed CLI commands plus explicit dynamicTools.
				"enabled":             false,
				"open_world_enabled":  false,
				"destructive_enabled": false,
			},
		},
		"features": map[string]any{
			// The app default does not override host config that explicitly
			// enables a connector, so disable the app feature for the session.
			// Codex also accepts `connectors` as the legacy alias for Apps.
			"apps":       false,
			"connectors": false,
		},
	}
}

// buildTurnStartParams is the turn/start payload, extracted so the schema
// contract test validates the exact bytes startTurn sends.
func buildTurnStartParams(in RunInput, threadID, prompt string, turn int, approvalPolicy any) codexAppServerTurnStartParams {
	return codexAppServerTurnStartParams{
		ThreadID:       threadID,
		Input:          turnInput(in, prompt, turn),
		CWD:            in.Workdir,
		ApprovalPolicy: approvalPolicy,
		SandboxPolicy:  in.Workflow.Config.Codex.TurnSandboxPolicy,
	}
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

func (c *appServerClient) recordTurnStarted(threadID, turnID string, turn int) {
	c.recordRuntimeEvent(task.EventTurnStarted, map[string]any{
		"session_id":  threadID + "-" + turnID,
		"thread_id":   threadID,
		"turn_id":     turnID,
		"turn_number": turn,
	})
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
		if result, matched, rpcErr := responseForID(msg, id); matched {
			return result, rpcErr
		}
		c.handleNotification(msg)
	}
}

// responseForID reports whether msg is the JSON-RPC response to request id. When
// it is (matched=true), it returns the result map — defaulting a missing or null
// result to an empty map — or a CategoryResponseError carrying the `error`
// member. A non-matching msg (matched=false) is an interleaved notification the
// caller must dispatch and keep reading past.
func responseForID(msg map[string]any, id int) (result map[string]any, matched bool, err error) {
	gotID, ok := numberID(msg["id"])
	if !ok || gotID != id {
		return nil, false, nil
	}
	if e, ok := msg["error"]; ok {
		return nil, true, NewError(CategoryResponseError, fmt.Sprintf("rpc error: %v", e), nil)
	}
	result, _ = msg["result"].(map[string]any)
	if result == nil {
		result = map[string]any{}
	}
	return result, true, nil
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

// startStdoutReader launches the single long-lived stdout reader. It is the
// source stage of https://go.dev/blog/pipelines: one goroutine owns the
// bufio.Scanner (which is not safe for concurrent use) and continuously scans
// lines, handing each to the request/response consumer over readCh. Its lifetime
// is the app-server process: it exits on exactly one of
//   - scanner.Scan returning false (stdout EOF or error — the process closed
//     stdout, or cmd.Wait closed the pipe), recording a sticky readErr; or
//   - readDone being closed at shutdown while it is parked handing off a line,
//     which the blog prescribes so a sender whose consumer has moved on cannot
//     "block indefinitely ... a resource leak".
//
// `defer close(readCh)` runs on every exit path, so a consumer waiting on readCh
// is always released and the close is the happens-before edge that publishes
// readErr (Go Code Review Comments, "Goroutine Lifetimes"). RunCodexAppServer
// joins it by draining readCh after cmd.Wait. The channels are created here so
// the goroutine and its communication state have a single owner.
func (c *appServerClient) startStdoutReader() {
	c.readCh = make(chan []byte)
	c.readDone = make(chan struct{})
	go func() {
		defer close(c.readCh)
		defer c.recoverReaderPanic()
		for c.scanner.Scan() {
			// Scanner.Bytes() is invalidated by the next Scan (bufio docs); copy
			// before handing the line across the goroutine boundary.
			line := append([]byte(nil), c.scanner.Bytes()...)
			select {
			case c.readCh <- line:
			case <-c.readDone:
				return
			}
		}
		c.readErr = scanTerminalError(c.scanner)
	}()
}

// recoverReaderPanic is the stdout reader's deferred recovery: a panic in the
// scan loop is published as readErr (so the consumer sees an error rather than
// hanging on a never-closed readCh) and logged, not swallowed.
func (c *appServerClient) recoverReaderPanic() {
	if r := recover(); r != nil {
		c.readErr = fmt.Errorf("codex app-server stdout reader panic: %v", r)
		log.Printf("event=codex_app_server_reader_panic error=%q", r)
	}
}

// scanTerminalError maps a finished scanner to the terminal error the consumer
// should observe once readCh closes. Scanner.Err is nil on io.EOF (bufio docs),
// so a clean end-of-stream normalizes to io.EOF; an over-cap line keeps the
// existing wrapped bufio.ErrTooLong contract.
func scanTerminalError(sc *bufio.Scanner) error {
	err := sc.Err()
	if err == nil {
		return io.EOF
	}
	if errors.Is(err, bufio.ErrTooLong) {
		return fmt.Errorf("codex app-server line exceeded %d bytes: %w", maxAppServerLineBytes, err)
	}
	return err
}

func (c *appServerClient) readLine(ctx context.Context) ([]byte, error) {
	readTimeout := time.Duration(c.readTimeoutMs) * time.Millisecond
	deadlineTimeout, hasDeadline := deadlineDuration(ctx)
	if c.readTimeoutMs <= 0 || (hasDeadline && deadlineTimeout < readTimeout) {
		return c.readLineOnce(ctx, nil)
	}
	return c.readLineOnce(ctx, time.After(readTimeout))
}

// readLineOnce waits for the next line from the long-lived reader, the
// per-read timeout, or context cancellation — whichever fires first. A closed
// readCh means the reader has exited; it surfaces the reader's sticky readErr
// (io.EOF when it stopped cleanly). The DeadlineExceeded preference is retained
// so a read that loses the race to an expiring context is classified as the
// deadline, not as the EOF the dying process subsequently produced.
func (c *appServerClient) readLineOnce(ctx context.Context, timeout <-chan time.Time) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timeout:
		return nil, &appServerReadTimeoutError{afterMs: c.readTimeoutMs}
	case line, ok := <-c.readCh:
		if !ok {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ctx.Err()
			}
			if c.readErr != nil {
				return nil, c.readErr
			}
			return nil, io.EOF
		}
		return line, nil
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

// appServerReadTimeoutError signals that a single stdio read exceeded the
// configured codex read_timeout_ms budget. It is a typed error so the
// classification paths detect it via errors.As rather than substring-matching
// the message (AGENTS.md clean-code rule 8). The message is unchanged from the
// original fmt.Errorf so existing logs and string-asserting tests still hold.
type appServerReadTimeoutError struct {
	afterMs int
}

func (e *appServerReadTimeoutError) Error() string {
	return fmt.Sprintf("codex app-server read timeout after %dms", e.afterMs)
}

// isAppServerReadTimeout reports whether err is (or wraps) the read-timeout
// signal. Both classifyAppServerOutcome and classifyTurnReadError consume it, so
// the typed check lives here once rather than being inlined at each call site.
func isAppServerReadTimeout(err error) bool {
	var rt *appServerReadTimeoutError
	return errors.As(err, &rt)
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

// appServerTurnTitle builds the SPEC §10.2 human label
// "<issue.identifier>: <issue.title>" used as the continuation-prompt subject
// (appServerContinuationPrompt). It is not a wire field: codex 0.137
// TurnStartParams has no title property even under the experimental schema, so
// a title sent on turn/start was silently dropped — the label survives only in
// the prompt the agent reads. Task.SourceEventID is the tracker identifier
// (e.g. "AIOPS-64", "MT-649"); Task.ID is an internal queue nonce that means
// nothing to operators. Prefer the identifier; fall back to title alone if the
// identifier is unset; fall back to the task nonce only as a last resort so
// prompt-only tests still get something to dispatch.
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
