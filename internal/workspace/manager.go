package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/policy"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// HookName identifies one of the SPEC-defined workspace lifecycle hooks.
type HookName string

const (
	HookAfterCreate  HookName = "after_create"
	HookBeforeRun    HookName = "before_run"
	HookAfterRun     HookName = "after_run"
	HookBeforeRemove HookName = "before_remove"
)

// HookResult captures one workspace hook command execution for task events and
// caller-side failure policy decisions.
type HookResult struct {
	Name      HookName
	Command   string
	ExitCode  int
	Output    string
	Truncated bool
	Duration  time.Duration
	Err       error
}

// HookError reports a failed workspace hook while preserving every command
// result captured before the failure.
type HookError struct {
	Name    HookName
	Results []HookResult
	Err     error
}

func (e *HookError) Error() string {
	if e == nil || e.Err == nil {
		return "workspace hook failed"
	}
	return fmt.Sprintf("workspace hook %s failed: %v", e.Name, e.Err)
}

func (e *HookError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// VerifyOutputCap bounds how many bytes of combined stdout+stderr we keep in
// memory per verify command. Verbose verify steps (e.g. `go test -v ./...`)
// can emit tens of MiB; capping prevents unbounded RAM use and the duplicate
// allocation that happens when the output is later written as an artifact.
const VerifyOutputCap = 1 << 20 // 1 MiB

const maxSanitizedLength = 120

// VerifyResult captures the outcome of running a workflow verify command.
// Output contains the combined stdout+stderr so it can be persisted as a
// run artifact even when the command fails. When the captured output exceeds
// VerifyOutputCap, Output is truncated to the cap and Truncated is set so
// callers can surface the fact in artifacts and event payloads.
type VerifyResult struct {
	Command   string
	ExitCode  int
	Output    string
	Truncated bool
	Duration  time.Duration
	Err       error
}

// cappedBuffer is an io.Writer that buffers up to Cap bytes and silently
// drops the rest while remembering how many bytes were dropped. It avoids
// holding the entire output of a verbose verify command in memory.
type cappedBuffer struct {
	Cap     int
	mu      sync.Mutex
	buf     bytes.Buffer
	dropped int64
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	remaining := c.Cap - c.buf.Len()
	if remaining > 0 {
		take := len(p)
		if take > remaining {
			take = remaining
		}
		c.buf.Write(p[:take])
		if take < len(p) {
			c.dropped += int64(len(p) - take)
		}
	} else {
		c.dropped += int64(len(p))
	}
	// Always report the full length so the producer treats the write as
	// successful and does not block trying to drain io.ErrShortWrite.
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.buf.String()
}

func (c *cappedBuffer) Truncated() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.dropped > 0
}

func (c *cappedBuffer) Dropped() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.dropped
}

// Compile-time check that cappedBuffer satisfies io.Writer.
var _ io.Writer = (*cappedBuffer)(nil)

// Manager owns per-issue workspace creation, layered on top of a process-wide
// bare mirror cache. Each issue gets its own detached worktree under Root so
// unrelated issues cannot stomp on each other, while the heavy network IO
// (object download) happens once per repo in the mirror cache and is
// reused on every subsequent task.
type Manager struct {
	// Root is the per-issue worktree root, typically WORKSPACE_ROOT.
	Root string
	// MirrorRoot overrides the bare mirror cache location. When empty,
	// MirrorRoot() resolves it from os.UserCacheDir or os.TempDir.
	MirrorRoot string
}

func New(root string) *Manager { return &Manager{Root: root} }

func (m *Manager) PathFor(t task.Task) string {
	return filepath.Join(m.Root, SanitizeComponent(t.RepoOwner), SanitizeComponent(t.RepoName), issueWorkspaceKey(t))
}

func issueWorkspaceKey(t task.Task) string {
	sourceType := strings.TrimSpace(t.SourceType)
	sourceEventID := strings.TrimSpace(t.SourceEventID)
	if sourceType != "" && sourceEventID != "" {
		return filepath.Join(SanitizeComponent(sourceType), SanitizeComponent(sourceEventID))
	}
	return SanitizeComponent(t.ID)
}

