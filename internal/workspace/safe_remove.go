package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrSafeRemoveInvalidPath is returned when removal validation receives an
// empty, whitespace-only, or non-absolute root/path.
var ErrSafeRemoveInvalidPath = errors.New("safe remove: invalid path")

// ErrSafeRemoveEscapesRoot is returned when SafeRemove is asked to delete a
// path that is not strictly contained under the configured workspace root —
// including the root itself, a sibling of the root, a `..` traversal, or a
// symlink whose resolved target points outside the root.
var ErrSafeRemoveEscapesRoot = errors.New("safe remove: path escapes workspace root")

// ValidatedRemoval carries the canonical root identity established before a
// cleanup hook. Remove revalidates against that identity so replacing the
// recorded root during the hook cannot make an external tree the new boundary.
type ValidatedRemoval struct {
	root          string
	path          string
	canonicalRoot string
}

// ValidateRemove confirms that path is an absolute, non-empty subdirectory
// strictly contained under an absolute root without deleting it. Cleanup paths
// that run hooks must retain the returned guard and call Remove afterward so a
// hook-time path or root swap is rejected too.
func ValidateRemove(root, path string) (ValidatedRemoval, error) {
	return validateRemoval(root, path)
}

// Remove repeats path validation and refuses removal when the canonical root
// differs from the identity captured by ValidateRemove.
func (v ValidatedRemoval) Remove() error {
	current, err := validateRemoval(v.root, v.path)
	if err != nil {
		return err
	}
	if current.canonicalRoot != v.canonicalRoot {
		return fmt.Errorf(
			"%w: workspace root changed from %s to %s",
			ErrSafeRemoveEscapesRoot,
			v.canonicalRoot,
			current.canonicalRoot,
		)
	}
	return os.RemoveAll(v.path)
}

// SafeRemove deletes path with `os.RemoveAll` after applying the same checks as
// ValidateRemove.
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
func SafeRemove(root, path string) error {
	removal, err := ValidateRemove(root, path)
	if err != nil {
		return err
	}
	return removal.Remove()
}

func validateRemoval(root, path string) (ValidatedRemoval, error) {
	root = strings.TrimSpace(root)
	path = strings.TrimSpace(path)
	if root == "" || path == "" {
		return ValidatedRemoval{}, ErrSafeRemoveInvalidPath
	}
	if !filepath.IsAbs(root) || !filepath.IsAbs(path) {
		return ValidatedRemoval{}, fmt.Errorf("%w: root and path must be absolute", ErrSafeRemoveInvalidPath)
	}
	if hasParentTraversal(root) || hasParentTraversal(path) {
		return ValidatedRemoval{}, fmt.Errorf("%w: parent traversal is not allowed", ErrSafeRemoveEscapesRoot)
	}
	lexicalRoot := filepath.Clean(root)
	lexicalPath := filepath.Clean(path)
	if err := assertContained(lexicalRoot, lexicalPath); err != nil {
		return ValidatedRemoval{}, err
	}
	canonicalRoot, err := resolveRemovePath(root)
	if err != nil {
		return ValidatedRemoval{}, err
	}
	canonicalPath, err := resolveRemovePath(path)
	if err != nil {
		return ValidatedRemoval{}, err
	}
	if err := assertContained(canonicalRoot, canonicalPath); err != nil {
		return ValidatedRemoval{}, err
	}
	return ValidatedRemoval{root: root, path: path, canonicalRoot: canonicalRoot}, nil
}

func resolveRemovePath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("safe remove: resolve symlinks: %w", err)
	}
	info, lstatErr := os.Lstat(path)
	if lstatErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: dangling symlink %s", ErrSafeRemoveEscapesRoot, path)
		}
		return "", fmt.Errorf("safe remove: path changed while resolving %s: %w", path, err)
	}
	if !os.IsNotExist(lstatErr) {
		return "", fmt.Errorf("safe remove: lstat %s: %w", path, lstatErr)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return filepath.Clean(path), nil
	}
	resolvedParent, err := resolveRemovePath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(path)), nil
}

func hasParentTraversal(path string) bool {
	for _, segment := range strings.Split(filepath.ToSlash(path), "/") {
		if segment == ".." {
			return true
		}
	}
	return false
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
