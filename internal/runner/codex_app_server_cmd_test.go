package runner

// codex_app_server_cmd_test.go holds characterization tests that pin the
// codex.command parsing helpers (hasShellSyntax and splitAppServerCommand) at
// their own function boundaries before #499 decomposes them. hasShellSyntax
// decides whether a codex.command takes the cross-platform direct-exec path or
// falls back to `sh -c`, and splitAppServerCommand tokenizes the direct-exec
// argv; a regression in either silently changes process spawning, so the
// assertions below must hold identically before and after the refactor.
//
// The end-to-end TestBuildCodexAppServerCmd* tests remain the authority for the
// command-building integration (cmd.Args, direct vs. sh -c); these exercise the
// helpers directly, including branches the indirect suite only samples.

import (
	"reflect"
	"testing"
)

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

func TestSplitAppServerCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    []string
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"plain", "codex app-server", []string{"codex", "app-server"}, false},
		{"collapses surrounding and repeated spaces", "  spaced   out  ", []string{"spaced", "out"}, false},

		// Quoting joins a span into one token; adjacent quoted/unquoted concatenate.
		{"double-quoted space preserved", `codex app-server --config "model = gpt-5"`, []string{"codex", "app-server", "--config", "model = gpt-5"}, false},
		{"single-quoted span", `a 'b c' d`, []string{"a", "b c", "d"}, false},
		{"adjacent quoted and unquoted concatenate", `x"y"z`, []string{"xyz"}, false},
		{"empty quoted token is preserved", `a "" b`, []string{"a", "", "b"}, false},

		// Backslash: literal when unquoted; escapes only the allow-listed runes
		// inside double quotes.
		{"unquoted backslash is literal and does not escape the space", `a\ b`, []string{`a\`, "b"}, false},
		{"double-quoted backslash before a non-escape rune stays literal", `codex app-server --cd "C:\proj"`, []string{"codex", "app-server", "--cd", `C:\proj`}, false},
		{"double-quoted backslash escapes a quote", `a "b\"c" d`, []string{"a", `b"c`, "d"}, false},
		{"double-quoted backslash escapes a dollar", `a "b\$c"`, []string{"a", "b$c"}, false},
		{"double-quoted backslash before n is literal", `a "b\nc"`, []string{"a", `b\nc`}, false},

		// Unterminated quotes are a parse error.
		{"unterminated double quote", `"unterminated`, nil, true},
		{"unterminated single quote", `'unterminated`, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := splitAppServerCommand(tt.command)
			if (err != nil) != tt.wantErr {
				t.Fatalf("splitAppServerCommand(%q) err = %v; wantErr %v", tt.command, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitAppServerCommand(%q) = %#v; want %#v", tt.command, got, tt.want)
			}
		})
	}
}
