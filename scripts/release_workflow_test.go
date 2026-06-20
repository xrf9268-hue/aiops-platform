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
	pinned := regexp.MustCompile(`^[0-9a-f]{40}\s+#\s+v[0-9]+(?:\.[0-9]+)*(?:\s|$)`)
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
	for _, name := range []string{"actions/checkout", "actions/setup-go", "actions/setup-node"} {
		if !seen[name] {
			t.Fatalf("release workflow missing %s action", name)
		}
	}
}

func TestReleaseWorkflowBuildsDashboardBeforeWorker(t *testing.T) {
	body, err := os.ReadFile("../.github/workflows/release.yml")
	if err != nil {
		t.Fatalf("ReadFile(release.yml): %v", err)
	}

	text := string(body)
	setupNode := strings.Index(text, "- name: Set up Node")
	buildDashboard := strings.Index(text, "- name: Build dashboard")
	buildRelease := strings.Index(text, "- name: Build release binaries")
	if setupNode < 0 || buildDashboard < 0 || buildRelease < 0 {
		t.Fatalf("release workflow missing setup/build steps: setupNode=%d buildDashboard=%d buildRelease=%d", setupNode, buildDashboard, buildRelease)
	}
	if setupNode > buildDashboard || buildDashboard > buildRelease {
		t.Fatalf("release workflow must build dashboard before Go binaries: setupNode=%d buildDashboard=%d buildRelease=%d", setupNode, buildDashboard, buildRelease)
	}
	for _, want := range []string{"node-version: 22", "npm ci", "npm run build"} {
		if !strings.Contains(text, want) {
			t.Fatalf("release workflow missing %q", want)
		}
	}
}
