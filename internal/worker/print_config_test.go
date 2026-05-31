package worker

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// TestPrintConfig_MasksTrackerAPIKey verifies the secret-masking
// contract from the spec. Even when --print-config is used legitimately
// for debugging, the API key must never reach stdout. We test with the
// env-var indirection style that examples/WORKFLOW.md uses, since that
// is the realistic source of a non-empty key.
func TestPrintConfig_MasksTrackerAPIKey(t *testing.T) {
	t.Setenv("AIOPS_TEST_LINEAR_KEY", "lin_super_secret_value")
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: linear\n  project_slug: platform\n  api_key: $AIOPS_TEST_LINEAR_KEY\n---\nprompt\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := printConfig(dir, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	got := stdout.String()
	if strings.Contains(got, "lin_super_secret_value") {
		t.Fatalf("api_key value leaked into stdout:\n%s", got)
	}
	if !strings.Contains(got, `"api_key": "***"`) {
		t.Fatalf("api_key not masked; stdout:\n%s", got)
	}
}

// TestPrintConfig_DefaultSource verifies the simplest case: an empty
// workdir resolves to source=default, and the JSON output reports it
// without a path or shadowed_by field.
func TestPrintConfig_DefaultSource(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := printConfig(dir, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	var out struct {
		Resolution struct {
			Source     string   `json:"source"`
			Path       string   `json:"path,omitempty"`
			ShadowedBy []string `json:"shadowed_by,omitempty"`
		} `json:"resolution"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\nstdout: %s", err, stdout.String())
	}
	if out.Resolution.Source != "default" {
		t.Fatalf("resolution.source = %q, want %q", out.Resolution.Source, "default")
	}
	if out.Resolution.Path != "" {
		t.Fatalf("resolution.path = %q, want empty", out.Resolution.Path)
	}
}

// TestPrintConfig_FileSourceWithPromptCanary covers two contracts at once:
//
//  1. Source=file populates config from the front matter (here: a
//     non-default agent.default and tracker.kind).
//  2. The prompt body is summarized rather than echoed. We embed a
//     recognizable canary string in the body and assert it never reaches
//     stdout. This is the spec's safety contract — see "Why prompt body
//     is summarized, not printed".
func TestPrintConfig_FileSourceWithPromptCanary(t *testing.T) {
	dir := t.TempDir()
	canary := "SHOULD_NOT_LEAK_xyz"
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\nagent:\n  default: codex-app-server\ntracker:\n  kind: linear\n  project_slug: platform\n---\nFirst line of prompt template.\nSecond line includes canary " + canary + " in the middle.\nMore body...\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := printConfig(dir, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}

	if strings.Contains(stdout.String(), canary) {
		t.Fatalf("canary %q leaked into stdout:\n%s", canary, stdout.String())
	}

	var out struct {
		Resolution struct {
			Source string `json:"source"`
		} `json:"resolution"`
		Config struct {
			Agent struct {
				Default string `json:"default"`
			} `json:"agent"`
		} `json:"config"`
		PromptTemplate struct {
			Length    int    `json:"length"`
			FirstLine string `json:"first_line"`
		} `json:"prompt_template"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\nstdout: %s", err, stdout.String())
	}
	if out.Resolution.Source != "file" {
		t.Fatalf("source = %q, want %q", out.Resolution.Source, "file")
	}
	if out.Config.Agent.Default != "codex-app-server" {
		t.Fatalf("agent.default = %q, want %q", out.Config.Agent.Default, "codex-app-server")
	}
	if out.PromptTemplate.FirstLine != "First line of prompt template." {
		t.Fatalf("first_line = %q", out.PromptTemplate.FirstLine)
	}
	if out.PromptTemplate.Length <= 0 {
		t.Fatalf("length = %d, want > 0", out.PromptTemplate.Length)
	}
}

// TestPrintConfig_RendersAgentTimeoutAsDurationString pins the contract
// from issue #53: agent.timeout in --print-config output must be a
// human-readable duration string (e.g. "30m0s") rather than a raw
// nanosecond integer. The default schema timeout is 30 minutes.
func TestPrintConfig_RendersAgentTimeoutAsDurationString(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := printConfig(dir, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	var out struct {
		Config struct {
			Agent struct {
				Timeout string `json:"timeout"`
			} `json:"agent"`
		} `json:"config"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\nstdout: %s", err, stdout.String())
	}
	if got, want := out.Config.Agent.Timeout, "30m0s"; got != want {
		t.Fatalf("agent.timeout = %q, want %q\nstdout:\n%s", got, want, stdout.String())
	}
	d, err := time.ParseDuration(out.Config.Agent.Timeout)
	if err != nil {
		t.Fatalf("ParseDuration(%q): %v", out.Config.Agent.Timeout, err)
	}
	if d != 30*time.Minute {
		t.Fatalf("round-trip duration = %v, want 30m", d)
	}
}

// TestPrintConfig_AgentTimeoutFromYAMLOverride covers the round-trip
// requirement: a YAML config supplying `timeout: 10m` must surface as
// "10m0s" in the print-config output, and that string must parse back
// to the same duration.
func TestPrintConfig_AgentTimeoutFromYAMLOverride(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\nagent:\n  timeout: 10m\ntracker:\n  kind: linear\n  project_slug: platform\n---\nprompt\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := printConfig(dir, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"timeout": "10m0s"`) {
		t.Fatalf("expected timeout=\"10m0s\" in output, got:\n%s", stdout.String())
	}
}

// TestPrintConfig_ExposesMaxRetryBackoffMs verifies the effective-config
// inspection path includes the retry backoff cap that controls orchestrator
// retry timing. Operators rely on --print-config to confirm workflow reloads
// and overrides took effect.
func TestPrintConfig_ExposesMaxRetryBackoffMs(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\nagent:\n  max_retry_backoff_ms: 45000\ntracker:\n  kind: linear\n  project_slug: platform\n---\nprompt\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := printConfig(dir, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	var out struct {
		Config struct {
			Agent struct {
				MaxRetryBackoffMs int `json:"max_retry_backoff_ms"`
			} `json:"agent"`
		} `json:"config"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\nstdout: %s", err, stdout.String())
	}
	if got, want := out.Config.Agent.MaxRetryBackoffMs, 45000; got != want {
		t.Fatalf("agent.max_retry_backoff_ms = %d, want %d\nstdout:\n%s", got, want, stdout.String())
	}
}

func TestPrintConfig_ExposesMaxRetryAttempts(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\nagent:\n  max_retry_attempts: 0\ntracker:\n  kind: linear\n  project_slug: platform\n---\nprompt\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := printConfig(dir, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	var out struct {
		Config struct {
			Agent struct {
				MaxRetryAttempts *int `json:"max_retry_attempts"`
			} `json:"agent"`
		} `json:"config"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\nstdout: %s", err, stdout.String())
	}
	if out.Config.Agent.MaxRetryAttempts == nil || *out.Config.Agent.MaxRetryAttempts != 0 {
		t.Fatalf("agent.max_retry_attempts = %v, want explicit 0\nstdout:\n%s", out.Config.Agent.MaxRetryAttempts, stdout.String())
	}
}

// TestPrintConfig_TopLevelSourceOmitsLegacyShadowedBy pins the #72
// SPEC-aligned contract: print-config still reports the effective source
// at the top level, but legacy alternate paths are ignored rather than
// reported as normal shadow workflow sources.
func TestPrintConfig_TopLevelSourceOmitsLegacyShadowedBy(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: linear\n  project_slug: platform\n---\nprompt\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write root: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".aiops"), 0o755); err != nil {
		t.Fatalf("mkdir .aiops: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aiops", "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write .aiops: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := printConfig(dir, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), `"shadowed_by"`) {
		t.Fatalf("legacy shadowed_by must be omitted:\n%s", stdout.String())
	}
	var out struct {
		Source     string `json:"source"`
		Resolution struct {
			Source string `json:"source"`
			Path   string `json:"path"`
		} `json:"resolution"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\nstdout: %s", err, stdout.String())
	}
	if out.Source != "file" {
		t.Fatalf("top-level source = %q, want %q", out.Source, "file")
	}
	if out.Resolution.Source != "file" {
		t.Fatalf("resolution.source = %q, want %q", out.Resolution.Source, "file")
	}
	if out.Resolution.Path != "WORKFLOW.md" {
		t.Fatalf("resolution.path = %q, want %q", out.Resolution.Path, "WORKFLOW.md")
	}
}

// TestPrintConfig_TopLevelOmitsEmptyShadowedBy keeps the common case
// terse: when nothing is being shadowed, `shadowed_by` is omitted from
// the top level (the JSON tag is `,omitempty`). The clean repo-root
// case is what most users see.
func TestPrintConfig_TopLevelOmitsEmptyShadowedBy(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := printConfig(dir, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), `"shadowed_by"`) {
		t.Fatalf("shadowed_by must be omitted when empty:\n%s", stdout.String())
	}
	var out struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\nstdout: %s", err, stdout.String())
	}
	if out.Source != "default" {
		t.Fatalf("top-level source = %q, want %q", out.Source, "default")
	}
}

// TestPrintConfig_MasksRepoCloneURLUserinfo pins the secret-masking
// contract for `repo.clone_url`: when the URL embeds basic-auth
// userinfo (the conventional way GitHub-style tokens are passed via
// HTTPS clones), the userinfo segment is stripped before serialization
// so `--print-config` paste-into-chat does not leak the token. Non-auth
// URL forms (plain HTTPS, SSH-style git URLs) must round-trip
// unchanged.
//
// Boundary-coverage rule: paired edges on the userinfo bracket — both
// halves (user+password), opening-only (user, no password), neither
// (plain), and a structurally distinct form (SSH) all in one test so a
// future regression on any branch is loud.
func TestPrintConfig_MasksRepoCloneURLUserinfo(t *testing.T) {
	cases := []struct {
		name     string
		clone    string
		wantOut  string
		mustHide []string
	}{
		{
			name:     "user_and_token_password",
			clone:    "https://oauth2:ghp_super_secret_token@github.com/o/r.git",
			wantOut:  "https://github.com/o/r.git",
			mustHide: []string{"ghp_super_secret_token", "oauth2:ghp_super_secret_token"},
		},
		{
			name:     "user_only_treated_as_token_alias",
			clone:    "https://ghp_super_secret_token@github.com/o/r.git",
			wantOut:  "https://github.com/o/r.git",
			mustHide: []string{"ghp_super_secret_token"},
		},
		{
			name:    "plain_https_round_trips",
			clone:   "https://github.com/o/r.git",
			wantOut: "https://github.com/o/r.git",
		},
		{
			name:    "ssh_git_url_round_trips",
			clone:   "git@example.com:o/r.git",
			wantOut: "git@example.com:o/r.git",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: " + tc.clone + "\ntracker:\n  kind: linear\n  project_slug: platform\n---\nprompt\n"
			if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			var stdout, stderr bytes.Buffer
			if code := printConfig(dir, nil, &stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
			}
			got := stdout.String()
			for _, secret := range tc.mustHide {
				if strings.Contains(got, secret) {
					t.Fatalf("secret %q leaked into stdout:\n%s", secret, got)
				}
			}
			wantLine := `"clone_url": "` + tc.wantOut + `"`
			if !strings.Contains(got, wantLine) {
				t.Fatalf("clone_url not rendered as %q in stdout:\n%s", tc.wantOut, got)
			}
		})
	}
}

// TestPrintConfig_MasksSandboxCredentialFiles pins the secret-masking
// contract for `sandbox.credential_files`: the path itself is sensitive
// (a precise on-disk pointer for any attacker with shell access) and
// each entry must be replaced with the standard `***` placeholder.
//
// Boundary-coverage rule applied to slice length: 0 (must not produce a
// placeholder — empty means empty), 1 (one entry masked), and 2 (every
// element masked, not just the first). The masked entries appear in
// stdout; the original paths must not.
func TestPrintConfig_MasksSandboxCredentialFiles(t *testing.T) {
	cases := []struct {
		name      string
		yamlList  string
		wantCount int
		mustHide  []string
	}{
		{
			name:      "empty_slice_no_placeholder",
			yamlList:  "",
			wantCount: 0,
			mustHide:  nil,
		},
		{
			name:      "single_entry_masked",
			yamlList:  "    - /etc/aiops/creds/lone-token\n",
			wantCount: 1,
			mustHide:  []string{"/etc/aiops/creds/lone-token", "lone-token"},
		},
		{
			name:      "two_entries_each_masked",
			yamlList:  "    - /etc/aiops/creds/alpha\n    - /etc/aiops/creds/bravo\n",
			wantCount: 2,
			mustHide:  []string{"/etc/aiops/creds/alpha", "/etc/aiops/creds/bravo", "alpha", "bravo"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			sandbox := "sandbox:\n"
			if tc.yamlList != "" {
				sandbox += "  credential_files:\n" + tc.yamlList
			} else {
				sandbox = ""
			}
			body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: linear\n  project_slug: platform\n" + sandbox + "---\nprompt\n"
			if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			var stdout, stderr bytes.Buffer
			if code := printConfig(dir, nil, &stdout, &stderr); code != 0 {
				t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
			}
			got := stdout.String()
			for _, secret := range tc.mustHide {
				if strings.Contains(got, secret) {
					t.Fatalf("secret substring %q leaked into stdout:\n%s", secret, got)
				}
			}
			var out struct {
				Config struct {
					Sandbox struct {
						CredentialFiles []string `json:"credential_files"`
					} `json:"sandbox"`
				} `json:"config"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
				t.Fatalf("decode: %v\nstdout: %s", err, stdout.String())
			}
			if got, want := len(out.Config.Sandbox.CredentialFiles), tc.wantCount; got != want {
				t.Fatalf("credential_files length = %d, want %d\nstdout:\n%s", got, want, stdout.String())
			}
			for i, entry := range out.Config.Sandbox.CredentialFiles {
				if entry != "***" {
					t.Fatalf("credential_files[%d] = %q, want %q\nstdout:\n%s", i, entry, "***", stdout.String())
				}
			}
		})
	}
}

