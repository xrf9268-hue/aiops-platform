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
// the right kinds without standing up a database.
type eventEmitter interface {
	AddEvent(ctx context.Context, taskID, typ, msg string) error
	AddEventWithPayload(ctx context.Context, taskID, typ, msg string, payload any) error
}

func main() {
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
		if err := runTask(ctx, store, *t); err != nil {
			log.Printf("task %s failed: %v", t.ID, err)
			_ = store.Fail(ctx, t.ID, err.Error())
			continue
		}
		_ = store.Complete(ctx, t.ID)
	}
}

func runTask(ctx context.Context, ev eventEmitter, t task.Task) error {
	mgr := workspace.New(env("WORKSPACE_ROOT", "/tmp/aiops-workspaces"))
	mgr.MirrorRoot = os.Getenv("AIOPS_MIRROR_ROOT")
	workdir, err := mgr.PrepareGitWorkspace(ctx, t)
	if err != nil {
		return err
	}

	wf, err := workflow.LoadOptional(workdir + "/WORKFLOW.md")
	if err != nil {
		return err
	}
	if t.Model == "" || t.Model == "mock" {
		t.Model = wf.Config.Agent.Default
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
	if err := writeTaskFiles(workdir, t, prompt); err != nil {
		return err
	}

	r, err := runner.New(t.Model)
	if err != nil {
		return err
	}

	emit(ctx, ev, t.ID, task.EventRunnerStart, "runner started", map[string]any{"model": t.Model})
	runnerStart := time.Now()
	res, runErr := r.Run(ctx, runner.RunInput{Task: t, Workflow: *wf, Workdir: workdir, Prompt: prompt})
	runnerDur := time.Since(runnerStart)
	runnerPayload := map[string]any{"model": t.Model, "duration_ms": runnerDur.Milliseconds()}
	if runErr != nil {
		runnerPayload["error"] = errSummary(runErr)
		emit(ctx, ev, t.ID, task.EventRunnerEnd, "runner failed", runnerPayload)
		writeFailureArtifacts(ctx, workdir, nil, "runner failed: "+errSummary(runErr))
		return runErr
	}
	if res.Summary != "" {
		runnerPayload["summary"] = res.Summary
	}
	emit(ctx, ev, t.ID, task.EventRunnerEnd, "runner completed", runnerPayload)

	if err := workspace.EnforcePolicy(ctx, workdir, wf.Config); err != nil {
		recordPolicyViolation(ctx, ev, t.ID, err)
		writeFailureArtifacts(ctx, workdir, nil, "policy check failed: "+errSummary(err))
		return err
	}

	emit(ctx, ev, t.ID, task.EventVerifyStart, "verify started", map[string]any{"commands": wf.Config.Verify.Commands})
	verifyStart := time.Now()
	verifyResults, verifyErr := workspace.RunVerify(ctx, workdir, wf.Config)
	verifyDur := time.Since(verifyStart)
	if writeErr := workspace.WriteVerification(workdir, verifyResults); writeErr != nil {
		log.Printf("task %s: write verification artifact: %v", t.ID, writeErr)
	}
	verifyPayload := map[string]any{
		"duration_ms": verifyDur.Milliseconds(),
		"commands":    summarizeVerifyResults(verifyResults),
	}
	if verifyErr != nil {
		verifyPayload["error"] = errSummary(verifyErr)
		emit(ctx, ev, t.ID, task.EventVerifyEnd, "verify failed", verifyPayload)
		writeFailureArtifacts(ctx, workdir, verifyResults, "verify failed: "+errSummary(verifyErr))
		return verifyErr
	}
	emit(ctx, ev, t.ID, task.EventVerifyEnd, "verify completed", verifyPayload)

	// Run the optional pre-push secret scanner between verify and push so a
	// branch carrying credential leaks never reaches the remote. Failures
	// flow through writeFailureArtifacts + the existing failed_attempt path
	// so operators can inspect what the scanner saw.
	if err := runSecretScan(ctx, ev, t.ID, workdir, wf.Config); err != nil {
		writeFailureArtifacts(ctx, workdir, verifyResults, "secret scan blocked push: "+errSummary(err))
		return err
	}

	// Snapshot the changed files AFTER all run artifacts have been written so
	// that CHANGED_FILES.txt, RUN_SUMMARY.md, and the success-path event
	// payload reflect what is actually about to be committed (artifacts
	// included). We seed the artifacts with empty stubs so they are present in
	// the working tree when `git status --porcelain` runs, then rewrite them
	// with the final snapshot contents.
	if err := workspace.WriteChangedFiles(workdir, nil); err != nil {
		log.Printf("task %s: seed changed files artifact: %v", t.ID, err)
	}
	if err := workspace.WriteSummary(workdir, ""); err != nil {
		log.Printf("task %s: seed run summary artifact: %v", t.ID, err)
	}
	changed, _ := workspace.AllChangedFiles(ctx, workdir)
	if err := workspace.WriteChangedFiles(workdir, changed); err != nil {
		log.Printf("task %s: write changed files artifact: %v", t.ID, err)
	}
	if err := workspace.WriteSummary(workdir, runSummary(t, res, changed, verifyResults)); err != nil {
		log.Printf("task %s: write run summary artifact: %v", t.ID, err)
	}

	pushStart := time.Now()
	if err := workspace.CommitAndPush(ctx, workdir, t.Title, t.WorkBranch); err != nil {
		emit(ctx, ev, t.ID, task.EventPush, "push failed", map[string]any{
			"branch":      t.WorkBranch,
			"duration_ms": time.Since(pushStart).Milliseconds(),
			"error":       errSummary(err),
		})
		return err
	}
	emit(ctx, ev, t.ID, task.EventPush, "push completed", map[string]any{
		"branch":         t.WorkBranch,
		"duration_ms":    time.Since(pushStart).Milliseconds(),
		"changed_files":  len(changed),
		"sample_changes": sampleSlice(changed, 10),
	})

	return createPR(ctx, ev, t, wf.Config)
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

func createPR(ctx context.Context, ev eventEmitter, t task.Task, cfg workflow.Config) error {
	_ = cfg
	client := gitea.Client{BaseURL: os.Getenv("GITEA_BASE_URL"), Token: os.Getenv("GITEA_TOKEN")}
	body := fmt.Sprintf("## AI Task\n\nTask ID: `%s`\n\n## Source\n\n%s / %s\n\n## Changes\n\nGenerated by aiops-platform Symphony-style worker.\n\n## Verification\n\nSee workflow verify commands and worker logs.\n\n## Risk\n\nHuman review required.\n", t.ID, t.SourceType, t.SourceEventID)
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

func runSummary(t task.Task, res runner.Result, changed []string, verifyResults []workspace.VerifyResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Run summary for %s\n\n", t.ID)
	fmt.Fprintf(&b, "- Title: %s\n", t.Title)
	fmt.Fprintf(&b, "- Actor: %s\n", t.Actor)
	fmt.Fprintf(&b, "- Model: %s\n", t.Model)
	if res.Summary != "" {
		fmt.Fprintf(&b, "- Runner: %s\n", res.Summary)
	}
	fmt.Fprintf(&b, "- Branch: %s -> %s\n", t.BaseBranch, t.WorkBranch)
	fmt.Fprintf(&b, "- Changed files: %d\n", len(changed))
	if len(changed) > 0 {
		b.WriteString("\n## Changed files\n\n")
		for _, f := range changed {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	if len(verifyResults) > 0 {
		b.WriteString("\n## Verification\n\n")
		for _, r := range verifyResults {
			status := "ok"
			if r.Err != nil {
				status = "failed"
			}
			fmt.Fprintf(&b, "- `%s` (%s, %dms)\n", r.Command, status, r.Duration.Milliseconds())
		}
	}
	return b.String()
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
