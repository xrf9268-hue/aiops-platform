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
	body := "---\nrepo:\n  owner: o\n  name: n\n  default_branch: main\n---\nhello\n"
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
	body := "---\nagent:\n  timeout: 5m\n  max_timeout_retries: 3\n---\nhello\n"
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

// TestLoadOptionalHonorsExplicitZeroMaxTimeoutRetries verifies that an
// operator who deliberately sets max_timeout_retries: 0 in YAML can
// disable the runner-timeout retry budget entirely. Previously the
// loader coerced 0 back to 1, silently undoing this override.
func TestLoadOptionalHonorsExplicitZeroMaxTimeoutRetries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := "---\nagent:\n  max_timeout_retries: 0\n---\nhello\n"
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