// TestPrintConfig_SandboxVisibleInConfigView covers the observability
// half of #194: the `sandbox` block must round-trip through
// --print-config (after masking) so operators can verify the effective
// sandbox posture, not just stare at silence.
func TestPrintConfig_SandboxVisibleInConfigView(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: linear\n  project_slug: platform\nsandbox:\n  enabled: true\n  backend: bubblewrap\n  network: none\n  env_allowlist:\n    - HOME\n    - PATH\n---\nprompt\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := printConfig(dir, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	var out struct {
		Config struct {
			Sandbox struct {
				Enabled      bool     `json:"enabled"`
				Backend      string   `json:"backend"`
				NetworkMode  string   `json:"network"`
				EnvAllowlist []string `json:"env_allowlist"`
			} `json:"sandbox"`
		} `json:"config"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v\nstdout: %s", err, stdout.String())
	}
	if !out.Config.Sandbox.Enabled {
		t.Fatalf("sandbox.enabled = false, want true\nstdout:\n%s", stdout.String())
	}
	if got, want := out.Config.Sandbox.Backend, "bubblewrap"; got != want {
		t.Fatalf("sandbox.backend = %q, want %q", got, want)
	}
	if got, want := out.Config.Sandbox.NetworkMode, "none"; got != want {
		t.Fatalf("sandbox.network = %q, want %q", got, want)
	}
	if got, want := out.Config.Sandbox.EnvAllowlist, []string{"HOME", "PATH"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sandbox.env_allowlist = %#v, want %#v", got, want)
	}
}

// TestPrintConfig_SchemaErrorReturnsExitOne pins the contract that
// schema validation failures produce a non-zero exit and route the
// human-readable error to stderr. Stdout must remain empty so a script
// piping the JSON elsewhere does not feed it a malformed document.
func TestPrintConfig_SchemaErrorReturnsExitOne(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\ntracker:\n  kind: gitea\n---\nprompt\n" // no clone_url
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := printConfig(dir, nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout not empty on error: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "repo.clone_url") {
		t.Fatalf("stderr missing field name: %s", stderr.String())
	}
}

// clearWorkerEnv neutralizes every worker env var the provenance block
// reads, so an ambient AIOPS_WORKSPACE_ROOT in the test runner's
// environment cannot make a "default"/"workflow" assertion flap. ResolveEnv
// treats an empty value as unset, so setting "" is equivalent to unset for
// resolution while still being restored by t.Setenv's cleanup.
func clearWorkerEnv(t *testing.T) {
	t.Helper()
	t.Setenv(workspaceRootEnv, "")
	t.Setenv(workspaceRootEnvLegacy, "")
	t.Setenv(mirrorRootEnv, "")
	t.Setenv(mirrorRootEnvLegacy, "")
}

// runPrintConfigProvenance writes body (when non-empty) as WORKFLOW.md in a
// fresh dir, runs printConfig with portOverride, and returns the decoded
// provenance block. dir is returned so callers can cross-check against an
// independent workflow.Resolve.
func runPrintConfigProvenance(t *testing.T, body string, portOverride *int) (configProvenance, string) {
	t.Helper()
	dir := t.TempDir()
	if body != "" {
		if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := printConfig(dir, portOverride, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	var out struct {
		Provenance configProvenance `json:"provenance"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("decode provenance: %v\nstdout: %s", err, stdout.String())
	}
	return out.Provenance, dir
}

// validFrontMatter wraps the minimal valid front matter (repo + linear
// tracker) around the supplied extra block so loader validation passes.
func validFrontMatter(extra string) string {
	return "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: linear\n  project_slug: platform\n" + extra + "---\nprompt\n"
}

// TestPrintConfig_WorkspaceRootProvenance pins the #375 acceptance
// criterion for workspace root: every SPEC §6.4 precedence layer
// (workflow > env, canonical vs deprecated alias > default) is annotated
// with the correct source, env var name, and deprecation flag.
func TestPrintConfig_WorkspaceRootProvenance(t *testing.T) {
	cases := []struct {
		name           string
		body           string
		envCanonical   string
		envLegacy      string
		wantValue      string
		wantSource     string
		wantEnvVar     string
		wantDeprecated bool
	}{
		{
			// workflow.root wins even though an env var is also set.
			name:         "workflow_wins_over_env",
			body:         validFrontMatter("workspace:\n  root: /custom/ws\n"),
			envCanonical: "/env/should-lose",
			wantValue:    "/custom/ws",
			wantSource:   sourceWorkflow,
		},
		{
			name:         "env_canonical",
			body:         "",
			envCanonical: "/env/ws",
			wantValue:    "/env/ws",
			wantSource:   sourceEnv,
			wantEnvVar:   workspaceRootEnv,
		},
		{
			name:           "env_deprecated_alias",
			body:           "",
			envLegacy:      "/legacy/ws",
			wantValue:      "/legacy/ws",
			wantSource:     sourceEnv,
			wantEnvVar:     workspaceRootEnvLegacy,
			wantDeprecated: true,
		},
		{
			name:       "default_no_file_no_env",
			body:       "",
			wantSource: sourceDefault,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearWorkerEnv(t)
			if tc.envCanonical != "" {
				t.Setenv(workspaceRootEnv, tc.envCanonical)
			}
			if tc.envLegacy != "" {
				t.Setenv(workspaceRootEnvLegacy, tc.envLegacy)
			}
			prov, dir := runPrintConfigProvenance(t, tc.body, nil)
			got := prov.WorkspaceRoot
			if got.Source != tc.wantSource {
				t.Fatalf("workspace_root.source = %q, want %q", got.Source, tc.wantSource)
			}
			if got.EnvVar != tc.wantEnvVar {
				t.Fatalf("workspace_root.env_var = %q, want %q", got.EnvVar, tc.wantEnvVar)
			}
			if got.EnvVarDeprecated != tc.wantDeprecated {
				t.Fatalf("workspace_root.env_var_deprecated = %v, want %v", got.EnvVarDeprecated, tc.wantDeprecated)
			}
			// The reported value must equal what the worker would actually
			// use, so it cannot drift from EffectiveWorkspaceRoot.
			wf, _, err := workflow.Resolve(dir)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			wantEffective := EffectiveWorkspaceRoot(LoadConfigFromEnv(), wf.Config)
			if got.Value != wantEffective {
				t.Fatalf("workspace_root.value = %q, want effective %q", got.Value, wantEffective)
			}
			if tc.wantValue != "" && got.Value != tc.wantValue {
				t.Fatalf("workspace_root.value = %q, want %q", got.Value, tc.wantValue)
			}
			if tc.wantSource == sourceDefault && got.Value == "" {
				t.Fatalf("default workspace_root.value must be the SPEC default, got empty")
			}
		})
	}
}

