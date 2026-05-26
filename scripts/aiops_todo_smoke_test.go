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

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(wd)
}
