package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CodexOutputPath is where the runner persists captured codex stdout+stderr,
// relative to the workdir.
const CodexOutputPath = ".aiops/CODEX_OUTPUT.txt"

// CodexLastMessagePath is where codex CLI writes its final message when
// invoked with -o; the runner ingests it as Result.Summary on success.
const CodexLastMessagePath = ".aiops/CODEX_LAST_MESSAGE.md"

// PromptPath is the workdir-relative location of the rendered prompt the
// worker writes before invoking the runner.
const PromptPath = ".aiops/PROMPT.md"

// CodexRunner is the profile-driven runner for the codex CLI. It replaces
// the generic ShellRunner for codex; claude continues to use ShellRunner.
//
// Profile dispatch lives entirely inside Run; the runner is stateless and
// safe to share across goroutines.
type CodexRunner struct{}

func (CodexRunner) Run(ctx context.Context, in RunInput) (Result, error) {
	promptAbs := filepath.Join(in.Workdir, PromptPath)
	if _, err := os.Stat(promptAbs); err != nil {
		return Result{}, fmt.Errorf("read %s: %w", PromptPath, err)
	}

	cmd, err := buildCodexCmd(ctx, in)
	if err != nil {
		return Result{}, err
	}
	cmd.Dir = in.Workdir
	configurePlatformKill(cmd)
	cmd.WaitDelay = killGrace

	stdin, err := os.Open(promptAbs)
	if err != nil {
		return Result{}, fmt.Errorf("open %s: %w", PromptPath, err)
	}
	defer stdin.Close()
	cmd.Stdin = stdin

	buf := &cappedWriter{Cap: CodexOutputCap}
	cmd.Stdout = buf
	cmd.Stderr = buf

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	writeCodexArtifact(in.Workdir, buf)

	res := Result{
		Summary:       readCodexSummary(in.Workdir),
		OutputBytes:   int64(len(buf.Bytes())),
		OutputDropped: buf.Dropped(),
	}
	head, tail := headTail(buf.Bytes(), CodexEventOutputCap)
	if len(head) > 0 {
		res.OutputHead = string(head)
	}
	res.OutputTail = tail

	if runErr != nil {
		if cerr := ctx.Err(); errors.Is(cerr, context.DeadlineExceeded) {
			return res, &TimeoutError{
				Timeout: deadlineBudget(ctx, start),
				Elapsed: elapsed,
				Cause:   runErr,
			}
		}
		return res, runErr
	}
	return res, nil
}

// buildCodexCmd assembles the *exec.Cmd for the requested profile. PROMPT.md
// is always provided via stdin, never via shell redirection.
func buildCodexCmd(ctx context.Context, in RunInput) (*exec.Cmd, error) {
	profile := in.Workflow.Config.Codex.Profile
	if profile == "" {
		profile = "safe"
	}
	switch profile {
	case "safe":
		if _, err := exec.LookPath("codex"); err != nil {
			return nil, fmt.Errorf("codex binary not found in PATH; install codex CLI or set agent.default to claude/mock")
		}
		return exec.CommandContext(ctx,
			"codex", "exec",
			"--full-auto",
			"--skip-git-repo-check",
			"--cd", in.Workdir,
			"-o", CodexLastMessagePath,
		), nil
	case "bypass":
		if _, err := exec.LookPath("codex"); err != nil {
			return nil, fmt.Errorf("codex binary not found in PATH; install codex CLI or set agent.default to claude/mock")
		}
		return exec.CommandContext(ctx,
			"codex", "exec",
			"--dangerously-bypass-approvals-and-sandbox",
			"--skip-git-repo-check",
			"--cd", in.Workdir,
			"-o", CodexLastMessagePath,
		), nil
	case "custom":
		command := strings.TrimSpace(in.Workflow.Config.Codex.Command)
		if command == "" {
			return nil, fmt.Errorf("codex.profile=custom requires codex.command to be non-empty")
		}
		return exec.CommandContext(ctx, "sh", "-lc", command), nil
	default:
		return nil, fmt.Errorf("codex.profile %q is not supported", profile)
	}
}

// writeCodexArtifact persists the buffered output to .aiops/CODEX_OUTPUT.txt.
// On truncation, an explanatory footer line is appended. Errors are swallowed
// (the artifact is best-effort; the event payload still carries head/tail).
func writeCodexArtifact(workdir string, buf *cappedWriter) {
	dir := filepath.Join(workdir, ".aiops")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	body := append([]byte{}, buf.Bytes()...)
	if buf.Dropped() > 0 {
		footer := fmt.Sprintf("\n...output truncated at %d bytes\n", CodexOutputCap)
		body = append(body, []byte(footer)...)
	}
	_ = os.WriteFile(filepath.Join(workdir, CodexOutputPath), body, 0o644)
}

// readCodexSummary returns the trimmed contents of CODEX_LAST_MESSAGE.md or
// "codex completed" when the file is missing/empty/unreadable.
func readCodexSummary(workdir string) string {
	b, err := os.ReadFile(filepath.Join(workdir, CodexLastMessagePath))
	if err != nil {
		return "codex completed"
	}
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" {
		return "codex completed"
	}
	return trimmed
}
