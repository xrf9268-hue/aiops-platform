package scripts

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestReleaseWorkflowPinsGitHubActionsBySHA(t *testing.T) {
	body, err := os.ReadFile("../.github/workflows/release.yml")
	if err != nil {
		t.Fatalf("ReadFile(release.yml): %v", err)
	}

	actionUse := regexp.MustCompile(`uses:\s+(actions/[A-Za-z0-9_.-]+)@(.+)`)
	pinned := regexp.MustCompile(`^[0-9a-f]{40}\s+#\s+v[0-9]+(?:\s|$)`)
	seen := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		match := actionUse.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		seen[match[1]] = true
		if strings.HasPrefix(match[2], "v") {
			t.Fatalf("release workflow uses floating action tag: %s", strings.TrimSpace(line))
		}
		if !pinned.MatchString(match[2]) {
			t.Fatalf("release workflow action is not pinned to SHA with version comment: %s", strings.TrimSpace(line))
		}
	}
	for _, name := range []string{"actions/checkout", "actions/setup-go"} {
		if !seen[name] {
			t.Fatalf("release workflow missing %s action", name)
		}
	}
}
