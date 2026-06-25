package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCrowdRunnerLifecycleRunbookDocumentsReusableSOP(t *testing.T) {
	root := repoRoot(t)
	readme := readFileString(t, filepath.Join(root, "README.md"))
	if !strings.Contains(readme, "docs/runbooks/local-gitea-crowdrunner-lifecycle-e2e.md") {
		t.Fatalf("README does not link the Crowd Runner lifecycle E2E runbook")
	}

	path := filepath.Join(root, "docs", "runbooks", "local-gitea-crowdrunner-lifecycle-e2e.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(body)
	for _, want := range []string{
		"scripts/e2e-crowdrunner-bootstrap.sh",
		"scripts/e2e-crowdrunner-capture.py",
		"scripts/e2e-crowdrunner-report.py",
		"Local binary mode intentionally reuses the operator's usual Codex setup",
		"aiops-platform_v0.1.9_darwin_arm64.tar.gz",
		"crowd-runner-product",
		"real `codex app-server`",
		"CONTROL cancel running Codex issue",
		"Continuation budget",
		"npm run test:e2e",
		"without `aiops/*` state labels",
		"add `aiops/todo` to issues 1-12",
		`--dashboard-url "$AIOPS_CROWDRUNNER_MAKER_DASHBOARD_URL"`,
		`--dashboard-url "$AIOPS_CROWDRUNNER_REVIEWER_DASHBOARD_URL"`,
		"Do not commit `env.local`",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("runbook missing %q", want)
		}
	}
	assertInOrder(t, text, []string{
		"## 5. Run Maker and Reviewer",
		`"$AIOPS_CROWDRUNNER_WORKER_BIN" --port "$AIOPS_CROWDRUNNER_MAKER_PORT"`,
		`--dashboard-url "$AIOPS_CROWDRUNNER_MAKER_DASHBOARD_URL"`,
		`--dashboard-url "$AIOPS_CROWDRUNNER_REVIEWER_DASHBOARD_URL"`,
		"add `aiops/todo` to issues 1-12",
		"trigger a work poll:",
	})
}