// TestPrintConfig_MirrorRootProvenance pins the #375 acceptance criterion
// for mirror root. There is no WORKFLOW.md field, so only env (canonical /
// deprecated alias) and the computed default apply.
func TestPrintConfig_MirrorRootProvenance(t *testing.T) {
	cases := []struct {
		name           string
		envCanonical   string
		envLegacy      string
		wantSource     string
		wantEnvVar     string
		wantValue      string
		wantDeprecated bool
	}{
		{
			name:         "env_canonical",
			envCanonical: "/env/mirror",
			wantSource:   sourceEnv,
			wantEnvVar:   mirrorRootEnv,
			wantValue:    "/env/mirror",
		},
		{
			name:           "env_deprecated_alias",
			envLegacy:      "/legacy/mirror",
			wantSource:     sourceEnv,
			wantEnvVar:     mirrorRootEnvLegacy,
			wantValue:      "/legacy/mirror",
			wantDeprecated: true,
		},
		{
			name:       "default_no_env",
			wantSource: sourceDefault,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearWorkerEnv(t)
			if tc.envCanonical != "" {
				t.Setenv(mirrorRootEnv, tc.envCanonical)
			}
			if tc.envLegacy != "" {
				t.Setenv(mirrorRootEnvLegacy, tc.envLegacy)
			}
			prov, _ := runPrintConfigProvenance(t, "", nil)
			got := prov.MirrorRoot
			if got.Source != tc.wantSource {
				t.Fatalf("mirror_root.source = %q, want %q", got.Source, tc.wantSource)
			}
			if got.EnvVar != tc.wantEnvVar {
				t.Fatalf("mirror_root.env_var = %q, want %q", got.EnvVar, tc.wantEnvVar)
			}
			if got.EnvVarDeprecated != tc.wantDeprecated {
				t.Fatalf("mirror_root.env_var_deprecated = %v, want %v", got.EnvVarDeprecated, tc.wantDeprecated)
			}
			if tc.wantValue != "" && got.Value != tc.wantValue {
				t.Fatalf("mirror_root.value = %q, want %q", got.Value, tc.wantValue)
			}
			if tc.wantSource == sourceDefault && got.Value == "" {
				t.Fatalf("default mirror_root.value must be the computed default, got empty")
			}
		})
	}
}

