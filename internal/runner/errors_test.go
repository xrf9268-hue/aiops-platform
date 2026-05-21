package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunnerSentinelsCoverSpecCategories(t *testing.T) {
	cases := []struct {
		category RunnerErrorCategory
		sentinel error
	}{
		{CategoryCodexNotFound, ErrCodexNotFound},
		{CategoryInvalidWorkspaceCWD, ErrInvalidWorkspaceCWD},
		{CategoryResponseTimeout, ErrResponseTimeout},
		{CategoryTurnTimeout, ErrTurnTimeout},
		{CategoryPortExit, ErrPortExit},
		{CategoryResponseError, ErrResponseError},
		{CategoryTurnFailed, ErrTurnFailed},
		{CategoryTurnCancelled, ErrTurnCancelled},
		{CategoryTurnInputRequired, ErrTurnInputRequired},
	}
	for _, tc := range cases {
		err := NewError(tc.category, "runner category", nil)
		if !errors.Is(err, tc.sentinel) {
			t.Fatalf("NewError(%q) = %T %[2]v, want errors.Is sentinel", tc.category, err)
		}
		if got, ok := ErrorCategory(err); !ok || got != tc.category {
			t.Fatalf("ErrorCategory(%q) = %q, %v", tc.category, got, ok)
		}
	}
}

func TestCodexAppServerRunnerSurfacesRunnerErrorCategories(t *testing.T) {
	t.Run("codex not found", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, err := buildCodexAppServerCmd(context.Background(), appServerInput(t.TempDir()))
		if !errors.Is(err, ErrCodexNotFound) {
			t.Fatalf("buildCodexAppServerCmd error = %T %[1]v, want ErrCodexNotFound", err)
		}
	})

	t.Run("invalid workspace cwd", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "missing")
		_, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(missing))
		if !errors.Is(err, ErrInvalidWorkspaceCWD) {
			t.Fatalf("Run missing workdir error = %T %[1]v, want ErrInvalidWorkspaceCWD", err)
		}
	})
}

func TestTimeoutTypesExposeRunnerCategories(t *testing.T) {
	read := &ReadTimeoutError{Timeout: time.Millisecond, Cause: errors.New("read")}
	if !errors.Is(read, ErrResponseTimeout) {
		t.Fatalf("ReadTimeoutError does not match ErrResponseTimeout")
	}
	turn := &TurnTimeoutError{Timeout: time.Millisecond, Elapsed: time.Millisecond, Cause: context.DeadlineExceeded}
	if !errors.Is(turn, ErrTurnTimeout) {
		t.Fatalf("TurnTimeoutError does not match ErrTurnTimeout")
	}
	if got, ok := ErrorCategory(turn); !ok || got != CategoryTurnTimeout {
		t.Fatalf("ErrorCategory(turn) = %q, %v; want %q, true", got, ok, CategoryTurnTimeout)
	}
}

func TestCodexAppServerRunnerStillReadsPromptForValidWorkspace(t *testing.T) {
	wd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wd, ".aiops"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd, PromptPath), []byte("prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	_, err := (CodexAppServerRunner{}).Run(context.Background(), appServerInput(wd))
	if err == nil || !strings.Contains(err.Error(), "codex binary not found") {
		t.Fatalf("Run valid workdir error = %T %[1]v, want codex lookup after prompt read", err)
	}
}
