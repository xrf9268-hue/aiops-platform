package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
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

// PRClient is the subset of gitea.Client the worker needs for PR handoff.
// Defined as an interface so tests can inject a fake implementation without
// standing up a real Gitea HTTP server.
type PRClient interface {
	FindOpenPullRequest(ctx context.Context, in gitea.FindOpenPullRequestInput) (*gitea.PullRequest, error)
	CreatePullRequest(ctx context.Context, in gitea.CreatePullRequestInput) (*gitea.PullRequest, error)
}

// secretScanFn is the indirection used to swap the real workspace scanner
// for a stub in tests. It mirrors workspace.RunSecretScan's signature.
type secretScanFn func(ctx context.Context, workdir string, cfg workflow.SecretScanConfig) workspace.SecretScanResult

// RunTaskError bundles the resolved workflow Config alongside the error so
// handleTaskFailure can route retries without re-resolving.
type RunTaskError struct {
	Cfg workflow.Config
	Err error
}

// ResolveWorkflow performs WORKFLOW.md discovery for a prepared workdir and
// emits the workflow_resolved event before any runner work begins. Returning
// the workflow_source string lets callers stamp it onto the runner_start
// payload as a quick-look field; the full provenance lives on the
// workflow_resolved event itself.
func ResolveWorkflow(ctx context.Context, ev EventEmitter, taskID, workdir string) (*workflow.Workflow, string, error) {
	wf, res, err := workflow.Resolve(workdir)
	if err != nil {
		return nil, "", err
	}
	payload := map[string]any{
		"source":        string(res.Source),
		"agent_default": wf.Config.Agent.Default,
		"policy_mode":   wf.Config.Policy.Mode,
		"tracker_kind":  wf.Config.Tracker.Kind,
	}
	if res.Path != "" {
		payload["path"] = res.Path
	}
	if len(res.ShadowedBy) > 0 {
		payload["shadowed_by"] = res.ShadowedBy
	}
	Emit(ctx, ev, taskID, task.EventWorkflowResolved, "workflow resolved", payload)
	logWorkflowResolved(taskID, res)
	return wf, string(res.Source), nil
}

// logWorkflowResolved prints a single info-level line summarizing how the
// workflow was discovered. Format:
//
//	task <id>: workflow resolved: source=<source> path=<path> shadowed=[a,b]
//
// The path and shadowed segments are omitted when empty so the common case
// (source=default or a clean repo-root WORKFLOW.md with no shadows) stays
// short. See issue #69 for the deviation rationale: multi-path discovery is
// a deliberate extension over SPEC, and operators need a fast way to answer
// "which file is in effect?" without re-parsing events.
func logWorkflowResolved(taskID string, res *workflow.Resolution) {
	parts := []string{"source=" + string(res.Source)}
	if res.Path != "" {
		parts = append(parts, "path="+res.Path)
	}
	if len(res.ShadowedBy) > 0 {
		parts = append(parts, "shadowed=["+strings.Join(res.ShadowedBy, ",")+"]")
	}
	log.Printf("task %s: workflow resolved: %s", taskID, strings.Join(parts, " "))
}

// failingStore is the subset of queue.Store handleTaskFailure needs.
// Defined as an interface so the terminality-routing logic can be
// unit-tested with a fake; *queue.Store satisfies it implicitly.
type failingStore interface {
	Fail(ctx context.Context, id, msg string) (bool, error)
	FailTimeout(ctx context.Context, id, msg string, maxTimeoutRetries int) (bool, error)
}