// PrepareGitWorkspace materialises a per-issue workspace as a worktree off
// the cached bare mirror for t.CloneURL. The worktree lives at PathFor(t)
// on the work branch, with origin set back to the upstream URL so
// `git push origin <branch>` works without further setup.
//
// On every call we refresh the mirror first so the worktree starts from
// up-to-date refs. The returned createdNow flag reports whether this call
// first touched the issue workspace path:
//
//   - createdNow=true (first touch or recovery from a corrupted leftover):
//     the worktree is added fresh from origin/<base>. The worker uses this
//     edge to fire the `after_create` hook exactly once per workspace,
//     matching SPEC §9.4.
//   - createdNow=false (reuse path, SPEC §9.1): the existing worktree's
//     tracked state is reset to origin/<base> via `git checkout --force -B`
//     so the runner starts from a clean base, but untracked artifacts
//     (cached deps, build outputs, .aiops policy feedback) survive across
//     runs.
func (m *Manager) PrepareGitWorkspace(ctx context.Context, t task.Task) (string, bool, error) {
	workdir := m.PathFor(t)
	if err := os.MkdirAll(filepath.Dir(workdir), 0o755); err != nil {
		return "", false, err
	}
	info, statErr := os.Stat(workdir)
	if statErr != nil && !os.IsNotExist(statErr) {
		return "", false, statErr
	}
	workdirExists := statErr == nil && info.IsDir()

	mirror := mirrorPathFor(MirrorRoot(m.MirrorRoot), t.CloneURL)
	unlock, err := acquireMirrorLock(mirror)
	if err != nil {
		return "", false, err
	}
	defer unlock()

	mirror, err = m.ensureMirrorLocked(ctx, t.CloneURL, mirror)
	if err != nil {
		return "", false, err
	}
	// Resolve the base ref via `origin/<base>` because the bare cache
	// stores upstream branches as remote-tracking refs (see EnsureMirror);
	// falling back to the bare name covers `file://` test fixtures where
	// the upstream is the same on-disk repo.
	startRef := "origin/" + t.BaseBranch
	if err := runQuiet(ctx, mirror, "git", "rev-parse", "--verify", startRef); err != nil {
		startRef = t.BaseBranch
	}

	// A workdir that exists but isn't a valid, mirror-linked git worktree
	// must not pin the worker into a bad state. Three classes of "looks
	// reusable but isn't" we explicitly reject:
	//
	//  1. The path is a symlink. An attacker who can plant a symlink at
	//     the workspace path could otherwise redirect the reuse-path
	//     `git reset` / `git checkout -B` into a repository outside the
	//     workspace root entirely. `os.Lstat` (vs the earlier `os.Stat`
	//     that follows the link) catches this.
	//  2. The path holds an independent git repository — for example, a
	//     prior agent run that rewrote its workspace as a fresh `git
	//     init` would still pass `git rev-parse --git-dir`. We verify
	//     `git rev-parse --git-common-dir` resolves to *our* mirror so
	//     unrelated repos can't masquerade as a reusable workspace.
	//  3. The path is the worktree of a different mirror (different
	//     clone URL routed to the same key, mirror was wiped and
	//     recreated between prepares, etc.). The git-common-dir check
	//     above also covers this.
	//
	// On any rejection we fall through to the nuke-and-recreate path
	// (`os.RemoveAll` removes the symlink itself, not its target) and
	// report `createdNow=true` so the worker fires `after_create` on
	// the recovery run.
	// foreignCommonDir is non-empty only when the gate ran the
	// `--git-common-dir` probe successfully and the result pointed at a
	// different mirror (e.g. `t.CloneURL`'s host changed between prepares
	// and `mirrorPathFor` routed it to a new mirror). We use it later to
	// prune the orphaned worktree admin entry from the foreign mirror
	// before recreating against the new one — otherwise the entry would
	// linger until `gc.worktreePruneExpire` (3 months default) expires it.
	var foreignCommonDir string
	reusable := false
	if workdirExists {
		lstatInfo, lstatErr := os.Lstat(workdir)
		if lstatErr == nil && lstatInfo.Mode()&os.ModeSymlink == 0 {
			if err := runQuiet(ctx, workdir, "git", "rev-parse", "--git-dir"); err == nil {
				// Silence the probe's stderr: on a broken-but-existing
				// `.git` (mid-corruption, partial `worktree add` crash,
				// race with `os.RemoveAll`) git prints `fatal: not a git
				// repository` while we're about to fall through to the
				// recreate path anyway. Without io.Discard that fatal line
				// pollutes the worker's inherited fd 2 and obscures the
				// recovery the gate is about to perform.
				probe := exec.CommandContext(ctx, "git", "-C", workdir, "rev-parse", "--git-common-dir")
				probe.Stderr = io.Discard
				cdOut, cdErr := probe.Output()
				if cdErr == nil {
					common := strings.TrimSpace(string(cdOut))
					if sameRealPath(common, workdir, mirror) {
						reusable = true
					} else if common != "" {
						foreignCommonDir = common
						if !filepath.IsAbs(foreignCommonDir) {
							foreignCommonDir = filepath.Join(workdir, foreignCommonDir)
						}
					}
				}
			}
		}
	}

	if reusable {
		// SPEC §9.1: workspaces are reused across runs for the same issue.
		// We clear the index first, then snap the work branch to the
		// refreshed base:
		//   - `git reset --quiet HEAD -- .` drops any intent-to-add
		//     entries the previous run's `EnforcePolicy` diffstat
		//     (`git add --intent-to-add --all` in Diffstat) left in the
		//     index. Without this step `git checkout` below would treat
		//     those untracked-but-staged files as removable from the
		//     working tree because they aren't present in startRef, and
		//     cached deps / hook artifacts would silently vanish on
		//     reuse.
		//   - `git checkout --force --no-track -B` then rebases the work
		//     branch to startRef and updates the working tree:
		//     `--force` discards tracked-file modifications, `--no-track`
		//     keeps the work branch from tracking the base branch (the
		//     SPEC contract honored by the create path's `worktree add
		//     --no-track`), and `-B` makes the rebase idempotent.
		// Untracked files (cached deps, build outputs, .aiops policy
		// feedback) survive intact.
		if err := runQuiet(ctx, workdir, "git", "reset", "--quiet", "HEAD", "--", "."); err != nil {
			return "", false, fmt.Errorf("worktree index reset: %w", err)
		}
		if err := run(ctx, workdir, "git", "checkout", "--force", "--no-track", "-B", t.WorkBranch, startRef); err != nil {
			return "", false, fmt.Errorf("worktree checkout: %w", err)
		}
		return workdir, false, nil
	}

	// First touch (or recovery from a corrupted leftover). If the gate
	// just rejected a workspace that linked to a *different* mirror,
	// prune the orphaned admin entry off that mirror first so its
	// `worktrees/` directory stays in sync with what's actually on disk.
	// Without this, the foreign mirror keeps a dead worktree record until
	// `gc.worktreePruneExpire` (3 months default) expires it; harmless in
	// the common case, confusing for an operator inspecting
	// `git worktree list` and a latent collision risk if the same
	// workspace path is ever re-targeted to the old mirror.
	if foreignCommonDir != "" {
		_ = runQuiet(ctx, foreignCommonDir, "git", "worktree", "prune")
	}
	// Drop any stale worktree entry the mirror still tracks for this path
	// before asking it to add a new one. Failures are ignored and stderr
	// silenced because the common case ("no such worktree") prints a scary
	// fatal line that obscures real worker logs.
	_ = runQuiet(ctx, mirror, "git", "worktree", "remove", "--force", workdir)
	_ = os.RemoveAll(workdir)
	if err := runQuiet(ctx, mirror, "git", "worktree", "prune"); err != nil {
		return "", false, fmt.Errorf("worktree prune: %w", err)
	}
	// Using -B makes the operation idempotent if a previous attempt left
	// an in-mirror branch behind. The new worktree inherits remote config
	// from the linked bare mirror, where `EnsureMirror`'s `git clone --bare`
	// already recorded origin = t.CloneURL (see internal/workspace/mirror.go);
	// we deliberately do not re-`git remote set-url` from inside the
	// worktree because there is no per-worktree config and the write would
	// land back in the shared mirror config redundantly.
	if err := run(ctx, mirror, "git", "worktree", "add", "--no-track", "-B", t.WorkBranch, workdir, startRef); err != nil {
		return "", false, fmt.Errorf("worktree add: %w", err)
	}
	return workdir, true, nil
}

