package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/worker"
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
		"env -u GH_TOKEN -u GITHUB_TOKEN",
		"It does **not** pass `GITHUB_TOKEN`",
		"required job named `build-test`",
		"required_approving_review_count",
		"aiops:blocked",
		"true operator-owned blockers",
		"Rework response:",
		"(headRefOid, baseRefOid, baseRefName)",
		"reviewer-owned `COMMENTED` checkpoint",
		"same-tuple retry",
		"one live snapshot",
		"never posts a second trigger",
		"changed head/base requires full review",
		"worker --doctor --deploy=binary --mode=real",
		"If `gh release view`, checksum, attestation",
		"git push --dry-run",
		`install -m 600 "$RUN_ROOT/env.example" "$RUN_ROOT/env.local"`,
		"--include-github-pages",
		"--browser-storage-state",
		"--tag final",
		"Do not downgrade the scenario into\nsingle-agent merge",
		"`gh pr merge <PR> --auto --squash --delete-branch --match-head-commit <sha> --body \"Closes #<N>\"`",
	} {
		if !strings.Contains(normalizedWorkflowText(body), normalizedWorkflowText(want)) {
			t.Fatalf("runbook missing %q", want)
		}
	}
	if strings.Contains(body, "\nGH_CONFIG_DIR=\"$AIOPS_GHMR_SETUP_GH_CONFIG_DIR\" \\\n  gh ") {
		t.Fatalf("runbook contains setup gh commands without stripping GH_TOKEN/GITHUB_TOKEN")
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

func TestGitHubMakerReviewerGovernanceGuideDocumentsProductionTopology(t *testing.T) {
	root := repoRoot(t)
	readme := readFileString(t, filepath.Join(root, "README.md"))
	guidePath := "docs/runbooks/github-maker-reviewer-governance.md"
	if !strings.Contains(readme, guidePath) {
		t.Fatalf("README does not link the GitHub maker/reviewer governance guide")
	}

	body := readFileString(t, filepath.Join(root, "docs", "runbooks", "github-maker-reviewer-governance.md"))
	for _, want := range []string{
		"distinct GitHub identities",
		"distinct `GH_CONFIG_DIR`",
		"env -u GH_TOKEN -u GITHUB_TOKEN",
		"distinct maker and reviewer `workspace.root`",
		"distinct maker and reviewer `AIOPS_MIRROR_ROOT`",
		"`AIOPS_EXPECTED_GITHUB_LOGIN`",
		"`aiops:todo`",
		"`aiops:rework`",
		"`aiops:human-review`",
		"`aiops:blocked`",
		"remove that role's active labels",
		"`aiops:canceled`",
		"`Rework response:`",
		"(headRefOid, baseRefOid, baseRefName)",
		"reviewer-owned `COMMENTED` checkpoint",
		"same-tuple retry",
		"one live snapshot",
		"does not repeat local review",
		"A head or base change invalidates the checkpoint",
		"absence of a reliable Codex signal is never clean",
		"branch protection",
		"required status check",
		"required approving review",
		"GitHub native auto-merge",
		"--body \"Closes #<N>\"",
		"Evidence checklist",
		"Failure recovery",
		"Worker/orchestrator boundary",
		"do not create PRs, approve PRs, merge PRs, close issues",
		"examples/github-maker-WORKFLOW.md",
		"examples/github-reviewer-automerge-WORKFLOW.md",
		"docs/runbooks/github-maker-reviewer-automerge-e2e.md",
		"scripts/github-maker-reviewer-release-preflight.sh",
		"scripts/github-maker-reviewer-capture.py",
		"scripts/github-maker-reviewer-report.py",
	} {
		if !strings.Contains(normalizedWorkflowText(body), normalizedWorkflowText(want)) {
			t.Fatalf("governance guide missing %q", want)
		}
	}
}

func TestGitHubMakerReviewerWorkflowExamplesLoadAndPinBoundaries(t *testing.T) {
	root := repoRoot(t)
	t.Setenv("GITHUB_TOKEN", "github-token")
	t.Setenv("AIOPS_GITHUB_REPO_CLONE_URL", "https://github.com/octo-org/octo-todo.git")

	for _, tc := range []struct {
		role            string
		path            string
		active          []string
		inactive        []string
		maxTurns        int
		maxContTurns    int
		maxClaimTokens  int64
		maxClaimSeconds int64
		verifyCommands  []string
		promptCommands  []string
	}{
		{
			role:            "maker",
			path:            "examples/github-maker-WORKFLOW.md",
			active:          []string{"aiops:todo", "aiops:rework"},
			inactive:        []string{"aiops:human-review", "aiops:blocked", "aiops:canceled"},
			maxTurns:        30,
			maxClaimTokens:  20000000,
			maxClaimSeconds: 7200,
			verifyCommands:  []string{"npm ci", "npm test", "npm run build", "npm run test:e2e"},
		},
		{
			role:            "reviewer",
			path:            "examples/github-reviewer-automerge-WORKFLOW.md",
			active:          []string{"aiops:human-review"},
			inactive:        []string{"aiops:todo", "aiops:rework", "aiops:blocked", "aiops:canceled"},
			maxTurns:        18,
			maxContTurns:    48,
			maxClaimTokens:  12000000,
			maxClaimSeconds: 7200,
			promptCommands:  []string{"npm ci", "npm test", "npm run build", "npm run test:e2e"},
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
		if strings.Join(cfg.Tracker.TerminalStates, ",") != "closed" {
			t.Fatalf("%s terminal states = %v; want [closed]", tc.path, cfg.Tracker.TerminalStates)
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
		if cfg.Agent.MaxTokensPerClaim != tc.maxClaimTokens {
			t.Fatalf("%s max_tokens_per_claim = %d; want %d", tc.path, cfg.Agent.MaxTokensPerClaim, tc.maxClaimTokens)
		}
		if cfg.Agent.MaxRuntimeSecondsPerClaim != tc.maxClaimSeconds {
			t.Fatalf("%s max_runtime_seconds_per_claim = %d; want %d", tc.path, cfg.Agent.MaxRuntimeSecondsPerClaim, tc.maxClaimSeconds)
		}
		if strings.Join(cfg.Verify.Commands, "\n") != strings.Join(tc.verifyCommands, "\n") {
			t.Fatalf("%s verify.commands = %v; want %v", tc.path, cfg.Verify.Commands, tc.verifyCommands)
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
		assertGitHubRolePromptContract(t, tc.role, loaded.PromptTemplate)
		if tc.role == "reviewer" {
			assertReviewerEffectiveVerifyContract(t, loaded.PromptTemplate, cfg.Verify.Commands, tc.promptCommands)
		}
	}
}

func assertReviewerEffectiveVerifyContract(t *testing.T, prompt string, configured, expected []string) {
	t.Helper()
	effective := worker.AppendVerifyDirective(prompt, configured)
	const marker = "**Verification (you own this):**"
	if strings.Count(effective, marker) != 1 {
		t.Fatalf("reviewer effective prompt marker count = %d; want 1", strings.Count(effective, marker))
	}
	if strings.Contains(effective, "before you hand off — i.e. before opening a PR") {
		t.Fatalf("reviewer effective prompt contains unconditional worker verify directive")
	}
	for _, command := range expected {
		if !strings.Contains(effective, "`"+command+"`") {
			t.Fatalf("reviewer effective prompt missing scoped command %q", command)
		}
	}
}

func assertGitHubRolePromptContract(t *testing.T, role, prompt string) {
	t.Helper()
	text := normalizedWorkflowText(prompt)
	commonForbidden := []string{"gitea_issue_labels", "--watch", "sleep "}
	for _, forbidden := range commonForbidden {
		if strings.Contains(text, forbidden) {
			t.Fatalf("%s prompt contains forbidden %q", role, forbidden)
		}
	}
	var required []string
	switch role {
	case "maker":
		required = []string{
			"Implement only this issue", "configured verification", "Refs #<N>",
			"Rework response:", "new head", "reviewThreads", "aiops:human-review",
			"true external/operator-owned blocker", "aiops:blocked", "LAST action",
		}
	case "reviewer":
		required = []string{
			"Do not start a separate review skill", "Complete the checkpoint and handoff yourself",
			"headRefOid", "baseRefOid", "baseRefName", "reviewer-owned `COMMENTED`",
			"local-rubric=PASS", "same exact tuple", "skip checkout", "one live snapshot",
			"`--paginate --slurp`", "`pageInfo`", "`hasNextPage`", "tuple-only guard",
			"detached checkout of `<HEAD>`", "`commit_id=<HEAD>`", "stale approval dismissal",
			"post-approval tuple guard", "dismiss that approval", "do not enable auto-merge",
			"Before any trigger, verdict, checkpoint, or approval write",
			"disable auto-merge and confirm it is absent before approval",
			"With an exact-head approval already present",
			"at most one `@codex review`", "absence of a reliable Codex signal is not clean",
			"use the `aiops:rework` transition above as the LAST action",
			"head or base changes", "reviewThreads", "current-head blockers from any author",
			"REST review API", "event `APPROVE`",
			`gh pr merge <PR_NUMBER> --auto --squash --delete-branch --match-head-commit <HEAD> --body "Closes #<N>"`,
			"gh issue edit <N> --remove-label aiops:human-review --add-label aiops:rework",
			"Re-read labels", "Repair and recheck before exit", "fail non-zero if it cannot converge",
			"Do not use `--admin`", "mergedAt", "PR is merged but the issue remains open",
			"gh issue close <N>", "approval and auto-merge already exist", "make no duplicate write", "aiops:blocked",
		}
	default:
		t.Fatalf("unknown role %q", role)
	}
	for _, want := range required {
		if !strings.Contains(text, normalizedWorkflowText(want)) {
			t.Fatalf("%s prompt missing invariant %q", role, want)
		}
	}
	const closingMergeBody = `--body "Closes #<N>"`
	if role == "maker" {
		if got := strings.Count(text, closingMergeBody); got != 0 {
			t.Fatalf("maker merge closing body count = %d; want 0", got)
		}
	}
	if role == "reviewer" {
		if got := strings.Count(text, closingMergeBody); got != 1 {
			t.Fatalf("reviewer merge closing body count = %d; want 1", got)
		}
		const reworkCommand = "gh issue edit <N> --remove-label aiops:human-review --add-label aiops:rework"
		if strings.Count(text, normalizedWorkflowText(reworkCommand)) != 1 {
			t.Fatalf("reviewer rework command count = %d; want 1", strings.Count(text, normalizedWorkflowText(reworkCommand)))
		}
		assertWorkflowInvariantOrder(t, text,
			"gh issue edit <N> --remove-label aiops:human-review --add-label aiops:rework",
			"Re-read labels", "Repair and recheck before exit",
			"Pagination is one bounded snapshot", "Before any trigger, verdict, checkpoint, or approval write",
			"## Exact-tuple checkpoint", "event `COMMENT`", "at most one `@codex review`",
			"Findings join the FAIL evidence", "use the `aiops:rework` transition above",
			"disable auto-merge and confirm it is absent before approval", "event `APPROVE`",
			"post-approval tuple guard",
			"dismiss that approval", "With an exact-head approval already present",
			`--match-head-commit <HEAD> --body "Closes #<N>"`)
	}
}

func assertWorkflowInvariantOrder(t *testing.T, text string, ordered ...string) {
	t.Helper()
	last := -1
	for _, marker := range ordered {
		at := strings.Index(text, normalizedWorkflowText(marker))
		if at <= last {
			t.Fatalf("workflow invariant %q appears at %d after %d", marker, at, last)
		}
		last = at
	}
}

func TestGitHubMakerReviewerPromptBudgetsAndNetNegativeDocs(t *testing.T) {
	root := repoRoot(t)
	paths := []string{
		"examples/github-maker-WORKFLOW.md",
		"examples/github-reviewer-automerge-WORKFLOW.md",
	}
	wantMax := []int{2439, 6406}
	total := 0
	for i, path := range paths {
		body := readFileString(t, filepath.Join(root, path))
		size := len([]byte(githubWorkflowPromptBody(t, path, body)))
		if size > wantMax[i] {
			t.Fatalf("%s prompt bytes = %d; want <= %d", path, size, wantMax[i])
		}
		total += size
	}
	if total > 8845 {
		t.Fatalf("combined prompt bytes = %d; want <= 8845", total)
	}

	tracked := []string{
		paths[0], paths[1],
		"docs/runbooks/github-maker-reviewer-governance.md",
		"docs/runbooks/github-maker-reviewer-automerge-e2e.md",
	}
	lines := 0
	for _, path := range tracked {
		lines += strings.Count(readFileString(t, filepath.Join(root, path)), "\n")
	}
	if lines >= 988 {
		t.Fatalf("tracked workflow/runbook lines = %d; want < 988", lines)
	}
}

func TestGitHubMakerReviewerTopologyRetiresDoneLabel(t *testing.T) {
	root := repoRoot(t)
	legacyDone := "aiops:" + "done"
	for _, path := range []string{
		"examples/github-maker-WORKFLOW.md",
		"examples/github-reviewer-automerge-WORKFLOW.md",
		"scripts/github-maker-reviewer-e2e-bootstrap.sh",
		"scripts/github-maker-reviewer-report.py",
		"scripts/github_maker_reviewer_e2e_test.go",
		"docs/runbooks/github-maker-reviewer-governance.md",
		"docs/runbooks/github-maker-reviewer-automerge-e2e.md",
	} {
		if got := strings.Count(readFileString(t, filepath.Join(root, path)), legacyDone); got != 0 {
			t.Fatalf("%s retired terminal label count = %d; want 0", path, got)
		}
	}
}

func githubWorkflowPromptBody(t *testing.T, path, body string) string {
	t.Helper()
	if !strings.HasPrefix(body, "---\n") {
		return body
	}
	rest := body[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		t.Fatalf("%s front matter is not terminated", path)
	}
	return rest[end+len("\n---\n"):]
}

func TestGitHubMakerReviewerWorkflowPromptsKeepTransientReviewGatesOutOfBlocked(t *testing.T) {
	root := repoRoot(t)
	makerPaths := []string{
		"examples/github-maker-WORKFLOW.md",
	}
	reviewerPaths := []string{
		"examples/github-reviewer-automerge-WORKFLOW.md",
	}
	for _, path := range append(append([]string{}, makerPaths...), reviewerPaths...) {
		body := readFileString(t, filepath.Join(root, path))
		bodyText := normalizedWorkflowText(body)
		for _, forbidden := range []string{
			"Codex reports a usage-limit result",
			"Codex reports a usage-limit/input-required result",
			"Codex usage-limit stops the review",
			"Codex usage-limit/input-required stops the review",
			"GitHub Codex review not confirmed for head <sha>` and move\n   the issue to `aiops:blocked`",
			"Do not leave an open issue labeled `aiops:human-review` when the next reviewer would only repeat",
		} {
			if strings.Contains(bodyText, normalizedWorkflowText(forbidden)) {
				t.Fatalf("%s still routes transient Codex review state to blocked via %q", path, forbidden)
			}
		}
	}
	for _, path := range makerPaths {
		body := readFileString(t, filepath.Join(root, path))
		bodyText := normalizedWorkflowText(body)
		for _, want := range []string{
			"Review uncertainty, no-signal, usage limits, CI, or merge state are not maker blockers",
			"Leave the current maker-active label unchanged",
			"true external/operator-owned blocker",
		} {
			if !strings.Contains(bodyText, normalizedWorkflowText(want)) {
				t.Fatalf("%s missing maker transient-review guidance %q", path, want)
			}
		}
	}
	for _, path := range reviewerPaths {
		body := readFileString(t, filepath.Join(root, path))
		bodyText := normalizedWorkflowText(body)
		for _, want := range []string{
			"No-signal", "usage-limit", "pending leaves `aiops:human-review` unchanged",
			"current-head blockers from any author",
			"move to `aiops:rework`",
		} {
			if !strings.Contains(bodyText, normalizedWorkflowText(want)) {
				t.Fatalf("%s missing reviewer transient-review guidance %q", path, want)
			}
		}
	}
}

func normalizedWorkflowText(text string) string {
	return strings.Join(strings.Fields(text), " ")
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
		`export AIOPS_GHMR_MAKER_MIRROR_ROOT=`,
		`export AIOPS_GHMR_REVIEWER_MIRROR_ROOT=`,
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
		"env -u GH_TOKEN -u GITHUB_TOKEN",
		`install -m 600 "` + runRoot + `/env.example" "` + runRoot + `/env.local"`,
		"release-preflight.sh",
		"required check named `build-test`",
		"branch protection",
		"Activate #1 first",
		`PLAYWRIGHT_BROWSERS_PATH="$PLAYWRIGHT_BROWSERS_PATH" python -m playwright install chromium`,
		`--gh-config-dir "$AIOPS_GHMR_SETUP_GH_CONFIG_DIR"`,
		`--tag final --skip-screenshots`,
		"AIOPS_MIRROR_ROOT",
		"AIOPS_EXPECTED_GITHUB_LOGIN",
		"capture.py",
		"final-verify.py",
		"report.py",
	} {
		if !strings.Contains(next, want) {
			t.Fatalf("NEXT-STEPS.md missing %q\n%s", want, next)
		}
	}
	assertInOrder(t, next, []string{
		"3. Install Chromium into the generated Playwright browser cache",
		"4. Seed the disposable",
		"5. Enable branch protection",
		"6. Create labels",
		"7. Create issues",
		"8. Run `tools/release-preflight.sh",
		"9. Start maker",
		"10. Start reviewer",
	})
}

func TestGitHubMakerReviewerPreflightValidatesRoleAuth(t *testing.T) {
	for _, tc := range []struct {
		name             string
		makerLogin       string
		reviewerLogin    string
		makerExpected    string
		reviewerExpected string
		wantErr          bool
		wantOutput       string
	}{
		{
			name:             "placeholder maker expected login",
			makerLogin:       "maker-bot",
			reviewerLogin:    "reviewer-bot",
			makerExpected:    "REPLACE_ME_MAKER_LOGIN",
			reviewerExpected: "reviewer-bot",
			wantErr:          true,
			wantOutput:       "AIOPS_GHMR_MAKER_LOGIN must be set to the observed maker GitHub login before preflight",
		},
		{
			name:             "same maker and reviewer login",
			makerLogin:       "same-bot",
			reviewerLogin:    "same-bot",
			makerExpected:    "same-bot",
			reviewerExpected: "same-bot",
			wantErr:          true,
			wantOutput:       "maker and reviewer GitHub logins must differ; both resolved to same-bot",
		},
		{
			name:             "distinct maker and reviewer logins",
			makerLogin:       "maker-bot",
			reviewerLogin:    "reviewer-bot",
			makerExpected:    "maker-bot",
			reviewerExpected: "reviewer-bot",
			wantOutput:       "release preflight complete for v0.0.0-test",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runGitHubMakerReviewerPreflightWithFakeTools(
				t,
				tc.makerLogin,
				tc.reviewerLogin,
				tc.makerExpected,
				tc.reviewerExpected,
			)
			if tc.wantErr && err == nil {
				t.Fatalf("preflight succeeded; want failure\n%s", out)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("preflight failed: %v\n%s", err, out)
			}
			if !strings.Contains(string(out), tc.wantOutput) {
				t.Fatalf("preflight output missing %q\n%s", tc.wantOutput, out)
			}
		})
	}
}

func TestGitHubMakerReviewerPreflightRejectsReviewerWithoutRepoWrite(t *testing.T) {
	out, err := runGitHubMakerReviewerPreflightWithFakeToolsAndRepo(
		t,
		"maker-bot",
		"reviewer-bot",
		"maker-bot",
		"reviewer-bot",
		"octo-org/octo-todo",
		"false",
	)
	if err == nil {
		t.Fatalf("preflight succeeded; want reviewer repo permission failure\n%s", out)
	}
	for _, want := range []string{
		"reviewer_repo_write=false",
		"reviewer login reviewer-bot must have write, maintain, or admin permission on octo-org/octo-todo",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("preflight output missing %q\n%s", want, out)
		}
	}
}

func TestGitHubMakerReviewerPreflightUsesRoleScopedAuthForRepoChecks(t *testing.T) {
	out, err := runGitHubMakerReviewerPreflightWithFakeToolsAndRepo(
		t,
		"maker-bot",
		"reviewer-bot",
		"maker-bot",
		"reviewer-bot",
		"octo-org/octo-todo",
		"true",
	)
	if err != nil {
		t.Fatalf("preflight failed: %v\n%s", err, out)
	}
	for _, want := range []string{
		"reviewer_repo_write=true",
		"release preflight complete for v0.0.0-test",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("preflight output missing %q\n%s", want, out)
		}
	}
}

