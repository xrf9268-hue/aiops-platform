package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
	"github.com/xrf9268-hue/aiops-platform/internal/workspace"
)

// EventEmitter is the subset of the queue store the worker needs to record
// per-stage events. Defined as an interface so unit tests can verify the
// worker emits the right kinds without standing up a database. The payload
// parameter is `any` so callers can pass either structured maps (Marshal'd
// by the store) or pre-serialized JSON []byte.
type EventEmitter interface {
	AddEvent(ctx context.Context, taskID, typ, msg string) error
	AddEventWithPayload(ctx context.Context, taskID, typ, msg string, payload any) error
}

// RunTaskError bundles the resolved workflow Config alongside the error so
// callers can classify failures without re-resolving the workflow.
type RunTaskError struct {
	Cfg workflow.Config
	Err error
}

// RunTaskResult carries success-side runner facts needed by the orchestrator.
type RunTaskResult struct {
	// IssueLeftActiveSet is true when SPEC §16.5's per-turn tracker refresh
	// stopped a successful runner session because the issue is no longer active.
	IssueLeftActiveSet bool
}

// ResolveWorkflow emits the workflow_resolved event for the service-level
// WORKFLOW.md that was loaded at process startup. Returning the workflow_source
// string lets callers stamp it onto the runner_start payload as a quick-look
// field; the full provenance lives on the workflow_resolved event itself.
// issueRenderVarsForTask returns the SPEC §4.1.1 normalized issue snapshot
// for the prompt template's `issue` variable. The orchestrator's
// TaskFromIssue precomputes IssueRender at dispatch; this helper falls back
// to the minimal Task-derived map for callers that build tasks by hand
// (e.g. worker tests) so they still render, just without the §4.1.1 fields
// the helper cannot reconstruct.
func issueRenderVarsForTask(t task.Task) map[string]any {
	if t.IssueRender != nil {
		out := make(map[string]any, len(t.IssueRender)+1)
		for k, v := range t.IssueRender {
			out[k] = v
		}
		out["actor"] = t.Actor
		return out
	}
	return map[string]any{
		"identifier":  t.SourceEventID,
		"title":       t.Title,
		"description": t.Description,
		"actor":       t.Actor,
	}
}

// ResolveWorkflow returns the resolved workflow for a task, emitting the
// canonical workflow_resolved event + log line. identifier is the tracker
// issue identifier (Task.SourceEventID); pass "" if unknown — the log line
// then omits issue_identifier= rather than emitting an empty value.
func ResolveWorkflow(ctx context.Context, ev EventEmitter, taskID, identifier string, wf *workflow.Workflow) (*workflow.Workflow, string, error) {
	if wf == nil {
		return nil, "", fmt.Errorf("service workflow is required")
	}
	res := &workflow.Resolution{Source: wf.Source, Path: wf.Path}
	payload := map[string]any{
		"source":        string(res.Source),
		"agent_default": wf.Config.Agent.Default,
		"policy_mode":   wf.Config.Policy.Mode,
		"tracker_kind":  wf.Config.Tracker.Kind,
	}
	if res.Path != "" {
		payload["path"] = res.Path
	}
	Emit(ctx, ev, taskID, identifier, task.EventWorkflowResolved, "workflow resolved", payload)
	logWorkflowResolved(taskID, identifier, res)
	return wf, string(res.Source), nil
}

// logWorkflowResolved prints the structured `event=workflow_resolved` line
// summarizing how the workflow was discovered. The path segment is omitted
// when empty so the common case (source=default) stays short; identifier is
// passed through to LogTaskIDEventf and omitted when "".
func logWorkflowResolved(taskID, identifier string, res *workflow.Resolution) {
	parts := []string{"source=" + string(res.Source)}
	if res.Path != "" {
		parts = append(parts, "path="+res.Path)
	}
	if len(res.ShadowedBy) > 0 {
		parts = append(parts, "shadowed=["+strings.Join(res.ShadowedBy, ",")+"]")
	}
	LogTaskIDEventf(taskID, identifier, "workflow_resolved", "%s", strings.Join(parts, " "))
}

