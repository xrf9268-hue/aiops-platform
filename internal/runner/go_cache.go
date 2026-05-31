package runner

import (
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// withSandboxGoToolchainCaches returns env with GOCACHE/GOMODCACHE defaulted to
// sandbox-writable directories under the host temp dir, unless the caller
// already set a non-empty value. It is applied only on the sandboxed codex
// app-server path.
//
// Why: Go derives GOCACHE from $HOME/.cache/go-build and GOMODCACHE from
// $GOPATH/pkg/mod, both outside codex's workspace-write sandbox writable roots,
// so the agent's first `go test ./...` fails with a cache-write error until it
// rediscovers a writable GOCACHE — a wasted turn on every Go workflow (#544).
//
// Isolation differs by cache, by trust model (codex review #548):
//   - GOCACHE (build cache) is keyed by action ID and is NOT re-verified on
//     reuse, so a shared writable one is a poisoning surface across trust
//     boundaries. It is isolated PER WORKSPACE (an issue's own retries reuse the
//     same workspace and keep their cache; different repos/issues do not share).
//   - GOMODCACHE (module cache) is SHARED (workspace-independent). go.sum
//     verification covers the download path but not reuse of an
//     already-extracted module tree, so sharing rests on the single-operator
//     trust model plus read-only (0444) cache entries raising the tampering
//     bar — not on cryptographic re-verification. Sharing also avoids
//     re-fetching the whole dependency graph on every issue's first turn.
//
// Anchor: os.TempDir() — /tmp on a default Linux worker (codex's workspace-write
// sandbox grants it; empirically confirmed in #544) and the platform temp dir
// ($TMPDIR, e.g. /var/folders/...) on macOS (#539). The load-bearing assumption
// is host-temp ≡ sandbox-writable-temp; it holds for the documented default
// worker env but NOT if an operator points the worker's TMPDIR at a directory
// the sandbox cannot write — that operator must override GOCACHE/GOMODCACHE via
// codex.env_passthrough, which wins because a default is applied only when the
// name is not already set to a non-empty value.
//
// The per-workspace build cache dirs live outside the workspace tree, so a
// lazy TTL reaper keeps long-lived workers from retaining one forever per
// distinct workspace. The claude ShellRunner does not use this — it runs
// unsandboxed, where Go's $HOME-based defaults already work.
// goToolchainCaches is the single source for the Go cache env vars the worker
// injects on the codex sandboxed path: GOCACHE is isolated per workspace (build
// cache, unverified → poisoning vector); GOMODCACHE is shared (subdir only).
// sandboxEnv reads goCacheNames() from this list to carry the injected defaults
// through the optional aiops bubblewrap/firejail allowlist.
var goToolchainCaches = []struct {
	name         string
	subdir       string
	perWorkspace bool
}{
	{name: "GOCACHE", subdir: "build", perWorkspace: true},
	{name: "GOMODCACHE", subdir: "mod", perWorkspace: false},
}

// goBuildCacheMaxAge is measured from the top-level build/<key> directory
// mtime, not from last cache hit; warm-but-idle caches may be rebuilt.
const goBuildCacheMaxAge = 7 * 24 * time.Hour

var (
	goCacheNow       = time.Now
	goCacheRemoveAll = os.RemoveAll

	goBuildCacheActiveMu sync.Mutex
	goBuildCacheActive   = map[string]int{}
)

// aiopsGoCacheRoot is the parent of the worker-injected Go cache dirs.
func aiopsGoCacheRoot() string { return filepath.Join(os.TempDir(), "aiops-go-cache") }

// isWorkerInjectedGoCache reports whether value is a worker-injected Go cache
// path (under aiopsGoCacheRoot). sandboxEnv carries only those through its
// allowlist, never re-exporting an operator's own GOCACHE/GOMODCACHE that the
// operator intentionally kept out of sandbox.env_allowlist (codex review #548).
func isWorkerInjectedGoCache(value string) bool {
	root := aiopsGoCacheRoot()
	return value == root || strings.HasPrefix(value, root+string(os.PathSeparator))
}

func withSandboxGoToolchainCaches(env []string, workdir string) []string {
	root := aiopsGoCacheRoot()
	for _, c := range goToolchainCaches {
		if envHasValue(env, c.name) {
			continue
		}
		path := filepath.Join(root, c.subdir)
		if c.perWorkspace {
			path = SandboxGoBuildCachePath(workdir)
		}
		env = append(env, c.name+"="+path)
	}
	return env
}

// SandboxGoBuildCachePath returns the worker-owned per-workspace Go build cache
// path for workdir. Worker cleanup uses it to remove caches with workspaces.
func SandboxGoBuildCachePath(workdir string) string {
	return filepath.Join(aiopsGoCacheRoot(), "build", workspaceCacheKey(workdir))
}

// RemoveSandboxGoBuildCache removes the worker-owned per-workspace Go build
// cache for workdir. Missing cache directories are treated as already removed.
func RemoveSandboxGoBuildCache(workdir string) error {
	path := SandboxGoBuildCachePath(workdir)
	if err := goCacheRemoveAll(path); err != nil {
		return fmt.Errorf("remove Go build cache %s: %w", path, err)
	}
	return nil
}

func markActiveGoBuildCache(workdir string) func() {
	key := workspaceCacheKey(workdir)
	goBuildCacheActiveMu.Lock()
	goBuildCacheActive[key]++
	goBuildCacheActiveMu.Unlock()
	return func() { unmarkActiveGoBuildCache(key) }
}

func unmarkActiveGoBuildCache(key string) {
	goBuildCacheActiveMu.Lock()
	defer goBuildCacheActiveMu.Unlock()
	if goBuildCacheActive[key] <= 1 {
		delete(goBuildCacheActive, key)
		return
	}
	goBuildCacheActive[key]--
}

func activeGoBuildCache(key string) bool {
	goBuildCacheActiveMu.Lock()
	defer goBuildCacheActiveMu.Unlock()
	return goBuildCacheActive[key] > 0
}

func reapSandboxGoBuildCaches() error {
	root := filepath.Join(aiopsGoCacheRoot(), "build")
	entries, err := readGoBuildCacheEntries(root)
	if err != nil {
		return err
	}
	cutoff := goCacheNow().Add(-goBuildCacheMaxAge)
	var errs []error
	for _, entry := range entries {
		if err := reapGoBuildCacheEntry(root, entry, cutoff); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func readGoBuildCacheEntries(root string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read Go build cache root %s: %w", root, err)
	}
	return entries, nil
}

func reapGoBuildCacheEntry(root string, entry os.DirEntry, cutoff time.Time) error {
	if !entry.IsDir() || activeGoBuildCache(entry.Name()) {
		return nil
	}
	path := filepath.Join(root, entry.Name())
	info, err := entry.Info()
	if err != nil {
		return goBuildCacheStatError(path, err)
	}
	if info.ModTime().After(cutoff) {
		return nil
	}
	// Directory mtime is only a stale-cache signal after active runs release
	// their key; live app-server sessions are excluded above.
	if err := goCacheRemoveAll(path); err != nil {
		return fmt.Errorf("remove stale Go build cache %s: %w", path, err)
	}
	return nil
}

func goBuildCacheStatError(path string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("stat Go build cache %s: %w", path, err)
}

// goCacheNames returns the names of the Go cache env vars the worker injects, so
// the aiops sandbox env builder can preserve them past its allowlist.
func goCacheNames() []string {
	names := make([]string, len(goToolchainCaches))
	for i, c := range goToolchainCaches {
		names[i] = c.name
	}
	return names
}

// workspaceCacheKey is a stable, collision-resistant token for a workspace path
// so two different repos/issues never share a GOCACHE directory while an issue's
// own retries (same workspace) reuse theirs. An empty workdir resolves to the
// worker CWD via filepath.Abs, collapsing all such runs onto one key — harmless
// because the codex path always supplies a per-issue workspace.
func workspaceCacheKey(workdir string) string {
	key := workdir
	if abs, err := filepath.Abs(workdir); err == nil && abs != "" {
		key = abs
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(key))
	return fmt.Sprintf("%016x", h.Sum64())
}

// envHasValue reports whether env already defines name with a NON-EMPTY value.
// An empty "name=" entry counts as absent so the default still applies — Go
// treats an empty GOCACHE as unset and falls back to the sandbox-invisible
// $HOME cache, which is the failure this fixes (codex review #548 P2).
func envHasValue(env []string, name string) bool {
	prefix := name + "="
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, prefix); ok && v != "" {
			return true
		}
	}
	return false
}
