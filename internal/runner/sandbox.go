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

func applySandbox(ctx context.Context, in RunInput, cmd *exec.Cmd) (*exec.Cmd, error) { //nolint:gocognit // baseline (#521)
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
	root := strings.TrimSpace(in.WorkspaceRoot)
	if root == "" {
		root = strings.TrimSpace(in.Workflow.Config.Workspace.Root)
	}
	if root == "" {
		return nil, fmt.Errorf("sandbox requires runtime workspace root so the workspace-root invariant can be enforced")
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve runtime workspace root: %w", err)
	}
	if err := ensurePathWithinRoot(workdir, rootAbs); err != nil {
		return nil, err
	}

	childArgs := append([]string{}, cmd.Args...)
	if len(childArgs) == 0 {
		return nil, fmt.Errorf("sandbox cannot wrap empty command")
	}
	env := sandboxEnv(cmd.Env, cfg.EnvAllowlist, in.Workflow.Config)

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
	if rel == "." {
		return fmt.Errorf("workspace path %q must be under workspace root %q, not the workspace root itself", path, root)
	}
	// Reject genuine parent-directory escapes: rel is exactly ".." or its first
	// path component is ".." (rel begins with "../"). A child whose name merely
	// begins with ".." (e.g. "..foo", "..foo/inside") is a legitimate descendant
	// and must not be over-rejected (#670). The rel == "" and filepath.IsAbs(rel)
	// clauses are fail-closed guards: filepath.Rel on two absolute,
	// symlink-resolved paths returns a non-empty relative path or an error
	// (already handled above), so they are defensive belt-and-suspenders rather
	// than reachable branches.
	if rel == "" || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("workspace path %q is outside workspace root %q", path, root)
	}
	return nil
}

func scopedEnv(allow []string, cfg workflow.Config) []string {
	return sandboxEnv(nil, allow, cfg)
}

func sandboxEnv(primary []string, allow []string, cfg workflow.Config) []string {
	if len(allow) == 0 {
		return []string{}
	}
	primaryByName := indexEnvByName(primary)
	seen := map[string]struct{}{}
	var env []string
	for _, name := range allow {
		name = strings.TrimSpace(name)
		if _, ok := seen[name]; ok {
			continue
		}
		value, ok := allowlistedEnvValue(name, primaryByName, cfg)
		if !ok {
			continue
		}
		seen[name] = struct{}{}
		env = append(env, name+"="+value)
	}
	return carryWorkerInjectedGoCache(env, primaryByName, seen)
}

// indexEnvByName maps each KEY=VALUE pair in env to KEY->VALUE, keeping the last
// value for a repeated key. Pairs without a non-empty name are skipped.
func indexEnvByName(env []string) map[string]string {
	byName := make(map[string]string, len(env))
	for _, pair := range env {
		name, value, ok := strings.Cut(pair, "=")
		if ok && name != "" {
			byName[name] = value
		}
	}
	return byName
}

// allowlistedEnvValue resolves the value an allowlisted name contributes to the
// sandbox env, or reports ok=false when the name must not cross the boundary.
// It enforces the default-deny boundary: a malformed name, or one the tracker
// token deny filter rejects, resolves to nothing; an admitted name takes its
// worker-supplied value (primary) ahead of the host environment.
func allowlistedEnvValue(name string, primaryByName map[string]string, cfg workflow.Config) (string, bool) {
	if name == "" || strings.Contains(name, "=") {
		return "", false
	}
	if workflow.AgentEnvPassthroughDenyReasonForConfig(name, cfg) != "" {
		return "", false
	}
	if value, ok := primaryByName[name]; ok {
		return value, true
	}
	return os.LookupEnv(name)
}

// carryWorkerInjectedGoCache appends the worker-injected Go toolchain cache
// defaults (#544) that the allowlist loop did not already admit, so the agent's
// first `go test` finds a writable cache under the optional bubblewrap/firejail
// wrapper (its tmpfs /tmp keeps the default writable). setupAppServerCommand
// sets GOCACHE/GOMODCACHE as agent-runtime requirements, not operator
// passthrough, so the operator should not have to allowlist them.
//
// Only WORKER-INJECTED values (under aiopsGoCacheRoot) are carried — an
// operator's own GOCACHE/GOMODCACHE kept out of sandbox.env_allowlist still
// respects that deny boundary (codex review #548). seen records the names the
// allowlist already emitted so a carried name is never duplicated.
func carryWorkerInjectedGoCache(env []string, primaryByName map[string]string, seen map[string]struct{}) []string {
	for _, name := range goCacheNames() {
		if _, ok := seen[name]; ok {
			continue
		}
		value, ok := primaryByName[name]
		if !ok || !isWorkerInjectedGoCache(value) {
			continue
		}
		seen[name] = struct{}{}
		env = append(env, name+"="+value)
	}
	return env
}

