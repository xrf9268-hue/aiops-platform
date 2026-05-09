package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/queue"
	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
	"github.com/xrf9268-hue/aiops-platform/internal/workspace"
)

// eventEmitter is the subset of the queue store the worker needs to record
// per-stage events. Defined here so unit tests can verify the worker emits
// the right kinds without standing up a database. The payload parameter is
// `any` so callers can pass either structured maps (Marshal'd by the store)
// or pre-serialized JSON []byte.
type eventEmitter interface {
	AddEvent(ctx context.Context, taskID, typ, msg string) error
	AddEventWithPayload(ctx context.Context, taskID, typ, msg string, payload any) error
}

// resolveWorkflow performs WORKFLOW.md discovery for a prepared workdir
// and emits the workflow_resolved event before any runner work begins.
// Returning the workflow_source string lets callers stamp it onto the
// runner_start payload as a quick-look field; the full provenance lives
// on the workflow_resolved event itself.
func resolveWorkflow(ctx context.Context, ev eventEmitter, taskID, workdir string) (*workflow.Workflow, string, error) {
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
	emit(ctx, ev, taskID, task.EventWorkflowResolved, "workflow resolved", payload)
	return wf, string(res.Source), nil
}

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "--print-config" {
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: worker --print-config <workdir>")
			os.Exit(2)
		}
		os.Exit(printConfig(os.Args[2], os.Stdout, os.Stderr))
	}
	ctx := context.Background()
	dsn := env("DATABASE_URL", "postgres://aiops:aiops@localhost:5432/aiops?sslmode=disable")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	store := queue.New(pool)

	for {
		t, err := store.Claim(ctx)
		if err != nil {
			log.Printf("claim error: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		if t == nil {
			time.Sleep(3 * time.Second)
			continue
		}
		cfg, err := runTask(ctx, store, *t)
		if err != nil {
			log.Printf("task %s failed: %v", t.ID, err)
			handleTaskFailure(ctx, store, *t, cfg, err)
			continue
		}
		_ = store.Complete(ctx, t.ID)
	}
}

// handleTaskFailure routes a task error to the right retry bucket.
//
// Runner timeouts use a dedicated budget (agent.max_timeout_retries) so a
// hung agent cannot consume the generic max_attempts reserved for
// verify/policy/transient infra failures. Non-timeout failures fall through
// to the legacy Fail path which is still gated by max_attempts. Failure
// artifacts (CHANGED_FILES.txt / RUN_SUMMARY.md / VERIFICATION.json) are
// written by the runTask code path on the way out, so this function is
// purely concerned with retry routing.
func handleTaskFailure(ctx context.Context, store *queue.Store, t task.Task, cfg workflow.Config, err error) {
	if runner.IsTimeout(err) {
		budget := cfg.Agent.MaxTimeoutRetriesValue()
		if _, ferr := store.FailTimeout(ctx, t.ID, err.Error(), budget); ferr != nil {
			log.Printf("task %s FailTimeout error: %v", t.ID, ferr)
		}
		return
	}
	_ = store.Fail(ctx, t.ID, err.Error())
}

