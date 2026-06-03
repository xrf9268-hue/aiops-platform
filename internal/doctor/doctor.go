package doctor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

type Status string

const (
	Pass Status = "PASS"
	Warn Status = "WARN"
	Fail Status = "FAIL"
)

const (
	aiopsModulePath       = "github.com/xrf9268-hue/aiops-platform"
	defaultCommandTimeout = 10 * time.Second
	goTestProbeTimeout    = 2 * time.Minute
)

type Check struct {
	Status Status
	Name   string
	Detail string
	Fix    string
}

type Options struct {
	WorkflowPath string
	Mode         string
	Deploy       string
	DashboardURL string
	GoTestDir    string
	GitHubIssue  int
	GitHubRepo   string
	Stdout       io.Writer
	Stderr       io.Writer
	Runner       CommandRunner
	HTTPClient   *http.Client
}

type CommandRunner func(context.Context, string, []string, []string, io.Reader) ([]byte, error)

type Report struct {
	Checks []Check
}

func Run(ctx context.Context, opts Options) int {
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	report := BuildReport(ctx, opts)
	writeTextReport(opts.Stdout, report)
	if report.HasFailures() {
		return 1
	}
	return 0
}

func BuildReport(ctx context.Context, opts Options) Report {
	r := &reportBuilder{opts: opts}
	r.normalize()
	wf, path := r.checkWorkflow()
	r.checkHostBinaries(wf)
	r.checkProjectToolchain(ctx)
	r.checkDockerCompose(ctx)
	if wf != nil {
		r.checkLinear(ctx, wf.Config)
		r.checkCodex(ctx, wf.Config)
		r.checkGitHubAgent(ctx, wf.Config)
		r.checkSandbox(ctx, wf.Config)
	}
	r.checkDashboard(ctx)
	if wf != nil && path != "" {
		r.pass("Workflow path", path)
	}
	return Report{Checks: r.checks}
}

func (r Report) HasFailures() bool {
	for _, check := range r.Checks {
		if check.Status == Fail {
			return true
		}
	}
	return false
}

type reportBuilder struct {
	opts   Options
	checks []Check
}

