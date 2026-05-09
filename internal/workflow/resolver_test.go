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

// TestResolve_LowerPriorityStatErrorIgnored guards against a regression
// where a permission error on .aiops/WORKFLOW.md (or any non-winning
// candidate) caused Resolve to fail even though the root WORKFLOW.md
// had already been picked. We can't portably create a "permission
// denied" error on macOS without changing process privileges, so we
// instead trigger an EACCES-equivalent by making the parent directory
// unreadable and confirming Resolve still succeeds via the root file.
//
// The platform-portable angle: turn the candidate path into something
// os.Stat can't classify cleanly. The simplest cross-platform option is
// to put a directory at the candidate path; os.Stat then returns
// info.IsDir()=true, the loop's success branch rejects it, and the
// error branch never fires — so this isn't really the bug we want to
// pin. Instead skip the symlink dance and write a test that depends on
// chmod, which is reliable on Linux/macOS and irrelevant on Windows
// (the CI matrix is Unix-only).
func TestResolve_LowerPriorityStatErrorIgnored(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: chmod-based permission test does not apply")
	}
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\n---\nprompt\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write root: %v", err)
	}
	aiopsDir := filepath.Join(dir, ".aiops")
	if err := os.Mkdir(aiopsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(aiopsDir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write aiops: %v", err)
	}
	// Strip read+execute on the .aiops directory so os.Stat on the file
	// inside it returns a permission error instead of NotExist.
	if err := os.Chmod(aiopsDir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(aiopsDir, 0o755) })

	_, res, err := Resolve(dir)
	if err != nil {
		t.Fatalf("Resolve must not fail when winner already found; got: %v", err)
	}
	if res.Source != SourceFile || res.Path != "WORKFLOW.md" {
		t.Fatalf("got Source=%q Path=%q; want file/WORKFLOW.md", res.Source, res.Path)
	}
}

// TestResolve_PromptOnlyFile pins the spec contract that a WORKFLOW.md
// without a YAML front matter block resolves as Source=prompt_only:
// the body becomes the prompt template, but config falls through to
// schema defaults. This is consistent with TestLoad_AcceptsPromptOnlyFile.
func TestResolve_PromptOnlyFile(t *testing.T) {
	dir := t.TempDir()
	body := "just a prompt template, no front matter\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	wf, res, err := Resolve(dir)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Source != SourcePromptOnly {
		t.Fatalf("Source = %q, want %q", res.Source, SourcePromptOnly)
	}
	if res.Path != "WORKFLOW.md" {
		t.Fatalf("Path = %q, want %q", res.Path, "WORKFLOW.md")
	}
	if wf.PromptTemplate != "just a prompt template, no front matter" {
		t.Fatalf("PromptTemplate = %q", wf.PromptTemplate)
	}
}

// TestResolve_AlternateLocations covers the .aiops/ and .github/
// fallback locations declared in the discovery contract. Each case
// puts WORKFLOW.md in exactly one place; precedence (when multiple
// exist) is covered by TestResolve_ShadowedBy.
func TestResolve_AlternateLocations(t *testing.T) {
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\n---\nprompt\n"
	cases := []struct {
		name string
		rel  string
	}{
		{"aiops_dir", ".aiops/WORKFLOW.md"},
		{"github_dir", ".github/WORKFLOW.md"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			abs := filepath.Join(dir, tc.rel)
			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, res, err := Resolve(dir)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if res.Source != SourceFile {
				t.Fatalf("Source = %q, want %q", res.Source, SourceFile)
			}
			if res.Path != tc.rel {
				t.Fatalf("Path = %q, want %q", res.Path, tc.rel)
			}
		})
	}
}
