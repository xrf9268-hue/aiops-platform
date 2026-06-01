package workflow

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeTempWorkflow(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/WORKFLOW.md"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoad_PRDraftFromFrontMatter verifies the YAML key `pr.draft` is parsed
// into Config.PR.Draft. This is the schema knob #41 wires through to
// gitea.CreatePullRequest.

func TestLoadParsesServerPort(t *testing.T) {
	path := writeTempWorkflow(t, `---
server:
  port: 4567
repo:
  clone_url: git@example.com:owner/repo.git
tracker:
  kind: gitea
---
Prompt body
`)
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := wf.Config.Server.Port; got != 4567 {
		t.Fatalf("server.port = %d, want 4567", got)
	}
}

func TestDefaultConfigEnablesStateServerOnPrivateLoopbackPort(t *testing.T) {
	cfg := DefaultConfig()
	if got := cfg.Server.Port; got != 4000 {
		t.Fatalf("default server.port = %d, want 4000", got)
	}
}

func TestDefaultConfigBindsStateServerToLoopbackHost(t *testing.T) {
	if got := DefaultConfig().Server.Host; got != "127.0.0.1" {
		t.Fatalf("default server.host = %q, want 127.0.0.1", got)
	}
}

func TestLoadParsesServerHost(t *testing.T) {
	path := writeTempWorkflow(t, `---
server:
  host: 0.0.0.0
  port: 4000
repo:
  clone_url: git@example.com:owner/repo.git
tracker:
  kind: gitea
---
Prompt body
`)
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := wf.Config.Server.Host; got != "0.0.0.0" {
		t.Fatalf("server.host = %q, want 0.0.0.0", got)
	}
}

// TestLoadKeepsLoopbackHostWhenServerBlockOmitsHost guards that overlaying a
// server block that sets only port does not zero out the loopback default.
func TestLoadKeepsLoopbackHostWhenServerBlockOmitsHost(t *testing.T) {
	path := writeTempWorkflow(t, `---
server:
  port: 4567
repo:
  clone_url: git@example.com:owner/repo.git
tracker:
  kind: gitea
---
Prompt body
`)
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := wf.Config.Server.Host; got != "127.0.0.1" {
		t.Fatalf("server.host = %q, want 127.0.0.1 (default survives partial server block)", got)
	}
}

func TestLoadDefaultsServerPortWhenServerBlockIsOmitted(t *testing.T) {
	path := writeTempWorkflow(t, `---
repo:
  clone_url: git@example.com:owner/repo.git
tracker:
  kind: gitea
---
Prompt body
`)
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load without server block: %v", err)
	}
	if got := wf.Config.Server.Port; got != 4000 {
		t.Fatalf("server.port default = %d, want 4000", got)
	}
}

func TestLoadRejectsInvalidServerPort(t *testing.T) {
	for _, port := range []int{-2, 0, 65536} {
		path := writeTempWorkflow(t, `---
server:
  port: `+fmt.Sprint(port)+`
repo:
  clone_url: git@example.com:owner/repo.git
tracker:
  kind: gitea
---
Prompt body
`)
		_, err := Load(path)
		if err == nil {
			t.Fatalf("Load(server.port=%d): expected error, got nil", port)
		}
		if !strings.Contains(err.Error(), "server.port") {
			t.Fatalf("Load(server.port=%d) error = %q, want server.port", port, err)
		}
	}
}

func TestLoadParsesSpecPollingAndLegacyTrackerPollInterval(t *testing.T) {
	t.Run("spec polling interval wins over default", func(t *testing.T) {
		path := writeTempWorkflow(t, `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
polling:
  interval_ms: 12345
tracker:
  kind: gitea
---
Prompt body
`)
		wf, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := wf.Config.Polling.IntervalMs; got != 12345 {
			t.Fatalf("polling.interval_ms = %d, want 12345", got)
		}
		if got := wf.Config.Tracker.PollIntervalMs; got != 12345 {
			t.Fatalf("legacy tracker poll interval mirror = %d, want 12345", got)
		}
	})

	t.Run("legacy tracker poll interval still migrates with warning", func(t *testing.T) {
		path := writeTempWorkflow(t, `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
tracker:
  kind: gitea
  poll_interval_ms: 45678
---
Prompt body
`)
		var logs bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&logs)
		t.Cleanup(func() { log.SetOutput(previous) })

		wf, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := wf.Config.Polling.IntervalMs; got != 45678 {
			t.Fatalf("polling.interval_ms migrated from tracker.poll_interval_ms = %d, want 45678", got)
		}
		if got := wf.Config.Tracker.PollIntervalMs; got != 45678 {
			t.Fatalf("tracker.poll_interval_ms compatibility mirror = %d, want 45678", got)
		}
		if !strings.Contains(logs.String(), "tracker.poll_interval_ms is deprecated; use polling.interval_ms") {
			t.Fatalf("legacy tracker.poll_interval_ms did not emit deprecation warning; logs: %s", logs.String())
		}
	})

	t.Run("both spellings prefer spec key and warn", func(t *testing.T) {
		path := writeTempWorkflow(t, `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
tracker:
  kind: gitea
  poll_interval_ms: 45678
polling:
  interval_ms: 12345
---
Prompt body
`)
		var logs bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&logs)
		t.Cleanup(func() { log.SetOutput(previous) })

		wf, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := wf.Config.Polling.IntervalMs; got != 12345 {
			t.Fatalf("polling.interval_ms = %d, want spec value 12345", got)
		}
		if got := wf.Config.Tracker.PollIntervalMs; got != 12345 {
			t.Fatalf("tracker.poll_interval_ms mirror = %d, want spec value 12345", got)
		}
		if !strings.Contains(logs.String(), "tracker.poll_interval_ms is deprecated and ignored because polling.interval_ms is set") {
			t.Fatalf("conflicting legacy tracker.poll_interval_ms did not emit warning; logs: %s", logs.String())
		}
	})

	t.Run("defaults keep spec and legacy mirrors synchronized", func(t *testing.T) {
		path := writeTempWorkflow(t, `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
tracker:
  kind: gitea
---
Prompt body
`)
		wf, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := wf.Config.Polling.IntervalMs; got != 30000 {
			t.Fatalf("polling.interval_ms default = %d, want 30000", got)
		}
		if got := wf.Config.Tracker.PollIntervalMs; got != wf.Config.Polling.IntervalMs {
			t.Fatalf("tracker.poll_interval_ms default mirror = %d, want %d", got, wf.Config.Polling.IntervalMs)
		}
	})

	t.Run("non-positive spec interval falls back to schema default", func(t *testing.T) {
		path := writeTempWorkflow(t, `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
polling:
  interval_ms: 0
tracker:
  kind: gitea
---
Prompt body
`)
		wf, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := wf.Config.Polling.IntervalMs; got != 30000 {
			t.Fatalf("polling.interval_ms with explicit 0 = %d, want default 30000", got)
		}
		if got := wf.Config.Tracker.PollIntervalMs; got != wf.Config.Polling.IntervalMs {
			t.Fatalf("tracker.poll_interval_ms compatibility mirror = %d, want %d", got, wf.Config.Polling.IntervalMs)
		}
	})
}

func TestLoadNormalizesSpecWorkspaceRootRelativeToWorkflowDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
workspace:
  root: .aiops-workspaces
tracker:
  kind: gitea
---
Prompt body
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(dir, ".aiops-workspaces")
	if wf.Config.Workspace.Root != want {
		t.Fatalf("workspace.root = %q, want %q", wf.Config.Workspace.Root, want)
	}
	if !strings.Contains(logs.String(), "workflow: relative workspace.root .aiops-workspaces resolved relative to workflow file") {
		t.Fatalf("relative workspace.root normalization was not logged; logs: %s", logs.String())
	}
}

func TestLoadKeepsAbsoluteWorkspaceRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspaces")
	path := writeTempWorkflow(t, `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
workspace:
  root: `+root+`
tracker:
  kind: gitea
---
Prompt body
`)
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if wf.Config.Workspace.Root != root {
		t.Fatalf("workspace.root = %q, want absolute root %q", wf.Config.Workspace.Root, root)
	}
}

func TestExpandConfigLeavesRelativeWorkspaceRootWithoutWorkflowPath(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Workspace.Root = "relative-workspaces"
	if err := expandConfig(&cfg); err != nil {
		t.Fatalf("expandConfig: %v", err)
	}
	if cfg.Workspace.Root != "relative-workspaces" {
		t.Fatalf("workspace.root = %q, want unchanged relative root without workflow path", cfg.Workspace.Root)
	}
}

func TestLoadAllowsUnknownTopLevelKeys(t *testing.T) {
	path := writeTempWorkflow(t, `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
future_extension:
  enabled: true
tracker:
  kind: gitea
---
Prompt body
`)
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	if _, err := Load(path); err != nil {
		t.Fatalf("Load rejected unknown top-level key: %v", err)
	}
	if !strings.Contains(logs.String(), "workflow: unknown top-level key future_extension ignored") {
		t.Fatalf("unknown top-level key was not logged; logs: %s", logs.String())
	}
}

func TestKnownTopLevelWorkflowKeysMatchConfigYAMLTags(t *testing.T) {
	cfgType := reflect.TypeOf(Config{})
	tags := make(map[string]string, cfgType.NumField())
	for i := 0; i < cfgType.NumField(); i++ {
		field := cfgType.Field(i)
		yamlTag := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if yamlTag == "" || yamlTag == "-" {
			continue
		}
		tags[yamlTag] = field.Name
		if _, ok := knownTopLevelWorkflowKeys[yamlTag]; !ok {
			t.Fatalf("knownTopLevelWorkflowKeys missing Config yaml tag %q from field %s", yamlTag, field.Name)
		}
	}
	for key := range knownTopLevelWorkflowKeys {
		if _, ok := tags[key]; !ok {
			t.Fatalf("knownTopLevelWorkflowKeys contains %q without a matching Config yaml tag", key)
		}
	}
}

