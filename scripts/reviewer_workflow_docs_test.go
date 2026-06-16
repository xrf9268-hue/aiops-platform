package scripts

import (
	"os"
	"strings"
	"testing"
)

func TestReviewerProtocolDocumentsSubagentDiscoveryBeforeCLIFallback(t *testing.T) {
	protocol := readReviewerDoc(t, "../docs/runbooks/pr-review-merge-protocol.md")

	for _, want := range []string{
		"Codex inline",
		"codex-sub-agent",
		"Claude Code Agent",
		"tool_search",
		"spawn_agent",
		"before CLI fallback",
		"current user request explicitly authorizes subagent delegation and the tool contract allows spawning for that request",
		"workflow prompt or runbook instruction alone is not sufficient authorization",
		"If subagent use is not authorized",
		"do not spawn",
		"Do not recursively spawn reviewers unless the parent task explicitly asks",
		"no authorized and appropriate Codex-family subagent path is available",
		"codex exec --output-schema",
		"Do not use",
		"codex exec review --base",
		"for this gate",
		"--output-format json",
		"--json-schema",
		"--max-turns 6",
		".structured_output",
		"Subagent review complements the two-family gate",
		"Codex-family and Claude-family",
		"Best-practice reviewer modes",
		"Subagents unavailable / opted out",
		"One sidecar subagent",
		"Parallel specialized subagents",
		"#891",
		"an available subagent reviewer",
		"subagent review is the default there (#900)",
		"CLI review only",
		"Codex inline is different",
	} {
		if !containsReviewerDocText(protocol, want) {
			t.Fatalf("pr-review-merge-protocol.md missing %q", want)
		}
	}
}

func TestIssueAndPRSkillsPointToSubagentFirstReviewerRouting(t *testing.T) {
	for _, path := range []string{
		"../.claude/skills/handle-issue/SKILL.md",
		"../.claude/skills/handle-pr/SKILL.md",
	} {
		body := readReviewerDoc(t, path)
		for _, want := range []string{
			"subagent-first reviewer routing",
			"CLI review only",
			"pr-review-merge-protocol.md",
		} {
			if !containsReviewerDocText(body, want) {
				t.Fatalf("%s missing %q", path, want)
			}
		}
	}
}

func TestDogfoodGuidancePointsToCanonicalReviewerProtocol(t *testing.T) {
	body := readReviewerDoc(t, "../docs/runbooks/dogfood-development.md")

	for _, want := range []string{
		"subagent-first reviewer routing",
		"pr-review-merge-protocol.md",
		"second source of truth",
	} {
		if !containsReviewerDocText(body, want) {
			t.Fatalf("dogfood-development.md missing %q", want)
		}
	}
}

func TestGitHubLocalWorkflowPromptPointsToCanonicalReviewerProtocol(t *testing.T) {
	body := workflowPromptBody(t, "../examples/github-local-WORKFLOW.md", readReviewerDoc(t, "../examples/github-local-WORKFLOW.md"))

	for _, want := range []string{
		"pr-review-merge-protocol.md",
		"subagent-first reviewer routing",
		"CLI fallback conditions",
		"Do not inline reviewer command mechanics here",
	} {
		if !containsReviewerDocText(body, want) {
			t.Fatalf("github-local-WORKFLOW.md prompt missing %q", want)
		}
	}
}

func TestTrellisGuidancePointsToCanonicalReviewerProtocol(t *testing.T) {
	body := readReviewerDoc(t, "../.trellis/spec/backend/agent-workflow-guidelines.md")

	for _, want := range []string{
		"pr-review-merge-protocol.md",
		"single source of truth",
		"Do not restate reviewer-routing mechanics",
	} {
		if !containsReviewerDocText(body, want) {
			t.Fatalf("agent-workflow-guidelines.md missing %q", want)
		}
	}
}

func TestConcreteReviewerRoutingMechanicsStayInProtocol(t *testing.T) {
	nonCanonicalDocs := []string{
		"../.claude/skills/handle-issue/SKILL.md",
		"../.claude/skills/handle-pr/SKILL.md",
		"../docs/runbooks/dogfood-development.md",
		"../docs/runbooks/github-local-automation.md",
		"../examples/github-local-WORKFLOW.md",
		"../.trellis/spec/backend/agent-workflow-guidelines.md",
	}
	for _, path := range nonCanonicalDocs {
		body := reviewerMechanicsSearchText(t, path)
		for _, forbidden := range []string{
			"tool_search",
			"spawn_agent",
			"multi_agent",
			"codex exec review",
			"flag 互斥",
			"codex exec --output-schema",
			"claude -p",
			`--tools ""`,
			"--json-schema",
			"--ephemeral",
			"--no-session-persistence",
		} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("%s duplicates protocol-only reviewer mechanic %q", path, forbidden)
			}
		}
	}
}

func TestLocalPRFollowThroughCodexReviewerUsesStructuredExec(t *testing.T) {
	body := readReviewerDoc(t, "local-pr-follow-through.sh")
	fn := shellFunctionBody(t, body, "run_codex_review")

	for _, want := range []string{
		`run_with_timeout "$review_timeout" codex exec`,
		"--output-schema \"$schema_file\"",
		`-o "$review_file"`,
		"--sandbox read-only",
		"--ephemeral",
	} {
		if !strings.Contains(fn, want) {
			t.Fatalf("run_codex_review missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"codex exec review",
		"review --base",
	} {
		if strings.Contains(fn, forbidden) {
			t.Fatalf("run_codex_review uses forbidden Codex review path %q", forbidden)
		}
	}
}

func readReviewerDoc(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(body)
}

func reviewerMechanicsSearchText(t *testing.T, path string) string {
	t.Helper()
	body := readReviewerDoc(t, path)
	if strings.HasSuffix(path, "WORKFLOW.md") {
		return workflowPromptBody(t, path, body)
	}
	return body
}

func workflowPromptBody(t *testing.T, path, body string) string {
	t.Helper()
	if !strings.HasPrefix(body, "---\n") {
		return body
	}
	rest := body[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		t.Fatalf("%s front matter is not terminated", path)
	}
	return rest[end+len("\n---\n"):]
}

func shellFunctionBody(t *testing.T, body, name string) string {
	t.Helper()
	startMarker := name + "() {\n"
	start := strings.Index(body, startMarker)
	if start < 0 {
		t.Fatalf("shell function %s not found", name)
	}
	body = body[start+len(startMarker):]
	endMarker := "\n}\n\n"
	end := strings.Index(body, endMarker)
	if end < 0 {
		t.Fatalf("shell function %s end not found", name)
	}
	return body[:end]
}

func containsReviewerDocText(body, want string) bool {
	return strings.Contains(normalizeReviewerDocText(body), normalizeReviewerDocText(want))
}

func normalizeReviewerDocText(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
