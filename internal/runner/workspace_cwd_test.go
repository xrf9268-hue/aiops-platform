package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateAgentCommandWorkdirRejectsDirMismatch(t *testing.T) {
	root := t.TempDir()
	workdir := filepath.Join(root, "issue-185")
	if err := os.Mkdir(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(context.Background(), "sh", "-c", "true")
	cmd.Dir = other

	err := validateAgentCommandWorkdir(RunInput{
		Workdir:       workdir,
		WorkspaceRoot: root,
	}, cmd)
	if err == nil {
		t.Fatal("expected cwd mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid_workspace_cwd") || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("error = %q, want invalid_workspace_cwd cwd mismatch", err)
	}
}

func TestValidateAgentCommandWorkdirRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "issue-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	cmd := exec.CommandContext(context.Background(), "sh", "-c", "true")
	cmd.Dir = link

	err := validateAgentCommandWorkdir(RunInput{
		Workdir:       link,
		WorkspaceRoot: root,
	}, cmd)
	if err == nil {
		t.Fatal("expected symlink escape error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid_workspace_cwd") || !strings.Contains(err.Error(), "outside workspace root") {
		t.Fatalf("error = %q, want invalid_workspace_cwd symlink escape", err)
	}
}
