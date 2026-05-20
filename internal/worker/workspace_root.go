package worker

import (
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func EffectiveWorkspaceRoot(cfg Config, wcfg workflow.Config) string {
	root := strings.TrimSpace(wcfg.Workspace.Root)
	if wcfg.Workspace.RootSet() && root != "" {
		return root
	}
	return cfg.WorkspaceRoot
}
