package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestGitHubMakerReviewerRunbookDocumentsReusableHarness(t *testing.T) {
	root := repoRoot(t)
	readme := readFileString(t, filepath.Join(root, "README.md"))
	if !strings.Contains(readme, "docs/runbooks/github-maker-reviewer-automerge-e2e.md") {
		t.Fatalf("README does not link the GitHub maker/reviewer E2E runbook")
	}

	body := readFileString(t, filepath.Join(root, "docs", "runbooks", "github-maker-reviewer-automerge-e2e.md"))
	for _, want := range []string{
		"scripts/github-maker-reviewer-e2e-bootstrap.sh",
		"scripts/github-maker-reviewer-release-preflight.sh",
		"scripts/github-maker-reviewer-capture.py",
		"scripts/github-maker-reviewer-final-verify.py",
		"scripts/github-maker-reviewer-report.py",
		"distinct file-backed `GH_CONFIG_DIR`",
		"It does **not** pass `GITHUB_TOKEN`",
		"required job named `build-test`",
		"required_approving_review_count",
		"worker --doctor --deploy=binary --mode=real",
		"If `gh release view`, checksum, attestation",
		"git push --dry-run",
		"--include-github-pages",
		"--browser-storage-state",
		"Do not downgrade the scenario into\nsingle-agent merge",
		"`gh pr merge <PR> --auto --squash --delete-branch --match-head-commit <sha>`",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("runbook missing %q", want)
		}
	}
	assertInOrder(t, body, []string{
		"## 1. Prepare the Run Root",
		"## 2. Configure GitHub Identities",
		"## 3. Seed the Disposable Repo",
		"## 4. Configure Labels and Branch Protection",
		"## 6. Preflight Binary and Doctor",
		"## 7. Start Workers",
		"## 8. Capture Key Evidence",
		"## 10. Final Verification and Report",
	})
}

func TestGitHubMakerReviewerWorkflowExamplesLoadAndPinBoundaries(t *testing.T) {
	root := repoRoot(t)
	t.Setenv("GITHUB_TOKEN", "github-token")
	t.Setenv("AIOPS_GITHUB_REPO_CLONE_URL", "https://github.com/octo-org/octo-todo.git")

	for _, tc := range []struct {
		path           string
		active         []string
		inactive       []string
		maxTurns       int
		maxContTurns   int
		mustContain    []string
		mustNotContain []string
	}{
		{
			path:     "examples/github-maker-WORKFLOW.md",
			active:   []string{"aiops:todo", "aiops:rework"},
			inactive: []string{"aiops:human-review", "aiops:done", "aiops:canceled"},
			maxTurns: 30,
			mustContain: []string{
				"Do NOT use `gh pr review`, `gh pr merge`, `gh issue close`, or",
				"Refs #<N>` or `Issue #<N>`",
				"`Rework response:`",
				"LAST action",
			},
			mustNotContain: []string{"gitea_issue_labels"},
		},
		{
			path:         "examples/github-reviewer-automerge-WORKFLOW.md",
			active:       []string{"aiops:human-review"},
			inactive:     []string{"aiops:todo", "aiops:rework", "aiops:done", "aiops:canceled"},
			maxTurns:     18,
			maxContTurns: 48,
			mustContain: []string{
				"gh pr review <PR> --approve",
				"gh pr merge <PR> --auto --squash --delete-branch --match-head-commit <sha>",
				"gh pr view <PR> --json state,mergedAt,headRefOid,mergeStateStatus",
				"Do not use `--admin`",
				"Do NOT close an issue before GitHub confirms the PR is merged",
				"Do NOT edit, commit, or push code",
			},
			mustNotContain: []string{"gitea_issue_labels", "--json merged,"},
		},
	} {
		full := filepath.Join(root, tc.path)
		loaded, err := workflow.Load(full)
		if err != nil {
			t.Fatalf("workflow.Load(%s) = %v; want nil", tc.path, err)
		}
		cfg := loaded.Config
		if cfg.Tracker.Kind != "github" {
			t.Fatalf("%s tracker.kind = %q; want github", tc.path, cfg.Tracker.Kind)
		}
		if strings.Join(cfg.Tracker.ActiveStates, ",") != strings.Join(tc.active, ",") {
			t.Fatalf("%s active states = %v; want %v", tc.path, cfg.Tracker.ActiveStates, tc.active)
		}
		if strings.Join(cfg.Tracker.InactiveStates, ",") != strings.Join(tc.inactive, ",") {
			t.Fatalf("%s inactive states = %v; want %v", tc.path, cfg.Tracker.InactiveStates, tc.inactive)
		}
		if cfg.Agent.Default != "codex-app-server" {
			t.Fatalf("%s agent.default = %q; want codex-app-server", tc.path, cfg.Agent.Default)
		}
		if cfg.Agent.MaxTurns != tc.maxTurns {
			t.Fatalf("%s max_turns = %d; want %d", tc.path, cfg.Agent.MaxTurns, tc.maxTurns)
		}
		if tc.maxContTurns > 0 && cfg.Agent.MaxContinuationTurns != tc.maxContTurns {
			t.Fatalf("%s max_continuation_turns = %d; want %d", tc.path, cfg.Agent.MaxContinuationTurns, tc.maxContTurns)
		}
		if cfg.Codex.ReadTimeoutMs != 30000 {
			t.Fatalf("%s codex.read_timeout_ms = %d; want 30000", tc.path, cfg.Codex.ReadTimeoutMs)
		}
		if containsString(cfg.Codex.EnvPassthrough, "GITHUB_TOKEN") {
			t.Fatalf("%s passes GITHUB_TOKEN to agent env: %v", tc.path, cfg.Codex.EnvPassthrough)
		}
		for _, want := range []string{"GH_CONFIG_DIR", "NPM_CONFIG_CACHE", "PLAYWRIGHT_BROWSERS_PATH", "AIOPS_EXPECTED_GITHUB_LOGIN"} {
			if !containsString(cfg.Codex.EnvPassthrough, want) {
				t.Fatalf("%s env_passthrough missing %q: %v", tc.path, want, cfg.Codex.EnvPassthrough)
			}
		}
		text := readFileString(t, full)
		for _, want := range tc.mustContain {
			if !strings.Contains(text, want) {
				t.Fatalf("%s missing %q", tc.path, want)
			}
		}
		for _, forbidden := range tc.mustNotContain {
			if strings.Contains(text, forbidden) {
				t.Fatalf("%s contains forbidden %q", tc.path, forbidden)
			}
		}
	}
}

