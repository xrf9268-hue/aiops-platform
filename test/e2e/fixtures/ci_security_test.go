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

// TestCIRunsTwoPhaseGoLint guards #413/#433/#459's rollout model: clean
// mechanical linters block CI, while the complexity baseline stays report-only.
func TestCIRunsTwoPhaseGoLint(t *testing.T) {
	ci := readCIWorkflow(t)

	blocking := workflowStep(t, ci, "Run golangci-lint blocking correctness gate")
	for _, want := range []string{
		"golangci/golangci-lint-action@",
		"version: v2.",
		"--config=.golangci.yml",
		"--enable-only=contextcheck,errcheck,errorlint,gocritic,govet,ineffassign,revive,staticcheck,unparam,unused",
	} {
		if !strings.Contains(blocking, want) {
			t.Errorf("blocking golangci-lint step = %q, want marker %q", blocking, want)
		}
	}
	if strings.Contains(blocking, "--issues-exit-code=0") {
		t.Errorf("blocking golangci-lint step = %q, want no report-only marker", blocking)
	}

	reportOnly := workflowStep(t, ci, "Run golangci-lint report-only baseline")
	for _, want := range []string{
		"golangci/golangci-lint-action@",
		"version: v2.",
		"--config=.golangci.yml",
		"--enable-only=funlen,gocognit",
		"--issues-exit-code=0",
	} {
		if !strings.Contains(reportOnly, want) {
			t.Errorf("report-only golangci-lint step = %q, want marker %q", reportOnly, want)
		}
	}
	if strings.Contains(reportOnly, "continue-on-error") {
		t.Errorf("report-only golangci-lint step = %q, want no continue-on-error", reportOnly)
	}
}

func workflowStep(t *testing.T, workflow, name string) string {
	t.Helper()
	stepName := "      - name: " + name
	stepStart := strings.Index(workflow, stepName)
	if stepStart < 0 {
		t.Fatalf("ci.yml is missing step %q", name)
	}
	step := workflow[stepStart:]
	if next := strings.Index(step[len(stepName):], "\n      - name:"); next >= 0 {
		step = step[:len(stepName)+next]
	}
	return step
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