func TestLoad_PRDraftFromFrontMatter(t *testing.T) {
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
pr:
  draft: true
tracker:
  kind: gitea
---
prompt body
`
	dir := t.TempDir()
	p := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	wf, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !wf.Config.PR.Draft {
		t.Fatalf("expected PR.Draft=true, got false")
	}
}

// TestDefaultConfig_PRDraftDefaultsFalse pins the contract that the
// built-in default for `PR.Draft` is false. Prior to PR #42, the worker
// did not forward draft to Gitea at all, so workflows that omit
// `pr.draft` got ready-for-review PRs. Keeping the default at false
// preserves that behavior; profiles like `company-cautious-WORKFLOW.md`
// must opt in explicitly with `pr.draft: true`.

func TestLoadParsesAgentMaxConcurrentAgentsByStateWithNormalizedKeys(t *testing.T) {
	path := writeTempWorkflow(t, `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
agent:
  default: mock
  max_concurrent_agents: 4
  max_concurrent_agents_by_state:
    In Progress: 2
    rework: 1
tracker:
  kind: gitea
---
Prompt body
`)

	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := wf.Config.Agent.MaxConcurrentAgentsByState
	if got["in_progress"] != 2 {
		t.Fatalf("normalized In Progress cap = %d, want 2 (map=%v)", got["in_progress"], got)
	}
	if got["rework"] != 1 {
		t.Fatalf("rework cap = %d, want 1 (map=%v)", got["rework"], got)
	}
	if _, ok := got["In Progress"]; ok {
		t.Fatalf("map kept unnormalized state key: %v", got)
	}
}

func TestLoadRejectsDuplicateNormalizedAgentMaxConcurrentAgentsByState(t *testing.T) {
	path := writeTempWorkflow(t, `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
agent:
  default: mock
  max_concurrent_agents_by_state:
    In Progress: 2
    in_progress: 5
tracker:
  kind: gitea
---
Prompt body
`)

	_, err := Load(path)
	if err == nil {
		t.Fatalf("Load succeeded with duplicate normalized state caps")
	}
	if !strings.Contains(err.Error(), "duplicates") || !strings.Contains(err.Error(), "in_progress") {
		t.Fatalf("Load error = %v, want duplicate normalized key guidance", err)
	}
}

func TestLoadRejectsInvalidAgentMaxConcurrentAgentsByState(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "empty state key",
			body: `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
agent:
  default: mock
  max_concurrent_agents_by_state:
    "": 1
tracker:
  kind: gitea
---
Prompt body
`,
			want: "empty state key",
		},
		{
			name: "non-positive limit",
			body: `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
agent:
  default: mock
  max_concurrent_agents_by_state:
    rework: 0
tracker:
  kind: gitea
---
Prompt body
`,
			want: "must be positive",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeTempWorkflow(t, tt.body))
			if err == nil {
				t.Fatalf("Load succeeded, want validation error containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDefaultConfig_PRDraftDefaultsFalse(t *testing.T) {
	if got := DefaultConfig().PR.Draft; got != false {
		t.Fatalf("DefaultConfig().PR.Draft: got %v want false (would regress non-draft default)", got)
	}
}

func TestLoadParsesServiceLinearRoutes(t *testing.T) {
	t.Setenv("TEST_SERVICE_REPO_URL", "git@example.com:acme/api.git")
	body := `---
repo:
  owner: fallback
  name: fallback
  clone_url: git@example.com:fallback/fallback.git
services:
  - name: api
    repo:
      owner: acme
      name: api
      clone_url: $TEST_SERVICE_REPO_URL
    tracker:
      project_slug: api-platform
      team_key: ENG
      labels:
        - backend
      custom_fields:
        Runtime: go
tracker:
  kind: gitea
---
prompt body
`
	wf, err := Load(writeTempWorkflow(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(wf.Config.Services); got != 1 {
		t.Fatalf("services = %d, want 1", got)
	}
	service := wf.Config.Services[0]
	if service.Name != "api" {
		t.Fatalf("service name = %q, want api", service.Name)
	}
	if service.Repo.CloneURL != "git@example.com:acme/api.git" {
		t.Fatalf("service repo clone URL = %q", service.Repo.CloneURL)
	}
	if service.Repo.DefaultBranch != "main" {
		t.Fatalf("service repo default branch = %q, want main", service.Repo.DefaultBranch)
	}
	if service.Tracker.ProjectSlug != "api-platform" || service.Tracker.TeamKey != "ENG" {
		t.Fatalf("service tracker route = %+v, want project api-platform team ENG", service.Tracker)
	}
	if !reflect.DeepEqual(service.Tracker.Labels, []string{"backend"}) {
		t.Fatalf("service tracker labels = %#v, want backend", service.Tracker.Labels)
	}
	if !reflect.DeepEqual(service.Tracker.CustomFields, map[string]string{"Runtime": "go"}) {
		t.Fatalf("service tracker custom fields = %#v, want Runtime=go", service.Tracker.CustomFields)
	}
}

func TestLoadAllowsLinearServiceOnlyWorkflowWithoutFallbackRepo(t *testing.T) {
	body := `---
tracker:
  kind: linear
services:
  - name: api
    repo:
      owner: acme
      name: api
      clone_url: git@example.com:acme/api.git
    tracker:
      project_slug: api-platform
---
prompt body
`

	wf, err := Load(writeTempWorkflow(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if wf.Config.Repo.CloneURL != "" {
		t.Fatalf("fallback repo clone URL = %q, want empty service-only fallback", wf.Config.Repo.CloneURL)
	}
}

func TestLoadAllowsLinearServiceOnlyWorkflowWithTopLevelProjectSlug(t *testing.T) {
	body := `---
tracker:
  kind: linear
  project_slug: platform
services:
  - name: api
    repo:
      owner: acme
      name: api
      clone_url: git@example.com:acme/api.git
    tracker:
      team_key: ENG
---
prompt body
`

	wf, err := Load(writeTempWorkflow(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if wf.Config.Tracker.ProjectSlug != "platform" {
		t.Fatalf("top-level project slug = %q, want platform", wf.Config.Tracker.ProjectSlug)
	}
}

func TestLoadRejectsLinearServiceOnlyWorkflowWithoutAnyProjectSlug(t *testing.T) {
	body := `---
tracker:
  kind: linear
services:
  - name: api
    repo:
      owner: acme
      name: api
      clone_url: git@example.com:acme/api.git
    tracker:
      team_key: ENG
---
prompt body
`

	_, err := Load(writeTempWorkflow(t, body))
	if err == nil {
		t.Fatal("Load returned nil error, want project slug requirement for service-only Linear workflow")
	}
	if !strings.Contains(err.Error(), "tracker.project_slug") && !strings.Contains(err.Error(), "services[0].tracker.project_slug") {
		t.Fatalf("Load error = %q, want project slug guidance", err)
	}
}

func TestLoadRejectsLinearFallbackRepoWorkflowWithServiceMissingProjectSlug(t *testing.T) {
	body := `---
tracker:
  kind: linear
repo:
  owner: acme
  name: fallback
  clone_url: git@example.com:acme/fallback.git
services:
  - name: api
    repo:
      owner: acme
      name: api
      clone_url: git@example.com:acme/api.git
    tracker:
      labels: [backend]
---
prompt body
`

	_, err := Load(writeTempWorkflow(t, body))
	if err == nil {
		t.Fatal("Load returned nil error, want project slug requirement for routed Linear polling")
	}
	if !strings.Contains(err.Error(), "tracker.project_slug") && !strings.Contains(err.Error(), "services[0].tracker.project_slug") {
		t.Fatalf("Load error = %q, want project slug guidance", err)
	}
}

func TestLoadRejectsLinearServiceWithoutExplicitRoute(t *testing.T) {
	body := `---
tracker:
  kind: linear
  project_slug: platform
services:
  - name: api
    repo:
      owner: acme
      name: api
      clone_url: git@example.com:acme/api.git
---
prompt body
`

	_, err := Load(writeTempWorkflow(t, body))
	if err == nil {
		t.Fatal("Load returned nil error, want explicit Linear route predicate requirement")
	}
	if !strings.Contains(err.Error(), "services[0].tracker") || !strings.Contains(err.Error(), "route predicate") {
		t.Fatalf("Load error = %q, want explicit route predicate guidance", err)
	}
}

// TestLoadRejectsLinearServiceCustomFields pins the #326 guard: Linear's
// GraphQL schema exposes no Issue custom-field data, so a service route
// carrying custom_fields can never match a polled issue and must be
// rejected at load time rather than silently dropping work. This branch is
// otherwise uncovered, so it characterizes validateConfig before the #410
// split moves it into a per-section validator.
func TestLoadRejectsLinearServiceCustomFields(t *testing.T) {
	body := `---
tracker:
  kind: linear
  project_slug: platform
services:
  - name: api
    repo:
      owner: acme
      name: api
      clone_url: git@example.com:acme/api.git
    tracker:
      project_slug: api-platform
      custom_fields:
        Runtime: go
---
prompt body
`

	_, err := Load(writeTempWorkflow(t, body))
	if err == nil {
		t.Fatal("Load returned nil error, want Linear custom_fields rejection (#326)")
	}
	if !strings.Contains(err.Error(), "services[0].tracker.custom_fields") || !strings.Contains(err.Error(), "#326") {
		t.Fatalf("Load error = %q, want services[0].tracker.custom_fields rejection citing #326", err)
	}
}

func TestLoadRejectsGiteaServiceOnlyWorkflowWithoutFallbackRepo(t *testing.T) {
	body := `---
tracker:
  kind: gitea
services:
  - name: api
    repo:
      owner: acme
      name: api
      clone_url: git@example.com:acme/api.git
    tracker:
      project_slug: api-platform
---
prompt body
`

	_, err := Load(writeTempWorkflow(t, body))
	if err == nil {
		t.Fatal("Load returned nil error, want missing fallback repo clone_url error for non-Linear services")
	}
	if !strings.Contains(err.Error(), "repo.clone_url") || !strings.Contains(err.Error(), "linear") {
		t.Fatalf("Load error = %q, want repo.clone_url and linear guidance", err)
	}
}

func TestLoadRejectsServiceRouteMissingRepoCloneURL(t *testing.T) {
	body := `---
repo:
  owner: fallback
  name: fallback
  clone_url: git@example.com:fallback/fallback.git
services:
  - name: api
    repo:
      owner: acme
      name: api
    tracker:
      project_slug: api-platform
tracker:
  kind: gitea
---
prompt body
`

	_, err := Load(writeTempWorkflow(t, body))
	if err == nil {
		t.Fatal("Load returned nil error, want missing service repo clone_url error")
	}
	if !strings.Contains(err.Error(), "services[0].repo.clone_url") {
		t.Fatalf("Load error = %q, want services[0].repo.clone_url", err)
	}
}

func TestLoadRejectsEmptyServiceName(t *testing.T) {
	body := `---
repo:
  owner: fallback
  name: fallback
  clone_url: git@example.com:fallback/fallback.git
services:
  - repo:
      owner: acme
      name: api
      clone_url: git@example.com:acme/api.git
    tracker:
      project_slug: api-platform
tracker:
  kind: gitea
---
prompt body
`

	_, err := Load(writeTempWorkflow(t, body))
	if err == nil {
		t.Fatal("Load returned nil error, want empty service name error")
	}
	if !strings.Contains(err.Error(), "services[0].name is required") {
		t.Fatalf("Load error = %q, want services[0].name guidance", err)
	}
}

func TestLoadRejectsDuplicateServiceNames(t *testing.T) {
	body := `---
repo:
  owner: fallback
  name: fallback
  clone_url: git@example.com:fallback/fallback.git
services:
  - name: api
    repo:
      owner: acme
      name: api
      clone_url: git@example.com:acme/api.git
    tracker:
      project_slug: api-platform
  - name: API
    repo:
      owner: acme
      name: api-v2
      clone_url: git@example.com:acme/api-v2.git
    tracker:
      project_slug: api-v2
tracker:
  kind: gitea
---
prompt body
`

	_, err := Load(writeTempWorkflow(t, body))
	if err == nil {
		t.Fatal("Load returned nil error, want duplicate service name error")
	}
	if !strings.Contains(err.Error(), `services[1].name "API" duplicates services[0].name`) {
		t.Fatalf("Load error = %q, want duplicate service name guidance", err)
	}
}

func TestLoad_PRDraftDefaultsAndExplicitFalse(t *testing.T) {
	cases := map[string]struct {
		front string
		want  bool
	}{
		"explicit false": {
			front: `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
pr:
  draft: false
tracker:
  kind: gitea
---
body
`,
			want: false,
		},
		"unset stays at default": {
			front: `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
tracker:
  kind: gitea
---
body
`,
			// DefaultConfig() sets PR.Draft=false, so workflows that omit
			// `pr.draft` keep the historical ready-for-review behavior.
			want: DefaultConfig().PR.Draft,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "WORKFLOW.md")
			if err := os.WriteFile(p, []byte(tc.front), 0o644); err != nil {
				t.Fatalf("write workflow: %v", err)
			}
			wf, err := Load(p)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if wf.Config.PR.Draft != tc.want {
				t.Fatalf("PR.Draft: got %v want %v", wf.Config.PR.Draft, tc.want)
			}
		})
	}
}

// TestDefaultConfigAgentTimeout pins the schema-level defaults the
// platform contract advertises: a 30-minute per-task timeout and
// SPEC-aligned unbounded retry budgets (the §15.5 harness-hardening
// caps are opt-in). The exponential-backoff ceiling is 5 minutes per
// SPEC §6.4.
func TestDefaultConfigAgentTimeout(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Agent.Timeout != 30*time.Minute {
		t.Fatalf("default Agent.Timeout: got %v want 30m", cfg.Agent.Timeout)
	}
	if got := cfg.Agent.MaxTimeoutRetriesValue(); got != UnboundedRetryBudget {
		t.Fatalf("default Agent.MaxTimeoutRetriesValue: got %d want UnboundedRetryBudget (%d)", got, UnboundedRetryBudget)
	}
	if got := cfg.Agent.MaxRetryAttemptsValue(); got != UnboundedRetryBudget {
		t.Fatalf("default Agent.MaxRetryAttemptsValue: got %d want UnboundedRetryBudget (%d)", got, UnboundedRetryBudget)
	}
	if cfg.Agent.MaxRetryBackoffMs != 300000 {
		t.Fatalf("default Agent.MaxRetryBackoffMs: got %d want 300000", cfg.Agent.MaxRetryBackoffMs)
	}
}

// TestMaxRetryAttemptsValue_OptInCapHonored verifies that an explicit
// positive opt-in survives MaxRetryAttemptsValue() exactly and that
// explicit zero is honored as "no retries" (the deliberate single-shot
// mode) rather than coerced back to the unbounded default. This is the
// harness-hardening path documented under SPEC §15.5.
func TestMaxRetryAttemptsValue_OptInCapHonored(t *testing.T) {
	cases := []struct {
		name string
		set  int
		want int
	}{
		{"explicit zero disables retries", 0, 0},
		{"explicit one matches historical default", 1, 1},
		{"explicit three caps at three", 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			val := tc.set
			cfg := AgentConfig{MaxRetryAttempts: &val}
			if got := cfg.MaxRetryAttemptsValue(); got != tc.want {
				t.Fatalf("MaxRetryAttemptsValue(%d) = %d, want %d", tc.set, got, tc.want)
			}
		})
	}
}

// TestMaxTimeoutRetriesValue_OptInCapHonored mirrors
// TestMaxRetryAttemptsValue_OptInCapHonored for runner-timeout retries.
func TestMaxTimeoutRetriesValue_OptInCapHonored(t *testing.T) {
	cases := []struct {
		name string
		set  int
		want int
	}{
		{"explicit zero disables re-queue", 0, 0},
		{"explicit one matches historical default", 1, 1},
		{"explicit five caps at five", 5, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			val := tc.set
			cfg := AgentConfig{MaxTimeoutRetries: &val}
			if got := cfg.MaxTimeoutRetriesValue(); got != tc.want {
				t.Fatalf("MaxTimeoutRetriesValue(%d) = %d, want %d", tc.set, got, tc.want)
			}
		})
	}
}

func TestLoadParsesAgentMaxRetryBackoffMs(t *testing.T) {
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
agent:
  max_retry_backoff_ms: 45000
tracker:
  kind: gitea
---
prompt body
`
	wf, err := Load(writeTempWorkflow(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if wf.Config.Agent.MaxRetryBackoffMs != 45000 {
		t.Fatalf("Agent.MaxRetryBackoffMs = %d, want 45000", wf.Config.Agent.MaxRetryBackoffMs)
	}
}

func TestLoadRejectsNonPositiveAgentMaxRetryBackoffMs(t *testing.T) {
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
agent:
  max_retry_backoff_ms: 0
tracker:
  kind: gitea
---
prompt body
`
	_, err := Load(writeTempWorkflow(t, body))
	if err == nil {
		t.Fatal("Load succeeded with agent.max_retry_backoff_ms=0, want validation error")
	}
	if !strings.Contains(err.Error(), "agent.max_retry_backoff_ms must be positive") {
		t.Fatalf("Load error = %v, want agent.max_retry_backoff_ms positivity guidance", err)
	}
}

func TestLoadRejectsNonPositiveAgentMaxTurns(t *testing.T) {
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
agent:
  max_turns: 0
tracker:
  kind: gitea
---
prompt body
`
	_, err := Load(writeTempWorkflow(t, body))
	if err == nil {
		t.Fatal("Load succeeded with agent.max_turns=0, want validation error")
	}
	if !strings.Contains(err.Error(), "agent.max_turns must be positive") {
		t.Fatalf("Load error = %v, want agent.max_turns guidance", err)
	}
}

func TestLoadParsesAgentMaxRetryAttempts(t *testing.T) {
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
agent:
  max_retry_attempts: 0
tracker:
  kind: gitea
---
prompt body
`
	wf, err := Load(writeTempWorkflow(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := wf.Config.Agent.MaxRetryAttemptsValue(); got != 0 {
		t.Fatalf("Agent.MaxRetryAttemptsValue = %d, want explicit zero", got)
	}
}

func TestLoadRejectsNegativeAgentMaxRetryAttempts(t *testing.T) {
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
agent:
  max_retry_attempts: -1
tracker:
  kind: gitea
---
prompt body
`
	_, err := Load(writeTempWorkflow(t, body))
	if err == nil {
		t.Fatal("Load succeeded with agent.max_retry_attempts=-1, want validation error")
	}
	if !strings.Contains(err.Error(), "agent.max_retry_attempts must be non-negative") {
		t.Fatalf("Load error = %v, want agent.max_retry_attempts guidance", err)
	}
}

func TestLoadParsesTopLevelWorkspaceHooks(t *testing.T) {
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
hooks:
  after_create:
    commands:
      - printf after_create
  before_run:
    commands:
      - printf before_run
  after_run:
    commands:
      - printf after_run
  before_remove:
    commands:
      - printf before_remove
  timeout_ms: 1234
tracker:
  kind: gitea
---
prompt body
`
	wf, err := Load(writeTempWorkflow(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	hooks := wf.Config.Hooks
	if !reflect.DeepEqual(hooks.AfterCreate.Commands, []string{"printf after_create"}) {
		t.Fatalf("Hooks.AfterCreate.Commands = %#v", hooks.AfterCreate.Commands)
	}
	if !reflect.DeepEqual(hooks.BeforeRun.Commands, []string{"printf before_run"}) {
		t.Fatalf("Hooks.BeforeRun.Commands = %#v", hooks.BeforeRun.Commands)
	}
	if !reflect.DeepEqual(hooks.AfterRun.Commands, []string{"printf after_run"}) {
		t.Fatalf("Hooks.AfterRun.Commands = %#v", hooks.AfterRun.Commands)
	}
	if !reflect.DeepEqual(hooks.BeforeRemove.Commands, []string{"printf before_remove"}) {
		t.Fatalf("Hooks.BeforeRemove.Commands = %#v", hooks.BeforeRemove.Commands)
	}
	if hooks.TimeoutMs != 1234 {
		t.Fatalf("Hooks.TimeoutMs = %d, want 1234", hooks.TimeoutMs)
	}
}

func TestLoadParsesSpecWorkspaceHookScriptStrings(t *testing.T) {
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
hooks:
  after_create: |
    printf after_create
  before_run: printf before_run
  after_run: |
    printf after_run
  before_remove: printf before_remove
tracker:
  kind: gitea
---
prompt body
`
	wf, err := Load(writeTempWorkflow(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	hooks := wf.Config.Hooks
	if !reflect.DeepEqual(hooks.AfterCreate.Commands, []string{"printf after_create\n"}) {
		t.Fatalf("Hooks.AfterCreate.Commands = %#v", hooks.AfterCreate.Commands)
	}
	if !reflect.DeepEqual(hooks.BeforeRun.Commands, []string{"printf before_run"}) {
		t.Fatalf("Hooks.BeforeRun.Commands = %#v", hooks.BeforeRun.Commands)
	}
	if !reflect.DeepEqual(hooks.AfterRun.Commands, []string{"printf after_run\n"}) {
		t.Fatalf("Hooks.AfterRun.Commands = %#v", hooks.AfterRun.Commands)
	}
	if !reflect.DeepEqual(hooks.BeforeRemove.Commands, []string{"printf before_remove"}) {
		t.Fatalf("Hooks.BeforeRemove.Commands = %#v", hooks.BeforeRemove.Commands)
	}
}

func TestWorkspaceHooksMergesTopLevelAndLegacyPerHook(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Hooks = WorkspaceHooks{
		AfterRun: WorkspaceHook{Commands: []string{"printf top-after-run"}},
	}
	cfg.Workspace.Hooks = WorkspaceHooks{
		AfterCreate:  WorkspaceHook{Commands: []string{"printf legacy-after-create"}},
		BeforeRemove: WorkspaceHook{Commands: []string{"printf legacy-before-remove"}},
	}

	hooks := cfg.WorkspaceHooks()
	if !reflect.DeepEqual(hooks.AfterCreate.Commands, []string{"printf legacy-after-create"}) {
		t.Fatalf("AfterCreate.Commands = %#v", hooks.AfterCreate.Commands)
	}
	if !reflect.DeepEqual(hooks.AfterRun.Commands, []string{"printf top-after-run"}) {
		t.Fatalf("AfterRun.Commands = %#v", hooks.AfterRun.Commands)
	}
	if !reflect.DeepEqual(hooks.BeforeRemove.Commands, []string{"printf legacy-before-remove"}) {
		t.Fatalf("BeforeRemove.Commands = %#v", hooks.BeforeRemove.Commands)
	}
}

func TestWorkspaceHooksHonorsLegacyTimeoutOverride(t *testing.T) {
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
workspace:
  hooks:
    timeout_ms: 4321
tracker:
  kind: gitea
---
prompt body
`
	wf, err := Load(writeTempWorkflow(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := wf.Config.WorkspaceHooks().TimeoutMs; got != 4321 {
		t.Fatalf("WorkspaceHooks().TimeoutMs = %d, want legacy timeout override 4321", got)
	}
}

func TestWorkspaceHooksPrefersExplicitTopLevelTimeout(t *testing.T) {
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
hooks:
  timeout_ms: 1234
workspace:
  hooks:
    timeout_ms: 4321
tracker:
  kind: gitea
---
prompt body
`
	wf, err := Load(writeTempWorkflow(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := wf.Config.WorkspaceHooks().TimeoutMs; got != 1234 {
		t.Fatalf("WorkspaceHooks().TimeoutMs = %d, want explicit top-level timeout 1234", got)
	}
}

func TestWorkspaceHooksPreservesExplicitEmptyTopLevelOverride(t *testing.T) {
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
hooks:
  before_run: []
workspace:
  hooks:
    before_run:
      - printf legacy-before-run
tracker:
  kind: gitea
---
prompt body
`
	wf, err := Load(writeTempWorkflow(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := wf.Config.WorkspaceHooks().BeforeRun.Commands; len(got) != 0 {
		t.Fatalf("WorkspaceHooks().BeforeRun.Commands = %#v, want explicit empty top-level hook to suppress legacy hook", got)
	}
}

func TestWorkspaceHooksHonorsLegacyEnvPassthrough(t *testing.T) {
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
workspace:
  hooks:
    env_passthrough:
      - LEGACY_VAR
tracker:
  kind: gitea
---
prompt body
`
	wf, err := Load(writeTempWorkflow(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := wf.Config.WorkspaceHooks().EnvPassthrough
	if !reflect.DeepEqual(got, []string{"LEGACY_VAR"}) {
		t.Fatalf("WorkspaceHooks().EnvPassthrough = %#v, want legacy env_passthrough to surface", got)
	}
}

func TestWorkspaceHooksPrefersExplicitTopLevelEnvPassthrough(t *testing.T) {
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
hooks:
  env_passthrough:
    - TOP_VAR
workspace:
  hooks:
    env_passthrough:
      - LEGACY_VAR
tracker:
  kind: gitea
---
prompt body
`
	wf, err := Load(writeTempWorkflow(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := wf.Config.WorkspaceHooks().EnvPassthrough
	if !reflect.DeepEqual(got, []string{"TOP_VAR"}) {
		t.Fatalf("WorkspaceHooks().EnvPassthrough = %#v, want explicit top-level to win over legacy", got)
	}
}

func TestWorkspaceHooksPreservesExplicitEmptyTopLevelEnvPassthrough(t *testing.T) {
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
hooks:
  env_passthrough: []
workspace:
  hooks:
    env_passthrough:
      - LEGACY_VAR
tracker:
  kind: gitea
---
prompt body
`
	wf, err := Load(writeTempWorkflow(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got := wf.Config.WorkspaceHooks().EnvPassthrough; len(got) != 0 {
		t.Fatalf("WorkspaceHooks().EnvPassthrough = %#v, want explicit empty top-level to suppress legacy passthrough", got)
	}
}

func TestDefaultConfigWorkspaceHooksTimeout(t *testing.T) {
	if got, want := DefaultConfig().Hooks.TimeoutMs, 60000; got != want {
		t.Fatalf("DefaultConfig().Hooks.TimeoutMs = %d, want SPEC default %d", got, want)
	}
}

func TestLoadRejectsNegativeWorkspaceHooksTimeout(t *testing.T) {
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
hooks:
  timeout_ms: -1
tracker:
  kind: gitea
---
prompt body
`
	_, err := Load(writeTempWorkflow(t, body))
	if err == nil {
		t.Fatal("Load succeeded, want negative hooks.timeout_ms validation error")
	}
	if !strings.Contains(err.Error(), "hooks.timeout_ms") {
		t.Fatalf("Load error = %v, want hooks.timeout_ms", err)
	}
}

func TestDefaultConfigSandboxDisabled(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Sandbox.Enabled {
		t.Fatal("sandbox hardening must be disabled by default for backward compatibility")
	}
	if cfg.Sandbox.Backend != "none" {
		t.Fatalf("default Sandbox.Backend: got %q want none", cfg.Sandbox.Backend)
	}
}

func TestLoadSandboxEnforcementConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: n
  clone_url: git@example.com:o/n.git
sandbox:
  enabled: true
  backend: firejail
  network: allowlist
  network_allowlist_cidrs:
    - 203.0.113.10/32
  network_interface: aiops0
  env_allowlist:
    - PATH
    - HOME
  credential_files:
    - ~/.config/aiops/token
tracker:
  kind: gitea
---
hello
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !wf.Config.Sandbox.Enabled {
		t.Fatal("Sandbox.Enabled = false, want true")
	}
	if wf.Config.Sandbox.Backend != "firejail" {
		t.Fatalf("Sandbox.Backend = %q, want firejail", wf.Config.Sandbox.Backend)
	}
	if wf.Config.Sandbox.NetworkMode != "allowlist" {
		t.Fatalf("Sandbox.NetworkMode = %q, want allowlist", wf.Config.Sandbox.NetworkMode)
	}
	if !reflect.DeepEqual(wf.Config.Sandbox.NetworkAllowlistCIDRs, []string{"203.0.113.10/32"}) {
		t.Fatalf("NetworkAllowlistCIDRs = %#v", wf.Config.Sandbox.NetworkAllowlistCIDRs)
	}
	if wf.Config.Sandbox.NetworkInterface != "aiops0" {
		t.Fatalf("NetworkInterface = %q, want aiops0", wf.Config.Sandbox.NetworkInterface)
	}
	if !reflect.DeepEqual(wf.Config.Sandbox.EnvAllowlist, []string{"PATH", "HOME"}) {
		t.Fatalf("EnvAllowlist = %#v", wf.Config.Sandbox.EnvAllowlist)
	}
	wantCredential := filepath.Join(os.Getenv("HOME"), ".config/aiops/token")
	if !reflect.DeepEqual(wf.Config.Sandbox.CredentialFiles, []string{wantCredential}) {
		t.Fatalf("CredentialFiles = %#v, want %#v", wf.Config.Sandbox.CredentialFiles, []string{wantCredential})
	}
}

func TestLoadRejectsSandboxNetworkAllowlistWithoutCIDRs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: n
  clone_url: git@example.com:o/n.git
sandbox:
  enabled: true
  backend: firejail
  network: allowlist
  env_allowlist:
    - PATH
tracker:
  kind: gitea
---
hello
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected allowlist without CIDRs error")
	}
	if !strings.Contains(err.Error(), "sandbox.network=allowlist") || !strings.Contains(err.Error(), "network_allowlist_cidrs") {
		t.Fatalf("Load error = %q, want allowlist CIDR guidance", err)
	}
}

func TestLoadRejectsUnsupportedSandboxNetwork(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: n
  clone_url: git@example.com:o/n.git
sandbox:
  enabled: true
  backend: firejail
  network: open-internet
  env_allowlist:
    - PATH
tracker:
  kind: gitea
---
hello
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected unsupported sandbox network error")
	}
	if !strings.Contains(err.Error(), "sandbox.network") || !strings.Contains(err.Error(), "open-internet") {
		t.Fatalf("Load error = %q, want sandbox.network open-internet", err)
	}
}

func TestLoadRejectsSandboxNetworkAllowlistWithoutFirejail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: n
  clone_url: git@example.com:o/n.git
sandbox:
  enabled: true
  backend: bubblewrap
  network: allowlist
  network_allowlist_cidrs:
    - 203.0.113.10/32
  env_allowlist:
    - PATH
tracker:
  kind: gitea
---
hello
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected allowlist without firejail error")
	}
	if !strings.Contains(err.Error(), "sandbox.network=allowlist") || !strings.Contains(err.Error(), "firejail") {
		t.Fatalf("Load error = %q, want firejail allowlist guidance", err)
	}
}

func TestLoadRejectsSandboxNetworkAllowlistWithoutInterface(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: n
  clone_url: git@example.com:o/n.git
sandbox:
  enabled: true
  backend: firejail
  network: allowlist
  network_allowlist_cidrs:
    - 203.0.113.10/32
  env_allowlist:
    - PATH
tracker:
  kind: gitea
---
hello
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected allowlist without network_interface error")
	}
	if !strings.Contains(err.Error(), "sandbox.network=allowlist") || !strings.Contains(err.Error(), "network_interface") {
		t.Fatalf("Load error = %q, want explicit network_interface guidance", err)
	}
}

func TestLoadRejectsSandboxNetworkAllowlistInvalidCIDR(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: n
  clone_url: git@example.com:o/n.git
sandbox:
  enabled: true
  backend: firejail
  network: allowlist
  network_allowlist_cidrs:
    - "0.0.0.0/0 -j ACCEPT\n-A OUTPUT -j ACCEPT"
  network_interface: aiops0
  env_allowlist:
    - PATH
tracker:
  kind: gitea
---
hello
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected invalid CIDR error")
	}
	if !strings.Contains(err.Error(), "sandbox.network_allowlist_cidrs") || !strings.Contains(err.Error(), "invalid CIDR") {
		t.Fatalf("Load error = %q, want invalid CIDR guidance", err)
	}
}

func TestLoadRejectsSandboxNetworkAllowlistIPv6CIDR(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: n
  clone_url: git@example.com:o/n.git
sandbox:
  enabled: true
  backend: firejail
  network: allowlist
  network_allowlist_cidrs:
    - 2001:db8::/32
  network_interface: aiops0
  env_allowlist:
    - PATH
tracker:
  kind: gitea
---
hello
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected IPv6 CIDR to be rejected at workflow load time")
	}
	if !strings.Contains(err.Error(), "sandbox.network_allowlist_cidrs") || !strings.Contains(err.Error(), "IPv4") || !strings.Contains(err.Error(), "2001:db8::/32") {
		t.Fatalf("Load error = %q, want IPv4-only CIDR guidance", err)
	}
}

func TestLoadRejectsEnabledSandboxWithoutEnvAllowlist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: n
  clone_url: git@example.com:o/n.git
sandbox:
  enabled: true
  backend: bubblewrap
tracker:
  kind: gitea
---
hello
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected enabled sandbox without env_allowlist error")
	}
	if !strings.Contains(err.Error(), "sandbox.env_allowlist") {
		t.Fatalf("Load error = %q, want env_allowlist guidance", err)
	}
}

func TestLoadRejectsUnsupportedSandboxBackend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: n
  clone_url: git@example.com:o/n.git
sandbox:
  enabled: true
  backend: vmagic
tracker:
  kind: gitea
---
hello
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected unsupported sandbox backend error")
	}
	if !strings.Contains(err.Error(), "sandbox.backend") || !strings.Contains(err.Error(), "vmagic") {
		t.Fatalf("Load error = %q, want sandbox.backend vmagic", err)
	}
}

func TestLoadRejectsEnabledSandboxWithoutBackend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: n
  clone_url: git@example.com:o/n.git
sandbox:
  enabled: true
  backend: none
tracker:
  kind: gitea
---
hello
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected enabled sandbox without backend error")
	}
	if !strings.Contains(err.Error(), "sandbox.enabled") || !strings.Contains(err.Error(), "backend") {
		t.Fatalf("Load error = %q, want sandbox.enabled backend guidance", err)
	}
}

// TestLoadOptionalAppliesAgentTimeoutDefaults verifies that a workflow
// missing agent.timeout in its front matter still ends up with the
// schema default after expandConfig runs.
func TestLoadOptionalAppliesAgentTimeoutDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := "---\nrepo:\n  owner: o\n  name: n\n  clone_url: git@example.com:o/n.git\n  default_branch: main\ntracker:\n  kind: gitea\n---\nhello\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	wf, err := LoadOptional(path)
	if err != nil {
		t.Fatalf("LoadOptional: %v", err)
	}
	if wf.Config.Agent.Timeout != 30*time.Minute {
		t.Fatalf("expanded Agent.Timeout: got %v want 30m", wf.Config.Agent.Timeout)
	}
	if got := wf.Config.Agent.MaxTimeoutRetriesValue(); got != UnboundedRetryBudget {
		t.Fatalf("expanded Agent.MaxTimeoutRetriesValue: got %d want UnboundedRetryBudget (%d)", got, UnboundedRetryBudget)
	}
}

// TestLoadOptionalHonorsExplicitAgentTimeout confirms a user-specified
// agent.timeout / max_timeout_retries override the schema defaults.
func TestLoadOptionalHonorsExplicitAgentTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := "---\nrepo:\n  owner: o\n  name: n\n  clone_url: git@example.com:o/n.git\nagent:\n  timeout: 5m\n  max_timeout_retries: 3\ntracker:\n  kind: gitea\n---\nhello\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	wf, err := LoadOptional(path)
	if err != nil {
		t.Fatalf("LoadOptional: %v", err)
	}
	if wf.Config.Agent.Timeout != 5*time.Minute {
		t.Fatalf("explicit Agent.Timeout: got %v want 5m", wf.Config.Agent.Timeout)
	}
	if got := wf.Config.Agent.MaxTimeoutRetriesValue(); got != 3 {
		t.Fatalf("explicit Agent.MaxTimeoutRetriesValue: got %d want 3", got)
	}
}

// TestLoad_RejectsRemovedAgentFallback verifies that workflows still
// carrying the removed `agent.fallback` key fail Load with an error that
// points the operator at the supported alternative (`agent.default`).
//
// `agent.fallback` was historically declared on AgentConfig but never
// read by the worker (issue #40). Silently dropping the key would let
// authors keep believing it controlled retry behavior, so Load must
// surface a clear validation error instead.
func TestLoad_RejectsRemovedAgentFallback(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
agent:
  default: mock
  fallback: claude
tracker:
  kind: gitea
---
prompt body
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatalf("Load: expected error for removed agent.fallback, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"agent.fallback", "agent.default"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("Load error %q: want substring %q", msg, want)
		}
	}
}

// TestLoad_RejectsRemovedVerifyExecutionFields verifies that workflows still
// carrying the worker-execution-only verify keys removed in #557
// (verify.timeout / allow_failure / env_passthrough) or the worker secret-scan
// gate removed in #561 (verify.secret_scan) fail Load with a clear error
// instead of being silently accepted while the worker ignores them —
// verification and pre-push secret scanning are the agent's job now.
// verify.commands stays valid.
func TestLoad_RejectsRemovedVerifyExecutionFields(t *testing.T) {
	const head = "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: gitea\n"
	for _, tc := range []struct{ name, block string }{
		{"timeout", "verify:\n  timeout: 5m\n"},
		{"allow_failure", "verify:\n  allow_failure: true\n"},
		{"env_passthrough", "verify:\n  env_passthrough:\n    - GOMODCACHE\n"},
		{"secret_scan", "verify:\n  secret_scan:\n    enabled: true\n    command:\n      - gitleaks\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "WORKFLOW.md")
			if err := os.WriteFile(p, []byte(head+tc.block+"---\nprompt body\n"), 0o644); err != nil {
				t.Fatalf("write workflow: %v", err)
			}
			_, err := Load(p)
			if err == nil {
				t.Fatalf("Load(verify.%s present) = nil; want rejection error", tc.name)
			}
			if msg := err.Error(); !strings.Contains(msg, "verify."+tc.name) {
				t.Fatalf("Load(verify.%s) error = %q; want substring %q", tc.name, msg, "verify."+tc.name)
			}
		})
	}

	t.Run("commands still accepted", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "WORKFLOW.md")
		body := head + "verify:\n  commands:\n    - go test ./...\n---\nprompt body\n"
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatalf("write workflow: %v", err)
		}
		wf, err := Load(p)
		if err != nil {
			t.Fatalf("Load(verify.commands) = %v; want nil", err)
		}
		if got := wf.Config.Verify.Commands; len(got) != 1 || got[0] != "go test ./..." {
			t.Fatalf("Verify.Commands = %v; want [go test ./...]", got)
		}
	})
}

// TestLoad_RejectsRemovedCodexProfile verifies that workflows still carrying
// the removed `codex.profile` key fail Load with an error pointing at the SPEC
// §10 app-server runner. The one-shot `codex exec` runner the profile
// configured was removed under #541; silently dropping the key would let
// authors keep believing it controlled the runner's sandbox/approval flags.
func TestLoad_RejectsRemovedCodexProfile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
agent:
  default: codex-app-server
codex:
  command: codex app-server
  profile: safe
tracker:
  kind: gitea
---
prompt body
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatalf("Load: expected error for removed codex.profile, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"codex.profile", "codex-app-server"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("Load error %q: want substring %q", msg, want)
		}
	}
}

// TestLoad_RejectsRemovedClaudeProfile verifies that a leftover `claude.profile`
// key still fails Load. The codex-only `profile` field was removed with the
// codex exec runner (#541); before that, validateCodexClaude rejected
// claude.profile explicitly. Silently dropping it would let an operator believe
// a Claude profile is active when none exists.
func TestLoad_RejectsRemovedClaudeProfile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
agent:
  default: claude
claude:
  command: claude
  profile: safe
tracker:
  kind: gitea
---
prompt body
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatalf("Load: expected error for removed claude.profile, got nil")
	}
	if msg := err.Error(); !strings.Contains(msg, "claude.profile") {
		t.Fatalf("Load error %q: want substring %q", msg, "claude.profile")
	}
}

// TestLoad_RejectsRemovedCodexExecCommand verifies that a workflow migrated to
// the app-server runner but still carrying the removed `codex.command: codex
// exec` fails Load with a clear config error. Without this guard the app-server
// launcher falls through to `sh -c "codex exec"` and the runner waits for
// JSON-RPC that never arrives, failing the first real run opaquely (#541).
func TestLoad_RejectsRemovedCodexExecCommand(t *testing.T) {
	for _, command := range []string{"codex exec", "codex exec --sandbox workspace-write"} {
		command := command
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "WORKFLOW.md")
			body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
agent:
  default: codex-app-server
codex:
  command: ` + command + `
tracker:
  kind: gitea
---
prompt body
`
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatalf("write workflow: %v", err)
			}
			_, err := Load(p)
			if err == nil {
				t.Fatalf("Load(codex.command=%q): expected error for removed codex exec runner, got nil", command)
			}
			msg := err.Error()
			for _, want := range []string{"codex.command", "codex app-server"} {
				if !strings.Contains(msg, want) {
					t.Fatalf("Load(codex.command=%q) error %q: want substring %q", command, msg, want)
				}
			}
		})
	}
}

// TestCommandRunsCodexExec pins the conservative load-time guard directly,
// including the shell-quoted spelling that a whitespace-only split would miss
// (#541, Codex PR #546 review). The runner's splitAppServerCommand stays the
// authoritative launch-time parser; this guard only needs to catch anything
// that would route to the removed one-shot runner.
func TestCommandRunsCodexExec(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		command string
		want    bool
	}{
		{"codex exec", true},
		{"codex exec --sandbox workspace-write", true},
		{`"codex" "exec"`, true}, // quoted spelling the runner tokenizes to codex exec
		{`'codex' 'exec' -o x`, true},
		{"codex app-server", false},
		{"codex app-server --config profile=ci", false},
		{"", false},
		{"codex", false},
		{"my-codex exec", false}, // different binary
	} {
		if got := commandRunsCodexExec(tc.command); got != tc.want {
			t.Errorf("commandRunsCodexExec(%q) = %v; want %v", tc.command, got, tc.want)
		}
	}
}

// TestLoad_AcceptsCodexAppServerCommand pins the positive case: the SPEC §10
// `codex app-server` command (and a flagged variant) still loads cleanly, so
// the removed-exec guard does not over-reject the supported runner.
func TestLoad_AcceptsCodexAppServerCommand(t *testing.T) {
	for _, command := range []string{"codex app-server", "codex app-server --config profile=ci"} {
		command := command
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "WORKFLOW.md")
			body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
agent:
  default: codex-app-server
codex:
  command: ` + command + `
tracker:
  kind: gitea
---
prompt body
`
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatalf("write workflow: %v", err)
			}
			if _, err := Load(p); err != nil {
				t.Fatalf("Load(codex.command=%q) = %v; want nil for the SPEC §10 runner", command, err)
			}
		})
	}
}

// TestLoad_RejectsMissingCloneURL verifies that a workflow front matter
// that omits `repo.clone_url` (or sets it to an empty string after env
// expansion) fails Load with an error that names the file path and the
// missing field. Without this check, the worker only discovered the
// missing URL deep inside `git clone`, producing a confusing "repository
// not found" failure (issue #9).
func TestLoad_RejectsMissingCloneURL(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: r
tracker:
  kind: gitea
---
prompt
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatalf("Load: expected error for missing repo.clone_url, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"repo.clone_url", p} {
		if !strings.Contains(msg, want) {
			t.Fatalf("Load error %q: want substring %q", msg, want)
		}
	}
}

// TestLoad_RejectsUnsupportedTrackerKind ensures Load fails fast when
// tracker.kind is set to a value the platform does not implement. The
// legal set is {"gitea", "linear"} — anything else would silently fall
// through to a runtime no-op in the poller. The error must point at the
// field, the file, and the offending value so the operator can fix it.
func TestLoad_RejectsUnsupportedTrackerKind(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
tracker:
  kind: jira
---
prompt
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatalf("Load: expected error for unsupported tracker.kind, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"tracker.kind", "jira", p} {
		if !strings.Contains(msg, want) {
			t.Fatalf("Load error %q: want substring %q", msg, want)
		}
	}
}

func TestLoad_AllowsGitHubTrackerKind(t *testing.T) {
	t.Setenv("AIOPS_TEST_GITHUB_TOKEN", "github-token")
	dir := t.TempDir()
	p := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
  default_branch: main
tracker:
  kind: github
  api_key: $AIOPS_TEST_GITHUB_TOKEN
  endpoint: https://api.github.test
  active_states:
    - priority:p2
  terminal_states:
    - closed
---
prompt
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	wf, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if wf.Config.Tracker.Kind != "github" {
		t.Fatalf("tracker.kind = %q, want github", wf.Config.Tracker.Kind)
	}
	if wf.Config.Tracker.APIKey != "github-token" {
		t.Fatalf("tracker.api_key did not expand from env: %q", wf.Config.Tracker.APIKey)
	}
	if wf.Config.Tracker.Endpoint != "https://api.github.test" {
		t.Fatalf("tracker.endpoint = %q", wf.Config.Tracker.Endpoint)
	}
}

func TestLoad_AllowsTrackerPaginationMaxPages(t *testing.T) {
	path := writeTempWorkflow(t, `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
tracker:
  kind: github
  api_key: github-token
  pagination_max_pages: 42
---
prompt`)
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := wf.Config.Tracker.PaginationMaxPages; got != 42 {
		t.Fatalf("tracker.pagination_max_pages = %d, want 42", got)
	}
}

func TestLoad_RejectsNegativeTrackerPaginationMaxPages(t *testing.T) {
	path := writeTempWorkflow(t, `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
tracker:
  kind: github
  api_key: github-token
  pagination_max_pages: -1
---
prompt`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load: expected error for negative tracker.pagination_max_pages")
	}
	if !strings.Contains(err.Error(), "tracker.pagination_max_pages") || !strings.Contains(err.Error(), "greater than zero") {
		t.Fatalf("Load error = %q, want tracker.pagination_max_pages guidance", err)
	}
}

// TestLoad_RejectsUnsupportedAgentDefault matches the runner registry in
// internal/runner: only mock/codex-app-server/claude are wired up. Catching a
// typo like `agent.default: codexx` at Load time prevents the worker from
// claiming a task and then dying with "unknown runner" partway through. The
// `codex` (one-shot `codex exec`) runner was removed under #541, so it is now
// rejected here too — the SPEC §10 runner is `codex-app-server`.
func TestLoad_RejectsUnsupportedAgentDefault(t *testing.T) {
	for _, agent := range []string{"codexx", "codex"} {
		agent := agent
		t.Run(agent, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "WORKFLOW.md")
			body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
agent:
  default: ` + agent + `
tracker:
  kind: gitea
---
prompt
`
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatalf("write workflow: %v", err)
			}
			_, err := Load(p)
			if err == nil {
				t.Fatalf("Load(agent.default=%q): expected unsupported-runner error, got nil", agent)
			}
			msg := err.Error()
			for _, want := range []string{"agent.default", agent, p} {
				if !strings.Contains(msg, want) {
					t.Fatalf("Load(agent.default=%q) error %q: want substring %q", agent, msg, want)
				}
			}
		})
	}
}

// TestLoad_AcceptsMinimalValidFrontMatter pins the positive case: a
// workflow that supplies the required repo.clone_url with default
// tracker/agent kinds parses cleanly. This guards against the new
// validator over-rejecting legitimate minimal workflows.
func TestLoad_AcceptsMinimalValidFrontMatter(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
tracker:
  kind: gitea
---
prompt
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("Load: unexpected error %v", err)
	}
}

// TestLoad_AcceptsPromptOnlyFile guards backward compatibility for
// WORKFLOW.md files that contain only a prompt template with no `---`
// front matter. These rely on the same built-in defaults that
// LoadOptional supplies when the file is absent, so Load must not
// invoke schema validation against an empty config (issue #9 review
// from chatgpt-codex-connector).
func TestLoad_AcceptsPromptOnlyFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "WORKFLOW.md")
	body := "just a prompt template, no front matter\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	wf, err := Load(p)
	if err != nil {
		t.Fatalf("Load: unexpected error %v", err)
	}
	if wf.PromptTemplate != "just a prompt template, no front matter" {
		t.Fatalf("PromptTemplate: got %q", wf.PromptTemplate)
	}
	if wf.Config.Repo.CloneURL != "" {
		t.Fatalf("Config.Repo.CloneURL: got %q want empty (defaults)", wf.Config.Repo.CloneURL)
	}
}

// TestLoad_CloneURLViaEnvExpansion confirms that a clone_url provided as
// an env var reference (e.g. `$REPO_URL`) is considered set as long as
// the variable resolves to a non-empty value. The validator must run
// after expandConfig so this works.
func TestLoad_CloneURLViaEnvExpansion(t *testing.T) {
	t.Setenv("AIOPS_TEST_REPO_URL", "git@example.com:o/r.git")
	dir := t.TempDir()
	p := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: r
  clone_url: $AIOPS_TEST_REPO_URL
tracker:
  kind: gitea
---
prompt
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	if _, err := Load(p); err != nil {
		t.Fatalf("Load: unexpected error %v", err)
	}
}

// TestLoadOptional_MissingFileSkipsValidation guards the operational
// contract that a repo without a WORKFLOW.md still loads cleanly with
// schema defaults. The validator must only run when an actual file was
// parsed; otherwise the worker would refuse to act on any repo that has
// not yet adopted Symphony.
func TestLoadOptional_MissingFileSkipsValidation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "WORKFLOW.md")
	wf, err := LoadOptional(p)
	if err != nil {
		t.Fatalf("LoadOptional: unexpected error %v", err)
	}
	if wf.Config.Repo.CloneURL != "" {
		t.Fatalf("default Config.Repo.CloneURL: got %q want empty", wf.Config.Repo.CloneURL)
	}
}

// TestLoadOptionalHonorsExplicitZeroMaxTimeoutRetries verifies that an
// operator who deliberately sets max_timeout_retries: 0 in YAML can
// disable the runner-timeout retry budget entirely. Previously the
// loader coerced 0 back to 1, silently undoing this override.
func TestLoadOptionalHonorsExplicitZeroMaxTimeoutRetries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := "---\nrepo:\n  owner: o\n  name: n\n  clone_url: git@example.com:o/n.git\nagent:\n  max_timeout_retries: 0\ntracker:\n  kind: gitea\n---\nhello\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	wf, err := LoadOptional(path)
	if err != nil {
		t.Fatalf("LoadOptional: %v", err)
	}
	if got := wf.Config.Agent.MaxTimeoutRetriesValue(); got != 0 {
		t.Fatalf("explicit zero Agent.MaxTimeoutRetriesValue: got %d want 0", got)
	}
}

// TestDefaultConfig_TrackerStatusesPopulated pins the schema-level
// defaults for tracker.statuses. The names mirror Linear's stock
// workflow ("In Progress", "Human Review", "Rework") so the personal
// profile works without extra YAML; teams that customize their
// workflow override only the names that differ.
func TestDefaultConfig_TrackerStatusesPopulated(t *testing.T) {
	got := DefaultConfig().Tracker.Statuses
	want := TrackerStatusConfig{
		InProgress:  "In Progress",
		HumanReview: "Human Review",
		Rework:      "Rework",
	}
	if got != want {
		t.Fatalf("DefaultConfig().Tracker.Statuses = %#v, want %#v", got, want)
	}
}

// TestLoad_TrackerStatusesPartialOverride verifies an operator can
// override a single status name (here: in_progress) without restating
// the others — expandConfig fills the remaining defaults so the
// minimal-edit ergonomic from the issue's "Status names are
// configurable" criterion is preserved.
func TestLoad_TrackerStatusesPartialOverride(t *testing.T) {
	dir := t.TempDir()
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
tracker:
  kind: linear
  project_slug: platform
  statuses:
    in_progress: "Doing"
---
prompt
`
	p := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	wf, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := wf.Config.Tracker.Statuses
	want := TrackerStatusConfig{
		InProgress:  "Doing",
		HumanReview: "Human Review", // default
		Rework:      "Rework",       // default
	}
	if got != want {
		t.Fatalf("Tracker.Statuses = %#v, want %#v", got, want)
	}
}

// TestLoad_TrackerStatusesAllOverride confirms all three names round-trip
// from YAML, so workflows whose Linear board uses non-default labels
// (e.g. "Coding" / "Review" / "Backlog") work as the issue's acceptance
// criterion 4 requires.
func TestLoad_TrackerStatusesAllOverride(t *testing.T) {
	dir := t.TempDir()
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
tracker:
  kind: linear
  project_slug: platform
  statuses:
    in_progress: "Coding"
    human_review: "Review"
    rework: "Backlog"
---
prompt
`
	p := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	wf, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := wf.Config.Tracker.Statuses
	want := TrackerStatusConfig{
		InProgress:  "Coding",
		HumanReview: "Review",
		Rework:      "Backlog",
	}
	if got != want {
		t.Fatalf("Tracker.Statuses = %#v, want %#v", got, want)
	}
}

func TestLoad_PreservesSafetyPolicyForOperatorInspection(t *testing.T) {
	t.Parallel()
	path := writeTempWorkflow(t, `---
repo:
  clone_url: file:///tmp/repo
tracker:
  kind: linear
  project_slug: platform
safety:
  allowed_networks:
    - git remote for this repository
  allowed_paths:
    - repository workspace for this task
  allowed_commands:
    - go test ./...
  forbidden:
    - reading host files outside the workspace
---
prompt
`)
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := wf.Config.Safety.AllowedNetworks, []string{"git remote for this repository"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Safety.AllowedNetworks = %#v, want %#v", got, want)
	}
	if got, want := wf.Config.Safety.AllowedPaths, []string{"repository workspace for this task"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Safety.AllowedPaths = %#v, want %#v", got, want)
	}
	if got, want := wf.Config.Safety.AllowedCommands, []string{"go test ./..."}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Safety.AllowedCommands = %#v, want %#v", got, want)
	}
	if got, want := wf.Config.Safety.Forbidden, []string{"reading host files outside the workspace"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Safety.Forbidden = %#v, want %#v", got, want)
	}
}

func TestLoad_AcceptsCodexAppServerRunnerAndRuntimeSettings(t *testing.T) {
	t.Parallel()
	path := writeTempWorkflow(t, `---
repo:
  clone_url: file:///tmp/repo
tracker:
  kind: linear
  project_slug: platform
agent:
  default: codex-app-server
codex:
  command: codex app-server
  approval_policy: never
  thread_sandbox: workspace-write
  turn_sandbox_policy:
    type: workspaceWrite
    writableRoots: []
    networkAccess: false
    excludeTmpdirEnvVar: false
    excludeSlashTmp: false
  turn_timeout_ms: 120000
  read_timeout_ms: 250
  stall_timeout_ms: 0
---
prompt
`)
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if wf.Config.Agent.Default != "codex-app-server" {
		t.Fatalf("Agent.Default = %q, want codex-app-server", wf.Config.Agent.Default)
	}
	if wf.Config.Codex.Command != "codex app-server" {
		t.Fatalf("Codex.Command = %q, want codex app-server", wf.Config.Codex.Command)
	}
	if wf.Config.Codex.ApprovalPolicy != "never" {
		t.Fatalf("Codex.ApprovalPolicy = %#v, want never", wf.Config.Codex.ApprovalPolicy)
	}
	if wf.Config.Codex.ThreadSandbox != "workspace-write" {
		t.Fatalf("Codex.ThreadSandbox = %q, want workspace-write", wf.Config.Codex.ThreadSandbox)
	}
	if got := wf.Config.Codex.TurnSandboxPolicy.Type; got != CodexSandboxWorkspaceWrite {
		t.Fatalf("Codex.TurnSandboxPolicy.Type = %#v, want workspaceWrite", got)
	}
	if wf.Config.Codex.TurnTimeoutMs != 120000 || wf.Config.Codex.ReadTimeoutMs != 250 || wf.Config.Codex.StallTimeoutMs != 0 {
		t.Fatalf("Codex timeouts = turn %d read %d stall %d, want 120000/250/0", wf.Config.Codex.TurnTimeoutMs, wf.Config.Codex.ReadTimeoutMs, wf.Config.Codex.StallTimeoutMs)
	}
}

func TestLoadRejectsLegacyCodexTurnSandboxPolicyFields(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "mode",
			body: "mode: workspace-write",
			want: "codex.turn_sandbox_policy.mode",
		},
		{
			name: "read_only_access",
			body: "type: workspaceWrite\n    readOnlyAccess: restricted",
			want: "codex.turn_sandbox_policy.readOnlyAccess",
		},
		{
			name: "access",
			body: "type: readOnly\n    access: restricted",
			want: "codex.turn_sandbox_policy.access",
		},
		{
			name: "unknown_field",
			body: "type: workspaceWrite\n    writableRoots: []\n    networkAccess: false\n    excludeTmpdirEnvVar: false\n    excludeSlashTmp: false\n    extra: nope",
			want: "unsupported field",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeTempWorkflow(t, `---
repo:
  clone_url: file:///tmp/repo
tracker:
  kind: linear
  project_slug: platform
agent:
  default: codex-app-server
codex:
  turn_sandbox_policy:
    `+tc.body+`
---
prompt
`)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load(%q) succeeded; want codex.turn_sandbox_policy rejection", path)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load(%q) error = %q; want %q", path, err.Error(), tc.want)
			}
		})
	}
}

// TestValidateConfigRejectsUnsupportedTurnSandboxPolicyType pins the
// defensive validateConfig guard for codex.turn_sandbox_policy. The YAML
// loader's UnmarshalYAML rejects unknown policy types before validateConfig
// runs, so this branch only fires for a programmatically constructed Config.
// The direct call keeps the guard covered across the #410 validateConfig
// split, which relocates it into a per-section validator.
func TestValidateConfigRejectsUnsupportedTurnSandboxPolicyType(t *testing.T) {
	t.Parallel()
	wf, err := Load(writeTempWorkflow(t, `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
tracker:
  kind: gitea
---
prompt
`))
	if err != nil {
		t.Fatalf("Load valid config: unexpected error %v", err)
	}
	cfg := wf.Config
	cfg.Codex.TurnSandboxPolicy = CodexSandboxPolicy{Type: "bogus"}
	err = validateConfig("WORKFLOW.md", cfg)
	if err == nil {
		t.Fatal("validateConfig(turn_sandbox_policy.type=bogus) = nil; want unsupported type rejection")
	}
	if !strings.Contains(err.Error(), "codex.turn_sandbox_policy.type") || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("validateConfig error = %q; want codex.turn_sandbox_policy.type unsupported rejection", err)
	}
}

func TestLoadAcceptsTypedCodexWorkspaceWriteSandboxPolicy(t *testing.T) {
	t.Parallel()
	path := writeTempWorkflow(t, `---
repo:
  clone_url: file:///tmp/repo
tracker:
  kind: linear
  project_slug: platform
agent:
  default: codex-app-server
codex:
  turn_sandbox_policy:
    type: workspaceWrite
    writableRoots:
      - /tmp/aiops-workspace
    networkAccess: false
    excludeTmpdirEnvVar: true
    excludeSlashTmp: false
---
prompt
`)
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q): %v", path, err)
	}
	policy := wf.Config.Codex.TurnSandboxPolicy
	if policy.Type != CodexSandboxWorkspaceWrite {
		t.Fatalf("Codex.TurnSandboxPolicy.Type = %#v, want workspaceWrite", policy.Type)
	}
	if got, want := policy.WritableRoots, []string{"/tmp/aiops-workspace"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Codex.TurnSandboxPolicy.WritableRoots = %#v, want %#v", got, want)
	}
	if policy.NetworkAccess != false {
		t.Fatalf("Codex.TurnSandboxPolicy.NetworkAccess = %#v, want false", policy.NetworkAccess)
	}
	if policy.ExcludeTmpdirEnvVar != true {
		t.Fatalf("Codex.TurnSandboxPolicy.ExcludeTmpdirEnvVar = %#v, want true", policy.ExcludeTmpdirEnvVar)
	}
	if policy.ExcludeSlashTmp != false {
		t.Fatalf("Codex.TurnSandboxPolicy.ExcludeSlashTmp = %#v, want false", policy.ExcludeSlashTmp)
	}
}

// TestLoadDerivesTurnSandboxPolicyFromThreadSandbox pins the #472 fix: when an
// operator sets codex.thread_sandbox but not codex.turn_sandbox_policy, the
// per-turn policy is derived from thread_sandbox instead of silently forcing
// workspace-write on every turn. Each case asserts the derived type so a
// regression that re-pins workspace-write (the pre-fix behavior) fails CI.
func TestLoadDerivesTurnSandboxPolicyFromThreadSandbox(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		threadSandbox string
		wantType      string
	}{
		{name: "danger_full_access", threadSandbox: "danger-full-access", wantType: CodexSandboxDangerFullAccess},
		{name: "read_only", threadSandbox: "read-only", wantType: CodexSandboxReadOnly},
		{name: "workspace_write", threadSandbox: "workspace-write", wantType: CodexSandboxWorkspaceWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeTempWorkflow(t, `---
repo:
  clone_url: file:///tmp/repo
tracker:
  kind: linear
  project_slug: platform
agent:
  default: codex-app-server
codex:
  thread_sandbox: `+tc.threadSandbox+`
---
prompt
`)
			wf, err := Load(path)
			if err != nil {
				t.Fatalf("Load(%q): %v", path, err)
			}
			if got := wf.Config.Codex.TurnSandboxPolicy.Type; got != tc.wantType {
				t.Fatalf("thread_sandbox=%q: TurnSandboxPolicy.Type = %q; want %q", tc.threadSandbox, got, tc.wantType)
			}
		})
	}
}

// TestLoadExplicitTurnSandboxPolicyOverridesThreadSandbox guards the override
// contract: even when thread_sandbox would derive a different type, an
// explicit turn_sandbox_policy wins. Without the turnSandboxPolicySet signal a
// naive IsZero()-only check would still derive dangerFullAccess here.
func TestLoadExplicitTurnSandboxPolicyOverridesThreadSandbox(t *testing.T) {
	t.Parallel()
	path := writeTempWorkflow(t, `---
repo:
  clone_url: file:///tmp/repo
tracker:
  kind: linear
  project_slug: platform
agent:
  default: codex-app-server
codex:
  thread_sandbox: danger-full-access
  turn_sandbox_policy:
    type: workspaceWrite
    writableRoots: []
    networkAccess: false
    excludeTmpdirEnvVar: false
    excludeSlashTmp: false
---
prompt
`)
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q): %v", path, err)
	}
	if got := wf.Config.Codex.TurnSandboxPolicy.Type; got != CodexSandboxWorkspaceWrite {
		t.Fatalf("explicit turn_sandbox_policy with thread_sandbox=danger-full-access: TurnSandboxPolicy.Type = %q; want %q (explicit must override)", got, CodexSandboxWorkspaceWrite)
	}
}

// TestDefaultConfig_AlignsToSPEC_6_4 pins every SPEC §6.4 default
// `DefaultConfig()` is responsible for. Each field gets its own
// assertion so a regression on any one of them surfaces in CI rather
// than being papered over by a sibling field's check. Mirrors the
// Elixir reference (`elixir/lib/symphony_elixir/config/schema.ex`
// lines 53, 54, 93, 131, 160).
func TestDefaultConfig_AlignsToSPEC_6_4(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()

	if cfg.Codex.Command != "codex app-server" {
		t.Errorf("Codex.Command = %q, want SPEC §6.4 default %q", cfg.Codex.Command, "codex app-server")
	}
	if cfg.Agent.MaxConcurrentAgents != 10 {
		t.Errorf("Agent.MaxConcurrentAgents = %d, want SPEC §6.4 default 10", cfg.Agent.MaxConcurrentAgents)
	}
	if cfg.Agent.MaxTurns != 20 {
		t.Errorf("Agent.MaxTurns = %d, want SPEC §6.4 default 20", cfg.Agent.MaxTurns)
	}

	// SPEC §6.4 says `<system-temp>/symphony_workspaces`; Elixir resolves
	// the same via Path.join(System.tmp_dir!(), ...). Comparing against
	// os.TempDir() pins the contract that the default lives under the
	// system temp dir (and remains writable for non-root processes),
	// rather than against a hard-coded `/tmp` that would diverge on
	// hosts with a non-default TMPDIR.
	wantRoot := filepath.Join(os.TempDir(), "symphony_workspaces")
	if cfg.Workspace.Root != wantRoot {
		t.Errorf("Workspace.Root = %q, want SPEC §6.4 default %q", cfg.Workspace.Root, wantRoot)
	}

	wantActive := []string{"Todo", "In Progress"}
	if !reflect.DeepEqual(cfg.Tracker.ActiveStates, wantActive) {
		t.Errorf("Tracker.ActiveStates = %#v, want SPEC §6.4 default %#v", cfg.Tracker.ActiveStates, wantActive)
	}
	wantTerminal := []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"}
	if !reflect.DeepEqual(cfg.Tracker.TerminalStates, wantTerminal) {
		t.Errorf("Tracker.TerminalStates = %#v, want SPEC §6.4 default %#v (order matters; mirrors Elixir schema.ex:54)", cfg.Tracker.TerminalStates, wantTerminal)
	}

	// SPEC §6.4 marks tracker.kind REQUIRED, so DefaultConfig must
	// not preset a value: a workflow that omits the field has to
	// fail the loader rather than silently inherit a profile-specific
	// implementation default. See DEVIATIONS D28 / #244 for the
	// retired `gitea` preset.
	if cfg.Tracker.Kind != "" {
		t.Errorf("Tracker.Kind = %q, want empty (SPEC §6.4 marks the field REQUIRED; the loader rejects an empty kind)", cfg.Tracker.Kind)
	}

	// Codex sub-defaults retained from the previous pinning so we do
	// not regress them when extending the SPEC §6.4 coverage above.
	if cfg.Codex.ThreadSandbox != "workspace-write" {
		t.Errorf("Codex.ThreadSandbox = %q, want workspace-write", cfg.Codex.ThreadSandbox)
	}
	if cfg.Codex.TurnTimeoutMs != 3600000 || cfg.Codex.ReadTimeoutMs != 5000 || cfg.Codex.StallTimeoutMs != 300000 {
		t.Errorf("Codex timeouts = turn %d read %d stall %d, want 3600000/5000/300000", cfg.Codex.TurnTimeoutMs, cfg.Codex.ReadTimeoutMs, cfg.Codex.StallTimeoutMs)
	}
}

// TestLoad_RejectsMissingTrackerKind pins SPEC §6.4's REQUIRED semantics
// for `tracker.kind`. A workflow that declares front matter but omits
// the field must fail Load with an error that names the field, the file
// path, and the supported values. Before #244 closed D28 the loader
// silently substituted DefaultConfig's `gitea`, so the assertion below
// also guards against a regression that re-introduces the platform
// default.
func TestLoad_RejectsMissingTrackerKind(t *testing.T) {
	t.Parallel()
	path := writeTempWorkflow(t, `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://example.invalid/repo.git
---
prompt`)
	_, err := Load(path)
	if err == nil {
		t.Fatalf("Load: expected error for missing tracker.kind, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"tracker.kind", "required", "SPEC §6.4", path} {
		if !strings.Contains(msg, want) {
			t.Fatalf("Load error %q: want substring %q", msg, want)
		}
	}
}

// TestLoad_RejectsExplicitZeroMaxConcurrentAgents pins the loader's
// Elixir-aligned rejection of `agent.max_concurrent_agents: 0`. Upstream
// `validate_number(:max_concurrent_agents, greater_than: 0)` errors on
// explicit zero rather than silently substituting the default — see
// `elixir/lib/symphony_elixir/config/schema.ex:131,145`.
func TestLoad_RejectsExplicitZeroMaxConcurrentAgents(t *testing.T) {
	t.Parallel()
	path := writeTempWorkflow(t, `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://example.invalid/repo.git
tracker:
  kind: gitea
agent:
  max_concurrent_agents: 0
---
prompt`)
	_, err := Load(path)
	if err == nil {
		t.Fatalf("Load with agent.max_concurrent_agents: 0 succeeded; want validation error")
	}
	if !strings.Contains(err.Error(), "agent.max_concurrent_agents") {
		t.Fatalf("error %q does not mention agent.max_concurrent_agents", err)
	}
}

// TestLoad_AbsentMaxConcurrentAgentsKeepsSPECDefault pins that a
// front-matter block which omits `agent.max_concurrent_agents` still
// inherits the SPEC §6.4 default of 10 (not the loader floor that the
// previous code carried).
func TestLoad_AbsentMaxConcurrentAgentsKeepsSPECDefault(t *testing.T) {
	t.Parallel()
	path := writeTempWorkflow(t, `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://example.invalid/repo.git
tracker:
  kind: gitea
---
prompt`)
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if wf.Config.Agent.MaxConcurrentAgents != 10 {
		t.Fatalf("Agent.MaxConcurrentAgents = %d, want SPEC §6.4 default 10", wf.Config.Agent.MaxConcurrentAgents)
	}
}

func TestLoad_RejectsInvalidCodexAppServerTimeouts(t *testing.T) {
	t.Parallel()
	path := writeTempWorkflow(t, `---
repo:
  clone_url: file:///tmp/repo
tracker:
  kind: linear
  project_slug: platform
agent:
  default: codex-app-server
codex:
  turn_timeout_ms: 0
  read_timeout_ms: 0
  stall_timeout_ms: -1
---
prompt
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load: expected error for invalid codex app-server timeouts, got nil")
	}
	for _, want := range []string{"codex.turn_timeout_ms", "codex.read_timeout_ms", "codex.stall_timeout_ms"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q; want substring %q", err, want)
		}
	}
}

func TestLoadRejectsAgentEnvPassthroughTrackerTokens(t *testing.T) {
	for _, tc := range []struct {
		name       string
		envName    string
		envValue   string
		body       string
		want       string
		wantReason string
	}{
		{
			name: "codex_linear_api_key",
			body: `---
repo:
  owner: o
  name: n
  clone_url: git@example.com:o/n.git
tracker:
  kind: gitea
codex:
  env_passthrough:
    - LINEAR_API_KEY
---
hello
`,
			want:       "codex.env_passthrough[0]",
			wantReason: "tracker/API token",
		},
		{
			name: "claude_github_token",
			body: `---
repo:
  owner: o
  name: n
  clone_url: git@example.com:o/n.git
tracker:
  kind: gitea
claude:
  env_passthrough:
    - GITHUB_TOKEN
---
hello
`,
			want:       "claude.env_passthrough[0]",
			wantReason: "tracker/API token",
		},
		{
			name:     "codex_tracker_api_key_source_env",
			envName:  "AIOPS_TEST_TRACKER_TOKEN",
			envValue: "tracker-secret",
			body: `---
repo:
  owner: o
  name: n
  clone_url: git@example.com:o/n.git
tracker:
  kind: gitea
  api_key: $AIOPS_TEST_TRACKER_TOKEN
codex:
  env_passthrough:
    - AIOPS_TEST_TRACKER_TOKEN
---
hello
`,
			want:       "codex.env_passthrough[0]",
			wantReason: "tracker.api_key environment variable",
		},
		{
			name:     "sandbox_tracker_api_key_source_env",
			envName:  "AIOPS_TEST_TRACKER_TOKEN",
			envValue: "tracker-secret",
			body: `---
repo:
  owner: o
  name: n
  clone_url: git@example.com:o/n.git
tracker:
  kind: gitea
  api_key: $AIOPS_TEST_TRACKER_TOKEN
sandbox:
  enabled: true
  backend: bubblewrap
  env_allowlist:
    - AIOPS_TEST_TRACKER_TOKEN
---
hello
`,
			want:       "sandbox.env_allowlist[0]",
			wantReason: "tracker.api_key environment variable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envName != "" {
				t.Setenv(tc.envName, tc.envValue)
			}
			_, err := Load(writeTempWorkflow(t, tc.body))
			if err == nil {
				t.Fatal("Load succeeded, want denied env_passthrough error")
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), tc.wantReason) {
				t.Fatalf("Load error = %q, want %s %s guidance", err, tc.want, tc.wantReason)
			}
		})
	}
}

func TestLoadedConfigRetainsTrackerAPIKeySourceEnv(t *testing.T) {
	t.Setenv("AIOPS_TEST_TRACKER_TOKEN", "tracker-secret")
	wf, err := Load(writeTempWorkflow(t, `---
repo:
  owner: o
  name: n
  clone_url: git@example.com:o/n.git
tracker:
  kind: gitea
  api_key: $AIOPS_TEST_TRACKER_TOKEN
---
hello
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	t.Setenv("AIOPS_TEST_TRACKER_TOKEN", "rotated-secret")
	reason := AgentEnvPassthroughDenyReasonForConfig("AIOPS_TEST_TRACKER_TOKEN", wf.Config)
	if !strings.Contains(reason, "tracker.api_key environment variable") {
		t.Fatalf("deny reason = %q, want tracker.api_key source env protection", reason)
	}
}