// handleTaskFailure routes a task error to the right retry bucket and
// reports whether the task reached its terminal failed state.
//
// Runner timeouts use a dedicated budget (agent.max_timeout_retries) so a
// hung agent cannot consume the generic max_attempts reserved for
// verify/policy/transient infra failures. Non-timeout failures fall through
// to the legacy Fail path which is still gated by max_attempts. Failure
// artifacts (CHANGED_FILES.txt / RUN_SUMMARY.md / VERIFICATION.json) are
// written by the runTask code path on the way out, so this function is
// purely concerned with retry routing.
//
// terminal=true means the queue actually transitioned the row to
// 'failed'; the worker's tracker hooks use this to skip status
// mutations during transient retries (which would otherwise flicker
// the linked Linear issue between In Progress and Rework, and trigger
// the poller's Rework re-enqueue path so the same issue accumulates
// duplicate tasks). A queue-side error propagates as terminal=false so
// we keep the conservative (no Linear write) behavior on uncertainty.
func handleTaskFailure(ctx context.Context, store failingStore, t task.Task, cfg workflow.Config, err error) bool {
	if runner.IsTimeout(err) {
		budget := cfg.Agent.MaxTimeoutRetriesValue()
		requeued, ferr := store.FailTimeout(ctx, t.ID, err.Error(), budget)
		if ferr != nil {
			log.Printf("task %s FailTimeout error: %v", t.ID, ferr)
			return false
		}
		return !requeued
	}
	terminal, ferr := store.Fail(ctx, t.ID, err.Error())
	if ferr != nil {
		log.Printf("task %s Fail error: %v", t.ID, ferr)
		return false
	}
	return terminal
}

