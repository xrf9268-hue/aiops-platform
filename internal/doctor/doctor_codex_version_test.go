package doctor

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestCheckCodexVersionStatusMatchesPinnedProtocol(t *testing.T) {
	installFakeCodex(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	want, ok := parseGoVersion(runner.CodexProtocolVersion)
	if !ok || want.minor == 0 {
		t.Fatalf("parseGoVersion(%q) = %+v, %v; want a version with a decrementable minor", runner.CodexProtocolVersion, want, ok)
	}
	customCommand := installFakeCodexWrapper(t) + " app-server"
	tests := []struct {
		name             string
		mode             string
		versionOutput    string
		command          string
		wantStatus       Status
		wantVersionCheck bool
		wantFailure      bool
		wantVersionCall  bool
	}{
		{name: "exact", versionOutput: "codex-cli " + formatTestVersion(want), wantStatus: Pass, wantVersionCheck: true, wantVersionCall: true},
		{name: "older", versionOutput: "codex-cli " + formatTestVersion(goVersion{major: want.major, minor: want.minor - 1, patch: want.patch, patchSet: true}), wantStatus: Fail, wantVersionCheck: true, wantFailure: true, wantVersionCall: true},
		{name: "newer patch", versionOutput: "codex-cli " + formatTestVersion(goVersion{major: want.major, minor: want.minor, patch: want.patch + 1, patchSet: true}), wantStatus: Fail, wantVersionCheck: true, wantFailure: true, wantVersionCall: true},
		{name: "newer minor", versionOutput: "codex-cli " + formatTestVersion(goVersion{major: want.major, minor: want.minor + 1, patch: 0, patchSet: true}), wantStatus: Fail, wantVersionCheck: true, wantFailure: true, wantVersionCall: true},
		{name: "newer major", versionOutput: "codex-cli " + formatTestVersion(goVersion{major: want.major + 1, minor: 0, patch: 0, patchSet: true}), wantStatus: Fail, wantVersionCheck: true, wantFailure: true, wantVersionCall: true},
		{name: "unparsable", versionOutput: "codex-cli unknown", wantStatus: Fail, wantVersionCheck: true, wantFailure: true, wantVersionCall: true},
		{name: "missing patch", versionOutput: fmt.Sprintf("codex-cli %d.%d", want.major, want.minor), wantStatus: Fail, wantVersionCheck: true, wantFailure: true, wantVersionCall: true},
		{name: "mock mode mismatch", mode: "mock", versionOutput: "codex-cli " + formatTestVersion(goVersion{major: want.major, minor: want.minor, patch: want.patch + 1, patchSet: true}), wantStatus: Warn, wantVersionCheck: true, wantVersionCall: true},
		{name: "custom command", command: customCommand, wantStatus: Warn},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			versionCalled := false
			cfg := workflow.DefaultConfig()
			cfg.Agent.Default = runner.NameCodexAppServer
			cfg.Codex.Command = tt.command
			mode := tt.mode
			if mode == "" {
				mode = "real"
			}
			r := &reportBuilder{opts: Options{
				Mode:   mode,
				Runner: codexVersionRunner(tt.versionOutput, &versionCalled),
			}}
			r.normalize()
			r.checkCodex(context.Background(), cfg)
			report := Report{Checks: r.checks}

			if tt.wantVersionCheck {
				if got := findCheck(t, report, "Codex version").Status; got != tt.wantStatus {
					t.Fatalf("Codex version status = %s; want %s", got, tt.wantStatus)
				}
			} else {
				if checkExists(report, "Codex version") {
					t.Fatalf("custom command report includes Codex version check: %+v", report.Checks)
				}
				if got := findCheck(t, report, "Codex CLI").Status; got != tt.wantStatus {
					t.Fatalf("Codex CLI status = %s; want %s", got, tt.wantStatus)
				}
			}
			if got := report.HasFailures(); got != tt.wantFailure {
				t.Fatalf("Report.HasFailures() = %v; want %v", got, tt.wantFailure)
			}
			if got := findCheck(t, report, "Codex app-server").Status; got != Pass {
				t.Fatalf("Codex app-server status = %s; want %s", got, Pass)
			}
			if versionCalled != tt.wantVersionCall {
				t.Fatalf("codex --version called = %v; want %v", versionCalled, tt.wantVersionCall)
			}
		})
	}
}

func TestRunReturnsFailureForCodexVersionMismatch(t *testing.T) {
	installFakeCodex(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	want, ok := parseGoVersion(runner.CodexProtocolVersion)
	if !ok {
		t.Fatalf("parseGoVersion(%q) ok = %v; want true", runner.CodexProtocolVersion, ok)
	}
	newer := formatTestVersion(goVersion{major: want.major, minor: want.minor, patch: want.patch + 1, patchSet: true})
	path := writeWorkflowBody(t, "gitea", "token", runner.NameCodexAppServer, "")
	var out strings.Builder
	code := Run(context.Background(), Options{
		WorkflowPath: path,
		Mode:         "real",
		Deploy:       "binary",
		Stdout:       &out,
		Runner:       codexVersionRunner("codex-cli "+newer, nil),
	})

	if code != 1 {
		t.Fatalf("Run(version mismatch) exit code = %d; want 1\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "FAIL Codex version") {
		t.Fatalf("doctor output missing version FAIL:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "PASS Codex app-server") {
		t.Fatalf("doctor output missing successful app-server probe:\n%s", out.String())
	}
}

func codexVersionRunner(versionOutput string, versionCalled *bool) CommandRunner {
	return func(_ context.Context, name string, args []string, _ []string, _ io.Reader) ([]byte, error) {
		if name != "codex" || len(args) == 0 {
			return nil, fmt.Errorf("unexpected command %q %v", name, args)
		}
		switch args[0] {
		case "--version":
			if versionCalled != nil {
				*versionCalled = true
			}
			return []byte(versionOutput + "\n"), nil
		case "login":
			return []byte("Logged in\n"), nil
		case "sandbox":
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected codex args %v", args)
		}
	}
}

func formatTestVersion(version goVersion) string {
	return fmt.Sprintf("%d.%d.%d", version.major, version.minor, version.patch)
}
