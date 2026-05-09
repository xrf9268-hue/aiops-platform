package workflow

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolve_NoFileReturnsDefaults pins the contract from spec section
// "Discovery Contract": when no WORKFLOW.md exists in any of the search
// locations, Resolve must succeed with Source=default and the schema
// defaults applied (so a fresh repo can run with the mock runner).
func TestResolve_NoFileReturnsDefaults(t *testing.T) {
	dir := t.TempDir()
	wf, res, err := Resolve(dir)
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if res.Source != SourceDefault {
		t.Fatalf("Source = %q, want %q", res.Source, SourceDefault)
	}
	if res.Path != "" {
		t.Fatalf("Path = %q, want empty (no file)", res.Path)
	}
	if len(res.ShadowedBy) != 0 {
		t.Fatalf("ShadowedBy = %v, want empty", res.ShadowedBy)
	}
	if wf == nil || wf.Config.Agent.Default != "mock" {
		t.Fatalf("default Agent.Default = %q, want %q", wf.Config.Agent.Default, "mock")
	}
}

// TestResolve_FindsRootWorkflowFile covers the most common case: a
// WORKFLOW.md at the repo root with valid YAML front matter resolves
// as Source=file with the relative path "WORKFLOW.md".
func TestResolve_FindsRootWorkflowFile(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\n---\nprompt body\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	wf, res, err := Resolve(dir)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Source != SourceFile {
		t.Fatalf("Source = %q, want %q", res.Source, SourceFile)
	}
	if res.Path != "WORKFLOW.md" {
		t.Fatalf("Path = %q, want %q", res.Path, "WORKFLOW.md")
	}
	if wf.Config.Repo.CloneURL != "git@example.com:o/r.git" {
		t.Fatalf("CloneURL = %q, not loaded from front matter", wf.Config.Repo.CloneURL)
	}
}
