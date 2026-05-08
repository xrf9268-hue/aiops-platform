package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/policy"
	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// VerifyResult captures the outcome of running a workflow verify command.
// Output contains the combined stdout+stderr so it can be persisted as a
// run artifact even when the command fails.
type VerifyResult struct {
	Command  string
	ExitCode int
	Output   string
	Duration time.Duration
	Err      error
}

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

// RunVerify executes the workflow verify commands in order. It returns one
// VerifyResult per non-empty command (even when one fails) so callers can
// persist the combined output as a run artifact for post-mortem inspection.
// On the first failing command the slice is returned together with the
// underlying error and remaining commands are skipped.
func RunVerify(ctx context.Context, workdir string, wf workflow.Config) ([]VerifyResult, error) {
	var results []VerifyResult
	for _, command := range wf.Verify.Commands {
		if strings.TrimSpace(command) == "" {
			continue
		}
		start := time.Now()
		var buf bytes.Buffer
		cmd := exec.CommandContext(ctx, "sh", "-lc", command)
		cmd.Dir = workdir
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		runErr := cmd.Run()
		res := VerifyResult{
			Command:  command,
			Output:   buf.String(),
			Duration: time.Since(start),
			ExitCode: cmd.ProcessState.ExitCode(),
		}
		if runErr != nil {
			res.Err = runErr
			results = append(results, res)
			return results, fmt.Errorf("verify command failed %q: %w", command, runErr)
		}
		results = append(results, res)
	}
	return results, nil
}

// WriteSummary writes the per-run summary to .aiops/RUN_SUMMARY.md so it can
// be committed alongside the change and inspected on failure paths.
func WriteSummary(workdir, summary string) error {
	return writeAiopsFile(workdir, "RUN_SUMMARY.md", summary)
}

// WriteChangedFiles writes one path per line to .aiops/CHANGED_FILES.txt.
func WriteChangedFiles(workdir string, files []string) error {
	body := strings.Join(files, "\n")
	if body != "" {
		body += "\n"
	}
	return writeAiopsFile(workdir, "CHANGED_FILES.txt", body)
}

// WriteVerification serializes verify command results to
// .aiops/VERIFICATION.txt so failed runs preserve the diagnostic output.
func WriteVerification(workdir string, results []VerifyResult) error {
	var buf bytes.Buffer
	for i, r := range results {
		if i > 0 {
			buf.WriteString("\n")
		}
		fmt.Fprintf(&buf, "$ %s\n", r.Command)
		fmt.Fprintf(&buf, "exit_code=%d duration_ms=%d\n", r.ExitCode, r.Duration.Milliseconds())
		if r.Err != nil {
			fmt.Fprintf(&buf, "error: %s\n", r.Err.Error())
		}
		if r.Output != "" {
			buf.WriteString(r.Output)
			if !strings.HasSuffix(r.Output, "\n") {
				buf.WriteString("\n")
			}
		}
	}
	return writeAiopsFile(workdir, "VERIFICATION.txt", buf.String())
}

func writeAiopsFile(workdir, name, body string) error {
	dir := filepath.Join(workdir, ".aiops")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
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
// `git diff --numstat -z HEAD` to capture both modified and added paths.
// The `-z` flag is required for correct policy matching: without it, git
// emits rename entries as `old => new` or `{a => b}/c` strings that no
// glob will match, silently bypassing deny/allow rules. With `-z`, rename
// entries are emitted as `added\tdeleted\t\0old\0new\0` (three NUL fields
// instead of one), letting us pull the destination path verbatim.
// Binary files (which numstat reports as "-\t-") contribute 0 lines but
// are still counted as changed files.
func Diffstat(ctx context.Context, workdir string) (policy.Diffstat, error) {
	// Mark untracked files so they appear in `git diff` without actually
	// staging their content. This is reversible and idempotent; the
	// subsequent `git add .` in CommitAndPush will fully stage them.
	if err := run(ctx, workdir, "git", "add", "--intent-to-add", "--all"); err != nil {
		return policy.Diffstat{}, fmt.Errorf("git add --intent-to-add: %w", err)
	}
	cmd := exec.CommandContext(ctx, "git", "diff", "--numstat", "-z", "HEAD")
	cmd.Dir = workdir
	out, err := cmd.Output()
	if err != nil {
		return policy.Diffstat{}, err
	}
	return parseNumstatZ(out)
}

// parseNumstatZ parses the NUL-delimited output of `git diff --numstat -z`.
// Each record is one of:
//
//	"<added>\t<deleted>\t<path>\0"               (normal)
//	"<added>\t<deleted>\t\0<old_path>\0<new_path>\0" (rename/copy)
//
// For renames we record only the destination path so that policy globs
// match the file as it will live in the tree.
func parseNumstatZ(out []byte) (policy.Diffstat, error) {
	var d policy.Diffstat
	// Split on NUL; the trailing terminator yields a final empty token we skip.
	tokens := strings.Split(string(out), "\x00")
	i := 0
	for i < len(tokens) {
		tok := tokens[i]
		if tok == "" {
			i++
			continue
		}
		// Each numstat record begins with "<added>\t<deleted>\t<rest>".
		parts := strings.SplitN(tok, "\t", 3)
		if len(parts) != 3 {
			i++
			continue
		}
		added, _ := strconv.Atoi(parts[0])
		deleted, _ := strconv.Atoi(parts[1])
		d.Lines += added + deleted
		if parts[2] == "" {
			// Rename/copy: next two NUL-delimited tokens are old, then new.
			if i+2 >= len(tokens) {
				return d, fmt.Errorf("malformed numstat -z rename record: %q", tok)
			}
			newPath := tokens[i+2]
			d.Files = append(d.Files, newPath)
			i += 3
			continue
		}
		d.Files = append(d.Files, parts[2])
		i++
	}
	return d, nil
}

// AllChangedFiles returns every path the worker is about to commit: tracked
// modifications plus untracked files reported by `git status --porcelain`.
// This is what we want to persist as a run artifact since the runner often
// creates new files (for example .aiops/RUN_SUMMARY.md) that `git diff`
// alone does not surface.
func AllChangedFiles(ctx context.Context, workdir string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain")
	cmd.Dir = workdir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	files := []string{}
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if path == "" {
			continue
		}
		// Rename entries look like "old -> new"; keep the new path.
		if idx := strings.Index(path, " -> "); idx >= 0 {
			path = strings.TrimSpace(path[idx+4:])
		}
		path = strings.Trim(path, "\"")
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		files = append(files, path)
	}
	return files, nil
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
