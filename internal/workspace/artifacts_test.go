package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureSensitiveArtifactExcludeFileAppendsMissingPatternsOnce(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	excludePath := filepath.Join(repo, ".git", "info", "exclude")
	if err := os.WriteFile(excludePath, []byte("existing-pattern"), 0o644); err != nil {
		t.Fatalf("write existing exclude: %v", err)
	}

	ctx := context.Background()
	if err := ensureSensitiveArtifactExcludeFile(ctx, repo); err != nil {
		t.Fatalf("ensureSensitiveArtifactExcludeFile first run: %v", err)
	}
	first, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude after first run: %v", err)
	}
	text := string(first)
	if !strings.HasPrefix(text, "existing-pattern\n") {
		t.Fatalf("exclude prefix = %q; want existing pattern followed by newline", text)
	}
	for _, rel := range sensitiveArtifactExcludePatterns {
		if got := strings.Count(text, rel); got != 1 {
			t.Fatalf("strings.Count(exclude, %q) = %d; want 1\n%s", rel, got, text)
		}
	}

	if err := ensureSensitiveArtifactExcludeFile(ctx, repo); err != nil {
		t.Fatalf("ensureSensitiveArtifactExcludeFile second run: %v", err)
	}
	second, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read exclude after second run: %v", err)
	}
	if string(second) != text {
		t.Fatalf("second ensure changed exclude:\nfirst=%q\nsecond=%q", text, string(second))
	}
}
