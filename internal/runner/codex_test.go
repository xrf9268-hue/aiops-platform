package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// codexStubScript writes a bash script named `codex` into a temp dir and
// prepends that dir to PATH for the duration of the test. The script body
// is supplied by the caller. The script always records its argv and stdin
// to predictable side files inside the same temp dir so tests can assert
// what codex was actually called with.
//
// Returns binDir which contains the recorded argv.txt / stdin.txt and the
// codex script itself.
func codexStubScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "codex")
	header := `#!/usr/bin/env bash
set -e
printf '%s\n' "$@" > "` + dir + `/argv.txt"
cat > "` + dir + `/stdin.txt"
`
	if err := os.WriteFile(scriptPath, []byte(header+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// codexWorkdir creates a per-test workdir with a populated .aiops/PROMPT.md.
func codexWorkdir(t *testing.T, prompt string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".aiops"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aiops", "PROMPT.md"), []byte(prompt), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func codexInput(workdir string) RunInput {
	return RunInput{
		Task: task.Task{ID: "tsk_codex_test", Model: "codex"},
		Workflow: workflow.Workflow{Config: workflow.Config{
			Codex: workflow.CommandConfig{Command: "codex exec", Profile: "safe"},
		}},
		Workdir: workdir,
		Prompt:  "ignored — runner reads .aiops/PROMPT.md",
	}
}

func TestCodexRunner_SafeProfileBuildsExpectedArgv(t *testing.T) {
	// codexStubScript calls t.Setenv; Go 1.25 forbids t.Parallel with t.Setenv.
	binDir := codexStubScript(t, `exit 0`)
	wd := codexWorkdir(t, "hello prompt")

	in := codexInput(wd)
	if _, err := (CodexRunner{}).Run(context.Background(), in); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(binDir, "argv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"exec",
		"--full-auto",
		"--skip-git-repo-check",
		"--cd",
		wd,
		"-o",
		filepath.Join(wd, ".aiops/CODEX_LAST_MESSAGE.md"),
	}
	gotLines := strings.Split(strings.TrimRight(string(got), "\n"), "\n")
	if len(gotLines) != len(want) {
		t.Fatalf("argv lines = %d, want %d; got %q", len(gotLines), len(want), gotLines)
	}
	for i := range want {
		if gotLines[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, gotLines[i], want[i])
		}
	}
}

func TestCodexRunner_PromptPipedToStdin(t *testing.T) {
	binDir := codexStubScript(t, `exit 0`)
	wd := codexWorkdir(t, "stdin canary 42")

	if _, err := (CodexRunner{}).Run(context.Background(), codexInput(wd)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(binDir, "stdin.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "stdin canary 42" {
		t.Fatalf("stdin = %q, want %q", got, "stdin canary 42")
	}
}

func TestCodexRunner_CapturesOutputArtifact(t *testing.T) {
	codexStubScript(t, `printf 'codex-canary-9134\n' ; printf 'err-line\n' >&2 ; exit 0`)
	wd := codexWorkdir(t, "x")

	res, err := (CodexRunner{}).Run(context.Background(), codexInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	out, err := os.ReadFile(filepath.Join(wd, ".aiops/CODEX_OUTPUT.txt"))
	if err != nil {
		t.Fatalf("read CODEX_OUTPUT.txt: %v", err)
	}
	if !strings.Contains(string(out), "codex-canary-9134") {
		t.Fatalf("artifact missing stdout canary; got %q", out)
	}
	if !strings.Contains(string(out), "err-line") {
		t.Fatalf("artifact missing stderr line; got %q", out)
	}
	if res.OutputBytes <= 0 {
		t.Fatalf("Result.OutputBytes = %d, want > 0", res.OutputBytes)
	}
	if res.OutputDropped != 0 {
		t.Fatalf("Result.OutputDropped = %d, want 0 for small output", res.OutputDropped)
	}
	if !strings.Contains(res.OutputHead, "codex-canary-9134") {
		t.Fatalf("OutputHead missing canary; got %q", res.OutputHead)
	}
}

func TestCodexRunner_LastMessageBecomesSummary(t *testing.T) {
	codexStubScript(t, `mkdir -p .aiops && printf 'codex completed task X\n' > .aiops/CODEX_LAST_MESSAGE.md ; exit 0`)
	wd := codexWorkdir(t, "x")

	res, err := (CodexRunner{}).Run(context.Background(), codexInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "codex completed task X" {
		t.Fatalf("Summary = %q, want %q", res.Summary, "codex completed task X")
	}
}

func TestCodexRunner_MissingLastMessageFallsBackToDefaultSummary(t *testing.T) {
	codexStubScript(t, `exit 0`)
	wd := codexWorkdir(t, "x")

	res, err := (CodexRunner{}).Run(context.Background(), codexInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "codex completed" {
		t.Fatalf("Summary = %q, want %q", res.Summary, "codex completed")
	}
}

func TestCodexRunner_OutputExceedsCapTruncates(t *testing.T) {
	// 1.5 MiB of stdout: comfortably above the 1 MiB cap.
	codexStubScript(t, `head -c 1572864 /dev/zero | tr '\0' 'a'`)
	wd := codexWorkdir(t, "x")

	res, err := (CodexRunner{}).Run(context.Background(), codexInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.OutputDropped <= 0 {
		t.Fatalf("OutputDropped = %d, want > 0", res.OutputDropped)
	}
	if res.OutputBytes != int64(CodexOutputCap) {
		t.Fatalf("OutputBytes = %d, want %d (the cap)", res.OutputBytes, CodexOutputCap)
	}
	body, err := os.ReadFile(filepath.Join(wd, ".aiops/CODEX_OUTPUT.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(body), "\n") || !strings.Contains(string(body), "...output truncated at") {
		tailStart := len(body) - 200
		if tailStart < 0 {
			tailStart = 0
		}
		t.Fatalf("artifact missing truncation footer; tail=%q", body[tailStart:])
	}
}

func TestCodexRunner_MissingPromptReturnsWrappedError(t *testing.T) {
	codexStubScript(t, `exit 0`)
	dir := t.TempDir() // no .aiops/PROMPT.md
	in := codexInput(dir)

	_, err := (CodexRunner{}).Run(context.Background(), in)
	if err == nil {
		t.Fatal("expected error for missing PROMPT.md, got nil")
	}
	if IsTimeout(err) {
		t.Fatalf("missing-prompt should not classify as timeout: %v", err)
	}
	if !strings.Contains(err.Error(), "PROMPT.md") {
		t.Fatalf("error %q should mention PROMPT.md", err)
	}
}

func TestCodexRunner_MissingBinaryReturnsClearError(t *testing.T) {
	// Cannot use t.Parallel() with t.Setenv — sequential only.
	t.Setenv("PATH", "") // no codex anywhere
	wd := codexWorkdir(t, "x")
	_, err := (CodexRunner{}).Run(context.Background(), codexInput(wd))
	if err == nil {
		t.Fatal("expected error for missing codex binary")
	}
	if !strings.Contains(err.Error(), "codex binary not found") {
		t.Fatalf("error %q should mention 'codex binary not found'", err)
	}
}

func TestCodexRunner_NonZeroExitNotTimeout(t *testing.T) {
	codexStubScript(t, `exit 3`)
	wd := codexWorkdir(t, "x")
	_, err := (CodexRunner{}).Run(context.Background(), codexInput(wd))
	if err == nil {
		t.Fatal("expected error from exit 3")
	}
	if IsTimeout(err) {
		t.Fatalf("non-zero exit must not classify as timeout: %v", err)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
	}
}

func TestCodexRunner_TimeoutKillsProcess(t *testing.T) {
	codexStubScript(t, `sleep 30`)
	wd := codexWorkdir(t, "x")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := (CodexRunner{}).Run(ctx, codexInput(wd))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !IsTimeout(err) {
		t.Fatalf("expected *TimeoutError, got %T: %v", err, err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("process not killed promptly: elapsed=%v", elapsed)
	}
}
