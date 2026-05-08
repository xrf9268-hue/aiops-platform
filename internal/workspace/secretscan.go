package workspace

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// secretScanOutputCap bounds captured stdout/stderr to keep task event
// payloads small. Mirrors the cap used elsewhere when summarizing long
// command output.
const secretScanOutputCap = 1 << 20 // 1 MiB

// SecretScanStatus categorizes the outcome of a secret scan run.
type SecretScanStatus string

const (
	// SecretScanSkipped indicates the scanner was disabled or unconfigured.
	SecretScanSkipped SecretScanStatus = "skipped"
	// SecretScanClean indicates the scanner exited zero (no findings).
	SecretScanClean SecretScanStatus = "clean"
	// SecretScanViolation indicates the scanner exited non-zero (findings).
	SecretScanViolation SecretScanStatus = "violation"
	// SecretScanError indicates the scanner could not be executed at all
	// (binary not found, etc.). This is distinct from a finding because it
	// is typically an operator misconfiguration.
	SecretScanError SecretScanStatus = "error"
)

// SecretScanResult is the structured outcome of a single scan invocation.
// It is suitable for direct JSON serialization into a task event payload.
type SecretScanResult struct {
	Status     SecretScanStatus `json:"status"`
	Command    []string         `json:"command,omitempty"`
	ExitCode   int              `json:"exit_code"`
	DurationMs int64            `json:"duration_ms"`
	Stdout     string           `json:"stdout,omitempty"`
	Stderr     string           `json:"stderr,omitempty"`
	// Err captures execution failures (e.g. binary not found). It is not
	// serialized as JSON; callers convert it to an event message instead.
	Err error `json:"-"`
}

// ShouldBlockPush reports whether this result should prevent the push,
// honoring the workflow's FailOnFinding setting. Execution errors always
// block; a clean or skipped result never blocks.
func (r SecretScanResult) ShouldBlockPush(cfg workflow.SecretScanConfig) bool {
	switch r.Status {
	case SecretScanError:
		return true
	case SecretScanViolation:
		return cfg.ShouldFailOnFinding()
	default:
		return false
	}
}

// secretScanCommandRunner is an indirection seam used by the unit tests to
// stub command execution without touching the real PATH. Production code
// uses runSecretScanCommand below.
type secretScanCommandRunner func(ctx context.Context, dir string, argv []string) (stdout, stderr []byte, exitCode int, err error)

var defaultSecretScanRunner secretScanCommandRunner = runSecretScanCommand

// RunSecretScan executes the configured pre-push secret scanner inside
// workdir. It returns a structured SecretScanResult; callers decide
// whether to block the push based on ShouldBlockPush.
//
// Behavior:
//   - If the section is disabled or no command is configured, the result
//     status is SecretScanSkipped and the caller proceeds with push.
//   - If the scanner exits zero, status is SecretScanClean.
//   - If the scanner exits non-zero, status is SecretScanViolation. Whether
//     this blocks the push depends on cfg.ShouldFailOnFinding().
//   - If the scanner cannot be executed (e.g. binary missing), status is
//     SecretScanError and Err is populated; this always blocks the push.
//
// Captured stdout/stderr are truncated to 1 MiB to bound event payload
// size, matching the behavior of other long-running command captures.
func RunSecretScan(ctx context.Context, workdir string, cfg workflow.SecretScanConfig) SecretScanResult {
	if !cfg.Enabled || len(cfg.Command) == 0 {
		return SecretScanResult{Status: SecretScanSkipped}
	}
	return runSecretScanWith(ctx, workdir, cfg, defaultSecretScanRunner)
}

func runSecretScanWith(ctx context.Context, workdir string, cfg workflow.SecretScanConfig, runner secretScanCommandRunner) SecretScanResult {
	argv := append([]string(nil), cfg.Command...)
	start := time.Now()
	stdout, stderr, code, err := runner(ctx, workdir, argv)
	dur := time.Since(start).Milliseconds()

	res := SecretScanResult{
		Command:    argv,
		ExitCode:   code,
		DurationMs: dur,
		Stdout:     truncate(stdout, secretScanOutputCap),
		Stderr:     truncate(stderr, secretScanOutputCap),
	}
	if err != nil {
		// If the parent context was canceled or its deadline elapsed, the
		// scan never had a chance to complete. We must not treat that as a
		// finding: report it as an execution error so the push is blocked
		// regardless of fail_on_finding (we simply do not know whether the
		// repo contains secrets).
		if ctxErr := ctx.Err(); errors.Is(ctxErr, context.Canceled) || errors.Is(ctxErr, context.DeadlineExceeded) {
			res.Status = SecretScanError
			res.Err = fmt.Errorf("secret scan aborted: %w", ctxErr)
			return res
		}
		// Distinguish "ran and exited non-zero" (ExitError) from "could
		// not start at all" (e.g. binary missing) and from "process was
		// killed by a signal" (OOM kill, external SIGTERM, etc.).
		//
		// A normal non-zero exit is a finding to surface (SecretScanViolation).
		// A failure to start or a signal-killed process is an operator/exec
		// error: the scan never produced a trustworthy result, so we map
		// it to SecretScanError, which always blocks the push.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if isSignaled(ee) {
				res.Status = SecretScanError
				res.Err = fmt.Errorf("secret scan terminated by signal: %w", err)
				return res
			}
			res.Status = SecretScanViolation
			return res
		}
		res.Status = SecretScanError
		res.Err = fmt.Errorf("secret scan exec: %w", err)
		return res
	}
	res.Status = SecretScanClean
	return res
}

// isSignaled reports whether the process exited because it received a
// signal (SIGKILL/SIGTERM/etc.) rather than calling exit(2) on its own.
// Such terminations are not trustworthy scanner results: an OOM kill or
// timeout-driven SIGKILL leaves the scan incomplete and must not be
// classified as "found a secret".
//
// On Unix, the underlying ProcessState's WaitStatus exposes Signaled().
// As a portable fallback, a -1 exit code (or any negative code) also
// indicates the OS reported no normal exit code, which Go uses for
// signal-terminated processes.
func isSignaled(ee *exec.ExitError) bool {
	if ee == nil || ee.ProcessState == nil {
		return false
	}
	if ws, ok := ee.ProcessState.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return true
		}
	}
	// Defensive fallback: ExitCode() returns -1 when the process did not
	// exit normally (e.g. killed by signal, or platforms without
	// WaitStatus). Treat any negative code as "not a real exit".
	if ee.ProcessState.ExitCode() < 0 {
		return true
	}
	return false
}

// runSecretScanCommand is the default command runner. It captures stdout
// and stderr separately, which lets callers log them independently and
// surface scanner findings (typically on stdout) cleanly in events. The
// in-memory buffers reuse cappedBuffer (defined in manager.go) so a
// runaway scanner cannot exhaust worker memory.
func runSecretScanCommand(ctx context.Context, dir string, argv []string) ([]byte, []byte, int, error) {
	if len(argv) == 0 {
		return nil, nil, 0, fmt.Errorf("secret scan: empty command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	stdout := &cappedBuffer{Cap: secretScanOutputCap}
	stderr := &cappedBuffer{Cap: secretScanOutputCap}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	} else if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	return []byte(stdout.String()), []byte(stderr.String()), code, err
}

func truncate(b []byte, max int) string {
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max])
}
