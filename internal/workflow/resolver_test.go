package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolve_NoRootWorkflowIgnoresLegacyFallbacks pins the #72/SPEC
// single-source contract: when the canonical WORKFLOW.md is absent,
// Resolve must succeed with Source=default even if legacy fallback files
// exist under .aiops/ or .github/.
func TestResolve_NoRootWorkflowIgnoresLegacyFallbacks(t *testing.T) {
	dir := t.TempDir()
	for _, rel := range []string{".aiops/WORKFLOW.md", ".github/WORKFLOW.md"} {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		body := "---\nrepo:\n  owner: legacy\n  name: legacy\n  clone_url: git@example.com:legacy/repo.git\n---\nlegacy prompt\n"
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

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
	if wf.Config.Repo.CloneURL != "" {
		t.Fatalf("CloneURL = %q, want empty default; legacy fallback must not load", wf.Config.Repo.CloneURL)
	}
}

// TestResolve_FindsRootWorkflowFile covers the most common case: a
// WORKFLOW.md at the repo root with valid YAML front matter resolves
// as Source=file with the relative path "WORKFLOW.md".
func TestResolve_FindsRootWorkflowFile(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: gitea\n---\nprompt body\n"
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

// TestResolve_DoesNotReportShadowedLegacyPaths covers the SPEC-aligned
// single-source contract: legacy alternate paths may exist in a repo, but
// they are not searched and workflow_resolved metadata must not normalize
// them as shadowed candidates.
func TestResolve_DoesNotReportShadowedLegacyPaths(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: gitea\n---\nprompt\n"
	for _, rel := range []string{"WORKFLOW.md", ".aiops/WORKFLOW.md", ".github/WORKFLOW.md"} {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	_, res, err := Resolve(dir)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Path != "WORKFLOW.md" {
		t.Fatalf("Path = %q, want root", res.Path)
	}
	if len(res.ShadowedBy) != 0 {
		t.Fatalf("ShadowedBy = %v, want empty; legacy paths are ignored", res.ShadowedBy)
	}
}

// TestResolve_PropagatesSchemaErrors guards the contract that Resolve
// does NOT silently fall back to defaults when a file is found but
// fails schema validation. Falling back would mask real configuration
// mistakes and reduce the entire validation effort from #48/#49 to
// theatre. The error must name the offending field and the file path.
func TestResolve_PropagatesSchemaErrors(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\ntracker:\n  kind: gitea\n---\nprompt\n" // no clone_url
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err := Resolve(dir)
	if err == nil {
		t.Fatalf("Resolve: expected error for missing clone_url, got nil")
	}
	for _, want := range []string{"repo.clone_url", "WORKFLOW.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q: want substring %q", err.Error(), want)
		}
	}
}

// TestResolve_NonExistentWorkdirErrors guards against the silent
// success that Codex review flagged on PR #52: previously a typo'd
// path returned SourceDefault, hiding the operator's mistake. Resolve
// now refuses to proceed when workdir does not exist.
func TestResolve_NonExistentWorkdirErrors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "definitely-does-not-exist")
	_, _, err := Resolve(dir)
	if err == nil {
		t.Fatalf("Resolve: expected error for non-existent workdir")
	}
	if !strings.Contains(err.Error(), "definitely-does-not-exist") {
		t.Fatalf("error %q should name the missing path", err.Error())
	}
}

// TestResolve_FileWorkdirErrors guards the second precondition: the
// workdir must be a directory, not a regular file. Without this check
// Resolve would fall through to "no candidates found" and return
// defaults, which is the same misleading silent success.
func TestResolve_FileWorkdirErrors(t *testing.T) {
	dir := t.TempDir()
	notDir := filepath.Join(dir, "i-am-a-file")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, _, err := Resolve(notDir)
	if err == nil {
		t.Fatalf("Resolve: expected error when workdir is a file")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("error %q should mention 'not a directory'", err.Error())
	}
}
