package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// TestNew_RemovedCodexExecRunnerIsUnknown pins the #541 removal: the
// non-SPEC `codex` (one-shot `codex exec`) runner is no longer in the
// registry, while the SPEC §10 `codex-app-server` runner still resolves.
func TestNew_RemovedCodexExecRunnerIsUnknown(t *testing.T) {
	if _, err := New("codex"); err == nil {
		t.Fatalf("New(%q) = nil error; want unknown-runner error after #541 removal", "codex")
	} else if !strings.Contains(err.Error(), "unknown runner") {
		t.Fatalf("New(%q) error = %q; want it to mention %q", "codex", err, "unknown runner")
	}

	r, err := New(NameCodexAppServer)
	if err != nil {
		t.Fatalf("New(%q) = %v; want the SPEC §10 app-server runner", NameCodexAppServer, err)
	}
	if _, ok := r.(CodexAppServerRunner); !ok {
		t.Fatalf("New(%q) = %T; want CodexAppServerRunner", NameCodexAppServer, r)
	}
}

// shellTestWorkdir creates a temp workdir with a stub .aiops/PROMPT.md so the
// ShellRunner can open it for the child's stdin before the actual command runs
// (we care about the kill path here, not the prompt content).
func shellTestWorkdir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".aiops"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aiops", "PROMPT.md"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestMockRunnerTimeoutReturnsTimeoutError verifies that when the mock
// runner is asked to sleep longer than the parent context's deadline,
// it returns *TimeoutError (not a generic ctx.Err()) so worker retry
// policy can route it to the timeout-specific bucket.
func TestMockRunnerTimeoutReturnsTimeoutError(t *testing.T) {
	t.Parallel()
	r := MockRunner{Sleep: 5 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := r.Run(ctx, RunInput{
		Task:     task.Task{ID: "tsk_test", Model: "mock"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from timed-out runner, got nil")
	}
	if !IsTimeout(err) {
		t.Fatalf("expected TimeoutError, got %T: %v", err, err)
	}
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("errors.As to *TimeoutError failed: %v", err)
	}
	if te.Elapsed <= 0 {
		t.Fatalf("expected non-zero elapsed, got %v", te.Elapsed)
	}
	// We should have returned promptly when ctx fired, well before the
	// 5s sleep would have completed naturally.
	if elapsed >= 2*time.Second {
		t.Fatalf("runner did not honor ctx cancellation; elapsed=%v", elapsed)
	}
}

// TestMockRunnerNoTimeoutWhenSleepShort confirms the happy path: with
// adequate budget the mock runner returns Result without a TimeoutError.
func TestMockRunnerNoTimeoutWhenSleepShort(t *testing.T) {
	t.Parallel()
	r := MockRunner{Sleep: 5 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res, err := r.Run(ctx, RunInput{
		Task:     task.Task{ID: "tsk_ok", Model: "mock"},
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Summary == "" {
		t.Fatal("expected non-empty Result.Summary on success")
	}
	if IsTimeout(err) {
		t.Fatal("IsTimeout should be false for nil error")
	}
}

// TestShellRunnerDeliversPromptOnStdin asserts the runner hands PROMPT.md to the
// child on stdin via cmd.Stdin, not by splicing `< .aiops/PROMPT.md` onto the
// command string. The command carries a trailing comment: the old
// string-concatenation would have appended the redirection *after* the `#`,
// swallowing it so the child saw empty stdin. Mutation-verify by deleting the
// `cmd.Stdin = prompt` assignment in shell.go — the capture file goes empty.
func TestShellRunnerDeliversPromptOnStdin(t *testing.T) {
	t.Parallel()
	workdir := shellTestWorkdir(t)
	prompt := "prompt body line 1\nprompt body line 2 # keep me literal\n"
	if err := os.WriteFile(filepath.Join(workdir, ".aiops", "PROMPT.md"), []byte(prompt), 0o644); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	capture := filepath.Join(t.TempDir(), "stdin-capture")
	wf := workflow.Workflow{Config: workflow.Config{
		Workspace: workflow.WorkspaceConfig{Root: filepath.Dir(workdir)},
		Claude:    workflow.CommandConfig{Command: "cat > " + capture + " # deliver prompt"},
	}}

	if _, err := (ShellRunner{Name: "claude"}).Run(context.Background(), RunInput{
		Task:     task.Task{ID: "tsk_stdin"},
		Workflow: wf,
		Workdir:  workdir,
	}); err != nil {
		t.Fatalf("Run(claude) error = %v; want nil", err)
	}

	got, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("read stdin capture: %v", err)
	}
	if string(got) != prompt {
		t.Fatalf("child stdin = %q; want %q", string(got), prompt)
	}
}

// TestShellRunnerFailsFastWhenPromptMissing asserts the runner surfaces a clear
// open error and never launches the command when .aiops/PROMPT.md is missing.
// PROMPT.md is opened before applySandbox so this failure path cannot leak the
// firejail netfilter temp file applySandbox would allocate (codex review #972);
// failing before cmd.Run is the observable contract that ordering preserves.
func TestShellRunnerFailsFastWhenPromptMissing(t *testing.T) {
	t.Parallel()
	workdir := t.TempDir() // deliberately no .aiops/PROMPT.md
	sentinel := filepath.Join(t.TempDir(), "ran")
	wf := workflow.Workflow{Config: workflow.Config{
		Workspace: workflow.WorkspaceConfig{Root: filepath.Dir(workdir)},
		Claude:    workflow.CommandConfig{Command: "touch " + sentinel},
	}}

	_, err := (ShellRunner{Name: "claude"}).Run(context.Background(), RunInput{
		Task:     task.Task{ID: "tsk_noprompt"},
		Workflow: wf,
		Workdir:  workdir,
	})
	if err == nil {
		t.Fatal("Run(claude) with missing PROMPT.md = nil error; want open failure")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Run(claude) error = %v; want errors.Is(err, os.ErrNotExist)", err)
	}
	if IsTimeout(err) {
		t.Fatalf("Run(claude) error = %v; want a non-timeout open failure", err)
	}
	if _, statErr := os.Stat(sentinel); statErr == nil {
		t.Fatal("command executed despite missing PROMPT.md; runner must fail before cmd.Run")
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("stat sentinel: %v", statErr)
	}
}

// TestShellRunnerKillsRunawayProcess wires the real ShellRunner against
// a `sleep 30` command and asserts that a 50ms timeout actually kills
// the subprocess (i.e. ctx-driven SIGTERM/SIGKILL works end-to-end). The
// guard `time.Since(start) < 5s` would fail loudly if the kill path
// regressed and we waited the full sleep budget.
func TestShellRunnerKillsRunawayProcess(t *testing.T) {
	t.Parallel()
	workdir := shellTestWorkdir(t)
	wf := workflow.Workflow{Config: workflow.Config{
		Workspace: workflow.WorkspaceConfig{Root: filepath.Dir(workdir)},
		Claude:    workflow.CommandConfig{Command: "sleep 30"},
	}}
	r := ShellRunner{Name: "claude"}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := r.Run(ctx, RunInput{
		Task:     task.Task{ID: "tsk_shell"},
		Workflow: wf,
		Workdir:  workdir,
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from killed sh subprocess")
	}
	if !IsTimeout(err) {
		t.Fatalf("expected TimeoutError from shell runner, got %T: %v", err, err)
	}
	// Even with the SIGTERM->SIGKILL grace (5s) the wait must complete
	// well before sleep 30s would have. Allow generous slack for CI.
	if elapsed > 10*time.Second {
		t.Fatalf("shell runner did not kill subprocess promptly; elapsed=%v", elapsed)
	}
}

// TestShellRunnerNonTimeoutErrorNotMisclassified guarantees a runner
// that exits non-zero quickly (no ctx expiry) is *not* tagged as a
// TimeoutError — verify-vs-timeout retry routing depends on this.
func TestShellRunnerNonTimeoutErrorNotMisclassified(t *testing.T) {
	t.Parallel()
	workdir := shellTestWorkdir(t)
	wf := workflow.Workflow{Config: workflow.Config{
		Workspace: workflow.WorkspaceConfig{Root: filepath.Dir(workdir)},
		Claude:    workflow.CommandConfig{Command: "exit 3"},
	}}
	r := ShellRunner{Name: "claude"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := r.Run(ctx, RunInput{
		Task:     task.Task{ID: "tsk_nonzero"},
		Workflow: wf,
		Workdir:  workdir,
	})
	if err == nil {
		t.Fatal("expected non-zero exit error")
	}
	if IsTimeout(err) {
		t.Fatalf("non-zero exit must not be classified as timeout: %v", err)
	}
}

func TestShellRunnerDoesNotInheritWorkerSecretsByDefault(t *testing.T) {
	workdir := shellTestWorkdir(t)
	t.Setenv("LINEAR_API_KEY", "linear-secret")
	t.Setenv("GITEA_TOKEN", "gitea-secret")
	t.Setenv("GITHUB_TOKEN", "github-secret")
	t.Setenv("CODEX_HOME", filepath.Join(workdir, "codex-home"))
	t.Setenv("TMPDIR", filepath.Join(workdir, "worker-tmp"))

	wf := workflow.Workflow{Config: workflow.Config{
		Workspace: workflow.WorkspaceConfig{Root: filepath.Dir(workdir)},
		Claude: workflow.CommandConfig{
			Command: "env > shell-env.txt",
		},
	}}

	if _, err := (ShellRunner{Name: "claude"}).Run(context.Background(), RunInput{
		Task:     task.Task{ID: "tsk_shell_env"},
		Workflow: wf,
		Workdir:  workdir,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(workdir, "shell-env.txt"))
	if err != nil {
		t.Fatalf("read shell-env.txt: %v", err)
	}
	for _, secretName := range []string{"LINEAR_API_KEY", "GITEA_TOKEN", "GITHUB_TOKEN"} {
		if strings.Contains(string(body), secretName+"=") {
			t.Fatalf("runner env leaked %s:\n%s", secretName, body)
		}
	}
	if strings.Contains(string(body), "CODEX_HOME=") {
		t.Fatalf("shell runner inherited Codex credential home by default:\n%s", body)
	}
	if strings.Contains(string(body), "TMPDIR=") {
		t.Fatalf("shell runner inherited Codex temporary root by default:\n%s", body)
	}
	if !strings.Contains(string(body), "PATH=") {
		t.Fatalf("runner env lost baseline PATH:\n%s", body)
	}
}

func TestShellRunnerDoesNotSourceProfileByDefault(t *testing.T) {
	workdir := shellTestWorkdir(t)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".profile"), []byte("export PROFILE_CANARY=from-profile\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)

	wf := workflow.Workflow{Config: workflow.Config{
		Workspace: workflow.WorkspaceConfig{Root: filepath.Dir(workdir)},
		Claude: workflow.CommandConfig{
			Command: `printf '%s' "${PROFILE_CANARY:-}" > profile-canary.txt`,
		},
	}}

	if _, err := (ShellRunner{Name: "claude"}).Run(context.Background(), RunInput{
		Task:     task.Task{ID: "tsk_shell_profile"},
		Workflow: wf,
		Workdir:  workdir,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(workdir, "profile-canary.txt"))
	if err != nil {
		t.Fatalf("read profile-canary.txt: %v", err)
	}
	if got := string(body); got != "" {
		t.Fatalf("shell runner sourced HOME profile; canary=%q", got)
	}
}

func TestShellRunnerHonorsExplicitEnvPassthrough(t *testing.T) {
	workdir := shellTestWorkdir(t)
	t.Setenv("AIOPS_RUNNER_CANARY", "allowed-value")
	t.Setenv("LINEAR_API_KEY", "linear-secret")

	wf := workflow.Workflow{Config: workflow.Config{
		Workspace: workflow.WorkspaceConfig{Root: filepath.Dir(workdir)},
		Claude: workflow.CommandConfig{
			Command:        "env > shell-env.txt",
			EnvPassthrough: []string{"AIOPS_RUNNER_CANARY"},
		},
	}}

	if _, err := (ShellRunner{Name: "claude"}).Run(context.Background(), RunInput{
		Task:     task.Task{ID: "tsk_shell_env_allow"},
		Workflow: wf,
		Workdir:  workdir,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(workdir, "shell-env.txt"))
	if err != nil {
		t.Fatalf("read shell-env.txt: %v", err)
	}
	if !strings.Contains(string(body), "AIOPS_RUNNER_CANARY=allowed-value") {
		t.Fatalf("runner env missing explicit passthrough:\n%s", body)
	}
	if strings.Contains(string(body), "LINEAR_API_KEY=") {
		t.Fatalf("runner env leaked token outside explicit passthrough:\n%s", body)
	}
}

func TestShellRunnerRejectsTrackerAPIKeyValuePassthrough(t *testing.T) {
	workdir := shellTestWorkdir(t)
	t.Setenv("AIOPS_RUNNER_CANARY", "allowed-value")
	t.Setenv("AIOPS_TRACKER_SECRET", "tracker-secret")

	wf := workflow.Workflow{Config: workflow.Config{
		Workspace: workflow.WorkspaceConfig{Root: filepath.Dir(workdir)},
		Tracker:   workflow.TrackerConfig{APIKey: "tracker-secret"},
		Claude: workflow.CommandConfig{
			Command:        "env > shell-env.txt",
			EnvPassthrough: []string{"AIOPS_RUNNER_CANARY", "AIOPS_TRACKER_SECRET"},
		},
	}}

	if _, err := (ShellRunner{Name: "claude"}).Run(context.Background(), RunInput{
		Task:     task.Task{ID: "tsk_shell_env_tracker_deny"},
		Workflow: wf,
		Workdir:  workdir,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(workdir, "shell-env.txt"))
	if err != nil {
		t.Fatalf("read shell-env.txt: %v", err)
	}
	if !strings.Contains(string(body), "AIOPS_RUNNER_CANARY=allowed-value") {
		t.Fatalf("runner env missing explicit passthrough:\n%s", body)
	}
	if strings.Contains(string(body), "AIOPS_TRACKER_SECRET=") {
		t.Fatalf("runner env leaked configured tracker API key value:\n%s", body)
	}
}

func TestAgentEnvRejectsTrackerTokenPassthrough(t *testing.T) {
	t.Setenv("AIOPS_RUNNER_CANARY", "allowed-value")
	t.Setenv("LINEAR_API_KEY", "linear-secret")
	t.Setenv("GITEA_TOKEN", "gitea-secret")
	t.Setenv("GITHUB_TOKEN", "github-secret")

	body := strings.Join(agentEnv([]string{"AIOPS_RUNNER_CANARY", "LINEAR_API_KEY", "GITEA_TOKEN", "GITHUB_TOKEN"}, workflow.Config{}), "\n")
	if !strings.Contains(body, "AIOPS_RUNNER_CANARY=allowed-value") {
		t.Fatalf("agent env missing non-secret passthrough:\n%s", body)
	}
	for _, secretName := range []string{"LINEAR_API_KEY", "GITEA_TOKEN", "GITHUB_TOKEN"} {
		if strings.Contains(body, secretName+"=") {
			t.Fatalf("agent env leaked denied passthrough %s:\n%s", secretName, body)
		}
	}
}

func TestAgentEnvRejectsTrackerAPIKeyValuePassthrough(t *testing.T) {
	t.Setenv("AIOPS_RUNNER_CANARY", "allowed-value")
	t.Setenv("AIOPS_TEST_TRACKER_TOKEN", "tracker-secret")

	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{APIKey: "tracker-secret"},
	}
	body := strings.Join(agentEnv([]string{"AIOPS_RUNNER_CANARY", "AIOPS_TEST_TRACKER_TOKEN"}, cfg), "\n")
	if !strings.Contains(body, "AIOPS_RUNNER_CANARY=allowed-value") {
		t.Fatalf("agent env missing non-secret passthrough:\n%s", body)
	}
	if strings.Contains(body, "AIOPS_TEST_TRACKER_TOKEN=") {
		t.Fatalf("agent env leaked tracker API key value:\n%s", body)
	}
}

func TestAgentEnvUsesLoginShellPATHSnapshot(t *testing.T) {
	env := agentEnvWithLookup(
		nil,
		workflow.Config{},
		func(name string) (string, bool) {
			if name == "PATH" {
				return "/worker/path", true
			}
			return "", false
		},
		func() string { return "/login/path" },
	)
	body := strings.Join(env, "\n")
	if !strings.Contains(body, "PATH=/login/path") {
		t.Fatalf("agent env did not use login-shell PATH snapshot:\n%s", body)
	}
	if strings.Contains(body, "PATH=/worker/path") {
		t.Fatalf("agent env used raw worker PATH instead of login-shell snapshot:\n%s", body)
	}
}

func TestAgentEnvForPreflightScopesCodexHomeToCodexAppServer(t *testing.T) {
	codexHome := filepath.Join(t.TempDir(), "codex-home")
	t.Setenv("CODEX_HOME", codexHome)

	cfg := workflow.Config{}
	codexEnv := strings.Join(AgentEnvForPreflight(NameCodexAppServer, cfg), "\n")
	if !strings.Contains(codexEnv, "CODEX_HOME="+codexHome) {
		t.Fatalf("codex preflight env missing CODEX_HOME=%q:\n%s", codexHome, codexEnv)
	}

	claudeEnv := strings.Join(AgentEnvForPreflight("claude", cfg), "\n")
	if strings.Contains(claudeEnv, "CODEX_HOME=") {
		t.Fatalf("claude preflight env inherited Codex credential home:\n%s", claudeEnv)
	}

	genericEnv := strings.Join(AgentEnvForPreflight("mock", cfg), "\n")
	if strings.Contains(genericEnv, "CODEX_HOME=") {
		t.Fatalf("generic preflight env inherited Codex credential home:\n%s", genericEnv)
	}
}

func TestAgentEnvWithLookupBoundaryTable(t *testing.T) {
	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{APIKey: "configured-tracker-secret"},
	}
	lookupValues := map[string]string{
		"PATH":                       "/worker/path",
		"HOME":                       "/home/agent",
		"CODEX_HOME":                 "/home/agent/.codex-alt",
		"USER":                       "agent-user",
		"AIOPS_ALLOWED":              "allowed-value",
		"AIOPS_DUPLICATE":            "duplicate-value",
		"LINEAR_API_KEY":             "linear-secret",
		"GITEA_TOKEN":                "gitea-secret",
		"GITHUB_TOKEN":               "github-secret",
		"AIOPS_CONFIGURED_TRACKER":   "configured-tracker-secret",
		"AIOPS_UNRELATED_TRACKERISH": "not-the-configured-secret",
	}
	env := agentEnvWithLookup(
		[]string{
			"AIOPS_ALLOWED",
			"LINEAR_API_KEY",
			"GITEA_TOKEN",
			"GITHUB_TOKEN",
			"AIOPS_CONFIGURED_TRACKER",
			"AIOPS_UNRELATED_TRACKERISH",
			"AIOPS_DUPLICATE",
			"AIOPS_DUPLICATE",
			"BAD=NAME",
			"",
			"PATH",
		},
		cfg,
		func(name string) (string, bool) {
			value, ok := lookupValues[name]
			return value, ok
		},
		func() string { return "/login/path" },
	)

	values, counts := envByName(env)
	for _, tc := range []struct {
		name      string
		wantValue string
	}{
		{name: "PATH", wantValue: "/login/path"},
		{name: "HOME", wantValue: "/home/agent"},
		{name: "USER", wantValue: "agent-user"},
		{name: "AIOPS_ALLOWED", wantValue: "allowed-value"},
		{name: "AIOPS_DUPLICATE", wantValue: "duplicate-value"},
		{name: "AIOPS_UNRELATED_TRACKERISH", wantValue: "not-the-configured-secret"},
	} {
		if values[tc.name] != tc.wantValue {
			t.Fatalf("%s = %q, want %q in env %#v", tc.name, values[tc.name], tc.wantValue, env)
		}
		if counts[tc.name] != 1 {
			t.Fatalf("%s appeared %d times, want 1 in env %#v", tc.name, counts[tc.name], env)
		}
	}
	if counts["CODEX_HOME"] != 0 {
		t.Fatalf("generic agent env included CODEX_HOME %d times; want 0 in env %#v", counts["CODEX_HOME"], env)
	}
	for _, denied := range []string{"LINEAR_API_KEY", "GITEA_TOKEN", "GITHUB_TOKEN", "AIOPS_CONFIGURED_TRACKER", "BAD"} {
		if counts[denied] != 0 {
			t.Fatalf("denied env %s appeared %d times in env %#v", denied, counts[denied], env)
		}
	}
}

func envByName(env []string) (map[string]string, map[string]int) {
	values := make(map[string]string, len(env))
	counts := make(map[string]int, len(env))
	for _, pair := range env {
		name, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		values[name] = value
		counts[name]++
	}
	return values, counts
}

func TestIsTimeoutNilAndOther(t *testing.T) {
	t.Parallel()
	if IsTimeout(nil) {
		t.Fatal("IsTimeout(nil) should be false")
	}
	if IsTimeout(errors.New("boom")) {
		t.Fatal("IsTimeout on plain error should be false")
	}
	te := &TimeoutError{Timeout: time.Second, Elapsed: time.Second, Cause: errors.New("x")}
	if !IsTimeout(te) {
		t.Fatal("IsTimeout on *TimeoutError should be true")
	}
	wrapped := errors.Join(errors.New("ctx"), te)
	if !IsTimeout(wrapped) {
		t.Fatal("IsTimeout should unwrap joined errors")
	}
}

// mockRunTask is a fixed task used by the mock-runner cancellation test so the
// asserted file names and template substitutions stay stable.
func mockRunTask() task.Task {
	return task.Task{ID: "tsk_chr", Title: "Characterize", Actor: "actor-x", Model: "model-y"}
}

// TestMockRunnerManualCancelReturnsBareContextError pins the non-deadline
// cancellation path: when the context is cancelled (not deadline-exceeded)
// while the runner is sleeping, Run returns the bare ctx.Err()
// (context.Canceled) rather than a *TimeoutError. The pre-#521 suite only
// pinned the DeadlineExceeded branch.
func TestMockRunnerManualCancelReturnsBareContextError(t *testing.T) {
	t.Parallel()
	r := MockRunner{Sleep: 5 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel shortly after Run begins sleeping so the select observes
	// ctx.Done() with context.Canceled (no deadline on this context).
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	_, err := r.Run(ctx, RunInput{
		Task:     mockRunTask(),
		Workflow: workflow.Workflow{},
		Workdir:  t.TempDir(),
	})
	if err == nil {
		t.Fatal("Run(manual-cancel) error = nil; want context.Canceled")
	}
	if IsTimeout(err) {
		t.Fatalf("Run(manual-cancel) IsTimeout = true (%T); want bare context error", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run(manual-cancel) error = %v; want errors.Is(context.Canceled)", err)
	}
}