func TestGitHubMakerReviewerBootstrapPreparesRunRoot(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	script := filepath.Join(root, "scripts", "github-maker-reviewer-e2e-bootstrap.sh")
	cmd := exec.Command(
		"bash",
		script,
		"--run-root", runRoot,
		"--repo", "octo-org/octo-todo",
		"--port-base", "4500",
	)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bootstrap failed: %v\n%s", err, out)
	}

	for _, rel := range []string{
		"env.example",
		"NEXT-STEPS.md",
		"workflows/github-maker-WORKFLOW.md",
		"workflows/github-reviewer-automerge-WORKFLOW.md",
		"tools/release-preflight.sh",
		"tools/capture.py",
		"tools/final-verify.py",
		"tools/report.py",
		"issues/01-happy-path-filters.md",
		"issues/02-rework-candidate-offline-delete.md",
		"issues/03-dependency-sequencing-bulk-actions.md",
		"issues/04-rework-control-forced-proof.md",
		"secrets/gh/setup",
		"secrets/gh/maker",
		"secrets/gh/reviewer",
		"workspaces/maker",
		"workspaces/reviewer",
		"forge-json",
		"screenshots",
		"final-verify/logs",
		"reports",
	} {
		if _, err := os.Stat(filepath.Join(runRoot, rel)); err != nil {
			t.Fatalf("bootstrap did not create %s: %v", rel, err)
		}
	}

	env := readFileString(t, filepath.Join(runRoot, "env.example"))
	for _, want := range []string{
		`export AIOPS_GHMR_REPO="octo-org/octo-todo"`,
		`export AIOPS_GITHUB_REPO_CLONE_URL="https://github.com/octo-org/octo-todo.git"`,
		`export AIOPS_GHMR_MAKER_GH_CONFIG_DIR=`,
		`export AIOPS_GHMR_REVIEWER_GH_CONFIG_DIR=`,
		`export AIOPS_GHMR_MAKER_PORT="4501"`,
		`export AIOPS_GHMR_REVIEWER_PORT="4502"`,
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("env.example missing %q\n%s", want, env)
		}
	}

	maker := readFileString(t, filepath.Join(runRoot, "workflows", "github-maker-WORKFLOW.md"))
	reviewer := readFileString(t, filepath.Join(runRoot, "workflows", "github-reviewer-automerge-WORKFLOW.md"))
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{
			name: "maker",
			body: maker,
			want: []string{
				"  owner: octo-org",
				"  name: octo-todo",
				"  root: " + filepath.Join(runRoot, "workspaces", "maker"),
				"    - aiops:todo",
			},
		},
		{
			name: "reviewer",
			body: reviewer,
			want: []string{
				"  owner: octo-org",
				"  name: octo-todo",
				"  root: " + filepath.Join(runRoot, "workspaces", "reviewer"),
				"    - aiops:human-review",
			},
		},
	} {
		for _, want := range tc.want {
			if !strings.Contains(tc.body, want) {
				t.Fatalf("%s workflow missing %q\n%s", tc.name, want, tc.body)
			}
		}
		for _, forbidden := range []string{"your-github-owner", "your-repo", "~/aiops-workspaces"} {
			if strings.Contains(tc.body, forbidden) {
				t.Fatalf("%s workflow still contains placeholder %q", tc.name, forbidden)
			}
		}
	}

	next := readFileString(t, filepath.Join(runRoot, "NEXT-STEPS.md"))
	for _, want := range []string{
		"three GitHub identities",
		"release-preflight.sh",
		"required check named `build-test`",
		"branch protection",
		"Activate #1 first",
		"AIOPS_EXPECTED_GITHUB_LOGIN",
		"capture.py",
		"final-verify.py",
		"report.py",
	} {
		if !strings.Contains(next, want) {
			t.Fatalf("NEXT-STEPS.md missing %q\n%s", want, next)
		}
	}
}