func (r *reportBuilder) normalize() {
	if r.opts.Mode == "" {
		r.opts.Mode = "mock"
	}
	if r.opts.Deploy == "" {
		r.opts.Deploy = "docker"
	}
	if r.opts.Runner == nil {
		r.opts.Runner = defaultRunner
	}
	if r.opts.HTTPClient == nil {
		r.opts.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
}

func (r *reportBuilder) checkWorkflow() (*workflow.Workflow, string) {
	path := strings.TrimSpace(r.opts.WorkflowPath)
	if path == "" {
		path = os.Getenv("AIOPS_WORKFLOW_PATH")
	}
	if path == "" {
		if wd, err := os.Getwd(); err == nil {
			path = filepath.Join(wd, "WORKFLOW.md")
		}
	}
	if path == "" {
		r.fail("Workflow", "no workflow path found", "Pass --doctor <path> or set AIOPS_WORKFLOW_PATH.")
		return nil, ""
	}
	wf, err := workflow.Load(path)
	if err != nil {
		r.fail("Workflow", err.Error(), "Fix the WORKFLOW.md front matter, then rerun worker --doctor.")
		return nil, path
	}
	if wf.Config.Repo.CloneURL == "" {
		r.fail("Runtime config", "repo.clone_url is required for worker dispatch", "Set repo.clone_url.")
	} else {
		r.pass("Runtime config", "repo/workflow config is dispatchable")
	}
	return wf, path
}

func (r *reportBuilder) checkHostBinaries(wf *workflow.Workflow) {
	r.requiredBinary("git")
	if wf != nil && workflowNeedsSSH(wf.Config) {
		r.requiredBinary("ssh")
	} else {
		r.pass("ssh", "not required for configured repository clone URLs")
	}
	if _, err := exec.LookPath("rg"); err != nil {
		r.warn("rg", "ripgrep not found on PATH", r.installFix("rg")+" It speeds up agent code search.")
	} else {
		r.pass("rg", "found on PATH")
	}
}

func (r *reportBuilder) checkProjectToolchain(ctx context.Context) { //nolint:gocognit // baseline (#521)
	if !r.realMode() {
		return
	}
	if r.binaryDeploy() {
		// The Go toolchain probes validate a dogfood/worker-image checkout
		// (they run `go test` for --go-test-dir), not a binary-runtime
		// prerequisite — a release-archive host ships only worker/tui and
		// has no Go. Skip them so --deploy=binary --mode=real does not FAIL
		// on an absent toolchain. #538.
		return
	}
	modulePath, goModVersion := r.goModule()
	moduleRoot := strings.TrimSpace(r.opts.GoTestDir)
	out, err := r.run(ctx, "go", []string{"version"})
	if err != nil {
		r.fail("Go version", trimOutput(out, err), r.installFix("the Go toolchain required by go.mod"))
	} else if version := strings.TrimSpace(string(out)); goModVersion != "" && !goVersionCompatible(version, goModVersion) {
		r.fail("Go version", version, "Install a Go toolchain compatible with go.mod version "+goModVersion+".")
	} else {
		if goModVersion != "" {
			version += "; go.mod requires " + goModVersion
		}
		r.pass("Go version", version)
	}
	gofmtProbe, err := writeGofmtProbe()
	if err != nil {
		r.fail("gofmt", err.Error(), "Create a writable temp directory for doctor preflight probes.")
	} else if gofmtPath, out, err := r.gofmtPath(ctx); err != nil {
		r.fail("gofmt", trimOutput(out, err), r.installFix("the Go toolchain (gofmt)"))
	} else if out, err := r.run(ctx, gofmtPath, []string{"-l", gofmtProbe}); err != nil {
		r.fail("gofmt", trimOutput(out, err), r.installFix("gofmt"))
	} else {
		r.pass("gofmt", "found at "+gofmtPath)
	}
	if gofmtProbe != "" {
		_ = os.Remove(gofmtProbe)
	}
	if moduleRoot == "" {
		r.warn("Go test", "not checked; no --go-test-dir supplied", "Pass --go-test-dir pointing at the repository module root for dogfood image preflight.")
		return
	}
	if modulePath != aiopsModulePath || goModVersion == "" {
		r.fail("Go test", goModuleFailureDetail(modulePath, goModVersion), "Pass --go-test-dir pointing at the aiops-platform checkout root.")
		return
	}
	if !fileExists(filepath.Join(moduleRoot, "internal", "doctor")) {
		r.fail("Go test", "targeted package internal/doctor not found under --go-test-dir", "Pass --go-test-dir pointing at the aiops-platform checkout root.")
		return
	}
	if out, err := r.runWithTimeout(ctx, goTestProbeTimeout, "go", []string{"test", "-C", moduleRoot, "./internal/doctor", "-run", "TestDoctorGoToolchainProbe", "-count=1"}); err != nil {
		r.fail("Go test", trimOutput(out, err), "Fix the project Go test prerequisites in the targeted module checkout.")
		return
	}
	r.pass("Go test", "go test ./internal/doctor -run TestDoctorGoToolchainProbe -count=1")
}

func (r *reportBuilder) goModule() (string, string) {
	start := strings.TrimSpace(r.opts.GoTestDir)
	if start == "" {
		return "", ""
	}
	return readGoMod(filepath.Join(start, "go.mod"))
}

func (r *reportBuilder) gofmtPath(ctx context.Context) (string, []byte, error) {
	out, err := r.run(ctx, "go", []string{"env", "GOROOT"})
	if err != nil {
		return "", out, err
	}
	goRoot := strings.TrimSpace(string(out))
	if goRoot == "" {
		return "", out, errors.New("go env GOROOT returned empty output")
	}
	return filepath.Join(goRoot, "bin", "gofmt"), out, nil
}

func goModuleFailureDetail(modulePath, version string) string {
	switch {
	case modulePath == "":
		return "no go.mod found for targeted project verification"
	case modulePath != aiopsModulePath:
		return fmt.Sprintf("go.mod module %q is not %s", modulePath, aiopsModulePath)
	case version == "":
		return "go.mod is missing a go version"
	default:
		return "go.mod is not usable for targeted project verification"
	}
}

func writeGofmtProbe() (string, error) {
	f, err := os.CreateTemp("", "aiops-doctor-gofmt-*.go")
	if err != nil {
		return "", err
	}
	defer closeBody(f)
	if _, err := io.WriteString(f, "package main\n"); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func (r *reportBuilder) checkDockerCompose(ctx context.Context) {
	if r.binaryDeploy() {
		// Docker Compose is irrelevant to a binary deployment; a
		// Docker-less host is the expected case, so don't report it
		// (and don't escalate to FAIL in real mode). #538.
		return
	}
	if runningInContainer() {
		r.warn("Docker Compose", "host Docker Compose check skipped inside the worker container", "Run docker compose config --quiet on the host before docker compose run/up.")
		return
	}
	out, err := r.run(ctx, "docker", []string{"compose", "version"})
	if err != nil {
		status := Warn
		if r.realMode() {
			status = Fail
		}
		r.add(status, "Docker Compose", trimOutput(out, err), "Install Docker with Compose v2 before running the Docker first-run path.")
		return
	}
	r.pass("Docker Compose", strings.TrimSpace(string(out)))
	if _, err := os.Stat("deploy/docker-compose.yml"); err == nil {
		if out, err := r.run(ctx, "docker", []string{"compose", "-f", "deploy/docker-compose.yml", "config", "--quiet"}); err != nil {
			r.fail("Compose config", trimOutput(out, err), "Fix deploy/docker-compose.yml or its required .env interpolation values.")
		} else {
			r.pass("Compose config", "deploy/docker-compose.yml renders")
		}
	}
}

func (r *reportBuilder) checkLinear(ctx context.Context, cfg workflow.Config) {
	if strings.TrimSpace(cfg.Tracker.Kind) != "linear" {
		r.warn("Linear", "tracker.kind is not linear; Linear smoke checks skipped", "Use a Linear workflow for the documented first-run path.")
		return
	}
	if strings.TrimSpace(cfg.Tracker.APIKey) == "" {
		r.fail("Linear API key", "tracker.api_key resolved empty", "Provide LINEAR_API_KEY via the worker environment (a systemd EnvironmentFile, a Docker secret, or a shell export).")
		return
	}
	if !r.realMode() {
		r.pass("Linear API key", "present; live auth skipped in mock mode")
		return
	}
	if err := r.checkLinearGraphQL(ctx, cfg); err != nil {
		r.fail("Linear auth", err.Error(), "Verify the token, project_slug, and Authorization header style.")
		return
	}
	r.pass("Linear auth", "API key authenticated and configured projects are visible")
}

func (r *reportBuilder) checkCodex(ctx context.Context, cfg workflow.Config) { //nolint:gocognit // baseline (#521)
	if !requiresCodex(cfg) {
		r.pass("Codex", "not required for mock runner")
		return
	}
	if usesDefaultCodexCLI(cfg) {
		if _, err := exec.LookPath("codex"); err != nil {
			r.fail("Codex CLI", "codex binary not found on PATH", "Install Codex CLI in this host/container or set codex.command to a runnable app-server wrapper.")
			return
		}
		if out, err := r.run(ctx, "codex", []string{"--version"}); err != nil {
			r.fail("Codex version", trimOutput(out, err), "Fix the Codex installation before dispatching real agents.")
		} else {
			r.pass("Codex version", strings.TrimSpace(string(out)))
		}
		if out, err := r.run(ctx, "codex", []string{"login", "status"}); err != nil {
			r.fail("Codex auth", trimOutput(out, err), "Run codex --login in the same CODEX_HOME/container user context.")
		} else {
			r.pass("Codex auth", firstLine(out))
		}
	} else {
		r.warn("Codex CLI", "version/auth checks skipped for custom codex.command", "Ensure the wrapper uses an authenticated Codex context; the app-server probe validates launch.")
	}
	if cfg.Agent.Default != runner.NameCodexAppServer {
		return
	}
	if err := r.probeCodexAppServer(ctx, cfg); err != nil {
		r.fail("Codex app-server", err.Error(), "Check CODEX_HOME, codex.command, and app-server support in the installed Codex version.")
	} else {
		r.pass("Codex app-server", "started and answered a JSON-RPC probe")
	}
	r.checkCodexAuthModel(cfg)
}

// checkCodexAuthModel reports the production auth mode and model configuration
// the app-server worker will use, so a preflight distinguishes a missing model
// API key from an expired ChatGPT login and surfaces model selection instead of
// leaving it hidden in a copied host config.toml (#465). Secrets are never
// printed: API-key presence is checked without echoing the value, and only the
// non-secret model/provider/effort keys are read from config.toml.
func (r *reportBuilder) checkCodexAuthModel(cfg workflow.Config) {
	home := codexHomeDir()
	if codexAPIKeyAuthSelected(cfg) {
		r.checkCodexAPIKeyAuth()
	} else {
		r.checkCodexLoginAuth(home)
	}
	r.checkCodexModelConfig(home)
}

func (r *reportBuilder) checkCodexAPIKeyAuth() {
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" {
		r.pass("Codex auth mode", "API key via OPENAI_API_KEY passthrough")
		return
	}
	status := Warn
	if r.realMode() {
		status = Fail
	}
	r.add(status, "Codex auth mode", "OPENAI_API_KEY is in codex.env_passthrough but resolved empty",
		"Provide the model API key in OPENAI_API_KEY via the worker environment (a Docker secret or systemd EnvironmentFile); never pass it on a command line.")
}

func (r *reportBuilder) checkCodexLoginAuth(home string) {
	detail := "ChatGPT/Codex login"
	if home != "" {
		detail += " (auth.json under " + home + ")"
	}
	r.pass("Codex auth mode", detail)
	if !r.realMode() || home == "" {
		return
	}
	if err := codexHomeWritable(home); err != nil {
		r.warn("Codex auth refresh", "CODEX_HOME is not writable: "+err.Error(),
			"Mount CODEX_HOME as a restricted writable volume so ChatGPT-login token refresh can persist for long-lived workers.")
	}
}

func (r *reportBuilder) checkCodexModelConfig(home string) {
	if home == "" {
		return
	}
	configPath := filepath.Join(home, "config.toml")
	model, provider, effort, ok := readCodexModelConfig(configPath)
	if !ok {
		r.warn("Codex model config", "no model declared in "+configPath,
			"Declare model (and optional model_provider/model_reasoning_effort) in a tracked, non-secret config.toml so model selection is auditable instead of relying on a copied host config.")
		return
	}
	detail := "model=" + model
	if provider != "" {
		detail += ", provider=" + provider
	}
	if effort != "" {
		detail += ", reasoning_effort=" + effort
	}
	r.pass("Codex model config", detail)
}

// effectiveSandboxMode returns the policy the app-server actually applies per
// turn: the explicit codex.turn_sandbox_policy when set (it overrides
// thread_sandbox in startTurn), else the thread_sandbox-derived default. Gating
// must follow the effective policy — e.g. a turn_sandbox_policy override to
// dangerFullAccess or externalSandbox (with thread_sandbox left at
// workspace-write) runs agent commands WITHOUT codex's own userns sandbox, so
// probing would be a false FAIL (codex review #549).
func effectiveSandboxMode(cfg workflow.Config) string {
	switch cfg.Codex.TurnSandboxPolicy.Type {
	case workflow.CodexSandboxDangerFullAccess:
		return "danger-full-access"
	case workflow.CodexSandboxExternalSandbox:
		return "external"
	case workflow.CodexSandboxReadOnly:
		return "read-only"
	case workflow.CodexSandboxWorkspaceWrite:
		return "workspace-write"
	}
	switch cfg.Codex.ThreadSandbox {
	case "danger-full-access":
		return "danger-full-access"
	case "read-only":
		return "read-only"
	default:
		return "workspace-write"
	}
}

// codexUsesUserNamespaceSandbox reports whether the effective mode makes codex
// run agent commands in its own bwrap user-namespace sandbox (read-only or
// workspace-write). danger-full-access (no sandbox) and external (host-provided
// isolation; codex skips its own) do not, so the userns probe is moot for them.
func codexUsesUserNamespaceSandbox(mode string) bool {
	return mode == "read-only" || mode == "workspace-write"
}

func (r *reportBuilder) checkSandbox(ctx context.Context, cfg workflow.Config) {
	if !requiresCodex(cfg) {
		return
	}
	mode := effectiveSandboxMode(cfg)
	if runningInContainerFn() && codexUsesUserNamespaceSandbox(mode) {
		r.warn("Codex sandbox", "containerized Codex may not support workspace-write namespaces", "Use the documented Docker-isolated profile or enable the required kernel/userns support.")
		return
	}
	if !codexUsesUserNamespaceSandbox(mode) {
		r.pass("Codex sandbox", "effective sandbox ("+mode+") runs agent commands without a user-namespace sandbox")
		return
	}
	// The live probe is authoritative only for a real-mode binary deploy on
	// Linux: codex sandboxes agent shell commands with a bwrap user namespace
	// there, so a host that blocks unprivileged user namespaces is exactly what
	// the agent will hit. On macOS codex uses the seatbelt sandbox (no user
	// namespaces), under --deploy=docker the sandbox runs inside the container
	// (a different userns policy than this host), and mock mode dispatches
	// nothing — keep the static classification in those cases.
	if !r.realMode() || !r.binaryDeploy() || runtime.GOOS != "linux" {
		r.pass("Codex sandbox", "selected profile is compatible with this preflight")
		return
	}
	r.probeCodexSandbox(ctx, cfg)
}

// userNamespaceRemediation explains how to restore the host capability codex's
// bwrap sandbox needs. The safer options lead; the host-wide sysctl is last
// because it weakens unprivileged user namespaces for every process. Phrased
// "most commonly" because the probe runs codex's real sandbox and surfaces its
// actual error, which is authoritative even if the cause is not the AppArmor
// userns restriction.
const userNamespaceRemediation = "codex sandboxes agent shell commands in a bubblewrap (bwrap) user namespace, which this host most commonly blocks because unprivileged user namespaces are restricted (Ubuntu 24.04+: kernel.apparmor_restrict_unprivileged_userns=1) — see the codex error above for the exact failure. Prefer: install an AppArmor profile that permits codex's bwrap user namespaces, or run the worker in a container that allows them, or set codex.thread_sandbox: danger-full-access only in an already-isolated environment. Last resort (weakens host security — re-enables unprivileged user namespaces process-wide): `sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0`, persisted via /etc/sysctl.d/99-userns.conf."

// probeCodexSandbox runs a trivial command through codex's OWN sandbox
// (`codex sandbox -- /bin/true`) so a host where codex cannot start a sandboxed
// command fails preflight instead of after a full agent turn is dispatched and
// tokens burned. The static check passes even when
// kernel.apparmor_restrict_unprivileged_userns=1 blocks codex's bwrap, so every
// agent command then dies at runtime (e.g. "bwrap: setting up uid map:
// Permission denied") (#542).
//
// This drives codex's vendored bwrap with codex's exact flags — not a
// hand-rolled system-bwrap proxy — so it is authoritative: it reproduces the
// real failure mode (uid-map, netns, or an AppArmor profile that covers a
// different binary path), which a system-bwrap proxy can miss (codex review
// #549). codex 0.135's `sandbox` subcommand does not forward the root
// `--sandbox` option, so the probe takes no mode argument; its default
// sandboxed mode uses the same bwrap user namespace every sandboxed profile
// needs, and checkSandbox has already skipped the only unsandboxed profile
// (effective danger-full-access) before reaching here (codex review #549).
// /bin/true needs no model turn, auth, or network. Routed through r.run so the
// probe is unit-testable. A custom codex.command can't be assumed to support
// `codex sandbox`, so it is WARN.
func (r *reportBuilder) probeCodexSandbox(ctx context.Context, cfg workflow.Config) {
	if !usesDefaultCodexCLI(cfg) {
		r.warn("Codex sandbox", "custom codex.command: skipped the codex sandbox self-test",
			"Verify the wrapper sandboxes agent commands, e.g. `codex sandbox -- /bin/true`.")
		return
	}
	out, err := r.run(ctx, "codex", []string{"sandbox", "--", "/bin/true"})
	detail := trimOutput(out, err)
	switch {
	case err == nil:
		r.pass("Codex sandbox", "codex sandbox started a command on this host")
	case errors.Is(err, exec.ErrNotFound):
		r.warn("Codex sandbox", "codex not found on PATH; cannot run the codex sandbox self-test",
			"Install Codex CLI on this host, then rerun worker --doctor --deploy=binary --mode=real.")
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		// A slow codex cold-start (not a sandbox denial) should not block
		// preflight with a misleading userns remediation.
		r.warn("Codex sandbox", "codex sandbox self-test timed out before completing; could not determine sandbox health",
			"Re-run worker --doctor; if it persists, check codex startup time on this host.")
	case codexLacksSandboxSubcommand(detail):
		// An older/forked codex without `codex sandbox` is a version problem,
		// not a host userns problem — warn to upgrade rather than FAIL.
		r.warn("Codex sandbox", "this codex build does not support `codex sandbox`; cannot self-test the sandbox: "+detail,
			"Upgrade Codex CLI to a build with the `sandbox` subcommand, or verify the sandbox manually: codex sandbox -- /bin/true.")
	default:
		r.fail("Codex sandbox", "codex sandbox cannot start commands: "+detail, userNamespaceRemediation)
	}
}

// codexLacksSandboxSubcommand reports whether codex's output is a CLI usage
// error for an unrecognized `sandbox` subcommand (an older/forked codex) rather
// than a real sandbox-startup failure, so a version mismatch warns (upgrade
// codex) instead of FAILing with the userns remediation.
func codexLacksSandboxSubcommand(out string) bool {
	out = strings.ToLower(out)
	return strings.Contains(out, "unrecognized subcommand") ||
		strings.Contains(out, "no such subcommand") ||
		strings.Contains(out, "unknown command") ||
		(strings.Contains(out, "unexpected argument") && strings.Contains(out, "sandbox"))
}

func (r *reportBuilder) checkGitHubAgent(ctx context.Context, cfg workflow.Config) {
	if r.opts.GitHubIssue == 0 {
		return
	}
	if !requiresCodex(cfg) {
		r.warn("GitHub agent credentials", "not checked because the selected agent is not Codex", "Use --github-issue only with a Codex workflow that expects agent-side GitHub access.")
		return
	}
	repo, err := selectGitHubRepo(cfg, r.opts.GitHubRepo)
	if err != nil {
		r.fail("GitHub agent credentials", err.Error(), "Set repo.owner, repo.name, and repo.clone_url, or pass --github-repo owner/name (or the exact clone_url) for the GitHub repository the agent will access.")
		return
	}
	env := runner.AgentEnvForPreflight(cfg.Agent.Default, cfg)
	if out, err := r.runEnv(ctx, "gh", []string{"issue", "view", strconv.Itoa(r.opts.GitHubIssue), "--repo", repo.fullName(), "--json", "number,title,url"}, env); err != nil {
		r.fail("GitHub agent gh auth", safeCommandFailure("gh issue view", out, err), "Create file-backed gh auth for the aiops user; do not rely on GH_TOKEN/GITHUB_TOKEN in the worker environment.")
		return
	}
	r.pass("GitHub agent gh auth", fmt.Sprintf("agent env can read %s#%d", repo.fullName(), r.opts.GitHubIssue))
	probeDir, cleanup, err := r.prepareGitPushProbe(ctx, env)
	if err != nil {
		r.fail("GitHub agent git push", err.Error(), "Create a writable temporary directory for the doctor git probe.")
		return
	}
	defer cleanup()
	if out, err := r.runEnv(ctx, "git", []string{"-C", probeDir, "push", "--dry-run", repo.CloneURL, "HEAD:refs/heads/aiops-doctor-preflight"}, env); err != nil {
		r.fail("GitHub agent git push", safeCommandFailure("git push --dry-run", out, err), "Configure deploy-key or gh git credential-helper access for the aiops user, then rerun worker --doctor --mode=real --github-issue.")
		return
	}
	r.pass("GitHub agent git push", "agent env passed git push --dry-run")
}

func (r *reportBuilder) prepareGitPushProbe(ctx context.Context, env []string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "aiops-doctor-git-*")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	steps := [][]string{
		{"-C", dir, "init", "-q"},
		{"-C", dir, "config", "user.email", "aiops-doctor@example.invalid"},
		{"-C", dir, "config", "user.name", "aiops doctor"},
		{"-C", dir, "commit", "--allow-empty", "-m", "aiops doctor preflight", "-q"},
	}
	for _, args := range steps {
		if out, err := r.runEnv(ctx, "git", args, env); err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("%s", safeCommandFailure("git "+strings.Join(args, " "), out, err))
		}
	}
	return dir, cleanup, nil
}

