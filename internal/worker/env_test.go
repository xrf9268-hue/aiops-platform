package worker_test

import (
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/worker"
)

func TestResolveEnv_CanonicalWins(t *testing.T) {
	t.Setenv("AIOPS_WORKSPACE_ROOT", "/canonical")
	t.Setenv("WORKSPACE_ROOT", "/legacy")
	res := worker.ResolveEnv("AIOPS_WORKSPACE_ROOT", "WORKSPACE_ROOT")
	if res.Value != "/canonical" || res.UsedName != "AIOPS_WORKSPACE_ROOT" {
		t.Fatalf("ResolveEnv = %+v, want canonical value", res)
	}
	if w := res.Warning(); w != "" {
		t.Fatalf("Warning() = %q, want empty when canonical is used", w)
	}
}

func TestResolveEnv_AliasHonoredWithWarning(t *testing.T) {
	t.Setenv("AIOPS_WORKSPACE_ROOT", "")
	t.Setenv("WORKSPACE_ROOT", "/legacy")
	res := worker.ResolveEnv("AIOPS_WORKSPACE_ROOT", "WORKSPACE_ROOT")
	if res.Value != "/legacy" || res.UsedName != "WORKSPACE_ROOT" {
		t.Fatalf("ResolveEnv = %+v, want legacy alias value (not a silent default fallback)", res)
	}
	w := res.Warning()
	if !strings.Contains(w, "WORKSPACE_ROOT") || !strings.Contains(w, "AIOPS_WORKSPACE_ROOT") {
		t.Fatalf("Warning() = %q, want a deprecation notice naming both the alias and canonical", w)
	}
}

func TestResolveEnv_UnsetYieldsEmptyNoWarning(t *testing.T) {
	t.Setenv("AIOPS_WORKSPACE_ROOT", "")
	t.Setenv("WORKSPACE_ROOT", "")
	res := worker.ResolveEnv("AIOPS_WORKSPACE_ROOT", "WORKSPACE_ROOT")
	if res.Value != "" || res.UsedName != "" {
		t.Fatalf("ResolveEnv = %+v, want empty resolution", res)
	}
	if w := res.Warning(); w != "" {
		t.Fatalf("Warning() = %q, want empty when nothing is set", w)
	}
}
