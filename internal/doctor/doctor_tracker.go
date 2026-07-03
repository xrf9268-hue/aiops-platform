package doctor

// doctor_tracker.go holds the tracker-facing checks: the per-kind tracker
// credential preflight (Linear API-key/GraphQL auth and project visibility;
// Gitea/GitHub auth by driving the worker's own tracker clients through
// their real poll query), and the GitHub agent-credential preflight (gh auth
// + git push dry-run), plus the clone-URL/repo-selection helpers they share.
// The report framework (reportBuilder, run helpers, shared output formatting)
// lives in doctor.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

// checkTracker dispatches the tracker credential preflight on tracker.kind so
// a Gitea or GitHub operator with a missing or bad token fails at doctor time
// instead of at the first poll (#781). The switch matches on the raw kind,
// exactly like the worker's trackerClientForWorkflow dispatch
// (cmd/worker/main.go): the loader's allowed-kinds validation compares raw
// values too (internal/workflow/validate.go), so a whitespace-bearing kind
// never loads and cannot reach this switch. The default branch is a
// defensive fallback.
func (r *reportBuilder) checkTracker(ctx context.Context, cfg workflow.Config) {
	switch cfg.Tracker.Kind {
	case "linear":
		r.checkLinear(ctx, cfg)
	case "gitea":
		r.checkGiteaTracker(ctx, cfg)
	case "github":
		r.checkGitHubTracker(ctx, cfg)
	default:
		r.warn("Tracker", fmt.Sprintf("tracker.kind %q has no doctor preflight", cfg.Tracker.Kind), "Set tracker.kind to gitea, github, or linear in WORKFLOW.md.")
	}
}