// TestPrintConfig_ServerPortProvenance pins the #375 acceptance criterion
// for server.port, including the CLI --port override precedence over a
// WORKFLOW.md value, and the workflow-vs-default distinction.
func TestPrintConfig_ServerPortProvenance(t *testing.T) {
	port := func(n int) *int { return &n }
	cases := []struct {
		name         string
		body         string
		portOverride *int
		wantValue    string
		wantSource   string
	}{
		{
			// --port wins even when WORKFLOW.md sets server.port.
			name:         "cli_wins_over_workflow",
			body:         validFrontMatter("server:\n  port: 5000\n"),
			portOverride: port(4001),
			wantValue:    "4001",
			wantSource:   sourceCLI,
		},
		{
			name:       "workflow",
			body:       validFrontMatter("server:\n  port: 5000\n"),
			wantValue:  "5000",
			wantSource: sourceWorkflow,
		},
		{
			name:       "default_no_file",
			body:       "",
			wantValue:  "4000",
			wantSource: sourceDefault,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearWorkerEnv(t)
			prov, _ := runPrintConfigProvenance(t, tc.body, tc.portOverride)
			got := prov.ServerPort
			if got.Source != tc.wantSource {
				t.Fatalf("server_port.source = %q, want %q", got.Source, tc.wantSource)
			}
			if got.Value != tc.wantValue {
				t.Fatalf("server_port.value = %q, want %q", got.Value, tc.wantValue)
			}
		})
	}
}