// RunWorkspaceHook executes the configured shell commands for a lifecycle hook
// in workdir, in order, using the shared workspace hook timeout. Hook
// subprocesses receive an explicit env built from the workspace baseline
// allowlist plus envPassthrough — secrets in the worker's environment that
// are not in either list are dropped. See
// docs/design/hook-verify-env-allowlist.md (#227).
func RunWorkspaceHook(ctx context.Context, workdir string, name HookName, hook workflow.WorkspaceHook, timeoutMs int, envPassthrough []string) ([]HookResult, error) {
	timeoutMs = EffectiveWorkspaceHookTimeoutMs(timeoutMs)
	env := subprocessEnv(envPassthrough)
	results := make([]HookResult, 0, len(hook.Commands))
	for _, raw := range hook.Commands {
		command := strings.TrimSpace(raw)
		if command == "" {
			continue
		}
		res := runWorkspaceHookCommand(ctx, workdir, name, command, timeoutMs, env)
		results = append(results, res)
		if res.Err != nil {
			return results, &HookError{Name: name, Results: results, Err: res.Err}
		}
	}
	return results, nil
}

func EffectiveWorkspaceHookTimeoutMs(timeoutMs int) int {
	if timeoutMs > 0 {
		return timeoutMs
	}
	return workflow.DefaultConfig().Hooks.TimeoutMs
}