func emitHookResults(ctx context.Context, ev EventEmitter, taskID, identifier string, results []workspace.HookResult) {
	for _, res := range results {
		Emit(ctx, ev, taskID, identifier, task.EventWorkspaceHookEnd, string(res.Name)+" hook completed", map[string]any{
			"hook":        string(res.Name),
			"command":     res.Command,
			"exit_code":   res.ExitCode,
			"output":      res.Output,
			"truncated":   res.Truncated,
			"duration_ms": res.Duration.Milliseconds(),
			"error":       ErrSummary(res.Err),
		})
	}
}

func runWorkspaceHook(ctx context.Context, ev EventEmitter, taskID, identifier, workdir string, name workspace.HookName, hook workflow.WorkspaceHook, timeoutMs int, envPassthrough []string) error {
	if len(hook.Commands) == 0 {
		return nil
	}
	Emit(ctx, ev, taskID, identifier, task.EventWorkspaceHookStart, string(name)+" hook started", map[string]any{
		"hook":       string(name),
		"commands":   len(hook.Commands),
		"timeout_ms": timeoutMs,
	})
	results, err := workspace.RunWorkspaceHook(ctx, workdir, name, hook, timeoutMs, envPassthrough)
	emitHookResults(ctx, ev, taskID, identifier, results)
	return err
}

func removeWorkdirAfterHookFailure(ctx context.Context, ev EventEmitter, taskID, identifier, workspaceRoot, workdir string, beforeRemove workflow.WorkspaceHook, timeoutMs int, envPassthrough []string, reason string) {
	if err := runWorkspaceHook(ctx, ev, taskID, identifier, workdir, workspace.HookBeforeRemove, beforeRemove, timeoutMs, envPassthrough); err != nil {
		LogTaskIDEventf(taskID, identifier, "before_remove_hook_failed", "reason=%s error=%q", reason, err)
	}
	if err := workspace.SafeRemove(workspaceRoot, workdir); err != nil {
		LogTaskIDEventf(taskID, identifier, "workspace_remove_failed", "reason=%s workdir=%q error=%q", reason, workdir, err)
	}
	if err := runner.RemoveSandboxGoBuildCache(workdir); err != nil {
		LogTaskIDEventf(taskID, identifier, "go_build_cache_remove_failed", "reason=%s workdir=%q error=%q", reason, workdir, err)
	}
}

// runState threads one task's lifecycle state across RunTask's phase helpers
// so each phase stays a single-responsibility function under the funlen
// budget. A fresh value is built per RunTask call and never shared across
// tasks, so its fields need no synchronization.
type runState struct {
	ctx context.Context
	ev  EventEmitter
	t   task.Task
	cfg Config

	wf             *workflow.Workflow
	wcfg           workflow.Config
	workflowSource string
	hooks          workflow.WorkspaceHooks
	workspaceRoot  string
	workdir        string
	prompt         string
	res            runner.Result
	sessionID      string

	currentPhase  task.RunAttemptPhase
	phaseTerminal bool
}

// emitPhase emits a run-attempt phase transition and records the new phase so
// RunTask's deferred guard knows whether a terminal phase was already reached.
func (rs *runState) emitPhase(from, to task.RunAttemptPhase) {
	EmitPhaseTransition(rs.ctx, rs.ev, rs.t.ID, rs.t.SourceEventID, from, to)
	rs.currentPhase = to
	rs.phaseTerminal = isTerminalPhase(to)
}

// RunTask executes a single in-memory task. The orchestrator-backed worker path
// uses this directly after claiming a tracker issue in runtime state.
//
// Per SPEC §1, push, PR creation, and tracker state writes are the agent's
// responsibility. The worker's role is: claim, prepare workspace, resolve
// workflow, run the agent session, emit events, and clean up. The lifecycle is
// split across runState phase
// helpers; RunTask only sequences them and stamps PhaseFailed on the way out
// of any non-terminal error path.
func RunTask(ctx context.Context, ev EventEmitter, t task.Task, cfg Config) *RunTaskError {
	_, rtErr := RunTaskWithResult(ctx, ev, t, cfg)
	return rtErr
}