func TestGitHubMakerReviewerPreflightTimesOutStalledGitHubCommands(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	setupDir := filepath.Join(runRoot, "secrets", "gh", "setup")
	fakeBin := filepath.Join(t.TempDir(), "fakebin")
	mkdirAll(t, setupDir, fakeBin)
	writeFileString(t, filepath.Join(fakeBin, "gh"), "#!/usr/bin/env bash\nsleep 5\n")
	if err := os.Chmod(filepath.Join(fakeBin, "gh"), 0o755); err != nil {
		t.Fatalf("chmod fake gh: %v", err)
	}

	script := filepath.Join(root, "scripts", "github-maker-reviewer-release-preflight.sh")
	cmd := exec.Command("bash", script, "--run-root", runRoot, "--release-repo", "octo-org/aiops-platform", "--tag", "v0.0.0-test")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"AIOPS_GHMR_SETUP_GH_CONFIG_DIR="+setupDir,
		"AIOPS_GHMR_COMMAND_TIMEOUT_SECONDS=1",
		"GH_TOKEN=tracker-token",
		"GITHUB_TOKEN=tracker-token",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("preflight succeeded; want release gh timeout\n%s", out)
	}
	if !strings.Contains(string(out), "gh release view") || !strings.Contains(string(out), "timed out after 1s") {
		t.Fatalf("preflight output missing timeout command\n%s", out)
	}
}