func workspaceHookWaitDelay(timeoutMs int) time.Duration {
	if timeoutMs <= 0 {
		return 100 * time.Millisecond
	}
	grace := time.Duration(math.Ceil(float64(timeoutMs)/10)) * time.Millisecond
	if grace < 100*time.Millisecond {
		return 100 * time.Millisecond
	}
	if grace > time.Second {
		return time.Second
	}
	return grace
}

func runWorkspaceHookCommand(ctx context.Context, workdir string, name HookName, command string, timeoutMs int, env []string) HookResult {
	start := time.Now()
	runCtx := ctx
	cancel := func() {}
	if timeoutMs > 0 {
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	}
	defer cancel()
	waitDelay := workspaceHookWaitDelay(timeoutMs)

	cmd := exec.CommandContext(runCtx, "sh", "-lc", command)
	cmd.Dir = workdir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = waitDelay
	var out cappedBuffer
	out.Cap = VerifyOutputCap
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		return HookResult{Name: name, Command: command, ExitCode: exitCode(err), Output: out.String(), Truncated: out.Truncated(), Duration: time.Since(start), Err: err}
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	var err error
	select {
	case err = <-done:
	case <-runCtx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case err = <-done:
		case <-time.After(waitDelay):
			err = fmt.Errorf("hook wait exceeded cleanup grace after timeout: %w", runCtx.Err())
		}
	}
	res := HookResult{Name: name, Command: command, ExitCode: exitCode(err), Output: out.String(), Truncated: out.Truncated(), Duration: time.Since(start), Err: err}
	if runCtx.Err() == context.DeadlineExceeded {
		res.Err = fmt.Errorf("hook timed out after %dms: %w", timeoutMs, runCtx.Err())
		res.ExitCode = -1
	}
	return res
}

func WritePrompt(workdir string, prompt string) error {
	dir := filepath.Join(workdir, ".aiops")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "PROMPT.md"), []byte(prompt), 0o644)
}

// RunVerify executes the workflow verify commands in order and returns
// one VerifyResult per non-empty command. Unlike the original
// short-circuit semantics, it does not stop on the first failing
// command: the AI workflow is more efficient when a single rework cycle
// can address every reported failure. Per-command failure detail stays
// on VerifyResult.Err and ExitCode.
//
// When wf.Verify.Timeout > 0 the entire phase runs under a derived
// deadline. If the deadline elapses, the in-flight command is killed
// via context cancellation and remaining commands are skipped (no
// result is recorded for the skipped tail). The returned aggregate
// error is non-nil iff at least one command failed or the phase
// deadline was exceeded; callers inspect individual results to see
// which.
func RunVerify(ctx context.Context, workdir string, wf workflow.Config) ([]VerifyResult, error) {
	runCtx := ctx
	if wf.Verify.Timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, wf.Verify.Timeout)
		defer cancel()
	}
	env := subprocessEnv(wf.Verify.EnvPassthrough)

	var (
		results  []VerifyResult
		failures int
	)
	for _, command := range wf.Verify.Commands {
		if strings.TrimSpace(command) == "" {
			continue
		}
		// Stop launching new commands once the phase deadline (or the
		// parent context) has elapsed. The previous command's cmd.Run
		// already returned with a context-related error; we don't need
		// to wait for anything.
		if runCtx.Err() != nil {
			break
		}
		start := time.Now()
		buf := &cappedBuffer{Cap: VerifyOutputCap}
		cmd := exec.CommandContext(runCtx, "sh", "-lc", command)
		cmd.Dir = workdir
		cmd.Env = env
		cmd.Stdout = buf
		cmd.Stderr = buf
		// Run each verify command in its own process group so a
		// shell-spawned child (e.g. `sleep 5`) doesn't outlive
		// cancellation as an orphan. Without Setpgid, exec's default
		// Cancel hook only SIGKILLs the shell pid; the child keeps the
		// stdout pipe open and cmd.Wait() stalls until the child exits
		// on its own — turning a 200ms verify timeout into a 5s wait.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			// Negative pid sends to the whole process group. ESRCH
			// is benign: the process already exited before we got
			// here. Anything else is logged but not propagated; the
			// goroutine that waits on the context owns the return
			// value of Run, not us.
			if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				return err
			}
			return nil
		}
		// Final safety net for the rare case where a wedged
		// grandchild keeps the pipe open even after the group SIGKILL
		// (e.g. a process with PR_SET_PDEATHSIG suppressed by a
		// container runtime). After this delay, Go forcibly closes
		// the pipes so cmd.Wait returns and we unblock.
		cmd.WaitDelay = 2 * time.Second
		runErr := cmd.Run()
		// Nil-guard ProcessState: when the context expires between the
		// pre-loop check and exec.Cmd.Start(), Go returns an error without
		// starting the process, leaving ProcessState nil. Calling ExitCode()
		// on a nil pointer would panic; -1 is Go's documented "no exit code"
		// sentinel and matches the behaviour callers already expect for
		// context-cancelled commands.
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		res := VerifyResult{
			Command:   command,
			Output:    buf.String(),
			Truncated: buf.Truncated(),
			Duration:  time.Since(start),
			ExitCode:  exitCode,
		}
		if runErr != nil {
			res.Err = runErr
			failures++
		}
		results = append(results, res)
	}

	if ctx.Err() != nil {
		return results, ctx.Err()
	}
	if runCtx.Err() == context.DeadlineExceeded {
		return results, fmt.Errorf("verify phase exceeded timeout %s after %d command(s)", wf.Verify.Timeout, len(results))
	}
	if failures > 0 {
		return results, fmt.Errorf("verify: %d of %d command(s) failed", failures, len(results))
	}
	return results, nil
}