// TestPrintConfig_WorkflowPathProvenance pins the #375 acceptance criterion
// for the workflow path source: a resolved file (front-matter or
// prompt-only) reports `workflow` with the repo-relative path, while no
// file reports `default` with no path.
func TestPrintConfig_WorkflowPathProvenance(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantValue  string
		wantSource string
	}{
		{
			name:       "file_front_matter",
			body:       validFrontMatter(""),
			wantValue:  "WORKFLOW.md",
			wantSource: sourceWorkflow,
		},
		{
			name:       "prompt_only_file",
			body:       "just a prompt body, no front matter\n",
			wantValue:  "WORKFLOW.md",
			wantSource: sourceWorkflow,
		},
		{
			name:       "default_no_file",
			body:       "",
			wantValue:  "",
			wantSource: sourceDefault,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearWorkerEnv(t)
			prov, _ := runPrintConfigProvenance(t, tc.body, nil)
			got := prov.WorkflowPath
			if got.Source != tc.wantSource {
				t.Fatalf("workflow_path.source = %q, want %q", got.Source, tc.wantSource)
			}
			if got.Value != tc.wantValue {
				t.Fatalf("workflow_path.value = %q, want %q", got.Value, tc.wantValue)
			}
		})
	}
}

// TestPrintConfig_ProvenanceDoesNotLeakSecrets keeps the #375 "secret
// masking unchanged" criterion honest: adding the provenance block must
// not echo the tracker API key even though provenance threads the env
// layer into the print path.
func TestPrintConfig_ProvenanceDoesNotLeakSecrets(t *testing.T) {
	clearWorkerEnv(t)
	t.Setenv("AIOPS_TEST_LINEAR_KEY", "lin_super_secret_value")
	t.Setenv(workspaceRootEnv, "/env/ws")
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: linear\n  project_slug: platform\n  api_key: $AIOPS_TEST_LINEAR_KEY\n---\nprompt\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var stdout, stderr bytes.Buffer
	if code := printConfig(dir, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	got := stdout.String()
	if strings.Contains(got, "lin_super_secret_value") {
		t.Fatalf("api_key leaked into stdout:\n%s", got)
	}
	if !strings.Contains(got, `"api_key": "***"`) {
		t.Fatalf("api_key not masked; stdout:\n%s", got)
	}
	// The provenance block still reports the (non-secret) env workspace root.
	if !strings.Contains(got, "/env/ws") {
		t.Fatalf("expected env workspace root in provenance; stdout:\n%s", got)
	}
}
