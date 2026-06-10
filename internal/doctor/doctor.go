package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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