// RunTaskWithResult is RunTask plus success-side metadata needed by the
// orchestrator. Keep ordinary callers on RunTask so a new metadata field does not
// force unrelated call sites to change.
func RunTaskWithResult(ctx context.Context, ev EventEmitter, t task.Task, cfg Config) (result RunTaskResult, ret *RunTaskError) {
	rs := &runState{ctx: ctx, ev: ev, t: t, cfg: cfg}
	defer func() {
		if ret != nil && rs.currentPhase != "" && !rs.phaseTerminal {
			rs.emitPhase(rs.currentPhase, task.PhaseFailed)
		}
	}()

	if rtErr := rs.prepareWorkspace(); rtErr != nil {
		return result, rtErr
	}
	if rtErr := rs.buildPrompt(); rtErr != nil {
		return result, rtErr
	}
	if rtErr := rs.runAgent(); rtErr != nil {
		return result, rtErr
	}
	rs.finalize()
	return RunTaskResult{IssueLeftActiveSet: rs.res.IssueLeftActiveSet}, nil
}

// prepareWorkspace resolves the service workflow, prepares the deterministic
// git workspace, runs the after_create hook on first creation, and defaults
// the task model. It corresponds to the PreparingWorkspace phase.
func (rs *runState) prepareWorkspace() *RunTaskError {
	rs.emitPhase("", task.PhasePreparingWorkspace)
	wf, workflowSource, err := ResolveWorkflow(rs.ctx, rs.ev, rs.t.ID, rs.t.SourceEventID, rs.cfg.Workflow)
	if err != nil {
		return &RunTaskError{Err: err}
	}
	rs.wf = wf
	rs.workflowSource = workflowSource
	rs.wcfg = wf.Config
	rs.hooks = rs.wcfg.WorkspaceHooks()

	rs.workspaceRoot = EffectiveWorkspaceRoot(rs.cfg, rs.wcfg)
	mgr := workspace.New(rs.workspaceRoot)
	mgr.MirrorRoot = rs.cfg.MirrorRoot
	workdir, createdNow, err := mgr.PrepareGitWorkspace(rs.ctx, rs.t)
	if err != nil {
		return &RunTaskError{Cfg: rs.wcfg, Err: err}
	}
	rs.workdir = workdir
	if createdNow {
		// SPEC §9.4: after_create runs only when a workspace directory
		// is newly created. Reuses skip it so bootstrap commands
		// (`npm ci`, `pip install`, …) remain the one-time init they're
		// documented as.
		if err := runWorkspaceHook(rs.ctx, rs.ev, rs.t.ID, rs.t.SourceEventID, workdir, workspace.HookAfterCreate, rs.hooks.AfterCreate, rs.hooks.TimeoutMs, rs.hooks.EnvPassthrough); err != nil {
			removeWorkdirAfterHookFailure(rs.ctx, rs.ev, rs.t.ID, rs.t.SourceEventID, rs.workspaceRoot, workdir, rs.hooks.BeforeRemove, rs.hooks.TimeoutMs, rs.hooks.EnvPassthrough, "after_create")
			return &RunTaskError{Cfg: rs.wcfg, Err: err}
		}
	}

	rs.applyDefaultModel()
	return nil
}

// applyDefaultModel resolves the task model from the workflow default when the
// task did not pin one (or pinned the mock sentinel), falling back to "mock".
func (rs *runState) applyDefaultModel() {
	if rs.t.Model == "" || rs.t.Model == "mock" {
		rs.t.Model = rs.wcfg.Agent.Default
		if rs.t.Model == "" {
			rs.t.Model = "mock"
		}
	}
}

