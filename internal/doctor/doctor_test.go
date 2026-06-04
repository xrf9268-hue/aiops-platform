package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestBuildReportMockModeCatchesMissingLinearKeyDuringWorkflowLoad(t *testing.T) {
	path := writeWorkflow(t, "linear", "$AIOPS_TEST_MISSING_LINEAR_KEY")
	report := BuildReport(context.Background(), Options{
		WorkflowPath: path,
		Mode:         "mock",
		Runner:       passingRunner,
	})

	check := findCheck(t, report, "Workflow")
	if check.Status != Fail {
		t.Fatalf("Workflow status = %s; want FAIL", check.Status)
	}
	if !strings.Contains(check.Detail, "missing_tracker_api_key") {
		t.Fatalf("detail = %q; want missing_tracker_api_key", check.Detail)
	}
}

func TestBuildReportRealModeAuthenticatesLinearProject(t *testing.T) {
	installFakeCodex(t)
	linear := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "lin-test" {
			t.Fatalf("Authorization = %q; want raw Linear API key", got)
		}
		_, _ = w.Write([]byte(`{"data":{"viewer":{"id":"u","name":"Ada"},"projects":{"nodes":[{"id":"p","slugId":"platform","name":"Platform"}]}}}`))
	}))
	defer linear.Close()
	path := writeWorkflowWithEndpoint(t, linear.URL, "codex-app-server")
	t.Setenv("AIOPS_TEST_LINEAR_KEY", "lin-test")
	report := BuildReport(context.Background(), Options{
		WorkflowPath: path,
		Mode:         "real",
		Runner:       fakeRealRunner,
	})

	if got := findCheck(t, report, "Linear auth").Status; got != Pass {
		t.Fatalf("Linear auth status = %s; want PASS", got)
	}
	if got := findCheck(t, report, "Codex auth").Status; got != Pass {
		t.Fatalf("Codex auth status = %s; want PASS", got)
	}
}

// codexSandboxCfg is a minimal codex-app-server workflow config with the given
// thread sandbox, for the checkSandbox user-namespace probe tests (#542).
func codexSandboxCfg(threadSandbox string) workflow.Config {
	return workflow.Config{
		Agent: workflow.AgentConfig{Default: "codex-app-server"},
		Codex: workflow.CommandConfig{ThreadSandbox: threadSandbox},
	}
}

// sandboxProbeRunner answers the `codex sandbox` self-test with out/err,
// records whether codex was invoked (so a test can assert the probe was
// skipped), and captures its argv into gotArgs (nil to ignore) so a test can
// assert the load-bearing probe args survive refactors. Any other command is
// unexpected on this path.
func sandboxProbeRunner(out []byte, err error, called *bool, gotArgs *[]string) CommandRunner {
	return func(_ context.Context, name string, args []string, _ []string, _ io.Reader) ([]byte, error) {
		if name == "codex" {
			*called = true
			if gotArgs != nil {
				*gotArgs = args
			}
			return out, err
		}
		return nil, fmt.Errorf("unexpected command %q", name)
	}
}

// assertCodexSandboxProbeArgs pins the exact load-bearing invocation (the
// validated `--sandbox <mode>` selecting the configured profile, the `sandbox`
// subcommand, the `--` separator, and `/bin/true`) so a weaker probe — omitting
// the mode, dropping `--`, reordering — is caught by tests.
func assertCodexSandboxProbeArgs(t *testing.T, args []string) {
	t.Helper()
	want := []string{"sandbox", "--", "/bin/true"}
	if !slices.Equal(args, want) {
		t.Errorf("codex sandbox probe args = %v; want %v", args, want)
	}
}

// noContainer pins runningInContainerFn to false so a checkSandbox unit test is
// not skewed by the host's container markers (the container branch precedes the
// mode/deploy/probe branches).
func noContainer(t *testing.T) {
	t.Helper()
	old := runningInContainerFn
	runningInContainerFn = func() bool { return false }
	t.Cleanup(func() { runningInContainerFn = old })
}

// requireLinuxProbe skips a test that depends on the live probe running, which
// happens only on a Linux binary deploy (codex sandboxes agent commands with
// bwrap user namespaces there; macOS uses seatbelt).
func requireLinuxProbe(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("codex sandbox probe runs only on linux; GOOS=%s", runtime.GOOS)
	}
}

func TestCheckSandboxWarnsInContainer(t *testing.T) {
	old := runningInContainerFn
	runningInContainerFn = func() bool { return true }
	t.Cleanup(func() { runningInContainerFn = old })
	var called bool
	r := &reportBuilder{opts: Options{Mode: "real", Deploy: "binary",
		Runner: sandboxProbeRunner([]byte(""), nil, &called, nil)}}
	r.checkSandbox(context.Background(), codexSandboxCfg("workspace-write"))
	if called {
		t.Fatal("checkSandbox ran the host probe inside a container; the container branch must short-circuit first")
	}
	if c := findCheck(t, Report{Checks: r.checks}, "Codex sandbox"); c.Status != Warn {
		t.Errorf("checkSandbox(container) status = %s; want WARN", c.Status)
	}
}

func TestCheckSandboxBinaryRealFailsWhenUserNamespaceBlocked(t *testing.T) {
	requireLinuxProbe(t)
	noContainer(t)
	var called bool
	var args []string
	r := &reportBuilder{opts: Options{Mode: "real", Deploy: "binary",
		Runner: sandboxProbeRunner([]byte("bwrap: setting up uid map: Permission denied\n"), errors.New("exit status 1"), &called, &args)}}
	r.checkSandbox(context.Background(), codexSandboxCfg("workspace-write"))
	if !called {
		t.Fatal("checkSandbox(real,binary) did not run the codex sandbox probe")
	}
	assertCodexSandboxProbeArgs(t, args)
	c := findCheck(t, Report{Checks: r.checks}, "Codex sandbox")
	if c.Status != Fail {
		t.Errorf("checkSandbox(real,binary,userns-blocked) status = %s; want FAIL", c.Status)
	}
	if !strings.Contains(c.Detail, "uid map") {
		t.Errorf("detail = %q; want codex's sandbox error surfaced", c.Detail)
	}
	if !strings.Contains(c.Fix, "apparmor_restrict_unprivileged_userns") {
		t.Errorf("fix = %q; want the AppArmor unprivileged-userns remediation", c.Fix)
	}
}

