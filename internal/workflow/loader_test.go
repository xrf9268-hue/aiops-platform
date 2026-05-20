package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRejectsLinearWorkflowWithoutProjectSlug(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: r
  clone_url: git@example.com:o/r.git
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
---
prompt
`
	t.Setenv("LINEAR_API_KEY", "test-key")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatalf("Load: expected error for missing tracker.project_slug, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"tracker.project_slug", "tracker.kind=linear", path} {
		if !strings.Contains(msg, want) {
			t.Fatalf("Load error %q: want substring %q", msg, want)
		}
	}
}