// buildPrompt assembles the prompt template variables, renders the prompt,
// appends the standing directives, and writes the task files. It transitions
// PreparingWorkspace → BuildingPrompt.
func (rs *runState) buildPrompt() *RunTaskError {
	t := rs.t
	renderVars := map[string]any{
		"task": map[string]any{
			"id":          t.ID,
			"title":       t.Title,
			"description": t.Description,
			"actor":       t.Actor,
		},
		"issue": issueRenderVarsForTask(t),
		"repo": map[string]any{
			"owner":  t.RepoOwner,
			"name":   t.RepoName,
			"branch": t.BaseBranch,
		},
		"attempt": nil,
	}
	if t.Attempts > 0 {
		renderVars["attempt"] = t.Attempts
	}
	rs.emitPhase(task.PhasePreparingWorkspace, task.PhaseBuildingPrompt)
	prompt, err := workflow.Render(rs.wf.PromptTemplate, renderVars)
	if err != nil {
		return &RunTaskError{Cfg: rs.wcfg, Err: err}
	}
	prompt = AppendAnalysisOnlyDirective(prompt, rs.wcfg.Policy.Mode)
	// Skip the verify directive in analysis-only mode: that mode forbids source
	// edits / PR handoff, so "run the verification commands before handing off"
	// has no code change to verify and contradicts the analysis-only directive.
	if rs.wcfg.Policy.Mode != "analysis_only" {
		prompt = AppendVerifyDirective(prompt, rs.wcfg.Verify.Commands)
	}
	if err := writeTaskFiles(rs.workdir, t, prompt); err != nil {
		return &RunTaskError{Cfg: rs.wcfg, Err: err}
	}
	rs.prompt = prompt
	return nil
}

// newRunner constructs the runner for a model. It is a package var so tests can
// inject a runner that returns context.Canceled to exercise the reconcile-cancel
// artifact-skip path (#543) without standing up a real agent subprocess.
var newRunner = runner.New

// runAgent resets stale run artifacts left over from a previous run, runs the
// before_run hook, invokes the runner under its timeout, and runs the after_run
// hook on both the success and failure paths.
func (rs *runState) runAgent() *RunTaskError {
	r, err := newRunner(rs.t.Model)
	if err != nil {
		return &RunTaskError{Cfg: rs.wcfg, Err: err}
	}

	if rtErr := rs.resetStaleArtifacts(); rtErr != nil {
		return rtErr
	}

	if err := runWorkspaceHook(rs.ctx, rs.ev, rs.t.ID, rs.t.SourceEventID, rs.workdir, workspace.HookBeforeRun, rs.hooks.BeforeRun, rs.hooks.TimeoutMs, rs.hooks.EnvPassthrough); err != nil {
		return &RunTaskError{Cfg: rs.wcfg, Err: err}
	}

	var refreshIssueState runner.IssueStateRefresher
	if rs.cfg.IssueStateRefresher != nil {
		refreshIssueState = rs.cfg.IssueStateRefresher(rs.t, rs.wcfg)
	}
	res, runErr := RunRunnerWithTimeout(rs.ctx, rs.ev, r, runner.RunInput{Task: rs.t, Workflow: *rs.wf, Workdir: rs.workdir, WorkspaceRoot: rs.workspaceRoot, Prompt: rs.prompt, CleanTurnBudget: rs.cfg.CleanTurnBudget, RefreshIssueState: refreshIssueState, PhaseTransitionSink: rs.emitPhase}, rs.wcfg.Agent.Timeout, rs.workflowSource)
	rs.res = res
	rs.sessionID = sessionIDFromRuntimeEvents(res.RuntimeEvents)
	if runErr != nil {
		return rs.handleRunnerFailure(runErr)
	}

	if err := runWorkspaceHook(rs.ctx, rs.ev, rs.t.ID, rs.t.SourceEventID, rs.workdir, workspace.HookAfterRun, rs.hooks.AfterRun, rs.hooks.TimeoutMs, rs.hooks.EnvPassthrough); err != nil {
		LogIssueSessionEventf(rs.t, rs.sessionID, "after_run_hook_failed", "error=%q", err)
	}
	return nil
}

