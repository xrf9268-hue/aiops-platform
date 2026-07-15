package scripts_test

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestClaimTokenBudgetDocsNameObservableScope(t *testing.T) {
	root := repoRoot(t)
	paths := []string{
		"README.md",
		"DEVIATIONS.md",
		"docs/runbooks/runtime-status.md",
		"docs/runbooks/task-api.md",
		"docs/runbooks/workflow-frontmatter-reference.md",
		"examples/github-maker-WORKFLOW.md",
		"examples/github-reviewer-automerge-WORKFLOW.md",
	}
	wants := []string{
		"worker-observed",
		"runner-reported codex",
		"external github",
		"@codex review",
		"unreported nested or subagent usage",
		"unmeasured, not zero",
		"max_tokens_per_claim",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			body := normalizeClaimTokenBudgetDocs(readFileString(t, filepath.Join(root, filepath.FromSlash(path))))
			for _, want := range wants {
				if !strings.Contains(body, want) {
					t.Errorf("strings.Contains(%s, %q) = false; want true", path, want)
				}
			}
		})
	}
}

func normalizeClaimTokenBudgetDocs(body string) string {
	return strings.Join(strings.Fields(strings.ToLower(body)), " ")
}
