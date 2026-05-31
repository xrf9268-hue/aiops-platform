package runner

import (
	"os"
	"path/filepath"
	"testing"
)

// useAgentLoginPATH overrides the package-level agentLoginPATH hook so a test
// can pin the PATH the runner injects for the agent subprocess, restoring the
// original on cleanup.
func useAgentLoginPATH(t *testing.T, path string) {
	t.Helper()
	old := agentLoginPATH
	agentLoginPATH = func() string { return path }
	t.Cleanup(func() {
		agentLoginPATH = old
	})
}

// codexWorkdir creates a per-test workdir with a populated .aiops/PROMPT.md.
func codexWorkdir(t *testing.T, prompt string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".aiops"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aiops", "PROMPT.md"), []byte(prompt), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// assertSensitiveArtifactPerm asserts a runner-written artifact carries the
// 0o600 mode the workspace package enforces for sensitive files.
func assertSensitiveArtifactPerm(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s mode = %#o, want %#o", path, got, os.FileMode(0o600))
	}
}
