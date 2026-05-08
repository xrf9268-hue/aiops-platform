package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/policy"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// initRepo creates a temp git repo seeded with a single committed file so
// `git diff HEAD` has a valid base.
func initRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"git", "init", "-q", "-b", "main"},
		{"git", "config", "user.email", "test@example.com"},
		{"git", "config", "user.name", "test"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"git", "add", "."},
		{"git", "commit", "-q", "-m", "seed"},
	} {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestDiffstatCountsModifiedAndUntracked(t *testing.T) {
	dir := initRepo(t)
	// modify seed
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\nmore\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// add untracked deny-pathish file
	if err := os.MkdirAll(filepath.Join(dir, "infra"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "infra", "main.tf"), []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := Diffstat(context.Background(), dir)
	if err != nil {
		t.Fatalf("Diffstat error: %v", err)
	}
	if len(d.Files) != 2 {
		t.Fatalf("expected 2 files, got %v", d.Files)
	}
	if d.Lines < 4 { // 1 added line on seed + 3 added lines on infra/main.tf
		t.Fatalf("expected >=4 lines, got %d", d.Lines)
	}
}

func TestEnforcePolicyDenyBlocksAndReturnsStructuredError(t *testing.T) {
	dir := initRepo(t)
	if err := os.MkdirAll(filepath.Join(dir, "infra"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "infra", "main.tf"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := workflow.Config{Policy: workflow.PolicyConfig{
		DenyPaths:       []string{"infra/**"},
		MaxChangedFiles: 100,
		MaxChangedLines: 100,
	}}
	err := EnforcePolicy(context.Background(), dir, cfg)
	if err == nil {
		t.Fatal("expected policy error")
	}
	var pe *PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *PolicyError, got %T: %v", err, err)
	}
	if len(pe.Violations) != 1 || pe.Violations[0].Kind != policy.KindDenyPath {
		t.Fatalf("unexpected violations: %+v", pe.Violations)
	}
}

func TestEnforcePolicyMaxLinesUsesLegacyLOC(t *testing.T) {
	dir := initRepo(t)
	// 10 added lines
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := workflow.Config{Policy: workflow.PolicyConfig{MaxChangedLOC: 3}}
	err := EnforcePolicy(context.Background(), dir, cfg)
	var pe *PolicyError
	if !errors.As(err, &pe) {
		t.Fatalf("expected PolicyError, got %v", err)
	}
	found := false
	for _, v := range pe.Violations {
		if v.Kind == policy.KindMaxChangedLines {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected max_changed_lines violation, got %+v", pe.Violations)
	}
}

func TestEnforcePolicyCleanWorkdir(t *testing.T) {
	dir := initRepo(t)
	cfg := workflow.Config{Policy: workflow.PolicyConfig{
		DenyPaths:       []string{"infra/**"},
		MaxChangedFiles: 5,
		MaxChangedLines: 50,
	}}
	if err := EnforcePolicy(context.Background(), dir, cfg); err != nil {
		t.Fatalf("expected no policy error on clean workdir, got %v", err)
	}
}
