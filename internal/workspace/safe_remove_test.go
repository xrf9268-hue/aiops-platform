package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestSafeRemoveRejectsEmptyRootOrPath(t *testing.T) {
	root := t.TempDir()

	if err := SafeRemove("", filepath.Join(root, "x")); !errors.Is(err, ErrSafeRemoveInvalidPath) {
		t.Fatalf("empty root err = %v, want ErrSafeRemoveInvalidPath", err)
	}
	if err := SafeRemove(root, ""); !errors.Is(err, ErrSafeRemoveInvalidPath) {
		t.Fatalf("empty path err = %v, want ErrSafeRemoveInvalidPath", err)
	}
	if err := SafeRemove(root, "   "); !errors.Is(err, ErrSafeRemoveInvalidPath) {
		t.Fatalf("whitespace path err = %v, want ErrSafeRemoveInvalidPath", err)
	}
}

func TestSafeRemoveRejectsRootItself(t *testing.T) {
	root := t.TempDir()

	if err := SafeRemove(root, root); !errors.Is(err, ErrSafeRemoveEscapesRoot) {
		t.Fatalf("root == path err = %v, want ErrSafeRemoveEscapesRoot", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("root must remain after rejection: %v", err)
	}
}

func TestSafeRemoveRejectsPathOutsideRoot(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	sibling := filepath.Join(outside, "victim")
	if err := os.WriteFile(sibling, []byte("keep"), 0o600); err != nil {
		t.Fatalf("seed sibling: %v", err)
	}

	if err := SafeRemove(root, sibling); !errors.Is(err, ErrSafeRemoveEscapesRoot) {
		t.Fatalf("outside path err = %v, want ErrSafeRemoveEscapesRoot", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("sibling must remain after rejection: %v", err)
	}
}

func TestSafeRemoveRejectsParentTraversal(t *testing.T) {
	root := t.TempDir()
	traverse := filepath.Join(root, "..", filepath.Base(t.TempDir()))

	if err := SafeRemove(root, traverse); !errors.Is(err, ErrSafeRemoveEscapesRoot) {
		t.Fatalf("traversal err = %v, want ErrSafeRemoveEscapesRoot", err)
	}
}

func TestSafeRemoveRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root := t.TempDir()
	external := t.TempDir()
	externalChild := filepath.Join(external, "do-not-delete")
	if err := os.WriteFile(externalChild, []byte("keep"), 0o600); err != nil {
		t.Fatalf("seed external: %v", err)
	}
	link := filepath.Join(root, "evil")
	if err := os.Symlink(external, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := SafeRemove(root, link); !errors.Is(err, ErrSafeRemoveEscapesRoot) {
		t.Fatalf("symlinked-out err = %v, want ErrSafeRemoveEscapesRoot", err)
	}
	if _, err := os.Stat(externalChild); err != nil {
		t.Fatalf("external child must remain: %v", err)
	}
}

func TestSafeRemoveSucceedsForValidPath(t *testing.T) {
	root := t.TempDir()
	taskDir := filepath.Join(root, "tsk-1")
	if err := os.MkdirAll(filepath.Join(taskDir, "nested"), 0o700); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "nested", "file"), []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := SafeRemove(root, taskDir); err != nil {
		t.Fatalf("SafeRemove valid: %v", err)
	}
	if _, err := os.Stat(taskDir); !os.IsNotExist(err) {
		t.Fatalf("task dir still exists, err=%v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("root removed unexpectedly: %v", err)
	}
}

func TestSafeRemoveIsIdempotentForMissingPath(t *testing.T) {
	root := t.TempDir()
	taskDir := filepath.Join(root, "tsk-missing")

	if err := SafeRemove(root, taskDir); err != nil {
		t.Fatalf("SafeRemove missing path = %v, want nil (idempotent)", err)
	}
}