func TestGitHubMakerReviewerReportGeneratesGovernanceDocs(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	mkdirAll(t,
		filepath.Join(runRoot, "forge-json"),
		filepath.Join(runRoot, "screenshots"),
		filepath.Join(runRoot, "final-verify", "logs"),
		filepath.Join(runRoot, "final-verify", "screenshots"),
	)
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-issues-all.json"), fakeGitHubClosedIssuesJSON())
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-prs-all.json"), fakeGitHubMergedPRsJSON())
	writeFakeGitHubDependencySequencingEvents(t, runRoot, "final")
	writeFakeGitHubGovernanceEvidence(t, runRoot, "final")
	writeFakeReviewedHeadEvidence(t, runRoot, "final")
	writeFileString(t, filepath.Join(runRoot, "screenshots", "github-issues-final.png"), "png")
	writeFileString(t, filepath.Join(runRoot, "final-verify", "screenshots", "final-app-desktop.png"), "png")

	script := filepath.Join(root, "scripts", "github-maker-reviewer-report.py")
	cmd := exec.Command(
		"python3",
		script,
		"--run-root", runRoot,
		"--repo", "octo-org/octo-todo",
		"--reviewer-login", "reviewer-bot",
		"--date", "2026-06-26",
	)
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
		"Branch protection evidence: present",
		"Merged PR build-test evidence: present",
		"Reviewer approval evidence: matched",
		"Fresh clone verification evidence: present",
		"Issue closure provenance evidence: matched",
	} {
		if got := strings.Contains(report, want); !got {
			t.Fatalf("strings.Contains(report, %q) = %v; want true\nreport:\n%s", want, got, report)
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

func TestGitHubMakerReviewerReportUsesCapturedPluralJson(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	mkdirAll(t, filepath.Join(runRoot, "forge-json"))
	writeFileString(t, filepath.Join(runRoot, "forge-json", "issues-issue3-closed.json"), fakeGitHubClosedIssuesJSON())
	writeFileString(t, filepath.Join(runRoot, "forge-json", "prs-issue3-closed.json"), fakeGitHubMergedPRsJSON())
	writeFakeGitHubDependencySequencingEvents(t, runRoot, "issue3-closed")
	writeFakeGitHubGovernanceEvidence(t, runRoot, "issue3-closed")
	writeFakeReviewedHeadEvidence(t, runRoot, "issue3-closed")

	script := filepath.Join(root, "scripts", "github-maker-reviewer-report.py")
	cmd := exec.Command(
		"python3",
		script,
		"--run-root", runRoot,
		"--repo", "octo-org/octo-todo",
		"--reviewer-login", "reviewer-bot",
		"--date", "2026-06-26",
	)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("report failed: %v\n%s", err, out)
	}
	report := readFileString(t, filepath.Join(runRoot, "reports", "report.md"))
	if !strings.Contains(report, "READY FOR OPERATOR PASS REVIEW") || !strings.Contains(report, "#3") {
		t.Fatalf("report did not use captured plural JSON\n%s", report)
	}
}

func TestGitHubMakerReviewerReportRequiresGovernanceEvidenceForReady(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	mkdirAll(t, filepath.Join(runRoot, "forge-json"), filepath.Join(runRoot, "final-verify", "logs"))
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-issues-all.json"), fakeGitHubClosedIssuesJSON())
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-prs-all.json"), fakeGitHubMergedPRsWithoutStatusJSON())
	writeFakeGitHubDependencySequencingEvents(t, runRoot, "final")
	writeFakeFinalVerifyLogs(t, runRoot)

	script := filepath.Join(root, "scripts", "github-maker-reviewer-report.py")
	cmd := exec.Command(
		"python3",
		script,
		"--run-root", runRoot,
		"--repo", "octo-org/octo-todo",
		"--reviewer-login", "reviewer-bot",
		"--date", "2026-06-26",
	)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("report failed: %v\n%s", err, out)
	}
	report := readFileString(t, filepath.Join(runRoot, "reports", "report.md"))
	for _, want := range []string{
		"INCOMPLETE - review the evidence before claiming PASS",
		"Branch protection evidence: missing",
		"Merged PR build-test evidence: missing",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q\n%s", want, report)
		}
	}
}

