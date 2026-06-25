package scripts_test

import (
	"net/http"
	"net/http/httptest"
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
		"scripts/e2e-crowdrunner-freeze.py",
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
		"operator milestone evidence",
		"Do not commit `env.local`",
		"`state/stress-final.json`",
		`--tag final`,
		`--stress-url "$AIOPS_CROWDRUNNER_STRESS_DASHBOARD_URL"`,
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
	assertInOrder(t, text, []string{
		"## 9. Generate the Report Pack",
		"state/stress-final.json",
		"scripts/e2e-crowdrunner-capture.py",
		`--tag final`,
		"scripts/e2e-crowdrunner-report.py",
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
		"state/continuation-control-expected.json",
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

	controlIssue := readFileString(t, filepath.Join(runRoot, "issues", "16-control-continuation-budget.md"))
	for _, want := range []string{
		"CONTINUATION_BUDGET_CANARY",
		".aiops/operator-continuation-release",
		"both aiops/in-progress and aiops/stress",
		"must not edit files, commit, push, open a PR",
		`method of continuation_budget for issue #16`,
	} {
		if !strings.Contains(controlIssue, want) {
			t.Fatalf("issue 16 missing %q\n%s", want, controlIssue)
		}
	}

	expectation := readFileString(t, filepath.Join(runRoot, "state", "continuation-control-expected.json"))
	for _, want := range []string{
		`"kind": "continuation_budget_control_expectation"`,
		`"issue_number": 16`,
		`"active_label": "aiops/in-progress"`,
		`"routing_label": "aiops/stress"`,
		`"expected_blocked_method": "continuation_budget"`,
		`"forbidden_pr_issue_reference": "#16"`,
		`"aiops/human-review"`,
	} {
		if !strings.Contains(expectation, want) {
			t.Fatalf("continuation expectation missing %q\n%s", want, expectation)
		}
	}

	nextSteps := readFileString(t, filepath.Join(runRoot, "NEXT-STEPS.md"))
	for _, want := range []string{
		`--dashboard-url "$AIOPS_CROWDRUNNER_MAKER_DASHBOARD_URL"`,
		`--dashboard-url "$AIOPS_CROWDRUNNER_REVIEWER_DASHBOARD_URL"`,
		"without\n   `aiops/*` state labels",
		"Add `aiops/todo` to product issues 01-12",
		"e2e-crowdrunner-freeze.py",
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
		"e2e-crowdrunner-freeze.py",
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
				"    - In Progress",
				"  required_labels:",
				"    - aiops/stress",
				"  root: " + filepath.Join(runRoot, "workspaces", "stress"),
				"deterministic continuation-budget stress-control agent",
				"Do not edit files, create branches, commit, push, open a pull request",
				`method: "continuation_budget"`,
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
	if strings.Contains(stressWorkflow, "    - Todo") ||
		strings.Contains(stressWorkflow, "    - Stress") ||
		strings.Contains(stressWorkflow, "    - Rework") {
		t.Fatalf("stress workflow should use mapped In Progress plus required labels, not maker/stress states:\n%s", stressWorkflow)
	}
	if strings.Contains(stressWorkflow, "You are an autonomous MAKER agent") {
		t.Fatalf("stress workflow should not inherit the maker PR prompt:\n%s", stressWorkflow)
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
	writeFileString(t, filepath.Join(runRoot, "state", "issues-final.json"), fakeCrowdRunnerDoneIssuesWithOpenControlJSON())
	writeFileString(t, filepath.Join(runRoot, "state", "prs-final.json"), fakeMergedPRsJSON())
	writeFileString(t, filepath.Join(runRoot, "state", "maker-final.json"), `{"counts":{"running":0,"blocked":0}}`)
	writeFileString(t, filepath.Join(runRoot, "state", "reviewer-final.json"), `{"counts":{"running":0,"blocked":0}}`)
	writeFileString(t, filepath.Join(runRoot, "state", "stress-final.json"), `{
		"counts":{"running":0,"blocked":1},
		"blocked":[{"issue_identifier":"#16","issue_url":"http://gitea.local/aiops-bot/crowd-runner-product/issues/16","method":"continuation_budget"}]
	}`)
	writeFileString(t, filepath.Join(runRoot, "state", "continuation-control-expected.json"), `{
		"kind":"continuation_budget_control_expectation",
		"issue_number":16,
		"expected_blocked_method":"continuation_budget",
		"forbidden_pr_issue_reference":"#16"
	}`)
	writeFileString(t, filepath.Join(runRoot, "state", "operator-milestone-freeze-after-10.json"), `{
		"kind":"operator_milestone_freeze",
		"stop_after":10,
		"completed_product_issues":10,
		"ready_labels_removed":[{"number":11},{"number":12}]
	}`)
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
		"Operator milestone freeze",
		"ready labels removed from #11, #12",
		"Continuation / turn-budget stress evidence",
		"Continuation control: PASS",
		"issue #16 blocked via `continuation_budget`",
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

func TestCrowdRunnerReportRejectsContinuationControlPR(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	mkdirAll(t, filepath.Join(runRoot, "state"), filepath.Join(runRoot, "reports"), filepath.Join(runRoot, "promo", "notes"))
	writeFileString(t, filepath.Join(runRoot, "state", "issues-final.json"), fakeCrowdRunnerDoneIssuesWithOpenControlJSON())
	writeFileString(t, filepath.Join(runRoot, "state", "prs-final.json"), `[
		{"number":22,"title":"test: finish control","body":"","state":"closed","merged":true,"head":{"ref":"ai/16"}}
	]`)
	writeFileString(t, filepath.Join(runRoot, "state", "stress-final.json"), `{
		"counts":{"running":0,"blocked":1},
		"blocked":[{"issue_identifier":"#16","method":"continuation_budget"}]
	}`)
	writeFileString(t, filepath.Join(runRoot, "state", "continuation-control-expected.json"), `{
		"kind":"continuation_budget_control_expectation",
		"issue_number":16,
		"expected_blocked_method":"continuation_budget",
		"forbidden_pr_issue_reference":"#16"
	}`)

	cmd := exec.Command("python3", filepath.Join(root, "scripts", "e2e-crowdrunner-report.py"), "--run-root", runRoot)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("report failed: %v\n%s", err, out)
	}
	report := readFileString(t, filepath.Join(runRoot, "reports", "report.md"))
	if !strings.Contains(report, "Continuation control: FAIL: control issue #16 produced PR(s) #22") {
		t.Fatalf("report did not reject the control PR:\n%s", report)
	}
}

func TestCrowdRunnerReportRejectsContinuationControlPRByTrackerID(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	mkdirAll(t, filepath.Join(runRoot, "state"), filepath.Join(runRoot, "reports"), filepath.Join(runRoot, "promo", "notes"))
	doneIssues := strings.TrimSuffix(strings.TrimPrefix(fakeCrowdRunnerDoneIssuesJSON(), "["), "]")
	controlIssue := `{"id":1601,"number":16,"title":"control","state":"open","labels":[]}`
	writeFileString(t, filepath.Join(runRoot, "state", "issues-final.json"), "["+doneIssues+","+controlIssue+"]")
	writeFileString(t, filepath.Join(runRoot, "state", "prs-final.json"), `[
		{"number":23,"title":"test: finish unrelated","body":"","state":"closed","merged":true,"head":{"ref":"ai/1601"}}
	]`)
	writeFileString(t, filepath.Join(runRoot, "state", "stress-final.json"), `{
		"counts":{"running":0,"blocked":1},
		"blocked":[{"issue_identifier":"#16","method":"continuation_budget"}]
	}`)
	writeFileString(t, filepath.Join(runRoot, "state", "continuation-control-expected.json"), `{
		"kind":"continuation_budget_control_expectation",
		"issue_number":16,
		"expected_blocked_method":"continuation_budget"
	}`)

	cmd := exec.Command("python3", filepath.Join(root, "scripts", "e2e-crowdrunner-report.py"), "--run-root", runRoot)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("report failed: %v\n%s", err, out)
	}
	report := readFileString(t, filepath.Join(runRoot, "reports", "report.md"))
	if !strings.Contains(report, "Continuation control: FAIL: control issue #16 produced PR(s) #23") {
		t.Fatalf("report did not reject the tracker-id control PR:\n%s", report)
	}
}

func TestCrowdRunnerReportRejectsContinuationControlTerminalLabel(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	mkdirAll(t, filepath.Join(runRoot, "state"), filepath.Join(runRoot, "reports"), filepath.Join(runRoot, "promo", "notes"))
	writeFileString(t, filepath.Join(runRoot, "state", "issues-final.json"), fakeCrowdRunnerDoneIssuesWithOpenControlJSON("aiops/human-review"))
	writeFileString(t, filepath.Join(runRoot, "state", "prs-final.json"), `[]`)
	writeFileString(t, filepath.Join(runRoot, "state", "stress-final.json"), `{
		"counts":{"running":0,"blocked":1},
		"blocked":[{"issue_identifier":"#16","method":"continuation_budget"}]
	}`)
	writeFileString(t, filepath.Join(runRoot, "state", "continuation-control-expected.json"), `{
		"kind":"continuation_budget_control_expectation",
		"issue_number":16,
		"expected_blocked_method":"continuation_budget",
		"forbidden_terminal_labels":["aiops/done","aiops/canceled","aiops/human-review"]
	}`)

	cmd := exec.Command("python3", filepath.Join(root, "scripts", "e2e-crowdrunner-report.py"), "--run-root", runRoot)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("report failed: %v\n%s", err, out)
	}
	report := readFileString(t, filepath.Join(runRoot, "reports", "report.md"))
	if !strings.Contains(report, "Continuation control: FAIL: control issue #16 has forbidden label(s) aiops/human-review.") {
		t.Fatalf("report did not reject the control terminal label:\n%s", report)
	}
}

func TestCrowdRunnerReportRejectsAnonymousContinuationControlRow(t *testing.T) {
	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	mkdirAll(t, filepath.Join(runRoot, "state"), filepath.Join(runRoot, "reports"), filepath.Join(runRoot, "promo", "notes"))
	writeFileString(t, filepath.Join(runRoot, "state", "issues-final.json"), fakeCrowdRunnerDoneIssuesWithOpenControlJSON())
	writeFileString(t, filepath.Join(runRoot, "state", "prs-final.json"), `[]`)
	writeFileString(t, filepath.Join(runRoot, "state", "stress-final.json"), `{
		"counts":{"running":0,"blocked":1},
		"blocked":[{"method":"continuation_budget"}]
	}`)
	writeFileString(t, filepath.Join(runRoot, "state", "continuation-control-expected.json"), `{
		"kind":"continuation_budget_control_expectation",
		"issue_number":16,
		"expected_blocked_method":"continuation_budget"
	}`)

	cmd := exec.Command("python3", filepath.Join(root, "scripts", "e2e-crowdrunner-report.py"), "--run-root", runRoot)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("report failed: %v\n%s", err, out)
	}
	report := readFileString(t, filepath.Join(runRoot, "reports", "report.md"))
	want := "Continuation control: FAIL: stress worker blocked, but no issue #16 `continuation_budget` row was captured."
	if !strings.Contains(report, want) {
		t.Fatalf("report accepted anonymous continuation row; missing %q\n%s", want, report)
	}
}

func TestCrowdRunnerFreezeStopsDispatchAfterMilestone(t *testing.T) {
	var deletes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token test-token" {
			t.Fatalf("Authorization = %q; want token test-token", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/aiops-bot/crowd-runner-product/issues":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fakeCrowdRunnerMilestoneIssuesJSON()))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/repos/aiops-bot/crowd-runner-product/issues/"):
			deletes = append(deletes, r.URL.Path)
			if strings.Contains(r.URL.Path, "/issues/11/") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()

	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	script := filepath.Join(root, "scripts", "e2e-crowdrunner-freeze.py")
	cmd := exec.Command(
		"python3",
		script,
		"--run-root", runRoot,
		"--gitea-url", srv.URL,
		"--repo-owner", "aiops-bot",
		"--repo-name", "crowd-runner-product",
		"--token", "test-token",
		"--stop-after", "10",
		"--timeout-seconds", "1",
		"--poll-interval-seconds", "0.01",
	)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("freeze helper failed: %v\n%s", err, out)
	}

	wantDeletes := []string{
		"/api/v1/repos/aiops-bot/crowd-runner-product/issues/11/labels/501",
		"/api/v1/repos/aiops-bot/crowd-runner-product/issues/12/labels/501",
	}
	if strings.Join(deletes, "\n") != strings.Join(wantDeletes, "\n") {
		t.Fatalf("delete calls = %v; want %v", deletes, wantDeletes)
	}
	state := readFileString(t, filepath.Join(runRoot, "state", "operator-milestone-freeze-after-10.json"))
	for _, want := range []string{
		`"kind": "operator_milestone_freeze"`,
		`"completed_product_issues": 10`,
		`"number": 12`,
		`"already_inactive_product_issues": [
    11
  ]`,
	} {
		if !strings.Contains(state, want) {
			t.Fatalf("freeze state missing %q\n%s", want, state)
		}
	}
	fragment := readFileString(t, filepath.Join(runRoot, "reports", "operator-milestone-freeze-after-10.md"))
	if !strings.Contains(fragment, "operator milestone freeze, not a worker failure") {
		t.Fatalf("freeze report did not classify the stop as operator freeze:\n%s", fragment)
	}
}

