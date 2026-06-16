package worker_test

import (
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/worker"
)

func TestResolveEnv_CanonicalWins(t *testing.T) {
	t.Setenv("AIOPS_WORKSPACE_ROOT", "/canonical")
	t.Setenv("WORKSPACE_ROOT", "/legacy")
	res := worker.ResolveEnv("AIOPS_WORKSPACE_ROOT")
	if res.Value != "/canonical" || res.UsedName != "AIOPS_WORKSPACE_ROOT" {
		t.Fatalf("ResolveEnv = %+v, want canonical value", res)
	}
}

func TestResolveEnv_IgnoresUnprefixedName(t *testing.T) {
	t.Setenv("AIOPS_WORKSPACE_ROOT", "")
	t.Setenv("WORKSPACE_ROOT", "/legacy")
	res := worker.ResolveEnv("AIOPS_WORKSPACE_ROOT")
	if res.Value != "" || res.UsedName != "" {
		t.Fatalf("ResolveEnv = %+v, want unprefixed name ignored", res)
	}
}

func TestResolveEnv_UnsetYieldsEmptyNoWarning(t *testing.T) {
	t.Setenv("AIOPS_WORKSPACE_ROOT", "")
	t.Setenv("WORKSPACE_ROOT", "")
	res := worker.ResolveEnv("AIOPS_WORKSPACE_ROOT")
	if res.Value != "" || res.UsedName != "" {
		t.Fatalf("ResolveEnv = %+v, want empty resolution", res)
	}
}