func TestGitHubMakerReviewerReportRejectsFinalBranchProtectionFailure(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	mkdirAll(t, filepath.Join(runRoot, "forge-json"), filepath.Join(runRoot, "final-verify", "logs"))
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-issues-all.json"), fakeGitHubClosedIssuesJSON())
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-prs-all.json"), fakeGitHubMergedPRsJSON())
	writeFakeGitHubDependencySequencingEvents(t, runRoot, "final")
	writeFakeReviewedHeadEvidence(t, runRoot, "final")
	writeFakeBranchProtectionEvidence(t, runRoot, "initial")
	writeFileString(t, filepath.Join(runRoot, "forge-json", "branch-protection-final.err"), "branch protection not found\n")
	writeFakeFinalVerifyLogs(t, runRoot)

	script := filepath.Join(root, "scripts", "github-maker-reviewer-report.py")
	cmd := exec.Command(
		"python3",
		script,
		"--run-root", runRoot,
		"--repo", "octo-org/octo-todo",
		"--reviewer-login", "reviewer-bot",
		"--date", "2026-06-26",
	)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("report failed: %v\n%s", err, out)
	}
	report := readFileString(t, filepath.Join(runRoot, "reports", "report.md"))
	if strings.Contains(report, "READY FOR OPERATOR PASS REVIEW") {
		t.Fatalf("report marked ready with stale initial branch protection\n%s", report)
	}
	if !strings.Contains(report, "Branch protection evidence: missing") {
		t.Fatalf("report missing final branch protection failure status\n%s", report)
	}
}

func TestGitHubMakerReviewerReportRejectsFailedFreshCloneExitStatus(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	mkdirAll(t, filepath.Join(runRoot, "forge-json"), filepath.Join(runRoot, "final-verify", "logs"))
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-issues-all.json"), fakeGitHubClosedIssuesJSON())
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-prs-all.json"), fakeGitHubMergedPRsJSON())
	writeFakeGitHubDependencySequencingEvents(t, runRoot, "final")
	writeFakeGitHubGovernanceEvidence(t, runRoot, "final")
	writeFakeReviewedHeadEvidence(t, runRoot, "final")
	writeFileString(t, filepath.Join(runRoot, "final-verify", "logs", "npm-e2e.log"), "$ npm run test:e2e\nvitest ended with 1 error\nAIOPS_FINAL_VERIFY_EXIT_STATUS: 1\n")

	script := filepath.Join(root, "scripts", "github-maker-reviewer-report.py")
	cmd := exec.Command(
		"python3",
		script,
		"--run-root", runRoot,
		"--repo", "octo-org/octo-todo",
		"--reviewer-login", "reviewer-bot",
		"--date", "2026-06-26",
	)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("report failed: %v\n%s", err, out)
	}
	report := readFileString(t, filepath.Join(runRoot, "reports", "report.md"))
	if strings.Contains(report, "READY FOR OPERATOR PASS REVIEW") {
		t.Fatalf("report marked ready with failed fresh-clone npm gate\n%s", report)
	}
	if !strings.Contains(report, "Fresh clone verification evidence: missing") {
		t.Fatalf("report missing failed fresh-clone status\n%s", report)
	}
}

func TestGitHubMakerReviewerReportRequiresExplicitReviewedHeadEvidence(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	mkdirAll(t, filepath.Join(runRoot, "forge-json"), filepath.Join(runRoot, "final-verify", "logs"))
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-issues-all.json"), fakeGitHubClosedIssuesJSON())
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-prs-all.json"), fakeGitHubMergedPRsJSON())
	writeFakeGitHubDependencySequencingEvents(t, runRoot, "final")
	writeFakeBranchProtectionEvidence(t, runRoot, "final")
	writeFakeFinalVerifyLogs(t, runRoot)

	script := filepath.Join(root, "scripts", "github-maker-reviewer-report.py")
	cmd := exec.Command(
		"python3",
		script,
		"--run-root", runRoot,
		"--repo", "octo-org/octo-todo",
		"--reviewer-login", "reviewer-bot",
		"--date", "2026-06-26",
	)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("report failed: %v\n%s", err, out)
	}
	report := readFileString(t, filepath.Join(runRoot, "reports", "report.md"))
	if strings.Contains(report, "READY FOR OPERATOR PASS REVIEW") {
		t.Fatalf("report marked ready without reviewed-head evidence\n%s", report)
	}
	if !strings.Contains(report, "Reviewer approval evidence: missing") {
		t.Fatalf("report missing reviewed-head evidence status\n%s", report)
	}
}

func TestGitHubMakerReviewerReportRequiresReviewerApprovalAndFreshCloneEvidence(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	mkdirAll(t, filepath.Join(runRoot, "forge-json"))
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-issues-all.json"), fakeGitHubClosedIssuesJSON())
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-prs-all.json"), fakeGitHubMergedPRsWithMakerApprovalJSON())
	writeFakeGitHubDependencySequencingEvents(t, runRoot, "final")
	writeFakeBranchProtectionEvidence(t, runRoot, "final")

	script := filepath.Join(root, "scripts", "github-maker-reviewer-report.py")
	cmd := exec.Command(
		"python3",
		script,
		"--run-root", runRoot,
		"--repo", "octo-org/octo-todo",
		"--reviewer-login", "reviewer-bot",
		"--date", "2026-06-26",
	)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("report failed: %v\n%s", err, out)
	}
	report := readFileString(t, filepath.Join(runRoot, "reports", "report.md"))
	for _, want := range []string{
		"INCOMPLETE - review the evidence before claiming PASS",
		"Reviewer approval evidence: missing",
		"Fresh clone verification evidence: missing",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q\n%s", want, report)
		}
	}
}

func TestGitHubMakerReviewerReportRejectsStaleReviewerApproval(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	mkdirAll(t, filepath.Join(runRoot, "forge-json"), filepath.Join(runRoot, "final-verify", "logs"))
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-issues-all.json"), fakeGitHubClosedIssuesJSON())
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-prs-all.json"), fakeGitHubMergedPRsWithStaleApprovalJSON())
	writeFakeGitHubDependencySequencingEvents(t, runRoot, "final")
	writeFakeGitHubGovernanceEvidence(t, runRoot, "final")

	script := filepath.Join(root, "scripts", "github-maker-reviewer-report.py")
	cmd := exec.Command(
		"python3",
		script,
		"--run-root", runRoot,
		"--repo", "octo-org/octo-todo",
		"--reviewer-login", "reviewer-bot",
		"--date", "2026-06-26",
	)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("report failed: %v\n%s", err, out)
	}
	report := readFileString(t, filepath.Join(runRoot, "reports", "report.md"))
	if strings.Contains(report, "READY FOR OPERATOR PASS REVIEW") {
		t.Fatalf("report marked ready with stale reviewer approval\n%s", report)
	}
	if !strings.Contains(report, "Reviewer approval evidence: missing") {
		t.Fatalf("report missing stale approval status\n%s", report)
	}
}

func TestGitHubMakerReviewerReportRequiresDependencySequencingEvidence(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	mkdirAll(t, filepath.Join(runRoot, "forge-json"))
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-issues-all.json"), fakeGitHubClosedIssuesJSON())
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-prs-all.json"), fakeGitHubMergedPRsJSON())

	script := filepath.Join(root, "scripts", "github-maker-reviewer-report.py")
	cmd := exec.Command(
		"python3",
		script,
		"--run-root", runRoot,
		"--repo", "octo-org/octo-todo",
		"--reviewer-login", "reviewer-bot",
		"--date", "2026-06-26",
	)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("report failed: %v\n%s", err, out)
	}
	report := readFileString(t, filepath.Join(runRoot, "reports", "report.md"))
	if strings.Contains(report, "READY FOR OPERATOR PASS REVIEW") {
		t.Fatalf("report marked ready without dependency sequencing events\n%s", report)
	}
	if !strings.Contains(report, "Dependency sequencing evidence: missing") {
		t.Fatalf("report missing sequencing evidence status\n%s", report)
	}
}

func TestGitHubMakerReviewerReportRejectsEarlyDependencyActivation(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	mkdirAll(t, filepath.Join(runRoot, "forge-json"))
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-issues-all.json"), fakeGitHubClosedIssuesJSON())
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-prs-all.json"), fakeGitHubMergedPRsJSON())
	writeFakeGitHubEarlyDependencySequencingEvents(t, runRoot, "final")

	script := filepath.Join(root, "scripts", "github-maker-reviewer-report.py")
	cmd := exec.Command(
		"python3",
		script,
		"--run-root", runRoot,
		"--repo", "octo-org/octo-todo",
		"--reviewer-login", "reviewer-bot",
		"--date", "2026-06-26",
	)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("report failed: %v\n%s", err, out)
	}
	report := readFileString(t, filepath.Join(runRoot, "reports", "report.md"))
	if strings.Contains(report, "READY FOR OPERATOR PASS REVIEW") {
		t.Fatalf("report marked ready after early dependency activation\n%s", report)
	}
	if !strings.Contains(report, "Dependency sequencing evidence: missing") {
		t.Fatalf("report missing sequencing evidence status\n%s", report)
	}
}

