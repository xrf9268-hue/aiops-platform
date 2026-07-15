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
			t.Errorf("security-posture.md normalized text missing %q", want)
		}
	}
	frontmatter := normalizeSandboxGuidance(readFileString(t, filepath.Join(root, "docs", "runbooks", "workflow-frontmatter-reference.md")))
	for _, want := range []string{
		"the worker process sandbox and codex turn sandbox are separate layers",
		"neither layer accepts repository-relative allow or deny paths",
	} {
		if !strings.Contains(frontmatter, want) {
			t.Errorf("workflow-frontmatter-reference.md normalized text missing %q", want)
		}
	}
}

func scanUnsupportedSandboxClaims(root string) ([]string, error) {
	patterns := []string{
		"AGENTS.md", "DEVIATIONS.md", "README.md",
		"docs/*.md", "docs/adr/*.md", "docs/runbooks/*.md", "docs/workflows/*.md",
		"examples/*.md", "internal/workflow/*.go",
	}
	forbidden := []string{
		"hard path prevention belongs to",
		"for hard path prevention",
		"for hard prevention",
		"restrict writes via the `sandbox:` block",
		"`sandbox:` write restrictions keep changes out",
		"keep the `sandbox:` write restrictions",
		"enforced preventively by the agent",
		"basic path policy",
		"basic deny-path policy",
		"deny sensitive paths in company repositories",
	}
	var violations []string
	for _, pattern := range patterns {
		paths, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			body, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil, err
			}
			normalized := normalizeSandboxGuidance(string(body))
			for _, phrase := range forbidden {
				if strings.Contains(normalized, phrase) {
					violations = append(violations, rel+": "+phrase)
				}
			}
		}
	}
	return violations, nil
}

func normalizeSandboxGuidance(text string) string {
	text = strings.ReplaceAll(text, "#", " ")
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
	t.Fatalf("security-posture.md missing sandbox boundary row for %q", layer)
	return nil
}
