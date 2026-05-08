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
			// DefaultConfig() sets PR.Draft=true, so unset preserves the default.
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
