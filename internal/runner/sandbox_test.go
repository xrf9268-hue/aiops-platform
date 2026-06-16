package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/task"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func sandboxInput(t *testing.T, workdir string, cfg workflow.SandboxConfig) RunInput {
	t.Helper()
	root := filepath.Dir(workdir)
	return RunInput{
		Task: task.Task{ID: "tsk_sandbox", Model: "codex"},
		Workflow: workflow.Workflow{Config: workflow.Config{
			Workspace: workflow.WorkspaceConfig{Root: root},
			Sandbox:   cfg,
		}},
		Workdir: workdir,
		Prompt:  "ignored",
	}
}

func requireLinuxSandboxHost(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("sandbox wrapper construction is Linux-only; current host OS is %s", runtime.GOOS)
	}
}

func TestSandboxDisabledLeavesCommandUnwrappedAndEnvironmentInherited(t *testing.T) {
	base := exec.CommandContext(context.Background(), "codex", "exec")
	base.Dir = t.TempDir()
	base.Env = nil

	wrapped, err := applySandbox(context.Background(), sandboxInput(t, base.Dir, workflow.SandboxConfig{}), base)
	if err != nil {
		t.Fatalf("applySandbox: %v", err)
	}
	if wrapped != base {
		t.Fatalf("disabled sandbox should return original command")
	}
	if wrapped.Env != nil {
		t.Fatalf("disabled sandbox should not scope environment, got %q", wrapped.Env)
	}
}

func TestSandboxBubblewrapBuildsWrappedCommandAndScopesEnvironment(t *testing.T) {
	requireLinuxSandboxHost(t)
	binDir := t.TempDir()
	bwrap := filepath.Join(binDir, "bwrap")
	if err := os.WriteFile(bwrap, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codex, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITHUB_TOKEN", "must-not-leak")
	t.Setenv("AIOPS_RUN_TOKEN", "allowed-secret")

	workdir := t.TempDir()
	base := exec.CommandContext(context.Background(), "codex", "exec", "--cd", workdir)
	base.Dir = workdir

	wrapped, err := applySandbox(context.Background(), sandboxInput(t, workdir, workflow.SandboxConfig{
		Enabled:      true,
		Backend:      "bubblewrap",
		NetworkMode:  "none",
		EnvAllowlist: []string{"AIOPS_RUN_TOKEN", "GITHUB_TOKEN"},
	}), base)
	if err != nil {
		t.Fatalf("applySandbox: %v", err)
	}
	if filepath.Base(wrapped.Path) != "bwrap" {
		t.Fatalf("wrapped.Path = %q, want bwrap", wrapped.Path)
	}
	joined := strings.Join(wrapped.Args, "\x00")
	for _, want := range []string{"--die-with-parent", "--unshare-net", "--bind", workdir, "--chdir", workdir, "--setenv", "AIOPS_RUN_TOKEN", "allowed-secret", "--", "codex", "exec"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("wrapped args missing %q in %#v", want, wrapped.Args)
		}
	}
	if strings.Contains(joined, "GITHUB_TOKEN") {
		t.Fatalf("wrapped args leaked denied credential: %#v", wrapped.Args)
	}
	for _, env := range wrapped.Env {
		if strings.HasPrefix(env, "GITHUB_TOKEN=") {
			t.Fatalf("sandbox env leaked non-allowlisted credential: %q", wrapped.Env)
		}
	}
}

func TestScopedEnvRejectsTrackerTokenNames(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "must-not-leak")
	t.Setenv("AIOPS_RUN_TOKEN", "allowed-secret")

	env := scopedEnv([]string{"AIOPS_RUN_TOKEN", "GITHUB_TOKEN"}, workflow.Config{})
	if !envContains(env, "AIOPS_RUN_TOKEN=allowed-secret") {
		t.Fatalf("scoped env missing allowed value: %q", env)
	}
	if envContains(env, "GITHUB_TOKEN=must-not-leak") {
		t.Fatalf("scoped env leaked denied token name: %q", env)
	}
}

