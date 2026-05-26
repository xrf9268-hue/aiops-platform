package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		"\"$worker_bin\" --doctor --mode=\"$mode\" \"$workflow\"",
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
		`state_array_contains_issue completed "$selected_issue_id" "$state_file"`,
		`state_array_contains_issue failed "$selected_issue_id" "$state_file"`,
		`"\"$field\":[[:space:]]*\[[^]]*\"$issue_id\""`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("smoke script missing selected-issue array check %q", want)
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

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(wd)
}