func (r *reportBuilder) checkLinear(ctx context.Context, cfg workflow.Config) {
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

// checkGiteaTracker preflights the Gitea tracker credentials by driving the
// worker's own tracker client through its real poll query. Every hand-built
// mirror of the worker's tracker access drifted in review (PR #801: base-URL
// duplication, endpoint divergence, /user false-negatives, and an
// unconditional /pulls probe the production client gates on the configured
// states), so the construction below calls the same shared constructors
// trackerClientForWorkflow (cmd/worker/main.go) calls
// (gitea.BaseURLFromEnv + gitea.NewTrackerClient) and ListActiveIssues
// inherits whatever the poll loop does — fidelity by shared construction,
// not by mirroring.
func (r *reportBuilder) checkGiteaTracker(ctx context.Context, cfg workflow.Config) {
	if !r.trackerPreflightStages(cfg, "Gitea API key", "Set tracker.api_key: $GITEA_TOKEN in WORKFLOW.md and provide GITEA_TOKEN via the worker environment.", "Gitea auth") {
		return
	}
	base := gitea.BaseURLFromEnv(cfg.Tracker)
	client := gitea.NewTrackerClient(cfg.Tracker, base, cfg.Repo.Owner, cfg.Repo.Name)
	client.HTTP = r.opts.HTTPClient
	listCtx, cancel := context.WithTimeout(ctx, trackerListingTimeout)
	defer cancel()
	_, err := client.ListActiveIssues(listCtx)
	r.reportTrackerListing("Gitea auth", base, "tracker.endpoint or GITEA_BASE_URL", cfg, err)
}

// checkGitHubTracker preflights the GitHub tracker credentials by driving the
// worker's own GitHub client through the shared constructor
// (tracker.NewGitHubClientFromEnv — the same call trackerClientForWorkflow
// makes in cmd/worker/main.go; same drift rationale as checkGiteaTracker).
// ListActiveIssues inherits the client's per-request deadlines (#295), the
// Bearer header, the githubStatesMayIncludeOpenIssues pulls gating,
// pagination, and JSON decoding — so a closed-only active_states workflow
// with an Issues-only token passes doctor exactly as it polls. Check names
// carry the "tracker" qualifier to stay distinct from the agent-side
// "GitHub agent …" checks.
func (r *reportBuilder) checkGitHubTracker(ctx context.Context, cfg workflow.Config) {
	if !r.trackerPreflightStages(cfg, "GitHub tracker API key", "Set tracker.api_key: $GITHUB_TOKEN in WORKFLOW.md and provide GITHUB_TOKEN via the worker environment.", "GitHub tracker auth") {
		return
	}
	client := tracker.NewGitHubClientFromEnv(cfg.Tracker, cfg.Repo.Owner, cfg.Repo.Name)
	client.HTTP = r.opts.HTTPClient
	listCtx, cancel := context.WithTimeout(ctx, trackerListingTimeout)
	defer cancel()
	_, err := client.ListActiveIssues(listCtx)
	r.reportTrackerListing("GitHub tracker auth", client.BaseURL, "tracker.endpoint or GITHUB_API_BASE_URL", cfg, err)
}

// trackerPreflightStages runs the doctor-side stages shared by the Gitea and
// GitHub tracker checks before any live traffic, mirroring checkLinear's
// branch order: an empty resolved api_key FAILs in both modes, mock mode
// passes without network access, and a missing repo.owner/repo.name FAILs
// because the production listing call needs the repository coordinates. It
// returns false when the live listing must not run.
func (r *reportBuilder) trackerPreflightStages(cfg workflow.Config, keyCheckName, keyFix, authCheckName string) bool {
	if strings.TrimSpace(cfg.Tracker.APIKey) == "" {
		r.fail(keyCheckName, "tracker.api_key resolved empty", keyFix)
		return false
	}
	if !r.realMode() {
		r.pass(keyCheckName, "present; live auth skipped in mock mode")
		return false
	}
	if strings.TrimSpace(cfg.Repo.Owner) == "" || strings.TrimSpace(cfg.Repo.Name) == "" {
		r.fail(authCheckName, "repo.owner and repo.name are required for the repository visibility probe", "Set repo.owner and repo.name to the repository the worker polls.")
		return false
	}
	return true
}

// reportTrackerListing turns the production client's ListActiveIssues result
// into the auth check verdict. Failures are rebuilt around the masked base
// URL (maskedProbeError) because tracker.endpoint may carry basic-auth
// userinfo that *url.Error embeds; the PASS detail masks it for the same
// reason.
func (r *reportBuilder) reportTrackerListing(authCheckName, base, baseHint string, cfg workflow.Config, err error) {
	repoFullName := strings.TrimSpace(cfg.Repo.Owner) + "/" + strings.TrimSpace(cfg.Repo.Name)
	if err != nil {
		r.fail(authCheckName, maskedProbeError(base, err).Error(), "Verify tracker.api_key, the tracker base URL ("+baseHint+"), and that the token can run the worker's issue listing for "+repoFullName+".")
		return
	}
	r.pass(authCheckName, "the worker's own issue listing for "+repoFullName+" succeeded against "+workflow.MaskCloneURL(base))
}

// trackerProbeTimeout bounds each live Linear GraphQL probe request. The
// default HTTPClient carries its own 10s timeout, but Options.HTTPClient is
// injectable, so per the repo convention the probe enforces its own deadline
// instead of trusting the client.
const trackerProbeTimeout = 10 * time.Second

// trackerListingTimeout bounds the whole Gitea/GitHub preflight listing call.
// The production clients already wrap every individual request in
// context.WithTimeout (internal/gitea requestTimeout; github.go
// RequestTimeout, #295) — injection of Options.HTTPClient cannot strip that —
// but ListActiveIssues spans multiple requests (states × pagination), so the
// call site adds an overall budget per the convention that external I/O is
// deadline-bounded at the caller even when the client honors ctx.
const trackerListingTimeout = time.Minute

// maskedProbeError rebuilds a tracker client or probe error around the masked
// endpoint: *url.Error embeds the request URL (parse errors verbatim,
// transport errors with only the password redacted by net/http), so a
// tracker.endpoint carrying basic-auth userinfo would otherwise leak into the
// doctor report through the FAIL detail.
func maskedProbeError(endpoint string, err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	return fmt.Errorf("probe %s: %w", workflow.MaskCloneURL(endpoint), err)
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

func (r *reportBuilder) checkLinearGraphQL(ctx context.Context, cfg workflow.Config) error {
	query := `query Doctor($projectSlug: String!) { viewer { id name } projects(filter: { slugId: { eq: $projectSlug } }, first: 1) { nodes { id slugId name } } }`
	projectSlugs := linearProjectSlugs(cfg)
	if len(projectSlugs) == 0 {
		return fmt.Errorf("linear project_slug is required at tracker.project_slug")
	}
	endpoint := strings.TrimSpace(cfg.Tracker.Endpoint)
	if endpoint == "" {
		endpoint = tracker.DefaultLinearEndpoint
	}
	for _, projectSlug := range projectSlugs {
		if err := r.probeLinearProjectSlug(ctx, endpoint, cfg.Tracker.APIKey, query, projectSlug); err != nil {
			return err
		}
	}
	return nil
}

// probeLinearProjectSlug runs one auth+visibility probe with its own request
// deadline: Options.HTTPClient is injectable, so the request must not trust
// the client's timeout (trackerProbeTimeout).
func (r *reportBuilder) probeLinearProjectSlug(ctx context.Context, endpoint, apiKey, query, projectSlug string) error {
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
	probeCtx, cancel := context.WithTimeout(ctx, trackerProbeTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"query": query, "variables": map[string]any{"projectSlug": projectSlug}})
	req, err := http.NewRequestWithContext(probeCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		// Same leak class as the Gitea/GitHub probes: *url.Error embeds the
		// endpoint, which may carry userinfo credentials.
		return maskedProbeError(endpoint, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", apiKey)
	resp, err := r.opts.HTTPClient.Do(req)
	if err != nil {
		return maskedProbeError(endpoint, err)
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
	return nil
}

func decodeLinearProjectProbe(resp *http.Response, out any) error {
	// The doctor probes every configured project slug through one shared
	// HTTPClient, so an undrained non-2xx body costs a fresh TCP+TLS
	// handshake per slug exactly when Linear is unhealthy (#771, the #762
	// drain class).
	defer tracker.DrainAndClose(resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("linear returned %s", resp.Status)
	}
	// Preflight mirrors the consumer: the poller decodes this response
	// immediately, so name the non-JSON body (a login proxy answering 200
	// HTML) instead of surfacing a bare json.SyntaxError.
	if err := tracker.DecodeJSONResponse(resp, out); err != nil {
		return fmt.Errorf("linear returned %s with a body that is not JSON or exceeded maximum size: %w", resp.Status, err)
	}
	return nil
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