// handleRunnerFailure runs the after_run hook on the runner-error path and
// returns the terminal RunTaskError. The failure reason is already carried by
// the SPEC §13.1/§13.2 structured events the runner path emits (runner_end
// "runner failed" with an `error` payload, or the workspace_hook_end error for a
// before_run failure), so there is nothing more to persist here. A codex
// sandbox-startup denial rides this same generic-failure path: the runner
// already re-tagged it as an output-free *SandboxStartupError (no raw bwrap text
// leaks) and it takes the SPEC §8.4 backoff like any other failure.
// `worker --doctor` (#542) is the preventive layer that catches the host
// misconfiguration before dispatch.
func (rs *runState) handleRunnerFailure(runErr error) *RunTaskError {
	if err := runWorkspaceHook(rs.ctx, rs.ev, rs.t.ID, rs.t.SourceEventID, rs.workdir, workspace.HookAfterRun, rs.hooks.AfterRun, rs.hooks.TimeoutMs, rs.hooks.EnvPassthrough); err != nil {
		LogIssueSessionEventf(rs.t, rs.sessionID, "after_run_hook_failed", "after_runner_error=true error=%q", err)
	}
	return &RunTaskError{Cfg: rs.wcfg, Err: runErr}
}

// resetStaleArtifacts deletes untracked worker/agent artifacts left over from a
// previous run (and, in analysis-only mode, the agent's .aiops/PLAN.md handoff
// artifact) before the runner starts so leftovers are not mistaken for this
// run's output. PrepareGitWorkspace resets tracked files to origin/<base> on
// every prepare (fresh checkout on first touch, `checkout --force -B` on reuse
// per SPEC §9.1), but those artifacts may also be committed on the base branch
// itself (left over from a prior PR or seeded by hand), and on reuse any
// untracked artifact written by a previous run still lingers in the workdir.
func (rs *runState) resetStaleArtifacts() *RunTaskError {
	// Clear artifacts retired by earlier worker versions: FAILURE.md and
	// CHANGED_FILES.txt (the failure post-mortem / changed-file snapshot removed
	// in #575 — failure reasons live in the SPEC §13.1/§13.2 structured events),
	// RUN_SUMMARY.md (#561), VERIFICATION.txt (#560), and BLOCKED.json (the
	// external-blocker cooldown artifact removed in #572). They are no longer
	// written or read, but on a long-lived workspace reused across a worker
	// upgrade an old untracked copy can linger (PrepareGitWorkspace preserves
	// untracked files) and be committed by an agent that runs `git add -A`.
	for _, retired := range []string{"FAILURE.md", "CHANGED_FILES.txt", "RUN_SUMMARY.md", "VERIFICATION.txt", "BLOCKED.json"} {
		if err := os.Remove(filepath.Join(rs.workdir, ".aiops", retired)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return &RunTaskError{Cfg: rs.wcfg, Err: fmt.Errorf("reset retired artifact %s: %w", retired, err)}
		}
	}
	// In analysis-only mode .aiops/PLAN.md is the agent's handoff artifact (it
	// writes its assessment there per AppendAnalysisOnlyDirective). On a reused
	// workspace a stale PLAN.md from a previous attempt survives
	// PrepareGitWorkspace (it preserves untracked files), so clear it before the
	// run — otherwise a prior assessment can be read as this run's output. The
	// removed post-turn analysis-only gate previously also relied on this reset;
	// the hygiene reason stands on its own (Codex review on #574).
	if rs.wcfg.Policy.Mode == "analysis_only" {
		if err := os.Remove(filepath.Join(rs.workdir, ".aiops", "PLAN.md")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return &RunTaskError{Cfg: rs.wcfg, Err: fmt.Errorf("reset analysis plan: %w", err)}
		}
	}
	return nil
}

// finalize stamps the terminal success phase. It does not push; that is the
// agent's job.
func (rs *runState) finalize() {
	rs.emitPhase(task.PhaseFinishing, task.PhaseSucceeded)
}

func isTerminalPhase(phase task.RunAttemptPhase) bool {
	switch phase {
	case task.PhaseSucceeded, task.PhaseFailed, task.PhaseTimedOut, task.PhaseStalled, task.PhaseCanceledByReconciliation:
		return true
	default:
		return false
	}
}