func TestCrowdRunnerBootstrapPreparesRunRoot(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	script := filepath.Join(root, "scripts", "e2e-crowdrunner-bootstrap.sh")
	cmd := exec.Command(
		"bash",
		script,
		"--run-root", runRoot,
		"--gitea-url", "https://gitea.example.test/",
		"--repo-owner", "aiops-bot",
		"--repo-name", "crowd-runner-product",
		"--port-base", "4200",
		"--release-tag", "v0.1.9",
	)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bootstrap failed: %v\n%s", err, out)
	}

	for _, rel := range []string{
		"env.example",
		"NEXT-STEPS.md",
		"workflows/maker-WORKFLOW.md",
		"workflows/reviewer-automerge-WORKFLOW.md",
		"workflows/maker-low-turn-WORKFLOW.md",
		"seed-repo/package.json",
		"seed-repo/.gitea/workflows/ci.yml",
		"seed-repo/docs/product/brief.md",
		"issues/01-product-foundation.md",
		"issues/12-release-polish-pwa.md",
		"issues/16-control-continuation-budget.md",
		"state",
		"artifacts",
		"logs",
		"downloads",
		"promo/screenshots",
		"promo/pages",
		"promo/notes",
		"reports",
		"mirrors/maker",
		"mirrors/reviewer",
		"mirrors/stress",
		"workspaces/maker",
		"workspaces/reviewer",
		"workspaces/stress",
		"final-verify/screenshots",
		"final-verify/videos",
		"final-verify/traces",
		"final-verify/playwright-report",
	} {
		if _, err := os.Stat(filepath.Join(runRoot, rel)); err != nil {
			t.Fatalf("bootstrap did not create %s: %v", rel, err)
		}
	}

	env := readFileString(t, filepath.Join(runRoot, "env.example"))
	for _, want := range []string{
		`export AIOPS_CROWDRUNNER_RELEASE_TAG="v0.1.9"`,
		`export AIOPS_CROWDRUNNER_MAKER_PORT="4201"`,
		`export AIOPS_CROWDRUNNER_REVIEWER_PORT="4202"`,
		`export AIOPS_CROWDRUNNER_STRESS_PORT="4203"`,
		`export AIOPS_CROWDRUNNER_TOOLS_ROOT="$HOME/.cache/aiops-crowdrunner-e2e-tools"`,
		`export REVIEWER_CLONE_URL="https://review-bot:REPLACE_ME@gitea.example.test/aiops-bot/crowd-runner-product.git"`,
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("env.example missing %q\n%s", want, env)
		}
	}

	productBrief := readFileString(t, filepath.Join(runRoot, "seed-repo", "docs", "product", "brief.md"))
	for _, want := range []string{
		"choose the best gate",
		"Core logic is testable without WebGL",
		"real product foundation",
	} {
		if !strings.Contains(productBrief, want) {
			t.Fatalf("product brief missing %q\n%s", want, productBrief)
		}
	}

	issue := readFileString(t, filepath.Join(runRoot, "issues", "07-rework-quality-gate.md"))
	for _, want := range []string{
		"[EXPECT-REWORK]",
		"Tests must execute behavior",
		"Reviewer should reject placebo coverage",
	} {
		if !strings.Contains(issue, want) {
			t.Fatalf("issue 07 missing %q\n%s", want, issue)
		}
	}

	nextSteps := readFileString(t, filepath.Join(runRoot, "NEXT-STEPS.md"))
	for _, want := range []string{
		`--dashboard-url "$AIOPS_CROWDRUNNER_MAKER_DASHBOARD_URL"`,
		`--dashboard-url "$AIOPS_CROWDRUNNER_REVIEWER_DASHBOARD_URL"`,
		"without\n   `aiops/*` state labels",
		"Add `aiops/todo` to product issues 01-12",
	} {
		if !strings.Contains(nextSteps, want) {
			t.Fatalf("NEXT-STEPS.md missing %q\n%s", want, nextSteps)
		}
	}
	assertInOrder(t, nextSteps, []string{
		"Start maker on port",
		`--dashboard-url "$AIOPS_CROWDRUNNER_MAKER_DASHBOARD_URL"`,
		`--dashboard-url "$AIOPS_CROWDRUNNER_REVIEWER_DASHBOARD_URL"`,
		"Add `aiops/todo` to product issues 01-12",
	})

	makerWorkflow := readFileString(t, filepath.Join(runRoot, "workflows", "maker-WORKFLOW.md"))
	reviewerWorkflow := readFileString(t, filepath.Join(runRoot, "workflows", "reviewer-automerge-WORKFLOW.md"))
	stressWorkflow := readFileString(t, filepath.Join(runRoot, "workflows", "maker-low-turn-WORKFLOW.md"))
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{
			name: "maker",
			body: makerWorkflow,
			want: []string{
				"  owner: aiops-bot",
				"  name: crowd-runner-product",
				"  endpoint: https://gitea.example.test",
				"  root: " + filepath.Join(runRoot, "workspaces", "maker"),
				"    - npm ci",
				"    - npm run lint",
				"    - npm run test -- --run",
				"    - npm run build",
			},
		},
		{
			name: "reviewer",
			body: reviewerWorkflow,
			want: []string{
				"  owner: aiops-bot",
				"  name: crowd-runner-product",
				"  endpoint: https://gitea.example.test",
				"  root: " + filepath.Join(runRoot, "workspaces", "reviewer"),
				"npm ci, npm run lint, npm run test -- --run, and npm run build",
			},
		},
		{
			name: "stress",
			body: stressWorkflow,
			want: []string{
				"  max_turns: 1",
				"  max_continuation_turns: 2",
				"    - Stress",
				"  root: " + filepath.Join(runRoot, "workspaces", "stress"),
			},
		},
	} {
		for _, want := range tc.want {
			if !strings.Contains(tc.body, want) {
				t.Fatalf("%s workflow missing %q\n%s", tc.name, want, tc.body)
			}
		}
		for _, forbidden := range []string{"your-gitea-user", "your-repo", "http://gitea.local", "~/aiops-workspaces"} {
			if strings.Contains(tc.body, forbidden) {
				t.Fatalf("%s workflow still contains placeholder %q\n%s", tc.name, forbidden, tc.body)
			}
		}
	}
	if strings.Contains(stressWorkflow, "    - Todo") || strings.Contains(stressWorkflow, "    - Rework") {
		t.Fatalf("stress workflow should not claim Todo/Rework states:\n%s", stressWorkflow)
	}
}

