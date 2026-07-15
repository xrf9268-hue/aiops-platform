package scripts_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestSandboxGuidanceDoesNotPromiseRepositorySubpathEnforcement(t *testing.T) {
	root := repoRoot(t)
	assertSandboxBoundaryContract(t, root)
	assertSandboxGuidanceText(t, root)
	violations, err := scanUnsupportedSandboxClaims(root)
	if err != nil {
		t.Fatalf("scan sandbox guidance: %v", err)
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("sandbox guidance contains unsupported repository-subpath enforcement claims:\n%s", strings.Join(violations, "\n"))
	}
}

func assertSandboxBoundaryContract(t *testing.T, root string) {
	t.Helper()
	securityBody := readFileString(t, filepath.Join(root, "docs", "security-posture.md"))
	for _, tc := range []struct {
		layer         string
		writableScope string
	}{
		{layer: "codex `workspacewrite`", writableScope: "issue workspace"},
		{layer: "worker `sandbox:`", writableScope: "whole issue workspace"},
	} {
		row := findSandboxBoundaryRow(t, securityBody, tc.layer)
		if !strings.Contains(strings.ToLower(row[1]), tc.writableScope) {
			t.Errorf("%s writable scope = %q; want substring %q", tc.layer, row[1], tc.writableScope)
		}
		if got, want := strings.ToLower(strings.TrimSpace(row[2])), "none"; got != want {
			t.Errorf("%s repository-subpath policy = %q; want %q", tc.layer, got, want)
		}
	}
}

func assertSandboxGuidanceText(t *testing.T, root string) {
	t.Helper()
	securityBody := readFileString(t, filepath.Join(root, "docs", "security-posture.md"))
	security := normalizeSandboxGuidance(securityBody)
	for _, want := range []string{"prompt path rules are advisory", "not a security boundary", "untrusted issue authors", "untrusted repositories"} {
		if !strings.Contains(security, want) {
			t.Errorf("strings.Contains(security-posture.md, %q) = false; want true", want)
		}
	}
	frontmatter := normalizeSandboxGuidance(readFileString(t, filepath.Join(root, "docs", "runbooks", "workflow-frontmatter-reference.md")))
	for _, want := range []string{
		"the worker process sandbox and codex turn sandbox are separate layers",
		"neither layer accepts repository-relative allow or deny paths",
		"worker-injected `gocache` and `gomodcache`",
		"firejail may still expose host paths accessible to the worker os user",
	} {
		if !strings.Contains(frontmatter, want) {
			t.Errorf("strings.Contains(workflow-frontmatter-reference.md, %q) = false; want true", want)
		}
	}
}

func scanUnsupportedSandboxClaims(root string) ([]string, error) {
	type retiredClaim struct{ path, text string }
	claims := []retiredClaim{
		{"DEVIATIONS.md", "SPEC §3.2 homes scope/validation rules in the operator's `WORKFLOW.md` prompt, hard path prevention belongs to `sandbox:` write restrictions, and upstream has no such config"},
		{"examples/WORKFLOW.md", "Scope and path rules (which files to keep changes within, which paths to avoid) belong in the prompt body below (SPEC §3.2); hard path prevention belongs to the `sandbox:` write restrictions"},
		{"examples/gitea-WORKFLOW.md", "Scope and path rules belong in the prompt body (SPEC §3.2); hard path prevention belongs to `sandbox:` write restrictions"},
		{"examples/github-local-WORKFLOW.md", "Scope and path rules belong in the prompt body (SPEC §3.2); hard path prevention belongs to `sandbox:` write restrictions"},
		{"docs/workflows/company-cautious-WORKFLOW.md", "For HARD path prevention on a company repo, restrict writes via the `sandbox:` block"},
		{"docs/workflows/company-cautious-WORKFLOW.md", "prompt + `sandbox:` write restrictions keep changes out of the directories you expect"},
		{"docs/workflows/company-cautious-WORKFLOW.md", "a tight size budget in the prompt, and keep the `sandbox:` write restrictions conservative"},
		{"docs/workflows/company-cautious-WORKFLOW.md", "without authoring any code, so you can validate policy guardrails before letting a real model touch the repository"},
		{"docs/workflows/company-cautious-WORKFLOW.md", "draft_pr keeps every change behind human review even after you graduate from analysis-only"},
		{"docs/runbooks/personal-daily-workflow.md", "so the agent self-limits; use `sandbox:` write restrictions for hard prevention"},
		{"docs/security-posture.md", "off-limits in the `WORKFLOW.md` prompt (SPEC §3.2) and, for hard prevention, restrict writes via the `sandbox:` block"},
		{"docs/security-posture.md", "mock loop has proven clone, branch, PR, label, and policy behavior"},
		{"docs/security-posture.md", "Unless the optional sandbox wrapper is enabled and validated on the worker host, do not assume the platform prevents a malicious or compromised agent run from"},
		{"docs/security-posture.md", "- draft-PR mode for human review before merge;"},
		{"docs/runbooks/gitea-bot-and-branch-protection.md", "Scope and path constraints now live in the operator's `WORKFLOW.md` prompt (SPEC §3.2), enforced preventively by the agent before push"},
		{"docs/adr/0001-symphony-style-personal-orchestrator.md", "- basic deny-path policy - verification commands"},
		{"docs/adr/0001-symphony-style-personal-orchestrator.md", "- deny sensitive paths in company repositories - do not automatically merge"},
		{"docs/symphony-integration.md", "- basic path policy - verification commands"},
		{"docs/runbooks/workflow-frontmatter-reference.md", "the agent process, environment, credential mounts, network, and visibility of host paths"},
		{"docs/runbooks/workflow-frontmatter-reference.md", "Exact env vars the sandboxed child keeps; same tracker-token deny-list as `env_passthrough`"},
		{"internal/workflow/config.go", "and upstream has no such config. Hard path prevention belongs to the `sandbox` write restrictions; scope guidance belongs to the prompt"},
		{"internal/workflow/reject.go", "Express scope/path rules in the WORKFLOW prompt (SPEC §3.2) and use sandbox write restrictions for hard path prevention"},
		{"examples/github-local-WORKFLOW.md", "Keep changes small enough for review. Respect the configured policy limits unless the issue explicitly requires a larger change"},
	}
	var violations []string
	for _, claim := range claims {
		body, err := os.ReadFile(filepath.Join(root, claim.path))
		if err != nil {
			return nil, err
		}
		if strings.Contains(normalizeSandboxGuidance(string(body)), normalizeSandboxGuidance(claim.text)) {
			violations = append(violations, claim.path+": "+claim.text)
		}
	}
	return violations, nil
}

func normalizeSandboxGuidance(text string) string {
	text = strings.NewReplacer("#", " ", "//", " ").Replace(text)
	return strings.ToLower(strings.Join(strings.Fields(text), " "))
}

func findSandboxBoundaryRow(t *testing.T, body, layer string) []string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) != 3 {
			continue
		}
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if strings.EqualFold(cells[0], layer) {
			return cells
		}
	}
	t.Fatalf("findSandboxBoundaryRow(layer=%q) found no matching 3-cell row; want one", layer)
	return nil
}