func TestCheckSandboxBinaryRealPassesWhenSandboxStarts(t *testing.T) {
	requireLinuxProbe(t)
	noContainer(t)
	var called bool
	var args []string
	r := &reportBuilder{opts: Options{Mode: "real", Deploy: "binary",
		Runner: sandboxProbeRunner([]byte(""), nil, &called, &args)}}
	r.checkSandbox(context.Background(), codexSandboxCfg("workspace-write"))
	if !called {
		t.Fatal("checkSandbox(real,binary) did not run the codex sandbox probe")
	}
	assertCodexSandboxProbeArgs(t, args)
	c := findCheck(t, Report{Checks: r.checks}, "Codex sandbox")
	if c.Status != Pass {
		t.Errorf("checkSandbox(real,binary,sandbox-ok) status = %s; want PASS (detail=%q)", c.Status, c.Detail)
	}
	if !strings.Contains(c.Detail, "codex sandbox") {
		t.Errorf("detail = %q; want it to confirm codex sandbox started a command", c.Detail)
	}
}

// TestCheckSandboxFailsAuthoritativelyOnNonUidmapError pins that probing codex's
// REAL sandbox is authoritative: a failure that is NOT the uid-map message (e.g.
// the netns/loopback denial that the same AppArmor restriction surfaces) is
// still a hard FAIL, because the agent will hit it too — no false-PASS, and no
// signature guessing.
func TestCheckSandboxFailsAuthoritativelyOnNonUidmapError(t *testing.T) {
	requireLinuxProbe(t)
	noContainer(t)
	var called bool
	r := &reportBuilder{opts: Options{Mode: "real", Deploy: "binary",
		Runner: sandboxProbeRunner([]byte("bwrap: loopback: Failed RTM_NEWADDR: Operation not permitted\n"), errors.New("exit status 1"), &called, nil)}}
	r.checkSandbox(context.Background(), codexSandboxCfg("workspace-write"))
	c := findCheck(t, Report{Checks: r.checks}, "Codex sandbox")
	if c.Status != Fail {
		t.Errorf("checkSandbox(real,binary,non-uidmap-error) status = %s; want FAIL (codex sandbox is authoritative)", c.Status)
	}
	if !strings.Contains(c.Detail, "RTM_NEWADDR") {
		t.Errorf("detail = %q; want codex's actual sandbox error surfaced", c.Detail)
	}
}

func TestCheckSandboxWarnsOnProbeTimeout(t *testing.T) {
	requireLinuxProbe(t)
	noContainer(t)
	// A slow codex cold-start (context deadline) is not a sandbox denial — it
	// must WARN, not FAIL with the userns remediation.
	var called bool
	r := &reportBuilder{opts: Options{Mode: "real", Deploy: "binary",
		Runner: sandboxProbeRunner(nil, context.DeadlineExceeded, &called, nil)}}
	r.checkSandbox(context.Background(), codexSandboxCfg("workspace-write"))
	c := findCheck(t, Report{Checks: r.checks}, "Codex sandbox")
	if c.Status != Warn {
		t.Errorf("checkSandbox(real,binary,probe-timeout) status = %s; want WARN", c.Status)
	}
	if strings.Contains(c.Fix, "apparmor_restrict_unprivileged_userns") {
		t.Errorf("fix = %q; a probe timeout must not assert the userns cause", c.Fix)
	}
}

func TestCheckSandboxWarnsWhenCodexLacksSandboxSubcommand(t *testing.T) {
	requireLinuxProbe(t)
	noContainer(t)
	// An older codex that does not know `codex sandbox` is a version problem,
	// not a host userns problem — WARN to upgrade, do not FAIL.
	var called bool
	r := &reportBuilder{opts: Options{Mode: "real", Deploy: "binary",
		Runner: sandboxProbeRunner([]byte("error: unrecognized subcommand 'sandbox'\n"), errors.New("exit status 2"), &called, nil)}}
	r.checkSandbox(context.Background(), codexSandboxCfg("workspace-write"))
	c := findCheck(t, Report{Checks: r.checks}, "Codex sandbox")
	if c.Status != Warn {
		t.Errorf("checkSandbox(real,binary,no-sandbox-subcommand) status = %s; want WARN", c.Status)
	}
	if !strings.Contains(c.Fix, "Upgrade") {
		t.Errorf("fix = %q; want an upgrade hint", c.Fix)
	}
}

func TestCheckSandboxBinaryRealWarnsWhenCodexMissing(t *testing.T) {
	requireLinuxProbe(t)
	noContainer(t)
	var called bool
	r := &reportBuilder{opts: Options{Mode: "real", Deploy: "binary",
		Runner: sandboxProbeRunner(nil, &exec.Error{Name: "codex", Err: exec.ErrNotFound}, &called, nil)}}
	r.checkSandbox(context.Background(), codexSandboxCfg("workspace-write"))
	c := findCheck(t, Report{Checks: r.checks}, "Codex sandbox")
	if c.Status != Warn {
		t.Errorf("checkSandbox(real,binary,codex-missing) status = %s; want WARN", c.Status)
	}
	if !strings.Contains(c.Detail, "codex not found") {
		t.Errorf("detail = %q; want it to note codex could not be run", c.Detail)
	}
}

func TestCheckSandboxWarnsForCustomCodexCommand(t *testing.T) {
	requireLinuxProbe(t)
	noContainer(t)
	var called bool
	cfg := codexSandboxCfg("workspace-write")
	cfg.Codex.Command = "/opt/mycodex app-server" // custom wrapper, not bare codex
	r := &reportBuilder{opts: Options{Mode: "real", Deploy: "binary",
		Runner: sandboxProbeRunner([]byte(""), nil, &called, nil)}}
	r.checkSandbox(context.Background(), cfg)
	if called {
		t.Fatal("checkSandbox ran `codex sandbox` for a custom codex.command; cannot assume the wrapper supports it")
	}
	if c := findCheck(t, Report{Checks: r.checks}, "Codex sandbox"); c.Status != Warn {
		t.Errorf("checkSandbox(custom codex.command) status = %s; want WARN", c.Status)
	}
}

