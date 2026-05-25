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
	path := dir + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", path)
	useAgentLoginPATH(t, path)
	return dir
}

func useAgentLoginPATH(t *testing.T, path string) {
	t.Helper()
	old := agentLoginPATH
	agentLoginPATH = func() string { return path }
	t.Cleanup(func() {
		agentLoginPATH = old
	})
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
			Workspace: workflow.WorkspaceConfig{Root: filepath.Dir(workdir)},
			Codex:     workflow.CommandConfig{Command: "codex exec", Profile: "safe"},
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
		"--sandbox",
		"workspace-write",
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
	// Per #330, the safe profile MUST use `--sandbox workspace-write`
	// rather than the deprecated `--full-auto` shorthand. Codex commit
	// 3d10ba9f3 (openai/codex#20133) flagged `--full-auto` as deprecated
	// in `codex exec`, prints a warning on every invocation, and codex-cli
	// b7dba72db removed it at the top level entirely. Pin the new
	// spelling here so a future "let's revert this one-liner" can't pass.
	for _, line := range gotLines {
		if line == "--full-auto" {
			t.Fatalf("safe profile must not emit --full-auto; codex deprecated it (see #330): argv=%q", gotLines)
		}
	}
}

func TestBuildCodexCmdResolvesCodexFromAgentEnvPATH(t *testing.T) {
	rawPathDir := t.TempDir()
	t.Setenv("PATH", rawPathDir)
	binDir := t.TempDir()
	codex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codex, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	wd := codexWorkdir(t, "x")

	cmd, err := buildCodexCmd(context.Background(), codexInput(wd), []string{"PATH=" + binDir})
	if err != nil {
		t.Fatalf("buildCodexCmd: %v", err)
	}
	if cmd.Path != codex {
		t.Fatalf("cmd.Path = %q, want codex from agent env PATH %q", cmd.Path, codex)
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
	outPath := filepath.Join(wd, ".aiops/CODEX_OUTPUT.txt")
	if err := os.WriteFile(outPath, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := (CodexRunner{}).Run(context.Background(), codexInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	out, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read CODEX_OUTPUT.txt: %v", err)
	}
	assertSensitiveArtifactPerm(t, outPath)
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
	assertSensitiveArtifactPerm(t, filepath.Join(wd, ".aiops/CODEX_LAST_MESSAGE.md"))
}

func TestCodexRunnerRejectsSymlinkedLastMessageArtifact(t *testing.T) {
	binDir := codexStubScript(t, `printf 'should not run\n' > "$8" ; exit 0`)
	wd := codexWorkdir(t, "x")
	lastMessage := filepath.Join(wd, ".aiops/CODEX_LAST_MESSAGE.md")
	target := filepath.Join(wd, "LAST.md")
	if err := os.Symlink("../LAST.md", lastMessage); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	_, err := (CodexRunner{}).Run(context.Background(), codexInput(wd))
	if err == nil || !strings.Contains(err.Error(), "prepare "+CodexLastMessagePath) {
		t.Fatalf("Run error = %v, want last-message prepare failure", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("symlinked last-message wrote outside .aiops: %v", err)
	}
	if _, err := os.Stat(filepath.Join(binDir, "argv.txt")); !os.IsNotExist(err) {
		t.Fatalf("codex command ran despite unsafe last-message path: %v", err)
	}
}

func TestCodexRunnerReplacesHardLinkedLastMessageArtifact(t *testing.T) {
	codexStubScript(t, `printf 'codex hardlink-safe\n' > "$8" ; exit 0`)
	wd := codexWorkdir(t, "x")
	lastMessage := filepath.Join(wd, ".aiops/CODEX_LAST_MESSAGE.md")
	target := filepath.Join(wd, "LAST.md")
	if err := os.WriteFile(target, []byte("public\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(target, lastMessage); err != nil {
		t.Skipf("hardlink unavailable: %v", err)
	}

	res, err := (CodexRunner{}).Run(context.Background(), codexInput(wd))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Summary != "codex hardlink-safe" {
		t.Fatalf("Summary = %q, want hardlink-safe output", res.Summary)
	}
	if body, err := os.ReadFile(target); err != nil {
		t.Fatal(err)
	} else if string(body) != "public\n" {
		t.Fatalf("hardlinked public target was modified: %q", body)
	}
	assertSensitiveArtifactPerm(t, lastMessage)
}

func assertSensitiveArtifactPerm(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s mode = %#o, want %#o", path, got, os.FileMode(0o600))
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
	emptyPath := t.TempDir()
	t.Setenv("PATH", emptyPath) // no codex anywhere
	useAgentLoginPATH(t, emptyPath)
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

func TestCodexRunner_CustomProfileUsesShellWithStdin(t *testing.T) {
	wd := codexWorkdir(t, "custom-profile-canary")
	in := RunInput{
		Task: task.Task{ID: "tsk_custom", Model: "codex"},
		Workflow: workflow.Workflow{Config: workflow.Config{
			Workspace: workflow.WorkspaceConfig{Root: filepath.Dir(wd)},
			Codex:     workflow.CommandConfig{Command: "cat", Profile: "custom"},
		}},
		Workdir: wd,
	}
	res, err := (CodexRunner{}).Run(context.Background(), in)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(wd, ".aiops/CODEX_OUTPUT.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "custom-profile-canary") {
		t.Fatalf("artifact missing canary; got %q", body)
	}
	if res.OutputBytes <= 0 {
		t.Fatalf("OutputBytes = %d, want > 0", res.OutputBytes)
	}
	// Custom profile does NOT write CODEX_LAST_MESSAGE.md (no -o flag);
	// summary should fall back.
	if res.Summary != "codex completed" {
		t.Fatalf("Summary = %q, want default fallback", res.Summary)
	}
}

func TestCodexRunner_CustomProfileDoesNotSourceProfile(t *testing.T) {
	wd := codexWorkdir(t, "custom-profile-canary")
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".profile"), []byte("export PROFILE_CANARY=from-profile\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	in := RunInput{
		Task: task.Task{ID: "tsk_custom_profile", Model: "codex"},
		Workflow: workflow.Workflow{Config: workflow.Config{
			Workspace: workflow.WorkspaceConfig{Root: filepath.Dir(wd)},
			Codex:     workflow.CommandConfig{Command: `printf '%s' "${PROFILE_CANARY:-}"`, Profile: "custom"},
		}},
		Workdir: wd,
	}
	if _, err := (CodexRunner{}).Run(context.Background(), in); err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(wd, ".aiops/CODEX_OUTPUT.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(body); got != "" {
		t.Fatalf("custom codex profile sourced HOME profile; canary=%q", got)
	}
}

func TestCodexRunner_CustomProfileEmptyCommandRejected(t *testing.T) {
	wd := codexWorkdir(t, "x")
	in := RunInput{
		Task: task.Task{ID: "tsk_custom_empty", Model: "codex"},
		Workflow: workflow.Workflow{Config: workflow.Config{
			Workspace: workflow.WorkspaceConfig{Root: filepath.Dir(wd)},
			Codex:     workflow.CommandConfig{Profile: "custom"},
		}},
		Workdir: wd,
	}
	_, err := (CodexRunner{}).Run(context.Background(), in)
	if err == nil {
		t.Fatal("expected error for custom profile with empty command")
	}
	if !strings.Contains(err.Error(), "codex.command") {
		t.Fatalf("error %q should mention codex.command", err)
	}
}

func TestCodexRunnerDoesNotInheritWorkerSecretsByDefault(t *testing.T) {
	codexStubScript(t, `env > env.txt ; exit 0`)
	wd := codexWorkdir(t, "x")
	t.Setenv("LINEAR_API_KEY", "linear-secret")
	t.Setenv("GITEA_TOKEN", "gitea-secret")
	t.Setenv("GITHUB_TOKEN", "github-secret")

	if _, err := (CodexRunner{}).Run(context.Background(), codexInput(wd)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(wd, "env.txt"))
	if err != nil {
		t.Fatalf("read env.txt: %v", err)
	}
	for _, secretName := range []string{"LINEAR_API_KEY", "GITEA_TOKEN", "GITHUB_TOKEN"} {
		if strings.Contains(string(body), secretName+"=") {
			t.Fatalf("codex env leaked %s:\n%s", secretName, body)
		}
	}
	if !strings.Contains(string(body), "PATH=") {
		t.Fatalf("codex env lost baseline PATH:\n%s", body)
	}
}