// RunRunnerWithTimeout invokes the runner under a per-task timeout derived
// from agent.timeout. It emits structured task events
// (runner_start, runner_end, runner_timeout) so retry policy and observers
// can distinguish a clean exit from a kill due to deadline. The returned
// runner.Result carries any output telemetry the runner captured; on failure
// (timeout or non-zero exit) the same Result is returned so that partial
// telemetry (OutputBytes, OutputHead, OutputTail, etc.) is still available to
// the caller.
func RunRunnerWithTimeout(ctx context.Context, ev EventEmitter, r runner.Runner, in runner.RunInput, timeout time.Duration, workflowSource string) (runner.Result, error) { //nolint:gocognit,funlen // baseline (#521)
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}

	Emit(ctx, ev, in.Task.ID, in.Task.SourceEventID, task.EventRunnerStart, "runner started", map[string]any{
		"model":           in.Task.Model,
		"timeout_ms":      timeout.Milliseconds(),
		"workflow_source": workflowSource,
	})
	currentPhase := task.PhaseBuildingPrompt
	upstreamPhaseSink := in.PhaseTransitionSink
	in.PhaseTransitionSink = func(from, to task.RunAttemptPhase) {
		if from == "" {
			from = currentPhase
		}
		currentPhase = to
		if upstreamPhaseSink != nil {
			upstreamPhaseSink(from, to)
			return
		}
		EmitPhaseTransition(ctx, ev, in.Task.ID, in.Task.SourceEventID, from, to)
	}
	in.PhaseTransitionSink(task.PhaseBuildingPrompt, task.PhaseLaunchingAgentProcess)
	emittedRuntimeEvents := map[string]bool{}
	upstreamRuntimeSink := in.RuntimeEventSink
	in.RuntimeEventSink = func(event task.RuntimeEvent) {
		emittedRuntimeEvents[runtimeEventKey(event)] = true
		if upstreamRuntimeSink != nil {
			upstreamRuntimeSink(event)
			return
		}
		EmitRuntimeEvents(ctx, ev, in.Task.ID, in.Task.SourceEventID, []task.RuntimeEvent{event})
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	res, runErr := r.Run(runCtx, in)
	elapsed := time.Since(start)

	for _, event := range res.RuntimeEvents {
		if emittedRuntimeEvents[runtimeEventKey(event)] {
			continue
		}
		EmitRuntimeEvents(ctx, ev, in.Task.ID, in.Task.SourceEventID, []task.RuntimeEvent{event})
	}

	if runErr != nil {
		var stall *runner.StallError
		if errors.As(runErr, &stall) {
			in.PhaseTransitionSink(currentPhase, task.PhaseStalled)
			stallPayload := map[string]any{
				"model":      in.Task.Model,
				"timeout_ms": stall.Timeout.Milliseconds(),
				"elapsed_ms": stall.Elapsed.Milliseconds(),
			}
			addOutputFields(stallPayload, res)
			Emit(ctx, ev, in.Task.ID, in.Task.SourceEventID, task.EventStalled, stall.Error(), stallPayload)
			Emit(ctx, ev, in.Task.ID, in.Task.SourceEventID, task.EventRunnerTimeout, stall.Error(), stallPayload)
			return res, runErr
		}
		var turnTimeout *runner.TurnTimeoutError
		if errors.As(runErr, &turnTimeout) {
			in.PhaseTransitionSink(currentPhase, task.PhaseTimedOut)
			timeoutPayload := map[string]any{
				"model":      in.Task.Model,
				"timeout_ms": turnTimeout.Timeout.Milliseconds(),
				"elapsed_ms": turnTimeout.Elapsed.Milliseconds(),
			}
			addOutputFields(timeoutPayload, res)
			Emit(ctx, ev, in.Task.ID, in.Task.SourceEventID, task.EventRunnerTimeout, turnTimeout.Error(), timeoutPayload)
			return res, runErr
		}
		var readTimeout *runner.ReadTimeoutError
		if errors.As(runErr, &readTimeout) {
			in.PhaseTransitionSink(currentPhase, task.PhaseTimedOut)
			timeoutPayload := map[string]any{
				"model":      in.Task.Model,
				"timeout_ms": readTimeout.Timeout.Milliseconds(),
				"elapsed_ms": elapsed.Milliseconds(),
			}
			addOutputFields(timeoutPayload, res)
			Emit(ctx, ev, in.Task.ID, in.Task.SourceEventID, task.EventRunnerTimeout, readTimeout.Error(), timeoutPayload)
			return res, runErr
		}
		var te *runner.TimeoutError
		if errors.As(runErr, &te) || (errors.Is(runErr, context.DeadlineExceeded) && errors.Is(runCtx.Err(), context.DeadlineExceeded)) {
			in.PhaseTransitionSink(currentPhase, task.PhaseTimedOut)
			if te == nil {
				te = &runner.TimeoutError{Timeout: timeout, Elapsed: elapsed, Cause: runErr}
				runErr = te
			}
			timeoutPayload := map[string]any{
				"model":      in.Task.Model,
				"timeout_ms": te.Timeout.Milliseconds(),
				"elapsed_ms": te.Elapsed.Milliseconds(),
			}
			addOutputFields(timeoutPayload, res)
			Emit(ctx, ev, in.Task.ID, in.Task.SourceEventID, task.EventRunnerTimeout, te.Error(), timeoutPayload)
			return res, runErr
		}
		if isReconcileCancel(ctx, runErr) {
			// The orchestrator stopped this run because its tracker issue left
			// the active set (e.g. the agent's own PR handoff to In Review).
			// That is a supervised stop, not a runner failure: record it as
			// stopped and do not count it as a failure for the superseded run
			// (#543).
			in.PhaseTransitionSink(currentPhase, task.PhaseCanceledByReconciliation)
			stoppedPayload := map[string]any{
				"model":       in.Task.Model,
				"duration_ms": elapsed.Milliseconds(),
				"ok":          true,
				"reason":      "reconcile_ineligible",
			}
			addOutputFields(stoppedPayload, res)
			Emit(ctx, ev, in.Task.ID, in.Task.SourceEventID, task.EventRunnerStopped, "runner stopped: reconcile ineligible", stoppedPayload)
			return res, runErr
		}
		in.PhaseTransitionSink(currentPhase, task.PhaseFailed)
		failurePayload := map[string]any{
			"model":       in.Task.Model,
			"duration_ms": elapsed.Milliseconds(),
			"error":       ErrSummary(runErr),
			"ok":          false,
		}
		addOutputFields(failurePayload, res)
		Emit(ctx, ev, in.Task.ID, in.Task.SourceEventID, task.EventRunnerEnd, "runner failed", failurePayload)
		return res, runErr
	}

	in.PhaseTransitionSink(currentPhase, task.PhaseFinishing)
	endPayload := map[string]any{
		"model":       in.Task.Model,
		"duration_ms": elapsed.Milliseconds(),
		"ok":          true,
	}
	if res.Summary != "" {
		endPayload["summary"] = res.Summary
	}
	addOutputFields(endPayload, res)
	Emit(ctx, ev, in.Task.ID, in.Task.SourceEventID, task.EventRunnerEnd, "runner completed", endPayload)
	return res, nil
}