func TestCheckSandboxSkipsProbeWhenTurnPolicyOverridesToFullAccess(t *testing.T) {
	noContainer(t)
	// thread_sandbox stays workspace-write, but an explicit turn_sandbox_policy
	// override to dangerFullAccess means the agent's turns run unsandboxed — the
	// probe must follow the effective policy and skip, not false-FAIL (#549).
	var called bool
	cfg := codexSandboxCfg("workspace-write")
	cfg.Codex.TurnSandboxPolicy = workflow.CodexSandboxPolicy{Type: workflow.CodexSandboxDangerFullAccess}
	r := &reportBuilder{opts: Options{Mode: "real", Deploy: "binary",
		Runner: sandboxProbeRunner([]byte(""), nil, &called, nil)}}
	r.checkSandbox(context.Background(), cfg)
	if called {
		t.Fatal("checkSandbox ran the probe though turn_sandbox_policy overrides to danger-full-access")
	}
	if c := findCheck(t, Report{Checks: r.checks}, "Codex sandbox"); c.Status != Pass {
		t.Errorf("checkSandbox(turn-policy=dangerFullAccess) status = %s; want PASS", c.Status)
	}
}

func TestCheckSandboxSkipsProbeForExternalSandbox(t *testing.T) {
	noContainer(t)
	// turn_sandbox_policy: externalSandbox means codex skips its own bwrap
	// (host-provided isolation), so the userns probe is moot — skip, don't FAIL.
	var called bool
	cfg := codexSandboxCfg("workspace-write")
	cfg.Codex.TurnSandboxPolicy = workflow.CodexSandboxPolicy{Type: workflow.CodexSandboxExternalSandbox}
	r := &reportBuilder{opts: Options{Mode: "real", Deploy: "binary",
		Runner: sandboxProbeRunner([]byte(""), nil, &called, nil)}}
	r.checkSandbox(context.Background(), cfg)
	if called {
		t.Fatal("checkSandbox ran the probe for externalSandbox (codex runs no userns sandbox)")
	}
	if c := findCheck(t, Report{Checks: r.checks}, "Codex sandbox"); c.Status != Pass {
		t.Errorf("checkSandbox(externalSandbox) status = %s; want PASS", c.Status)
	}
}

func TestCheckSandboxProbesEffectiveReadOnlyTurnPolicy(t *testing.T) {
	requireLinuxProbe(t)
	noContainer(t)
	// read-only is still a sandboxed (bwrap-userns) profile, so the probe runs;
	// the codex sandbox subcommand ignores a passed mode, so no mode arg.
	var args []string
	var called bool
	cfg := codexSandboxCfg("workspace-write")
	cfg.Codex.TurnSandboxPolicy = workflow.CodexSandboxPolicy{Type: workflow.CodexSandboxReadOnly}
	r := &reportBuilder{opts: Options{Mode: "real", Deploy: "binary",
		Runner: sandboxProbeRunner([]byte(""), nil, &called, &args)}}
	r.checkSandbox(context.Background(), cfg)
	if !called {
		t.Fatal("checkSandbox(read-only effective) did not run the probe")
	}
	assertCodexSandboxProbeArgs(t, args)
}

func TestCheckSandboxSkipsProbeInMockMode(t *testing.T) {
	noContainer(t)
	var called bool
	r := &reportBuilder{opts: Options{Mode: "mock", Deploy: "binary",
		Runner: sandboxProbeRunner([]byte(""), nil, &called, nil)}}
	r.checkSandbox(context.Background(), codexSandboxCfg("workspace-write"))
	if called {
		t.Fatal("checkSandbox(mock) ran the codex sandbox probe; mock mode must keep the static check")
	}
	if c := findCheck(t, Report{Checks: r.checks}, "Codex sandbox"); c.Status != Pass {
		t.Errorf("checkSandbox(mock) status = %s; want static PASS", c.Status)
	}
}

func TestCheckSandboxSkipsProbeForDockerDeploy(t *testing.T) {
	noContainer(t)
	var called bool
	r := &reportBuilder{opts: Options{Mode: "real", Deploy: "docker",
		Runner: sandboxProbeRunner([]byte(""), nil, &called, nil)}}
	r.checkSandbox(context.Background(), codexSandboxCfg("workspace-write"))
	if called {
		t.Fatal("checkSandbox(real,docker) ran the host probe; it is not authoritative for a container deploy")
	}
	if c := findCheck(t, Report{Checks: r.checks}, "Codex sandbox"); c.Status != Pass {
		t.Errorf("checkSandbox(real,docker) status = %s; want static PASS", c.Status)
	}
}

func TestCheckSandboxDangerFullAccessSkipsProbe(t *testing.T) {
	noContainer(t)
	var called bool
	r := &reportBuilder{opts: Options{Mode: "real", Deploy: "binary",
		Runner: sandboxProbeRunner([]byte(""), nil, &called, nil)}}
	r.checkSandbox(context.Background(), codexSandboxCfg("danger-full-access"))
	if called {
		t.Fatal("checkSandbox(danger-full-access) ran the probe; that profile has no userns sandbox")
	}
	if c := findCheck(t, Report{Checks: r.checks}, "Codex sandbox"); c.Status != Pass {
		t.Errorf("checkSandbox(danger-full-access) status = %s; want PASS", c.Status)
	}
}

func TestBuildReportDoesNotRequireSSHForHTTPSCloneURLs(t *testing.T) {
	installFakeGitOnly(t)
	path := writeWorkflow(t, "linear", "$AIOPS_TEST_LINEAR_KEY")
	t.Setenv("AIOPS_TEST_LINEAR_KEY", "lin-test")
	report := BuildReport(context.Background(), Options{
		WorkflowPath: path,
		Mode:         "mock",
		Runner:       passingRunner,
	})

	if got := findCheck(t, report, "ssh"); got.Status != Pass {
		t.Fatalf("ssh check = %+v; want PASS for HTTPS clone URLs without ssh on PATH", got)
	}
	for _, check := range report.Checks {
		if check.Status == Fail {
			t.Fatalf("HTTPS workflow without ssh should not fail; got %+v", check)
		}
	}
}

func TestBuildReportRequiresSSHForSSHCloneURLs(t *testing.T) {
	installFakeGitOnly(t)
	path := writeWorkflowWithCloneURL(t, "git@example.com:o/r.git")
	t.Setenv("AIOPS_TEST_LINEAR_KEY", "lin-test")
	report := BuildReport(context.Background(), Options{
		WorkflowPath: path,
		Mode:         "mock",
		Runner:       passingRunner,
	})

	if got := findCheck(t, report, "ssh"); got.Status != Fail {
		t.Fatalf("ssh check = %+v; want FAIL for SSH clone URL without ssh on PATH", got)
	}
}

