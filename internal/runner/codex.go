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

	"github.com/xrf9268-hue/aiops-platform/internal/workspace"
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
	if err := workspace.WriteSensitiveArtifact(filepath.Join(in.Workdir, CodexLastMessagePath), nil); err != nil {
		return Result{}, fmt.Errorf("prepare %s: %w", CodexLastMessagePath, err)
	}
	env := agentEnv(in.Workflow.Config.Codex.EnvPassthrough, in.Workflow.Config)
	cmd, err := buildCodexCmd(ctx, in, env)
	if err != nil {
		return Result{}, err
	}
	cmd.Dir = in.Workdir
	cmd.Env = env
	if err := validateAgentCommandWorkdir(in, cmd); err != nil {
		return Result{}, err
	}
	cmd, err = applySandbox(ctx, in, cmd)
	if err != nil {
		return Result{}, err
	}
	configurePlatformKill(cmd)
	cmd.WaitDelay = killGrace

	stdin, err := os.Open(promptAbs)
	if err != nil {
		return Result{}, fmt.Errorf("open %s: %w", PromptPath, err)
	}
	defer func() { _ = stdin.Close() }()
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
	head, tail := headTail(buf.Bytes())
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
func buildCodexCmd(ctx context.Context, in RunInput, env []string) (*exec.Cmd, error) {
	profile := in.Workflow.Config.Codex.Profile
	if profile == "" {
		profile = "safe"
	}
	// Both safe and bypass set --cd <workdir> for codex's own workspace
	// hint AND set cmd.Dir = workdir at the OS level. The -o path is made
	// absolute so that even if the two ever diverge in a future refactor,
	// the artifact lands in the workdir we own (issue #17 review).
	lastMessageAbs := filepath.Join(in.Workdir, CodexLastMessagePath)
	switch profile {
	case "safe":
		codexPath, err := lookPathInEnv("codex", env)
		if err != nil {
			return nil, fmt.Errorf("codex binary not found in PATH; install codex CLI or set agent.default to claude/mock")
		}
		// `--sandbox workspace-write` replaces the deprecated `--full-auto`.
		// Per codex commit 3d10ba9f3 (PR openai/codex#20133) `--full-auto`
		// is now a deprecation alias whose migration target is
		// `--sandbox workspace-write`; codex-rs/exec/src/lib.rs maps the
		// removed flag to `SandboxMode::WorkspaceWrite` and prints
		//   warning: `--full-auto` is deprecated; use `--sandbox workspace-write` instead.
		// on every invocation, polluting stderr capture (#330). Both flags
		// resolve to the same sandbox mode; the new spelling is forward-
		// compatible if codex eventually removes the alias.
		return exec.CommandContext(ctx,
			codexPath, "exec",
			"--sandbox", "workspace-write",
			"--skip-git-repo-check",
			"--cd", in.Workdir,
			"-o", lastMessageAbs,
		), nil
	case "bypass":
		codexPath, err := lookPathInEnv("codex", env)
		if err != nil {
			return nil, fmt.Errorf("codex binary not found in PATH; install codex CLI or set agent.default to claude/mock")
		}
		return exec.CommandContext(ctx,
			codexPath, "exec",
			"--dangerously-bypass-approvals-and-sandbox",
			"--skip-git-repo-check",
			"--cd", in.Workdir,
			"-o", lastMessageAbs,
		), nil
	case "custom":
		command := strings.TrimSpace(in.Workflow.Config.Codex.Command)
		if command == "" {
			return nil, fmt.Errorf("codex.profile=custom requires codex.command to be non-empty")
		}
		return exec.CommandContext(ctx, "sh", "-c", command), nil
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
	_ = workspace.WriteSensitiveArtifact(filepath.Join(workdir, CodexOutputPath), body)
}

// readCodexSummary returns the trimmed contents of CODEX_LAST_MESSAGE.md or
// "codex completed" when the file is missing/empty/unreadable.
func readCodexSummary(workdir string) string {
	b, err := workspace.ReadSensitiveArtifact(filepath.Join(workdir, CodexLastMessagePath))
	if err != nil {
		return "codex completed"
	}
	trimmed := strings.TrimSpace(string(b))
	if trimmed == "" {
		return "codex completed"
	}
	return trimmed
}