func TestGitHubMakerReviewerReportRequiresClosedDependencyScenario(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	mkdirAll(t, filepath.Join(runRoot, "forge-json"), filepath.Join(runRoot, "final-verify", "logs"))
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-issues-all.json"), fakeGitHubOpenDependencyIssuesJSON())
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-prs-all.json"), fakeGitHubMergedPRsJSON())
	writeFakeGitHubDependencySequencingEvents(t, runRoot, "final")
	writeFakeGitHubGovernanceEvidence(t, runRoot, "final")
	writeFakeReviewedHeadEvidence(t, runRoot, "final")

	script := filepath.Join(root, "scripts", "github-maker-reviewer-report.py")
	cmd := exec.Command(
		"python3",
		script,
		"--run-root", runRoot,
		"--repo", "octo-org/octo-todo",
		"--reviewer-login", "reviewer-bot",
		"--date", "2026-06-26",
	)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("report failed: %v\n%s", err, out)
	}
	report := readFileString(t, filepath.Join(runRoot, "reports", "report.md"))
	const readyVerdict = "READY FOR OPERATOR PASS REVIEW"
	if got := strings.Contains(report, readyVerdict); got {
		t.Fatalf("strings.Contains(report, %q) = %v; want false\nreport:\n%s", readyVerdict, got, report)
	}
	if !strings.Contains(report, "INCOMPLETE - review the evidence before claiming PASS") {
		t.Fatalf("report missing incomplete verdict\n%s", report)
	}
}

func TestGitHubMakerReviewerReportRejectsUnprovenIssueClosures(t *testing.T) {
	root := repoRoot(t)
	for _, tc := range []struct {
		name         string
		prsJSON      string
		issue1Events string
	}{
		{"operator manual close", fakeGitHubMergedPRsJSON(), `[{"event":"closed","created_at":"2026-06-26T07:20:00Z","actor":{"login":"setup-bot"},"commit_id":null}]`},
		{"reviewer close before merge", fakeGitHubMergedPRsJSON(), `[{"event":"closed","created_at":"2026-06-26T07:19:00Z","actor":{"login":"reviewer-bot"},"commit_id":null}]`},
		{"mismatched native commit", fakeGitHubMergedPRsJSON(), `[{"event":"closed","created_at":"2026-06-26T07:19:58Z","actor":{"login":"reviewer-bot"},"commit_id":"dddddddddddddddddddddddddddddddddddddddd"}]`},
		{"wrong refs linkage", strings.Replace(fakeGitHubMergedPRsJSON(), `Refs #1.`, `Refs #10.`, 1), ""},
		{"maker closing reference", strings.Replace(fakeGitHubMergedPRsJSON(), `"closingIssuesReferences":[]`, `"closingIssuesReferences":[{"number":1}]`, 1), ""},
		{"missing closing reference evidence", strings.Replace(fakeGitHubMergedPRsJSON(), `"closingIssuesReferences":[],`, "", 1), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runRoot := filepath.Join(t.TempDir(), "run")
			mkdirAll(t, filepath.Join(runRoot, "forge-json"), filepath.Join(runRoot, "final-verify", "logs"))
			writeFileString(t, filepath.Join(runRoot, "forge-json", "final-issues-all.json"), fakeGitHubClosedIssuesJSON())
			writeFileString(t, filepath.Join(runRoot, "forge-json", "final-prs-all.json"), tc.prsJSON)
			writeFakeGitHubDependencySequencingEvents(t, runRoot, "final")
			writeFakeGitHubGovernanceEvidence(t, runRoot, "final")
			writeFakeReviewedHeadEvidence(t, runRoot, "final")
			if tc.issue1Events != "" {
				writeFileString(t, filepath.Join(runRoot, "forge-json", "issue-1-events-final.json"), tc.issue1Events)
			}
			script := filepath.Join(root, "scripts", "github-maker-reviewer-report.py")
			cmd := exec.Command("python3", script, "--run-root", runRoot, "--repo", "octo-org/octo-todo", "--reviewer-login", "reviewer-bot", "--date", "2026-06-26")
			cmd.Dir = root
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("report failed: %v\n%s", err, out)
			}
			report := readFileString(t, filepath.Join(runRoot, "reports", "report.md"))
			for value, want := range map[string]bool{
				"READY FOR OPERATOR PASS REVIEW":             false,
				"Issue closure provenance evidence: missing": true,
			} {
				if got := strings.Contains(report, value); got != want {
					t.Fatalf("strings.Contains(report, %q) = %v; want %v\nreport:\n%s", value, got, want, report)
				}
			}
		})
	}
}

func TestGitHubMakerReviewerReportRequiresReviewerOwnedMerges(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	mkdirAll(t, filepath.Join(runRoot, "forge-json"))
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-issues-all.json"), fakeGitHubClosedIssuesJSON())
	writeFileString(t, filepath.Join(runRoot, "forge-json", "final-prs-all.json"), fakeGitHubMakerMergedPRsJSON())
	writeFakeGitHubDependencySequencingEvents(t, runRoot, "final")

	script := filepath.Join(root, "scripts", "github-maker-reviewer-report.py")
	cmd := exec.Command(
		"python3",
		script,
		"--run-root", runRoot,
		"--repo", "octo-org/octo-todo",
		"--reviewer-login", "reviewer-bot",
		"--date", "2026-06-26",
	)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("report failed: %v\n%s", err, out)
	}
	report := readFileString(t, filepath.Join(runRoot, "reports", "report.md"))
	if strings.Contains(report, "READY FOR OPERATOR PASS REVIEW") {
		t.Fatalf("report marked ready when maker merged own PRs\n%s", report)
	}
	if !strings.Contains(report, "INCOMPLETE - review the evidence before claiming PASS") {
		t.Fatalf("report missing incomplete verdict\n%s", report)
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

func TestGitHubMakerReviewerCaptureTimesOutStalledGitHubCalls(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	fakeBin := filepath.Join(t.TempDir(), "fakebin")
	mkdirAll(t, fakeBin)
	writeFileString(t, filepath.Join(fakeBin, "gh"), "#!/usr/bin/env bash\nsleep 5\n")
	if err := os.Chmod(filepath.Join(fakeBin, "gh"), 0o755); err != nil {
		t.Fatalf("chmod fake gh: %v", err)
	}

	script := filepath.Join(root, "scripts", "github-maker-reviewer-capture.py")
	cmd := exec.Command(
		"python3",
		script,
		"--run-root", runRoot,
		"--repo", "octo-org/octo-todo",
		"--tag", "timeout",
		"--skip-screenshots",
		"--command-timeout-seconds", "1",
	)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("capture succeeded; want gh timeout\n%s", out)
	}
	if !strings.Contains(string(out), "gh issue list") || !strings.Contains(string(out), "timed out after 1s; see") {
		t.Fatalf("capture output missing timeout command and log path\n%s", out)
	}
	log := readFileString(t, filepath.Join(runRoot, "logs", "capture-timeout-commands.log"))
	if !strings.Contains(log, "TIMEOUT after 1s") || !strings.Contains(log, "command: gh issue list") {
		t.Fatalf("capture log missing timeout marker and command\n%s", log)
	}
}

func TestGitHubMakerReviewerCaptureStripsAmbientGitHubTokens(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	setupDir := filepath.Join(runRoot, "secrets", "gh", "setup")
	fakeBin := filepath.Join(t.TempDir(), "fakebin")
	argsLog := filepath.Join(t.TempDir(), "gh-args.log")
	mkdirAll(t, setupDir, fakeBin)
	writeFileString(t, filepath.Join(fakeBin, "gh"), fakeGhForCaptureTokenCheck())
	if err := os.Chmod(filepath.Join(fakeBin, "gh"), 0o755); err != nil {
		t.Fatalf("chmod fake gh: %v", err)
	}

	script := filepath.Join(root, "scripts", "github-maker-reviewer-capture.py")
	cmd := exec.Command(
		"python3",
		script,
		"--run-root", runRoot,
		"--repo", "octo-org/octo-todo",
		"--tag", "token",
		"--skip-screenshots",
		"--gh-config-dir", setupDir,
	)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"AIOPS_FAKE_GH_ARGS_LOG="+argsLog,
		"GH_TOKEN=tracker-token",
		"GITHUB_TOKEN=tracker-token",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("capture failed: %v\n%s", err, out)
	}
	commands := strings.Split(readFileString(t, argsLog), "\n")
	for _, prefix := range []string{"pr list ", "pr view "} {
		var command string
		for _, line := range commands {
			if strings.HasPrefix(line, prefix) {
				command = line
				break
			}
		}
		if command == "" {
			t.Fatalf("captured command with prefix %q = %q; want non-empty", prefix, command)
		}
		for _, field := range []string{"mergeCommit", "closingIssuesReferences"} {
			if got := strings.Contains(command, field); !got {
				t.Fatalf("strings.Contains(%q, %q) = %v; want true", command, field, got)
			}
		}
	}
}