// runTask executes a single task end-to-end and returns the resolved
// workflow config alongside any error so callers can route retries by
// failure class. The returned config carries the per-agent timeout and
// retry budgets so handleTaskFailure does not need to re-prepare the
// workspace just to read agent settings.
func runTask(ctx context.Context, ev eventEmitter, t task.Task) (workflow.Config, error) {
	mgr := workspace.New(env("WORKSPACE_ROOT", "/tmp/aiops-workspaces"))
	mgr.MirrorRoot = os.Getenv("AIOPS_MIRROR_ROOT")
	workdir, err := mgr.PrepareGitWorkspace(ctx, t)
	if err != nil {
		return workflow.Config{}, err
	}

	wf, workflowSource, err := resolveWorkflow(ctx, ev, t.ID, workdir)
	if err != nil {
		return workflow.Config{}, err
	}
	cfg := wf.Config
	if t.Model == "" || t.Model == "mock" {
		t.Model = cfg.Agent.Default
		if t.Model == "" {
			t.Model = "mock"
		}
	}

	prompt := workflow.Render(wf.PromptTemplate, map[string]string{
		"task.id":          t.ID,
		"task.title":       t.Title,
		"task.description": t.Description,
		"task.actor":       t.Actor,
		"repo.owner":       t.RepoOwner,
		"repo.name":        t.RepoName,
		"repo.branch":      t.BaseBranch,
	})
	prompt = appendRunSummaryDirective(prompt)
	if err := writeTaskFiles(workdir, t, prompt); err != nil {
		return cfg, err
	}

	r, err := runner.New(t.Model)
	if err != nil {
		return cfg, err
	}

	// Make sure the post-runner CheckSummary gate cannot pass on a stale
	// summary. PrepareGitWorkspace already nukes the workdir on a fresh
	// clone, but we still defend against the case where the base branch
	// *itself* has a committed .aiops/RUN_SUMMARY.md (left over from a prior
	// PR or seeded by hand). Deleting it here means CheckSummary can only
	// succeed when the runner produced a summary during this invocation.
	if err := workspace.ResetRunSummary(workdir); err != nil {
		return cfg, fmt.Errorf("reset run summary: %w", err)
	}

	if _, runErr := runRunnerWithTimeout(ctx, ev, r, runner.RunInput{Task: t, Workflow: *wf, Workdir: workdir, Prompt: prompt}, cfg.Agent.Timeout, workflowSource); runErr != nil {
		writeFailureArtifacts(ctx, workdir, nil, "runner failed: "+errSummary(runErr))
		return cfg, runErr
	}

	if err := workspace.EnforcePolicy(ctx, workdir, cfg); err != nil {
		recordPolicyViolation(ctx, ev, t.ID, err)
		writeFailureArtifacts(ctx, workdir, nil, "policy check failed: "+errSummary(err))
		return cfg, err
	}

	verifyDegraded, err := runVerifyPhase(ctx, ev, t.ID, workdir, cfg)
	if err != nil {
		return cfg, err
	}

	// Gate: the runner is required to write .aiops/RUN_SUMMARY.md describing
	// the change. If it is missing/empty/a placeholder we refuse to open a PR
	// and emit `summary_missing` + `failed_attempt` events so the human can
	// see exactly why the task did not progress. We do NOT fall back to a
	// worker-generated summary here on purpose: the artifact must come from
	// the runner so it reflects intent, not a synthesized recap. The failure
	// path below (writeFailureArtifacts) still seeds a worker summary for
	// post-mortem inspection — that is observed but never gates a PR.
	//
	// This gate runs BEFORE the secret scanner: a run with no real summary
	// is not allowed to push regardless of scanner outcome, and skipping the
	// scanner on this path keeps the failure attribution unambiguous.
	summary, status, checkErr := workspace.CheckSummary(workdir)
	if checkErr != nil {
		emit(ctx, ev, t.ID, "summary_missing", "read RUN_SUMMARY.md failed", map[string]any{
			"path":  workspace.SummaryPath,
			"error": errSummary(checkErr),
		})
		emit(ctx, ev, t.ID, task.EventFailedAttempt, "missing run summary artifact", map[string]any{
			"reason": "summary_unreadable",
			"path":   workspace.SummaryPath,
		})
		writeFailureArtifacts(ctx, workdir, nil, "RUN_SUMMARY.md unreadable: "+errSummary(checkErr))
		return cfg, fmt.Errorf("read %s: %w", workspace.SummaryPath, checkErr)
	}
	if status != workspace.SummaryOK {
		emit(ctx, ev, t.ID, "summary_missing", "runner did not produce RUN_SUMMARY.md", map[string]any{
			"path":   workspace.SummaryPath,
			"status": string(status),
		})
		emit(ctx, ev, t.ID, task.EventFailedAttempt, "missing run summary artifact", map[string]any{
			"reason": "summary_" + string(status),
			"path":   workspace.SummaryPath,
		})
		writeFailureArtifacts(ctx, workdir, nil, fmt.Sprintf("RUN_SUMMARY.md %s; runner must write %s before exiting.", status, workspace.SummaryPath))
		return cfg, fmt.Errorf("missing required artifact %s (%s)", workspace.SummaryPath, status)
	}

	// Run the optional pre-push secret scanner between the summary gate and
	// push so a branch carrying credential leaks never reaches the remote.
	// Failures flow through writeFailureArtifacts + the existing
	// failed_attempt path so operators can inspect what the scanner saw.
	if err := runSecretScan(ctx, ev, t.ID, workdir, wf.Config); err != nil {
		writeFailureArtifacts(ctx, workdir, nil, "secret scan blocked push: "+errSummary(err))
		return cfg, err
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
		emit(ctx, ev, t.ID, task.EventPush, "push failed", map[string]any{
			"branch":      t.WorkBranch,
			"duration_ms": time.Since(pushStart).Milliseconds(),
			"error":       errSummary(err),
		})
		return cfg, err
	}
	emit(ctx, ev, t.ID, task.EventPush, "push completed", map[string]any{
		"branch":         t.WorkBranch,
		"duration_ms":    time.Since(pushStart).Milliseconds(),
		"changed_files":  len(changed),
		"sample_changes": sampleSlice(changed, 10),
	})

	if verifyDegraded {
		cfg.PR.Draft = true
	}
	return cfg, createPR(ctx, ev, t, cfg, summary, verifyDegraded)
}

