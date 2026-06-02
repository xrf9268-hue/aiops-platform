package scripts_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTodoSmokeScriptRunsDoctorAndWritesReport(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "aiops-todo-smoke.sh")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("read %s: %v", script, err)
	}
	text := string(body)
	for _, want := range []string{
		`doctor_args=(--doctor --mode="$mode")`,
		`doctor_args+=("$workflow")`,
		`"$worker_bin" "${doctor_args[@]}"`,
		"Authorization: Bearer $state_api_token",
		"api_curl -X POST -H 'X-AIOPS-Refresh: true'",
		"dashboard-url must include an explicit host:port",
		"dashboard_port=\"${dashboard_hostport##*:}\"",
		"ready=\"false\"",
		"FAIL timed out waiting for worker readiness.",
		"\"$worker_bin\" --port=\"$dashboard_port\" \"$workflow\"",
		"X-AIOPS-Refresh: true",
		"selected issue",
		"[ -z \"$issue\" ] && [ \"$completed_now\" -gt \"$completed_before\" ]",
		"completed_total advanced",
		"docs/validation/smoke",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("smoke script missing %q", want)
		}
	}
}

func TestTodoSmokeScriptSupportsGitHubDraftPRValidation(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "aiops-todo-smoke.sh")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("read %s: %v", script, err)
	}
	text := string(body)
	for _, want := range []string{
		`--github-repo OWNER/REPO --github-issue NUMBER --expect-draft-pr`,
		`expect-draft-pr requires --github-issue and --github-repo`,
		`draft_pr_poll_attempts="${AIOPS_SMOKE_PR_POLL_ATTEMPTS:-30}"`,
		`doctor_args+=(--github-issue "$github_issue")`,
		`doctor_args+=(--github-repo "$github_repo")`,
		`verify_expected_draft_pr`,
		`gh api --paginate`,
		`gh pr view "$number"`,
		`open_draft_prs`,
		`pulls?state=open&per_page=100`,
		`smoke_started_at`,
		`--json number,isDraft,url,closingIssuesReferences`,
		`.url == \"$github_issue_url\"`,
		`while [ "$attempt" -le "$draft_pr_poll_attempts" ]`,
		`no new open draft PR in`,
		"Verified new `%s` closes `%s#%s`.",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("smoke script missing GitHub PR validation fragment %q", want)
		}
	}
	if strings.Contains(text, `--search "$github_issue"`) {
		t.Fatalf("smoke script should not use gh search API for draft PR verification")
	}
}

func TestTodoSmokeScriptAcceptsDraftPRCreatedAfterBaseline(t *testing.T) {
	report, err := runTodoSmokePRFixture(t, "new")
	if err != nil {
		t.Fatalf("smoke script with new draft PR failed: %v\n%s", err, report)
	}
	if !strings.Contains(report, "Verified new `#11 https://example.test/pr/11` closes `xrf9268-hue/aiops-platform#450`") {
		t.Fatalf("report missing new draft PR verification:\n%s", report)
	}
}

func TestTodoSmokeScriptRejectsStaleDraftPR(t *testing.T) {
	report, err := runTodoSmokePRFixture(t, "stale")
	if err == nil {
		t.Fatalf("smoke script with only stale draft PR succeeded; report:\n%s", report)
	}
	if !strings.Contains(report, "FAIL no new open draft PR") {
		t.Fatalf("report missing stale draft PR failure:\n%s", report)
	}
	if !strings.Contains(report, "State snapshot:") || !strings.Contains(report, "Worker log:") {
		t.Fatalf("report missing diagnostics for stale draft PR failure:\n%s", report)
	}
}

func TestTodoSmokeScriptRejectsEditedExistingDraftPR(t *testing.T) {
	report, err := runTodoSmokePRFixture(t, "edited")
	if err == nil {
		t.Fatalf("smoke script accepted existing draft PR edited after baseline; report:\n%s", report)
	}
	if !strings.Contains(report, "FAIL no new open draft PR") {
		t.Fatalf("report missing edited existing draft PR failure:\n%s", report)
	}
}