func TestGitHubMakerReviewerFinalVerifyTimesOutStalledCommands(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	fakeBin := filepath.Join(t.TempDir(), "fakebin")
	mkdirAll(t, fakeBin)
	writeFileString(t, filepath.Join(fakeBin, "gh"), fakeGhForFinalVerify())
	writeFileString(t, filepath.Join(fakeBin, "npm"), "#!/usr/bin/env bash\nsleep 5\n")
	for _, name := range []string{"gh", "npm"} {
		if err := os.Chmod(filepath.Join(fakeBin, name), 0o755); err != nil {
			t.Fatalf("chmod fake %s: %v", name, err)
		}
	}

	script := filepath.Join(root, "scripts", "github-maker-reviewer-final-verify.py")
	cmd := exec.Command(
		"python3",
		script,
		"--run-root", runRoot,
		"--repo", "octo-org/octo-todo",
		"--skip-screenshots",
		"--command-timeout-seconds", "1",
	)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GH_TOKEN=tracker-token",
		"GITHUB_TOKEN=tracker-token",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("final verify succeeded; want npm timeout\n%s", out)
	}
	if !strings.Contains(string(out), "npm ci timed out after 1s; see") {
		t.Fatalf("final verify output missing timeout and log path\n%s", out)
	}
	log := readFileString(t, filepath.Join(runRoot, "final-verify", "logs", "npm-ci.log"))
	if !strings.Contains(log, "TIMEOUT after 1s") {
		t.Fatalf("npm log missing timeout marker\n%s", log)
	}
}

func TestGitHubMakerReviewerFinalVerifyRecordsFailedNpmExitStatus(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	fakeBin := filepath.Join(t.TempDir(), "fakebin")
	mkdirAll(t, fakeBin)
	writeFileString(t, filepath.Join(fakeBin, "gh"), fakeGhForFinalVerify())
	writeFileString(t, filepath.Join(fakeBin, "npm"), `#!/usr/bin/env bash
set -euo pipefail

case "${1:-}" in
  ci|test)
    echo "ok"
    exit 0
    ;;
  run)
    case "${2:-}" in
      build)
        echo "ok"
        exit 0
        ;;
      test:e2e)
        echo "vitest ended with 1 error"
        exit 7
        ;;
    esac
    ;;
esac

echo "unexpected npm args: $*" >&2
exit 42
`)
	for _, name := range []string{"gh", "npm"} {
		if err := os.Chmod(filepath.Join(fakeBin, name), 0o755); err != nil {
			t.Fatalf("chmod fake %s: %v", name, err)
		}
	}

	script := filepath.Join(root, "scripts", "github-maker-reviewer-final-verify.py")
	cmd := exec.Command(
		"python3",
		script,
		"--run-root", runRoot,
		"--repo", "octo-org/octo-todo",
		"--skip-screenshots",
	)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("final verify succeeded; want npm e2e failure\n%s", out)
	}
	if !strings.Contains(string(out), "npm run test:e2e failed; see") {
		t.Fatalf("final verify output missing failed command and log path\n%s", out)
	}
	log := readFileString(t, filepath.Join(runRoot, "final-verify", "logs", "npm-e2e.log"))
	if !strings.Contains(log, "AIOPS_FINAL_VERIFY_EXIT_STATUS: 7") {
		t.Fatalf("npm log missing exit status marker\n%s", log)
	}
}

func fakeGitHubClosedIssuesJSON() string {
	return `[
  {"number":1,"title":"Happy path: persistent filter tabs","state":"closed","closedAt":"2026-06-26T07:19:58Z","labels":[{"name":"aiops:human-review"}]},
  {"number":2,"title":"Rework candidate: stale offline delete guard","state":"closed","closedAt":"2026-06-26T08:11:26Z","labels":[{"name":"aiops:human-review"}]},
  {"number":3,"title":"Dependency: bulk complete active todos","state":"closed","closedAt":"2026-06-26T08:27:22Z","labels":[{"name":"aiops:human-review"}]}
]`
}

func fakeGitHubOpenDependencyIssuesJSON() string {
	return `[
  {"number":1,"title":"Happy path: persistent filter tabs","state":"closed","closedAt":"2026-06-26T07:19:58Z","labels":[{"name":"aiops:human-review"}]},
  {"number":2,"title":"Rework candidate: stale offline delete guard","state":"closed","closedAt":"2026-06-26T08:11:26Z","labels":[{"name":"aiops:human-review"}]},
  {"number":3,"title":"Dependency: bulk complete active todos","state":"open","closedAt":null,"labels":[{"name":"aiops:human-review"}]}
]`
}

func writeFakeGitHubDependencySequencingEvents(t *testing.T, runRoot string, tag string) {
	t.Helper()
	writeFakeGitHubRequiredClosureEvents(t, runRoot, tag)
	writeFileString(t, filepath.Join(runRoot, "forge-json", "issue-3-events-"+tag+".json"), `[
  {"event":"labeled","created_at":"2026-06-26T08:20:00Z","label":{"name":"aiops:todo"}},
  {"event":"closed","created_at":"2026-06-26T08:27:22Z","actor":{"login":"reviewer-bot"},"commit_id":"cccccccccccccccccccccccccccccccccccccccc"}
]`)
}

func writeFakeGitHubEarlyDependencySequencingEvents(t *testing.T, runRoot string, tag string) {
	t.Helper()
	writeFakeGitHubRequiredClosureEvents(t, runRoot, tag)
	writeFileString(t, filepath.Join(runRoot, "forge-json", "issue-3-events-"+tag+".json"), `[
  {"event":"labeled","created_at":"2026-06-26T08:00:00Z","label":{"name":"aiops:todo"}},
  {"event":"unlabeled","created_at":"2026-06-26T08:05:00Z","label":{"name":"aiops:todo"}},
  {"event":"labeled","created_at":"2026-06-26T08:20:00Z","label":{"name":"aiops:todo"}},
  {"event":"closed","created_at":"2026-06-26T08:27:22Z","actor":{"login":"reviewer-bot"},"commit_id":"cccccccccccccccccccccccccccccccccccccccc"}
]`)
}

func writeFakeGitHubRequiredClosureEvents(t *testing.T, runRoot string, tag string) {
	t.Helper()
	writeFileString(t, filepath.Join(runRoot, "forge-json", "issue-1-events-"+tag+".json"), `[
  {"event":"closed","created_at":"2026-06-26T07:19:58Z","actor":{"login":"reviewer-bot"},"commit_id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
]`)
	writeFileString(t, filepath.Join(runRoot, "forge-json", "issue-2-events-"+tag+".json"), `[
  {"event":"closed","created_at":"2026-06-26T08:11:26Z","actor":{"login":"reviewer-bot"},"commit_id":null}
]`)
}

func writeFakeGitHubGovernanceEvidence(t *testing.T, runRoot string, tag string) {
	t.Helper()
	writeFakeBranchProtectionEvidence(t, runRoot, tag)
	writeFakeFinalVerifyLogs(t, runRoot)
}

func writeFakeReviewedHeadEvidence(t *testing.T, runRoot string, tag string) {
	t.Helper()
	for number, head := range map[string]string{
		"5": "1111111111111111111111111111111111111111",
		"8": "2222222222222222222222222222222222222222",
		"9": "3333333333333333333333333333333333333333",
	} {
		writeFileString(t, filepath.Join(runRoot, "forge-json", "pr-"+number+"-reviews-"+tag+".json"), `[
  {"state":"APPROVED","user":{"login":"reviewer-bot"},"commit_id":"`+head+`"}
]`)
	}
}

func writeFakeBranchProtectionEvidence(t *testing.T, runRoot string, tag string) {
	t.Helper()
	writeFileString(t, filepath.Join(runRoot, "forge-json", "branch-protection-"+tag+".json"), `{
  "required_status_checks":{"contexts":["build-test"]},
  "required_pull_request_reviews":{"required_approving_review_count":1}
}`)
}

func writeFakeFinalVerifyLogs(t *testing.T, runRoot string) {
	t.Helper()
	logs := filepath.Join(runRoot, "final-verify", "logs")
	mkdirAll(t, logs)
	for _, tc := range []struct {
		name    string
		command string
	}{
		{"npm-ci.log", "npm ci"},
		{"npm-test.log", "npm test"},
		{"npm-build.log", "npm run build"},
		{"npm-e2e.log", "npm run test:e2e"},
	} {
		writeFileString(t, filepath.Join(logs, tc.name), "$ "+tc.command+"\nok\nAIOPS_FINAL_VERIFY_EXIT_STATUS: 0\n")
	}
}