func TestGitHubMakerReviewerReportGeneratesGovernanceDocs(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	mkdirAll(t,
		filepath.Join(runRoot, "forge-json"),
		filepath.Join(runRoot, "screenshots"),
		filepath.Join(runRoot, "final-verify", "screenshots"),
	)
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-issues-all.json"), fakeGitHubDoneIssuesJSON())
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-prs-all.json"), fakeGitHubMergedPRsJSON())
	writeFileString(t, filepath.Join(runRoot, "screenshots", "github-issues-final.png"), "png")
	writeFileString(t, filepath.Join(runRoot, "final-verify", "screenshots", "final-app-desktop.png"), "png")

	script := filepath.Join(root, "scripts", "github-maker-reviewer-report.py")
	cmd := exec.Command("python3", script, "--run-root", runRoot, "--repo", "octo-org/octo-todo", "--date", "2026-06-26")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("report failed: %v\n%s", err, out)
	}

	report := readFileString(t, filepath.Join(runRoot, "reports", "report.md"))
	for _, want := range []string{
		"READY FOR OPERATOR PASS REVIEW",
		"Pass Criteria Checklist",
		"Maker and reviewer used distinct GitHub identities",
		"Issue / PR Table",
		"#1",
		"#3",
		"Auto-Merge Evidence",
		"Rework Evidence",
		"Screenshot Index",
		"final-verify/screenshots/final-app-desktop.png",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q\n%s", want, report)
		}
	}

	retro := readFileString(t, filepath.Join(runRoot, "reports", "merge-mechanism-retro.md"))
	for _, want := range []string{
		"Single-agent agent-side merge",
		"Maker + reviewer-automerge",
		"Worker/orchestrator merge",
		"not become PR writer, merger, or terminal tracker writer",
	} {
		if !strings.Contains(retro, want) {
			t.Fatalf("retro missing %q\n%s", want, retro)
		}
	}
}

func TestGitHubMakerReviewerHelperEntrypoints(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{
		"scripts/github-maker-reviewer-e2e-bootstrap.sh",
		"scripts/github-maker-reviewer-release-preflight.sh",
		"scripts/github-maker-reviewer-capture.py",
		"scripts/github-maker-reviewer-final-verify.py",
		"scripts/github-maker-reviewer-report.py",
	} {
		var cmd *exec.Cmd
		path := filepath.Join(root, rel)
		switch filepath.Ext(rel) {
		case ".sh":
			cmd = exec.Command("bash", path, "--help")
		default:
			cmd = exec.Command("python3", path, "--help")
		}
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s --help failed: %v\n%s", rel, err, out)
		}
		if !strings.Contains(strings.ToLower(string(out)), "usage:") {
			t.Fatalf("%s --help did not print usage:\n%s", rel, out)
		}
	}
}

func fakeGitHubDoneIssuesJSON() string {
	return `[
  {"number":1,"title":"happy","state":"closed","closedAt":"2026-06-26T07:19:58Z","labels":[{"name":"aiops:done"}]},
  {"number":2,"title":"rework","state":"closed","closedAt":"2026-06-26T08:11:26Z","labels":[{"name":"aiops:done"}]},
  {"number":3,"title":"dependency","state":"closed","closedAt":"2026-06-26T08:27:22Z","labels":[{"name":"aiops:done"}]}
]`
}

func fakeGitHubMergedPRsJSON() string {
	return `[
  {"number":5,"title":"feat: filters","state":"MERGED","author":{"login":"maker-bot"},"headRefOid":"1111111111111111111111111111111111111111","mergedAt":"2026-06-26T07:19:15Z","mergedBy":{"login":"reviewer-bot"},"reviews":[{"state":"APPROVED"}]},
  {"number":8,"title":"fix: stale delete","state":"MERGED","author":{"login":"maker-bot"},"headRefOid":"2222222222222222222222222222222222222222","mergedAt":"2026-06-26T08:10:45Z","mergedBy":{"login":"reviewer-bot"},"reviews":[{"state":"CHANGES_REQUESTED"},{"state":"APPROVED"}]},
  {"number":9,"title":"feat: bulk complete","state":"MERGED","author":{"login":"maker-bot"},"headRefOid":"3333333333333333333333333333333333333333","mergedAt":"2026-06-26T08:26:36Z","mergedBy":{"login":"reviewer-bot"},"reviews":[{"state":"APPROVED"}]}
]`
}

func containsString(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}