func TestBuildReportBinaryDeploySkipsDockerComposeChecks(t *testing.T) {
	installFakeGitOnly(t)
	path := writeWorkflow(t, "linear", "$AIOPS_TEST_LINEAR_KEY")
	t.Setenv("AIOPS_TEST_LINEAR_KEY", "lin-test")

	dockerReport := BuildReport(context.Background(), Options{
		WorkflowPath: path,
		Mode:         "mock",
		Deploy:       "docker",
		Runner:       passingRunner,
	})
	if !hasCheck(dockerReport, "Docker Compose") {
		t.Fatalf("Deploy=docker: missing Docker Compose check in %+v", dockerReport.Checks)
	}

	binaryReport := BuildReport(context.Background(), Options{
		WorkflowPath: path,
		Mode:         "mock",
		Deploy:       "binary",
		Runner:       passingRunner,
	})
	for _, name := range []string{"Docker Compose", "Compose config"} {
		if hasCheck(binaryReport, name) {
			t.Errorf("Deploy=binary: %q check present; want it skipped in %+v", name, binaryReport.Checks)
		}
	}
}

func TestBuildReportInstallHintMatchesDeployTarget(t *testing.T) {
	installFakeGitOnly(t) // git on PATH, ssh absent
	path := writeWorkflowWithCloneURL(t, "git@example.com:o/r.git")
	t.Setenv("AIOPS_TEST_LINEAR_KEY", "lin-test")

	docker := findCheck(t, BuildReport(context.Background(), Options{
		WorkflowPath: path, Mode: "mock", Deploy: "docker", Runner: passingRunner,
	}), "ssh")
	if docker.Status != Fail {
		t.Fatalf("Deploy=docker ssh status = %s; want FAIL", docker.Status)
	}
	if !strings.Contains(docker.Fix, "in the worker image") {
		t.Fatalf("Deploy=docker ssh fix = %q; want the container-image hint", docker.Fix)
	}

	binary := findCheck(t, BuildReport(context.Background(), Options{
		WorkflowPath: path, Mode: "mock", Deploy: "binary", Runner: passingRunner,
	}), "ssh")
	if binary.Status != Fail {
		t.Fatalf("Deploy=binary ssh status = %s; want FAIL", binary.Status)
	}
	if strings.Contains(binary.Fix, "worker image") {
		t.Fatalf("Deploy=binary ssh fix = %q; should not mention a worker image", binary.Fix)
	}
	if !strings.Contains(binary.Fix, "on this host and ensure it is on PATH") {
		t.Fatalf("Deploy=binary ssh fix = %q; want the host PATH hint", binary.Fix)
	}
}

func TestCheckProjectToolchainSkippedForBinaryDeploy(t *testing.T) {
	// docker real mode runs the Go toolchain probes...
	dockerR := &reportBuilder{opts: Options{Mode: "real", Deploy: "docker", Runner: fakeRealRunner}}
	dockerR.normalize()
	dockerR.checkProjectToolchain(context.Background())
	if !hasCheck(Report{Checks: dockerR.checks}, "Go version") {
		t.Fatalf("Deploy=docker real: missing Go version probe in %+v", dockerR.checks)
	}

	// ...binary deploy skips them, so an absent Go toolchain is not a
	// spurious real-mode FAIL on a release-archive host.
	binaryR := &reportBuilder{opts: Options{Mode: "real", Deploy: "binary", Runner: fakeRealRunner}}
	binaryR.normalize()
	binaryR.checkProjectToolchain(context.Background())
	for _, name := range []string{"Go version", "gofmt", "Go test"} {
		if hasCheck(Report{Checks: binaryR.checks}, name) {
			t.Errorf("Deploy=binary real: %q probe present; want it skipped in %+v", name, binaryR.checks)
		}
	}
}

func TestBuildReportRealModeUsesCustomCodexCommandForAppServerProbe(t *testing.T) {
	wrapper := installFakeCodexWrapper(t)
	path := writeWorkflowWithCodexCommand(t, wrapper+" app-server")
	report := BuildReport(context.Background(), Options{
		WorkflowPath: path,
		Mode:         "real",
		Runner:       dockerOnlyRunner,
	})

	if got := findCheck(t, report, "Codex CLI").Status; got != Warn {
		t.Fatalf("Codex CLI status = %s; want WARN for custom codex.command", got)
	}
	if got := findCheck(t, report, "Codex app-server").Status; got != Pass {
		t.Fatalf("Codex app-server status = %s; want PASS", got)
	}
	for _, check := range report.Checks {
		if check.Status == Fail {
			t.Fatalf("custom codex.command report has failure %+v", check)
		}
	}
}

func TestBuildReportRealModeSkipsCodexForMockAgent(t *testing.T) {
	installFakeGitOnly(t)
	path := writeWorkflow(t, "gitea", "token")
	report := BuildReport(context.Background(), Options{
		WorkflowPath: path,
		Mode:         "real",
		Runner:       passingRunner,
	})

	if got := findCheck(t, report, "Codex").Status; got != Pass {
		t.Fatalf("Codex status = %s; want PASS skip for mock agent", got)
	}
	if checkExists(report, "Codex CLI") {
		t.Fatalf("Codex CLI check should be skipped for mock agent in real mode")
	}
}