func fakeGitHubMergedPRsJSON() string {
	return `[
  {"number":5,"title":"feat: filters","body":"Implemented the requested filter.\n\nRefs #1.","closingIssuesReferences":[],"state":"MERGED","author":{"login":"maker-bot"},"headRefOid":"1111111111111111111111111111111111111111","mergedAt":"2026-06-26T07:19:15Z","mergedBy":{"login":"reviewer-bot"},"mergeCommit":{"oid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"reviews":[{"state":"APPROVED","author":{"login":"reviewer-bot"}}],"statusCheckRollup":[{"name":"build-test","conclusion":"SUCCESS","status":"COMPLETED"}]},
  {"number":8,"title":"fix: stale delete","body":"Refs #2","closingIssuesReferences":[],"state":"MERGED","author":{"login":"maker-bot"},"headRefOid":"2222222222222222222222222222222222222222","mergedAt":"2026-06-26T08:10:45Z","mergedBy":{"login":"reviewer-bot"},"mergeCommit":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"reviews":[{"state":"CHANGES_REQUESTED","author":{"login":"reviewer-bot"}},{"state":"APPROVED","author":{"login":"reviewer-bot"}}],"statusCheckRollup":[{"name":"build-test","conclusion":"SUCCESS","status":"COMPLETED"}]},
  {"number":9,"title":"feat: bulk complete","body":"Refs #3","closingIssuesReferences":[],"state":"MERGED","author":{"login":"maker-bot"},"headRefOid":"3333333333333333333333333333333333333333","mergedAt":"2026-06-26T08:26:36Z","mergedBy":{"login":"reviewer-bot"},"mergeCommit":{"oid":"cccccccccccccccccccccccccccccccccccccccc"},"reviews":[{"state":"APPROVED","author":{"login":"reviewer-bot"}}],"statusCheckRollup":[{"name":"build-test","conclusion":"SUCCESS","status":"COMPLETED"}]}
]`
}

func fakeGitHubMergedPRsWithoutStatusJSON() string {
	return `[
  {"number":5,"title":"feat: filters","body":"Refs #1","closingIssuesReferences":[],"state":"MERGED","author":{"login":"maker-bot"},"headRefOid":"1111111111111111111111111111111111111111","mergedAt":"2026-06-26T07:19:15Z","mergedBy":{"login":"reviewer-bot"},"mergeCommit":{"oid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"reviews":[{"state":"APPROVED","author":{"login":"reviewer-bot"}}]},
  {"number":8,"title":"fix: stale delete","body":"Refs #2","closingIssuesReferences":[],"state":"MERGED","author":{"login":"maker-bot"},"headRefOid":"2222222222222222222222222222222222222222","mergedAt":"2026-06-26T08:10:45Z","mergedBy":{"login":"reviewer-bot"},"mergeCommit":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"reviews":[{"state":"CHANGES_REQUESTED","author":{"login":"reviewer-bot"}},{"state":"APPROVED","author":{"login":"reviewer-bot"}}]},
  {"number":9,"title":"feat: bulk complete","body":"Refs #3","closingIssuesReferences":[],"state":"MERGED","author":{"login":"maker-bot"},"headRefOid":"3333333333333333333333333333333333333333","mergedAt":"2026-06-26T08:26:36Z","mergedBy":{"login":"reviewer-bot"},"mergeCommit":{"oid":"cccccccccccccccccccccccccccccccccccccccc"},"reviews":[{"state":"APPROVED","author":{"login":"reviewer-bot"}}]}
]`
}

func fakeGitHubMergedPRsWithMakerApprovalJSON() string {
	return `[
  {"number":5,"title":"feat: filters","body":"Refs #1","closingIssuesReferences":[],"state":"MERGED","author":{"login":"maker-bot"},"headRefOid":"1111111111111111111111111111111111111111","mergedAt":"2026-06-26T07:19:15Z","mergedBy":{"login":"reviewer-bot"},"mergeCommit":{"oid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"reviews":[{"state":"APPROVED","author":{"login":"maker-bot"}}],"statusCheckRollup":[{"name":"build-test","conclusion":"SUCCESS","status":"COMPLETED"}]},
  {"number":8,"title":"fix: stale delete","body":"Refs #2","closingIssuesReferences":[],"state":"MERGED","author":{"login":"maker-bot"},"headRefOid":"2222222222222222222222222222222222222222","mergedAt":"2026-06-26T08:10:45Z","mergedBy":{"login":"reviewer-bot"},"mergeCommit":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"reviews":[{"state":"CHANGES_REQUESTED","author":{"login":"reviewer-bot"}},{"state":"APPROVED","author":{"login":"maker-bot"}}],"statusCheckRollup":[{"name":"build-test","conclusion":"SUCCESS","status":"COMPLETED"}]},
  {"number":9,"title":"feat: bulk complete","body":"Refs #3","closingIssuesReferences":[],"state":"MERGED","author":{"login":"maker-bot"},"headRefOid":"3333333333333333333333333333333333333333","mergedAt":"2026-06-26T08:26:36Z","mergedBy":{"login":"reviewer-bot"},"mergeCommit":{"oid":"cccccccccccccccccccccccccccccccccccccccc"},"reviews":[{"state":"APPROVED","author":{"login":"maker-bot"}}],"statusCheckRollup":[{"name":"build-test","conclusion":"SUCCESS","status":"COMPLETED"}]}
]`
}

func fakeGitHubMergedPRsWithStaleApprovalJSON() string {
	return `[
  {"number":5,"title":"feat: filters","body":"Refs #1","closingIssuesReferences":[],"state":"MERGED","author":{"login":"maker-bot"},"headRefOid":"1111111111111111111111111111111111111111","mergedAt":"2026-06-26T07:19:15Z","mergedBy":{"login":"reviewer-bot"},"mergeCommit":{"oid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"reviews":[{"state":"APPROVED","author":{"login":"reviewer-bot"},"commitOid":"1111111111111111111111111111111111111111"}],"statusCheckRollup":[{"name":"build-test","conclusion":"SUCCESS","status":"COMPLETED"}]},
  {"number":8,"title":"fix: stale delete","body":"Refs #2","closingIssuesReferences":[],"state":"MERGED","author":{"login":"maker-bot"},"headRefOid":"2222222222222222222222222222222222222222","mergedAt":"2026-06-26T08:10:45Z","mergedBy":{"login":"reviewer-bot"},"mergeCommit":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"reviews":[{"state":"CHANGES_REQUESTED","author":{"login":"reviewer-bot"}},{"state":"APPROVED","author":{"login":"reviewer-bot"},"commitOid":"old2222222222222222222222222222222222222"}],"statusCheckRollup":[{"name":"build-test","conclusion":"SUCCESS","status":"COMPLETED"}]},
  {"number":9,"title":"feat: bulk complete","body":"Refs #3","closingIssuesReferences":[],"state":"MERGED","author":{"login":"maker-bot"},"headRefOid":"3333333333333333333333333333333333333333","mergedAt":"2026-06-26T08:26:36Z","mergedBy":{"login":"reviewer-bot"},"mergeCommit":{"oid":"cccccccccccccccccccccccccccccccccccccccc"},"reviews":[{"state":"APPROVED","author":{"login":"reviewer-bot"},"commitOid":"3333333333333333333333333333333333333333"}],"statusCheckRollup":[{"name":"build-test","conclusion":"SUCCESS","status":"COMPLETED"}]}
]`
}

