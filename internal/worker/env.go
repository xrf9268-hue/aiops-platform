package worker

import "os"

// Worker env var names. The AIOPS_ prefix is the single convention (#368).
const (
	workspaceRootEnv = "AIOPS_WORKSPACE_ROOT"
	mirrorRootEnv    = "AIOPS_MIRROR_ROOT"
	workflowPathEnv  = "AIOPS_WORKFLOW_PATH"
)

// WorkflowPathEnv resolves the workflow-path env var. It is exported because
// cmd/worker resolves this one outside LoadConfigFromEnv.
func WorkflowPathEnv() EnvResolution {
	return ResolveEnv(workflowPathEnv)
}

// EnvResolution records which canonical environment variable supplied a worker
// configuration value.
//
// Worker-owned operational env vars use the AIOPS_ prefix as the single
// convention (AIOPS_WORKSPACE_ROOT / AIOPS_MIRROR_ROOT / AIOPS_WORKFLOW_PATH).
type EnvResolution struct {
	// Canonical is the preferred (AIOPS_-prefixed) variable name.
	Canonical string
	// Value is the resolved value; empty when none of the names is set.
	Value string
	// UsedName is the variable actually read; empty when nothing was set.
	UsedName string
}

// ResolveEnv reads one canonical env var. An unset or empty variable is skipped
// so an explicitly blank value falls through to the code/workflow default.
func ResolveEnv(canonical string) EnvResolution {
	if v := os.Getenv(canonical); v != "" {
		return EnvResolution{Canonical: canonical, Value: v, UsedName: canonical}
	}
	return EnvResolution{Canonical: canonical}
}
