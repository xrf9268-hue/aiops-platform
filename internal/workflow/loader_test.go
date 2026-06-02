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
tracker:
  kind: gitea
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
tracker:
  kind: gitea
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
tracker:
  kind: gitea
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
tracker:
  kind: gitea
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

// TestLoadMigratesLegacyTrackerBaseURL pins the #242 deprecation alias:
// `tracker.base_url` in a legacy WORKFLOW.md still parses, surfaces on the
// renamed `Tracker.Endpoint` field, and clears the legacy `BaseURL` field
// so downstream code reads only one place.
func TestLoadMigratesLegacyTrackerBaseURL(t *testing.T) {
	path := writeTempWorkflow(t, `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
tracker:
  kind: gitea
  base_url: https://tracker.example/legacy-base-url
---
prompt body
`)
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := wf.Config.Tracker.Endpoint; got != "https://tracker.example/legacy-base-url" {
		t.Fatalf("Tracker.Endpoint = %q, want legacy base_url migrated to endpoint", got)
	}
	if wf.Config.Tracker.BaseURL != "" {
		t.Fatalf("Tracker.BaseURL = %q, want cleared after migration", wf.Config.Tracker.BaseURL)
	}
}

// TestLoadPrefersEndpointWhenBothFieldsSet: explicit `tracker.endpoint` wins
// over the deprecated `tracker.base_url`.
func TestLoadPrefersEndpointWhenBothFieldsSet(t *testing.T) {
	path := writeTempWorkflow(t, `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
tracker:
  kind: gitea
  endpoint: https://tracker.example/canonical
  base_url: https://tracker.example/legacy
---
prompt body
`)
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := wf.Config.Tracker.Endpoint; got != "https://tracker.example/canonical" {
		t.Fatalf("Tracker.Endpoint = %q, want canonical endpoint to win over base_url", got)
	}
}

func TestLoadMigratesGiteaProjectSlugBaseURLToEndpoint(t *testing.T) {
	path := writeTempWorkflow(t, `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
tracker:
  kind: gitea
  project_slug: https://gitea.example/legacy
---
prompt body
`)
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := wf.Config.Tracker.Endpoint; got != "https://gitea.example/legacy" {
		t.Fatalf("Tracker.Endpoint = %q, want legacy Gitea project_slug migrated to endpoint", got)
	}
	if wf.Config.Tracker.ProjectSlug != "" {
		t.Fatalf("Tracker.ProjectSlug = %q, want cleared after Gitea migration", wf.Config.Tracker.ProjectSlug)
	}
}

func TestLoadPrefersGiteaEndpointOverProjectSlugBaseURL(t *testing.T) {
	path := writeTempWorkflow(t, `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
tracker:
  kind: gitea
  endpoint: https://gitea.example/canonical
  project_slug: https://gitea.example/legacy
---
prompt body
`)
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := wf.Config.Tracker.Endpoint; got != "https://gitea.example/canonical" {
		t.Fatalf("Tracker.Endpoint = %q, want tracker.endpoint to win over legacy Gitea project_slug", got)
	}
	if wf.Config.Tracker.ProjectSlug != "" {
		t.Fatalf("Tracker.ProjectSlug = %q, want cleared after Gitea migration", wf.Config.Tracker.ProjectSlug)
	}
}