// EmitPhaseTransition records SPEC §7.2 run-attempt phase transitions with the
// canonical phase names in a structured payload.
func EmitPhaseTransition(ctx context.Context, ev EventEmitter, taskID, identifier string, from, to task.RunAttemptPhase) {
	if to == "" {
		return
	}
	payload := task.PhaseTransitionEvent(from, to)
	Emit(ctx, ev, taskID, identifier, task.EventRunPhaseTransition, string(to), payload)
}

// EmitRuntimeEvents forwards SPEC §10.4 app-server runtime events captured by
// the runner into the task event stream. The runtime event name is already the
// task event kind; payload is preserved verbatim so downstream conformance
// checks can inspect the app-server details without parsing runner output.
func EmitRuntimeEvents(ctx context.Context, ev EventEmitter, taskID, identifier string, events []task.RuntimeEvent) {
	for _, event := range events {
		if event.Event == "" {
			continue
		}
		Emit(ctx, ev, taskID, identifier, event.Event, event.Event, event.Payload)
	}
}

func runtimeEventKey(event task.RuntimeEvent) string {
	encoded, err := json.Marshal(event.Payload)
	if err != nil {
		encoded = []byte(fmt.Sprintf("%#v", event.Payload))
	}
	return event.Event + "\x00" + string(encoded)
}

