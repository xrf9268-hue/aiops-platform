package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// The RUN_SUMMARY gate and its CheckSummary/ResetRunSummary/WriteSummary/
// ReadSummary helpers were removed under #561 (the worker no longer demands an
// agent-written summary — the PR body is the record per SPEC §1). The tests that
// pinned them were deleted with them; WriteFailureSummary (the worker's own
// FAILURE.md post-mortem) is exercised via the worker failure-path tests.

func TestRunWorkspaceHookStopsOnNonZeroAndCapturesOutput(t *testing.T) {
	dir := t.TempDir()
	hook := workflow.WorkspaceHook{Commands: []string{
		"printf first",
		"printf boom && exit 7",
		"printf never > should-not-exist",
	}}

	results, err := RunWorkspaceHook(context.Background(), dir, HookBeforeRun, hook, 0, nil)
	if err == nil {
		t.Fatalf("RunWorkspaceHook should fail on non-zero exit")
	}
	var hookErr *HookError
	if !errors.As(err, &hookErr) {
		t.Fatalf("error type = %T, want *HookError", err)
	}
	if hookErr.Name != HookBeforeRun {
		t.Fatalf("hook error name = %q, want %q", hookErr.Name, HookBeforeRun)
	}
	if got, want := len(results), 2; got != want {
		t.Fatalf("results len = %d, want %d", got, want)
	}
	if results[0].Output != "first" || results[0].ExitCode != 0 || results[0].Err != nil {
		t.Fatalf("first result = %#v, want successful captured output", results[0])
	}
	if results[1].Output != "boom" || results[1].ExitCode != 7 || results[1].Err == nil {
		t.Fatalf("second result = %#v, want failing captured output with exit 7", results[1])
	}
	if _, err := os.Stat(filepath.Join(dir, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("commands after failed hook should not run; stat err=%v", err)
	}
}

func TestRunWorkspaceHookTimeoutStopsCommand(t *testing.T) {
	dir := t.TempDir()
	hook := workflow.WorkspaceHook{Commands: []string{"sleep 2"}}

	start := time.Now()
	results, err := RunWorkspaceHook(context.Background(), dir, HookAfterRun, hook, 50, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("RunWorkspaceHook should fail on timeout")
	}
	if elapsed > time.Second {
		t.Fatalf("hook timeout took %v, want under 1s", elapsed)
	}
	if got, want := len(results), 1; got != want {
		t.Fatalf("results len = %d, want %d", got, want)
	}
	if results[0].ExitCode != -1 || results[0].Err == nil {
		t.Fatalf("timeout result = %#v, want exit -1 with error", results[0])
	}
	if !strings.Contains(results[0].Err.Error(), "timed out") {
		t.Fatalf("timeout error = %v, want timed out", results[0].Err)
	}
}

func TestEffectiveWorkspaceHookTimeoutUsesSpecDefaultWhenUnset(t *testing.T) {
	if got, want := EffectiveWorkspaceHookTimeoutMs(0), 60000; got != want {
		t.Fatalf("EffectiveWorkspaceHookTimeoutMs(0) = %d, want SPEC default %d", got, want)
	}
}

func TestRunWorkspaceHookTimeoutDoesNotWaitForeverOnEscapedDescendantOutput(t *testing.T) {
	dir := t.TempDir()
	hook := workflow.WorkspaceHook{Commands: []string{"setsid sh -c 'while true; do printf x; sleep 1; done' & sleep 2"}}

	start := time.Now()
	results, err := RunWorkspaceHook(context.Background(), dir, HookAfterRun, hook, 50, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("RunWorkspaceHook should fail on timeout")
	}
	if elapsed > time.Second {
		t.Fatalf("hook timeout waited %v for escaped descendant output, want under 1s", elapsed)
	}
	if got, want := len(results), 1; got != want {
		t.Fatalf("results len = %d, want %d", got, want)
	}
	if results[0].ExitCode != -1 || results[0].Err == nil {
		t.Fatalf("timeout result = %#v, want exit -1 with error", results[0])
	}
	if !strings.Contains(results[0].Err.Error(), "timed out") {
		t.Fatalf("timeout error = %v, want timed out", results[0].Err)
	}
}

func TestWriteArtifacts(t *testing.T) {
	dir := t.TempDir()

	if err := WriteFailureSummary(dir, "failure note"); err != nil {
		t.Fatalf("WriteFailureSummary error: %v", err)
	}
	if err := WriteChangedFiles(dir, []string{"a.go", "b.go"}); err != nil {
		t.Fatalf("WriteChangedFiles error: %v", err)
	}

	for name, want := range map[string]string{
		"FAILURE.md":        "failure note",
		"CHANGED_FILES.txt": "a.go\nb.go\n",
	} {
		got, err := os.ReadFile(filepath.Join(dir, ".aiops", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if !strings.Contains(string(got), want) {
			t.Fatalf("%s = %q, want substring %q", name, got, want)
		}
	}
}

// TestResetFailureSummary verifies the helper resetStaleArtifacts calls before
// each run removes a stale .aiops/FAILURE.md and is idempotent on a missing
// file (#561 review: a failure note from a prior attempt must not survive
// workspace reuse).
func TestResetFailureSummary(t *testing.T) {
	dir := t.TempDir()
	if err := WriteFailureSummary(dir, "runner failed: boom\n"); err != nil {
		t.Fatalf("WriteFailureSummary: %v", err)
	}
	if err := ResetFailureSummary(dir); err != nil {
		t.Fatalf("ResetFailureSummary: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, FailureSummaryPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("FAILURE.md stat err = %v; want not-exist after reset", err)
	}
	// Idempotent: removing a missing file is not an error.
	if err := ResetFailureSummary(dir); err != nil {
		t.Fatalf("ResetFailureSummary (missing) = %v; want nil (idempotent)", err)
	}
}

func TestCappedBufferDropsBeyondCap(t *testing.T) {
	buf := &cappedBuffer{Cap: 10}
	n, err := buf.Write([]byte("hello "))
	if err != nil || n != 6 {
		t.Fatalf("first write n=%d err=%v", n, err)
	}
	n, err = buf.Write([]byte("world!!"))
	if err != nil || n != 7 {
		t.Fatalf("second write should report full length, n=%d err=%v", n, err)
	}
	if got := buf.String(); got != "hello worl" {
		t.Fatalf("buffered content = %q, want %q", got, "hello worl")
	}
	if !buf.Truncated() {
		t.Fatalf("buffer should report Truncated=true after exceeding cap")
	}
	if buf.Dropped() != 3 {
		t.Fatalf("dropped bytes = %d, want 3", buf.Dropped())
	}
}

func TestAllChangedFilesIncludesArtifactsWhenSnapshotAfterWrite(t *testing.T) {
	// Regression: success-path metadata under-reported pushed contents because
	// AllChangedFiles ran before .aiops artifacts were written. Ordering the
	// snapshot after WriteChangedFiles/WriteFailureSummary should make those
	// files appear in the result.
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")
	mustGit(t, dir, "config", "user.email", "test@example.com")
	mustGit(t, dir, "config", "user.name", "test")
	mustGit(t, dir, "config", "commit.gpgsign", "false")
	mustGit(t, dir, "config", "tag.gpgsign", "false")
	mustGit(t, dir, "commit", "--allow-empty", "-q", "-m", "init")

	if err := os.WriteFile(filepath.Join(dir, "src.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("write src.go: %v", err)
	}
	if err := WriteChangedFiles(dir, nil); err != nil {
		t.Fatalf("seed CHANGED_FILES.txt: %v", err)
	}
	if err := WriteFailureSummary(dir, "failure note"); err != nil {
		t.Fatalf("seed FAILURE.md: %v", err)
	}

	files, err := AllChangedFiles(context.Background(), dir)
	if err != nil {
		t.Fatalf("AllChangedFiles: %v", err)
	}
	got := strings.Join(files, ",")
	for _, want := range []string{"src.go", ".aiops/CHANGED_FILES.txt", ".aiops/FAILURE.md"} {
		if !strings.Contains(got, want) {
			t.Fatalf("AllChangedFiles missing %q in %q", want, got)
		}
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func TestRunWorkspaceHookEnvDropsNonAllowlistedSecretsByDefault(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "lin_secret_must_not_leak_xx")
	t.Setenv("GITHUB_TOKEN", "ghp_secret_must_not_leak_xx")
	dir := t.TempDir()
	hook := workflow.WorkspaceHook{Commands: []string{
		`printf '<%s><%s><%s>' "$LINEAR_API_KEY" "$GITHUB_TOKEN" "$PATH"`,
	}}

	results, err := RunWorkspaceHook(context.Background(), dir, HookBeforeRun, hook, 0, nil)
	if err != nil {
		t.Fatalf("RunWorkspaceHook: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	out := results[0].Output
	if strings.Contains(out, "lin_secret_must_not_leak_xx") {
		t.Fatalf("LINEAR_API_KEY leaked into hook env: %q", out)
	}
	if strings.Contains(out, "ghp_secret_must_not_leak_xx") {
		t.Fatalf("GITHUB_TOKEN leaked into hook env: %q", out)
	}
	if !strings.Contains(out, "<><>") {
		t.Fatalf("hook output = %q, want empty secret slots <><>", out)
	}
	// PATH should still flow through the baseline allowlist so the shell
	// can resolve `sh`, `printf`, etc.
	if strings.Contains(out, "<><><>") {
		t.Fatalf("PATH unexpectedly empty in hook env: %q", out)
	}
}

func TestRunWorkspaceHookEnvPassthroughLetsNamedVarThrough(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "lin_secret_must_not_leak_yy")
	t.Setenv("EXTRA_BUILD_VAR", "let-me-in")
	dir := t.TempDir()
	hook := workflow.WorkspaceHook{Commands: []string{
		`printf '<%s><%s>' "$LINEAR_API_KEY" "$EXTRA_BUILD_VAR"`,
	}}

	results, err := RunWorkspaceHook(context.Background(), dir, HookBeforeRun, hook, 0, []string{"EXTRA_BUILD_VAR"})
	if err != nil {
		t.Fatalf("RunWorkspaceHook: %v", err)
	}
	out := results[0].Output
	if strings.Contains(out, "lin_secret_must_not_leak_yy") {
		t.Fatalf("LINEAR_API_KEY leaked despite not being in passthrough: %q", out)
	}
	if !strings.Contains(out, "let-me-in") {
		t.Fatalf("EXTRA_BUILD_VAR did not pass through: %q", out)
	}
}

// TestRunWorkspaceHookDoesNotSourceUserLoginProfile is a regression for
// #314: the hook runner used `sh -lc`, which under dash re-sources
// /etc/profile.d/* per command and leaks any stdout those scripts emit into
// HookResult.Output. The fix runs hooks under `sh -c`, so a $HOME/.profile
// that prints to stdout (a portable stand-in for /etc/profile.d/nvm.sh)
// must no longer contaminate the captured hook output.
func TestRunWorkspaceHookDoesNotSourceUserLoginProfile(t *testing.T) {
	home := t.TempDir()
	profilePath := filepath.Join(home, ".profile")
	if err := os.WriteFile(profilePath, []byte("printf 'LEAKED_FROM_PROFILE\\n'\n"), 0o644); err != nil {
		t.Fatalf("seed .profile: %v", err)
	}
	t.Setenv("HOME", home)

	dir := t.TempDir()
	hook := workflow.WorkspaceHook{Commands: []string{`printf clean`}}

	results, err := RunWorkspaceHook(context.Background(), dir, HookBeforeRun, hook, 0, nil)
	if err != nil {
		t.Fatalf("RunWorkspaceHook: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].Output != "clean" {
		t.Fatalf("hook output = %q, want %q (login-profile stdout leaked into hook output buffer)", results[0].Output, "clean")
	}
}

func TestSubprocessEnvSkipsUnsetPassthroughNames(t *testing.T) {
	t.Setenv("DEFINITELY_SET_227", "v1")
	if err := os.Unsetenv("DEFINITELY_UNSET_227"); err != nil {
		t.Fatalf("Unsetenv(%q) = %v; want nil", "DEFINITELY_UNSET_227", err)
	}

	env := subprocessEnv([]string{"DEFINITELY_SET_227", "DEFINITELY_UNSET_227", "DEFINITELY_SET_227"})

	var sawSet, sawUnsetMarker int
	for _, kv := range env {
		if strings.HasPrefix(kv, "DEFINITELY_SET_227=") {
			sawSet++
		}
		if strings.HasPrefix(kv, "DEFINITELY_UNSET_227") {
			sawUnsetMarker++
		}
	}
	if sawSet != 1 {
		t.Fatalf("DEFINITELY_SET_227 appeared %d times, want 1 (dedup)", sawSet)
	}
	if sawUnsetMarker != 0 {
		t.Fatalf("unset passthrough name leaked into env: %v", env)
	}
}
