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

type Check struct {
	Status Status
	Name   string
	Detail string
	Fix    string
}

type Options struct {
	WorkflowPath string
	Mode         string
	DashboardURL string
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
	r.checkDockerCompose(ctx)
	if wf != nil {
		r.checkLinear(ctx, wf.Config)
		r.checkCodex(ctx, wf.Config)
		r.checkSandbox(wf.Config)
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
	if wf.Config.Repo.CloneURL == "" && len(wf.Config.Services) == 0 {
		r.fail("Runtime config", "repo.clone_url is required for worker dispatch", "Set repo.clone_url or configure services[].repo.clone_url.")
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
		r.warn("rg", "ripgrep not found on PATH", "Install rg in the worker image for faster agent code search.")
	} else {
		r.pass("rg", "found on PATH")
	}
}

func (r *reportBuilder) checkDockerCompose(ctx context.Context) {
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
		r.fail("Linear API key", "tracker.api_key resolved empty", "Set LINEAR_API_KEY or mount it from a Docker secret into the workflow env.")
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

func (r *reportBuilder) checkCodex(ctx context.Context, cfg workflow.Config) {
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
}

func (r *reportBuilder) checkSandbox(cfg workflow.Config) {
	if !requiresCodex(cfg) {
		return
	}
	if runningInContainer() && cfg.Codex.ThreadSandbox != "danger-full-access" {
		r.warn("Codex sandbox", "containerized Codex may not support workspace-write namespaces", "Use the documented Docker-isolated profile or enable the required kernel/userns support.")
		return
	}
	r.pass("Codex sandbox", "selected profile is compatible with this preflight")
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

func (r *reportBuilder) checkLinearGraphQL(ctx context.Context, cfg workflow.Config) error {
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

func (r *reportBuilder) probeCodexAppServer(ctx context.Context, cfg workflow.Config) error {
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
		r.fail(name, "not found on PATH", "Install "+name+" in the worker image.")
		return
	}
	r.pass(name, "found on PATH")
}

func (r *reportBuilder) realMode() bool {
	return r.opts.Mode == "real"
}

func (r *reportBuilder) run(ctx context.Context, name string, args []string) ([]byte, error) {
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return r.opts.Runner(runCtx, name, args, nil, nil)
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
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = stdin
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out, ctx.Err()
	}
	return out, err
}

func requiresCodex(cfg workflow.Config) bool {
	return cfg.Agent.Default == "codex" || cfg.Agent.Default == runner.NameCodexAppServer
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
	for _, service := range cfg.Services {
		add(service.Tracker.ProjectSlug)
	}
	return slugs
}

func usesDefaultCodexCLI(cfg workflow.Config) bool {
	command := strings.TrimSpace(cfg.Codex.Command)
	return command == "" || command == "codex exec" || command == "codex app-server" || strings.HasPrefix(command, "codex app-server ")
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
	urls := make([]string, 0, 1+len(cfg.Services))
	if strings.TrimSpace(cfg.Repo.CloneURL) != "" {
		urls = append(urls, cfg.Repo.CloneURL)
	}
	for _, service := range cfg.Services {
		if strings.TrimSpace(service.Repo.CloneURL) != "" {
			urls = append(urls, service.Repo.CloneURL)
		}
	}
	return urls
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