func TestTodoSmokeScriptRecordsDraftPRWhenWorkerFails(t *testing.T) {
	report, err := runTodoSmokePRFixture(t, "new", "failed")
	if err == nil {
		t.Fatalf("smoke script with failed worker succeeded; report:\n%s", report)
	}
	if !strings.Contains(report, "Verified new `#11 https://example.test/pr/11` closes `xrf9268-hue/aiops-platform#450`") {
		t.Fatalf("report missing draft PR verification on worker failure:\n%s", report)
	}
	if !strings.Contains(report, "FAIL selected issue `AIS-1` failed: boom") {
		t.Fatalf("report missing selected issue failure (with last_error detail):\n%s", report)
	}
}

func TestTodoSmokeScriptRecordsDraftPRWhenWorkerExitsBeforeReady(t *testing.T) {
	report, err := runTodoSmokePRFixture(t, "new", "exit")
	if err == nil {
		t.Fatalf("smoke script with early worker exit succeeded; report:\n%s", report)
	}
	if !strings.Contains(report, "Verified new `#11 https://example.test/pr/11` closes `xrf9268-hue/aiops-platform#450`") {
		t.Fatalf("report missing draft PR verification on early worker exit:\n%s", report)
	}
	if !strings.Contains(report, "FAIL worker exited before smoke completed") {
		t.Fatalf("report missing worker exit failure:\n%s", report)
	}
}

func TestTodoSmokeScriptRecordsDraftPRWhenRefreshFails(t *testing.T) {
	report, err := runTodoSmokePRFixture(t, "new", "refresh-fail")
	if err == nil {
		t.Fatalf("smoke script with refresh failure succeeded; report:\n%s", report)
	}
	if !strings.Contains(report, "Verified new `#11 https://example.test/pr/11` closes `xrf9268-hue/aiops-platform#450`") {
		t.Fatalf("report missing draft PR verification on refresh failure:\n%s", report)
	}
	if !strings.Contains(report, "FAIL refresh request failed.") {
		t.Fatalf("report missing refresh failure:\n%s", report)
	}
}

func TestTodoSmokeScriptRecordsDraftPRWhenStateRequestFails(t *testing.T) {
	report, err := runTodoSmokePRFixture(t, "new", "state-fail")
	if err == nil {
		t.Fatalf("smoke script with state failure succeeded; report:\n%s", report)
	}
	if !strings.Contains(report, "Verified new `#11 https://example.test/pr/11` closes `xrf9268-hue/aiops-platform#450`") {
		t.Fatalf("report missing draft PR verification on state failure:\n%s", report)
	}
	if !strings.Contains(report, "FAIL state request failed.") {
		t.Fatalf("report missing state failure:\n%s", report)
	}
}

func TestTodoSmokeScriptClassifiesPRViewFailure(t *testing.T) {
	report, err := runTodoSmokePRFixture(t, "view-fail")
	if err == nil {
		t.Fatalf("smoke script with pr view failure succeeded; report:\n%s", report)
	}
	if !strings.Contains(report, "FAIL `gh` could not inspect draft PRs") {
		t.Fatalf("report missing gh inspection failure:\n%s", report)
	}
	if strings.Contains(report, "FAIL no new open draft PR") {
		t.Fatalf("report misclassified gh inspection failure as missing PR:\n%s", report)
	}
}

func TestTodoSmokeScriptUsesPrintfOptionSeparatorForMarkdownLists(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "aiops-todo-smoke.sh")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("read %s: %v", script, err)
	}
	text := string(body)
	for _, want := range []string{
		`printf -- '- timestamp:`,
		`printf -- '- mode:`,
		`printf -- '- workflow:`,
		`printf -- '- dashboard_url:`,
		`printf -- '- issue:`,
		`printf -- '- github_repo:`,
		`printf -- '- github_issue:`,
		`printf -- '- expect_draft_pr:`,
		`printf -- '- workspace_root:`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("smoke script missing %q", want)
		}
	}
}

func TestTodoSmokeScriptUsesPortableMktempTemplates(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "aiops-todo-smoke.sh")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("read %s: %v", script, err)
	}
	text := string(body)
	for _, forbidden := range []string{
		`XXXXXX.log`,
		`XXXXXX.json`,
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("smoke script contains non-portable mktemp template %q", forbidden)
		}
	}
	for _, want := range []string{
		`aiops-smoke-worker.log.XXXXXX`,
		`aiops-smoke-state.json.XXXXXX`,
		`aiops-smoke-issue.json.XXXXXX`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("smoke script missing portable mktemp template %q", want)
		}
	}
}

