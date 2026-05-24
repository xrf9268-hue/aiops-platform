package worker_test

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/worker"
)

// captureLog redirects the stdlib logger for the duration of fn and returns
// everything written.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	fn()
	return buf.String()
}

// TestWarnDeprecatedEnv_EmitsOncePerAlias guards that the single warning site
// logs exactly one structured deprecation line per legacy alias in use (#368
// PR review: the warning must not be duplicated across repeated loader calls).
func TestWarnDeprecatedEnv_EmitsOncePerAlias(t *testing.T) {
	t.Setenv("AIOPS_WORKSPACE_ROOT", "")
	t.Setenv("WORKSPACE_ROOT", "/legacy")
	t.Setenv("AIOPS_MIRROR_ROOT", "")
	t.Setenv("MIRROR_ROOT", "")
	t.Setenv("AIOPS_WORKFLOW_PATH", "")
	t.Setenv("WORKFLOW_PATH", "")

	out := captureLog(t, worker.WarnDeprecatedEnv)
	if got := strings.Count(out, "used=WORKSPACE_ROOT"); got != 1 {
		t.Fatalf("WORKSPACE_ROOT deprecation logged %d times, want 1\n%s", got, out)
	}
	if !strings.Contains(out, "event=config_env_deprecated_alias") {
		t.Fatalf("warning is not structured (missing event=): %s", out)
	}
}

// TestLoadConfigFromEnv_DoesNotLog locks in loader purity: resolving config
// must not emit warnings, so calling it multiple times during startup cannot
// duplicate the deprecation notice (the single site is WarnDeprecatedEnv).
func TestLoadConfigFromEnv_DoesNotLog(t *testing.T) {
	t.Setenv("AIOPS_WORKSPACE_ROOT", "")
	t.Setenv("WORKSPACE_ROOT", "/legacy")

	out := captureLog(t, func() {
		worker.LoadConfigFromEnv()
		worker.LoadConfigFromEnv()
	})
	if out != "" {
		t.Fatalf("LoadConfigFromEnv logged %q, want no output (warnings belong to WarnDeprecatedEnv)", out)
	}
}

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