func TestLoadPrefersLegacyBaseURLOverGiteaProjectSlugBaseURL(t *testing.T) {
	path := writeTempWorkflow(t, `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
tracker:
  kind: gitea
  base_url: https://gitea.example/base-url
  project_slug: https://gitea.example/project-slug
---
prompt body
`)
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := wf.Config.Tracker.Endpoint; got != "https://gitea.example/base-url" {
		t.Fatalf("Tracker.Endpoint = %q, want legacy base_url to win over legacy Gitea project_slug", got)
	}
	if wf.Config.Tracker.ProjectSlug != "" {
		t.Fatalf("Tracker.ProjectSlug = %q, want cleared after Gitea migration", wf.Config.Tracker.ProjectSlug)
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
  kind: gitea
  api_key: ${AIOPS_TEST_TRACKER_KEY}
  endpoint: $AIOPS_TEST_TRACKER_BASE_URL
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
	if got := wf.Config.Tracker.Endpoint; got != "https://tracker.example/api" {
		t.Fatalf("tracker.endpoint = %q", got)
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
  kind: gitea
  endpoint: https://tracker.example/$USER/api
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
	if got := wf.Config.Tracker.Endpoint; got != "https://tracker.example/$USER/api" {
		t.Fatalf("tracker.endpoint = %q", got)
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
  kind: gitea
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
	if err := os.Unsetenv("AIOPS_TEST_UNSET_CODEX_COMMAND"); err != nil {
		t.Fatalf("Unsetenv(%q) = %v; want nil", "AIOPS_TEST_UNSET_CODEX_COMMAND", err)
	}
	path := writeTempWorkflow(t, `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
tracker:
  kind: gitea
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
  kind: gitea
  api_key: ${linear_token}
  endpoint: $Mixed_Case_Url
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
	if got := wf.Config.Tracker.Endpoint; got != "https://tracker.example/mixed" {
		t.Fatalf("tracker.endpoint = %q, mixed-case env not resolved", got)
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
				"  default: codex-app-server\n" +
				"---\n" +
				"prompt body\n",
			wantFront: "description: |\n" +
				"  Reference: see below.\n" +
				"  ---\n" +
				"  More notes.\n" +
				"agent:\n" +
				"  default: codex-app-server\n",
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

// TestSplitFrontMatter_EmptyFrontMatterIsPromptOnly covers the
// Codex-flagged edge case from PR #264: an opening fence followed
// immediately by a closing fence (`---\n---\n...`) returns an empty
// front block. The loader treats empty fronts as prompt-only via
// strings.TrimSpace(front) != "", so HasFrontMatterAt must report
// the same — otherwise the workflow_resolved event labels the file
// `source=file` while Load() boots with default Config.
func TestSplitFrontMatter_EmptyFrontMatterIsPromptOnly(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantFront string
		wantBody  string
	}{
		{
			name:      "opening_then_immediate_closing",
			input:     "---\n---\nbody\n",
			wantFront: "",
			wantBody:  "body\n",
		},
		{
			name:      "opening_blank_line_closing",
			input:     "---\n\n---\nbody\n",
			wantFront: "\n",
			wantBody:  "body\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			front, body := splitFrontMatter(tc.input)
			if front != tc.wantFront {
				t.Fatalf("front = %q, want %q", front, tc.wantFront)
			}
			if body != tc.wantBody {
				t.Fatalf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}

// TestHasFrontMatterAt_EmptyFrontIsPromptOnly pins the
// resolver-vs-loader agreement: an opening fence immediately followed
// by a closing fence is reported as NOT having front matter, mirroring
// the loader's `TrimSpace(front) != ""` check. Without this,
// `workflow_resolved` would carry `source=file` while Load() produced
// schema defaults.
func TestHasFrontMatterAt_EmptyFrontIsPromptOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(path, []byte("---\n---\nprompt body\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := HasFrontMatterAt(path)
	if err != nil {
		t.Fatalf("HasFrontMatterAt: %v", err)
	}
	if got {
		t.Fatalf("HasFrontMatterAt = true for empty front matter; loader treats this case as prompt_only, so the resolver must agree")
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
  default: codex-app-server
---
Prompt body across the inner --- line.
`)
	t.Setenv("LINEAR_API_KEY_BLOCK_SCALAR_TEST", "linear-secret")
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := wf.Config.Agent.Default; got != "codex-app-server" {
		t.Fatalf("agent.default = %q, want %q (front matter was truncated by inner --- line)", got, "codex-app-server")
	}
	if got := wf.Config.Tracker.APIKey; got != "linear-secret" {
		t.Fatalf("tracker.api_key = %q, want %q", got, "linear-secret")
	}
	if !strings.Contains(wf.PromptTemplate, "Prompt body across the inner --- line.") {
		t.Fatalf("prompt template missing expected body: %q", wf.PromptTemplate)
	}
}

func TestLoadAcceptsLinearGraphQLMutationOptIn(t *testing.T) {
	path := writeTempWorkflow(t, `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
codex:
  linear_graphql:
    allow_mutations: true
    allowed_mutations:
      - issueUpdate
      - commentCreate
tracker:
  kind: gitea
---
Prompt body
`)
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !wf.Config.Codex.LinearGraphQL.AllowMutations {
		t.Fatalf("AllowMutations = false, want true")
	}
	if got := wf.Config.Codex.LinearGraphQL.AllowedMutations; len(got) != 2 || got[0] != "issueUpdate" || got[1] != "commentCreate" {
		t.Fatalf("AllowedMutations = %#v, want [issueUpdate commentCreate]", got)
	}
}

func TestLoadRejectsAllowedMutationsWithoutAllowMutations(t *testing.T) {
	path := writeTempWorkflow(t, `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
codex:
  linear_graphql:
    allowed_mutations:
      - issueUpdate
tracker:
  kind: gitea
---
Prompt body
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(allowed_mutations without allow_mutations) = nil, want validation error")
	}
	for _, want := range []string{path, "allowed_mutations requires", "allow_mutations: true"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Load error = %q, want substring %q", err, want)
		}
	}
}

func TestLoadRejectsInvalidAllowedMutationNames(t *testing.T) {
	tests := []struct {
		name  string
		entry string
	}{
		{name: "empty string", entry: `""`},
		{name: "leading space", entry: `" issueUpdate"`},
		{name: "punctuation", entry: `"issue-update"`},
		{name: "leading digit", entry: `"1issueUpdate"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTempWorkflow(t, `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
codex:
  linear_graphql:
    allow_mutations: true
    allowed_mutations:
      - `+tt.entry+`
tracker:
  kind: gitea
---
Prompt body
`)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load(invalid allowed_mutations entry %s) = nil, want validation error", tt.entry)
			}
			if !strings.Contains(err.Error(), "codex.linear_graphql.allowed_mutations") {
				t.Fatalf("Load error = %q, want it to name codex.linear_graphql.allowed_mutations", err)
			}
		})
	}
}