// runTask executes a single task end-to-end and returns a *RunTaskError when
// the task fails, bundling the resolved workflow config so callers can route
// retries by failure class. Returns nil on success.
func runTask(ctx context.Context, ev EventEmitter, t task.Task, cfg Config) *RunTaskError {
	mgr := workspace.New(cfg.WorkspaceRoot)
	mgr.MirrorRoot = cfg.MirrorRoot
	workdir, err := mgr.PrepareGitWorkspace(ctx, t)
	if err != nil {
		return &RunTaskError{Err: err}
	}

	wf, workflowSource, err := ResolveWorkflow(ctx, ev, t.ID, workdir)
	if err != nil {
		return &RunTaskError{Err: err}
	}
	wcfg := wf.Config
	if t.Model == "" || t.Model == "mock" {
		t.Model = wcfg.Agent.Default
		if t.Model == "" {
			t.Model = "mock"
		}
	}

	// The tracker transitioner is resolved from the workflow's tracker
	// config (per-task, not per-worker) because each repo carries its
	// own credentials. Constructed here so OnClaim sees the freshly
	// loaded config; the same instance is reused for OnPRCreated below
	// to amortize any future client-internal caching.
	var tr Transitioner
	if cfg.NewTransitioner != nil {
		tr = cfg.NewTransitioner(wcfg.Tracker)
	}
	OnClaim(ctx, ev, tr, t, wcfg)

	prompt := workflow.Render(wf.PromptTemplate, map[string]string{
		"task.id":          t.ID,
		"task.title":       t.Title,
		"task.description": t.Description,
		"task.actor":       t.Actor,
		"repo.owner":       t.RepoOwner,
		"repo.name":        t.RepoName,
		"repo.branch":      t.BaseBranch,
	})
	prompt = AppendRunSummaryDirective(prompt)
	if err := writeTaskFiles(workdir, t, prompt); err != nil {
		return &RunTaskError{Cfg: wcfg, Err: err}
	}

	r, err := runner.New(t.Model)
	if err != nil {
		return &RunTaskError{Cfg: wcfg, Err: err}
	}

	// Make sure the post-runner CheckSummary gate cannot pass on a stale
	// summary. PrepareGitWorkspace already nukes the workdir on a fresh
	// clone, but we still defend against the case where the base branch
	// *itself* has a committed .aiops/RUN_SUMMARY.md (left over from a prior
	// PR or seeded by hand). Deleting it here means CheckSummary can only
	// succeed when the runner produced a summary during this invocation.
	if err := workspace.ResetRunSummary(workdir); err != nil {
		return &RunTaskError{Cfg: wcfg, Err: fmt.Errorf("reset run summary: %w", err)}
	}

	if _, runErr := RunRunnerWithTimeout(ctx, ev, r, runner.RunInput{Task: t, Workflow: *wf, Workdir: workdir, Prompt: prompt}, wcfg.Agent.Timeout, workflowSource); runErr != nil {
		WriteFailureArtifacts(ctx, workdir, nil, "runner failed: "+ErrSummary(runErr))
		return &RunTaskError{Cfg: wcfg, Err: runErr}
	}

	if err := workspace.EnforcePolicy(ctx, workdir, wcfg); err != nil {
		recordPolicyViolation(ctx, ev, t.ID, err)
		WriteFailureArtifacts(ctx, workdir, nil, "policy check failed: "+ErrSummary(err))
		return &RunTaskError{Cfg: wcfg, Err: err}
	}

	verifyDegraded, err := RunVerifyPhase(ctx, ev, t.ID, workdir, wcfg)
	if err != nil {
		return &RunTaskError{Cfg: wcfg, Err: err}
	}

	// Gate: the runner is required to write .aiops/RUN_SUMMARY.md describing
	// the change. If it is missing/empty/a placeholder we refuse to open a PR
	// and emit `summary_missing` + `failed_attempt` events so the human can
	// see exactly why the task did not progress. We do NOT fall back to a
	// worker-generated summary here on purpose: the artifact must come from
	// the runner so it reflects intent, not a synthesized recap. The failure
	// path below (WriteFailureArtifacts) still seeds a worker summary for
	// post-mortem inspection — that is observed but never gates a PR.
	//
	// This gate runs BEFORE the secret scanner: a run with no real summary
	// is not allowed to push regardless of scanner outcome, and skipping the
	// scanner on this path keeps the failure attribution unambiguous.
	summary, status, checkErr := workspace.CheckSummary(workdir)
	if checkErr != nil {
		Emit(ctx, ev, t.ID, "summary_missing", "read RUN_SUMMARY.md failed", map[string]any{
			"path":  workspace.SummaryPath,
			"error": ErrSummary(checkErr),
		})
		Emit(ctx, ev, t.ID, task.EventFailedAttempt, "missing run summary artifact", map[string]any{
			"reason": "summary_unreadable",
			"path":   workspace.SummaryPath,
		})
		WriteFailureArtifacts(ctx, workdir, nil, "RUN_SUMMARY.md unreadable: "+ErrSummary(checkErr))
		return &RunTaskError{Cfg: wcfg, Err: fmt.Errorf("read %s: %w", workspace.SummaryPath, checkErr)}
	}
	if status != workspace.SummaryOK {
		Emit(ctx, ev, t.ID, "summary_missing", "runner did not produce RUN_SUMMARY.md", map[string]any{
			"path":   workspace.SummaryPath,
			"status": string(status),
		})
		Emit(ctx, ev, t.ID, task.EventFailedAttempt, "missing run summary artifact", map[string]any{
			"reason": "summary_" + string(status),
			"path":   workspace.SummaryPath,
		})
		WriteFailureArtifacts(ctx, workdir, nil, fmt.Sprintf("RUN_SUMMARY.md %s; runner must write %s before exiting.", status, workspace.SummaryPath))
		return &RunTaskError{Cfg: wcfg, Err: fmt.Errorf("missing required artifact %s (%s)", workspace.SummaryPath, status)}
	}

	// Run the optional pre-push secret scanner between the summary gate and
	// push so a branch carrying credential leaks never reaches the remote.
	// Failures flow through WriteFailureArtifacts + the existing
	// failed_attempt path so operators can inspect what the scanner saw.
	if err := runSecretScan(ctx, ev, t.ID, workdir, wf.Config); err != nil {
		WriteFailureArtifacts(ctx, workdir, nil, "secret scan blocked push: "+ErrSummary(err))
		return &RunTaskError{Cfg: wcfg, Err: err}
	}

	// Snapshot the changed files AFTER the gate has accepted the runner's
	// summary and the secret scanner has cleared the tree. We seed
	// CHANGED_FILES.txt as an empty stub so it shows up in
	// `git status --porcelain` alongside the runner-produced summary, then
	// rewrite it with the final list. RUN_SUMMARY.md is intentionally NOT
	// rewritten by the worker on the success path — see gate above.
	if err := workspace.WriteChangedFiles(workdir, nil); err != nil {
		log.Printf("task %s: seed changed files artifact: %v", t.ID, err)
	}
	changed, _ := workspace.AllChangedFiles(ctx, workdir)
	if err := workspace.WriteChangedFiles(workdir, changed); err != nil {
		log.Printf("task %s: write changed files artifact: %v", t.ID, err)
	}

	pushStart := time.Now()
	if err := workspace.CommitAndPush(ctx, workdir, t.Title, t.WorkBranch); err != nil {
		Emit(ctx, ev, t.ID, task.EventPush, "push failed", map[string]any{
			"branch":      t.WorkBranch,
			"duration_ms": time.Since(pushStart).Milliseconds(),
			"error":       ErrSummary(err),
		})
		return &RunTaskError{Cfg: wcfg, Err: err}
	}
	Emit(ctx, ev, t.ID, task.EventPush, "push completed", map[string]any{
		"branch":         t.WorkBranch,
		"duration_ms":    time.Since(pushStart).Milliseconds(),
		"changed_files":  len(changed),
		"sample_changes": sampleSlice(changed, 10),
	})

	if verifyDegraded {
		wcfg.PR.Draft = true
	}
	prErr := CreatePR(ctx, ev, t, wcfg, cfg, summary, verifyDegraded)
	if prErr == nil {
		// Fired on both create-new and reuse-existing paths so a retry
		// that lands on an already-open PR still flips the Linear issue
		// to "Human Review" — the human's signal that hands have been
		// handed off, regardless of which gitea code path produced the PR.
		OnPRCreated(ctx, ev, tr, t, wcfg)
	}
	return wrapErr(wcfg, prErr)
}