func TestScopedEnvRejectsTrackerAPIKeyValue(t *testing.T) {
	t.Setenv("AIOPS_TEST_TRACKER_TOKEN", "tracker-secret")

	env := scopedEnv([]string{"AIOPS_TEST_TRACKER_TOKEN"}, workflow.Config{
		Tracker: workflow.TrackerConfig{APIKey: "tracker-secret"},
	})
	if envContains(env, "AIOPS_TEST_TRACKER_TOKEN=tracker-secret") {
		t.Fatalf("scoped env leaked tracker API key value: %q", env)
	}
}

func TestSandboxEnvTreatsAllowlistAsFinalBoundary(t *testing.T) {
	t.Setenv("AIOPS_RUNNER_CANARY", "from-worker-env")
	t.Setenv("AIOPS_SANDBOX_CANARY", "from-sandbox-allowlist")

	env := sandboxEnv(
		[]string{"AIOPS_RUNNER_CANARY=from-runner-env", "AIOPS_BLOCKED_CANARY=blocked", "PATH=/agent/bin"},
		[]string{"AIOPS_RUNNER_CANARY", "AIOPS_SANDBOX_CANARY"},
		workflow.Config{},
	)
	if !envContains(env, "AIOPS_RUNNER_CANARY=from-runner-env") {
		t.Fatalf("sandbox env missing allowlisted runner value: %q", env)
	}
	if envContains(env, "AIOPS_RUNNER_CANARY=from-worker-env") {
		t.Fatalf("sandbox env used worker env instead of runner-scoped value: %q", env)
	}
	if !envContains(env, "AIOPS_SANDBOX_CANARY=from-sandbox-allowlist") {
		t.Fatalf("sandbox env missing allowlisted worker value: %q", env)
	}
	if envContains(env, "AIOPS_BLOCKED_CANARY=blocked") || envContains(env, "PATH=/agent/bin") {
		t.Fatalf("sandbox env leaked non-allowlisted runner values: %q", env)
	}
}

func TestSandboxBubblewrapTreatsEnvAllowlistAsFinalBoundary(t *testing.T) {
	requireLinuxSandboxHost(t)
	binDir := t.TempDir()
	bwrap := filepath.Join(binDir, "bwrap")
	if err := os.WriteFile(bwrap, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codex, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AIOPS_SANDBOX_CANARY", "from-sandbox-allowlist")
	t.Setenv("AIOPS_RUNNER_CANARY", "from-worker-env")

	workdir := t.TempDir()
	base := exec.CommandContext(context.Background(), "codex", "exec", "--cd", workdir)
	base.Dir = workdir
	base.Env = []string{"AIOPS_RUNNER_CANARY=from-runner-env", "AIOPS_BLOCKED_CANARY=blocked", "PATH=" + os.Getenv("PATH")}

	wrapped, err := applySandbox(context.Background(), sandboxInput(t, workdir, workflow.SandboxConfig{
		Enabled:      true,
		Backend:      "bubblewrap",
		NetworkMode:  "none",
		EnvAllowlist: []string{"AIOPS_RUNNER_CANARY", "AIOPS_SANDBOX_CANARY"},
	}), base)
	if err != nil {
		t.Fatalf("applySandbox: %v", err)
	}
	joined := strings.Join(wrapped.Args, "\x00")
	for _, want := range []string{"--setenv", "AIOPS_RUNNER_CANARY", "from-runner-env", "AIOPS_SANDBOX_CANARY", "from-sandbox-allowlist"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("wrapped args missing %q in %#v", want, wrapped.Args)
		}
	}
	for _, denied := range []string{"AIOPS_BLOCKED_CANARY", "PATH=" + os.Getenv("PATH")} {
		if strings.Contains(joined, denied) {
			t.Fatalf("wrapped args leaked non-allowlisted runner env %q in %#v", denied, wrapped.Args)
		}
	}
	if !envContains(wrapped.Env, "AIOPS_RUNNER_CANARY=from-runner-env") {
		t.Fatalf("wrapped Env missing runner-scoped env: %q", wrapped.Env)
	}
	if envContains(wrapped.Env, "AIOPS_RUNNER_CANARY=from-worker-env") {
		t.Fatalf("wrapped Env used worker env instead of runner-scoped env: %q", wrapped.Env)
	}
	if envContains(wrapped.Env, "AIOPS_BLOCKED_CANARY=blocked") {
		t.Fatalf("wrapped Env leaked non-allowlisted runner env: %q", wrapped.Env)
	}
	if !envContains(wrapped.Env, "AIOPS_SANDBOX_CANARY=from-sandbox-allowlist") {
		t.Fatalf("wrapped Env missing sandbox allowlist env: %q", wrapped.Env)
	}
}

