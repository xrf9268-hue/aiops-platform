package runner

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func applySandbox(ctx context.Context, in RunInput, cmd *exec.Cmd) (*exec.Cmd, error) {
	cfg := in.Workflow.Config.Sandbox
	if !cfg.Enabled || cfg.Backend == "" || cfg.Backend == "none" {
		return cmd, nil
	}
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("sandbox.backend %q requires linux host OS; current host OS is %s", cfg.Backend, runtime.GOOS)
	}
	if cfg.NetworkMode == "allowlist" {
		if cfg.Backend != "firejail" {
			return nil, fmt.Errorf("sandbox network allowlist requires sandbox.backend firejail")
		}
		if len(cfg.NetworkAllowlistCIDRs) == 0 {
			return nil, fmt.Errorf("sandbox.network=allowlist requires sandbox.network_allowlist_cidrs")
		}
	}
	workdir, err := filepath.Abs(in.Workdir)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox workdir: %w", err)
	}
	root := strings.TrimSpace(in.Workflow.Config.Workspace.Root)
	if root == "" {
		return nil, fmt.Errorf("sandbox requires workspace.root so the workspace-root invariant can be enforced")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	if err := ensurePathWithinRoot(workdir, rootAbs); err != nil {
		return nil, err
	}

	childArgs := append([]string{}, cmd.Args...)
	if len(childArgs) == 0 {
		return nil, fmt.Errorf("sandbox cannot wrap empty command")
	}
	env := scopedEnv(cfg.EnvAllowlist)

	switch cfg.Backend {
	case "bubblewrap":
		return bubblewrapCommand(ctx, cfg, workdir, childArgs, env)
	case "firejail":
		return firejailCommand(ctx, cfg, workdir, childArgs, env)
	default:
		return nil, fmt.Errorf("sandbox.backend %q is not supported (allowed: none, bubblewrap, firejail)", cfg.Backend)
	}
}

func ensurePathWithinRoot(path, root string) error {
	canonicalPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("check workspace root invariant for %q: %w", path, err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("check workspace root invariant for root %q: %w", root, err)
	}
	rel, err := filepath.Rel(canonicalRoot, canonicalPath)
	if err != nil {
		return fmt.Errorf("check workspace root invariant: %w", err)
	}
	if rel == "." || (rel != "" && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)) {
		return nil
	}
	return fmt.Errorf("sandbox workdir %q is outside workspace root %q", path, root)
}

