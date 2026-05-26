package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func TestBuildReportRealModeAuthenticatesServiceLinearProject(t *testing.T) {
	installFakeCodex(t)
	var gotSlugs []string
	linear := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode Linear probe: %v", err)
		}
		gotSlugs = append(gotSlugs, body.Variables["projectSlug"])
		_, _ = w.Write([]byte(`{"data":{"viewer":{"id":"u","name":"Ada"},"projects":{"nodes":[{"id":"p","slugId":"api-platform","name":"API"}]}}}`))
	}))
	defer linear.Close()
	path := writeServiceLinearWorkflow(t, linear.URL)
	t.Setenv("AIOPS_TEST_LINEAR_KEY", "lin-test")
	report := BuildReport(context.Background(), Options{
		WorkflowPath: path,
		Mode:         "real",
		Runner:       fakeRealRunner,
	})

	if got := findCheck(t, report, "Linear auth").Status; got != Pass {
		t.Fatalf("Linear auth status = %s; want PASS", got)
	}
	if len(gotSlugs) != 1 || gotSlugs[0] != "api-platform" {
		t.Fatalf("Linear probe project slugs = %#v; want service project slug only", gotSlugs)
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

func TestBuildReportRealModeSkipsAppServerProbeForCodexExec(t *testing.T) {
	installFakeCodexWithoutAppServer(t)
	path := writeWorkflowWithAgent(t, "codex")
	report := BuildReport(context.Background(), Options{
		WorkflowPath: path,
		Mode:         "real",
		Runner:       fakeRealRunner,
	})

	if got := findCheck(t, report, "Codex auth").Status; got != Pass {
		t.Fatalf("Codex auth status = %s; want PASS", got)
	}
	if checkExists(report, "Codex app-server") {
		t.Fatalf("Codex app-server check should be skipped for agent.default codex")
	}
	for _, check := range report.Checks {
		if check.Status == Fail {
			t.Fatalf("codex exec workflow should not fail app-server preflight; got %+v", check)
		}
	}
}

func installFakeCodex(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	body := `#!/bin/sh
case "$1" in
  --version) echo "codex-cli 0.133.0"; exit 0 ;;
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

func installFakeCodexWithoutAppServer(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	body := `#!/bin/sh
case "$1" in
  --version) echo "codex-cli 0.133.0"; exit 0 ;;
  login) echo "Logged in"; exit 0 ;;
  app-server) echo "app-server unavailable" >&2; exit 1 ;;
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

func writeWorkflow(t *testing.T, trackerKind, apiKey string) string {
	t.Helper()
	return writeWorkflowBody(t, trackerKind, apiKey, "mock", "")
}

func writeWorkflowWithEndpoint(t *testing.T, endpoint, agent string) string {
	t.Helper()
	body := "\n  endpoint: " + endpoint
	return writeWorkflowBody(t, "linear", "$AIOPS_TEST_LINEAR_KEY", agent, body)
}

func writeServiceLinearWorkflow(t *testing.T, endpoint string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	body := `---
tracker:
  kind: linear
  api_key: $AIOPS_TEST_LINEAR_KEY
  endpoint: ` + endpoint + `
services:
  - name: api
    repo:
      owner: o
      name: r
      clone_url: https://example.invalid/o/r.git
    tracker:
      project_slug: api-platform
agent:
  default: codex-app-server
---
prompt
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write service workflow: %v", err)
	}
	return path
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

func writeWorkflowWithAgent(t *testing.T, agent string) string {
	t.Helper()
	return writeWorkflowBody(t, "gitea", "token", agent, "")
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

func passingRunner(context.Context, string, []string, []string, io.Reader) ([]byte, error) {
	return []byte("ok\n"), nil
}

func fakeRealRunner(_ context.Context, name string, args []string, _ []string, _ io.Reader) ([]byte, error) {
	if name != "docker" && name != "codex" {
		return nil, errors.New("unexpected command")
	}
	if name == "codex" && len(args) > 0 && args[0] == "login" {
		return []byte("Logged in\n"), nil
	}
	if name == "codex" && len(args) > 0 && args[0] == "--version" {
		return []byte("codex-cli 0.133.0\n"), nil
	}
	return []byte("ok\n"), nil
}

func dockerOnlyRunner(_ context.Context, name string, _ []string, _ []string, _ io.Reader) ([]byte, error) {
	if name != "docker" {
		return nil, errors.New("unexpected command")
	}
	return []byte("ok\n"), nil
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
