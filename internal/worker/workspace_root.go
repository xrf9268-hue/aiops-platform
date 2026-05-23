package worker

import (
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// EffectiveWorkspaceRoot resolves the worker's runtime workspace root with
// SPEC §6.4 precedence:
//
//  1. WORKFLOW.md `workspace.root` (explicit; RootSet=true) wins outright.
//  2. WORKSPACE_ROOT env (non-empty cfg.WorkspaceRoot) wins over the
//     SPEC default — operators can still steer a host without touching
//     WORKFLOW.md.
//  3. Otherwise fall back to the workflow's `Workspace.Root`, which
//     DefaultConfig seeds with the SPEC §6.4 default
//     (`<system-temp>/symphony_workspaces`).
//
// Pre-#319 the env loader's literal `/tmp/aiops-workspaces` fallback
// silently shadowed the SPEC default whenever a WORKFLOW.md omitted
// `workspace.root`. The env layer no longer carries a default literal
// (see LoadConfigFromEnv), so an unset env var leaves cfg.WorkspaceRoot
// empty and the SPEC default wins by precedence.
func EffectiveWorkspaceRoot(cfg Config, wcfg workflow.Config) string {
	workflowRoot := strings.TrimSpace(wcfg.Workspace.Root)
	if wcfg.Workspace.RootSet() && workflowRoot != "" {
		return workflowRoot
	}
	if envRoot := strings.TrimSpace(cfg.WorkspaceRoot); envRoot != "" {
		return envRoot
	}
	return workflowRoot
}