func TestBuildReportRealModeChecksGoToolchain(t *testing.T) {
	installFakeGitOnly(t)
	moduleRoot := writeGoModule(t, "1.24.0")
	var goTestArgs []string
	var goTestDeadline time.Duration
	var gofmtCommand string
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeWorkflow(t, "gitea", "token"),
		Mode:         "real",
		GoTestDir:    moduleRoot,
		Runner: func(ctx context.Context, name string, args []string, env []string, stdin io.Reader) ([]byte, error) {
			if filepath.Base(name) == "gofmt" {
				gofmtCommand = name
			}
			if name == "go" && len(args) > 0 && args[0] == "test" {
				goTestArgs = append([]string(nil), args...)
				deadline, ok := ctx.Deadline()
				if !ok {
					t.Fatalf("go test context has no deadline")
				}
				goTestDeadline = time.Until(deadline)
			}
			return fakeRealRunner(ctx, name, args, env, stdin)
		},
	})

	if got := findCheck(t, report, "Go version"); got.Status != Pass || !strings.Contains(got.Detail, "go.mod requires 1.24.0") {
		t.Fatalf("Go version check = %+v; want PASS with go.mod compatibility detail", got)
	}
	if got := findCheck(t, report, "gofmt").Status; got != Pass {
		t.Fatalf("gofmt status = %s; want PASS", got)
	}
	if got, want := gofmtCommand, filepath.Join("/usr/local/go", "bin", "gofmt"); got != want {
		t.Fatalf("gofmt command = %q; want %q", got, want)
	}
	if got := findCheck(t, report, "Go test").Status; got != Pass {
		t.Fatalf("Go test status = %s; want PASS", got)
	}
	if got, want := strings.Join(goTestArgs, " "), "test -C "+moduleRoot+" ./internal/doctor -run TestDoctorGoToolchainProbe -count=1"; got != want {
		t.Fatalf("go test args = %q; want %q", got, want)
	}
	if goTestDeadline < time.Minute {
		t.Fatalf("go test deadline = %s; want at least 1m to avoid cold-cache false negatives", goTestDeadline)
	}
}

func TestBuildReportRealModeFailsWithoutGoModuleForTargetedTest(t *testing.T) {
	installFakeGitOnly(t)
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeWorkflow(t, "gitea", "token"),
		Mode:         "real",
		GoTestDir:    t.TempDir(),
		Runner:       fakeRealRunner,
	})

	check := findCheck(t, report, "Go test")
	if check.Status != Fail {
		t.Fatalf("Go test status = %s; want FAIL", check.Status)
	}
	if !strings.Contains(check.Fix, "--go-test-dir") {
		t.Fatalf("Go test fix = %q; want --go-test-dir guidance", check.Fix)
	}
}

func TestBuildReportRealModeWarnsWithoutExplicitGoTestDir(t *testing.T) {
	installFakeGitOnly(t)
	report := BuildReport(context.Background(), Options{
		WorkflowPath: writeWorkflow(t, "gitea", "token"),
		Mode:         "real",
		Runner:       fakeRealRunner,
	})

	check := findCheck(t, report, "Go test")
	if check.Status != Warn {
		t.Fatalf("Go test status = %s; want WARN", check.Status)
	}
	if !strings.Contains(check.Fix, "--go-test-dir") {
		t.Fatalf("Go test fix = %q; want --go-test-dir guidance", check.Fix)
	}
	if report.HasFailures() {
		t.Fatalf("report.HasFailures() = true; want false when only targeted Go test dir is omitted")
	}
}

func TestBuildReportGitHubAgentPreflightUsesAgentEnvironment(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "worker-token-must-not-leak")
	path := writeGitHubWorkflow(t, "codex-app-server")
	var checked []string
	runner := func(_ context.Context, name string, args []string, env []string, _ io.Reader) ([]byte, error) {
		switch name {
		case "docker":
			return []byte("ok\n"), nil
		case "go":
			if len(args) > 0 && args[0] == "version" {
				return []byte("go version go1.25.0 linux/amd64\n"), nil
			}
			if len(args) > 1 && args[0] == "env" && args[1] == "GOROOT" {
				return []byte("/usr/local/go\n"), nil
			}
			return nil, errors.New("unexpected go command")
		case "gofmt", filepath.Join("/usr/local/go", "bin", "gofmt"):
			return []byte(""), nil
		case "codex":
			if len(args) > 0 && args[0] == "--version" {
				return []byte("codex-cli 0.136.0\n"), nil
			}
			if len(args) > 1 && args[0] == "login" && args[1] == "status" {
				return []byte("Logged in\n"), nil
			}
			return nil, errors.New("unexpected codex command")
		case "gh", "git":
			joinedEnv := strings.Join(env, "\n")
			if strings.Contains(joinedEnv, "GITHUB_TOKEN=") || strings.Contains(joinedEnv, "GH_TOKEN=") {
				t.Fatalf("%s env leaked worker GitHub token:\n%s", name, joinedEnv)
			}
			if !strings.Contains(joinedEnv, "HOME=") || !strings.Contains(joinedEnv, "PATH=") {
				t.Fatalf("%s env = %q; want agent baseline HOME and PATH", name, joinedEnv)
			}
			checked = append(checked, name+" "+strings.Join(args, " "))
			return []byte("ok\n"), nil
		default:
			return nil, errors.New("unexpected command")
		}
	}

	report := BuildReport(context.Background(), Options{
		WorkflowPath: path,
		Mode:         "real",
		GitHubIssue:  451,
		Runner:       runner,
	})

	for _, name := range []string{"GitHub agent gh auth", "GitHub agent git push"} {
		if got := findCheck(t, report, name).Status; got != Pass {
			t.Fatalf("%s status = %s; want PASS", name, got)
		}
	}
	if len(checked) != 6 {
		t.Fatalf("checked commands = %#v; want gh plus five git probe commands", checked)
	}
	if checked[0] != "gh issue view 451 --repo xrf9268-hue/aiops-platform --json number,title,url" {
		t.Fatalf("first checked command = %q; want gh issue view", checked[0])
	}
	if !strings.Contains(checked[5], " push --dry-run https://github.com/xrf9268-hue/aiops-platform.git HEAD:refs/heads/aiops-doctor-preflight") {
		t.Fatalf("final checked command = %q; want git push dry-run", checked[5])
	}
}

func TestSafeCommandFailureOmitsCommandOutput(t *testing.T) {
	detail := safeCommandFailure("git push --dry-run", []byte("fatal: https://secret@github.com/o/r.git\n"), errors.New("exit status 128"))
	if strings.Contains(detail, "secret") || strings.Contains(detail, "github.com/o/r") {
		t.Fatalf("safeCommandFailure leaked command output: %q", detail)
	}
	if !strings.Contains(detail, "git push --dry-run failed") {
		t.Fatalf("safeCommandFailure = %q; want command failure detail", detail)
	}
}