func (r *reportBuilder) checkDashboard(ctx context.Context) {
	if strings.TrimSpace(r.opts.DashboardURL) == "" {
		r.warn("Dashboard state API", "not checked; no dashboard URL supplied", "Pass --dashboard-url while the worker is running to verify state API auth.")
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(r.opts.DashboardURL, "/")+"/api/v1/state", nil)
	if err != nil {
		r.fail("Dashboard state API", err.Error(), "Pass a valid dashboard base URL.")
		return
	}
	if tok := os.Getenv("AIOPS_STATE_API_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := r.opts.HTTPClient.Do(req)
	if err != nil {
		r.fail("Dashboard state API", err.Error(), "Start the worker or fix the dashboard URL/network mapping.")
		return
	}
	defer closeBody(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		r.fail("Dashboard state API", resp.Status, "Set AIOPS_STATE_API_TOKEN or use the loopback-only URL.")
		return
	}
	r.pass("Dashboard state API", resp.Status)
}

func (r *reportBuilder) checkLinearGraphQL(ctx context.Context, cfg workflow.Config) error { //nolint:gocognit // baseline (#521)
	query := `query Doctor($projectSlug: String!) { viewer { id name } projects(filter: { slugId: { eq: $projectSlug } }, first: 1) { nodes { id slugId name } } }`
	projectSlugs := linearProjectSlugs(cfg)
	if len(projectSlugs) == 0 {
		return fmt.Errorf("linear project_slug is required at tracker.project_slug or services[].tracker.project_slug")
	}
	endpoint := strings.TrimSpace(cfg.Tracker.Endpoint)
	if endpoint == "" {
		endpoint = tracker.DefaultLinearEndpoint
	}
	for _, projectSlug := range projectSlugs {
		var out struct {
			Data struct {
				Projects struct {
					Nodes []struct {
						ID string `json:"id"`
					} `json:"nodes"`
				} `json:"projects"`
			} `json:"data"`
			Errors []map[string]any `json:"errors"`
		}
		body, _ := json.Marshal(map[string]any{"query": query, "variables": map[string]any{"projectSlug": projectSlug}})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", cfg.Tracker.APIKey)
		resp, err := r.opts.HTTPClient.Do(req)
		if err != nil {
			return err
		}
		if err := decodeLinearProjectProbe(resp, &out); err != nil {
			return err
		}
		if len(out.Errors) > 0 {
			return fmt.Errorf("linear GraphQL errors for project_slug %q: %v", projectSlug, out.Errors)
		}
		if len(out.Data.Projects.Nodes) == 0 {
			return fmt.Errorf("project_slug %q is not visible to the token", projectSlug)
		}
	}
	return nil
}

func decodeLinearProjectProbe(resp *http.Response, out any) error {
	defer closeBody(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("linear returned %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (r *reportBuilder) probeCodexAppServer(ctx context.Context, cfg workflow.Config) error { //nolint:gocognit // baseline (#521)
	probe := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"clientInfo":{"name":"aiops-doctor","title":"aiops doctor","version":"0.1.0"}}}` + "\n"
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	env := os.Environ()
	cmd, _, err := runner.NewCodexAppServerCommand(probeCtx, cfg, env)
	if err != nil {
		return err
	}
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		terminate(cmd)
		_ = cmd.Wait()
	}()
	if _, err := io.WriteString(stdin, probe); err != nil {
		return err
	}
	defer closeBody(stdin)
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		ok, err := validateAppServerProbeLine(sc.Bytes())
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if probeCtx.Err() != nil {
		return probeCtx.Err()
	}
	return fmt.Errorf("no JSON-RPC response: %s", strings.TrimSpace(stderr.String()))
}

func validateAppServerProbeLine(line []byte) (bool, error) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return false, nil
	}
	if strings.TrimSpace(string(msg.ID)) != "1" {
		return false, nil
	}
	if len(msg.Error) > 0 && strings.TrimSpace(string(msg.Error)) != "null" {
		return false, fmt.Errorf("app-server initialize error: %s", strings.TrimSpace(string(msg.Error)))
	}
	if len(msg.Result) == 0 || strings.TrimSpace(string(msg.Result)) == "null" {
		return false, fmt.Errorf("unexpected app-server response: %s", strings.TrimSpace(string(line)))
	}
	return true, nil
}

func terminate(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func closeBody(body io.Closer) {
	_ = body.Close()
}

func (r *reportBuilder) requiredBinary(name string) {
	if _, err := exec.LookPath(name); err != nil {
		r.fail(name, "not found on PATH", r.installFix(name))
		return
	}
	r.pass(name, "found on PATH")
}

func (r *reportBuilder) realMode() bool {
	return r.opts.Mode == "real"
}

func (r *reportBuilder) binaryDeploy() bool {
	return r.opts.Deploy == "binary"
}

// installFix renders an "install this tool" remediation matching the
// deployment target: a host PATH for a binary deploy, the worker image
// for a container deploy. A binary operator has no image to edit, so the
// container-only phrasing misleads them (#538).
func (r *reportBuilder) installFix(what string) string {
	if r.binaryDeploy() {
		return "Install " + what + " on this host and ensure it is on PATH."
	}
	return "Install " + what + " in the worker image."
}

func (r *reportBuilder) run(ctx context.Context, name string, args []string) ([]byte, error) {
	return r.runWithTimeout(ctx, defaultCommandTimeout, name, args)
}

func (r *reportBuilder) runWithTimeout(ctx context.Context, timeout time.Duration, name string, args []string) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return r.opts.Runner(runCtx, name, args, nil, nil)
}

func (r *reportBuilder) runEnv(ctx context.Context, name string, args []string, env []string) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return r.opts.Runner(runCtx, name, args, env, nil)
}

func (r *reportBuilder) pass(name, detail string) {
	r.add(Pass, name, detail, "")
}

func (r *reportBuilder) warn(name, detail, fix string) {
	r.add(Warn, name, detail, fix)
}

func (r *reportBuilder) fail(name, detail, fix string) {
	r.add(Fail, name, detail, fix)
}

func (r *reportBuilder) add(status Status, name, detail, fix string) {
	r.checks = append(r.checks, Check{Status: status, Name: name, Detail: redact(detail), Fix: redact(fix)})
}

func defaultRunner(ctx context.Context, name string, args []string, env []string, stdin io.Reader) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if env == nil {
		cmd.Env = os.Environ()
	} else {
		cmd.Env = env
		// exec.CommandContext resolves a bare name against the worker's PATH.
		// For the GitHub agent preflight to be authoritative, re-resolve the
		// lookup against the PATH that the agent will actually see in cmd.Env,
		// and overwrite both cmd.Path and cmd.Err (set by the worker-PATH probe).
		if !strings.ContainsRune(name, os.PathSeparator) {
			resolved, lookErr := lookPathInEnv(name, env)
			if lookErr != nil {
				return nil, lookErr
			}
			cmd.Path = resolved
			cmd.Err = nil
		}
	}
	cmd.Stdin = stdin
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out, ctx.Err()
	}
	return out, err
}

// lookPathInEnv resolves name against the PATH entry in env, mirroring
// exec.LookPath's executable-bit check on Unix without touching process state.
func lookPathInEnv(name string, env []string) (string, error) { //nolint:gocognit // baseline (#521)
	var pathVal string
	for _, kv := range env {
		if rest, ok := strings.CutPrefix(kv, "PATH="); ok {
			pathVal = rest
		}
	}
	if pathVal == "" {
		return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
	}
	for _, dir := range filepath.SplitList(pathVal) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err != nil {
			continue
		}
		if info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

func requiresCodex(cfg workflow.Config) bool {
	return cfg.Agent.Default == runner.NameCodexAppServer
}

// codexAPIKeyAuthSelected reports whether the workflow opts the agent into
// model API-key auth by passing OPENAI_API_KEY through to the Codex
// subprocess. Without the passthrough the key never reaches the agent and the
// app-server falls back to the ChatGPT/Codex login in CODEX_HOME.
func codexAPIKeyAuthSelected(cfg workflow.Config) bool {
	for _, name := range cfg.Codex.EnvPassthrough {
		if strings.EqualFold(strings.TrimSpace(name), "OPENAI_API_KEY") {
			return true
		}
	}
	return false
}

// codexHomeDir resolves the directory Codex reads auth and config from, the
// same way the CLI does: CODEX_HOME wins, otherwise $HOME/.codex.
func codexHomeDir() string {
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		return home
	}
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		return filepath.Join(home, ".codex")
	}
	return ""
}

// codexHomeWritable confirms the worker can create files in CODEX_HOME, which
// a long-lived ChatGPT-login deployment needs so Codex can persist refreshed
// tokens. The probe file is removed before returning.
func codexHomeWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".aiops-doctor-write-*")
	if err != nil {
		return err
	}
	name := f.Name()
	closeBody(f)
	_ = os.Remove(name)
	return nil
}

// readCodexModelConfig extracts the non-secret top-level model selection keys
// from a Codex config.toml. It deliberately ignores every `[table]` section so
// provider blocks (which may name secret env keys) are never surfaced. ok is
// true only when a model is declared.
func readCodexModelConfig(path string) (model, provider, effort string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", "", false
	}
	values := topLevelTOMLScalars(string(data))
	model = values["model"]
	return model, values["model_provider"], values["model_reasoning_effort"], model != ""
}

// topLevelTOMLScalars returns the bare top-level `key = value` scalars in a
// TOML document, skipping every `[table]` section so nested provider blocks
// (which can name secret env keys) are never surfaced.
func topLevelTOMLScalars(data string) map[string]string {
	values := map[string]string{}
	inTable := false
	for _, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "["):
			inTable = true
		case inTable:
		default:
			if key, val, found := strings.Cut(line, "="); found {
				values[strings.TrimSpace(key)] = tomlScalarString(val)
			}
		}
	}
	return values
}

// tomlScalarString reads a quoted or bare TOML scalar, dropping a trailing
// inline comment on bare values. It is intentionally minimal: doctor only
// surfaces the value for display, so full TOML parsing is not warranted.
func tomlScalarString(v string) string {
	v = strings.TrimSpace(v)
	if len(v) > 0 && (v[0] == '"' || v[0] == '\'') {
		if j := strings.IndexByte(v[1:], v[0]); j >= 0 {
			return v[1 : 1+j]
		}
		return strings.Trim(v, string(v[0]))
	}
	if i := strings.IndexByte(v, '#'); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

func linearProjectSlugs(cfg workflow.Config) []string {
	seen := map[string]bool{}
	var slugs []string
	add := func(raw string) {
		slug := strings.TrimSpace(raw)
		if slug == "" || seen[slug] {
			return
		}
		seen[slug] = true
		slugs = append(slugs, slug)
	}
	add(cfg.Tracker.ProjectSlug)
	return slugs
}

func usesDefaultCodexCLI(cfg workflow.Config) bool {
	command := strings.TrimSpace(cfg.Codex.Command)
	return command == "" || command == "codex app-server" || strings.HasPrefix(command, "codex app-server ")
}

func workflowNeedsSSH(cfg workflow.Config) bool {
	for _, cloneURL := range workflowCloneURLs(cfg) {
		if cloneURLNeedsSSH(cloneURL) {
			return true
		}
	}
	return false
}

func workflowCloneURLs(cfg workflow.Config) []string {
	if strings.TrimSpace(cfg.Repo.CloneURL) == "" {
		return nil
	}
	return []string{cfg.Repo.CloneURL}
}

type githubRepo struct {
	Owner    string
	Name     string
	CloneURL string
}

func (r githubRepo) fullName() string {
	return r.Owner + "/" + r.Name
}

func selectGitHubRepo(cfg workflow.Config, target string) (githubRepo, error) {
	repos := githubRepos(cfg)
	if len(repos) == 0 {
		return githubRepo{}, fmt.Errorf("no GitHub repo owner/name and clone_url found in workflow")
	}
	if target = strings.TrimSpace(target); target != "" && len(matchGitHubRepos(repos, target)) == 0 {
		// target may itself be a clone_url carrying basic-auth userinfo;
		// mask it before echoing (owner/name targets pass through unchanged).
		return githubRepo{}, fmt.Errorf("github repo %q not found in workflow", workflow.MaskCloneURL(target))
	}
	return repos[0], nil
}

// matchGitHubRepos returns the configured repos selected by target, which may be
// an owner/name, a raw clone URL, or the masked clone URL the ambiguity error
// displays (see workflow.MaskCloneURL) so an operator never has to retype an
// embedded token on the command line. Exact owner/name and raw clone-URL matches
// take precedence: a fully specified target is never widened by a masked-form
// collision with a different repo, and a bare clone URL that exactly matches one
// repo selects it even if another repo masks to the same value.
// Clone-URL comparison is a normalized string match (see normalizeCloneURL), so
// equivalent SSH spellings such as scp-style git@host:o/r.git versus
// ssh://git@host/o/r.git are not folded; pass the exact configured form.
func matchGitHubRepos(repos []githubRepo, target string) []githubRepo {
	var exact, masked []githubRepo
	for _, repo := range repos {
		switch {
		case strings.EqualFold(repo.fullName(), target) || sameCloneURL(repo.CloneURL, target):
			exact = append(exact, repo)
		case sameCloneURL(workflow.MaskCloneURL(repo.CloneURL), target):
			masked = append(masked, repo)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return masked
}

func githubRepos(cfg workflow.Config) []githubRepo {
	if repo, ok := githubRepoFromConfig(cfg.Repo); ok {
		return []githubRepo{repo}
	}
	return nil
}

func githubRepoFromConfig(repo workflow.RepoConfig) (githubRepo, bool) {
	owner := strings.TrimSpace(repo.Owner)
	name := strings.TrimSpace(repo.Name)
	cloneURL := strings.TrimSpace(repo.CloneURL)
	if owner == "" || name == "" || cloneURL == "" || !isGitHubCloneURL(cloneURL) {
		return githubRepo{}, false
	}
	return githubRepo{Owner: owner, Name: name, CloneURL: cloneURL}, true
}

// normalizeCloneURL canonicalizes a clone URL for identity comparison. GitHub
// treats the host and owner/name path case-insensitively, so lowercasing folds
// case-variant duplicates while keeping distinct protocols (https vs ssh) apart.
func normalizeCloneURL(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func sameCloneURL(a, b string) bool {
	return normalizeCloneURL(a) == normalizeCloneURL(b)
}

func isGitHubCloneURL(raw string) bool {
	cloneURL := strings.TrimSpace(raw)
	if strings.Contains(cloneURL, "://") {
		u, err := url.Parse(cloneURL)
		if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
			return false
		}
		scheme := strings.ToLower(u.Scheme)
		return scheme == "https" || scheme == "ssh" || scheme == "git+ssh"
	}
	return strings.HasPrefix(strings.ToLower(cloneURL), "git@github.com:")
}

func cloneURLNeedsSSH(raw string) bool {
	cloneURL := strings.TrimSpace(raw)
	if cloneURL == "" {
		return false
	}
	if strings.Contains(cloneURL, "://") {
		u, err := url.Parse(cloneURL)
		if err != nil {
			return false
		}
		switch strings.ToLower(u.Scheme) {
		case "ssh", "git+ssh":
			return true
		default:
			return false
		}
	}
	at := strings.Index(cloneURL, "@")
	colon := strings.Index(cloneURL, ":")
	return at > 0 && colon > at
}

func writeTextReport(w io.Writer, report Report) {
	for _, check := range report.Checks {
		_, _ = fmt.Fprintf(w, "%s %s", check.Status, check.Name)
		if check.Detail != "" {
			_, _ = fmt.Fprintf(w, ": %s", check.Detail)
		}
		_, _ = fmt.Fprintln(w)
		if check.Fix != "" && check.Status != Pass {
			_, _ = fmt.Fprintf(w, "     Fix: %s\n", check.Fix)
		}
	}
}

func trimOutput(out []byte, err error) string {
	msg := strings.TrimSpace(string(out))
	if msg == "" && err != nil {
		msg = err.Error()
	}
	return msg
}

func safeCommandFailure(command string, out []byte, err error) string {
	_ = out
	var detail string
	if err != nil {
		detail = err.Error()
	}
	if detail == "" {
		return command + " failed"
	}
	return command + " failed: " + detail
}

func readGoMod(path string) (string, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	var modulePath, version string
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			switch fields[0] {
			case "module":
				modulePath = fields[1]
			case "go":
				version = fields[1]
			}
		}
	}
	return modulePath, version
}

// goVersion is a parsed Go release version. patchSet records whether the source
// string carried a patch component, so a go.mod `go` directive pinned to
// major.minor (`go 1.25`) is distinguished from an exact patch (`go 1.25.11`).
type goVersion struct {
	major, minor, patch int
	patchSet            bool
}

// goVersionCompatible reports whether the installed toolchain (from `go version`
// output) satisfies the go.mod `go` directive, comparing at the precision go.mod
// declares. When go.mod pins an exact patch (`go 1.25.11`), a host on an older
// patch of the same minor (`go1.25.10`) is rejected: GOTOOLCHAIN=local cannot
// build it and GOTOOLCHAIN=auto would silently download the floor, so reporting
// it compatible would mislead the operator. When go.mod pins only major.minor,
// the patch is not compared.
func goVersionCompatible(output, goModVersion string) bool {
	got, gotOK := goVersionFromOutput(output)
	want, wantOK := parseGoVersion(goModVersion)
	if !gotOK || !wantOK {
		return false
	}
	if got.major != want.major {
		return got.major > want.major
	}
	if got.minor != want.minor {
		return got.minor > want.minor
	}
	// Same major.minor: the patch only gates when go.mod declared one.
	return !want.patchSet || got.patch >= want.patch
}

func goVersionFromOutput(output string) (goVersion, bool) {
	for _, field := range strings.Fields(output) {
		if strings.HasPrefix(field, "go1.") {
			return parseGoVersion(strings.TrimPrefix(field, "go"))
		}
	}
	return goVersion{}, false
}

// parseGoVersion reads a `major.minor[.patch]` version, tolerating a trailing
// pre-release suffix (e.g. `1.25rc1`). It requires at least major.minor; a
// present patch component sets patchSet.
func parseGoVersion(version string) (goVersion, bool) {
	var v goVersion
	n, _ := fmt.Sscanf(version, "%d.%d.%d", &v.major, &v.minor, &v.patch)
	if n < 2 {
		return goVersion{}, false
	}
	v.patchSet = n >= 3
	return v, true
}

func firstLine(out []byte) string {
	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if line == "" {
		return "ok"
	}
	return line
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// runningInContainerFn is a seam so unit tests that construct a reportBuilder
// and call checkSandbox directly are not skewed by the host's container markers
// (the container branch precedes the mode/deploy/probe branches).
var runningInContainerFn = runningInContainer

func runningInContainer() bool {
	return fileExists("/.dockerenv") || fileExists("/run/.containerenv")
}

func redact(s string) string {
	for _, env := range []string{"LINEAR_API_KEY", "GITHUB_TOKEN", "GITEA_TOKEN", "OPENAI_API_KEY", "AIOPS_STATE_API_TOKEN"} {
		if v := os.Getenv(env); v != "" {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	return s
}
