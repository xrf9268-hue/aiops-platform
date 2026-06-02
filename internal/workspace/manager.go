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
	"strings"
	"sync"
	"syscall"
	"time"

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
//     (cached deps, build outputs) survive across
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
	startRef := resolveStartRef(ctx, mirror, t.BaseBranch)

	createdNow, err := attachWorktree(ctx, workdir, mirror, t.WorkBranch, startRef, workdirExists)
	if err != nil {
		return "", false, err
	}
	return workdir, createdNow, nil
}

// attachWorktree reuses the existing workdir when it is a valid, mirror-linked
// worktree and otherwise (re)creates it. It returns createdNow=true on the
// create path so the caller fires after_create on first touch / recovery.
func attachWorktree(ctx context.Context, workdir, mirror, workBranch, startRef string, workdirExists bool) (createdNow bool, err error) {
	reusable, foreignCommonDir := false, ""
	if workdirExists {
		reusable, foreignCommonDir = classifyExistingWorkdir(ctx, workdir, mirror)
	}
	if reusable {
		return false, reuseWorktree(ctx, workdir, workBranch, startRef)
	}
	return true, createWorktree(ctx, workdir, mirror, workBranch, startRef, foreignCommonDir)
}

// resolveStartRef resolves the base ref via `origin/<base>` because the bare
// cache stores upstream branches as remote-tracking refs (see EnsureMirror);
// it falls back to the bare name to cover `file://` test fixtures where the
// upstream is the same on-disk repo.
func resolveStartRef(ctx context.Context, mirror, baseBranch string) string {
	startRef := "origin/" + baseBranch
	if err := runGitQuiet(ctx, mirror, "rev-parse", "--verify", startRef); err != nil {
		startRef = baseBranch
	}
	return startRef
}

// classifyExistingWorkdir decides whether an existing workdir is a valid,
// mirror-linked worktree safe to reuse. A workdir that exists but isn't must
// not pin the worker into a bad state, so three classes of "looks reusable but
// isn't" are explicitly rejected (returning reusable=false):
//
//  1. The path is a symlink. An attacker who can plant a symlink at the
//     workspace path could otherwise redirect the reuse-path `git reset` /
//     `git checkout -B` into a repository outside the workspace root entirely.
//     `os.Lstat` (vs the `os.Stat` the caller used) catches this.
//  2. The path holds an independent git repository — e.g. a prior agent run
//     that rewrote its workspace as a fresh `git init` would still pass `git
//     rev-parse --git-dir`. We verify `git rev-parse --git-common-dir`
//     resolves to *our* mirror so unrelated repos can't masquerade.
//  3. The path is the worktree of a different mirror. The git-common-dir
//     check above also covers this.
//
// The returned foreignCommonDir is non-empty only when the common-dir probe
// succeeded but pointed at a different mirror; the caller uses it to prune the
// orphaned worktree admin entry off that foreign mirror before recreating,
// otherwise the entry lingers until `gc.worktreePruneExpire` (3 months).
func classifyExistingWorkdir(ctx context.Context, workdir, mirror string) (reusable bool, foreignCommonDir string) {
	lstatInfo, lstatErr := os.Lstat(workdir)
	if lstatErr != nil || lstatInfo.Mode()&os.ModeSymlink != 0 {
		return false, ""
	}
	if err := runGitQuiet(ctx, workdir, "rev-parse", "--git-dir"); err != nil {
		return false, ""
	}
	// Silence the probe's stderr: on a broken-but-existing `.git`
	// (mid-corruption, partial `worktree add` crash, race with
	// `os.RemoveAll`) git prints `fatal: not a git repository` while we're
	// about to fall through to the recreate path anyway. Without io.Discard
	// that fatal line pollutes the worker's inherited fd 2.
	probe := exec.CommandContext(ctx, "git", "-C", workdir, "rev-parse", "--git-common-dir")
	probe.Stderr = io.Discard
	cdOut, cdErr := probe.Output()
	if cdErr != nil {
		return false, ""
	}
	common := strings.TrimSpace(string(cdOut))
	if sameRealPath(common, workdir, mirror) {
		return true, ""
	}
	if common != "" {
		if !filepath.IsAbs(common) {
			common = filepath.Join(workdir, common)
		}
		return false, common
	}
	return false, ""
}

