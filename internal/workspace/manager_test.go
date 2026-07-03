package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// The RUN_SUMMARY gate (#561) and the worker's FAILURE.md / CHANGED_FILES.txt
// observability artifacts (#575) were removed because they duplicated the
// SPEC §13.1/§13.2 structured event log. Their write/reset helpers and the tests
// that pinned them were deleted with them; failure reasons are now asserted via
// the worker's structured-event tests.

func TestRunWorkspaceHookStopsOnNonZeroAndCapturesOutput(t *testing.T) {
	dir := t.TempDir()
	hook := workflow.WorkspaceHook{Commands: []string{
		"printf first",
		"printf boom && exit 7",
		"printf never > should-not-exist",
	}}

	results, err := RunWorkspaceHook(context.Background(), dir, HookBeforeRun, hook, 0, nil, workflow.Config{})
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

func TestRunWorkspaceHookRedactsCredentialedURLsFromOutput(t *testing.T) {
	const secret = "token-1032"
	dir := t.TempDir()
	hook := workflow.WorkspaceHook{Commands: []string{
		"printf '%s' 'fetch scheme://user:" + secret + "@example.com/repo.git failed'",
	}}

	results, err := RunWorkspaceHook(context.Background(), dir, HookBeforeRun, hook, 0, nil, workflow.Config{})
	if err != nil {
		t.Fatalf("RunWorkspaceHook: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	got := results[0].Output
	if strings.Contains(got, secret) || strings.Contains(got, "user:"+secret+"@") {
		t.Fatalf("hook output leaked credential: %q", got)
	}
	if !strings.Contains(got, "scheme://example.com/repo.git") {
		t.Fatalf("hook output = %q, want redacted URL host/path preserved", got)
	}
	if strings.Contains(results[0].Command, secret) || strings.Contains(results[0].Command, "user:"+secret+"@") {
		t.Fatalf("hook command leaked credential: %q", results[0].Command)
	}
	if !strings.Contains(results[0].Command, "scheme://example.com/repo.git") {
		t.Fatalf("hook command = %q, want redacted URL host/path preserved", results[0].Command)
	}
}

func TestRunWorkspaceHookRedactsBeforeOutputCap(t *testing.T) {
	const secret = "token-1032"
	leakingRawPrefix := "scheme://user:" + secret
	prefixLen := VerifyOutputCap - len(leakingRawPrefix)
	if prefixLen <= 0 {
		t.Fatalf("VerifyOutputCap = %d, want room for boundary test", VerifyOutputCap)
	}
	dir := t.TempDir()
	hook := workflow.WorkspaceHook{Commands: []string{
		fmt.Sprintf("printf '%%*s' %d '' | tr ' ' A; printf '%%s' 'scheme://user:%s@example.com/repo.git'", prefixLen, secret),
	}}

	results, err := RunWorkspaceHook(context.Background(), dir, HookBeforeRun, hook, 0, nil, workflow.Config{})
	if err != nil {
		t.Fatalf("RunWorkspaceHook: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if !results[0].Truncated {
		t.Fatalf("truncated = false, want true for boundary output")
	}
	if strings.Contains(results[0].Output, secret) || strings.Contains(results[0].Output, "user:"+secret) {
		t.Fatalf("truncated hook output leaked credential: %q", results[0].Output[len(results[0].Output)-64:])
	}
}

func TestRunWorkspaceHookRedactsIncompleteTimedOutOutput(t *testing.T) {
	const secret = "token-1032"
	dir := t.TempDir()
	hook := workflow.WorkspaceHook{Commands: []string{
		"printf '%s' 'scheme://user:" + secret + "'; sleep 2",
	}}

	results, err := RunWorkspaceHook(context.Background(), dir, HookBeforeRun, hook, 50, nil, workflow.Config{})
	if err == nil {
		t.Fatal("RunWorkspaceHook error = nil; want timeout")
	}
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if results[0].Truncated {
		t.Fatalf("truncated = true, want false for timeout-only incomplete output")
	}
	if strings.Contains(results[0].Output, secret) || strings.Contains(results[0].Output, "user:"+secret) {
		t.Fatalf("timed-out hook output leaked credential: %q", results[0].Output)
	}
}

func TestRunWorkspaceHookTimeoutStopsCommand(t *testing.T) {
	dir := t.TempDir()
	hook := workflow.WorkspaceHook{Commands: []string{"sleep 2"}}

	start := time.Now()
	results, err := RunWorkspaceHook(context.Background(), dir, HookAfterRun, hook, 50, nil, workflow.Config{})
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
	results, err := RunWorkspaceHook(context.Background(), dir, HookAfterRun, hook, 50, nil, workflow.Config{})
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

func TestRunWorkspaceHookEnvDropsNonAllowlistedSecretsByDefault(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "lin_secret_must_not_leak_xx")
	t.Setenv("GITHUB_TOKEN", "ghp_secret_must_not_leak_xx")
	dir := t.TempDir()
	hook := workflow.WorkspaceHook{Commands: []string{
		`printf '<%s><%s><%s>' "$LINEAR_API_KEY" "$GITHUB_TOKEN" "$PATH"`,
	}}

	results, err := RunWorkspaceHook(context.Background(), dir, HookBeforeRun, hook, 0, nil, workflow.Config{})
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

	results, err := RunWorkspaceHook(context.Background(), dir, HookBeforeRun, hook, 0, []string{"EXTRA_BUILD_VAR"}, workflow.Config{})
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

func TestRunWorkspaceHookEnvRejectsTrackerAPIKeyValuePassthrough(t *testing.T) {
	t.Setenv("EXTRA_BUILD_VAR", "let-me-in")
	t.Setenv("AIOPS_TRACKER_SECRET", "hook-tracker-secret")
	dir := t.TempDir()
	hook := workflow.WorkspaceHook{Commands: []string{
		`printf '<%s><%s>' "$EXTRA_BUILD_VAR" "$AIOPS_TRACKER_SECRET"`,
	}}
	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{APIKey: "hook-tracker-secret"},
	}

	results, err := RunWorkspaceHook(
		context.Background(),
		dir,
		HookBeforeRun,
		hook,
		0,
		[]string{"EXTRA_BUILD_VAR", "AIOPS_TRACKER_SECRET"},
		cfg,
	)
	if err != nil {
		t.Fatalf("RunWorkspaceHook: %v", err)
	}
	out := results[0].Output
	if !strings.Contains(out, "let-me-in") {
		t.Fatalf("EXTRA_BUILD_VAR did not pass through: %q", out)
	}
	if strings.Contains(out, "hook-tracker-secret") {
		t.Fatalf("hook env leaked configured tracker API key value: %q", out)
	}
	if !strings.Contains(out, "<let-me-in><>") {
		t.Fatalf("hook output = %q, want tracker secret slot empty", out)
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

	results, err := RunWorkspaceHook(context.Background(), dir, HookBeforeRun, hook, 0, nil, workflow.Config{})
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

	env := subprocessEnv([]string{"DEFINITELY_SET_227", "DEFINITELY_UNSET_227", "DEFINITELY_SET_227"}, workflow.Config{})

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

func TestSubprocessEnvWithLookupBoundaryTable(t *testing.T) {
	cfg := workflow.Config{
		Tracker: workflow.TrackerConfig{APIKey: "configured-tracker-secret"},
	}
	lookupValues := map[string]string{
		"PATH":                       "/worker/path",
		"HOME":                       "/home/hook",
		"USER":                       "hook-user",
		"AIOPS_ALLOWED":              "allowed-value",
		"AIOPS_DUPLICATE":            "duplicate-value",
		"LINEAR_API_KEY":             "linear-secret",
		"GITEA_TOKEN":                "gitea-secret",
		"GITHUB_TOKEN":               "github-secret",
		"AIOPS_CONFIGURED_TRACKER":   "configured-tracker-secret",
		"AIOPS_UNRELATED_TRACKERISH": "not-the-configured-secret",
	}
	env := subprocessEnvWithLookup(
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

	values, counts := workspaceEnvByName(env)
	for _, tc := range []struct {
		name      string
		wantValue string
	}{
		{name: "PATH", wantValue: "/login/path"},
		{name: "HOME", wantValue: "/home/hook"},
		{name: "USER", wantValue: "hook-user"},
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
	for _, denied := range []string{"LINEAR_API_KEY", "GITEA_TOKEN", "GITHUB_TOKEN", "AIOPS_CONFIGURED_TRACKER", "BAD"} {
		if counts[denied] != 0 {
			t.Fatalf("denied env %s appeared %d times in env %#v", denied, counts[denied], env)
		}
	}
}

func workspaceEnvByName(env []string) (map[string]string, map[string]int) {
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
