package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Per-operation deadlines for git subprocesses. The dispatch run context is
// cancel-only by design (the agent run is unbounded per SPEC §7.1), so the
// workspace layer must bound its own external I/O or a black-holed remote
// stalls the dispatch — and its capacity slot — until operator intervention
// (#759, AGENTS.md "All external I/O is timeout-bounded"). Vars, not consts,
// so tests can shrink them. context.WithTimeout keeps an earlier parent
// deadline, and parent cancellation still pre-empts immediately.
var (
	gitNetworkTimeout = 10 * time.Minute // clone --bare / fetch of a large repo over a slow link
	gitLocalTimeout   = 5 * time.Minute  // config / worktree / checkout ops on slow disks
	// gitWaitGrace bounds cmd.Wait after the context fires: a descendant
	// (ssh, credential helper) that inherits the output pipe and outlives
	// the killed git process would otherwise hold Wait open to its own exit.
	gitWaitGrace = 5 * time.Second
)

// classifyGitContextErr rewraps a git failure caused by context expiry so
// callers can classify with errors.Is instead of matching "signal: killed"
// text (clean-code rule 8). A plain git failure passes through untouched,
// and a parent WithCancelCause cause (e.g. reconcile-cancel) is preserved.
func classifyGitContextErr(ctx context.Context, budget time.Duration, err error) error {
	// ErrWaitDelay is only returned when the process exited successfully but
	// a descendant kept an output pipe open past gitWaitGrace (e.g. a fetch
	// kicking off background auto-gc). The git operation itself succeeded —
	// reporting it as failure would fail dispatches on healthy repos.
	if errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	if err == nil || ctx.Err() == nil {
		return err
	}
	cause := context.Cause(ctx)
	if errors.Is(cause, context.DeadlineExceeded) {
		return fmt.Errorf("git operation exceeded %s budget (%w): %w", budget, err, cause)
	}
	return fmt.Errorf("git operation canceled (%w): %w", err, cause)
}

// runGit runs a local git op (worktree add, checkout, config) with stdout and
// stderr forwarded to the worker's live logs. Unlike the probe helpers below, a
// failed runGit folds git's last actionable stderr line into the returned error
// so the runtime event / dashboard surfaces e.g.
// "fatal: ambiguous object name: 'origin/main'" instead of only "exit status
// 255" — which in #976 turned a one-line diagnosis into a multi-hour hunt
// (#978).
//
// stderr is captured to a temp *os.File, not an io.Writer: a real fd is handed
// to the child, so os/exec spawns no copy goroutine, and parent cancellation
// (reconcile-cancel, shutdown) still pre-empts promptly. Tee-ing stderr to an
// in-process buffer turns it into a pipe whose drain blocks cmd.Wait until
// gitWaitGrace on a kill — the regression caught by
// TestRunGitParentCancellationPreemptsBudget. runGit is local-only;
// credentialed clone/fetch must use runGitRedacted, so the folded line can never
// carry basic-auth userinfo.
func runGit(ctx context.Context, dir string, args ...string) error {
	runCtx, cancel := context.WithTimeout(ctx, gitLocalTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "git", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.WaitDelay = gitWaitGrace

	capture, err := os.CreateTemp("", "aiops-git-stderr-*")
	if err != nil {
		// Temp file unavailable: fall back to the pre-#978 behavior (stderr
		// straight to live logs, bare exit status in the error) rather than
		// failing the git op over a logging concern.
		cmd.Stderr = os.Stderr
		return classifyGitContextErr(runCtx, gitLocalTimeout, cmd.Run())
	}
	defer func() {
		_ = capture.Close()
		_ = os.Remove(capture.Name())
	}()
	cmd.Stderr = capture

	runErr := classifyGitContextErr(runCtx, gitLocalTimeout, cmd.Run())
	stderr := forwardCapturedStderr(capture)
	if runErr == nil {
		return nil
	}
	return foldGitStderr(runErr, stderr)
}

// forwardCapturedStderr reads the captured git stderr back from the temp file,
// writes it to the worker's os.Stderr so live logs are preserved, and returns
// it for folding into a failed op's error. Best-effort: a seek/read failure
// yields no captured text and never masks the git result itself.
func forwardCapturedStderr(f *os.File) string {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return ""
	}
	b, err := io.ReadAll(f)
	if err != nil || len(b) == 0 {
		return ""
	}
	_, _ = os.Stderr.Write(b)
	return string(b)
}

// foldGitStderr appends git's last actionable stderr line to err. %w keeps the
// underlying *exec.ExitError and any cancellation cause classifiable via
// errors.Is/errors.As (clean-code rule 8); an empty capture leaves err as-is.
func foldGitStderr(err error, stderr string) error {
	line := lastActionableLine(stderr)
	if line == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, line)
}

// lastActionableLine returns the last "fatal:"/"error:" line git wrote, or its
// last non-empty line if it emitted neither. Folding a single line keeps the
// wrapped error bounded no matter how much progress noise preceded the failure.
func lastActionableLine(s string) string {
	var fallback, actionable string
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		fallback = line
		if strings.HasPrefix(line, "fatal:") || strings.HasPrefix(line, "error:") {
			actionable = line
		}
	}
	if actionable != "" {
		return actionable
	}
	return fallback
}

// runGitRedacted runs git like runGit but scrubs basic-auth userinfo from the
// forwarded stdout/stderr. Use it for clone/fetch against a credentialed
// remote, where git can echo the `user:token@host` clone URL (the agent's push
// credential) on failure — directly, when the URL is on the clone command line,
// or via the stored remote.origin.url on a fetch. Without this, a failed
// `git clone --bare <cloneURL>` / `git fetch` leaks the credential to the
// worker's os.Stderr (#595, AGENTS.md secret-masking convention).
func runGitRedacted(ctx context.Context, dir string, args ...string) error {
	runCtx, cancel := context.WithTimeout(ctx, gitNetworkTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "git", args...)
	cmd.Dir = dir
	cmd.WaitDelay = gitWaitGrace
	return classifyGitContextErr(runCtx, gitNetworkTimeout, runRedacted(cmd))
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

// runGitQuiet runs a command without forwarding stdout/stderr. We use it for
// probe operations like `git rev-parse --verify` whose stderr is expected
// noise on the unhappy path.
func runGitQuiet(ctx context.Context, dir string, args ...string) error {
	runCtx, cancel := context.WithTimeout(ctx, gitLocalTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "git", args...)
	cmd.Dir = dir
	cmd.WaitDelay = gitWaitGrace
	return classifyGitContextErr(runCtx, gitLocalTimeout, cmd.Run())
}

// runGitOutput runs a local git probe and returns its stdout. Stderr stays
// nil (devnull / ExitError.Stderr) so probe noise on the unhappy path —
// e.g. `fatal: not a git repository` from a broken-but-existing `.git` —
// never pollutes the worker's inherited fd 2.
func runGitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, gitLocalTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "git", args...)
	cmd.Dir = dir
	cmd.WaitDelay = gitWaitGrace
	out, err := cmd.Output()
	return out, classifyGitContextErr(runCtx, gitLocalTimeout, err)
}