func TestSelectGitHubRepoAcceptsMaskedCloneURLForSelection(t *testing.T) {
	const secret = "ghp_supersecrettoken"
	credURL := "https://x-access-token:" + secret + "@github.com/owner/repo.git"
	maskedURL := "https://github.com/owner/repo.git"
	cfg := workflow.Config{
		Repo: workflow.RepoConfig{Owner: "owner", Name: "repo", CloneURL: credURL},
	}

	// The masked URL is the credential-free form an operator can copy from a
	// report; passing it to --github-repo must select the credentialed repo
	// without retyping the embedded token.
	repo, err := selectGitHubRepo(cfg, maskedURL)
	if err != nil {
		t.Fatalf("selectGitHubRepo(cfg, %q) = %v; want the credentialed HTTPS repo", maskedURL, err)
	}
	if repo.CloneURL != credURL {
		t.Fatalf("selectGitHubRepo(cfg, %q).CloneURL = %q; want %q", maskedURL, repo.CloneURL, credURL)
	}
}

func TestSelectGitHubRepoNotFoundErrorMasksCloneURLCredentials(t *testing.T) {
	const secret = "ghp_supersecrettoken"
	cfg := workflow.Config{
		Repo: workflow.RepoConfig{
			Owner:    "owner",
			Name:     "repo",
			CloneURL: "https://github.com/owner/repo.git",
		},
	}
	// An operator may pass a clone_url to --github-repo (the new disambiguation
	// path); a non-matching one must not echo its embedded token into the report.
	target := "https://x-access-token:" + secret + "@github.com/other/repo.git"

	_, err := selectGitHubRepo(cfg, target)
	if err == nil {
		t.Fatalf("selectGitHubRepo(cfg, <clone_url>) error = nil; want not-found error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("selectGitHubRepo(cfg, <clone_url>) error = %q; must not leak clone_url credentials", err)
	}
}

func TestDefaultRunnerResolvesBinaryAgainstSuppliedEnvPATH(t *testing.T) {
	envDir := t.TempDir()
	envBin := filepath.Join(envDir, "agentonly")
	if err := os.WriteFile(envBin, []byte("#!/bin/sh\necho from-env\n"), 0o700); err != nil {
		t.Fatalf("write env-only binary: %v", err)
	}

	workerDir := t.TempDir()
	workerBin := filepath.Join(workerDir, "workeronly")
	if err := os.WriteFile(workerBin, []byte("#!/bin/sh\necho from-worker\n"), 0o700); err != nil {
		t.Fatalf("write worker-only binary: %v", err)
	}
	t.Setenv("PATH", workerDir)

	out, err := defaultRunner(context.Background(), "agentonly", nil, []string{"PATH=" + envDir}, nil)
	if err != nil {
		t.Fatalf("defaultRunner(agentonly) = %v; want success using env PATH", err)
	}
	if strings.TrimSpace(string(out)) != "from-env" {
		t.Fatalf("defaultRunner(agentonly) stdout = %q; want from-env", string(out))
	}

	_, err = defaultRunner(context.Background(), "workeronly", nil, []string{"PATH=" + envDir}, nil)
	if err == nil {
		t.Fatalf("defaultRunner(workeronly) err = nil; want not-found because env PATH excludes worker binary")
	}
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("defaultRunner(workeronly) err = %v; want exec.ErrNotFound (worker PATH must not leak)", err)
	}
}

func TestBuildReportFailsCodexAppServerErrorResponse(t *testing.T) {
	wrapper := installFakeCodexWrapperBody(t, `#!/bin/sh
if [ "$1" = "app-server" ]; then
  read line
  echo '{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"bad init"}}'
  exit 0
fi
exit 1
`)
	path := writeWorkflowWithCodexCommand(t, wrapper+" app-server")
	report := BuildReport(context.Background(), Options{
		WorkflowPath: path,
		Mode:         "real",
		Runner:       dockerOnlyRunner,
	})

	check := findCheck(t, report, "Codex app-server")
	if check.Status != Fail {
		t.Fatalf("Codex app-server status = %s; want FAIL", check.Status)
	}
	if !strings.Contains(check.Detail, "app-server initialize error") || !strings.Contains(check.Detail, "bad init") {
		t.Fatalf("Codex app-server detail = %q; want JSON-RPC initialize error message", check.Detail)
	}
}

func TestBuildReportRealModeKeepsCodexAppServerProbeStdinOpen(t *testing.T) {
	wrapper := installFakeCodexWrapperBody(t, `#!/bin/sh
if [ "$1" = "app-server" ]; then
  read line
  tmp="$(mktemp)"
  if timeout 0.2 cat >"$tmp"; then
    rm -f "$tmp"
    echo '{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"stdin closed during probe"}}'
    exit 0
  fi
  rm -f "$tmp"
  echo '{"jsonrpc":"2.0","id":1,"result":{"ok":true}}'
  exit 0
fi
exit 1
`)
	path := writeWorkflowWithCodexCommand(t, wrapper+" app-server")
	report := BuildReport(context.Background(), Options{
		WorkflowPath: path,
		Mode:         "real",
		Runner:       dockerOnlyRunner,
	})

	check := findCheck(t, report, "Codex app-server")
	if check.Status != Pass {
		t.Fatalf("Codex app-server check = %+v; want PASS with stdin held open", check)
	}
}

func installFakeCodex(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	body := `#!/bin/sh
case "$1" in
  --version) echo "codex-cli 0.136.0"; exit 0 ;;
  login) echo "Logged in"; exit 0 ;;
  app-server) read line; echo '{"jsonrpc":"2.0","id":1,"result":{"ok":true}}'; exit 0 ;;
esac
exit 1
`
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake codex: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func installFakeGitOnly(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "git")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	t.Setenv("PATH", dir)
}

func installFakeCodexWrapper(t *testing.T) string {
	t.Helper()
	body := `#!/bin/sh
if [ "$1" = "app-server" ]; then
  read line
  echo '{"jsonrpc":"2.0","id":1,"result":{"ok":true}}'
  exit 0
fi
exit 1
`
	return installFakeCodexWrapperBody(t, body)
}

func installFakeCodexWrapperBody(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "wrapped-codex")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake codex wrapper: %v", err)
	}
	return path
}

