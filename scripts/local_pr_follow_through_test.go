package scripts

import (
	"os"
	"strings"
	"testing"
)

func TestLocalPRFollowThroughClaudeReviewIsDiffOnly(t *testing.T) {
	body, err := os.ReadFile("local-pr-follow-through.sh")
	if err != nil {
		t.Fatalf("ReadFile(local-pr-follow-through.sh): %v", err)
	}
	script := string(body)

	for _, want := range []string{
		`local max_turns="${AIOPS_CLAUDE_REVIEW_MAX_TURNS:-2}"`,
		`--tools ""`,
		`--permission-mode bypassPermissions`,
		`--no-session-persistence`,
		`--json-schema "$(cat "$schema_file")"`,
		`< "$prompt_file" > "$review_file"`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("local-pr-follow-through.sh missing %q", want)
		}
	}
	if strings.Contains(script, "--allowedTools") || strings.Contains(script, "--allowed-tools") {
		t.Fatal("Claude review gate must not grant repository tools; use supplied diff only")
	}
}

func TestLocalScriptsIncludeUserLocalBinForLaunchd(t *testing.T) {
	for _, path := range []string{"local-pr-follow-through.sh", "local-github-worker.sh"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
		if !strings.Contains(string(body), `$HOME/.local/bin`) {
			t.Fatalf("%s PATH must include $HOME/.local/bin so launchd can find Claude Code", path)
		}
	}
}

func TestInstallLaunchAgentsDefaultsPRFollowThroughToNoMerge(t *testing.T) {
	body, err := os.ReadFile("install-local-launchagents.sh")
	if err != nil {
		t.Fatalf("ReadFile(install-local-launchagents.sh): %v", err)
	}
	script := string(body)
	for _, want := range []string{
		`follow_auto_merge="${AIOPS_AUTO_MERGE:-0}"`,
		`<key>AIOPS_AUTO_MERGE</key>`,
		`<string>${follow_auto_merge}</string>`,
		`<key>AIOPS_REVIEW_TIMEOUT</key>`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("install-local-launchagents.sh missing %q", want)
		}
	}
}
