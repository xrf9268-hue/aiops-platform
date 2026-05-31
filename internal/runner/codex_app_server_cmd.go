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
	if command == "" {
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

// commandTokenizer carries splitAppServerCommand's token-builder state across
// the rune scan so the per-quote-state helpers can advance it in place.
type commandTokenizer struct {
	args         []string
	current      strings.Builder
	quote        rune
	tokenStarted bool
}

func splitAppServerCommand(command string) ([]string, error) {
	var t commandTokenizer
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		if t.quote != 0 {
			i += t.consumeQuoted(runes, i)
			continue
		}
		t.consumeUnquoted(runes[i])
	}
	if t.quote != 0 {
		return nil, fmt.Errorf("parse codex.command")
	}
	t.flush()
	return t.args, nil
}

// consumeQuoted handles runes[i] inside a quoted span and returns how many extra
// runes to skip (1 for a consumed double-quote backslash escape). The matching
// quote closes the span; inside double quotes a backslash escapes only the
// allow-listed runes ($ ` " \ newline) — a backslash before anything else (or at
// end of input) stays literal, as does every other rune.
func (t *commandTokenizer) consumeQuoted(runes []rune, i int) int {
	r := runes[i]
	switch {
	case r == t.quote:
		t.quote = 0
		t.tokenStarted = true
	case t.quote == '"' && r == '\\' && i+1 < len(runes) && strings.ContainsRune("$`\"\\\n", runes[i+1]):
		t.current.WriteRune(runes[i+1])
		t.tokenStarted = true
		return 1
	default:
		t.current.WriteRune(r)
		t.tokenStarted = true
	}
	return 0
}

// consumeUnquoted handles one rune outside any quote: an unquoted backslash is a
// literal (quotes, not backslashes, group tokens here), a quote opens a span,
// whitespace flushes the current token, and any other rune extends it.
func (t *commandTokenizer) consumeUnquoted(r rune) {
	switch {
	case r == '\\':
		t.current.WriteRune(r)
		t.tokenStarted = true
	case r == '\'' || r == '"':
		t.quote = r
		t.tokenStarted = true
	case isCommandSpace(r):
		t.flush()
	default:
		t.current.WriteRune(r)
		t.tokenStarted = true
	}
}

// flush appends the current token (including an empty quoted one, since
// tokenStarted is set on quote-open) and resets the builder for the next token.
func (t *commandTokenizer) flush() {
	if t.tokenStarted {
		t.args = append(t.args, t.current.String())
		t.current.Reset()
		t.tokenStarted = false
	}
}

// shellSyntaxScan carries hasShellSyntax's quote and token-boundary state across
// the rune scan so each per-quote-state helper can advance it in place.
type shellSyntaxScan struct {
	quote         rune
	tokenBoundary bool
}

func hasShellSyntax(command string) bool {
	scan := shellSyntaxScan{tokenBoundary: true}
	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		switch scan.quote {
		case '\'':
			scan.scanInSingleQuote(runes[i])
		case '"':
			found, skip := scan.scanInDoubleQuote(runes, i)
			if found {
				return true
			}
			i += skip
		default:
			if scan.scanUnquoted(runes[i]) {
				return true
			}
		}
	}
	// An unterminated quote is itself shell syntax the direct-exec path cannot
	// honor.
	return scan.quote != 0
}

// scanInSingleQuote consumes one rune inside a single-quoted span, where only
// the closing quote is significant — every other rune is literal.
func (s *shellSyntaxScan) scanInSingleQuote(r rune) {
	if r == '\'' {
		s.quote = 0
	}
}

// scanInDoubleQuote consumes runes[i] inside a double-quoted span. It reports
// whether the rune reveals active shell syntax ($, backtick, a literal newline,
// or a backslash line-continuation) and how many extra runes to skip for a
// consumed backslash escape.
func (s *shellSyntaxScan) scanInDoubleQuote(runes []rune, i int) (found bool, skip int) {
	switch runes[i] {
	case '"':
		s.quote = 0
	case '$', '`', '\n':
		return true, 0
	case '\\':
		if i+1 < len(runes) && runes[i+1] == '\n' {
			return true, 0
		}
		return false, 1
	}
	return false, 0
}

// scanUnquoted consumes one rune outside any quotes, updating the
// token-boundary state the # comment rule depends on and reporting whether the
// rune is shell syntax that forces the sh -c fallback.
func (s *shellSyntaxScan) scanUnquoted(r rune) bool {
	switch {
	case r == '\'' || r == '"':
		s.quote = r
		s.tokenBoundary = false
	case r == '\n' || r == '\r':
		return true
	case r == '#':
		if s.tokenBoundary {
			return true
		}
		s.tokenBoundary = false
	case isCommandSpace(r):
		s.tokenBoundary = true
	case r == '\\':
		return true
	case strings.ContainsRune("|&;<>$()`{}[]*?~", r):
		return true
	default:
		s.tokenBoundary = false
	}
	return false
}
func isCommandSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r'
}
