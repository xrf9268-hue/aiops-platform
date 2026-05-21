package runner

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

func validateAgentCommandWorkdir(in RunInput, cmd *exec.Cmd) error {
	if cmd == nil {
		return fmt.Errorf("invalid_workspace_cwd: command is required")
	}
	return validateAgentWorkdir(in, cmd.Dir)
}

func validateAgentWorkdir(in RunInput, cwd string) error {
	workspacePath := strings.TrimSpace(in.Workdir)
	if workspacePath == "" {
		return fmt.Errorf("invalid_workspace_cwd: workspace_path is required")
	}
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return fmt.Errorf("invalid_workspace_cwd: cwd is required")
	}
	workspaceAbs, err := filepath.Abs(workspacePath)
	if err != nil {
		return fmt.Errorf("invalid_workspace_cwd: resolve workspace_path: %w", err)
	}
	cwdAbs, err := filepath.Abs(cwd)
	if err != nil {
		return fmt.Errorf("invalid_workspace_cwd: resolve cwd: %w", err)
	}
	if cwdAbs != workspaceAbs {
		return fmt.Errorf("invalid_workspace_cwd: cwd %q does not match workspace_path %q", cwdAbs, workspaceAbs)
	}

	root := strings.TrimSpace(in.WorkspaceRoot)
	if root == "" {
		root = strings.TrimSpace(in.Workflow.Config.Workspace.Root)
	}
	if root == "" {
		return fmt.Errorf("invalid_workspace_cwd: workspace root is required")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("invalid_workspace_cwd: resolve workspace root: %w", err)
	}
	// ensurePathWithinRoot canonicalizes symlinks before comparing paths.
	if err := ensurePathWithinRoot(workspaceAbs, rootAbs); err != nil {
		return fmt.Errorf("invalid_workspace_cwd: %w", err)
	}
	return nil
}
