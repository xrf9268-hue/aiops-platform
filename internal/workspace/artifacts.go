package workspace

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const SensitiveArtifactFileMode os.FileMode = 0o600

var AllowedHandoffArtifactPaths = []string{
	".aiops/PLAN.md",
}

var sensitiveArtifactPaths = []string{
	".aiops/PROMPT.md",
	".aiops/TASK.md",
	".aiops/CODEX_OUTPUT.txt",
	".aiops/CODEX_APP_SERVER_OUTPUT.txt",
	".aiops/CODEX_LAST_MESSAGE.md",
}

var sensitiveArtifactExcludePatterns = append(sensitiveArtifactPaths, ".aiops/logs/")

func sensitiveArtifactPreCommitHook() string {
	patterns := make([]string, 0, len(AllowedHandoffArtifactPaths))
	for _, path := range AllowedHandoffArtifactPaths {
		patterns = append(patterns, "-e '"+path+"'")
	}
	return fmt.Sprintf(`#!/bin/sh
blocked=$(git diff --cached --name-only --diff-filter=ACMRTUXB -- .aiops | grep -Fxv %s || true)
if [ -n "$blocked" ]; then
	printf '%%s\n' "aiops-platform: staged internal .aiops artifact blocked:" >&2
	printf '%%s\n' "$blocked" >&2
	exit 1
fi
`, strings.Join(patterns, " "))
}

func WriteSensitiveArtifact(path string, body []byte) error { //nolint:gocognit // baseline (#521)
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	if info, err := os.Lstat(parent); err != nil {
		return err
	} else if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("sensitive artifact parent %s is a symlink", parent)
	}
	if info, err := os.Lstat(path); err == nil {
		if links, ok := linkCount(info); ok && links > 1 {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, SensitiveArtifactFileMode)
	if err != nil {
		return err
	}
	if info, err := f.Stat(); err != nil {
		_ = f.Close()
		return err
	} else if links, ok := linkCount(info); ok && links > 1 {
		_ = f.Close()
		return fmt.Errorf("sensitive artifact %s has %d hard links", path, links)
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(SensitiveArtifactFileMode); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close sensitive artifact %s: %w", path, err)
	}
	return nil
}

func ReadSensitiveArtifact(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err != nil {
		return nil, err
	} else if links, ok := linkCount(info); ok && links > 1 {
		return nil, fmt.Errorf("sensitive artifact %s has %d hard links", path, links)
	}
	if err := f.Chmod(SensitiveArtifactFileMode); err != nil {
		return nil, err
	}
	return io.ReadAll(f)
}

func linkCount(info os.FileInfo) (uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true
}

func EnsureSensitiveArtifactExcludes(ctx context.Context, workdir string) error { //nolint:gocognit // baseline (#521)
	excludePath, err := gitPath(ctx, workdir, "info/exclude")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return err
	}
	existing, err := os.ReadFile(excludePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	seen := map[string]struct{}{}
	for _, line := range strings.Split(string(existing), "\n") {
		seen[strings.TrimSpace(line)] = struct{}{}
	}
	var add []string
	for _, rel := range sensitiveArtifactExcludePatterns {
		if _, ok := seen[rel]; !ok {
			add = append(add, rel)
		}
	}
	if len(add) > 0 {
		f, err := os.OpenFile(excludePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		closeFile := true
		defer func() {
			if closeFile {
				_ = f.Close()
			}
		}()
		if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
			if _, err := f.WriteString("\n"); err != nil {
				return err
			}
		}
		if _, err := f.WriteString(strings.Join(add, "\n") + "\n"); err != nil {
			return err
		}
		closeFile = false
		if err := f.Close(); err != nil {
			return fmt.Errorf("close git exclude %s: %w", excludePath, err)
		}
	}
	out, err := runGitOutput(ctx, workdir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return fmt.Errorf("resolve worktree git dir: %w", err)
	}
	hookDir := filepath.Join(strings.TrimSpace(string(out)), "aiops-hooks")
	if err := os.MkdirAll(hookDir, 0o755); err != nil {
		return err
	}
	if err := runGitQuiet(ctx, workdir, "config", "--worktree", "core.bare", "false"); err != nil {
		return err
	}
	if err := runGitQuiet(ctx, workdir, "config", "--worktree", "core.hooksPath", hookDir); err != nil {
		return err
	}
	hookPath := filepath.Join(hookDir, "pre-commit")
	if err := os.WriteFile(hookPath, []byte(sensitiveArtifactPreCommitHook()), 0o755); err != nil {
		return err
	}
	return os.Chmod(hookPath, 0o755)
}

func gitPath(ctx context.Context, workdir, rel string) (string, error) {
	out, err := runGitOutput(ctx, workdir, "rev-parse", "--git-path", rel)
	if err != nil {
		return "", fmt.Errorf("resolve git path %s: %w", rel, err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("resolve git path %s: empty path", rel)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workdir, path)
	}
	return path, nil
}
