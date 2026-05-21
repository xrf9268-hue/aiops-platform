package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRejectsLinearTrackerWithoutProjectSlug(t *testing.T) {
	path := writeTempWorkflow(t, `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
tracker:
  kind: linear
---
Prompt body
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(linear without tracker.project_slug) = nil, want validation error")
	}
	for _, want := range []string{path, "tracker.project_slug", "required", "tracker.kind is linear"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Load error = %q, want substring %q", err, want)
		}
	}
}

func TestLoadRejectsLinearServiceRouteWithoutAnyProjectSlug(t *testing.T) {
	path := writeTempWorkflow(t, `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
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
Prompt body
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(linear service route without any project slug) = nil, want validation error")
	}
	for _, want := range []string{path, "services[0].tracker.project_slug", "tracker.project_slug", "required"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Load error = %q, want substring %q", err, want)
		}
	}
}

func TestLoadRejectsNonPositiveHooksTimeoutMs(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "top-level negative",
			body: `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
hooks:
  timeout_ms: -1
---
Prompt body
`,
		},
		{
			name: "top-level zero",
			body: `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
hooks:
  timeout_ms: 0
---
Prompt body
`,
		},
		{
			name: "legacy negative",
			body: `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
workspace:
  hooks:
    timeout_ms: -1
---
Prompt body
`,
		},
		{
			name: "legacy zero",
			body: `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
workspace:
  hooks:
    timeout_ms: 0
---
Prompt body
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempWorkflow(t, tt.body)
			_, err := Load(path)
			if err == nil {
				t.Fatal("Load succeeded with explicit non-positive hooks timeout, want validation error")
			}
			for _, want := range []string{path, "timeout_ms", "positive integer"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Load error = %q, want substring %q", err, want)
				}
			}
		})
	}
}

func TestLoadResolvesExactEnvironmentReferences(t *testing.T) {
	workspaceRoot := filepath.Join(t.TempDir(), "workspaces")
	credentialPath := filepath.Join(t.TempDir(), "token")
	t.Setenv("AIOPS_TEST_REPO_URL", "git@example.com:o/r.git")
	t.Setenv("AIOPS_TEST_TRACKER_KEY", "tracker-secret")
	t.Setenv("AIOPS_TEST_TRACKER_BASE_URL", "https://tracker.example/api")
	t.Setenv("AIOPS_TEST_WORKSPACE_ROOT", workspaceRoot)
	t.Setenv("AIOPS_TEST_CODEX_COMMAND", "codex app-server")
	t.Setenv("AIOPS_TEST_CLAUDE_COMMAND", "claude --print")
	t.Setenv("AIOPS_TEST_CREDENTIAL_PATH", credentialPath)

	path := writeTempWorkflow(t, `---
repo:
  owner: o
  name: r
  clone_url: $AIOPS_TEST_REPO_URL
tracker:
  api_key: ${AIOPS_TEST_TRACKER_KEY}
  base_url: $AIOPS_TEST_TRACKER_BASE_URL
workspace:
  root: $AIOPS_TEST_WORKSPACE_ROOT
codex:
  command: $AIOPS_TEST_CODEX_COMMAND
claude:
  command: ${AIOPS_TEST_CLAUDE_COMMAND}
sandbox:
  credential_files:
    - ${AIOPS_TEST_CREDENTIAL_PATH}
---
Prompt body
`)

	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := wf.Config.Repo.CloneURL; got != "git@example.com:o/r.git" {
		t.Fatalf("repo.clone_url = %q", got)
	}
	if got := wf.Config.Tracker.APIKey; got != "tracker-secret" {
		t.Fatalf("tracker.api_key = %q", got)
	}
	if got := wf.Config.Tracker.BaseURL; got != "https://tracker.example/api" {
		t.Fatalf("tracker.base_url = %q", got)
	}
	if got := wf.Config.Workspace.Root; got != workspaceRoot {
		t.Fatalf("workspace.root = %q, want %q", got, workspaceRoot)
	}
	if got := wf.Config.Codex.Command; got != "codex app-server" {
		t.Fatalf("codex.command = %q", got)
	}
	if got := wf.Config.Claude.Command; got != "claude --print" {
		t.Fatalf("claude.command = %q", got)
	}
	if len(wf.Config.Sandbox.CredentialFiles) != 1 || wf.Config.Sandbox.CredentialFiles[0] != credentialPath {
		t.Fatalf("sandbox.credential_files = %#v, want [%q]", wf.Config.Sandbox.CredentialFiles, credentialPath)
	}
}

func TestLoadPreservesLiteralDollarSubstrings(t *testing.T) {
	t.Setenv("USER", "expanded-user")
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: r
  clone_url: https://gitea.example/$USER/aiops
tracker:
  base_url: https://tracker.example/$USER/api
workspace:
  root: .aiops-$USER
codex:
  command: bash -lc 'echo $RUNTIME_VAR'
claude:
  command: claude --append-system-prompt '$LITERAL'
sandbox:
  credential_files:
    - /secrets/$TOKEN/path
---
Prompt body
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := wf.Config.Repo.CloneURL; got != "https://gitea.example/$USER/aiops" {
		t.Fatalf("repo.clone_url = %q", got)
	}
	if got := wf.Config.Tracker.BaseURL; got != "https://tracker.example/$USER/api" {
		t.Fatalf("tracker.base_url = %q", got)
	}
	wantRoot := filepath.Join(dir, ".aiops-$USER")
	if got := wf.Config.Workspace.Root; got != wantRoot {
		t.Fatalf("workspace.root = %q, want %q", got, wantRoot)
	}
	if got := wf.Config.Codex.Command; got != "bash -lc 'echo $RUNTIME_VAR'" {
		t.Fatalf("codex.command = %q", got)
	}
	if got := wf.Config.Claude.Command; got != "claude --append-system-prompt '$LITERAL'" {
		t.Fatalf("claude.command = %q", got)
	}
	if len(wf.Config.Sandbox.CredentialFiles) != 1 || wf.Config.Sandbox.CredentialFiles[0] != "/secrets/$TOKEN/path" {
		t.Fatalf("sandbox.credential_files = %#v, want literal dollar path", wf.Config.Sandbox.CredentialFiles)
	}
}

func TestLoadRejectsMissingExplicitEnvironmentReference(t *testing.T) {
	t.Setenv("AIOPS_TEST_EMPTY_TRACKER_KEY", "")
	path := writeTempWorkflow(t, `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
tracker:
  api_key: $AIOPS_TEST_EMPTY_TRACKER_KEY
---
Prompt body
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load = nil, want missing env reference error")
	}
	for _, want := range []string{path, "missing_tracker_api_key", "tracker.api_key", "$AIOPS_TEST_EMPTY_TRACKER_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Load error = %q, want substring %q", err, want)
		}
	}
}

func TestLoadRejectsMissingExplicitEnvironmentReferenceNonTrackerField(t *testing.T) {
	os.Unsetenv("AIOPS_TEST_UNSET_CODEX_COMMAND")
	path := writeTempWorkflow(t, `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
tracker:
  api_key: literal-key
codex:
  command: $AIOPS_TEST_UNSET_CODEX_COMMAND
---
Prompt body
`)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load = nil, want missing env reference error for codex.command")
	}
	if strings.Contains(err.Error(), "missing_tracker_api_key") {
		t.Fatalf("Load error = %q, codex.command must not use tracker.api_key category", err)
	}
	for _, want := range []string{path, "workflow_config_missing_value", "codex.command", "$AIOPS_TEST_UNSET_CODEX_COMMAND"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Load error = %q, want substring %q", err, want)
		}
	}
}
