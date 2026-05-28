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

// secretScanFn is the indirection used to swap the real workspace scanner
// for a stub in tests. It mirrors workspace.RunSecretScan's signature.
type secretScanFn func(ctx context.Context, workdir string, cfg workflow.SecretScanConfig) workspace.SecretScanResult

// RunTaskError bundles the resolved workflow Config alongside the error so
// callers can classify failures without re-resolving the workflow.
type RunTaskError struct {
	Cfg          workflow.Config
	Err          error
	NonRetryable bool
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

// runTask executes a single task end-to-end and returns a *RunTaskError when
// the task fails. Returns nil on success.
//
// Per SPEC §1, push, PR creation, and tracker state writes are the agent's
// responsibility. The worker's role is: claim, prepare workspace, resolve
// workflow, run agent session, enforce policy/secret-scan/RUN_SUMMARY gates,
// emit events, and clean up.

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
}

// RunTask executes a single in-memory task. The orchestrator-backed worker path
// uses this directly after claiming a tracker issue in runtime state.
func RunTask(ctx context.Context, ev EventEmitter, t task.Task, cfg Config) (ret *RunTaskError) {
	currentPhase := task.RunAttemptPhase("")
	phaseTerminal := false
	emitTaskPhase := func(from, to task.RunAttemptPhase) {
		EmitPhaseTransition(ctx, ev, t.ID, t.SourceEventID, from, to)
		currentPhase = to
		phaseTerminal = isTerminalPhase(to)
	}
	defer func() {
		if ret != nil && currentPhase != "" && !phaseTerminal {
			emitTaskPhase(currentPhase, task.PhaseFailed)
		}
	}()

	emitTaskPhase("", task.PhasePreparingWorkspace)
	wf, workflowSource, err := ResolveWorkflow(ctx, ev, t.ID, t.SourceEventID, cfg.Workflow)
	if err != nil {
		return &RunTaskError{Err: err}
	}
	wcfg := wf.Config
	hooks := wcfg.WorkspaceHooks()

	workspaceRoot := EffectiveWorkspaceRoot(cfg, wcfg)
	mgr := workspace.New(workspaceRoot)
	mgr.MirrorRoot = cfg.MirrorRoot
	workdir, createdNow, err := mgr.PrepareGitWorkspace(ctx, t)
	if err != nil {
		return &RunTaskError{Cfg: wcfg, Err: err}
	}
	workspaceBase := ""
	if wcfg.Policy.Mode == "analysis_only" {
		workspaceBase, err = workspace.ResolveBaseBranchRef(ctx, workdir, t.BaseBranch)
		if err != nil {
			return &RunTaskError{Cfg: wcfg, Err: fmt.Errorf("resolve workspace base: %w", err)}
		}
	}
	if createdNow {
		// SPEC §9.4: after_create runs only when a workspace directory
		// is newly created. Reuses skip it so bootstrap commands
		// (`npm ci`, `pip install`, …) remain the one-time init they're
		// documented as.
		if err := runWorkspaceHook(ctx, ev, t.ID, t.SourceEventID, workdir, workspace.HookAfterCreate, hooks.AfterCreate, hooks.TimeoutMs, hooks.EnvPassthrough); err != nil {
			removeWorkdirAfterHookFailure(ctx, ev, t.ID, t.SourceEventID, workspaceRoot, workdir, hooks.BeforeRemove, hooks.TimeoutMs, hooks.EnvPassthrough, "after_create")
			return &RunTaskError{Cfg: wcfg, Err: err}
		}
	}

	if t.Model == "" || t.Model == "mock" {
		t.Model = wcfg.Agent.Default
		if t.Model == "" {
			t.Model = "mock"
		}
	}

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
	policyFeedback, policyFeedbackPath, feedbackErr := readPolicyViolationFeedback(workspaceRoot, t)
	if feedbackErr != nil {
		Emit(ctx, ev, t.ID, t.SourceEventID, task.EventPolicyFeedbackReadError, "failed to read prior policy violation feedback", map[string]any{
			"path":  policyFeedbackPath,
			"error": ErrSummary(feedbackErr),
		})
		WriteFailureArtifacts(ctx, workdir, nil, "policy feedback read failed: "+ErrSummary(feedbackErr))
		return &RunTaskError{Cfg: wcfg, Err: fmt.Errorf("read policy violation feedback: %w", feedbackErr), NonRetryable: true}
	} else if policyFeedback != nil {
		policyBudget := wcfg.Agent.PolicyViolationBudgetValue()
		Emit(ctx, ev, t.ID, t.SourceEventID, task.EventPolicyFeedbackLoaded, "loaded prior policy violation feedback", map[string]any{
			"path":            policyFeedbackPath,
			"violation_count": policyFeedback.Count,
			"summary":         policyFeedback.Summary,
			"budget":          policyBudget,
		})
		if policyBudget > 0 && policyFeedback.Count >= policyBudget {
			WriteFailureArtifacts(ctx, workdir, nil, "policy violation retry budget already exhausted: "+policyFeedback.Summary)
			return &RunTaskError{Cfg: wcfg, Err: fmt.Errorf("policy violation retry budget already exhausted after %d attempts: %s", policyFeedback.Count, policyFeedback.Summary), NonRetryable: true}
		}
	}
	emitTaskPhase(task.PhasePreparingWorkspace, task.PhaseBuildingPrompt)
	prompt, err := workflow.Render(wf.PromptTemplate, renderVars)
	if err != nil {
		return &RunTaskError{Cfg: wcfg, Err: err, NonRetryable: true}
	}
	prompt = appendPolicyViolationFeedback(prompt, policyFeedback, wcfg)
	prompt = AppendAnalysisOnlyDirective(prompt, wcfg.Policy.Mode)
	prompt = AppendRunSummaryDirective(prompt)
	if err := writeTaskFiles(workdir, t, prompt); err != nil {
		return &RunTaskError{Cfg: wcfg, Err: err}
	}

	r, err := runner.New(t.Model)
	if err != nil {
		return &RunTaskError{Cfg: wcfg, Err: err}
	}

	// Make sure post-runner artifact gates cannot pass on stale files
	// from a previous run or from the base branch. PrepareGitWorkspace
	// resets tracked files to origin/<base> on every prepare (fresh
	// checkout on first touch, `checkout --force -B` on reuse per
	// SPEC §9.1), but RUN_SUMMARY.md / .aiops/PLAN.md may also be
	// committed on the base branch itself (left over from a prior PR
	// or seeded by hand), and on reuse any untracked artifact written
	// by the previous run still lingers in the workdir. Deleting them
	// here means the gates can only succeed when the runner produced
	// artifacts during this invocation.
	if err := workspace.ResetRunSummary(workdir); err != nil {
		return &RunTaskError{Cfg: wcfg, Err: fmt.Errorf("reset run summary: %w", err)}
	}
	if wcfg.Policy.Mode == "analysis_only" {
		if err := os.Remove(filepath.Join(workdir, ".aiops", "PLAN.md")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return &RunTaskError{Cfg: wcfg, Err: fmt.Errorf("reset analysis plan: %w", err)}
		}
	}

	if err := runWorkspaceHook(ctx, ev, t.ID, t.SourceEventID, workdir, workspace.HookBeforeRun, hooks.BeforeRun, hooks.TimeoutMs, hooks.EnvPassthrough); err != nil {
		WriteFailureArtifacts(ctx, workdir, nil, "before_run hook failed: "+ErrSummary(err))
		return &RunTaskError{Cfg: wcfg, Err: err}
	}

	var refreshIssueState runner.IssueStateRefresher
	if cfg.IssueStateRefresher != nil {
		refreshIssueState = cfg.IssueStateRefresher(t, wcfg)
	}
	res, runErr := RunRunnerWithTimeout(ctx, ev, r, runner.RunInput{Task: t, Workflow: *wf, Workdir: workdir, WorkspaceRoot: workspaceRoot, Prompt: prompt, RefreshIssueState: refreshIssueState, PhaseTransitionSink: func(from, to task.RunAttemptPhase) {
		emitTaskPhase(from, to)
	}}, wcfg.Agent.Timeout, workflowSource)
	sessionID := sessionIDFromRuntimeEvents(res.RuntimeEvents)
	if runErr != nil {
		if err := runWorkspaceHook(ctx, ev, t.ID, t.SourceEventID, workdir, workspace.HookAfterRun, hooks.AfterRun, hooks.TimeoutMs, hooks.EnvPassthrough); err != nil {
			LogIssueSessionEventf(t, sessionID, "after_run_hook_failed", "after_runner_error=true error=%q", err)
		}
		WriteFailureArtifacts(ctx, workdir, nil, "runner failed: "+ErrSummary(runErr))
		return &RunTaskError{Cfg: wcfg, Err: runErr}
	}

	if err := runWorkspaceHook(ctx, ev, t.ID, t.SourceEventID, workdir, workspace.HookAfterRun, hooks.AfterRun, hooks.TimeoutMs, hooks.EnvPassthrough); err != nil {
		LogIssueSessionEventf(t, sessionID, "after_run_hook_failed", "error=%q", err)
	}

	if err := workspace.EnforcePolicy(ctx, workdir, wcfg); err != nil {
		feedback, feedbackPath, feedbackErr := writePolicyViolationFeedback(workspaceRoot, t, err)
		willRetry := true
		if feedbackErr != nil {
			willRetry = false
		}
		policyBudget := wcfg.Agent.PolicyViolationBudgetValue()
		if feedback != nil && policyBudget > 0 && feedback.Count >= policyBudget {
			willRetry = false
		}
		recordPolicyViolation(ctx, ev, t.ID, t.SourceEventID, err, feedback, feedbackPath, willRetry, feedbackErr, policyBudget)
		WriteFailureArtifacts(ctx, workdir, nil, "policy check failed: "+ErrSummary(err))
		if feedbackErr != nil {
			return &RunTaskError{Cfg: wcfg, Err: fmt.Errorf("write policy violation feedback: %w; original policy error: %w", feedbackErr, err), NonRetryable: true}
		}
		if !willRetry {
			return &RunTaskError{Cfg: wcfg, Err: fmt.Errorf("repeated policy violation after %d attempts: %w", feedback.Count, err), NonRetryable: true}
		}
		return &RunTaskError{Cfg: wcfg, Err: err}
	}

	if _, err := RunVerifyPhase(ctx, ev, t.ID, t.SourceEventID, workdir, wcfg); err != nil {
		return &RunTaskError{Cfg: wcfg, Err: err}
	}

	if err := enforceAnalysisOnlyChanges(ctx, ev, t.ID, t.SourceEventID, workdir, workspaceBase, wcfg); err != nil {
		WriteFailureArtifacts(ctx, workdir, nil, ErrSummary(err))
		return &RunTaskError{Cfg: wcfg, Err: err}
	}

	// Gate: the runner is required to write .aiops/RUN_SUMMARY.md describing
	// the change. If it is missing/empty/a placeholder we refuse to record
	// success and emit `summary_missing` + `failed_attempt` events so the
	// human can see exactly why the task did not progress. We do NOT fall
	// back to a worker-generated summary here on purpose: the artifact must
	// come from the runner so it reflects intent, not a synthesized recap.
	//
	// This gate runs BEFORE the secret scanner: a run with no real summary
	// is not allowed to proceed regardless of scanner outcome, and skipping
	// the scanner on this path keeps the failure attribution unambiguous.
	_, status, checkErr := workspace.CheckSummary(workdir)
	if checkErr != nil {
		Emit(ctx, ev, t.ID, t.SourceEventID, "summary_missing", "read RUN_SUMMARY.md failed", map[string]any{
			"path":  workspace.SummaryPath,
			"error": ErrSummary(checkErr),
		})
		Emit(ctx, ev, t.ID, t.SourceEventID, task.EventFailedAttempt, "missing run summary artifact", map[string]any{
			"reason": "summary_unreadable",
			"path":   workspace.SummaryPath,
		})
		WriteFailureArtifacts(ctx, workdir, nil, "RUN_SUMMARY.md unreadable: "+ErrSummary(checkErr))
		return &RunTaskError{Cfg: wcfg, Err: fmt.Errorf("read %s: %w", workspace.SummaryPath, checkErr)}
	}
	if status != workspace.SummaryOK {
		Emit(ctx, ev, t.ID, t.SourceEventID, "summary_missing", "runner did not produce RUN_SUMMARY.md", map[string]any{
			"path":   workspace.SummaryPath,
			"status": string(status),
		})
		Emit(ctx, ev, t.ID, t.SourceEventID, task.EventFailedAttempt, "missing run summary artifact", map[string]any{
			"reason": "summary_" + string(status),
			"path":   workspace.SummaryPath,
		})
		WriteFailureArtifacts(ctx, workdir, nil, fmt.Sprintf("RUN_SUMMARY.md %s; runner must write %s before exiting.", status, workspace.SummaryPath))
		return &RunTaskError{Cfg: wcfg, Err: fmt.Errorf("missing required artifact %s (%s)", workspace.SummaryPath, status)}
	}

	// Run the optional secret scanner. The scanner runs even
	// though push is now the agent's responsibility — it acts as a final
	// gate that fails the task before the orchestrator records success, so
	// a branch carrying credential leaks is never considered complete.
	if err := runSecretScan(ctx, ev, t.ID, t.SourceEventID, workdir, wf.Config); err != nil {
		WriteFailureArtifacts(ctx, workdir, nil, "secret scan blocked: "+ErrSummary(err))
		return &RunTaskError{Cfg: wcfg, Err: err}
	}

	// Snapshot the changed files after all gates have passed so
	// CHANGED_FILES.txt is available as a workspace artifact for post-run
	// inspection. This does not push anything; that is the agent's job.
	if err := workspace.WriteChangedFiles(workdir, nil); err != nil {
		LogIssueEventf(t, "changed_files_seed_failed", "error=%q", err)
	}
	changed, _ := workspace.AllChangedFiles(ctx, workdir)
	if err := workspace.WriteChangedFiles(workdir, changed); err != nil {
		LogIssueEventf(t, "changed_files_write_failed", "error=%q", err)
	}
	clearPolicyViolationFeedback(workspaceRoot, t)

	emitTaskPhase(task.PhaseFinishing, task.PhaseSucceeded)
	return nil
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
// runner.Result is what runTask passes to runSummary on the success path;
// on failure (timeout or non-zero exit) the same Result is returned so that
// any partial output telemetry the runner managed to capture (OutputBytes,
// OutputHead, OutputTail, etc.) is still available to the caller.
func RunRunnerWithTimeout(ctx context.Context, ev EventEmitter, r runner.Runner, in runner.RunInput, timeout time.Duration, workflowSource string) (runner.Result, error) {
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

// RunVerifyPhase runs the configured verify commands, persists the
// VERIFICATION.txt artifact, emits the verify_start/verify_end events,
// and returns whether the run is in degraded mode. Degraded mode means
// at least one command failed (or the phase deadline elapsed) AND the
// operator has opted into verify.allow_failure: the caller continues
// but must annotate the result. The agent is responsible for deciding
// whether to open a draft PR when degraded=true.
//
// Returns (degraded, err). When err is non-nil, verify failed AND
// allow_failure was off; the caller propagates the error. When
// degraded=true, err is nil.
func RunVerifyPhase(ctx context.Context, ev EventEmitter, taskID, identifier, workdir string, cfg workflow.Config) (bool, error) {
	Emit(ctx, ev, taskID, identifier, task.EventVerifyStart, "verify started", map[string]any{
		"commands":      cfg.Verify.Commands,
		"timeout_ms":    cfg.Verify.Timeout.Milliseconds(),
		"allow_failure": cfg.Verify.AllowFailure,
	})
	start := time.Now()
	results, verifyErr := workspace.RunVerify(ctx, workdir, cfg)
	if writeErr := workspace.WriteVerification(workdir, results); writeErr != nil {
		LogTaskIDEventf(taskID, identifier, "verification_write_failed", "error=%q", writeErr)
	}
	payload := map[string]any{
		"duration_ms":   time.Since(start).Milliseconds(),
		"commands":      SummarizeVerifyResults(results),
		"failed_count":  countVerifyFailures(results),
		"allow_failure": cfg.Verify.AllowFailure,
	}
	if verifyErr == nil {
		payload["status"] = "ok"
		Emit(ctx, ev, taskID, identifier, task.EventVerifyEnd, "verify completed", payload)
		return false, nil
	}
	payload["error"] = ErrSummary(verifyErr)
	// Parent context cancellation (worker shutdown, task abort) must always
	// propagate, even when allow_failure is on. allow_failure only downgrades
	// real verification failures — a canceled task is not an "investigation"
	// case and must not result in the task completing. RunVerify
	// returns ctx.Err() directly on parent-cancel, so errors.Is matches.
	if errors.Is(verifyErr, context.Canceled) || errors.Is(verifyErr, context.DeadlineExceeded) {
		payload["status"] = "canceled"
		Emit(ctx, ev, taskID, identifier, task.EventVerifyEnd, "verify canceled", payload)
		return false, verifyErr
	}
	if cfg.Verify.AllowFailure {
		payload["status"] = "failed_allowed"
		Emit(ctx, ev, taskID, identifier, task.EventVerifyEnd, "verify failed (investigation mode)", payload)
		return true, nil
	}
	payload["status"] = "failed"
	Emit(ctx, ev, taskID, identifier, task.EventVerifyEnd, "verify failed", payload)
	WriteFailureArtifacts(ctx, workdir, results, "verify failed: "+ErrSummary(verifyErr))
	return false, verifyErr
}

func countVerifyFailures(results []workspace.VerifyResult) int {
	n := 0
	for _, r := range results {
		if r.Err != nil || r.ExitCode != 0 {
			n++
		}
	}
	return n
}

// runSecretScan executes the configured secret scanner and
// records structured task events for the start, clean exit, finding, or
// execution error cases. It returns a non-nil error only when the task
// should be failed, so the caller can simply propagate the error and let
// the existing failed_attempt path take over.
//
// Event kinds emitted (mirroring the existing event vocabulary):
//
//   - secret_scan_start:     scanner is about to run
//   - secret_scan_clean:     scanner exited zero, no findings
//   - secret_scan_violation: scanner exited non-zero (potential secrets)
//   - secret_scan_error:     scanner failed to run (binary missing, etc.)
//
// When the scan is disabled or unconfigured, no events are emitted; this
// preserves the worker's previous behavior for repos that have not opted
// in.
func runSecretScan(ctx context.Context, ev EventEmitter, taskID, _ string, workdir string, cfg workflow.Config) error {
	return runSecretScanWith(ctx, ev, taskID, workdir, cfg, workspace.RunSecretScan)
}

func runSecretScanWith(ctx context.Context, ev EventEmitter, taskID string, workdir string, cfg workflow.Config, scan secretScanFn) error {
	scfg := cfg.Verify.SecretScan
	if !scfg.Enabled || len(scfg.Command) == 0 {
		return nil
	}

	_ = ev.AddEventWithPayload(ctx, taskID, "secret_scan_start", "running secret scan", map[string]any{
		"command": scfg.Command,
	})

	res := scan(ctx, workdir, scfg)
	payload := map[string]any{
		"command":     res.Command,
		"exit_code":   res.ExitCode,
		"duration_ms": res.DurationMs,
		"stdout":      res.Stdout,
		"stderr":      res.Stderr,
	}

	switch res.Status {
	case workspace.SecretScanClean:
		_ = ev.AddEventWithPayload(ctx, taskID, "secret_scan_clean", "secret scan reported no findings", payload)
		return nil
	case workspace.SecretScanViolation:
		msg := fmt.Sprintf("secret scan reported findings (exit %d)", res.ExitCode)
		_ = ev.AddEventWithPayload(ctx, taskID, "secret_scan_violation", msg, payload)
		if res.ShouldBlockCompletion(scfg) {
			return errors.New(msg)
		}
		return nil
	case workspace.SecretScanError:
		msg := fmt.Sprintf("secret scan failed to execute: %v", res.Err)
		_ = ev.AddEventWithPayload(ctx, taskID, "secret_scan_error", msg, payload)
		// Execution errors always block: an operator-misconfigured scanner
		// must not silently allow the task to complete.
		return errors.New(msg)
	default:
		// Defensive: an unexpected status should not leak through. Treat
		// like a violation so the task is blocked rather than waved through.
		msg := fmt.Sprintf("secret scan returned unexpected status %q", res.Status)
		_ = ev.AddEventWithPayload(ctx, taskID, "secret_scan_error", msg, payload)
		return errors.New(msg)
	}
}

// recordPolicyViolation writes a structured `policy_violation` task event
// before the worker fails the task. budget is the configured
// `agent.policy_violation_budget` (resolved via PolicyViolationBudgetValue),
// emitted alongside the running count so dashboards can render "violation N
// of M (final attempt)".
func recordPolicyViolation(ctx context.Context, ev EventEmitter, taskID, _ string, err error, feedback *policyViolationFeedback, feedbackPath string, willRetry bool, feedbackErr error, budget int) {
	if ev == nil {
		return
	}
	var perr *workspace.PolicyError
	if errors.As(err, &perr) {
		payload := map[string]any{
			"violations": perr.Violations,
			"will_retry": willRetry,
			"budget":     budget,
		}
		if feedback != nil {
			payload["violation_count"] = feedback.Count
		}
		if feedbackPath != "" {
			payload["feedback_path"] = feedbackPath
		}
		if feedbackErr != nil {
			payload["feedback_error"] = ErrSummary(feedbackErr)
		}
		if emitErr := ev.AddEventWithPayload(ctx, taskID, task.EventPolicyViolation, perr.Error(), payload); emitErr == nil {
			return
		}
	}
	_ = ev.AddEvent(ctx, taskID, task.EventPolicyViolation, err.Error())
}

func writeTaskFiles(workdir string, t task.Task, prompt string) error {
	if err := workspace.WritePrompt(workdir, prompt); err != nil {
		return err
	}
	return workspace.WriteSensitiveArtifact(workdir+"/.aiops/TASK.md", []byte(fmt.Sprintf("# Task %s\n\n%s\n", t.ID, t.Description)))
}

// WriteFailureArtifacts persists what we know on the failure path so failed
// tasks can be inspected after the fact via the workspace tree.
func WriteFailureArtifacts(ctx context.Context, workdir string, verifyResults []workspace.VerifyResult, summary string) {
	if changed, err := workspace.AllChangedFiles(ctx, workdir); err == nil {
		_ = workspace.WriteChangedFiles(workdir, changed)
	}
	if len(verifyResults) > 0 {
		_ = workspace.WriteVerification(workdir, verifyResults)
	}
	_ = workspace.WriteSummary(workdir, summary+"\n")
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

// SummarizeVerifyResults converts a slice of VerifyResult to a JSON-safe
// slice of maps for event payloads.
func SummarizeVerifyResults(results []workspace.VerifyResult) []map[string]any {
	out := make([]map[string]any, 0, len(results))
	for _, r := range results {
		entry := map[string]any{
			"command":     r.Command,
			"exit_code":   r.ExitCode,
			"duration_ms": r.Duration.Milliseconds(),
		}
		if r.Err != nil {
			entry["error"] = ErrSummary(r.Err)
		}
		out = append(out, entry)
	}
	return out
}

// runSummaryDirective is appended to every rendered prompt so runners know the
// worker requires a RUN_SUMMARY.md artifact. Per SPEC §1, push, PR creation,
// and tracker writes are not orchestrator responsibilities; workflow/tooling
// instructions control whether the in-run agent performs those actions.
const runSummaryDirective = "\n\n---\n\n" +
	"**Required output:** before exiting, you MUST write `.aiops/RUN_SUMMARY.md` " +
	"describing what you changed, why, and how it was verified. The task will " +
	"fail if this file is missing, empty, or contains only a placeholder."

const analysisOnlyDirective = "\n\n---\n\n" +
	"**Analysis-only mode:** do not edit source files, commit, push, open PRs, " +
	"or post tracker comments on the worker's behalf. Produce your assessment " +
	"as `.aiops/PLAN.md`; any optional tracker handoff must happen through " +
	"agent-side tools advertised to you by the runtime."

func enforceAnalysisOnlyChanges(ctx context.Context, ev EventEmitter, taskID, identifier, workdir, baseRef string, cfg workflow.Config) error {
	if cfg.Policy.Mode != "analysis_only" {
		return nil
	}
	planPath := filepath.Join(workdir, ".aiops", "PLAN.md")
	plan, err := os.ReadFile(planPath)
	if err != nil || strings.TrimSpace(string(plan)) == "" {
		Emit(ctx, ev, taskID, identifier, task.EventAnalysisOnlyViolation, "analysis-only run did not produce .aiops/PLAN.md", map[string]any{
			"path": ".aiops/PLAN.md",
		})
		if err != nil {
			return fmt.Errorf("analysis-only run did not produce .aiops/PLAN.md: %w", err)
		}
		return fmt.Errorf("analysis-only run did not produce .aiops/PLAN.md")
	}
	changed, err := workspace.AllChangedFilesSinceRef(ctx, workdir, baseRef)
	if err != nil {
		return fmt.Errorf("inspect analysis-only changes: %w", err)
	}
	violations := make([]string, 0)
	for _, path := range changed {
		if analysisOnlyArtifactAllowed(path) {
			continue
		}
		violations = append(violations, path)
	}
	if len(violations) > 0 {
		Emit(ctx, ev, taskID, identifier, task.EventAnalysisOnlyViolation, "analysis-only run changed source files", map[string]any{
			"files": violations,
		})
		return fmt.Errorf("analysis-only run changed source files: %s", strings.Join(violations, ", "))
	}
	committed, err := workspace.HasCommitsSinceRef(ctx, workdir, baseRef)
	if err != nil {
		return fmt.Errorf("inspect analysis-only commits: %w", err)
	}
	if committed {
		Emit(ctx, ev, taskID, identifier, task.EventAnalysisOnlyViolation, "analysis-only run created commits", nil)
		return fmt.Errorf("analysis-only run created commits")
	}
	return nil
}

func analysisOnlyArtifactAllowed(path string) bool {
	return workspace.IsAllowedHandoffArtifact(path)
}

// AppendRunSummaryDirective adds the RUN_SUMMARY.md contract to the rendered
// prompt unless the full directive is already present.
func AppendRunSummaryDirective(prompt string) string {
	if strings.Contains(prompt, strings.TrimSpace(runSummaryDirective)) {
		return prompt
	}
	return prompt + runSummaryDirective
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
