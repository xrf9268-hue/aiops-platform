package runner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestSandboxEnvCarriesWorkerInjectedGoCaches(t *testing.T) {
	// The aiops sandbox allowlist (PATH only here) must not strip the
	// worker-injected GOCACHE/GOMODCACHE — else the agent's first `go test`
	// re-breaks under the optional bubblewrap/firejail wrapper (#548 review).
	t.Setenv("TMPDIR", "/sandbox-tmp")
	gocache := "GOCACHE=" + filepath.Join(aiopsGoCacheRoot(), "build", "k")
	gomod := "GOMODCACHE=" + filepath.Join(aiopsGoCacheRoot(), "mod")
	body := strings.Join(sandboxEnv([]string{"PATH=/login", gocache, gomod}, []string{"PATH"}, workflow.Config{}), "\n")
	for _, want := range []string{"PATH=/login", gocache, gomod} {
		if !strings.Contains(body, want) {
			t.Errorf("sandboxEnv dropped %q from the sandbox env; got:\n%s", want, body)
		}
	}
}

func TestSandboxEnvDoesNotCarryOperatorOrHostGoCaches(t *testing.T) {
	t.Setenv("TMPDIR", "/sandbox-tmp")
	// A non-codex runner injects nothing -> nothing fabricated from the host env.
	body := strings.Join(sandboxEnv([]string{"PATH=/login"}, []string{"PATH"}, workflow.Config{}), "\n")
	if strings.Contains(body, "GOCACHE") {
		t.Errorf("sandboxEnv fabricated GOCACHE when not worker-injected; got:\n%s", body)
	}
	// An operator's own GOCACHE (NOT under aiopsGoCacheRoot) that they kept out
	// of env_allowlist must NOT be re-exported by the carry-through (#548).
	body = strings.Join(sandboxEnv([]string{"PATH=/login", "GOCACHE=/operator/cache"}, []string{"PATH"}, workflow.Config{}), "\n")
	if strings.Contains(body, "GOCACHE=/operator/cache") {
		t.Errorf("sandboxEnv carried an operator GOCACHE past the allowlist boundary; got:\n%s", body)
	}
}

func TestWithSandboxGoToolchainCachesDefaults(t *testing.T) {
	t.Setenv("TMPDIR", "/sandbox-tmp")
	got := withSandboxGoToolchainCaches([]string{"PATH=/login"}, "/ws/issue-1")
	base := filepath.Join("/sandbox-tmp", "aiops-go-cache")
	wantCache := "GOCACHE=" + filepath.Join(base, "build", workspaceCacheKey("/ws/issue-1"))
	wantMod := "GOMODCACHE=" + filepath.Join(base, "mod") // shared, workspace-independent
	body := strings.Join(got, "\n")
	for _, want := range []string{wantCache, wantMod} {
		if !strings.Contains(body, want) {
			t.Errorf("withSandboxGoToolchainCaches() missing %q; got:\n%s", want, body)
		}
	}
}

func TestWithSandboxGoToolchainCachesIsolatesGocacheButSharesGomodcache(t *testing.T) {
	t.Setenv("TMPDIR", "/sandbox-tmp")
	a := goCacheEnv(t, withSandboxGoToolchainCaches(nil, "/ws/repo-a/issue-1"))
	b := goCacheEnv(t, withSandboxGoToolchainCaches(nil, "/ws/repo-b/issue-1"))

	// GOCACHE (build cache, unverified) is isolated across repos/issues...
	if a["GOCACHE"] == b["GOCACHE"] {
		t.Errorf("different workspaces shared a GOCACHE dir: %q", a["GOCACHE"])
	}
	// ...but GOMODCACHE (checksum-verified) is shared to avoid re-downloads.
	if a["GOMODCACHE"] != b["GOMODCACHE"] {
		t.Errorf("GOMODCACHE not shared across workspaces: %q vs %q", a["GOMODCACHE"], b["GOMODCACHE"])
	}
	// An issue's retry (same workspace) reuses its GOCACHE.
	again := goCacheEnv(t, withSandboxGoToolchainCaches(nil, "/ws/repo-a/issue-1"))
	if again["GOCACHE"] != a["GOCACHE"] {
		t.Errorf("same workspace produced a different GOCACHE: %q vs %q", a["GOCACHE"], again["GOCACHE"])
	}
}

func TestWithSandboxGoToolchainCachesRespectsOperatorOverride(t *testing.T) {
	t.Setenv("TMPDIR", "/sandbox-tmp")
	got := goCacheEnv(t, withSandboxGoToolchainCaches([]string{"GOCACHE=/operator/cache"}, "/ws/issue-1"))
	if got["GOCACHE"] != "/operator/cache" {
		t.Errorf("operator GOCACHE override = %q; want /operator/cache to win", got["GOCACHE"])
	}
	if got["GOMODCACHE"] == "" {
		t.Errorf("GOMODCACHE default not applied alongside an operator GOCACHE override")
	}
}

func TestWithSandboxGoToolchainCachesTreatsEmptyValueAsAbsent(t *testing.T) {
	t.Setenv("TMPDIR", "/sandbox-tmp")
	// A value-less "GOCACHE=" (e.g. a passthrough of an unset host var) must NOT
	// suppress the default — Go treats empty GOCACHE as unset and reverts to the
	// sandbox-invisible $HOME cache, reviving the bug (codex review #548 P2).
	got := goCacheEnv(t, withSandboxGoToolchainCaches([]string{"GOCACHE="}, "/ws/issue-1"))
	if got["GOCACHE"] == "" {
		t.Errorf("empty GOCACHE= suppressed the default; want the sandbox-writable path applied")
	}
}

// TestSetupAppServerCommandPinsPerWorkspaceGoCache pins the wiring: the codex
// app-server command's env must actually carry the per-workspace GOCACHE, so
// deleting the withSandboxGoToolchainCaches call (helper still passing) is
// caught here.
func TestSetupAppServerCommandPinsPerWorkspaceGoCache(t *testing.T) {
	codexAppServerStubScript(t, "\n") // installs codex stub + login PATH
	t.Setenv("TMPDIR", "/sandbox-tmp")
	in := appServerInput(codexWorkdir(t, "issue-42"))

	cmd, _, _, err := setupAppServerCommand(context.Background(), in)
	if err != nil {
		t.Fatalf("setupAppServerCommand: %v", err)
	}
	want := "GOCACHE=" + filepath.Join("/sandbox-tmp", "aiops-go-cache", "build", workspaceCacheKey(in.Workdir))
	if !strings.Contains(strings.Join(cmd.Env, "\n"), want) {
		t.Errorf("setupAppServerCommand cmd.Env missing %q; got:\n%s", want, strings.Join(cmd.Env, "\n"))
	}
}

// goCacheEnv extracts GOCACHE/GOMODCACHE values from an env slice for assertions.
func goCacheEnv(t *testing.T, env []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, kv := range env {
		for _, name := range []string{"GOCACHE", "GOMODCACHE"} {
			if v, ok := strings.CutPrefix(kv, name+"="); ok {
				out[name] = v
			}
		}
	}
	return out
}