func fakeGitHubMakerMergedPRsJSON() string {
	return `[
  {"number":5,"title":"feat: filters","body":"Refs #1","closingIssuesReferences":[],"state":"MERGED","author":{"login":"maker-bot"},"headRefOid":"1111111111111111111111111111111111111111","mergedAt":"2026-06-26T07:19:15Z","mergedBy":{"login":"maker-bot"},"mergeCommit":{"oid":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"reviews":[{"state":"APPROVED","author":{"login":"reviewer-bot"}}],"statusCheckRollup":[{"name":"build-test","conclusion":"SUCCESS","status":"COMPLETED"}]},
  {"number":8,"title":"fix: stale delete","body":"Refs #2","closingIssuesReferences":[],"state":"MERGED","author":{"login":"maker-bot"},"headRefOid":"2222222222222222222222222222222222222222","mergedAt":"2026-06-26T08:10:45Z","mergedBy":{"login":"maker-bot"},"mergeCommit":{"oid":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"reviews":[{"state":"CHANGES_REQUESTED","author":{"login":"reviewer-bot"}},{"state":"APPROVED","author":{"login":"reviewer-bot"}}],"statusCheckRollup":[{"name":"build-test","conclusion":"SUCCESS","status":"COMPLETED"}]},
  {"number":9,"title":"feat: bulk complete","body":"Refs #3","closingIssuesReferences":[],"state":"MERGED","author":{"login":"maker-bot"},"headRefOid":"3333333333333333333333333333333333333333","mergedAt":"2026-06-26T08:26:36Z","mergedBy":{"login":"maker-bot"},"mergeCommit":{"oid":"cccccccccccccccccccccccccccccccccccccccc"},"reviews":[{"state":"APPROVED","author":{"login":"reviewer-bot"}}],"statusCheckRollup":[{"name":"build-test","conclusion":"SUCCESS","status":"COMPLETED"}]}
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

func runGitHubMakerReviewerPreflightWithFakeTools(
	t *testing.T,
	makerLogin string,
	reviewerLogin string,
	makerExpected string,
	reviewerExpected string,
) ([]byte, error) {
	t.Helper()

	return runGitHubMakerReviewerPreflightWithFakeToolsAndRepo(
		t,
		makerLogin,
		reviewerLogin,
		makerExpected,
		reviewerExpected,
		"",
		"true",
	)
}

func runGitHubMakerReviewerPreflightWithFakeToolsAndRepo(
	t *testing.T,
	makerLogin string,
	reviewerLogin string,
	makerExpected string,
	reviewerExpected string,
	repo string,
	reviewerCanWrite string,
) ([]byte, error) {
	t.Helper()

	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	setupDir := filepath.Join(runRoot, "secrets", "gh", "setup")
	makerDir := filepath.Join(runRoot, "secrets", "gh", "maker")
	reviewerDir := filepath.Join(runRoot, "secrets", "gh", "reviewer")
	mkdirAll(t, setupDir, makerDir, reviewerDir)
	writeFileString(t, filepath.Join(setupDir, "login"), "setup-bot\n")
	writeFileString(t, filepath.Join(makerDir, "login"), makerLogin+"\n")
	writeFileString(t, filepath.Join(reviewerDir, "login"), reviewerLogin+"\n")

	fakeBin := filepath.Join(t.TempDir(), "fakebin")
	mkdirAll(t, fakeBin)
	writeFileString(t, filepath.Join(fakeBin, "gh"), fakeGhForGitHubMakerReviewerPreflight())
	writeFileString(t, filepath.Join(fakeBin, "git"), fakeGitForGitHubMakerReviewerPreflight())
	writeFileString(t, filepath.Join(fakeBin, "codex"), "#!/usr/bin/env bash\necho 'codex test'\n")
	for _, name := range []string{"gh", "git", "codex"} {
		if err := os.Chmod(filepath.Join(fakeBin, name), 0o755); err != nil {
			t.Fatalf("chmod fake %s: %v", name, err)
		}
	}

	script := filepath.Join(root, "scripts", "github-maker-reviewer-release-preflight.sh")
	cmd := exec.Command("bash", script, "--run-root", runRoot, "--release-repo", "octo-org/aiops-platform", "--tag", "v0.0.0-test")
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"AIOPS_GHMR_SETUP_GH_CONFIG_DIR="+setupDir,
		"AIOPS_GHMR_MAKER_GH_CONFIG_DIR="+makerDir,
		"AIOPS_GHMR_REVIEWER_GH_CONFIG_DIR="+reviewerDir,
		"AIOPS_GHMR_MAKER_LOGIN="+makerExpected,
		"AIOPS_GHMR_REVIEWER_LOGIN="+reviewerExpected,
		"AIOPS_GHMR_REPO="+repo,
		"AIOPS_FAKE_REVIEWER_CAN_WRITE="+reviewerCanWrite,
		"GH_TOKEN=tracker-token",
		"GITHUB_TOKEN=tracker-token",
	)
	return cmd.CombinedOutput()
}

func fakeGhForGitHubMakerReviewerPreflight() string {
	return `#!/usr/bin/env bash
set -euo pipefail

assert_no_role_token() {
  if [ -n "${GH_TOKEN:-}" ] || [ -n "${GITHUB_TOKEN:-}" ]; then
    echo "role command leaked ambient GitHub token env" >&2
    exit 43
  fi
}

case "${1:-}" in
  --version|version)
    echo "gh version 0.0.0-test"
    ;;
  api)
    if [ "${2:-}" = "user" ]; then
      assert_no_role_token
      cat "$GH_CONFIG_DIR/login"
      exit 0
    fi
    case "${2:-}" in
      repos/*)
        assert_no_role_token
        echo "${AIOPS_FAKE_REVIEWER_CAN_WRITE:-true}"
        exit 0
        ;;
    esac
    echo "unexpected gh api args: $*" >&2
    exit 42
    ;;
  attestation)
    assert_no_role_token
    echo "Verified OK"
    ;;
  release)
    assert_no_role_token
    case "${2:-}" in
      view)
        echo '{"tagName":"v0.0.0-test","publishedAt":"2026-06-26T00:00:00Z","url":"https://example.invalid/release","assets":[]}'
        ;;
      download)
        dir=""
        pattern=""
        while [ "$#" -gt 0 ]; do
          case "$1" in
            --dir)
              dir="${2:-}"
              shift 2
              ;;
            --pattern)
              pattern="${2:-}"
              shift 2
              ;;
            *)
              shift
              ;;
          esac
        done
        mkdir -p "$dir"
        case "$pattern" in
          *.tar.gz)
            base="${pattern%.tar.gz}"
            tmp="$(mktemp -d)"
            mkdir -p "$tmp/$base"
            cat > "$tmp/$base/worker" <<'WORKER'
#!/usr/bin/env bash
if [ "${1:-}" = "--version" ]; then
  echo "worker test"
  exit 0
fi
if [ "${1:-}" = "--doctor" ]; then
  echo "doctor ok"
  exit 0
fi
echo "worker stub"
WORKER
            cat > "$tmp/$base/tui" <<'TUI'
#!/usr/bin/env bash
echo "tui test"
TUI
            chmod +x "$tmp/$base/worker" "$tmp/$base/tui"
            tar -czf "$dir/$pattern" -C "$tmp" "$base"
            rm -rf "$tmp"
            ;;
          *_SHA256SUMS)
            asset="$(find "$dir" -maxdepth 1 -name 'aiops-platform_*.tar.gz' -print -quit)"
            if [ -z "$asset" ]; then
              echo "missing release asset before checksum download" >&2
              exit 42
            fi
            (cd "$dir" && shasum -a 256 "$(basename "$asset")") > "$dir/$pattern"
            ;;
          *_sbom.cdx.json)
            echo '{"bomFormat":"CycloneDX","specVersion":"1.5","serialNumber":"urn:uuid:test","components":[]}' > "$dir/$pattern"
            ;;
          *)
            echo "unexpected release asset pattern: $pattern" >&2
            exit 42
            ;;
        esac
        ;;
      *)
        echo "unexpected gh release args: $*" >&2
        exit 42
        ;;
    esac
    ;;
  repo)
    if [ "${2:-}" = "clone" ]; then
      assert_no_role_token
      dest="${4:-}"
      if [ -z "$dest" ]; then
        echo "missing clone destination" >&2
        exit 42
      fi
      mkdir -p "$dest"
      exit 0
    fi
    echo "unexpected gh repo args: $*" >&2
    exit 42
    ;;
  *)
    echo "unexpected gh args: $*" >&2
    exit 42
    ;;
esac
`
}

func fakeGitForGitHubMakerReviewerPreflight() string {
	return `#!/usr/bin/env bash
set -euo pipefail

if [ -n "${GH_TOKEN:-}" ] || [ -n "${GITHUB_TOKEN:-}" ]; then
  echo "role git leaked ambient GitHub token env" >&2
  exit 43
fi

echo "fake git $*"
`
}

func fakeGhForCaptureTokenCheck() string {
	return `#!/usr/bin/env bash
set -euo pipefail

if [ -n "${GH_TOKEN:-}" ] || [ -n "${GITHUB_TOKEN:-}" ]; then
  echo "capture gh leaked ambient GitHub token env" >&2
  exit 43
fi

printf '%s\n' "$*" >> "$AIOPS_FAKE_GH_ARGS_LOG"

case "${1:-}:${2:-}" in
  issue:list|run:list)
    echo '[]'
    ;;
  pr:list)
    echo '[{"number":5}]'
    ;;
  pr:view)
    echo '{"number":5}'
    ;;
  api:*)
    echo '{}'
    ;;
  *)
    echo "unexpected gh args: $*" >&2
    exit 42
    ;;
esac
`
}

func fakeGhForFinalVerify() string {
	return `#!/usr/bin/env bash
set -euo pipefail

if [ "${1:-}" = "repo" ] && [ "${2:-}" = "clone" ]; then
  if [ -n "${GH_TOKEN:-}" ] || [ -n "${GITHUB_TOKEN:-}" ]; then
    echo "final verify gh leaked ambient GitHub token env" >&2
    exit 43
  fi
  dest="${4:-}"
  if [ -z "$dest" ]; then
    echo "missing clone destination" >&2
    exit 42
  fi
  mkdir -p "$dest"
  printf '{"scripts":{"test":"true","build":"true","test:e2e":"true"}}\n' > "$dest/package.json"
  exit 0
fi

echo "unexpected gh args: $*" >&2
exit 42
`
}