// runRunnerWithTimeout invokes the runner under a per-task timeout derived
// from agent.timeout. It emits structured task events
// (runner_start, runner_end, runner_timeout) so retry policy and observers
// can distinguish a clean exit from a kill due to deadline. The returned
// runner.Result is what runTask passes to runSummary on the success path;
// on failure it is zero-valued.
func runRunnerWithTimeout(ctx context.Context, ev eventEmitter, r runner.Runner, in runner.RunInput, timeout time.Duration, workflowSource string) (runner.Result, error) {
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}

	emit(ctx, ev, in.Task.ID, task.EventRunnerStart, "runner started", map[string]any{
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
			emit(ctx, ev, in.Task.ID, task.EventRunnerTimeout, te.Error(), map[string]any{
				"model":      in.Task.Model,
				"timeout_ms": te.Timeout.Milliseconds(),
				"elapsed_ms": te.Elapsed.Milliseconds(),
			})
			return runner.Result{}, runErr
		}
		emit(ctx, ev, in.Task.ID, task.EventRunnerEnd, "runner failed", map[string]any{
			"model":       in.Task.Model,
			"duration_ms": elapsed.Milliseconds(),
			"error":       errSummary(runErr),
			"ok":          false,
		})
		return runner.Result{}, runErr
	}

	endPayload := map[string]any{
		"model":       in.Task.Model,
		"duration_ms": elapsed.Milliseconds(),
		"ok":          true,
	}
	if res.Summary != "" {
		endPayload["summary"] = res.Summary
	}
	emit(ctx, ev, in.Task.ID, task.EventRunnerEnd, "runner completed", endPayload)
	return res, nil
}