func TestCrowdRunnerFreezeRedactsFailureSecrets(t *testing.T) {
	const token = "freeze-secret-token"
	const proxySecret = "proxy-secret"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token "+token {
			t.Fatalf("Authorization = %q; want token %s", got, token)
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("echoed " + token + " and https://bot:" + proxySecret + "@gitea.example.test/path"))
	}))
	defer srv.Close()

	root := repoRoot(t)
	runRoot := filepath.Join(t.TempDir(), "run")
	run := func(giteaURL string) string {
		cmd := exec.Command(
			"python3",
			filepath.Join(root, "scripts", "e2e-crowdrunner-freeze.py"),
			"--run-root", runRoot,
			"--gitea-url", giteaURL,
			"--repo-owner", "aiops-bot",
			"--repo-name", "crowd-runner-product",
			"--token", token,
			"--stop-after", "10",
			"--timeout-seconds", "1",
			"--poll-interval-seconds", "0.01",
		)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("freeze helper unexpectedly succeeded:\n%s", out)
		}
		return string(out)
	}

	userinfoOut := run(strings.Replace(srv.URL, "http://", "http://bot:"+proxySecret+"@", 1))
	if strings.Contains(userinfoOut, proxySecret) || !strings.Contains(userinfoOut, "http://[redacted]@") {
		t.Fatalf("freeze helper did not redact URL userinfo:\n%s", userinfoOut)
	}

	bodyOut := run(srv.URL)
	for _, forbidden := range []string{token, proxySecret} {
		if strings.Contains(bodyOut, forbidden) {
			t.Fatalf("freeze helper leaked secret %q:\n%s", forbidden, bodyOut)
		}
	}
	for _, want := range []string{"[redacted-token]", "https://[redacted]@"} {
		if !strings.Contains(bodyOut, want) {
			t.Fatalf("freeze helper output missing redaction marker %q:\n%s", want, bodyOut)
		}
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

func fakeCrowdRunnerDoneIssuesWithOpenControlJSON(labels ...string) string {
	body := strings.TrimSuffix(strings.TrimPrefix(fakeCrowdRunnerDoneIssuesJSON(), "["), "]")
	var labelRows []string
	for _, label := range labels {
		labelRows = append(labelRows, `{"name":"`+label+`"}`)
	}
	control := `{"number":16,"title":"control","state":"open","labels":[` + strings.Join(labelRows, ",") + `]}`
	return "[" + body + "," + control + "]"
}

func fakeCrowdRunnerMilestoneIssuesJSON() string {
	var rows []string
	for i := 1; i <= 10; i++ {
		rows = append(rows, `{"number":`+itoa(i)+`,"title":"issue `+itoa(i)+`","state":"closed","labels":[{"id":502,"name":"aiops/done"}]}`)
	}
	rows = append(rows,
		`{"number":11,"title":"issue 11","state":"open","labels":[{"id":501,"name":"aiops/todo"}]}`,
		`{"number":12,"title":"issue 12","state":"open","labels":[{"id":501,"name":"aiops/todo"}]}`,
		`{"number":13,"title":"control","state":"open","labels":[{"id":501,"name":"aiops/todo"}]}`,
	)
	return "[" + strings.Join(rows, ",") + "]"
}