func TestRunReturnsFailureWhenReportHasFailures(t *testing.T) {
	path := writeWorkflow(t, "linear", "$AIOPS_TEST_MISSING_LINEAR_KEY")
	var out strings.Builder
	code := Run(context.Background(), Options{
		WorkflowPath: path,
		Mode:         "mock",
		Stdout:       &out,
		Runner:       passingRunner,
	})

	if code != 1 {
		t.Fatalf("Run exit code = %d; want 1", code)
	}
	if !strings.Contains(out.String(), "FAIL Workflow") || !strings.Contains(out.String(), "missing_tracker_api_key") {
		t.Fatalf("doctor output missing workflow credential failure:\n%s", out.String())
	}
}

func TestCheckCodexAuthModelAPIKeyMissingFailsInRealMode(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "")
	cfg := workflow.Config{}
	cfg.Codex.EnvPassthrough = []string{"OPENAI_API_KEY"}
	r := &reportBuilder{opts: Options{Mode: "real"}}
	r.checkCodexAuthModel(cfg)

	check := findCheck(t, Report{Checks: r.checks}, "Codex auth mode")
	if check.Status != Fail {
		t.Fatalf("checkCodexAuthModel(api-key, OPENAI_API_KEY unset) status = %s; want %s", check.Status, Fail)
	}
}

func TestCheckCodexAuthModelAPIKeyPresentPasses(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "sk-doctor-test")
	cfg := workflow.Config{}
	cfg.Codex.EnvPassthrough = []string{"OPENAI_API_KEY"}
	r := &reportBuilder{opts: Options{Mode: "real"}}
	r.checkCodexAuthModel(cfg)

	check := findCheck(t, Report{Checks: r.checks}, "Codex auth mode")
	if check.Status != Pass {
		t.Fatalf("checkCodexAuthModel(api-key, OPENAI_API_KEY set) status = %s; want %s", check.Status, Pass)
	}
	if strings.Contains(check.Detail, "sk-doctor-test") {
		t.Fatalf("Codex auth mode detail = %q; must not echo the API key", check.Detail)
	}
}

func TestCheckCodexAuthModelChatGPTLoginReportsModelConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	writeFile(t, filepath.Join(home, "config.toml"), "model = \"gpt-5-codex\"\nmodel_provider = \"openai\"\nmodel_reasoning_effort = \"high\"\n")
	cfg := workflow.Config{}
	r := &reportBuilder{opts: Options{Mode: "real"}}
	r.checkCodexAuthModel(cfg)

	mode := findCheck(t, Report{Checks: r.checks}, "Codex auth mode")
	if mode.Status != Pass || !strings.Contains(mode.Detail, "ChatGPT/Codex login") {
		t.Fatalf("Codex auth mode = %+v; want PASS ChatGPT/Codex login", mode)
	}
	model := findCheck(t, Report{Checks: r.checks}, "Codex model config")
	if model.Status != Pass {
		t.Fatalf("Codex model config status = %s; want %s", model.Status, Pass)
	}
	for _, want := range []string{"model=gpt-5-codex", "provider=openai", "reasoning_effort=high"} {
		if !strings.Contains(model.Detail, want) {
			t.Fatalf("Codex model config detail = %q; want it to contain %q", model.Detail, want)
		}
	}
}

func TestCheckCodexModelConfigWarnsWhenModelMissing(t *testing.T) {
	home := t.TempDir()
	r := &reportBuilder{opts: Options{Mode: "real"}}
	r.checkCodexModelConfig(home)

	check := findCheck(t, Report{Checks: r.checks}, "Codex model config")
	if check.Status != Warn {
		t.Fatalf("checkCodexModelConfig(no config.toml) status = %s; want %s", check.Status, Warn)
	}
}

