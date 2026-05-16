package runner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
		EnvAllowlist: []string{"AIOPS_RUN_TOKEN"},
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
	for _, env := range wrapped.Env {
		if strings.HasPrefix(env, "GITHUB_TOKEN=") {
			t.Fatalf("sandbox env leaked non-allowlisted credential: %q", wrapped.Env)
		}
	}
}

func TestSandboxEnabledFailsWhenDependencyMissing(t *testing.T) {
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

func TestSandboxNetworkAllowlistRequiresFirejail(t *testing.T) {
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

func TestSandboxFirejailBuildsNetworkAllowlistAndCredentialScope(t *testing.T) {
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
	for _, want := range []string{"--noprofile", "--net=", "--netfilter=", "--env=AIOPS_RUN_TOKEN=allowed-secret", "--read-only=" + credential, "--whitelist=" + credential, "--", "codex", "app-server"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("wrapped args missing %q in %#v", want, wrapped.Args)
		}
	}
	for _, arg := range wrapped.Args {
		if arg == "--env=AIOPS_RUN_TOKEN" {
			t.Fatalf("firejail env allowlist must preserve values with name=value args, got %#v", wrapped.Args)
		}
	}
	for _, env := range wrapped.Env {
		if strings.HasPrefix(env, "GITHUB_TOKEN=") {
			t.Fatalf("sandbox env leaked non-allowlisted credential: %q", wrapped.Env)
		}
	}
}

func TestFirejailNetfilterFileIsRemovedWhenWrappedCommandExits(t *testing.T) {
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
		NetworkMode:           "allowlist",
		NetworkAllowlistCIDRs: []string{"203.0.113.10/32"},
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
	if err := wrapped.Run(); err != nil {
		t.Fatalf("wrapped command failed: %v", err)
	}
	if _, err := os.Stat(filterPath); !os.IsNotExist(err) {
		t.Fatalf("netfilter file should be removed after command exit, stat err = %v", err)
	}
}