// wrapErr returns nil if err is nil, otherwise wraps it in a RunTaskError.
func wrapErr(cfg workflow.Config, err error) *RunTaskError {
	if err == nil {
		return nil
	}
	return &RunTaskError{Cfg: cfg, Err: err}
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

	Emit(ctx, ev, in.Task.ID, task.EventRunnerStart, "runner started", map[string]any{
		"model":           in.Task.Model,
		"timeout_ms":      timeout.Milliseconds(),
		"workflow_source": workflowSource,
	})

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	res, runErr := r.Run(runCtx, in)
	elapsed := time.Since(start)

	if runErr != nil {
		var te *runner.TimeoutError
		if errors.As(runErr, &te) {
			timeoutPayload := map[string]any{
				"model":      in.Task.Model,
				"timeout_ms": te.Timeout.Milliseconds(),
				"elapsed_ms": te.Elapsed.Milliseconds(),
			}
			addOutputFields(timeoutPayload, res)
			Emit(ctx, ev, in.Task.ID, task.EventRunnerTimeout, te.Error(), timeoutPayload)
			return res, runErr
		}
		failurePayload := map[string]any{
			"model":       in.Task.Model,
			"duration_ms": elapsed.Milliseconds(),
			"error":       ErrSummary(runErr),
			"ok":          false,
		}
		addOutputFields(failurePayload, res)
		Emit(ctx, ev, in.Task.ID, task.EventRunnerEnd, "runner failed", failurePayload)
		return res, runErr
	}

	endPayload := map[string]any{
		"model":       in.Task.Model,
		"duration_ms": elapsed.Milliseconds(),
		"ok":          true,
	}
	if res.Summary != "" {
		endPayload["summary"] = res.Summary
	}
	addOutputFields(endPayload, res)
	Emit(ctx, ev, in.Task.ID, task.EventRunnerEnd, "runner completed", endPayload)
	return res, nil
}

