package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
