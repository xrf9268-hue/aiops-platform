package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestSetupAppServerCommandReapsStaleGoBuildCaches(t *testing.T) {
	codexAppServerStubScript(t, "\n") // installs codex stub + login PATH
	t.Setenv("TMPDIR", t.TempDir())
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	oldNow := goCacheNow
	goCacheNow = func() time.Time { return now }
	t.Cleanup(func() { goCacheNow = oldNow })

	buildRoot := filepath.Join(aiopsGoCacheRoot(), "build")
	stale := filepath.Join(buildRoot, "stale")
	recent := filepath.Join(buildRoot, "recent")
	modCache := filepath.Join(aiopsGoCacheRoot(), "mod")
	for _, dir := range []string{stale, recent, modCache} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	staleTime := now.Add(-goBuildCacheMaxAge - time.Hour)
	if err := os.Chtimes(stale, staleTime, staleTime); err != nil {
		t.Fatalf("backdate stale cache %s: %v", stale, err)
	}
	recentTime := now.Add(-time.Hour)
	if err := os.Chtimes(recent, recentTime, recentTime); err != nil {
		t.Fatalf("backdate recent cache %s: %v", recent, err)
	}

	in := appServerInput(codexWorkdir(t, "issue-42"))
	if _, _, _, err := setupAppServerCommand(context.Background(), in); err != nil {
		t.Fatalf("setupAppServerCommand: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale Go build cache stat err = %v; want not exist", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent Go build cache stat err = %v; want nil", err)
	}
	if _, err := os.Stat(modCache); err != nil {
		t.Fatalf("shared Go module cache stat err = %v; want nil", err)
	}
}

func TestReapSandboxGoBuildCachesSkipsActiveCache(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	oldNow := goCacheNow
	goCacheNow = func() time.Time { return now }
	t.Cleanup(func() { goCacheNow = oldNow })

	workdir := filepath.Join(t.TempDir(), "issue-42")
	cache := filepath.Join(aiopsGoCacheRoot(), "build", workspaceCacheKey(workdir))
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatalf("mkdir active Go build cache %s: %v", cache, err)
	}
	old := now.Add(-goBuildCacheMaxAge - time.Hour)
	if err := os.Chtimes(cache, old, old); err != nil {
		t.Fatalf("backdate active Go build cache %s: %v", cache, err)
	}

	release := markActiveGoBuildCache(workdir)
	if err := reapSandboxGoBuildCaches(); err != nil {
		t.Fatalf("reapSandboxGoBuildCaches(active) err = %v; want nil", err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("active Go build cache stat err = %v; want nil", err)
	}
	release()
	if err := reapSandboxGoBuildCaches(); err != nil {
		t.Fatalf("reapSandboxGoBuildCaches(released) err = %v; want nil", err)
	}
	if _, err := os.Stat(cache); !os.IsNotExist(err) {
		t.Fatalf("released stale Go build cache stat err = %v; want not exist", err)
	}
}

func TestReapSandboxGoBuildCachesContinuesAfterRemoveError(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	oldNow := goCacheNow
	goCacheNow = func() time.Time { return now }
	t.Cleanup(func() { goCacheNow = oldNow })
	removeErr := errors.New("remove denied")
	oldRemoveAll := goCacheRemoveAll
	t.Cleanup(func() { goCacheRemoveAll = oldRemoveAll })

	buildRoot := filepath.Join(aiopsGoCacheRoot(), "build")
	blocked := filepath.Join(buildRoot, "00-blocked")
	later := filepath.Join(buildRoot, "10-later")
	for _, dir := range []string{blocked, later} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		old := now.Add(-goBuildCacheMaxAge - time.Hour)
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatalf("backdate Go build cache %s: %v", dir, err)
		}
	}
	goCacheRemoveAll = func(path string) error {
		if path == blocked {
			return removeErr
		}
		return os.RemoveAll(path)
	}

	err := reapSandboxGoBuildCaches()
	if !errors.Is(err, removeErr) {
		t.Fatalf("reapSandboxGoBuildCaches() err = %v; want %v", err, removeErr)
	}
	if _, err := os.Stat(blocked); err != nil {
		t.Fatalf("blocked Go build cache stat err = %v; want nil after injected remove failure", err)
	}
	if _, err := os.Stat(later); !os.IsNotExist(err) {
		t.Fatalf("later stale Go build cache stat err = %v; want not exist", err)
	}
}

// TestReapSandboxGoBuildCachesConcurrentWithActiveChurnIsRaceFree pins the
// invariant the goBuildCacheActiveMu guard exists for: a workspace marked active
// is never reaped out from under a live app-server, even while concurrent sweeps
// race a stream of mark/unmark calls. goBuildCacheActive is package-shared state
// mutated from per-run goroutines and read by the reaper, so this exercises the
// mark/active/reap paths under -race; without the mutex the detector trips on the
// map and counter, and a dropped active check would delete heldCache.
func TestReapSandboxGoBuildCachesConcurrentWithActiveChurnIsRaceFree(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	oldNow := goCacheNow
	goCacheNow = func() time.Time { return now }
	t.Cleanup(func() { goCacheNow = oldNow })

	// One workspace stays continuously marked for the whole run; its stale-mtime
	// cache must survive every concurrent reap.
	held := filepath.Join(t.TempDir(), "held")
	heldCache := filepath.Join(aiopsGoCacheRoot(), "build", workspaceCacheKey(held))
	staleGoBuildCacheDir(t, heldCache, now)
	releaseHeld := markActiveGoBuildCache(held)
	t.Cleanup(releaseHeld)

	const churnKeys = 8
	churn := make([]string, churnKeys)
	for i := range churn {
		churn[i] = filepath.Join(t.TempDir(), fmt.Sprintf("churn-%d", i))
		staleGoBuildCacheDir(t, filepath.Join(aiopsGoCacheRoot(), "build", workspaceCacheKey(churn[i])), now)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ { // reapers hammer the sweep against active-set churn
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = reapSandboxGoBuildCaches()
				}
			}
		}()
	}
	for _, w := range churn { // mark/unmark distinct keys, racing the reapers
		wg.Add(1)
		go func(workdir string) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				select {
				case <-stop:
					return
				default:
					markActiveGoBuildCache(workdir)()
				}
			}
		}(w)
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()

	if _, err := os.Stat(heldCache); err != nil {
		t.Fatalf("held active Go build cache stat err = %v; want nil (never reaped while marked)", err)
	}
	// Balanced mark/unmark must drain every churn key, leaving only the held key.
	heldKey := workspaceCacheKey(held)
	goBuildCacheActiveMu.Lock()
	defer goBuildCacheActiveMu.Unlock()
	for key, n := range goBuildCacheActive {
		if key != heldKey {
			t.Errorf("active set leaked key %q = %d; want only held key %q", key, n, heldKey)
		}
	}
}

// staleGoBuildCacheDir creates dir and backdates its mtime past goBuildCacheMaxAge
// relative to now, so the reaper treats it as eligible for eviction.
func staleGoBuildCacheDir(t *testing.T, dir string, now time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir Go build cache %s: %v", dir, err)
	}
	old := now.Add(-goBuildCacheMaxAge - time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatalf("backdate Go build cache %s: %v", dir, err)
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
