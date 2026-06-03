package aiopsplatform_test

import (
	"os"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

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
		return
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
	if dashboardNPM.CommitMessage.Prefix != "deps" {
		t.Fatalf("unexpected dashboard npm commit prefix: %q", dashboardNPM.CommitMessage.Prefix)
	}
	group, ok := dashboardNPM.Groups["npm-dependencies"]
	if !ok {
		t.Fatalf("dashboard npm config missing npm-dependencies group: %v", dashboardNPM.Groups)
	}
	if !slices.Contains(group.Patterns, "*") {
		t.Fatalf("dashboard npm group must cover all packages, got %v", group.Patterns)
	}
}
