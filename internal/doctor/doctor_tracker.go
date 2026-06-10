package doctor

// doctor_tracker.go holds the tracker-facing checks: Linear API-key/GraphQL
// auth and project visibility, and the GitHub agent-credential preflight
// (gh auth + git push dry-run), plus the clone-URL/repo-selection helpers they
// share. The report framework (reportBuilder, run helpers, shared output
// formatting) lives in doctor.go.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/xrf9268-hue/aiops-platform/internal/runner"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

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
