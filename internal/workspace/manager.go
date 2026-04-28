package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type Manager struct {
	Root string
}

func New(root string) *Manager { return &Manager{Root: root} }

func (m *Manager) PathFor(t task.Task) string {
	repo := sanitize(t.RepoOwner + "_" + t.RepoName)
	return filepath.Join(m.Root, repo, t.ID)
}

func (m *Manager) PrepareGitWorkspace(ctx context.Context, t task.Task) (string, error) {
	workdir := m.PathFor(t)
	_ = os.RemoveAll(workdir)
	if err := os.MkdirAll(filepath.Dir(workdir), 0o755); err != nil {
		return "", err
	}
	if err := run(ctx, filepath.Dir(workdir), "git", "clone", t.CloneURL, workdir); err != nil {
		return "", err
	}
	if err := run(ctx, workdir, "git", "checkout", t.BaseBranch); err != nil {
		return "", err
	}
	if err := run(ctx, workdir, "git", "checkout", "-b", t.WorkBranch); err != nil {
		return "", err
	}
	return workdir, nil
}

func WritePrompt(workdir string, prompt string) error {
	dir := filepath.Join(workdir, ".aiops")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "PROMPT.md"), []byte(prompt), 0o644)
}

func RunVerify(ctx context.Context, workdir string, wf workflow.Config) error {
	for _, command := range wf.Verify.Commands {
		if strings.TrimSpace(command) == "" {
			continue
		}
		if err := run(ctx, workdir, "sh", "-lc", command); err != nil {
			return fmt.Errorf("verify command failed %q: %w", command, err)
		}
	}
	return nil
}

func ChangedFiles(ctx context.Context, workdir string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only")
	cmd.Dir = workdir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	files := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			files = append(files, strings.TrimSpace(l))
		}
	}
	return files, nil
}

func EnforcePolicy(ctx context.Context, workdir string, cfg workflow.Config) error {
	files, err := ChangedFiles(ctx, workdir)
	if err != nil {
		return err
	}
	if cfg.Policy.MaxChangedFiles > 0 && len(files) > cfg.Policy.MaxChangedFiles {
		return fmt.Errorf("changed files %d exceeds max %d", len(files), cfg.Policy.MaxChangedFiles)
	}
	for _, f := range files {
		for _, p := range cfg.Policy.DenyPaths {
			if matchesPath(p, f) {
				return fmt.Errorf("changed denied path %s matches %s", f, p)
			}
		}
	}
	return nil
}

func CommitAndPush(ctx context.Context, workdir string, title string, branch string) error {
	if err := run(ctx, workdir, "git", "add", "."); err != nil {
		return err
	}
	if err := run(ctx, workdir, "git", "diff", "--cached", "--quiet"); err == nil {
		return fmt.Errorf("no changes to commit")
	}
	if err := run(ctx, workdir, "git", "commit", "-m", "chore(ai): "+title); err != nil {
		return err
	}
	return run(ctx, workdir, "git", "push", "origin", branch)
}

func run(ctx context.Context, dir string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, " ", "_")
	return s
}

func matchesPath(pattern, file string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if strings.HasSuffix(pattern, "/**") {
		return strings.HasPrefix(file, strings.TrimSuffix(pattern, "**"))
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(file, strings.TrimSuffix(pattern, "*"))
	}
	ok, _ := filepath.Match(pattern, file)
	return ok || pattern == file
}
