package runner

// codex_app_server_cmd_test.go holds characterization tests that pin
// hasShellSyntax's input->bool contract at its own function boundary before
// #499 decomposes the 19-cognitive-complexity quote/metacharacter scanner into
// per-quote-state helpers. hasShellSyntax decides whether a codex.command takes
// the cross-platform direct-exec path or falls back to `sh -c`, so a routing
// regression here silently changes process spawning; the assertions below must
// hold identically before and after that refactor.
//
// The end-to-end TestBuildCodexAppServerCmd* tests remain the authority for the
// command-building integration (cmd.Args, direct vs. sh -c); these exercise the
// scanner directly, including the per-metacharacter set and the quoted-state
// transitions the indirect suite only samples.

import "testing"

// shellMetacharacters is the unquoted single-rune set hasShellSyntax flags via
// its strings.ContainsRune branch (line 144 of codex_app_server_cmd.go). '#' is
// intentionally excluded: it is governed by the separate token-boundary rule,
// not this set.
const shellMetacharacters = "|&;<>$()`{}[]*?~"

func TestHasShellSyntax(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{"empty", "", false},
		{"plain default", "codex app-server", false},
		{"plain with dash args", "codex app-server --config model", false},
		{"equals and dots unquoted", "codex app-server --config model=gpt-5.2", false},

		// Quoted spans neutralize metacharacters.
		{"double-quoted space", `codex app-server --config "model = gpt-5"`, false},
		{"single-quoted metachars", `codex app-server --config 'a|b;c(d)'`, false},
		{"double-quoted pipe and semicolon", `codex app-server --config "a|b;c"`, false},
		{"double-quoted glob and parens", `codex app-server --config "a*b(c)"`, false},

		// Backslash handling.
		{"unquoted backslash", `codex app-server --config foo\ bar`, true},
		{"unquoted backslash before letter", `codex app-server --config foo\z`, true},
		{"unquoted windows path", `codex app-server --config C:\Users\agent`, true},
		{"double-quoted backslash before letter", `codex app-server --cd "C:\proj"`, false},
		{"double-quoted escaped quote", `codex app-server --config "a\"b"`, false},
		// The backslash escape must defuse an otherwise-active double-quote
		// special ($ / backtick), not only consume a quote char.
		{"double-quoted backslash escapes dollar", `codex app-server "a\$b"`, false},
		{"double-quoted backslash escapes backtick", "codex app-server \"a\\`b\"", false},
		{"double-quoted backslash line-continuation", "codex app-server --config \"a\\\nb\"", true},

		// Comment rune: only at a token boundary.
		{"hash at token boundary after space", "codex app-server --config x # note", true},
		{"hash at string start", "#!/bin/sh", true},
		{"hash mid-token", "codex app-server --config release#2026.toml", false},

		// Newlines and carriage returns.
		{"unquoted newline", "codex app-server\nfoo", true},
		{"unquoted carriage return", "codex app-server\rfoo", true},
		{"literal newline inside double quote", "codex app-server --config \"a\nb\"", true},
		{"carriage return inside double quote", "codex app-server --config \"a\rb\"", false},

		// Unterminated quotes report shell syntax (the trailing quote != 0).
		{"unterminated double quote", `codex app-server --config "unterminated`, true},
		{"unterminated single quote", `codex app-server --config 'unterminated`, true},
		{"lone single quote", `'`, true},
		{"balanced empty single quotes", `codex app-server ''`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasShellSyntax(tt.command); got != tt.want {
				t.Errorf("hasShellSyntax(%q) = %v; want %v", tt.command, got, tt.want)
			}
		})
	}
}

// TestHasShellSyntax_MetacharacterSet pins every rune in the unquoted
// metacharacter set: flagged unquoted, neutralized inside single quotes.
func TestHasShellSyntax_MetacharacterSet(t *testing.T) {
	for _, r := range shellMetacharacters {
		m := string(r)
		t.Run("unquoted_"+m, func(t *testing.T) {
			cmd := "codex app-server " + m
			if got := hasShellSyntax(cmd); !got {
				t.Errorf("hasShellSyntax(%q) = %v; want true (unquoted %q is shell syntax)", cmd, got, m)
			}
		})
		t.Run("single_quoted_"+m, func(t *testing.T) {
			cmd := "codex app-server '" + m + "'"
			if got := hasShellSyntax(cmd); got {
				t.Errorf("hasShellSyntax(%q) = %v; want false (%q is neutralized inside single quotes)", cmd, got, m)
			}
		})
	}
}

// TestHasShellSyntax_DoubleQuoteSpecials pins the runes that hasShellSyntax
// flags even inside a double-quoted span: $, backtick, and a literal newline.
func TestHasShellSyntax_DoubleQuoteSpecials(t *testing.T) {
	for _, tt := range []struct {
		name    string
		command string
	}{
		{"dollar", `codex app-server "$x"`},
		{"backtick", "codex app-server \"`x`\""},
		{"command substitution", `codex app-server --config "$(cat cfg)"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasShellSyntax(tt.command); !got {
				t.Errorf("hasShellSyntax(%q) = %v; want true (%s is active inside double quotes)", tt.command, got, tt.name)
			}
		})
	}
	// Sanity: the same runes are inert inside single quotes.
	for _, cmd := range []string{`codex app-server '$x'`, "codex app-server '`x`'"} {
		if got := hasShellSyntax(cmd); got {
			t.Errorf("hasShellSyntax(%q) = %v; want false (inert inside single quotes)", cmd, got)
		}
	}
}
