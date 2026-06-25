package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestWebTodoLifecycleRunbookDocumentsReusableSOP(t *testing.T) {
	root := repoRoot(t)
	readme := readFileString(t, filepath.Join(root, "README.md"))
	if !strings.Contains(readme, "docs/runbooks/local-gitea-webtodo-lifecycle-e2e.md") {
		t.Fatalf("README does not link the Web Todo lifecycle E2E runbook")
	}

	path := filepath.Join(root, "docs", "runbooks", "local-gitea-webtodo-lifecycle-e2e.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(body)
	for _, want := range []string{
		"scripts/e2e-webtodo-bootstrap.sh",
		"scripts/e2e-webtodo-capture.py",
		"scripts/e2e-webtodo-report.py",
		"Local binary mode reuses the operator's Codex configuration",
		"## 0. Prepare the Host Environment",
		`python3 -m venv "$AIOPS_WEBTODO_TOOLS_ROOT/venv"`,
		"python -m pip install playwright",
		"python -m playwright install chromium",
		"python -m playwright install-deps chromium",
		"Create the run-root directory skeleton",
		"set -euo pipefail",
		"CONTROL cancel running codex issue",
		`"$AIOPS_WEBTODO_MAKER_WORKFLOW"`,
		`"$AIOPS_WEBTODO_REVIEWER_WORKFLOW"`,
		`--dashboard-url "$AIOPS_WEBTODO_MAKER_DASHBOARD_URL"`,
		`--dashboard-url "$AIOPS_WEBTODO_REVIEWER_DASHBOARD_URL"`,
		"initially without\n  `aiops/*` state labels",
		"add `aiops/todo` to issues 1-10",
		"aiops/todo",
		"aiops/human-review",
		"aiops/rework",
		"aiops/done",
		"Do not commit `env.local`",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("runbook missing %q", want)
		}
	}
	assertInOrder(t, text, []string{
		"## 4. Run Maker and Reviewer",
		`--port "$AIOPS_WEBTODO_MAKER_PORT"`,
		`--dashboard-url "$AIOPS_WEBTODO_MAKER_DASHBOARD_URL"`,
		`--dashboard-url "$AIOPS_WEBTODO_REVIEWER_DASHBOARD_URL"`,
		"add `aiops/todo` to issues 1-10",
		"trigger a work poll:",
	})
	for _, forbidden := range []string{
		`"$AIOPS_WEBTODO_MAKER_WORKDIR" \`,
		`"$AIOPS_WEBTODO_REVIEWER_WORKDIR" \`,
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("runbook still passes workdir as worker positional argument: %q", forbidden)
		}
	}
}

func TestWebTodoBootstrapPreparesRunRoot(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	script := filepath.Join(root, "scripts", "e2e-webtodo-bootstrap.sh")
	cmd := exec.Command(
		"bash",
		script,
		"--run-root", runRoot,
		"--gitea-url", "https://gitea.example.test/",
		"--repo-owner", "aiops-bot",
		"--repo-name", "web-todo",
		"--port-base", "4100",
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
		"issues/01-scaffold-stdlib-server.md",
		"issues/13-control-blocked-held.md",
		"state",
		"artifacts",
		"logs",
		"promo/screenshots",
		"promo/pages",
		"promo/notes",
		"reports",
		"mirrors/maker",
		"mirrors/reviewer",
		"workspaces/maker",
		"workspaces/reviewer",
		"final-verify/screenshots",
		"final-verify/videos",
	} {
		if _, err := os.Stat(filepath.Join(runRoot, rel)); err != nil {
			t.Fatalf("bootstrap did not create %s: %v", rel, err)
		}
	}

	env := readFileString(t, filepath.Join(runRoot, "env.example"))
	for _, want := range []string{
		`export AIOPS_WEBTODO_MAKER_PORT="4101"`,
		`export AIOPS_WEBTODO_REVIEWER_PORT="4102"`,
		`export AIOPS_WEBTODO_TOOLS_ROOT="$HOME/.cache/aiops-webtodo-e2e-tools"`,
		`export AIOPS_WEBTODO_MAKER_MIRROR_ROOT=`,
		`export REVIEWER_CLONE_URL="https://review-bot:REPLACE_ME@gitea.example.test/aiops-bot/web-todo.git"`,
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("env.example missing %q\n%s", want, env)
		}
	}

	issue := readFileString(t, filepath.Join(runRoot, "issues", "04-patch-delete-api.md"))
	if !strings.Contains(issue, "completed must reject null") {
		t.Fatalf("issue 04 missing abnormal PATCH acceptance:\n%s", issue)
	}
	nextSteps := readFileString(t, filepath.Join(runRoot, "NEXT-STEPS.md"))
	for _, want := range []string{
		"Install or verify host tools",
		"Playwright venv",
		"Activate the Playwright venv",
		`--dashboard-url "$AIOPS_WEBTODO_MAKER_DASHBOARD_URL"`,
		`--dashboard-url "$AIOPS_WEBTODO_REVIEWER_DASHBOARD_URL"`,
		"without `aiops/*` state labels",
		"Add `aiops/todo` to primary issues 01-10",
	} {
		if !strings.Contains(nextSteps, want) {
			t.Fatalf("NEXT-STEPS.md missing %q\n%s", want, nextSteps)
		}
	}
	assertInOrder(t, nextSteps, []string{
		"Start maker on port",
		`--dashboard-url "$AIOPS_WEBTODO_MAKER_DASHBOARD_URL"`,
		`--dashboard-url "$AIOPS_WEBTODO_REVIEWER_DASHBOARD_URL"`,
		"Add `aiops/todo` to primary issues 01-10",
	})

	makerWorkflow := readFileString(t, filepath.Join(runRoot, "workflows", "maker-WORKFLOW.md"))
	reviewerWorkflow := readFileString(t, filepath.Join(runRoot, "workflows", "reviewer-automerge-WORKFLOW.md"))
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
				"  name: web-todo",
				"  endpoint: https://gitea.example.test",
				"  root: " + filepath.Join(runRoot, "workspaces", "maker"),
			},
		},
		{
			name: "reviewer",
			body: reviewerWorkflow,
			want: []string{
				"  owner: aiops-bot",
				"  name: web-todo",
				"  endpoint: https://gitea.example.test",
				"  root: " + filepath.Join(runRoot, "workspaces", "reviewer"),
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
}

func TestWebTodoReportGeneratesMarkdownFromState(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	mkdirAll(t,
		filepath.Join(runRoot, "state"),
		filepath.Join(runRoot, "promo", "screenshots"),
		filepath.Join(runRoot, "promo", "pages"),
		filepath.Join(runRoot, "final-verify", "screenshots"),
		filepath.Join(runRoot, "final-verify", "videos"),
	)
	writeFileString(t, filepath.Join(runRoot, "state", "issues-final.json"), fakeDoneIssuesJSON())
	writeFileString(t, filepath.Join(runRoot, "state", "prs-final.json"), fakeMergedPRsJSON())
	writeFileString(t, filepath.Join(runRoot, "state", "maker-final.json"), `{"counts":{"running":0,"blocked":0}}`)
	writeFileString(t, filepath.Join(runRoot, "state", "reviewer-final.json"), `{"counts":{"running":0,"blocked":0}}`)
	writeFileString(t, filepath.Join(runRoot, "promo", "screenshots", "maker-dashboard.png"), "png")
	writeFileString(t, filepath.Join(runRoot, "promo", "pages", "tui-maker-final.txt"), "maker frame")
	writeFileString(t, filepath.Join(runRoot, "final-verify", "screenshots", "web-final.png"), "png")
	writeFileString(t, filepath.Join(runRoot, "final-verify", "videos", "web-final.webm"), "video")
	writeFileString(t, filepath.Join(runRoot, "final-verify", "verification.log"), "go test ./...")

	script := filepath.Join(root, "scripts", "e2e-webtodo-report.py")
	cmd := exec.Command("python3", script, "--run-root", runRoot, "--date", "2026-06-17")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("report failed: %v\n%s", err, out)
	}

	report := readFileString(t, filepath.Join(runRoot, "reports", "report.md"))
	for _, want := range []string{
		"READY FOR OPERATOR PASS REVIEW",
		"Operator Pass Checklist",
		"Control no-ready issue never dispatched",
		"#1",
		"#10",
		"Evidence Inventory",
		"Final Web UI screenshots",
		"final-verify/videos/web-final.webm",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q\n%s", want, report)
		}
	}
	promo := readFileString(t, filepath.Join(runRoot, "promo", "notes", "promotion-materials.md"))
	if !strings.Contains(promo, "Local binary mode intentionally uses the operator's Codex configuration") {
		t.Fatalf("promo notes missing local Codex configuration caveat:\n%s", promo)
	}
}

func TestWebTodoCaptureScriptDocumentsPlaywrightAndTUI(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "scripts", "e2e-webtodo-capture.py")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(body)
	for _, want := range []string{
		"playwright.sync_api",
		"--screenshot",
		"--no-default-screenshots",
		"safe_screenshot_stem",
		"AIOPS_STATE_API_TOKEN",
		"tui",
		"--raw",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("capture script missing %q", want)
		}
	}
}

func TestWebTodoCaptureSkipsOnlyDefaultScreenshotsWithoutPlaywright(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "e2e-webtodo-capture.py")
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

func fakeDoneIssuesJSON() string {
	var rows []string
	for i := 1; i <= 10; i++ {
		rows = append(rows, `{"number":`+itoa(i)+`,"title":"issue `+itoa(i)+`","state":"closed","labels":[{"name":"aiops/done"}]}`)
	}
	return "[" + strings.Join(rows, ",") + "]"
}

func fakeMergedPRsJSON() string {
	var rows []string
	for i := 1; i <= 10; i++ {
		rows = append(rows, `{"number":`+itoa(i)+`,"title":"pr `+itoa(i)+`","state":"closed","merged":true,"head":{"ref":"ai/`+itoa(i)+`"}}`)
	}
	return "[" + strings.Join(rows, ",") + "]"
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

func mkdirAll(t *testing.T, dirs ...string) {
	t.Helper()
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

func writeFileString(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
