package scripts_test

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestReadmeDocumentsNativeAgentLoopPositioning(t *testing.T) {
	root := repoRoot(t)
	readme := readFileString(t, filepath.Join(root, "README.md"))
	normalized := strings.Join(strings.Fields(readme), " ")

	for _, want := range []string{
		"## Choosing aiops-platform vs native agent loops",
		"tracker-first issue-to-workspace-to-agent-to-PR runtime",
		"session/thread-local agent capabilities",
		"Claude Code dynamic workflows",
		"Claude Code `/goal`",
		"Codex Goal mode",
		"Use aiops-platform when",
		"Prefer native workflow/goal mechanisms when",
		"Sources checked: 2026-06-29",
		"https://code.claude.com/docs/en/workflows",
		"https://code.claude.com/docs/en/goal",
		"https://developers.openai.com/codex/prompting#goal-mode",
		"https://developers.openai.com/codex/app/commands#set-or-manage-a-goal-with-goal",
		"https://developers.openai.com/codex/cli/slash-commands#set-or-view-a-task-goal-with-goal",
		"dynamic workflows can be useful manual research and decomposition tools",
		"not a worker plugin, runner mode, or platform integration target",
		"Do not integrate Claude Code dynamic workflows into the worker.",
		"Do not add a custom worker-owned goal loop, goal-aware runner, or goal evaluator.",
		"Do not add worker-owned push, PR, merge, approval, or tracker-write operations.",
		"Do not add metrics taxonomy, pair-aware preflight, distributed state, or queue changes here.",
	} {
		if !strings.Contains(readme, want) && !strings.Contains(normalized, want) {
			t.Fatalf("README missing positioning text %q", want)
		}
	}
}
