package aiopsplatform_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// conventionalCommitTypes mirrors the allow-list in
// .github/workflows/pr-title-lint.yml and AGENTS.md → "Commit / PR-title
// convention". Dependabot turns commit-message.prefix into the PR-title prefix
// ("<prefix>: …"), and the required `Validate PR title (Conventional Commits)`
// check rejects any non-conventional type — so a prefix outside this set would
// silently brick every Dependabot PR.
var conventionalCommitTypes = []string{
	"feat", "fix", "perf", "refactor", "docs", "style",
	"test", "build", "ci", "chore", "revert",
}

// isConventionalCommitPrefix reports whether a Dependabot commit-message prefix
// is a Conventional Commit type with an optional (scope) — e.g. "chore(deps)" or
// "build".
func isConventionalCommitPrefix(prefix string) bool {
	typePart := prefix
	if i := strings.IndexByte(prefix, '('); i >= 0 {
		if !strings.HasSuffix(prefix, ")") {
			return false
		}
		typePart = prefix[:i]
	}
	return slices.Contains(conventionalCommitTypes, typePart)
}

type dependabotConfig struct {
	Updates []dependabotUpdate `yaml:"updates"`
}

type dependabotUpdate struct {
	PackageEcosystem      string                     `yaml:"package-ecosystem"`
	Directory             string                     `yaml:"directory"`
	Schedule              dependabotSchedule         `yaml:"schedule"`
	OpenPullRequestsLimit int                        `yaml:"open-pull-requests-limit"`
	Labels                []string                   `yaml:"labels"`
	CommitMessage         dependabotCommitMessage    `yaml:"commit-message"`
	Groups                map[string]dependabotGroup `yaml:"groups"`
}

type dependabotSchedule struct {
	Interval string `yaml:"interval"`
	Day      string `yaml:"day"`
	Time     string `yaml:"time"`
	Timezone string `yaml:"timezone"`
}

type dependabotCommitMessage struct {
	Prefix string `yaml:"prefix"`
}

type dependabotGroup struct {
	Patterns []string `yaml:"patterns"`
}

func TestDependabotCoversDashboardNPMDependencies(t *testing.T) {
	body, err := os.ReadFile(".github/dependabot.yml")
	if err != nil {
		t.Fatalf("read dependabot config: %v", err)
	}

	var config dependabotConfig
	if err := yaml.Unmarshal(body, &config); err != nil {
		t.Fatalf("parse dependabot config: %v", err)
	}

	var dashboardNPM *dependabotUpdate
	for i := range config.Updates {
		update := &config.Updates[i]
		if update.PackageEcosystem == "npm" && update.Directory == "/cmd/worker/dashboard" {
			dashboardNPM = update
			break
		}
	}
	if dashboardNPM == nil {
		t.Fatal("dependabot config must cover dashboard npm dependencies")
	}

	if dashboardNPM.Schedule.Interval != "weekly" ||
		dashboardNPM.Schedule.Day != "friday" ||
		dashboardNPM.Schedule.Time != "09:30" ||
		dashboardNPM.Schedule.Timezone != "America/Los_Angeles" {
		t.Fatalf("unexpected dashboard npm schedule: %+v", dashboardNPM.Schedule)
	}
	if dashboardNPM.OpenPullRequestsLimit != 5 {
		t.Fatalf("unexpected dashboard npm open PR limit: %d", dashboardNPM.OpenPullRequestsLimit)
	}
	for _, label := range []string{"dependencies", "area:observability"} {
		if !slices.Contains(dashboardNPM.Labels, label) {
			t.Fatalf("dashboard npm config missing label %q in %v", label, dashboardNPM.Labels)
		}
	}
	if !isConventionalCommitPrefix(dashboardNPM.CommitMessage.Prefix) {
		t.Fatalf("dashboard npm commit prefix %q is not a Conventional Commit type (Dependabot titles would fail the PR-title lint)", dashboardNPM.CommitMessage.Prefix)
	}
	group, ok := dashboardNPM.Groups["npm-dependencies"]
	if !ok {
		t.Fatalf("dashboard npm config missing npm-dependencies group: %v", dashboardNPM.Groups)
	}
	if !slices.Contains(group.Patterns, "*") {
		t.Fatalf("dashboard npm group must cover all packages, got %v", group.Patterns)
	}
}

// TestDependabotPrefixesAreConventionalCommitTypes guards the coupling codex
// flagged on #838: every Dependabot ecosystem's commit-message.prefix must be a
// Conventional Commit type so its generated PR titles pass the required
// `Validate PR title (Conventional Commits)` check. A bare `deps:` (the prior
// value) would leave every dependency/security-update PR stuck on that gate.
func TestDependabotPrefixesAreConventionalCommitTypes(t *testing.T) {
	body, err := os.ReadFile(".github/dependabot.yml")
	if err != nil {
		t.Fatalf("read dependabot config: %v", err)
	}
	var config dependabotConfig
	if err := yaml.Unmarshal(body, &config); err != nil {
		t.Fatalf("parse dependabot config: %v", err)
	}
	if len(config.Updates) == 0 {
		t.Fatal("dependabot config declares no updates")
	}
	for _, update := range config.Updates {
		prefix := update.CommitMessage.Prefix
		if !isConventionalCommitPrefix(prefix) {
			t.Errorf("%s (%s): commit-message.prefix %q is not a Conventional Commit type; Dependabot PR titles would fail the required PR-title lint — use e.g. chore(deps) or build(deps)",
				update.PackageEcosystem, update.Directory, prefix)
		}
	}
}