func TestTodoSmokeScriptRequiresIssueIDInsideTerminalArrays(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, "scripts", "aiops-todo-smoke.sh")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("read %s: %v", script, err)
	}
	text := string(body)
	for _, want := range []string{
		// Completed is still detected by membership in the `completed` array;
		// failure is now detected by a non-null per-issue `last_error` (a failed
		// run is parked in `retrying`, not a terminal `failed` set — #584, D29).
		`state_array_contains_issue completed "$selected_issue_id" "$state_file"`,
		`[ -n "$selected_error" ]`,
		`"\"$field\":[[:space:]]*\[[^]]*\"$issue_id\""`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("smoke script missing selected-issue lifecycle check %q", want)
		}
	}
	for _, forbidden := range []string{
		`"\"completed\":[^]]*\"$selected_issue_id\""`,
		`"\"failed\":[^]]*\"$selected_issue_id\""`,
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("smoke script contains false-positive selected-issue grep %q", forbidden)
		}
	}
}

func runTodoSmokePRFixture(t *testing.T, ghMode string, workerMode ...string) (string, error) {
	t.Helper()
	root := repoRoot(t)
	fakeBin := t.TempDir()
	reportDir := t.TempDir()
	workflow := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(workflow, []byte("prompt\n"), 0o600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	port := freePort(t)
	writeFakeWorker(t, filepath.Join(fakeBin, "worker"))
	writeFakeGH(t, filepath.Join(fakeBin, "gh"))
	countFile := filepath.Join(t.TempDir(), "gh-count")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fakeWorkerState := "completed"
	if len(workerMode) > 0 {
		fakeWorkerState = workerMode[0]
	}
	cmd := exec.CommandContext(ctx,
		filepath.Join(root, "scripts", "aiops-todo-smoke.sh"),
		"--mode", "real",
		"--workflow", workflow,
		"--issue", "AIS-1",
		"--dashboard-url", fmt.Sprintf("http://127.0.0.1:%d", port),
		"--report-dir", reportDir,
		"--github-repo", "xrf9268-hue/aiops-platform",
		"--github-issue", "450",
		"--expect-draft-pr",
	)
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"AIOPS_FAKE_GH_COUNT="+countFile,
		"AIOPS_FAKE_GH_MODE="+ghMode,
		"AIOPS_FAKE_WORKER_STATE="+fakeWorkerState,
		"AIOPS_SMOKE_TIMEOUT_SECONDS=10",
		"AIOPS_SMOKE_PR_POLL_ATTEMPTS=2",
		"AIOPS_SMOKE_PR_POLL_INTERVAL_SECONDS=0",
	)
	out, err := cmd.CombinedOutput()
	report := latestSmokeReport(t, reportDir)
	if len(out) > 0 {
		report += "\nstdout/stderr:\n" + string(out)
	}
	return report, err
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for free port: %v", err)
	}
	defer func() {
		if err := ln.Close(); err != nil {
			t.Fatalf("close free-port listener: %v", err)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func writeFakeWorker(t *testing.T, path string) {
	t.Helper()
	body := `#!/usr/bin/env python3
import http.server, json, os, sys

if "--doctor" in sys.argv:
    sys.exit(0)
if os.environ.get("AIOPS_FAKE_WORKER_STATE") == "exit":
    sys.exit(0)

port = None
for arg in sys.argv:
    if arg.startswith("--port="):
        port = int(arg.split("=", 1)[1])
if port is None:
    sys.exit(2)

class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass
    def do_POST(self):
        if self.path == "/api/v1/refresh":
            if os.environ.get("AIOPS_FAKE_WORKER_STATE") == "refresh-fail":
                self.send_response(500)
                self.end_headers()
                return
            self.send_response(204)
            self.end_headers()
            return
        self.send_response(404)
        self.end_headers()
    def do_GET(self):
        if self.path == "/readyz":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")
            return
        if self.path == "/api/v1/AIS-1":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            # Mirror the real apiIssueFromView: a failed run is parked in
            # retrying (the Retrying branch wins over the terminal-event scan),
            # so the drill-down reports status "retrying" with retry.kind
            # "failure" and a non-null last_error. There is no terminal "failed"
            # status for a retrying run anymore (#584).
            if os.environ.get("AIOPS_FAKE_WORKER_STATE") == "failed":
                issue = {"issue_id":"issue-1","status":"retrying","retry":{"attempt":1,"kind":"failure"},"last_error":"boom"}
            else:
                issue = {"issue_id":"issue-1","status":"completed","last_error":None}
            self.wfile.write(json.dumps(issue).encode())
            return
        if self.path == "/api/v1/state":
            if os.environ.get("AIOPS_FAKE_WORKER_STATE") == "state-fail":
                self.send_response(500)
                self.end_headers()
                return
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            # Failures retry on the SPEC 8.4 backoff (parked in retrying); the
            # worker no longer emits failed / failed_total (#584, D29).
            if os.environ.get("AIOPS_FAKE_WORKER_STATE") == "failed":
                state = {"completed_total":0,"completed":[],"retrying":[{"issue_id":"issue-1","kind":"failure"}]}
            else:
                state = {"completed_total":1,"completed":["issue-1"]}
            self.wfile.write(json.dumps(state).encode())
            return
        self.send_response(404)
        self.end_headers()

http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()
`
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake worker: %v", err)
	}
}

func writeFakeGH(t *testing.T, path string) {
	t.Helper()
	body := `#!/bin/sh
set -eu
args=" $* "
case "$args" in
  *" --search "* ) printf 'unexpected search API use: %s\n' "$args" >&2; exit 43 ;;
esac
count_file="${AIOPS_FAKE_GH_COUNT:?}"
if [ "$1" = "api" ]; then
  case "$args" in
    *" --paginate repos/xrf9268-hue/aiops-platform/pulls?state=open&per_page=100 "* ) ;;
    * ) printf 'missing paginated pulls API: %s\n' "$args" >&2; exit 41 ;;
  esac
  count=0
  if [ -f "$count_file" ]; then
    count="$(cat "$count_file")"
  fi
  count=$((count + 1))
  printf '%s' "$count" >"$count_file"
  if [ "${AIOPS_FAKE_GH_MODE:-}" = "edited" ]; then
    printf '10|#10 https://example.test/pr/10\n'
  else
    printf '10|#10 https://example.test/pr/10\n'
  fi
  if { [ "${AIOPS_FAKE_GH_MODE:-}" = "new" ] || [ "${AIOPS_FAKE_GH_MODE:-}" = "view-fail" ]; } && [ "$count" -ge 2 ]; then
    printf '11|#11 https://example.test/pr/11\n'
  fi
  exit 0
fi
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then
  case "$args" in
    *" --repo xrf9268-hue/aiops-platform "* ) ;;
    * ) printf 'missing pr view repo: %s\n' "$args" >&2; exit 44 ;;
  esac
  case "$args" in
    *" --json number,isDraft,url,closingIssuesReferences "* ) ;;
    * ) printf 'missing pr view json: %s\n' "$args" >&2; exit 44 ;;
  esac
  case "$args" in
    *"https://github.com/xrf9268-hue/aiops-platform/issues/450"* ) ;;
    * ) printf 'missing issue-url jq filter: %s\n' "$args" >&2; exit 45 ;;
  esac
  count=0
  if [ -f "$count_file" ]; then
    count="$(cat "$count_file")"
  fi
  if [ "$3" = "10" ]; then
    if [ "${AIOPS_FAKE_GH_MODE:-}" != "edited" ] || [ "$count" -ge 2 ]; then
      printf '10|#10 https://example.test/pr/10\n'
    fi
  fi
  if [ "$3" = "11" ]; then
    if [ "${AIOPS_FAKE_GH_MODE:-}" = "view-fail" ]; then
      printf 'simulated pr view failure\n' >&2
      exit 47
    fi
    printf '11|#11 https://example.test/pr/11\n'
  fi
  exit 0
fi
printf 'unexpected gh invocation: %s\n' "$args" >&2
exit 46
`
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
}

func latestSmokeReport(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read report dir: %v", err)
	}
	if len(entries) == 0 {
		return ""
	}
	path := filepath.Join(dir, entries[len(entries)-1].Name())
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report %s: %v", path, err)
	}
	return string(body)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(wd)
}
