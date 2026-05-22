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

func TestLoadResolvesLowercaseEnvironmentReferences(t *testing.T) {
	t.Setenv("aiops_test_repo_url", "git@example.com:o/lower.git")
	t.Setenv("linear_token", "linear-secret")
	t.Setenv("Mixed_Case_Url", "https://tracker.example/mixed")

	path := writeTempWorkflow(t, `---
repo:
  owner: o
  name: r
  clone_url: $aiops_test_repo_url
tracker:
  api_key: ${linear_token}
  base_url: $Mixed_Case_Url
---
Prompt body
`)

	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := wf.Config.Repo.CloneURL; got != "git@example.com:o/lower.git" {
		t.Fatalf("repo.clone_url = %q, lowercase env not resolved", got)
	}
	if got := wf.Config.Tracker.APIKey; got != "linear-secret" {
		t.Fatalf("tracker.api_key = %q, lowercase ${} env not resolved", got)
	}
	if got := wf.Config.Tracker.BaseURL; got != "https://tracker.example/mixed" {
		t.Fatalf("tracker.base_url = %q, mixed-case env not resolved", got)
	}
}

// TestSplitFrontMatter_LineAwareClosingFence pins the #231 contract.
// The closing fence is a line that is exactly "---" (with optional
// CR before the LF). A bare substring match is wrong because YAML
// block scalars and quoted strings can legally contain a "---" line.
//
// Boundary-coverage rule for "must be exactly the three-character
// sequence on a line":
//   - "---" \n → accept (=N cap, exact)
//   - "---" \r\n → accept (paired line-ending edge)
//   - "----" \n → reject (=N+1, one extra char on the line)
//   - "--- " \n → reject (trailing whitespace is not the fence)
//   - "  ---" \n → reject (indented; not at column 0)
//   - missing fence → unchanged "(empty, s)" contract
func TestSplitFrontMatter_LineAwareClosingFence(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantFront string
		wantBody  string
	}{
		{
			name:      "lf_exact_fence",
			input:     "---\nfoo: bar\n---\nbody\n",
			wantFront: "foo: bar\n",
			wantBody:  "body\n",
		},
		{
			name:      "crlf_exact_fence",
			input:     "---\r\nfoo: bar\r\n---\r\nbody\r\n",
			wantFront: "foo: bar\r\n",
			wantBody:  "body\r\n",
		},
		{
			name: "block_scalar_with_inner_dashes_does_not_close",
			input: "---\n" +
				"description: |\n" +
				"  Reference: see below.\n" +
				"  ---\n" +
				"  More notes.\n" +
				"agent:\n" +
				"  default: codex\n" +
				"---\n" +
				"prompt body\n",
			wantFront: "description: |\n" +
				"  Reference: see below.\n" +
				"  ---\n" +
				"  More notes.\n" +
				"agent:\n" +
				"  default: codex\n",
			wantBody: "prompt body\n",
		},
		{
			name:      "four_dashes_is_not_fence",
			input:     "---\nfoo: bar\n----\nstill front\n---\nbody\n",
			wantFront: "foo: bar\n----\nstill front\n",
			wantBody:  "body\n",
		},
		{
			name:      "trailing_space_is_not_fence",
			input:     "---\nfoo: bar\n--- \nstill front\n---\nbody\n",
			wantFront: "foo: bar\n--- \nstill front\n",
			wantBody:  "body\n",
		},
		{
			name:      "indented_dashes_are_not_fence",
			input:     "---\nfoo: bar\n  ---\nstill front\n---\nbody\n",
			wantFront: "foo: bar\n  ---\nstill front\n",
			wantBody:  "body\n",
		},
		{
			name:      "missing_closing_fence_returns_empty_front",
			input:     "---\nfoo: bar\nno fence here\n",
			wantFront: "",
			wantBody:  "---\nfoo: bar\nno fence here\n",
		},
		{
			name:      "no_opening_fence_returns_empty_front",
			input:     "no fence at all\n",
			wantFront: "",
			wantBody:  "no fence at all\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			front, body := splitFrontMatter(tc.input)
			if front != tc.wantFront {
				t.Fatalf("front =\n%q\nwant\n%q", front, tc.wantFront)
			}
			if body != tc.wantBody {
				t.Fatalf("body =\n%q\nwant\n%q", body, tc.wantBody)
			}
		})
	}
}

// TestLoadAcceptsBlockScalarContainingDashes is the user-visible
// regression: a workflow file whose front-matter block scalar
// contains a "---" line must Load() cleanly and surface the values
// past that line, not get truncated to a partial Config.
func TestLoadAcceptsBlockScalarContainingDashes(t *testing.T) {
	path := writeTempWorkflow(t, `---
description: |
  Reference: see SPEC §5.2 for delimiter handling.
  ---
  See above for the fence-rule note.
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
tracker:
  kind: linear
  project_slug: example
  api_key: $LINEAR_API_KEY_BLOCK_SCALAR_TEST
agent:
  default: codex
---
Prompt body across the inner --- line.
`)
	t.Setenv("LINEAR_API_KEY_BLOCK_SCALAR_TEST", "linear-secret")
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := wf.Config.Agent.Default; got != "codex" {
		t.Fatalf("agent.default = %q, want %q (front matter was truncated by inner --- line)", got, "codex")
	}
	if got := wf.Config.Tracker.APIKey; got != "linear-secret" {
		t.Fatalf("tracker.api_key = %q, want %q", got, "linear-secret")
	}
	if !strings.Contains(wf.PromptTemplate, "Prompt body across the inner --- line.") {
		t.Fatalf("prompt template missing expected body: %q", wf.PromptTemplate)
	}
}