func scopedEnv(allow []string) []string {
	if len(allow) == 0 {
		return []string{}
	}
	seen := map[string]struct{}{}
	var env []string
	for _, name := range allow {
		name = strings.TrimSpace(name)
		if name == "" || strings.Contains(name, "=") {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func bubblewrapCommand(ctx context.Context, cfg workflow.SandboxConfig, workdir string, childArgs []string, env []string) (*exec.Cmd, error) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("bubblewrap sandbox requested but bwrap binary not found in PATH: %w", err)
	}
	if len(childArgs) == 0 {
		return nil, fmt.Errorf("sandbox cannot wrap empty command")
	}
	if !filepath.IsAbs(childArgs[0]) && !strings.ContainsAny(childArgs[0], `/\\`) {
		resolved, err := exec.LookPath(childArgs[0])
		if err != nil {
			return nil, fmt.Errorf("sandbox child binary %q not found in PATH: %w", childArgs[0], err)
		}
		childArgs = append([]string{resolved}, childArgs[1:]...)
	}
	if cfg.NetworkMode == "allowlist" || len(cfg.NetworkAllowlistCIDRs) > 0 {
		return nil, fmt.Errorf("sandbox network allowlist requires firejail --netfilter support; bubblewrap only supports network: none via --unshare-net")
	}
	args := []string{
		"--die-with-parent",
		"--new-session",
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		"--proc", "/proc",
		"--dev", "/dev",
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/bin", "/bin",
		"--ro-bind", "/lib", "/lib",
		"--ro-bind", "/lib64", "/lib64",
		"--tmpfs", "/tmp",
		"--bind", workdir, workdir,
		"--chdir", workdir,
	}
	if cfg.NetworkMode == "none" || cfg.NetworkMode == "" {
		args = append(args, "--unshare-net")
	}
	for _, envPair := range env {
		name, value, _ := strings.Cut(envPair, "=")
		args = append(args, "--setenv", name, value)
	}
	for _, f := range cfg.CredentialFiles {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if _, err := os.Stat(f); err != nil {
			return nil, fmt.Errorf("sandbox credential file %q is not readable: %w", f, err)
		}
		args = append(args, "--ro-bind", f, f)
	}
	args = append(args, "--")
	args = append(args, childArgs...)
	wrapped := exec.CommandContext(ctx, bwrap, args...)
	wrapped.Dir = workdir
	wrapped.Env = env
	return wrapped, nil
}

func firejailCommand(ctx context.Context, cfg workflow.SandboxConfig, workdir string, childArgs []string, env []string) (*exec.Cmd, error) {
	firejail, err := exec.LookPath("firejail")
	if err != nil {
		return nil, fmt.Errorf("firejail sandbox requested but firejail binary not found in PATH: %w", err)
	}
	if len(childArgs) == 0 {
		return nil, fmt.Errorf("sandbox cannot wrap empty command")
	}
	if !filepath.IsAbs(childArgs[0]) && !strings.ContainsAny(childArgs[0], `/\\`) {
		resolved, err := exec.LookPath(childArgs[0])
		if err != nil {
			return nil, fmt.Errorf("sandbox child binary %q not found in PATH: %w", childArgs[0], err)
		}
		childArgs = append([]string{resolved}, childArgs[1:]...)
	}
	args := []string{"--quiet", "--private=" + workdir, "--whitelist=" + workdir, "--private-tmp"}
	if cfg.NetworkMode == "allowlist" || len(cfg.NetworkAllowlistCIDRs) > 0 {
		filter, err := writeFirejailNetfilter(cfg.NetworkAllowlistCIDRs)
		if err != nil {
			return nil, err
		}
		args = append(args, "--netfilter="+filter)
	} else {
		args = append(args, "--net=none")
	}
	for _, envPair := range env {
		name, _, _ := strings.Cut(envPair, "=")
		args = append(args, "--env="+name)
	}
	for _, f := range cfg.CredentialFiles {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if _, err := os.Stat(f); err != nil {
			return nil, fmt.Errorf("sandbox credential file %q is not readable: %w", f, err)
		}
		args = append(args, "--read-only="+f, "--whitelist="+f)
	}
	args = append(args, "--")
	args = append(args, childArgs...)
	wrapped := exec.CommandContext(ctx, firejail, args...)
	wrapped.Dir = workdir
	wrapped.Env = env
	return wrapped, nil
}

func writeFirejailNetfilter(cidrs []string) (string, error) {
	if len(cidrs) == 0 {
		return "", fmt.Errorf("sandbox network allowlist requires at least one CIDR")
	}
	var b strings.Builder
	b.WriteString("*filter\n:OUTPUT DROP [0:0]\n")
	for _, cidr := range cidrs {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		_, parsed, err := net.ParseCIDR(cidr)
		if err != nil {
			return "", fmt.Errorf("sandbox network allowlist contains invalid CIDR %q: %w", cidr, err)
		}
		b.WriteString("-A OUTPUT -d ")
		b.WriteString(parsed.String())
		b.WriteString(" -j ACCEPT\n")
	}
	b.WriteString("COMMIT\n")
	f, err := os.CreateTemp("", "aiops-firejail-netfilter-*.conf")
	if err != nil {
		return "", fmt.Errorf("create firejail netfilter allowlist: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(b.String()); err != nil {
		return "", fmt.Errorf("write firejail netfilter allowlist: %w", err)
	}
	return f.Name(), nil
}
