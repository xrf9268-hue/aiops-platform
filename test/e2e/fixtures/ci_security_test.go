package fixtures_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func readCIWorkflow(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", ".github", "workflows", "ci.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestCIRunsSecurityScanning guards the #372 security/supply-chain coverage:
// the workflow must run govulncheck, an explicit go vet, and a Trivy image
// scan + SBOM. Deleting any one fails this test.
func TestCIRunsSecurityScanning(t *testing.T) {
	ci := readCIWorkflow(t)
	for _, want := range []string{
		"govulncheck",
		"go vet ./...",
		"trivy-action",
		"cyclonedx",
	} {
		if !strings.Contains(ci, want) {
			t.Errorf("ci.yml is missing %q", want)
		}
	}
}

// TestCIRunsReportOnlyGoLint guards #413's rollout model: newly earned Go
// engineering rules need a machine-visible golangci-lint gate, but the initial
// baseline must be report-only so existing violations can be burned down in
// follow-up PRs.
func TestCIRunsReportOnlyGoLint(t *testing.T) {
	ci := readCIWorkflow(t)
	const stepName = "      - name: Run golangci-lint (report only)"
	stepStart := strings.Index(ci, stepName)
	if stepStart < 0 {
		t.Fatalf("ci.yml is missing %q", stepName)
	}
	step := ci[stepStart:]
	if next := strings.Index(step[len(stepName):], "\n      - name:"); next >= 0 {
		step = step[:len(stepName)+next]
	}
	for _, want := range []string{
		"golangci/golangci-lint-action@",
		"version: v2.",
		"--config=.golangci.yml",
		"--issues-exit-code=0",
	} {
		if !strings.Contains(step, want) {
			t.Errorf("golangci-lint step is missing report-only marker %q", want)
		}
	}
	if strings.Contains(step, "continue-on-error") {
		t.Errorf("golangci-lint step must not mask configuration or action failures with continue-on-error")
	}
}

// TestCIActionsArePinnedToSHA guards supply-chain hardening (#372): every
// third-party action `uses:` must reference a 40-hex commit SHA, not a movable
// tag like @v6. A reverted pin re-introduces the mutable-tag risk and fails.
func TestCIActionsArePinnedToSHA(t *testing.T) {
	ci := readCIWorkflow(t)
	usesRE := regexp.MustCompile(`uses:\s*([^\s]+)`)
	sha := regexp.MustCompile(`^[0-9a-f]{40}$`)
	matches := usesRE.FindAllStringSubmatch(ci, -1)
	if len(matches) == 0 {
		t.Fatal("ci.yml has no `uses:` action references")
	}
	for _, m := range matches {
		ref := m[1]
		// Local actions / reusable workflows (e.g. ./.github/workflows/x.yml)
		// have no @ref and are not a third-party supply-chain pin concern.
		if strings.HasPrefix(ref, "./") {
			continue
		}
		at := strings.LastIndex(ref, "@")
		if at < 0 {
			t.Errorf("action %q is not version-pinned", ref)
			continue
		}
		if !sha.MatchString(ref[at+1:]) {
			t.Errorf("action %q is not pinned to a 40-hex commit SHA", ref)
		}
	}
}