func envContains(env []string, want string) bool {
	for _, got := range env {
		if got == want {
			return true
		}
	}
	return false
}

func TestSandboxBubblewrapSkipsMissingLib64Bind(t *testing.T) {
	requireLinuxSandboxHost(t)
	binDir := t.TempDir()
	bwrap := filepath.Join(binDir, "bwrap")
	if err := os.WriteFile(bwrap, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codex, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	workdir := t.TempDir()
	base := exec.CommandContext(context.Background(), "codex", "exec")
	base.Dir = workdir

	wrapped, err := applySandbox(context.Background(), sandboxInput(t, workdir, workflow.SandboxConfig{
		Enabled:     true,
		Backend:     "bubblewrap",
		NetworkMode: "none",
	}), base)
	if err != nil {
		t.Fatalf("applySandbox: %v", err)
	}

	for i := 0; i+2 < len(wrapped.Args); i++ {
		if wrapped.Args[i] == "--ro-bind" && wrapped.Args[i+1] == "/lib64" && wrapped.Args[i+2] == "/lib64" {
			if _, err := os.Stat("/lib64"); os.IsNotExist(err) {
				t.Fatalf("bubblewrap must not require missing /lib64 source, args=%#v", wrapped.Args)
			}
		}
	}
}

func TestSandboxEnabledFailsWhenDependencyMissing(t *testing.T) {
	requireLinuxSandboxHost(t)
	t.Setenv("PATH", "")
	workdir := t.TempDir()
	base := exec.CommandContext(context.Background(), "codex", "exec")
	base.Dir = workdir

	_, err := applySandbox(context.Background(), sandboxInput(t, workdir, workflow.SandboxConfig{Enabled: true, Backend: "bubblewrap"}), base)
	if err == nil {
		t.Fatal("expected missing bubblewrap dependency error")
	}
	if !strings.Contains(err.Error(), "bubblewrap") || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %q, want missing bubblewrap dependency", err)
	}
}

func TestSandboxEnabledFailsFastOnUnsupportedHostOS(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux hosts support bubblewrap/firejail discovery; unsupported-OS behavior is covered by the helper")
	}
	workdir := t.TempDir()
	base := exec.CommandContext(context.Background(), "codex", "exec")
	base.Dir = workdir

	_, err := applySandbox(context.Background(), sandboxInput(t, workdir, workflow.SandboxConfig{Enabled: true, Backend: "bubblewrap"}), base)
	if err == nil {
		t.Fatal("expected unsupported host OS error")
	}
	if !strings.Contains(err.Error(), "linux") || !strings.Contains(err.Error(), runtime.GOOS) {
		t.Fatalf("error = %q, want linux-only guidance for %s", err, runtime.GOOS)
	}
}

