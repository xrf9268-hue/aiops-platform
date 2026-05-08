package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/policy"
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
	d, err := Diffstat(ctx, workdir)
	if err != nil {
		return nil, err
	}
	return d.Files, nil
}

// Diffstat returns the set of changed files and the total added+deleted
// lines for the working tree at workdir. It uses `git add --intent-to-add
// --all` so newly created (untracked) files show up, then runs
// `git diff --numstat HEAD` to capture both modified and added paths.
// Binary files (which numstat reports as "-\t-") contribute 0 lines but
// are still counted as changed files.
func Diffstat(ctx context.Context, workdir string) (policy.Diffstat, error) {
	// Mark untracked files so they appear in `git diff` without actually
	// staging their content. This is reversible and idempotent; the
	// subsequent `git add .` in CommitAndPush will fully stage them.
	if err := run(ctx, workdir, "git", "add", "--intent-to-add", "--all"); err != nil {
		return policy.Diffstat{}, fmt.Errorf("git add --intent-to-add: %w", err)
	}
	cmd := exec.CommandContext(ctx, "git", "diff", "--numstat", "HEAD")
	cmd.Dir = workdir
	out, err := cmd.Output()
	if err != nil {
		return policy.Diffstat{}, err
	}
	var d policy.Diffstat
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// numstat format: "<added>\t<deleted>\t<path>" (path may include rename "old => new")
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		added, _ := strconv.Atoi(parts[0])
		deleted, _ := strconv.Atoi(parts[1])
		d.Lines += added + deleted
		d.Files = append(d.Files, parts[2])
	}
	return d, nil
}

// PolicyError is returned by EnforcePolicy when one or more policy checks
// fail. It carries the structured violations so callers (the worker) can
// emit a precise task event.
type PolicyError struct {
	Violations []policy.Violation
}

func (e *PolicyError) Error() string {
	return "policy violation: " + policy.Summarize(e.Violations)
}

// EnforcePolicy gathers a diffstat for workdir and evaluates it against the
// workflow PolicyConfig. On any violation it returns *PolicyError so that
// the worker can both block the push and write a structured task event.
func EnforcePolicy(ctx context.Context, workdir string, cfg workflow.Config) error {
	d, err := Diffstat(ctx, workdir)
	if err != nil {
		return err
	}
	pcfg := policy.Config{
		AllowPaths:      cfg.Policy.AllowPaths,
		DenyPaths:       cfg.Policy.DenyPaths,
		MaxChangedFiles: cfg.Policy.MaxChangedFiles,
		MaxChangedLines: cfg.Policy.LineLimit(),
	}
	if vs := policy.Evaluate(d, pcfg); len(vs) > 0 {
		return &PolicyError{Violations: vs}
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