func writeTaskFiles(workdir string, t task.Task, prompt string) error {
	if err := workspace.WritePrompt(workdir, prompt); err != nil {
		return err
	}
	return workspace.WriteSensitiveArtifact(workdir+"/.aiops/TASK.md", []byte(fmt.Sprintf("# Task %s\n\n%s\n", t.ID, t.Description)))
}

// Emit records a structured task event. It is a no-op when ev is nil.
// identifier is the tracker issue identifier (Task.SourceEventID) carried
// alongside taskID so the fallback `event_emit_failed` log on emitter error
// satisfies SPEC §13.1's required context fields. Pass "" when the caller
// does not have the identifier (e.g. reconciliation paths that synthesise a
// non-task taskID); the log line then omits issue_identifier=.
func Emit(ctx context.Context, ev EventEmitter, taskID, identifier, kind, msg string, payload any) {
	if ev == nil {
		return
	}
	if err := ev.AddEventWithPayload(ctx, taskID, kind, msg, payload); err != nil {
		LogTaskIDEventf(taskID, identifier, "event_emit_failed", "kind=%s error=%q", kind, err)
	}
}

// ErrSummary returns a bounded string representation of an error.
func ErrSummary(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > 500 {
		msg = msg[:500] + "..."
	}
	return msg
}

const analysisOnlyDirective = "\n\n---\n\n" +
	"**Analysis-only mode:** do not edit source files, commit, push, open PRs, " +
	"or post tracker comments on the worker's behalf. Produce your assessment " +
	"as `.aiops/PLAN.md`; any optional tracker handoff must happen through " +
	"agent-side tools advertised to you by the runtime."

const verifyDirectiveMarker = "**Verification (you own this):**"

const verifyDirectiveTemplate = "\n\n---\n\n" +
	verifyDirectiveMarker + " before you hand off — i.e. before opening a PR or moving " +
	"the issue to a review/inactive state — run the workflow's verification commands in " +
	"the workspace and make sure they pass: %s. If any fail, fix the code and re-run until " +
	"they pass; do not hand off on red. The orchestrator does not run these for you."

// AppendVerifyDirective adds the operator-declared verify.commands to the
// rendered prompt as the agent's own pre-handoff responsibility. Verification
// is the agent's job per SPEC §1; the worker no longer runs these commands.
// No-op when no commands are configured or the directive is already present.
func AppendVerifyDirective(prompt string, commands []string) string {
	cmds := make([]string, 0, len(commands))
	for _, c := range commands {
		if strings.TrimSpace(c) != "" {
			cmds = append(cmds, strings.TrimSpace(c))
		}
	}
	if len(cmds) == 0 || strings.Contains(prompt, verifyDirectiveMarker) {
		return prompt
	}
	return prompt + fmt.Sprintf(verifyDirectiveTemplate, strings.Join(cmds, "; "))
}

// AppendAnalysisOnlyDirective adds the plan-artifact/no-handoff contract for
// analysis-only workflows. The directive lives in the shared worker prompt path
// so every runner, not only the mock runner, receives the same behavior request.
func AppendAnalysisOnlyDirective(prompt, mode string) string {
	if mode != "analysis_only" {
		return prompt
	}
	if strings.Contains(prompt, strings.TrimSpace(analysisOnlyDirective)) {
		return prompt
	}
	return prompt + analysisOnlyDirective
}

// addOutputFields merges runner Result output telemetry into a payload map
// when the runner reported any. Mock runs that leave Result.Output* zero
// add no keys, preserving payload diffs for tests written before
// codex/log capture landed.
func addOutputFields(payload map[string]any, res runner.Result) {
	if res.OutputBytes > 0 {
		payload["output_bytes"] = res.OutputBytes
	}
	if res.OutputDropped > 0 {
		payload["output_dropped"] = res.OutputDropped
	}
	if res.OutputHead != "" {
		payload["output_head"] = res.OutputHead
	}
	if res.OutputTail != "" {
		payload["output_tail"] = res.OutputTail
	}
}