// RunVerifyPhase runs the configured verify commands, persists the
// VERIFICATION.txt artifact, emits the verify_start/verify_end events,
// and returns whether the run is in degraded mode. Degraded mode means
// at least one command failed (or the phase deadline elapsed) AND the
// operator has opted into verify.allow_failure: the caller continues
// to PR creation but must mark the PR draft and annotate the body.
//
// Returns (degraded, err). When err is non-nil, verify failed AND
// allow_failure was off; the caller propagates the error and skips PR
// creation. When degraded=true, err is nil but downstream stages must
// signal the verify failure to the human.
func RunVerifyPhase(ctx context.Context, ev EventEmitter, taskID, workdir string, cfg workflow.Config) (bool, error) {
	Emit(ctx, ev, taskID, task.EventVerifyStart, "verify started", map[string]any{
		"commands":      cfg.Verify.Commands,
		"timeout_ms":    cfg.Verify.Timeout.Milliseconds(),
		"allow_failure": cfg.Verify.AllowFailure,
	})
	start := time.Now()
	results, verifyErr := workspace.RunVerify(ctx, workdir, cfg)
	if writeErr := workspace.WriteVerification(workdir, results); writeErr != nil {
		log.Printf("task %s: write verification artifact: %v", taskID, writeErr)
	}
	payload := map[string]any{
		"duration_ms":   time.Since(start).Milliseconds(),
		"commands":      SummarizeVerifyResults(results),
		"failed_count":  countVerifyFailures(results),
		"allow_failure": cfg.Verify.AllowFailure,
	}
	if verifyErr == nil {
		payload["status"] = "ok"
		Emit(ctx, ev, taskID, task.EventVerifyEnd, "verify completed", payload)
		return false, nil
	}
	payload["error"] = ErrSummary(verifyErr)
	// Parent context cancellation (worker shutdown, task abort) must always
	// propagate, even when allow_failure is on. allow_failure only downgrades
	// real verification failures — a canceled task is not an "investigation"
	// case and must not result in a degraded PR being opened. RunVerify
	// returns ctx.Err() directly on parent-cancel, so errors.Is matches.
	if errors.Is(verifyErr, context.Canceled) || errors.Is(verifyErr, context.DeadlineExceeded) {
		payload["status"] = "canceled"
		Emit(ctx, ev, taskID, task.EventVerifyEnd, "verify canceled", payload)
		return false, verifyErr
	}
	if cfg.Verify.AllowFailure {
		payload["status"] = "failed_allowed"
		Emit(ctx, ev, taskID, task.EventVerifyEnd, "verify failed (investigation mode)", payload)
		return true, nil
	}
	payload["status"] = "failed"
	Emit(ctx, ev, taskID, task.EventVerifyEnd, "verify failed", payload)
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

// runSecretScan executes the configured pre-push secret scanner and
// records structured task events for the start, clean exit, finding, or
// execution error cases. It returns a non-nil error only when the push
// should be aborted, so the caller can simply propagate the error and let
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
func runSecretScan(ctx context.Context, ev EventEmitter, taskID string, workdir string, cfg workflow.Config) error {
	return runSecretScanWith(ctx, ev, taskID, workdir, cfg, workspace.RunSecretScan)
}

func runSecretScanWith(ctx context.Context, ev EventEmitter, taskID string, workdir string, cfg workflow.Config, scan secretScanFn) error {
	scfg := cfg.Verify.SecretScan
	if !scfg.Enabled || len(scfg.Command) == 0 {
		return nil
	}

	_ = ev.AddEventWithPayload(ctx, taskID, "secret_scan_start", "running pre-push secret scan", map[string]any{
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
		if res.ShouldBlockPush(scfg) {
			return errors.New(msg)
		}
		return nil
	case workspace.SecretScanError:
		msg := fmt.Sprintf("secret scan failed to execute: %v", res.Err)
		_ = ev.AddEventWithPayload(ctx, taskID, "secret_scan_error", msg, payload)
		// Execution errors always block: an operator-misconfigured scanner
		// must not silently allow pushes.
		return errors.New(msg)
	default:
		// Defensive: an unexpected status should not leak through. Treat
		// like a violation so pushes are blocked rather than waved through.
		msg := fmt.Sprintf("secret scan returned unexpected status %q", res.Status)
		_ = ev.AddEventWithPayload(ctx, taskID, "secret_scan_error", msg, payload)
		return errors.New(msg)
	}
}

// recordPolicyViolation writes a structured `policy_violation` task event
// before the worker fails the task. The push/PR step is skipped because the
// caller returns the error immediately after this call.
func recordPolicyViolation(ctx context.Context, ev EventEmitter, taskID string, err error) {
	if ev == nil {
		return
	}
	var perr *workspace.PolicyError
	if errors.As(err, &perr) {
		if emitErr := ev.AddEventWithPayload(ctx, taskID, "policy_violation", perr.Error(), map[string]any{"violations": perr.Violations}); emitErr == nil {
			return
		}
	}
	_ = ev.AddEvent(ctx, taskID, "policy_violation", err.Error())
}

// CreatePR creates a pull request using the gitea client configured from
// the worker Config. Use CreatePRWith to inject a custom client.
func CreatePR(ctx context.Context, ev EventEmitter, t task.Task, wcfg workflow.Config, workerCfg Config, summary string, verifyDegraded bool) error {
	var client PRClient
	if workerCfg.NewPRClient != nil {
		client = workerCfg.NewPRClient()
	} else {
		client = gitea.Client{BaseURL: workerCfg.GiteaBaseURL, Token: workerCfg.GiteaToken}
	}
	return CreatePRWith(ctx, ev, t, wcfg, summary, verifyDegraded, client)
}

// CreatePRWith performs the PR handoff using the supplied client. It first
// looks for an existing open PR for the work branch and reuses it instead of
// asking Gitea to create a duplicate — this is what makes retries safe to
// re-enter after a previous attempt already produced a PR. A list error is
// logged and falls through to the create path so a transient Gitea hiccup
// during the lookup does not block the task; the create call itself remains
// the source of truth for surfacing real failures.
func CreatePRWith(ctx context.Context, ev EventEmitter, t task.Task, cfg workflow.Config, summary string, verifyDegraded bool, client PRClient) error {
	if existing, err := client.FindOpenPullRequest(ctx, gitea.FindOpenPullRequestInput{
		Owner: t.RepoOwner, Repo: t.RepoName, Head: t.WorkBranch,
	}); err != nil {
		log.Printf("task %s: list open PRs failed: %v", t.ID, err)
	} else if existing != nil {
		Emit(ctx, ev, t.ID, task.EventPRReused, "pr reused", map[string]any{
			"number":   existing.Number,
			"html_url": existing.HTMLURL,
			"title":    existing.Title,
		})
		log.Printf("task %s reused PR #%d %s", t.ID, existing.Number, existing.HTMLURL)
		return nil
	}
	body := BuildPRBody(t, summary, verifyDegraded)
	pr, err := client.CreatePullRequest(ctx, gitea.CreatePullRequestInput{Owner: t.RepoOwner, Repo: t.RepoName, Title: "chore(ai): " + t.Title, Body: body, Head: t.WorkBranch, Base: t.BaseBranch, Draft: cfg.PR.Draft})
	if err != nil {
		Emit(ctx, ev, t.ID, task.EventPRCreated, "pr creation failed", map[string]any{"error": ErrSummary(err)})
		return err
	}
	Emit(ctx, ev, t.ID, task.EventPRCreated, "pr created", map[string]any{
		"number":   pr.Number,
		"html_url": pr.HTMLURL,
		"title":    pr.Title,
	})
	log.Printf("task %s created PR #%d %s", t.ID, pr.Number, pr.HTMLURL)
	return nil
}

func writeTaskFiles(workdir string, t task.Task, prompt string) error {
	if err := workspace.WritePrompt(workdir, prompt); err != nil {
		return err
	}
	return os.WriteFile(workdir+"/.aiops/TASK.md", []byte(fmt.Sprintf("# Task %s\n\n%s\n", t.ID, t.Description)), 0o644)
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
func Emit(ctx context.Context, ev EventEmitter, taskID, kind, msg string, payload any) {
	if ev == nil {
		return
	}
	if err := ev.AddEventWithPayload(ctx, taskID, kind, msg, payload); err != nil {
		log.Printf("task %s: emit %s event: %v", taskID, kind, err)
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

// PRBodySummaryCap bounds how much of the runner-produced RUN_SUMMARY.md
// we inline into the PR body. 8 KiB is well under Gitea's body limit and
// keeps the rendered review page navigable; the full file is always
// available via the .aiops/RUN_SUMMARY.md path linked beneath the excerpt.
const PRBodySummaryCap = 8 << 10 // 8 KiB

// BuildPRBody renders the pull request body with the runner-produced
// RUN_SUMMARY.md content inlined (truncated to PRBodySummaryCap) and a link
// to the full artifact path on the work branch. Callers must pass a summary
// that has already been validated by workspace.CheckSummary. When
// verifyDegraded is true (verify failed under verify.allow_failure), the
// body is prepended with a blockquote banner pointing reviewers at
// .aiops/VERIFICATION.txt.
func BuildPRBody(t task.Task, summary string, verifyDegraded bool) string {
	excerpt, truncated := truncateForPR(summary, PRBodySummaryCap)
	var b strings.Builder
	if verifyDegraded {
		b.WriteString("> ⚠️ **Verification failed (investigation mode).** ")
		b.WriteString("This PR was opened despite a failing verify phase because ")
		b.WriteString("`verify.allow_failure` is enabled. Inspect ")
		b.WriteString("`.aiops/VERIFICATION.txt` before merging.\n\n")
	}
	fmt.Fprintf(&b, "## AI Task\n\nTask ID: `%s`\n\n", t.ID)
	fmt.Fprintf(&b, "## Source\n\n%s / %s\n\n", t.SourceType, t.SourceEventID)
	b.WriteString("## Run summary\n\n")
	if excerpt == "" {
		// Should not happen because CheckSummary gates this, but stay safe.
		b.WriteString("_No summary provided._\n\n")
	} else {
		b.WriteString(excerpt)
		if !strings.HasSuffix(excerpt, "\n") {
			b.WriteString("\n")
		}
		if truncated {
			fmt.Fprintf(&b, "\n_Summary truncated at %d bytes; see full artifact below._\n", PRBodySummaryCap)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Full artifact: `%s` on `%s`.\n\n", workspace.SummaryPath, t.WorkBranch)
	b.WriteString("## Verification\n\nSee `.aiops/VERIFICATION.txt` and worker logs.\n\n")
	b.WriteString("## Risk\n\nHuman review required.\n")
	return b.String()
}

// truncateForPR returns the head of s up to cap bytes (UTF-8 safe at the
// boundary) along with a flag indicating whether truncation occurred.
func truncateForPR(s string, cap int) (string, bool) {
	if cap <= 0 || len(s) <= cap {
		return s, false
	}
	cut := cap
	// Walk back to a rune boundary so we never split a multi-byte sequence.
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut], true
}

// runSummaryDirective is the line we append to every rendered prompt so
// runners (codex/claude) know the worker will reject the task without a
// runner-produced .aiops/RUN_SUMMARY.md. The mock runner writes one
// directly; this directive is the contract for shell-based runners.
const runSummaryDirective = "\n\n---\n\n" +
	"**Required output:** before exiting, you MUST write " +
	"`.aiops/RUN_SUMMARY.md` describing what you changed, why, and how it " +
	"was verified. The task will fail if this file is missing, empty, or " +
	"contains only a placeholder."

// AppendRunSummaryDirective adds the RUN_SUMMARY.md contract to the rendered
// prompt unless it is already present (so workflow templates that already
// include the directive do not get a duplicate).
func AppendRunSummaryDirective(prompt string) string {
	if strings.Contains(prompt, "RUN_SUMMARY.md") {
		return prompt
	}
	return prompt + runSummaryDirective
}

func sampleSlice(items []string, max int) []string {
	if len(items) <= max {
		return items
	}
	return items[:max]
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
