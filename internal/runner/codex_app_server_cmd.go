package runner

// codex_app_server_cmd.go builds the `codex app-server` subprocess command:
// resolving the configured command string, splitting it safely, and rejecting
// shell-syntax that the direct-exec path cannot honor. The protocol client
// that drives the launched process lives in codex_app_server.go.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// buildCodexAppServerCmd returns the codex app-server *exec.Cmd plus a
// directCodexExec flag. The flag is true only when the command launches the
// `codex app-server` binary directly (so `cmd.Process.Pid` after Start() is
// the actual app-server pid). Custom wrapper commands route through
// `sh -c <command>`, in which case `cmd.Process.Pid` is the shell wrapper, not
// codex — see codex_app_server.go's session_started emit for the resulting
// PID-emission guard.
func buildCodexAppServerCmd(ctx context.Context, in RunInput, env []string) (*exec.Cmd, bool, error) {
	return NewCodexAppServerCommand(ctx, in.Workflow.Config, env)
}

// NewCodexAppServerCommand returns the configured Codex app-server command plus
// whether it directly execs the codex binary. Callers that preflight or run the
// app-server must share this path so codex.command overrides behave identically.
func NewCodexAppServerCommand(ctx context.Context, cfg workflow.Config, env []string) (*exec.Cmd, bool, error) {
	command := strings.TrimSpace(cfg.Codex.Command)
	if command == "" || command == "codex exec" {
		command = "codex app-server"
	}
	// A codex-prefixed command with no shell syntax execs the codex binary
	// directly. This keeps the common case (including args like
	// `codex app-server --config "..."`) off any shell, so it stays
	// cross-platform: PR #414 briefly routed every non-default command through
	// `sh -c` and regressed Windows deployments that set a codex-prefixed
	// codex.command (#417 restored this direct path; #471 pins it).
	args, err := splitAppServerCommand(command)
	if err == nil && len(args) >= 2 && args[0] == "codex" && args[1] == "app-server" && !hasShellSyntax(command) {
		codexPath, err := lookPathInEnv("codex", env)
		if err != nil {
			return nil, false, NewError(CategoryCodexNotFound, "codex binary not found in PATH; install codex CLI or set agent.default to claude/mock", err)
		}
		return exec.CommandContext(ctx, codexPath, args[1:]...), true, nil
	}
	// Commands that need a shell (wrappers, pipelines, globs) fall back to
	// `sh -c`. This is intentionally Unix-only: it matches the linux/darwin
	// release matrix and upstream Symphony, which spawns `bash -lc <command>`
	// unconditionally (elixir/lib/symphony_elixir/codex/app_server.ex). Windows
	// is not a supported deployment target; codex-prefixed commands take the
	// cross-platform direct path above.
	return exec.CommandContext(ctx, "sh", "-c", command), false, nil
}
func splitAppServerCommand(command string) ([]string, error) {
	var args []string
	var current strings.Builder
	var quote rune
	tokenStarted := false
	runes := []rune(command)

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote != 0:
			switch {
			case r == quote:
				quote = 0
				tokenStarted = true
			case quote == '"' && r == '\\' && i+1 < len(runes) && strings.ContainsRune("$`\"\\\n", runes[i+1]):
				i++
				current.WriteRune(runes[i])
				tokenStarted = true
			default:
				current.WriteRune(r)
				tokenStarted = true
			}
		case r == '\\':
			current.WriteRune(r)
			tokenStarted = true
		case r == '\'' || r == '"':
			quote = r
			tokenStarted = true
		case isCommandSpace(r):
			if tokenStarted {
				args = append(args, current.String())
				current.Reset()
				tokenStarted = false
			}
		default:
			current.WriteRune(r)
			tokenStarted = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("parse codex.command")
	}
	if tokenStarted {
		args = append(args, current.String())
	}
	return args, nil
}
func hasShellSyntax(command string) bool {
	var quote rune
	tokenBoundary := true
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '$', '`', '\n':
				return true
			case '\\':
				if i+1 < len(runes) && runes[i+1] == '\n' {
					return true
				}
				i++
			}
		case r == '\'' || r == '"':
			quote = r
			tokenBoundary = false
		case r == '\n' || r == '\r':
			return true
		case r == '#':
			if tokenBoundary {
				return true
			}
			tokenBoundary = false
		case isCommandSpace(r):
			tokenBoundary = true
			continue
		case r == '\\':
			return true
		case strings.ContainsRune("|&;<>$()`{}[]*?~", r):
			return true
		default:
			tokenBoundary = false
		}
	}
	return quote != 0
}
func isCommandSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}