func TestCrowdRunnerReportGeneratesThreeVerdicts(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	mkdirAll(t,
		filepath.Join(runRoot, "state"),
		filepath.Join(runRoot, "promo", "screenshots"),
		filepath.Join(runRoot, "promo", "pages"),
		filepath.Join(runRoot, "final-verify", "screenshots"),
		filepath.Join(runRoot, "final-verify", "videos"),
		filepath.Join(runRoot, "final-verify", "traces"),
		filepath.Join(runRoot, "final-verify", "playwright-report"),
		filepath.Join(runRoot, "logs"),
	)
	writeFileString(t, filepath.Join(runRoot, "state", "issues-final.json"), fakeCrowdRunnerDoneIssuesJSON())
	writeFileString(t, filepath.Join(runRoot, "state", "prs-final.json"), fakeMergedPRsJSON())
	writeFileString(t, filepath.Join(runRoot, "state", "maker-final.json"), `{"counts":{"running":0,"blocked":0}}`)
	writeFileString(t, filepath.Join(runRoot, "state", "reviewer-final.json"), `{"counts":{"running":0,"blocked":0}}`)
	writeFileString(t, filepath.Join(runRoot, "state", "stress-final.json"), `{"counts":{"running":0,"blocked":1}}`)
	writeFileString(t, filepath.Join(runRoot, "promo", "screenshots", "maker-dashboard.png"), "png")
	writeFileString(t, filepath.Join(runRoot, "promo", "pages", "tui-maker-final.txt"), "maker frame")
	writeFileString(t, filepath.Join(runRoot, "final-verify", "screenshots", "gameplay.png"), "png")
	writeFileString(t, filepath.Join(runRoot, "final-verify", "videos", "gameplay.webm"), "video")
	writeFileString(t, filepath.Join(runRoot, "logs", "maker-worker.log"), "worker")
	writeFileString(t, filepath.Join(runRoot, "final-verify", "verification.log"), strings.Join([]string{
		"$ npm ci",
		"$ npm run lint",
		"$ npm run test -- --run",
		"$ npm run test:e2e",
		"$ npm run build",
	}, "\n"))

	script := filepath.Join(root, "scripts", "e2e-crowdrunner-report.py")
	cmd := exec.Command("python3", script, "--run-root", runRoot, "--date", "2026-06-24")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("report failed: %v\n%s", err, out)
	}

	report := readFileString(t, filepath.Join(runRoot, "reports", "report.md"))
	for _, want := range []string{
		"aiops-platform lifecycle",
		"Codex product delivery",
		"Product quality",
		"READY FOR OPERATOR PASS REVIEW",
		"At least ten product issues reached `aiops/done`",
		"Continuation / turn-budget stress evidence",
		"#12",
		"final-verify/videos/gameplay.webm",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q\n%s", want, report)
		}
	}
	promo := readFileString(t, filepath.Join(runRoot, "promo", "notes", "promotion-materials.md"))
	if !strings.Contains(promo, "fresh crowd-runner design") {
		t.Fatalf("promo notes missing fresh product positioning:\n%s", promo)
	}
}

func TestCrowdRunnerCaptureSkipsOnlyDefaultScreenshotsWithoutPlaywright(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "e2e-crowdrunner-capture.py")
	shadow := filepath.Join(t.TempDir(), "py")
	mkdirAll(t, filepath.Join(shadow, "playwright"))
	writeFileString(t, filepath.Join(shadow, "playwright", "__init__.py"), "")
	writeFileString(t, filepath.Join(shadow, "playwright", "sync_api.py"), "raise ImportError('blocked playwright')\n")

	runRoot := filepath.Join(t.TempDir(), "default")
	cmd := exec.Command(
		"python3",
		script,
		"--run-root", runRoot,
		"--gitea-url", "http://127.0.0.1:9",
	)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PYTHONPATH="+shadow)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("default screenshot capture should skip missing Playwright, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "skipped default screenshots") {
		t.Fatalf("default capture did not report skipped screenshots:\n%s", out)
	}

	explicitRunRoot := filepath.Join(t.TempDir(), "explicit")
	cmd = exec.Command(
		"python3",
		script,
		"--run-root", explicitRunRoot,
		"--screenshot", "required=http://127.0.0.1:9",
	)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PYTHONPATH="+shadow)
	out, err = cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("explicit screenshot capture should require Playwright:\n%s", out)
	}
	if !strings.Contains(string(out), "Playwright is required for requested screenshots") {
		t.Fatalf("explicit capture did not report missing Playwright:\n%s", out)
	}
}

func fakeCrowdRunnerDoneIssuesJSON() string {
	var rows []string
	for i := 1; i <= 12; i++ {
		rows = append(rows, `{"number":`+itoa(i)+`,"title":"issue `+itoa(i)+`","state":"closed","labels":[{"name":"aiops/done"}]}`)
	}
	return "[" + strings.Join(rows, ",") + "]"
}
