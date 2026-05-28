package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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
	path := writeGitHubWorkflow(t, "codex")
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
				return []byte("codex-cli 0.133.0\n"), nil
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

func TestSelectGitHubRepoRequiresExplicitTargetForMultiRepoWorkflow(t *testing.T) {
	cfg := workflow.Config{
		Repo: workflow.RepoConfig{
			Owner:    "owner",
			Name:     "primary",
			CloneURL: "https://github.com/owner/primary.git",
		},
		Services: []workflow.ServiceConfig{{
			Repo: workflow.RepoConfig{
				Owner:    "owner",
				Name:     "service",
				CloneURL: "https://github.com/owner/service.git",
			},
		}},
	}

	if _, err := selectGitHubRepo(cfg, ""); err == nil || !strings.Contains(err.Error(), "multiple GitHub repos") {
		t.Fatalf("selectGitHubRepo without target error = %v; want multiple repo error", err)
	}
	repo, err := selectGitHubRepo(cfg, "owner/service")
	if err != nil {
		t.Fatalf("selectGitHubRepo explicit target: %v", err)
	}
	if repo.fullName() != "owner/service" {
		t.Fatalf("selected repo = %q; want owner/service", repo.fullName())
	}
}

func TestSelectGitHubRepoDeduplicatesIdenticalRepoEntries(t *testing.T) {
	cfg := workflow.Config{
		Repo: workflow.RepoConfig{
			Owner:    "owner",
			Name:     "primary",
			CloneURL: "https://github.com/owner/primary.git",
		},
		Services: []workflow.ServiceConfig{
			{
				Repo: workflow.RepoConfig{
					Owner:    "Owner",
					Name:     "Primary",
					CloneURL: "https://github.com/Owner/Primary.git",
				},
			},
			{
				Repo: workflow.RepoConfig{
					Owner:    "owner",
					Name:     "primary",
					CloneURL: "https://github.com/owner/primary.git",
				},
			},
		},
	}

	repo, err := selectGitHubRepo(cfg, "")
	if err != nil {
		t.Fatalf("selectGitHubRepo(cfg, \"\") = %v; want single repo after dedup", err)
	}
	if repo.fullName() != "owner/primary" {
		t.Fatalf("selectGitHubRepo full name = %q; want owner/primary", repo.fullName())
	}
}

func TestSelectGitHubRepoDisambiguatesSameOwnerNameByCloneURL(t *testing.T) {
	const (
		httpsURL = "https://github.com/owner/repo.git"
		sshURL   = "git@github.com:owner/repo.git"
	)
	cfg := workflow.Config{
		Repo: workflow.RepoConfig{
			Owner:    "owner",
			Name:     "repo",
			CloneURL: httpsURL,
		},
		Services: []workflow.ServiceConfig{{
			Repo: workflow.RepoConfig{
				Owner:    "owner",
				Name:     "repo",
				CloneURL: sshURL,
			},
		}},
	}

	// Distinct clone URLs for one owner/name must survive dedup; collapsing
	// them would hide the URL TaskFromIssue swaps in for service-routed work.
	if repos := githubRepos(cfg); len(repos) != 2 {
		t.Fatalf("githubRepos(...) kept %d repos; want 2 (distinct clone URLs)", len(repos))
	}

	_, err := selectGitHubRepo(cfg, "owner/repo")
	if err == nil || !strings.Contains(err.Error(), "multiple clone URLs") {
		t.Fatalf("selectGitHubRepo(cfg, %q) error = %v; want ambiguous clone URL error", "owner/repo", err)
	}
	if !strings.Contains(err.Error(), httpsURL) || !strings.Contains(err.Error(), sshURL) {
		t.Fatalf("selectGitHubRepo(cfg, %q) error = %q; want both clone URLs listed", "owner/repo", err)
	}

	repo, err := selectGitHubRepo(cfg, sshURL)
	if err != nil {
		t.Fatalf("selectGitHubRepo(cfg, %q) = %v; want SSH service repo", sshURL, err)
	}
	if repo.CloneURL != sshURL {
		t.Fatalf("selectGitHubRepo(cfg, %q).CloneURL = %q; want %q", sshURL, repo.CloneURL, sshURL)
	}

	repo, err = selectGitHubRepo(cfg, httpsURL)
	if err != nil {
		t.Fatalf("selectGitHubRepo(cfg, %q) = %v; want HTTPS fallback repo", httpsURL, err)
	}
	if repo.CloneURL != httpsURL {
		t.Fatalf("selectGitHubRepo(cfg, %q).CloneURL = %q; want %q", httpsURL, repo.CloneURL, httpsURL)
	}
}

func TestSelectGitHubRepoAmbiguityErrorMasksCloneURLCredentials(t *testing.T) {
	const secret = "ghp_supersecrettoken"
	cfg := workflow.Config{
		Repo: workflow.RepoConfig{
			Owner:    "owner",
			Name:     "repo",
			CloneURL: "https://x-access-token:" + secret + "@github.com/owner/repo.git",
		},
		Services: []workflow.ServiceConfig{{
			Repo: workflow.RepoConfig{
				Owner:    "owner",
				Name:     "repo",
				CloneURL: "https://oauth2:" + secret + "@github.com/owner/repo.git",
			},
		}},
	}

	_, err := selectGitHubRepo(cfg, "owner/repo")
	if err == nil {
		t.Fatalf("selectGitHubRepo(cfg, %q) error = nil; want ambiguity error", "owner/repo")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("selectGitHubRepo(cfg, %q) error = %q; must not leak clone_url credentials", "owner/repo", err)
	}
	if !strings.Contains(err.Error(), "github.com/owner/repo.git") {
		t.Fatalf("selectGitHubRepo(cfg, %q) error = %q; want masked clone URLs listed", "owner/repo", err)
	}
}

func TestSelectGitHubRepoAcceptsMaskedCloneURLForSelection(t *testing.T) {
	const secret = "ghp_supersecrettoken"
	credURL := "https://x-access-token:" + secret + "@github.com/owner/repo.git"
	maskedURL := "https://github.com/owner/repo.git"
	sshURL := "git@github.com:owner/repo.git"
	cfg := workflow.Config{
		Repo: workflow.RepoConfig{Owner: "owner", Name: "repo", CloneURL: credURL},
		Services: []workflow.ServiceConfig{{
			Repo: workflow.RepoConfig{Owner: "owner", Name: "repo", CloneURL: sshURL},
		}},
	}

	// The masked URL is what the ambiguity error shows the operator; selecting
	// the credentialed HTTPS path with it must work without retyping the token.
	repo, err := selectGitHubRepo(cfg, maskedURL)
	if err != nil {
		t.Fatalf("selectGitHubRepo(cfg, %q) = %v; want the credentialed HTTPS repo", maskedURL, err)
	}
	if repo.CloneURL != credURL {
		t.Fatalf("selectGitHubRepo(cfg, %q).CloneURL = %q; want %q", maskedURL, repo.CloneURL, credURL)
	}

	// The SSH path is still selectable by its own (already credential-free) form.
	repo, err = selectGitHubRepo(cfg, sshURL)
	if err != nil {
		t.Fatalf("selectGitHubRepo(cfg, %q) = %v; want the SSH repo", sshURL, err)
	}
	if repo.CloneURL != sshURL {
		t.Fatalf("selectGitHubRepo(cfg, %q).CloneURL = %q; want %q", sshURL, repo.CloneURL, sshURL)
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
		return []byte("codex-cli 0.133.0\n"), nil
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