// WriteSummary writes the per-run summary to .aiops/RUN_SUMMARY.md so it can
// be committed alongside the change and inspected on failure paths.
func WriteSummary(workdir, summary string) error {
	return writeAiopsFile(workdir, "RUN_SUMMARY.md", summary)
}

// SummaryPath is the location where runners are required to write their
// per-run summary so the worker can include it in the PR body.
const SummaryPath = ".aiops/RUN_SUMMARY.md"

// ResetRunSummary deletes any pre-existing .aiops/RUN_SUMMARY.md from the
// workdir before the runner starts so the post-runner CheckSummary gate can
// only pass when the runner produced a summary for *this* run. Without this
// reset, a stale summary committed to the repo (or left over from a previous
// attempt in the same workspace) would silently satisfy the gate and let the
// worker open a PR with content that does not describe the current change.
//
// The function is idempotent: a missing file is not an error. Any other I/O
// error (permission, etc.) is returned so the caller can surface it before
// running the runner.
func ResetRunSummary(workdir string) error {
	path := filepath.Join(workdir, SummaryPath)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reset %s: %w", SummaryPath, err)
	}
	return nil
}

// summaryPlaceholderHints are substrings whose presence (in a short file)
// indicates the runner did not actually fill in a real summary. We keep the
// list intentionally narrow so legitimate summaries are not rejected.
var summaryPlaceholderHints = []string{
	"TODO",
	"PLACEHOLDER",
	"<fill in",
	"<TBD>",
}

// ReadSummary returns the trimmed contents of .aiops/RUN_SUMMARY.md.
// It returns an empty string and nil error when the file does not exist so
// callers can distinguish "missing" from "unreadable". Any other I/O error
// is propagated.
func ReadSummary(workdir string) (string, error) {
	path := filepath.Join(workdir, SummaryPath)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// SummaryStatus describes why a candidate RUN_SUMMARY.md was rejected. It is
// "ok" when the file is present and looks like a real summary.
type SummaryStatus string

const (
	SummaryOK          SummaryStatus = "ok"
	SummaryMissing     SummaryStatus = "missing"
	SummaryEmpty       SummaryStatus = "empty"
	SummaryPlaceholder SummaryStatus = "placeholder"
)

// CheckSummary inspects the current workdir for a runner-produced
// .aiops/RUN_SUMMARY.md. It returns the trimmed contents (when present) and
// a status describing whether the file satisfies the artifact contract:
//
//   - SummaryOK         file exists, has substantive content
//   - SummaryMissing    file does not exist on disk
//   - SummaryEmpty      file exists but is empty / whitespace only
//   - SummaryPlaceholder file is suspiciously short and contains a TODO/TBD
//     style marker, suggesting the runner wrote a stub instead of an actual
//     summary
//
// The threshold for "substantive" is intentionally permissive (32 chars):
// the goal is to catch obvious skips like "TODO\n", not to grade prose.
func CheckSummary(workdir string) (string, SummaryStatus, error) {
	path := filepath.Join(workdir, SummaryPath)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", SummaryMissing, nil
		}
		return "", SummaryMissing, err
	}
	content := strings.TrimSpace(string(b))
	if content == "" {
		return "", SummaryEmpty, nil
	}
	// Reject obvious stubs: short files (<32 chars after trimming) that
	// contain a known placeholder marker.
	if len(content) < 32 {
		upper := strings.ToUpper(content)
		for _, hint := range summaryPlaceholderHints {
			if strings.Contains(upper, strings.ToUpper(hint)) {
				return content, SummaryPlaceholder, nil
			}
		}
	}
	return content, SummaryOK, nil
}