// runVerifyPhase runs the configured verify commands, persists the
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
func runVerifyPhase(ctx context.Context, ev eventEmitter, taskID, workdir string, cfg workflow.Config) (bool, error) {
	emit(ctx, ev, taskID, task.EventVerifyStart, "verify started", map[string]any{
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
		"commands":      summarizeVerifyResults(results),
		"failed_count":  countVerifyFailures(results),
		"allow_failure": cfg.Verify.AllowFailure,
	}
	if verifyErr == nil {
		payload["status"] = "ok"
		emit(ctx, ev, taskID, task.EventVerifyEnd, "verify completed", payload)
		return false, nil
	}
	payload["error"] = errSummary(verifyErr)
	if cfg.Verify.AllowFailure {
		payload["status"] = "failed_allowed"
		emit(ctx, ev, taskID, task.EventVerifyEnd, "verify failed (investigation mode)", payload)
		return true, nil
	}
	payload["status"] = "failed"
	emit(ctx, ev, taskID, task.EventVerifyEnd, "verify failed", payload)
	writeFailureArtifacts(ctx, workdir, results, "verify failed: "+errSummary(verifyErr))
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

// secretScanFn is the indirection used to swap the real workspace scanner
// for a stub in tests. It mirrors workspace.RunSecretScan's signature.
type secretScanFn func(ctx context.Context, workdir string, cfg workflow.SecretScanConfig) workspace.SecretScanResult

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
func runSecretScan(ctx context.Context, ev eventEmitter, taskID string, workdir string, cfg workflow.Config) error {
	return runSecretScanWith(ctx, ev, taskID, workdir, cfg, workspace.RunSecretScan)
}

func runSecretScanWith(ctx context.Context, ev eventEmitter, taskID string, workdir string, cfg workflow.Config, scan secretScanFn) error {
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
func recordPolicyViolation(ctx context.Context, ev eventEmitter, taskID string, err error) {
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

// prClient is the subset of gitea.Client the worker needs for PR handoff.
// Defined as an interface so tests can inject a fake implementation without
// standing up a real Gitea HTTP server.
type prClient interface {
	FindOpenPullRequest(ctx context.Context, in gitea.FindOpenPullRequestInput) (*gitea.PullRequest, error)
	CreatePullRequest(ctx context.Context, in gitea.CreatePullRequestInput) (*gitea.PullRequest, error)
}

func createPR(ctx context.Context, ev eventEmitter, t task.Task, cfg workflow.Config, summary string, verifyDegraded bool) error {
	client := gitea.Client{BaseURL: os.Getenv("GITEA_BASE_URL"), Token: os.Getenv("GITEA_TOKEN")}
	return createPRWith(ctx, ev, t, cfg, summary, verifyDegraded, client)
}

// createPRWith performs the PR handoff using the supplied client. It first
// looks for an existing open PR for the work branch and reuses it instead of
// asking Gitea to create a duplicate — this is what makes retries safe to
// re-enter after a previous attempt already produced a PR. A list error is
// logged and falls through to the create path so a transient Gitea hiccup
// during the lookup does not block the task; the create call itself remains
// the source of truth for surfacing real failures.
func createPRWith(ctx context.Context, ev eventEmitter, t task.Task, cfg workflow.Config, summary string, verifyDegraded bool, client prClient) error {
	if existing, err := client.FindOpenPullRequest(ctx, gitea.FindOpenPullRequestInput{
		Owner: t.RepoOwner, Repo: t.RepoName, Head: t.WorkBranch,
	}); err != nil {
		log.Printf("task %s: list open PRs failed: %v", t.ID, err)
	} else if existing != nil {
		emit(ctx, ev, t.ID, task.EventPRReused, "pr reused", map[string]any{
			"number":   existing.Number,
			"html_url": existing.HTMLURL,
			"title":    existing.Title,
		})
		log.Printf("task %s reused PR #%d %s", t.ID, existing.Number, existing.HTMLURL)
		return nil
	}
	body := buildPRBody(t, summary, verifyDegraded)
	pr, err := client.CreatePullRequest(ctx, gitea.CreatePullRequestInput{Owner: t.RepoOwner, Repo: t.RepoName, Title: "chore(ai): " + t.Title, Body: body, Head: t.WorkBranch, Base: t.BaseBranch, Draft: cfg.PR.Draft})
	if err != nil {
		emit(ctx, ev, t.ID, task.EventPRCreated, "pr creation failed", map[string]any{"error": errSummary(err)})
		return err
	}
	emit(ctx, ev, t.ID, task.EventPRCreated, "pr created", map[string]any{
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

// writeFailureArtifacts persists what we know on the failure path so failed
// tasks can be inspected after the fact via the workspace tree.
func writeFailureArtifacts(ctx context.Context, workdir string, verifyResults []workspace.VerifyResult, summary string) {
	if changed, err := workspace.AllChangedFiles(ctx, workdir); err == nil {
		_ = workspace.WriteChangedFiles(workdir, changed)
	}
	if len(verifyResults) > 0 {
		_ = workspace.WriteVerification(workdir, verifyResults)
	}
	_ = workspace.WriteSummary(workdir, summary+"\n")
}

func emit(ctx context.Context, ev eventEmitter, taskID, kind, msg string, payload any) {
	if ev == nil {
		return
	}
	if err := ev.AddEventWithPayload(ctx, taskID, kind, msg, payload); err != nil {
		log.Printf("task %s: emit %s event: %v", taskID, kind, err)
	}
}

func errSummary(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > 500 {
		msg = msg[:500] + "..."
	}
	return msg
}

func summarizeVerifyResults(results []workspace.VerifyResult) []map[string]any {
	out := make([]map[string]any, 0, len(results))
	for _, r := range results {
		entry := map[string]any{
			"command":     r.Command,
			"exit_code":   r.ExitCode,
			"duration_ms": r.Duration.Milliseconds(),
		}
		if r.Err != nil {
			entry["error"] = errSummary(r.Err)
		}
		out = append(out, entry)
	}
	return out
}

// prBodySummaryCap bounds how much of the runner-produced RUN_SUMMARY.md
// we inline into the PR body. 8 KiB is well under Gitea's body limit and
// keeps the rendered review page navigable; the full file is always
// available via the .aiops/RUN_SUMMARY.md path linked beneath the excerpt.
const prBodySummaryCap = 8 << 10 // 8 KiB

// buildPRBody renders the pull request body with the runner-produced
// RUN_SUMMARY.md content inlined (truncated to prBodySummaryCap) and a link
// to the full artifact path on the work branch. Callers must pass a summary
// that has already been validated by workspace.CheckSummary. verifyDegraded
// indicates the verify phase failed with allow_failure=true; Task 4 will use
// this flag to render the investigation-mode banner.
func buildPRBody(t task.Task, summary string, verifyDegraded bool) string {
	excerpt, truncated := truncateForPR(summary, prBodySummaryCap)
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
			fmt.Fprintf(&b, "\n_Summary truncated at %d bytes; see full artifact below._\n", prBodySummaryCap)
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

// appendRunSummaryDirective adds the RUN_SUMMARY.md contract to the rendered
// prompt unless it is already present (so workflow templates that already
// include the directive do not get a duplicate).
func appendRunSummaryDirective(prompt string) string {
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

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
