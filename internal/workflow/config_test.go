package workflow

import (
	"os"
	"path/filepath"
	"testing"
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