func TestLoadRejectsDuplicateAllowedMutations(t *testing.T) {
	path := writeTempWorkflow(t, `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
codex:
  linear_graphql:
    allow_mutations: true
    allowed_mutations:
      - issueUpdate
      - commentCreate
      - issueUpdate
tracker:
  kind: gitea
---
Prompt body
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(duplicate allowed_mutations) = nil, want validation error")
	}
	for _, want := range []string{path, "allowed_mutations[2]", "duplicates", "issueUpdate"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Load error = %q, want substring %q", err, want)
		}
	}
}

func TestLoadRejectsLinearGraphQLOnClaudeEmbed(t *testing.T) {
	path := writeTempWorkflow(t, `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
claude:
  linear_graphql:
    allow_mutations: true
tracker:
  kind: gitea
---
Prompt body
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load(claude.linear_graphql) = nil, want validation error")
	}
	for _, want := range []string{path, "claude.linear_graphql", "codex.linear_graphql"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Load error = %q, want substring %q", err, want)
		}
	}
}

// TestServerPortSetTracksFrontMatterPresence pins the #375 enabler: the
// loader must record whether server.port was explicitly present in the
// front matter, so `worker --print-config` can label the port `workflow`
// vs `default`. An explicit value sets PortSet; relying on the
// DefaultConfig port (4000) leaves it false, even when the file carries
// other server keys.
func TestServerPortSetTracksFrontMatterPresence(t *testing.T) {
	explicit := writeTempWorkflow(t, `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
server:
  port: 5000
tracker:
  kind: linear
  project_slug: platform
---
Prompt body
`)
	wf, err := Load(explicit)
	if err != nil {
		t.Fatalf("Load(explicit server.port): %v", err)
	}
	if !wf.Config.Server.PortSet() {
		t.Fatal("PortSet() = false for explicit server.port, want true")
	}
	if wf.Config.Server.Port != 5000 {
		t.Fatalf("Server.Port = %d, want 5000", wf.Config.Server.Port)
	}

	// server.host present but server.port absent: PortSet must stay false
	// so the inherited default port is labeled `default`, not `workflow`.
	hostOnly := writeTempWorkflow(t, `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
server:
  host: 127.0.0.1
tracker:
  kind: linear
  project_slug: platform
---
Prompt body
`)
	wf, err = Load(hostOnly)
	if err != nil {
		t.Fatalf("Load(host only): %v", err)
	}
	if wf.Config.Server.PortSet() {
		t.Fatal("PortSet() = true when only server.host present, want false")
	}
	if wf.Config.Server.Port != 4000 {
		t.Fatalf("Server.Port = %d, want DefaultConfig 4000", wf.Config.Server.Port)
	}

	// DefaultConfig (no file) must not report the port as workflow-sourced.
	if DefaultConfig().Server.PortSet() {
		t.Fatal("DefaultConfig().Server.PortSet() = true, want false")
	}
}
