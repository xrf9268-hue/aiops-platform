package workspace

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/policy"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// VerifyOutputCap bounds how many bytes of combined stdout+stderr we keep in
// memory per verify command. Verbose verify steps (e.g. `go test -v ./...`)
// can emit tens of MiB; capping prevents unbounded RAM use and the duplicate
// allocation that happens when the output is later written as an artifact.
const VerifyOutputCap = 1 << 20 // 1 MiB

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
	buf     bytes.Buffer
	dropped int64
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
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

func (c *cappedBuffer) String() string  { return c.buf.String() }
func (c *cappedBuffer) Truncated() bool { return c.dropped > 0 }
func (c *cappedBuffer) Dropped() int64  { return c.dropped }

// Compile-time check that cappedBuffer satisfies io.Writer.
var _ io.Writer = (*cappedBuffer)(nil)

// Manager owns per-task workspace creation, layered on top of a process-wide
// bare mirror cache. Each task gets its own detached worktree under Root so
// concurrent workers cannot stomp on each other, while the heavy network IO
// (object download) happens once per repo in the mirror cache and is
// reused on every subsequent task.
type Manager struct {
	// Root is the per-task worktree root, typically WORKSPACE_ROOT.
	Root string
	// MirrorRoot overrides the bare mirror cache location. When empty,
	// MirrorRoot() resolves it from os.UserCacheDir or os.TempDir.
	MirrorRoot string
}

func New(root string) *Manager { return &Manager{Root: root} }

func (m *Manager) PathFor(t task.Task) string {
	repo := sanitize(t.RepoOwner + "_" + t.RepoName)
	return filepath.Join(m.Root, repo, t.ID)
}

// PrepareGitWorkspace materialises a per-task workspace by adding a fresh
// worktree off the cached bare mirror for t.CloneURL. The worktree is
// created at PathFor(t) on the work branch, with origin set back to the
// upstream URL so `git push origin <branch>` works without further setup.
//
// On every call we refresh the mirror first so the worktree starts from
// up-to-date refs; this replaces the previous behaviour of running a fresh
// `git clone` per task, which was both slow and wasteful for large repos.
func (m *Manager) PrepareGitWorkspace(ctx context.Context, t task.Task) (string, error) {
	workdir := m.PathFor(t)
	if err := os.MkdirAll(filepath.Dir(workdir), 0o755); err != nil {
		return "", err
	}
	mirror, err := m.EnsureMirror(ctx, t.CloneURL)
	if err != nil {
		return "", err
	}
	// Drop any leftover worktree from a previous run *before* asking git to
	// add a new one at the same path. We deliberately ignore failures and
	// silence stderr because the common case ("no such worktree") prints a
	// scary fatal line that obscures real worker logs.
	_ = runQuiet(ctx, mirror, "git", "worktree", "remove", "--force", workdir)
	_ = os.RemoveAll(workdir)
	if err := runQuiet(ctx, mirror, "git", "worktree", "prune"); err != nil {
		return "", fmt.Errorf("worktree prune: %w", err)
	}
	// Create the worktree on the work branch, branched from the requested
	// base ref. We resolve the base via `origin/<base>` because the bare
	// cache stores upstream branches as remote-tracking refs (see
	// EnsureMirror); falling back to the bare name covers `file://` test
	// fixtures where the upstream is the same on-disk repo. Using -B makes
	// the operation idempotent if a previous attempt left an in-mirror
	// branch behind.
	startRef := "origin/" + t.BaseBranch
	if err := runQuiet(ctx, mirror, "git", "rev-parse", "--verify", startRef); err != nil {
		startRef = t.BaseBranch
	}
	if err := run(ctx, mirror, "git", "worktree", "add", "-B", t.WorkBranch, workdir, startRef); err != nil {
		return "", fmt.Errorf("worktree add: %w", err)
	}
	// The mirror's "origin" remote points at the upstream URL, but inside
	// the worktree git resolves remotes via the linked repo, so push works
	// out of the box. We still set the URL explicitly for clarity in case
	// downstream tooling inspects `git remote -v` from within the worktree.
	_ = run(ctx, workdir, "git", "remote", "set-url", "origin", t.CloneURL)
	return workdir, nil
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

	var (
		results  []VerifyResult
		failures int
	)
	for _, command := range wf.Verify.Commands {
		if strings.TrimSpace(command) == "" {
			continue
		}
		// Stop launching new commands once the phase deadline has
		// elapsed; the in-flight command (if any) was already killed
		// by runCtx.Done().
		if runCtx.Err() != nil {
			break
		}
		start := time.Now()
		buf := &cappedBuffer{Cap: VerifyOutputCap}
		cmd := exec.CommandContext(runCtx, "sh", "-lc", command)
		cmd.Dir = workdir
		cmd.Stdout = buf
		cmd.Stderr = buf
		runErr := cmd.Run()
		res := VerifyResult{
			Command:   command,
			Output:    buf.String(),
			Truncated: buf.Truncated(),
			Duration:  time.Since(start),
			ExitCode:  cmd.ProcessState.ExitCode(),
		}
		if runErr != nil {
			res.Err = runErr
			failures++
		}
		results = append(results, res)
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
	if err := run(ctx, workdir, "git", "commit", "-m", "chore(ai): "+title); err != nil {
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

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, " ", "_")
	return s
}