func TestSandboxRejectsWorkdirOutsideWorkspaceRoot(t *testing.T) {
	requireLinuxSandboxHost(t)
	binDir := t.TempDir()
	bwrap := filepath.Join(binDir, "bwrap")
	if err := os.WriteFile(bwrap, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	root := t.TempDir()
	outside := t.TempDir()
	base := exec.CommandContext(context.Background(), "codex", "exec")
	base.Dir = outside
	in := RunInput{
		Task: task.Task{ID: "tsk_sandbox", Model: "codex"},
		Workflow: workflow.Workflow{Config: workflow.Config{
			Workspace: workflow.WorkspaceConfig{Root: root},
			Sandbox:   workflow.SandboxConfig{Enabled: true, Backend: "bubblewrap"},
		}},
		Workdir: outside,
	}

	_, err := applySandbox(context.Background(), in, base)
	if err == nil {
		t.Fatal("expected workspace-root invariant error")
	}
	if !strings.Contains(err.Error(), "workspace root") {
		t.Fatalf("error = %q, want workspace root invariant", err)
	}
}

func TestSandboxRejectsSymlinkEscapeOutsideWorkspaceRoot(t *testing.T) {
	requireLinuxSandboxHost(t)
	binDir := t.TempDir()
	bwrap := filepath.Join(binDir, "bwrap")
	if err := os.WriteFile(bwrap, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "link-outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	base := exec.CommandContext(context.Background(), "codex", "exec")
	base.Dir = link
	in := RunInput{
		Task: task.Task{ID: "tsk_sandbox", Model: "codex"},
		Workflow: workflow.Workflow{Config: workflow.Config{
			Workspace: workflow.WorkspaceConfig{Root: root},
			Sandbox:   workflow.SandboxConfig{Enabled: true, Backend: "bubblewrap", EnvAllowlist: []string{"PATH"}},
		}},
		Workdir: link,
	}

	_, err := applySandbox(context.Background(), in, base)
	if err == nil {
		t.Fatal("expected workspace-root invariant error for symlink escape")
	}
	if !strings.Contains(err.Error(), "workspace root") {
		t.Fatalf("error = %q, want workspace root invariant", err)
	}
}

func TestSandboxAllowsWorkdirUnderRuntimeWorkspaceRootWhenWorkflowRootDiffers(t *testing.T) {
	requireLinuxSandboxHost(t)
	binDir := t.TempDir()
	bwrap := filepath.Join(binDir, "bwrap")
	if err := os.WriteFile(bwrap, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codex, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	runtimeRoot := t.TempDir()
	workdir := filepath.Join(runtimeRoot, "issue-123")
	if err := os.Mkdir(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	workflowRoot := t.TempDir()
	base := exec.CommandContext(context.Background(), "codex", "exec")
	base.Dir = workdir
	in := RunInput{
		Task: task.Task{ID: "tsk_sandbox", Model: "codex"},
		Workflow: workflow.Workflow{Config: workflow.Config{
			Workspace: workflow.WorkspaceConfig{Root: workflowRoot},
			Sandbox:   workflow.SandboxConfig{Enabled: true, Backend: "bubblewrap", EnvAllowlist: []string{"PATH"}},
		}},
		Workdir:       workdir,
		WorkspaceRoot: runtimeRoot,
	}

	wrapped, err := applySandbox(context.Background(), in, base)
	if err != nil {
		t.Fatalf("applySandbox should validate against runtime workspace root: %v", err)
	}
	if wrapped.Dir != workdir {
		t.Fatalf("wrapped.Dir = %q, want %q", wrapped.Dir, workdir)
	}
}

func TestEnsurePathWithinRoot(t *testing.T) {
	// EvalSymlinks requires every path to exist, so materialize a real tree.
	root := t.TempDir()
	sibling := t.TempDir() // shares root's parent → rel begins with "../"

	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	// A descendant whose first component merely begins with ".." is legitimate,
	// not a parent-directory escape (#670).
	dotPrefixedChild := filepath.Join(root, "..foo", "inside")
	if err := os.MkdirAll(dotPrefixedChild, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(sibling, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		path      string
		wantErr   bool
		errSubstr string
	}{
		{name: "root itself rejected", path: root, wantErr: true, errSubstr: "not the workspace root itself"},
		{name: "child accepted", path: child, wantErr: false},
		{name: "dot-dot-prefixed child accepted", path: dotPrefixedChild, wantErr: false},
		{name: "immediate parent rejected", path: filepath.Dir(root), wantErr: true, errSubstr: "outside workspace root"},
		{name: "sibling rejected", path: sibling, wantErr: true, errSubstr: "outside workspace root"},
		{name: "parent-escape path rejected", path: outside, wantErr: true, errSubstr: "outside workspace root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ensurePathWithinRoot(tt.path, root)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ensurePathWithinRoot(%q, %q) = nil; want error", tt.path, root)
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("ensurePathWithinRoot(%q, %q) error = %q; want substring %q", tt.path, root, err, tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ensurePathWithinRoot(%q, %q) = %v; want nil", tt.path, root, err)
			}
		})
	}
}

func TestSandboxNetworkAllowlistRequiresFirejail(t *testing.T) {
	requireLinuxSandboxHost(t)
	binDir := t.TempDir()
	bwrap := filepath.Join(binDir, "bwrap")
	if err := os.WriteFile(bwrap, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codex, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	workdir := t.TempDir()
	base := exec.CommandContext(context.Background(), "codex", "exec")
	base.Dir = workdir

	_, err := applySandbox(context.Background(), sandboxInput(t, workdir, workflow.SandboxConfig{
		Enabled:               true,
		Backend:               "bubblewrap",
		NetworkMode:           "allowlist",
		NetworkAllowlistCIDRs: []string{"203.0.113.10/32"},
	}), base)
	if err == nil {
		t.Fatal("expected network allowlist unsupported error")
	}
	if !strings.Contains(err.Error(), "network allowlist") || !strings.Contains(err.Error(), "firejail") {
		t.Fatalf("error = %q, want firejail network allowlist guidance", err)
	}
}

func TestSandboxFirejailNetworkNoneIgnoresStaleAllowlistCIDRs(t *testing.T) {
	requireLinuxSandboxHost(t)
	binDir := t.TempDir()
	firejail := filepath.Join(binDir, "firejail")
	if err := os.WriteFile(firejail, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codex, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	workdir := t.TempDir()
	base := exec.CommandContext(context.Background(), "codex", "app-server")
	base.Dir = workdir

	wrapped, err := applySandbox(context.Background(), sandboxInput(t, workdir, workflow.SandboxConfig{
		Enabled:               true,
		Backend:               "firejail",
		NetworkMode:           "none",
		NetworkAllowlistCIDRs: []string{"203.0.113.10/32"},
	}), base)
	if err != nil {
		t.Fatalf("applySandbox: %v", err)
	}
	joined := strings.Join(wrapped.Args, "\x00")
	if !strings.Contains(joined, "--net=none") {
		t.Fatalf("network none must disable egress even when stale allowlist CIDRs are present, args=%#v", wrapped.Args)
	}
	if strings.Contains(joined, "--netfilter=") || strings.Contains(joined, "--net=eth0") {
		t.Fatalf("network none must not enable firejail allowlist mode from stale CIDRs, args=%#v", wrapped.Args)
	}
}

func TestSandboxBubblewrapNetworkNoneIgnoresStaleAllowlistCIDRs(t *testing.T) {
	requireLinuxSandboxHost(t)
	binDir := t.TempDir()
	bwrap := filepath.Join(binDir, "bwrap")
	if err := os.WriteFile(bwrap, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codex, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	workdir := t.TempDir()
	base := exec.CommandContext(context.Background(), "codex", "app-server")
	base.Dir = workdir

	wrapped, err := applySandbox(context.Background(), sandboxInput(t, workdir, workflow.SandboxConfig{
		Enabled:               true,
		Backend:               "bubblewrap",
		NetworkMode:           "none",
		NetworkAllowlistCIDRs: []string{"203.0.113.10/32"},
	}), base)
	if err != nil {
		t.Fatalf("applySandbox: %v", err)
	}
	joined := strings.Join(wrapped.Args, "\x00")
	if !strings.Contains(joined, "--unshare-net") {
		t.Fatalf("bubblewrap network none must disable egress even when stale allowlist CIDRs are present, args=%#v", wrapped.Args)
	}
	if strings.Contains(joined, "--netfilter=") {
		t.Fatalf("bubblewrap network none must not enable allowlist mode from stale CIDRs, args=%#v", wrapped.Args)
	}
}

func TestSandboxFirejailAllowlistRequiresExplicitNetworkInterface(t *testing.T) {
	requireLinuxSandboxHost(t)
	binDir := t.TempDir()
	firejail := filepath.Join(binDir, "firejail")
	if err := os.WriteFile(firejail, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codex, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	workdir := t.TempDir()
	base := exec.CommandContext(context.Background(), "codex", "app-server")
	base.Dir = workdir

	_, err := applySandbox(context.Background(), sandboxInput(t, workdir, workflow.SandboxConfig{
		Enabled:               true,
		Backend:               "firejail",
		NetworkMode:           "allowlist",
		NetworkAllowlistCIDRs: []string{"203.0.113.10/32"},
	}), base)
	if err == nil {
		t.Fatal("expected explicit firejail allowlist network_interface error")
	}
	if !strings.Contains(err.Error(), "sandbox.network_interface") || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("error = %q, want explicit network_interface guidance", err)
	}
}

func TestSandboxFirejailBuildsNetworkAllowlistAndCredentialScope(t *testing.T) {
	requireLinuxSandboxHost(t)
	binDir := t.TempDir()
	firejail := filepath.Join(binDir, "firejail")
	if err := os.WriteFile(firejail, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codex, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AIOPS_RUN_TOKEN", "allowed-secret")
	t.Setenv("GITHUB_TOKEN", "must-not-leak")

	workdir := t.TempDir()
	credential := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(credential, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := exec.CommandContext(context.Background(), "codex", "app-server")
	base.Dir = workdir

	wrapped, err := applySandbox(context.Background(), sandboxInput(t, workdir, workflow.SandboxConfig{
		Enabled:               true,
		Backend:               "firejail",
		NetworkMode:           "allowlist",
		NetworkAllowlistCIDRs: []string{"203.0.113.10/32"},
		NetworkInterface:      "aiops0",
		EnvAllowlist:          []string{"AIOPS_RUN_TOKEN"},
		CredentialFiles:       []string{credential},
	}), base)
	if err != nil {
		t.Fatalf("applySandbox: %v", err)
	}
	joined := strings.Join(wrapped.Args, "\x00")
	if filepath.Base(wrapped.Path) != "firejail" && !strings.Contains(joined, "firejail") {
		t.Fatalf("wrapped command should execute firejail directly or via cleanup wrapper, path=%q args=%#v", wrapped.Path, wrapped.Args)
	}
	for _, want := range []string{"--quiet", "--noprofile", "--net=aiops0", "--netfilter=", "--env=AIOPS_RUN_TOKEN=allowed-secret", "--read-only=" + credential, "--whitelist=" + credential, "--", "codex", "app-server"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("wrapped args missing %q in %#v", want, wrapped.Args)
		}
	}
	for _, arg := range wrapped.Args {
		if arg == "--env=AIOPS_RUN_TOKEN" {
			t.Fatalf("firejail env allowlist must preserve values with name=value args, got %#v", wrapped.Args)
		}
		if arg == "--net=none" {
			t.Fatalf("firejail allowlist mode must use a connected network namespace, got %#v", wrapped.Args)
		}
	}
	for _, env := range wrapped.Env {
		if strings.HasPrefix(env, "GITHUB_TOKEN=") {
			t.Fatalf("sandbox env leaked non-allowlisted credential: %q", wrapped.Env)
		}
	}
}

func TestFirejailNetfilterFileIsRemovedWhenSandboxCommandIsCanceled(t *testing.T) {
	filterPath, wrapped := firejailAllowlistCommandForCleanupTest(t, "#!/usr/bin/env sh\nsleep 30\n")

	if err := wrapped.Cancel(); err != nil {
		t.Fatalf("cancel failed: %v", err)
	}
	if _, err := os.Stat(filterPath); !os.IsNotExist(err) {
		t.Fatalf("netfilter file should be removed when sandbox command is canceled, stat err = %v", err)
	}
}

func TestFirejailNetfilterFileIsRemovedWhenWrappedCommandExits(t *testing.T) {
	filterPath, wrapped := firejailAllowlistCommandForCleanupTest(t, "#!/usr/bin/env sh\nexit 0\n")

	if err := wrapped.Run(); err != nil {
		t.Fatalf("wrapped command failed: %v", err)
	}
	if _, err := os.Stat(filterPath); !os.IsNotExist(err) {
		t.Fatalf("netfilter file should be removed after command exit, stat err = %v", err)
	}
}

func TestFirejailNetfilterFileIsRemovedWhenTerminateProcessKillsWrapper(t *testing.T) {
	filterPath, wrapped := firejailAllowlistCommandForCleanupTest(t, "#!/usr/bin/env sh\nsleep 30\n")
	configurePlatformKill(wrapped)

	if err := wrapped.Start(); err != nil {
		t.Fatalf("start wrapped command: %v", err)
	}
	if wrapped.Cancel == nil {
		t.Fatal("expected sandbox command to register cleanup cancellation hook")
	}
	if err := wrapped.Cancel(); err != nil {
		t.Fatalf("cancel wrapped command: %v", err)
	}
	_ = wrapped.Wait()
	if _, err := os.Stat(filterPath); !os.IsNotExist(err) {
		t.Fatalf("netfilter file should be removed after forced process termination, stat err = %v", err)
	}
}

func TestFirejailNetfilterRejectsIPv6CIDR(t *testing.T) {
	_, err := writeFirejailNetfilter([]string{"2001:db8::/32"})
	if err == nil {
		t.Fatal("expected IPv6 CIDR to be rejected for firejail IPv4 netfilter")
	}
	if !strings.Contains(err.Error(), "IPv4") || !strings.Contains(err.Error(), "2001:db8::/32") {
		t.Fatalf("error = %q, want IPv4-only guidance for IPv6 CIDR", err)
	}
}

func TestFirejailNetfilterAcceptsIPv4CIDR(t *testing.T) {
	path, err := writeFirejailNetfilter([]string{"203.0.113.10/32"})
	if err != nil {
		t.Fatalf("writeFirejailNetfilter: %v", err)
	}
	defer func() { _ = os.Remove(path) }()

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read netfilter file: %v", err)
	}
	if !strings.Contains(string(content), "-A OUTPUT -d 203.0.113.10/32 -j ACCEPT") {
		t.Fatalf("netfilter file missing IPv4 allow rule: %s", content)
	}
}

func TestBuildFirejailNetArgsBuildsAllowlistAndCleanupFile(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())

	args, cleanupFiles, err := buildFirejailNetArgs(workflow.SandboxConfig{
		NetworkMode:           "allowlist",
		NetworkAllowlistCIDRs: []string{"203.0.113.10/32"},
		NetworkInterface:      "aiops0",
	})
	if err != nil {
		t.Fatalf("buildFirejailNetArgs: %v", err)
	}
	defer func() { _ = removeFiles(cleanupFiles) }()
	if len(cleanupFiles) != 1 {
		t.Fatalf("cleanupFiles = %#v; want one netfilter file", cleanupFiles)
	}
	wantArgs := []string{"--net=aiops0", "--netfilter=" + cleanupFiles[0]}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("buildFirejailNetArgs args = %#v; want %#v", args, wantArgs)
	}
	if _, err := os.Stat(cleanupFiles[0]); err != nil {
		t.Fatalf("netfilter cleanup file should exist before wrapping: %v", err)
	}
}

func TestBuildFirejailNetArgsRemovesNetfilterWhenInterfaceMissing(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("TMPDIR", tmpDir)

	_, _, err := buildFirejailNetArgs(workflow.SandboxConfig{
		NetworkMode:           "allowlist",
		NetworkAllowlistCIDRs: []string{"203.0.113.10/32"},
	})
	if err == nil {
		t.Fatal("expected missing firejail network interface error")
	}
	matches, globErr := filepath.Glob(filepath.Join(tmpDir, "aiops-firejail-netfilter-*.conf"))
	if globErr != nil {
		t.Fatalf("glob netfilter files: %v", globErr)
	}
	if len(matches) != 0 {
		t.Fatalf("buildFirejailNetArgs setup error should remove netfilter files, left %v", matches)
	}
}

func TestFirejailNetfilterFileIsRemovedWhenCredentialValidationFails(t *testing.T) {
	requireLinuxSandboxHost(t)
	binDir := t.TempDir()
	firejail := filepath.Join(binDir, "firejail")
	if err := os.WriteFile(firejail, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codex, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMPDIR", t.TempDir())

	workdir := t.TempDir()
	base := exec.CommandContext(context.Background(), "codex", "app-server")
	base.Dir = workdir

	missingCredential := filepath.Join(t.TempDir(), "missing-token")
	_, err := applySandbox(context.Background(), sandboxInput(t, workdir, workflow.SandboxConfig{
		Enabled:               true,
		Backend:               "firejail",
		NetworkMode:           "allowlist",
		NetworkAllowlistCIDRs: []string{"203.0.113.10/32"},
		NetworkInterface:      "aiops0",
		CredentialFiles:       []string{missingCredential},
	}), base)
	if err == nil {
		t.Fatal("expected missing credential file error")
	}

	matches, globErr := filepath.Glob(filepath.Join(os.Getenv("TMPDIR"), "aiops-firejail-netfilter-*.conf"))
	if globErr != nil {
		t.Fatalf("glob netfilter files: %v", globErr)
	}
	if len(matches) != 0 {
		t.Fatalf("firejail setup error should remove netfilter files, left %v", matches)
	}
}

func firejailAllowlistCommandForCleanupTest(t *testing.T, firejailScript string) (string, *exec.Cmd) {
	t.Helper()
	requireLinuxSandboxHost(t)
	binDir := t.TempDir()
	firejail := filepath.Join(binDir, "firejail")
	if err := os.WriteFile(firejail, []byte(firejailScript), 0o755); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(binDir, "codex")
	if err := os.WriteFile(codex, []byte("#!/usr/bin/env sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	workdir := t.TempDir()
	base := exec.CommandContext(context.Background(), "codex", "app-server")
	base.Dir = workdir

	wrapped, err := applySandbox(context.Background(), sandboxInput(t, workdir, workflow.SandboxConfig{
		Enabled:               true,
		Backend:               "firejail",
		NetworkMode:           "allowlist",
		NetworkAllowlistCIDRs: []string{"203.0.113.10/32"},
		NetworkInterface:      "aiops0",
	}), base)
	if err != nil {
		t.Fatalf("applySandbox: %v", err)
	}
	var filterPath string
	for _, arg := range wrapped.Args {
		if strings.HasPrefix(arg, "--netfilter=") {
			filterPath = strings.TrimPrefix(arg, "--netfilter=")
		}
	}
	if filterPath == "" {
		t.Fatalf("wrapped args missing --netfilter path: %#v", wrapped.Args)
	}
	if _, err := os.Stat(filterPath); err != nil {
		t.Fatalf("netfilter file should exist before command starts: %v", err)
	}
	return filterPath, wrapped
}
