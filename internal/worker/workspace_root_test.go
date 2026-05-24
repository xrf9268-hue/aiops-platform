package worker_test

import (
	"os"
	"path/filepath"
	"testing"

	worker "github.com/xrf9268-hue/aiops-platform/internal/worker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// TestEffectiveWorkspaceRoot_PrecedencePerSPEC pins SPEC §6.4 precedence
// at the helper level and locks the #319 fix in place:
//
//  1. RootSet (explicit yaml) > env > workflow-default.
//  2. An unset WORKSPACE_ROOT (empty cfg.WorkspaceRoot) falls through to
//     the workflow's default `Workspace.Root` rather than a private
//     env-loader literal.
//
// The "spec default fallback" and "env overrides spec default" cases
// are the ones that regressed before #319: the env loader's
// `/tmp/aiops-workspaces` literal shadowed `DefaultConfig.Workspace.Root`
// (`<system-temp>/symphony_workspaces`) for any WORKFLOW.md that omitted
// `workspace.root`.
func TestEffectiveWorkspaceRoot_PrecedencePerSPEC(t *testing.T) {
	specDefault := filepath.Join(os.TempDir(), "symphony_workspaces")

	t.Run("rootSet yaml wins over env", func(t *testing.T) {
		wcfg := loadWorkflowConfigForTest(t, "workspace:\n  root: /yaml/root\n")
		got := worker.EffectiveWorkspaceRoot(worker.Config{WorkspaceRoot: "/env/root"}, wcfg)
		if got != "/yaml/root" {
			t.Fatalf("EffectiveWorkspaceRoot = %q, want %q", got, "/yaml/root")
		}
	})

	t.Run("rootSet but empty falls back to env", func(t *testing.T) {
		// Operator wrote `workspace.root: ""` (RootSet=true, value empty).
		// The empty value can't be honored, so env takes precedence next.
		wcfg := loadWorkflowConfigForTest(t, "workspace:\n  root: \"\"\n")
		got := worker.EffectiveWorkspaceRoot(worker.Config{WorkspaceRoot: "/env/root"}, wcfg)
		if got != "/env/root" {
			t.Fatalf("EffectiveWorkspaceRoot = %q, want %q", got, "/env/root")
		}
	})

	t.Run("env overrides spec default when rootSet false", func(t *testing.T) {
		wcfg := loadWorkflowConfigForTest(t, "")
		got := worker.EffectiveWorkspaceRoot(worker.Config{WorkspaceRoot: "/env/root"}, wcfg)
		if got != "/env/root" {
			t.Fatalf("EffectiveWorkspaceRoot = %q, want %q", got, "/env/root")
		}
	})

	t.Run("spec default wins when nothing else set", func(t *testing.T) {
		wcfg := loadWorkflowConfigForTest(t, "")
		got := worker.EffectiveWorkspaceRoot(worker.Config{WorkspaceRoot: ""}, wcfg)
		if got != specDefault {
			t.Fatalf("EffectiveWorkspaceRoot = %q, want SPEC §6.4 default %q", got, specDefault)
		}
	})

	t.Run("whitespace-only env falls through to spec default", func(t *testing.T) {
		wcfg := loadWorkflowConfigForTest(t, "")
		got := worker.EffectiveWorkspaceRoot(worker.Config{WorkspaceRoot: "   "}, wcfg)
		if got != specDefault {
			t.Fatalf("EffectiveWorkspaceRoot = %q, want SPEC §6.4 default %q", got, specDefault)
		}
	})
}

// loadWorkflowConfigForTest writes a minimal WORKFLOW.md with the given
// extra front-matter (e.g. a `workspace:` block) and returns the parsed
// Config. Going through the real loader is what makes `RootSet=true`
// observable — the field is unexported and only the loader flips it
// when `workspace.root` is present in the YAML.
func loadWorkflowConfigForTest(t *testing.T, extraFrontMatter string) workflow.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	body := "---\n" + extraFrontMatter + `repo:
  owner: acme
  name: demo
  clone_url: https://example.invalid/acme/demo.git
  default_branch: main
tracker:
  kind: gitea
` + "---\nPrompt body\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	wf, err := workflow.Load(path)
	if err != nil {
		t.Fatalf("Load workflow: %v", err)
	}
	return wf.Config
}

// TestLoadConfigFromEnv_NoLiteralWorkspaceRootDefault pins the
// loader-floor invariant from #319: when WORKSPACE_ROOT is unset
// Config.WorkspaceRoot must stay empty so EffectiveWorkspaceRoot can
// fall through to the workflow's SPEC default. Pre-#319 the loader
// returned the literal `/tmp/aiops-workspaces`.
func TestLoadConfigFromEnv_NoLiteralWorkspaceRootDefault(t *testing.T) {
	t.Setenv("AIOPS_WORKSPACE_ROOT", "")
	t.Setenv("WORKSPACE_ROOT", "")

	cfg := worker.LoadConfigFromEnv()
	if cfg.WorkspaceRoot != "" {
		t.Fatalf("LoadConfigFromEnv().WorkspaceRoot = %q, want \"\" (no literal default; see #319)", cfg.WorkspaceRoot)
	}
}

// TestLoadConfigFromEnv_HonorsWorkspaceRootEnv covers the still-supported
// legacy alias path: an explicit WORKSPACE_ROOT flows through to
// Config.WorkspaceRoot verbatim (now as a deprecated alias; see #368).
func TestLoadConfigFromEnv_HonorsWorkspaceRootEnv(t *testing.T) {
	want := filepath.Join(t.TempDir(), "operator-root")
	t.Setenv("AIOPS_WORKSPACE_ROOT", "")
	t.Setenv("WORKSPACE_ROOT", want)

	cfg := worker.LoadConfigFromEnv()
	if cfg.WorkspaceRoot != want {
		t.Fatalf("LoadConfigFromEnv().WorkspaceRoot = %q, want %q", cfg.WorkspaceRoot, want)
	}
}

// TestLoadConfigFromEnv_HonorsAIOPSWorkspaceRoot is the #368 regression: the
// AIOPS_-prefixed canonical name a user naturally tries is no longer silently
// ignored in favor of the code default.
func TestLoadConfigFromEnv_HonorsAIOPSWorkspaceRoot(t *testing.T) {
	want := filepath.Join(t.TempDir(), "prefixed-root")
	t.Setenv("WORKSPACE_ROOT", "")
	t.Setenv("AIOPS_WORKSPACE_ROOT", want)

	cfg := worker.LoadConfigFromEnv()
	if cfg.WorkspaceRoot != want {
		t.Fatalf("LoadConfigFromEnv().WorkspaceRoot = %q, want %q (AIOPS_WORKSPACE_ROOT must be honored)", cfg.WorkspaceRoot, want)
	}
}

// TestLoadConfigFromEnv_CanonicalWorkspaceRootWinsOverLegacy pins precedence
// when both the canonical and the deprecated alias are set.
func TestLoadConfigFromEnv_CanonicalWorkspaceRootWinsOverLegacy(t *testing.T) {
	canonical := filepath.Join(t.TempDir(), "canonical")
	t.Setenv("AIOPS_WORKSPACE_ROOT", canonical)
	t.Setenv("WORKSPACE_ROOT", filepath.Join(t.TempDir(), "legacy"))

	cfg := worker.LoadConfigFromEnv()
	if cfg.WorkspaceRoot != canonical {
		t.Fatalf("LoadConfigFromEnv().WorkspaceRoot = %q, want canonical %q", cfg.WorkspaceRoot, canonical)
	}
}
