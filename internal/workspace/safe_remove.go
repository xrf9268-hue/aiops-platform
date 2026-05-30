package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrSafeRemoveInvalidPath is returned when SafeRemove is called with an empty
// or whitespace-only root/path.
var ErrSafeRemoveInvalidPath = errors.New("safe remove: invalid path")

// ErrSafeRemoveEscapesRoot is returned when SafeRemove is asked to delete a
// path that is not strictly contained under the configured workspace root —
// including the root itself, a sibling of the root, a `..` traversal, or a
// symlink whose resolved target points outside the root.
var ErrSafeRemoveEscapesRoot = errors.New("safe remove: path escapes workspace root")

// SafeRemove deletes path with `os.RemoveAll` after confirming that path is a
// non-empty subdirectory strictly contained under root.
//
// Defense-in-depth guard for the worker cleanup paths (per-task workdir
// rollback after a hook failure, reconcile-driven workspace removal): if a
// future refactor or malformed hook output ever feeds an empty string, the
// process cwd, the workspace root itself, or a symlink that points outside the
// root into these call sites, SafeRemove refuses rather than recursively
// deleting whatever the path happens to point at. See SPEC §9.5 Invariants
// 2 & 3, §15.2 (mandatory filesystem safety).
//
// A path that does not exist is treated as success — SafeRemove is meant to be
// idempotent because both worker call sites can race with manual operator
// cleanup. Containment is checked first so that a non-existent path under root
// is allowed but a non-existent path outside root is still rejected.
func SafeRemove(root, path string) error { //nolint:gocognit // baseline (#521)
	root = strings.TrimSpace(root)
	path = strings.TrimSpace(path)
	if root == "" || path == "" {
		return ErrSafeRemoveInvalidPath
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("safe remove: abs root: %w", err)
	}
	absRoot = filepath.Clean(absRoot)
	absPath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("safe remove: abs path: %w", err)
	}
	absPath = filepath.Clean(absPath)
	if err := assertContained(absRoot, absPath); err != nil {
		return err
	}
	// Re-validate after resolving symlinks on both root and path. A directory
	// symlink under root that points outside (e.g. a stray operator-created
	// link, or a malicious workspace file) must be rejected even though its
	// raw `absPath` looks contained. Root is symlink-resolved too so platforms
	// whose tempdir is itself a symlink (e.g. macOS `/var` → `/private/var`)
	// don't trip the comparison.
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		if os.IsNotExist(err) {
			resolvedRoot = absRoot
		} else {
			return fmt.Errorf("safe remove: resolve root symlinks: %w", err)
		}
	}
	resolvedRoot = filepath.Clean(resolvedRoot)
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		if err := assertContained(resolvedRoot, filepath.Clean(resolved)); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("safe remove: resolve symlinks: %w", err)
	}
	return os.RemoveAll(absPath)
}

func assertContained(absRoot, absPath string) error {
	if absRoot == absPath {
		return fmt.Errorf("%w: refuses to delete the workspace root itself (%s)", ErrSafeRemoveEscapesRoot, absRoot)
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return fmt.Errorf("safe remove: relpath: %w", err)
	}
	if rel == "." || rel == "" {
		return fmt.Errorf("%w: refuses to delete the workspace root itself (%s)", ErrSafeRemoveEscapesRoot, absRoot)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %s not under %s", ErrSafeRemoveEscapesRoot, absPath, absRoot)
	}
	return nil
}
