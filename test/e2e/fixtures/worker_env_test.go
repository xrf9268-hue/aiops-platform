package fixtures_test

import "testing"

// TestWorkerComposeUsesCanonicalWorkspaceRootEnv guards that the shipped
// Compose worker uses the canonical AIOPS_WORKSPACE_ROOT name (#368), so the
// default deployment never trips the deprecated-alias warning.
func TestWorkerComposeUsesCanonicalWorkspaceRootEnv(t *testing.T) {
	worker := service(t, readCompose(t), "worker")
	env, ok := worker["environment"].(map[string]any)
	if !ok {
		t.Fatalf("worker.environment = %#v, want map", worker["environment"])
	}
	if _, ok := env["WORKSPACE_ROOT"]; ok {
		t.Error("worker uses deprecated WORKSPACE_ROOT; rename to AIOPS_WORKSPACE_ROOT")
	}
	if _, ok := env["AIOPS_WORKSPACE_ROOT"]; !ok {
		t.Error("worker is missing AIOPS_WORKSPACE_ROOT")
	}
}