// WriteChangedFiles writes one path per line to .aiops/CHANGED_FILES.txt.
func WriteChangedFiles(workdir string, files []string) error {
	body := strings.Join(files, "\n")
	if body != "" {
		body += "\n"
	}
	return writeAiopsFile(workdir, "CHANGED_FILES.txt", body)
}

// WriteVerification serializes verify command results to
// .aiops/VERIFICATION.txt so failed runs preserve the diagnostic output.
func WriteVerification(workdir string, results []VerifyResult) error {
	var buf bytes.Buffer
	for i, r := range results {
		if i > 0 {
			buf.WriteString("\n")
		}
		fmt.Fprintf(&buf, "$ %s\n", r.Command)
		fmt.Fprintf(&buf, "exit_code=%d duration_ms=%d\n", r.ExitCode, r.Duration.Milliseconds())
		if r.Err != nil {
			fmt.Fprintf(&buf, "error: %s\n", r.Err.Error())
		}
		if r.Output != "" {
			buf.WriteString(r.Output)
			if !strings.HasSuffix(r.Output, "\n") {
				buf.WriteString("\n")
			}
		}
		if r.Truncated {
			fmt.Fprintf(&buf, "...output truncated at %d bytes\n", VerifyOutputCap)
		}
	}
	return writeAiopsFile(workdir, "VERIFICATION.txt", buf.String())
}

func writeAiopsFile(workdir, name, body string) error {
	dir := filepath.Join(workdir, ".aiops")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
}

func ChangedFiles(ctx context.Context, workdir string) ([]string, error) {
	d, err := Diffstat(ctx, workdir)
	if err != nil {
		return nil, err
	}
	return d.Files, nil
}

// Diffstat returns the set of changed files and the total added+deleted
// lines for the working tree at workdir. It uses `git add --intent-to-add
// --all` so newly created (untracked) files show up, then runs
// `git diff --numstat -z HEAD` to capture both modified and added paths.
// The `-z` flag is required for correct policy matching: without it, git
// emits rename entries as `old => new` or `{a => b}/c` strings that no
// glob will match, silently bypassing deny/allow rules. With `-z`, rename
// entries are emitted as `added\tdeleted\t\0old\0new\0` (three NUL fields
// instead of one), letting us pull the destination path verbatim.
// Binary files (which numstat reports as "-\t-") contribute 0 lines but
// are still counted as changed files.
func Diffstat(ctx context.Context, workdir string) (policy.Diffstat, error) {
	// Mark untracked files so they appear in `git diff` without actually
	// staging their content. This is reversible and idempotent; the
	// subsequent `git add .` in CommitAndPush will fully stage them.
	if err := run(ctx, workdir, "git", "add", "--intent-to-add", "--all"); err != nil {
		return policy.Diffstat{}, fmt.Errorf("git add --intent-to-add: %w", err)
	}
	cmd := exec.CommandContext(ctx, "git", "diff", "--numstat", "-z", "HEAD")
	cmd.Dir = workdir
	out, err := cmd.Output()
	if err != nil {
		return policy.Diffstat{}, err
	}
	return parseNumstatZ(out)
}

// parseNumstatZ parses the NUL-delimited output of `git diff --numstat -z`.
// Each record is one of:
//
//	"<added>\t<deleted>\t<path>\0"               (normal)
//	"<added>\t<deleted>\t\0<old_path>\0<new_path>\0" (rename/copy)
//
// For renames we record only the destination path so that policy globs
// match the file as it will live in the tree.
func parseNumstatZ(out []byte) (policy.Diffstat, error) {
	var d policy.Diffstat
	// Split on NUL; the trailing terminator yields a final empty token we skip.
	tokens := strings.Split(string(out), "\x00")
	i := 0
	for i < len(tokens) {
		tok := tokens[i]
		if tok == "" {
			i++
			continue
		}
		// Each numstat record begins with "<added>\t<deleted>\t<rest>".
		parts := strings.SplitN(tok, "\t", 3)
		if len(parts) != 3 {
			i++
			continue
		}
		added, _ := strconv.Atoi(parts[0])
		deleted, _ := strconv.Atoi(parts[1])
		d.Lines += added + deleted
		if parts[2] == "" {
			// Rename/copy: next two NUL-delimited tokens are old, then new.
			if i+2 >= len(tokens) {
				return d, fmt.Errorf("malformed numstat -z rename record: %q", tok)
			}
			newPath := tokens[i+2]
			d.Files = append(d.Files, newPath)
			i += 3
			continue
		}
		d.Files = append(d.Files, parts[2])
		i++
	}
	return d, nil
}