// reuseWorktree refreshes a reusable worktree (SPEC §9.1: workspaces are reused
// across runs for the same issue). It clears the index first, then snaps the
// work branch to the refreshed base:
//   - `git reset --quiet HEAD -- .` drops any stray intent-to-add entries left
//     in the index (e.g. by a hook running `git add -N`); without it the
//     checkout below would treat those untracked-but-staged files as removable
//     and cached deps / hook artifacts would vanish on reuse.
//   - `git checkout --force --no-track -B` rebases the work branch to startRef:
//     `--force` discards tracked-file modifications, `--no-track` keeps the work
//     branch from tracking the base branch (matching the create path's
//     `worktree add --no-track`), and `-B` makes it idempotent.
//
// Untracked files (cached deps, build outputs) survive.
func reuseWorktree(ctx context.Context, workdir, workBranch, startRef string) error {
	if err := runGitQuiet(ctx, workdir, "reset", "--quiet", "HEAD", "--", "."); err != nil {
		return fmt.Errorf("worktree index reset: %w", err)
	}
	if err := runGit(ctx, workdir, "checkout", "--force", "--no-track", "-B", workBranch, startRef); err != nil {
		return fmt.Errorf("worktree checkout: %w", err)
	}
	if err := EnsureSensitiveArtifactExcludes(ctx, workdir); err != nil {
		return fmt.Errorf("install sensitive artifact excludes: %w", err)
	}
	return nil
}

// createWorktree handles a first touch (or recovery from a corrupted leftover).
// If the reuse gate rejected a workspace linked to a *different* mirror,
// foreignCommonDir prunes the orphaned admin entry off that mirror first so its
// `worktrees/` directory stays in sync with disk. It then drops any stale
// worktree entry the mirror still tracks for this path, removes the workdir,
// prunes, and adds a fresh `--no-track -B` worktree (idempotent via `-B`; the
// worktree inherits origin from the linked bare mirror, so no remote set-url).
func createWorktree(ctx context.Context, workdir, mirror, workBranch, startRef, foreignCommonDir string) error {
	if foreignCommonDir != "" {
		_ = runGitQuiet(ctx, foreignCommonDir, "worktree", "prune")
	}
	// Failures here are ignored and stderr silenced: the common case ("no such
	// worktree") prints a scary fatal line that obscures real worker logs.
	_ = runGitQuiet(ctx, mirror, "worktree", "remove", "--force", workdir)
	_ = os.RemoveAll(workdir)
	if err := runGitQuiet(ctx, mirror, "worktree", "prune"); err != nil {
		return fmt.Errorf("worktree prune: %w", err)
	}
	if err := runGit(ctx, mirror, "worktree", "add", "--no-track", "-B", workBranch, workdir, startRef); err != nil {
		return fmt.Errorf("worktree add: %w", err)
	}
	if err := EnsureSensitiveArtifactExcludes(ctx, workdir); err != nil {
		return fmt.Errorf("install sensitive artifact excludes: %w", err)
	}
	return nil
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

	// Hook commands run under `sh -c` (not `-lc`) so the user's login
	// profile is not re-sourced per command. The PATH that a login shell
	// would build is captured once at startup and propagated via cmd.Env
	// (see LoginPATH in path_snapshot.go and #314). Without this split,
	// every hook command captured the stdout of /etc/profile.d/* into
	// HookResult.Output — surfaced to operators in runtime events and
	// consumed by policy-feedback parsers.
	cmd := exec.CommandContext(runCtx, "sh", "-c", command)
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
	return WriteSensitiveArtifact(filepath.Join(dir, "PROMPT.md"), []byte(prompt))
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

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runGitRedacted runs git like runGit but scrubs basic-auth userinfo from the
// forwarded stdout/stderr. Use it for clone/fetch against a credentialed
// remote, where git can echo the `user:token@host` clone URL (the agent's push
// credential) on failure — directly, when the URL is on the clone command line,
// or via the stored remote.origin.url on a fetch. Without this, a failed
// `git clone --bare <cloneURL>` / `git fetch` leaks the credential to the
// worker's os.Stderr (#595, AGENTS.md secret-masking convention).
func runGitRedacted(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	return runRedacted(cmd)
}

// runRedacted runs cmd with its stdout/stderr forwarded to os.Stdout/os.Stderr
// through a credentialRedactingWriter, so embedded basic-auth userinfo never
// reaches the worker's logs. It is split out from runGitRedacted so the
// redaction wiring can be tested end-to-end with a command that emits a real
// `user:token@` URL on stderr — git's own messages already sanitise the URL on
// some versions, which would make a git-driven test a placebo (#595).
func runRedacted(cmd *exec.Cmd) error {
	outW := &credentialRedactingWriter{w: os.Stdout}
	errW := &credentialRedactingWriter{w: os.Stderr}
	cmd.Stdout = outW
	cmd.Stderr = errW
	err := cmd.Run()
	// Flush regardless of err: a failed clone/fetch is exactly when git emits
	// the credentialed URL, and that output may not end in a newline.
	_ = outW.Flush()
	_ = errW.Flush()
	return err
}

// runQuiet runs a command without forwarding stdout/stderr. We use it for
// probe operations like `git rev-parse --verify` whose stderr is expected
// noise on the unhappy path.
func runGitQuiet(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
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