func bubblewrapCommand(ctx context.Context, cfg workflow.SandboxConfig, workdir string, childArgs []string, env []string) (*exec.Cmd, error) { //nolint:gocognit // baseline (#521)
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("bubblewrap sandbox requested but bwrap binary not found in PATH: %w", err)
	}
	childArgs, err = resolveSandboxChildArgs(childArgs)
	if err != nil {
		return nil, err
	}
	if cfg.NetworkMode == "allowlist" {
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
		"--tmpfs", "/tmp",
		"--bind", workdir, workdir,
		"--chdir", workdir,
	}
	if _, err := os.Stat("/lib64"); err == nil {
		args = append(args, "--ro-bind", "/lib64", "/lib64")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("check optional bubblewrap /lib64 bind source: %w", err)
	}
	if cfg.NetworkMode == "none" || cfg.NetworkMode == "" {
		args = append(args, "--unshare-net")
	}
	for _, envPair := range env {
		name, value, _ := strings.Cut(envPair, "=")
		args = append(args, "--setenv", name, value)
	}
	args, err = appendCredentialMounts(args, cfg.CredentialFiles, bubblewrapCredentialMount)
	if err != nil {
		return nil, err
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
	childArgs, err = resolveSandboxChildArgs(childArgs)
	if err != nil {
		return nil, err
	}
	args := []string{"--quiet", "--noprofile", "--private=" + workdir, "--whitelist=" + workdir, "--private-tmp"}
	netArgs, cleanupFiles, err := buildFirejailNetArgs(cfg)
	if err != nil {
		return nil, err
	}
	args = append(args, netArgs...)
	cleanupOnError := len(cleanupFiles) > 0
	defer func() {
		if cleanupOnError {
			_ = removeFiles(cleanupFiles)
		}
	}()
	for _, envPair := range env {
		args = append(args, "--env="+envPair)
	}
	args, err = appendCredentialMounts(args, cfg.CredentialFiles, firejailCredentialMount)
	if err != nil {
		return nil, err
	}
	args = append(args, "--")
	args = append(args, childArgs...)
	wrapped, err := wireFirejailCleanup(ctx, firejail, args, cleanupFiles)
	if err != nil {
		return nil, err
	}
	wrapped.Dir = workdir
	wrapped.Env = env
	cleanupOnError = false
	return wrapped, nil
}

func resolveSandboxChildArgs(childArgs []string) ([]string, error) {
	if len(childArgs) == 0 {
		return nil, fmt.Errorf("sandbox cannot wrap empty command")
	}
	if filepath.IsAbs(childArgs[0]) || strings.ContainsAny(childArgs[0], `/\\`) {
		return childArgs, nil
	}
	resolved, err := exec.LookPath(childArgs[0])
	if err != nil {
		return nil, fmt.Errorf("sandbox child binary %q not found in PATH: %w", childArgs[0], err)
	}
	return append([]string{resolved}, childArgs[1:]...), nil
}

func appendCredentialMounts(args []string, credentialFiles []string, mountArgs func(string) []string) ([]string, error) {
	for _, f := range credentialFiles {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if _, err := os.Stat(f); err != nil {
			return nil, fmt.Errorf("sandbox credential file %q is not readable: %w", f, err)
		}
		args = append(args, mountArgs(f)...)
	}
	return args, nil
}

func bubblewrapCredentialMount(path string) []string {
	return []string{"--ro-bind", path, path}
}

func firejailCredentialMount(path string) []string {
	return []string{"--read-only=" + path, "--whitelist=" + path}
}

func buildFirejailNetArgs(cfg workflow.SandboxConfig) ([]string, []string, error) {
	if cfg.NetworkMode != "allowlist" {
		return []string{"--net=none"}, nil, nil
	}
	filter, err := writeFirejailNetfilter(cfg.NetworkAllowlistCIDRs)
	if err != nil {
		return nil, nil, err
	}
	cleanupFiles := []string{filter}
	cleanupOnError := true
	defer func() {
		if cleanupOnError {
			_ = removeFiles(cleanupFiles)
		}
	}()
	netArg, err := firejailAllowlistNetArg(cfg)
	if err != nil {
		return nil, nil, err
	}
	cleanupOnError = false
	return []string{netArg, "--netfilter=" + filter}, cleanupFiles, nil
}

func wireFirejailCleanup(ctx context.Context, firejail string, args []string, cleanupFiles []string) (*exec.Cmd, error) {
	wrapped := exec.CommandContext(ctx, firejail, args...)
	if len(cleanupFiles) == 0 {
		return wrapped, nil
	}
	if len(cleanupFiles) != 1 {
		return nil, fmt.Errorf("firejail sandbox expected one cleanup file, got %d", len(cleanupFiles))
	}
	cleanupScript := `cleanup_file=$1; shift; "$@"; status=$?; rm -f "$cleanup_file"; exit "$status"`
	shellArgs := []string{"-c", cleanupScript, "aiops-firejail-cleanup", cleanupFiles[0], firejail}
	shellArgs = append(shellArgs, args...)
	wrapped = exec.CommandContext(ctx, "/bin/sh", shellArgs...)
	wrapped.Cancel = cleanupCancel(cleanupFiles)
	wrapped.WaitDelay = 1
	return wrapped, nil
}

func cleanupCancel(cleanupFiles []string) func() error {
	return func() error {
		return removeFiles(cleanupFiles)
	}
}

func removeFiles(paths []string) error {
	var firstErr error
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func firejailAllowlistNetArg(cfg workflow.SandboxConfig) (string, error) {
	iface := strings.TrimSpace(cfg.NetworkInterface)
	if iface == "" {
		return "", fmt.Errorf("sandbox.network=allowlist requires sandbox.network_interface because Firejail --netfilter must attach to an explicit host interface")
	}
	return "--net=" + iface, nil
}

func writeFirejailNetfilter(cidrs []string) (string, error) { //nolint:gocognit // baseline (#521)
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
		ip, parsed, err := net.ParseCIDR(cidr)
		if err != nil {
			return "", fmt.Errorf("sandbox network allowlist contains invalid CIDR %q: %w", cidr, err)
		}
		if ip.To4() == nil {
			return "", fmt.Errorf("sandbox network allowlist CIDR %q is IPv6; Firejail --netfilter supports IPv4 rules only", cidr)
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
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write firejail netfilter allowlist: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close firejail netfilter allowlist: %w", err)
	}
	return f.Name(), nil
}
