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