// AllChangedFiles returns every path the worker is about to commit: tracked
// modifications plus untracked files reported by `git status --porcelain`.
// This is what we want to persist as a run artifact since the runner often
// creates new files (for example .aiops/RUN_SUMMARY.md) that `git diff`
// alone does not surface. We pass `-uall` so newly created directories like
// `.aiops/` are expanded to their individual files instead of collapsed to a
// single `.aiops/` entry.
func AllChangedFiles(ctx context.Context, workdir string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain", "-uall")
	cmd.Dir = workdir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	files := []string{}
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if path == "" {
			continue
		}
		// Rename entries look like "old -> new"; keep the new path.
		if idx := strings.Index(path, " -> "); idx >= 0 {
			path = strings.TrimSpace(path[idx+4:])
		}
		path = strings.Trim(path, "\"")
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		files = append(files, path)
	}
	return files, nil
}

// AllChangedFilesSinceUpstream returns both uncommitted worktree changes and
// files changed by commits ahead of the current branch's upstream. Worker policy
// gates that must constrain the whole agent-produced branch (not only the dirty
// worktree) should use this instead of AllChangedFiles.
func ResolveBaseBranchRef(ctx context.Context, workdir, baseBranch string) (string, error) {
	baseRef := "origin/" + strings.TrimSpace(baseBranch)
	if strings.TrimSpace(baseBranch) == "" || runQuiet(ctx, workdir, "git", "rev-parse", "--verify", baseRef) != nil {
		baseRef = strings.TrimSpace(baseBranch)
	}
	if baseRef == "" {
		baseRef = "@{upstream}"
	}
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", baseRef+"^{commit}")
	cmd.Dir = workdir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func AllChangedFilesSinceRef(ctx context.Context, workdir, baseRef string) ([]string, error) {
	files, err := AllChangedFiles(ctx, workdir)
	if err != nil {
		return nil, err
	}
	if baseRef == "" || runQuiet(ctx, workdir, "git", "rev-parse", "--verify", baseRef) != nil {
		baseRef = "@{upstream}"
	}
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", "-z", baseRef+"...HEAD")
	cmd.Dir = workdir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		seen[file] = struct{}{}
	}
	for _, file := range strings.Split(string(out), "\x00") {
		if file == "" {
			continue
		}
		if _, ok := seen[file]; ok {
			continue
		}
		seen[file] = struct{}{}
		files = append(files, file)
	}
	return files, nil
}

func AllChangedFilesSinceUpstream(ctx context.Context, workdir string) ([]string, error) {
	return AllChangedFilesSinceRef(ctx, workdir, "@{upstream}")
}

