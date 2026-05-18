package workflow

import (
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
func TestLoad_PRDraftFromFrontMatter(t *testing.T) {
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
pr:
  draft: true
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
func TestDefaultConfig_PRDraftDefaultsFalse(t *testing.T) {
	if got := DefaultConfig().PR.Draft; got != false {
		t.Fatalf("DefaultConfig().PR.Draft: got %v want false (would regress non-draft default)", got)
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
// platform contract advertises: a 30-minute per-task timeout and one
// dedicated retry slot for runner timeouts.
func TestDefaultConfigAgentTimeout(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Agent.Timeout != 30*time.Minute {
		t.Fatalf("default Agent.Timeout: got %v want 30m", cfg.Agent.Timeout)
	}
	if got := cfg.Agent.MaxTimeoutRetriesValue(); got != 1 {
		t.Fatalf("default Agent.MaxTimeoutRetriesValue: got %d want 1", got)
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
	body := "---\nrepo:\n  owner: o\n  name: n\n  clone_url: git@example.com:o/n.git\n  default_branch: main\n---\nhello\n"
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
	if got := wf.Config.Agent.MaxTimeoutRetriesValue(); got != 1 {
		t.Fatalf("expanded Agent.MaxTimeoutRetriesValue: got %d want 1", got)
	}
}

// TestLoadOptionalHonorsExplicitAgentTimeout confirms a user-specified
// agent.timeout / max_timeout_retries override the schema defaults.
func TestLoadOptionalHonorsExplicitAgentTimeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := "---\nrepo:\n  owner: o\n  name: n\n  clone_url: git@example.com:o/n.git\nagent:\n  timeout: 5m\n  max_timeout_retries: 3\n---\nhello\n"
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

// TestLoad_RejectsUnsupportedAgentDefault matches the runner registry in
// internal/runner: only mock/codex/claude are wired up. Catching a typo
// like `agent.default: codexx` at Load time prevents the worker from
// claiming a task and then dying with "unknown runner" partway through.
func TestLoad_RejectsUnsupportedAgentDefault(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
agent:
  default: codexx
---
prompt
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatalf("Load: expected error for unsupported agent.default, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"agent.default", "codexx", p} {
		if !strings.Contains(msg, want) {
			t.Fatalf("Load error %q: want substring %q", msg, want)
		}
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
	body := "---\nrepo:\n  owner: o\n  name: n\n  clone_url: git@example.com:o/n.git\nagent:\n  max_timeout_retries: 0\n---\nhello\n"
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

func TestLoad_VerifyTimeoutAndAllowFailureRoundTrip(t *testing.T) {
	dir := t.TempDir()
	body := "---\n" +
		"repo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\n" +
		"verify:\n  timeout: 5m\n  allow_failure: true\n  commands:\n    - go test ./...\n" +
		"---\nprompt\n"
	p := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	wf, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := wf.Config.Verify.Timeout, 5*time.Minute; got != want {
		t.Fatalf("Verify.Timeout = %v, want %v", got, want)
	}
	if !wf.Config.Verify.AllowFailure {
		t.Fatalf("Verify.AllowFailure = false, want true")
	}
}

func TestDefaultConfig_CodexProfileIsSafe(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	if cfg.Codex.Profile != "safe" {
		t.Fatalf("DefaultConfig().Codex.Profile = %q, want %q", cfg.Codex.Profile, "safe")
	}
}

func TestLoad_AcceptsSupportedCodexProfiles(t *testing.T) {
	t.Parallel()
	for _, profile := range []string{"safe", "bypass", "custom"} {
		profile := profile
		t.Run(profile, func(t *testing.T) {
			path := writeTempWorkflow(t, `---
repo:
  clone_url: file:///tmp/repo
tracker:
  kind: linear
agent:
  default: codex
codex:
  command: codex exec
  profile: `+profile+`
---
prompt body
`)
			wf, err := Load(path)
			if err != nil {
				t.Fatalf("Load(%s): %v", profile, err)
			}
			if wf.Config.Codex.Profile != profile {
				t.Fatalf("Codex.Profile = %q, want %q", wf.Config.Codex.Profile, profile)
			}
		})
	}
}

func TestLoad_RejectsUnknownCodexProfile(t *testing.T) {
	t.Parallel()
	path := writeTempWorkflow(t, `---
repo:
  clone_url: file:///tmp/repo
tracker:
  kind: linear
agent:
  default: codex
codex:
  command: codex exec
  profile: yolo
---
prompt
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load: expected error for codex.profile=yolo, got nil")
	}
	if !strings.Contains(err.Error(), "codex.profile") || !strings.Contains(err.Error(), "yolo") {
		t.Fatalf("error = %q; want it to mention codex.profile and yolo", err)
	}
}

func TestLoad_NormalizesEmptyCodexProfileToSafe(t *testing.T) {
	t.Parallel()
	path := writeTempWorkflow(t, `---
repo:
  clone_url: file:///tmp/repo
tracker:
  kind: linear
agent:
  default: codex
codex:
  command: codex exec
---
prompt
`)
	wf, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if wf.Config.Codex.Profile != "safe" {
		t.Fatalf("Codex.Profile = %q, want %q (normalization)", wf.Config.Codex.Profile, "safe")
	}
}

func TestLoad_RejectsClaudeProfile(t *testing.T) {
	t.Parallel()
	path := writeTempWorkflow(t, `---
repo:
  clone_url: file:///tmp/repo
tracker:
  kind: linear
agent:
  default: claude
claude:
  command: claude
  profile: safe
---
prompt
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load: expected error for claude.profile, got nil")
	}
	if !strings.Contains(err.Error(), "claude.profile") {
		t.Fatalf("error = %q; want it to mention claude.profile", err)
	}
}

func TestLoad_PreservesSafetyPolicyForOperatorInspection(t *testing.T) {
	t.Parallel()
	path := writeTempWorkflow(t, `---
repo:
  clone_url: file:///tmp/repo
tracker:
  kind: linear
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
agent:
  default: codex-app-server
codex:
  command: codex app-server
  approval_policy: never
  thread_sandbox: workspace-write
  turn_sandbox_policy:
    mode: workspace-write
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
	if got := wf.Config.Codex.TurnSandboxPolicy["mode"]; got != "workspace-write" {
		t.Fatalf("Codex.TurnSandboxPolicy[mode] = %#v, want workspace-write", got)
	}
	if wf.Config.Codex.TurnTimeoutMs != 120000 || wf.Config.Codex.ReadTimeoutMs != 250 || wf.Config.Codex.StallTimeoutMs != 0 {
		t.Fatalf("Codex timeouts = turn %d read %d stall %d, want 120000/250/0", wf.Config.Codex.TurnTimeoutMs, wf.Config.Codex.ReadTimeoutMs, wf.Config.Codex.StallTimeoutMs)
	}
}

func TestDefaultConfig_CodexAppServerDefaultsPreserveOneShotCommand(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	if cfg.Codex.Command != "codex exec" {
		t.Fatalf("Default Codex.Command = %q, want legacy codex exec", cfg.Codex.Command)
	}
	if cfg.Agent.MaxTurns != 20 {
		t.Fatalf("Default Agent.MaxTurns = %d, want Symphony default 20", cfg.Agent.MaxTurns)
	}
	if cfg.Codex.ThreadSandbox != "workspace-write" {
		t.Fatalf("Default Codex.ThreadSandbox = %q, want workspace-write", cfg.Codex.ThreadSandbox)
	}
	if cfg.Codex.TurnTimeoutMs != 3600000 || cfg.Codex.ReadTimeoutMs != 5000 || cfg.Codex.StallTimeoutMs != 300000 {
		t.Fatalf("Codex timeout defaults = turn %d read %d stall %d", cfg.Codex.TurnTimeoutMs, cfg.Codex.ReadTimeoutMs, cfg.Codex.StallTimeoutMs)
	}
}

func TestLoad_RejectsInvalidCodexAppServerTimeouts(t *testing.T) {
	t.Parallel()
	path := writeTempWorkflow(t, `---
repo:
  clone_url: file:///tmp/repo
tracker:
  kind: linear
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