func TestReadCodexModelConfigIgnoresTableSections(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "config.toml")
	writeFile(t, path, `# top-level model selection
model = "gpt-5-codex" # inline comment
[model_providers.custom]
model_provider = "leaked-provider"
model_reasoning_effort = "leaked-effort"
env_key = "SECRET_PROVIDER_KEY"
`)
	model, provider, effort, ok := readCodexModelConfig(path)
	if !ok || model != "gpt-5-codex" {
		t.Fatalf("readCodexModelConfig model = %q, ok = %v; want %q, true", model, ok, "gpt-5-codex")
	}
	if provider != "" || effort != "" {
		t.Fatalf("readCodexModelConfig leaked table keys: provider = %q, effort = %q; want both empty", provider, effort)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeWorkflow(t *testing.T, trackerKind, apiKey string) string {
	t.Helper()
	return writeWorkflowBody(t, trackerKind, apiKey, "mock", "")
}

func writeWorkflowWithEndpoint(t *testing.T, endpoint, agent string) string {
	t.Helper()
	body := "\n  endpoint: " + endpoint
	return writeWorkflowBody(t, "linear", "$AIOPS_TEST_LINEAR_KEY", agent, body)
}

func writeWorkflowWithCloneURL(t *testing.T, cloneURL string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: r
  clone_url: ` + cloneURL + `
tracker:
  kind: linear
  api_key: $AIOPS_TEST_LINEAR_KEY
  project_slug: platform
agent:
  default: mock
---
prompt
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	return path
}

func writeWorkflowWithCodexCommand(t *testing.T, command string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: r
  clone_url: https://example.invalid/o/r.git
tracker:
  kind: gitea
agent:
  default: codex-app-server
codex:
  command: ` + command + `
---
prompt
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	return path
}

func writeGitHubWorkflow(t *testing.T, agent string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: xrf9268-hue
  name: aiops-platform
  clone_url: https://github.com/xrf9268-hue/aiops-platform.git
tracker:
  kind: gitea
  api_key: token
  project_slug: platform
agent:
  default: ` + agent + `
---
prompt
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	return path
}

func writeWorkflowBody(t *testing.T, trackerKind, apiKey, agent, extraTracker string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := `---
repo:
  owner: o
  name: r
  clone_url: https://example.invalid/o/r.git
tracker:
  kind: ` + trackerKind + `
  api_key: ` + apiKey + `
  project_slug: platform` + extraTracker + `
agent:
  default: ` + agent + `
---
prompt
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	return path
}

func writeGoModule(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module "+aiopsModulePath+"\n\ngo "+version+"\n"), 0o600); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "internal", "doctor"), 0o700); err != nil {
		t.Fatalf("create internal/doctor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "doctor", "doctor_test.go"), []byte("package doctor\n"), 0o600); err != nil {
		t.Fatalf("write doctor test package: %v", err)
	}
	return dir
}

func TestCodexVersionFromOutput(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   goVersion
		wantOK bool
	}{
		{"codex-cli line", "codex-cli 0.136.0\n", goVersion{major: 0, minor: 136, patch: 0, patchSet: true}, true},
		{"bare version", "0.135.2", goVersion{major: 0, minor: 135, patch: 2, patchSet: true}, true},
		{"v-prefixed", "codex v0.137.0", goVersion{major: 0, minor: 137, patch: 0, patchSet: true}, true},
		{"no version token", "codex-cli unknown", goVersion{}, false},
	}
	for _, tc := range cases {
		got, ok := codexVersionFromOutput(tc.output)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("codexVersionFromOutput(%q) = (%+v, %v); want (%+v, %v)", tc.output, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestCodexVersionAtLeast(t *testing.T) {
	v := func(mj, mn, p int) goVersion { return goVersion{major: mj, minor: mn, patch: p, patchSet: true} }
	cases := []struct {
		name      string
		got, want goVersion
		atLeast   bool
	}{
		{"equal", v(0, 136, 0), v(0, 136, 0), true},
		{"older minor warns", v(0, 135, 9), v(0, 136, 0), false},
		{"newer minor ok", v(0, 137, 0), v(0, 136, 0), true},
		{"older patch warns", v(0, 136, 0), v(0, 136, 1), false},
		{"newer patch ok", v(0, 136, 2), v(0, 136, 1), true},
		{"older major warns", v(0, 99, 0), v(1, 0, 0), false},
	}
	for _, tc := range cases {
		if got := codexVersionAtLeast(tc.got, tc.want); got != tc.atLeast {
			t.Errorf("codexVersionAtLeast(%+v, %+v) = %v; want %v (%s)", tc.got, tc.want, got, tc.atLeast, tc.name)
		}
	}
}

func TestDoctorGoToolchainProbe(t *testing.T) {}

func passingRunner(context.Context, string, []string, []string, io.Reader) ([]byte, error) {
	return []byte("ok\n"), nil
}

func fakeRealRunner(_ context.Context, name string, args []string, _ []string, _ io.Reader) ([]byte, error) {
	if name != "docker" && name != "codex" && name != "go" && filepath.Base(name) != "gofmt" {
		return nil, errors.New("unexpected command")
	}
	if filepath.Base(name) == "gofmt" {
		return []byte(""), nil
	}
	if name == "go" && len(args) > 0 && args[0] == "version" {
		return []byte("go version go1.25.0 linux/amd64\n"), nil
	}
	if name == "go" && len(args) > 1 && args[0] == "env" && args[1] == "GOROOT" {
		return []byte("/usr/local/go\n"), nil
	}
	if name == "go" && len(args) > 0 && args[0] == "test" {
		if len(args) < 4 || args[1] != "-C" || args[3] != "./internal/doctor" {
			return nil, fmt.Errorf("unexpected go test args: %v", args)
		}
		if !fileExists(filepath.Join(args[2], "internal", "doctor", "doctor_test.go")) {
			return nil, errors.New("missing targeted doctor test package")
		}
		return []byte("ok\n"), nil
	}
	if name == "codex" && len(args) > 0 && args[0] == "login" {
		return []byte("Logged in\n"), nil
	}
	if name == "codex" && len(args) > 0 && args[0] == "--version" {
		return []byte("codex-cli 0.136.0\n"), nil
	}
	return []byte("ok\n"), nil
}

func dockerOnlyRunner(_ context.Context, name string, args []string, _ []string, _ io.Reader) ([]byte, error) {
	if name != "docker" && name != "go" && filepath.Base(name) != "gofmt" {
		return nil, errors.New("unexpected command")
	}
	if filepath.Base(name) == "gofmt" {
		return []byte(""), nil
	}
	if name == "go" && len(args) > 0 && args[0] == "version" {
		return []byte("go version go1.25.0 linux/amd64\n"), nil
	}
	if name == "go" && len(args) > 1 && args[0] == "env" && args[1] == "GOROOT" {
		return []byte("/usr/local/go\n"), nil
	}
	return []byte("ok\n"), nil
}

func hasCheck(report Report, name string) bool {
	for _, check := range report.Checks {
		if check.Name == name {
			return true
		}
	}
	return false
}

func findCheck(t *testing.T, report Report, name string) Check {
	t.Helper()
	for _, check := range report.Checks {
		if check.Name == name {
			return check
		}
	}
	t.Fatalf("missing check %q in %+v", name, report.Checks)
	return Check{}
}

func checkExists(report Report, name string) bool {
	for _, check := range report.Checks {
		if check.Name == name {
			return true
		}
	}
	return false
}

func TestGoVersionCompatible(t *testing.T) {
	const goOutput = "go version %s linux/amd64\n"
	cases := []struct {
		name       string
		host       string // `go version` field, e.g. "go1.25.10"
		goMod      string // go.mod `go` directive, e.g. "1.25.11"
		compatible bool
	}{
		// go.mod pins an exact patch: the patch gates same-minor hosts.
		{"exact patch match", "go1.25.11", "1.25.11", true},
		{"host patch below floor", "go1.25.10", "1.25.11", false},
		{"host patch above floor", "go1.25.12", "1.25.11", true},
		{"host patch-zero below floor", "go1.25.0", "1.25.11", false},
		// Minor differences resolve before the patch is consulted.
		{"host newer minor ignores patch", "go1.26.0", "1.25.11", true},
		{"host older minor", "go1.24.99", "1.25.11", false},
		// go.mod pins only major.minor: the patch is not compared.
		{"floor major.minor, host same minor", "go1.25.0", "1.25", true},
		{"floor major.minor, host higher patch", "go1.25.10", "1.25", true},
		{"floor major.minor, host older minor", "go1.24.9", "1.25", false},
		// Pre-release suffixes parse to their numeric prefix.
		{"host rc suffix at floor", "go1.25.11rc1", "1.25.11", true},
		// Unparseable inputs are never compatible.
		{"empty host output", "", "1.25.11", false},
		{"unparseable go.mod", "go1.25.11", "weird", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output := ""
			if tc.host != "" {
				output = fmt.Sprintf(goOutput, tc.host)
			}
			if got := goVersionCompatible(output, tc.goMod); got != tc.compatible {
				t.Errorf("goVersionCompatible(%q, %q) = %v; want %v", output, tc.goMod, got, tc.compatible)
			}
		})
	}
}