func HasCommitsSinceRef(ctx context.Context, workdir, baseRef string) (bool, error) {
	if baseRef == "" || runQuiet(ctx, workdir, "git", "rev-parse", "--verify", baseRef) != nil {
		baseRef = "@{upstream}"
	}
	cmd := exec.CommandContext(ctx, "git", "rev-list", "--count", baseRef+"..HEAD")
	cmd.Dir = workdir
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func HasCommitsSinceWorkspaceBase(ctx context.Context, workdir string) (bool, error) {
	return HasCommitsSinceRef(ctx, workdir, "@{upstream}")
}

// PolicyError is returned by EnforcePolicy when one or more policy checks
// fail. It carries the structured violations so callers (the worker) can
// emit a precise task event.
type PolicyError struct {
	Violations []policy.Violation
}

func (e *PolicyError) Error() string {
	return "policy violation: " + policy.Summarize(e.Violations)
}

// EnforcePolicy gathers a diffstat for workdir and evaluates it against the
// workflow PolicyConfig. On any violation it returns *PolicyError so that
// the worker can both block the push and write a structured task event.
func EnforcePolicy(ctx context.Context, workdir string, cfg workflow.Config) error {
	d, err := Diffstat(ctx, workdir)
	if err != nil {
		return err
	}
	pcfg := policy.Config{
		AllowPaths:      cfg.Policy.AllowPaths,
		DenyPaths:       cfg.Policy.DenyPaths,
		MaxChangedFiles: cfg.Policy.MaxChangedFiles,
		MaxChangedLines: cfg.Policy.LineLimit(),
	}
	if vs := policy.Evaluate(d, pcfg); len(vs) > 0 {
		return &PolicyError{Violations: vs}
	}
	return nil
}

// CommitIdentName and CommitIdentEmail are the git author/committer
// identity the worker stamps on every PR commit. Passing them inline via
// `git -c` flags keeps the worker self-contained: it works on hosts
// without a global git config (e.g. a fresh CI runner or a hardened
// container image) and gives all aiops-platform commits a uniform,
// recognizable attribution. Operators who want a different identity can
// still override at runtime via the standard
// GIT_{AUTHOR,COMMITTER}_{NAME,EMAIL} env vars, which take precedence
// over the -c session config.
const (
	CommitIdentName  = "aiops-platform worker"
	CommitIdentEmail = "worker@aiops-platform.local"
)

// CommitAndPush stages the workdir, commits with a Title-derived message,
// and pushes to origin/<branch>. Retries for the same task ID reuse the same
// work branch (see queue.Postgres.Enqueue), so on retry origin may already
// hold a commit from the previous attempt. To make that case safe we probe
// the upstream first and choose the push mode explicitly:
//
//   - Remote branch exists: refresh the local tracking ref via `git fetch`
//     and push with `--force-with-lease`, so the retry overwrites the stale
//     tip without silently clobbering anything else.
//   - Remote branch is absent (first attempt, or an operator cleaned up the
//     branch between retries): delete any stale `refs/remotes/origin/<branch>`
//     so a leftover tracking ref cannot block a fresh create with `stale info`,
//     then plain-push to (re)create the branch upstream.
func CommitAndPush(ctx context.Context, workdir string, title string, branch string) error {
	if err := run(ctx, workdir, "git", "add", "."); err != nil {
		return err
	}
	if err := run(ctx, workdir, "git", "diff", "--cached", "--quiet"); err == nil {
		return fmt.Errorf("no changes to commit")
	}
	if err := run(ctx, workdir, "git",
		"-c", "user.email="+CommitIdentEmail,
		"-c", "user.name="+CommitIdentName,
		"commit", "-m", "chore(ai): "+title,
	); err != nil {
		return err
	}
	exists, err := remoteBranchExists(ctx, workdir, branch)
	if err != nil {
		return fmt.Errorf("probe remote branch %q: %w", branch, err)
	}
	if exists {
		if err := run(ctx, workdir, "git", "fetch", "origin", branch); err != nil {
			return fmt.Errorf("fetch origin/%s: %w", branch, err)
		}
		return run(ctx, workdir, "git", "push", "--force-with-lease", "origin", branch)
	}
	_ = runQuiet(ctx, workdir, "git", "update-ref", "-d", "refs/remotes/origin/"+branch)
	return run(ctx, workdir, "git", "push", "origin", branch)
}

// remoteBranchExists reports whether `origin/<branch>` exists upstream by
// asking `git ls-remote --heads`. It is intentionally separate from
// `git fetch` so we can distinguish "branch absent upstream" (a normal,
// non-error state for first push or after manual cleanup) from "fetch
// failed" (a real connectivity / auth problem we should surface).
func remoteBranchExists(ctx context.Context, workdir, branch string) (bool, error) {
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--heads", "origin", branch)
	cmd.Dir = workdir
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return false, err
	}
	return strings.TrimSpace(stdout.String()) != "", nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// sameRealPath reports whether the path that `git rev-parse --git-common-dir`
// printed from workdir resolves to the same filesystem location as want.
// `--git-common-dir` may emit a relative path (e.g. `.git` for a non-worktree
// repo) which has to be resolved against workdir first, and both sides are
// passed through `filepath.EvalSymlinks` so the comparison ignores symlinked
// path prefixes (e.g. `/tmp` -> `/private/tmp` on macOS).
func sameRealPath(commonDir, workdir, want string) bool {
	if commonDir == "" {
		return false
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(workdir, commonDir)
	}
	commonReal, err := filepath.EvalSymlinks(commonDir)
	if err != nil {
		return false
	}
	wantReal, err := filepath.EvalSymlinks(want)
	if err != nil {
		return false
	}
	return commonReal == wantReal
}

func run(ctx context.Context, dir string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runQuiet runs a command without forwarding stdout/stderr. We use it for
// probe operations like `git rev-parse --verify` whose stderr is expected
// noise on the unhappy path.
func runQuiet(ctx context.Context, dir string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd.Run()
}

// SanitizeComponent returns the SPEC §4.2 workspace path component for s:
// any character outside [A-Za-z0-9._-] is replaced with `_`, and case is
// preserved verbatim. The harness adds two filesystem-safety rules on top
// of the pure SPEC rule: cap the result at maxSanitizedLength runes so it
// fits common filesystem name limits, and reject the two single-component
// path-traversal strings ("." and "..") by mapping them — along with the
// empty string — to "unknown" so PathFor never emits a traversal segment.
func SanitizeComponent(s string) string {
	return sanitizeComponent(s)
}

func sanitizeComponent(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z',
			r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if runes := []rune(out); len(runes) > maxSanitizedLength {
		out = string(runes[:maxSanitizedLength])
	}
	if out == "" || out == "." || out == ".." {
		return "unknown"
	}
	return out
}
