package scripts_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestConcurrentLinearLifecycleRunbookPinsFiveStateHandoffContract(t *testing.T) {
	root := repoRoot(t)
	path := filepath.Join(root, "docs", "runbooks", "concurrent-linear-codex-e2e.md")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	text := string(body)

	for _, want := range []string{
		"Backlog, Todo, In Progress, In Review, Done",
		"Todo/In Progress active work -> In Review",
		"Backlog and Done are schema checks, not required agent transitions",
		"Do not move an already-active current issue back into In Progress",
		"projects(filter: { slugId: { eq: $projectSlug } }, first: 1)",
		"max_concurrent_agents: 2",
		"Use the issue body as the task source of truth",
		"do not copy issue-specific task text into WORKFLOW.md",
		"Do not run this smoke against a shared Linear project",
		"exactly two active issues",
		"running: 2",
		"/api/v1/state",
		"cmd/tui --raw",
		"AIOPS_STATE_API_TOKEN",
		"LINEAR_API_KEY",
		"api_key: $LINEAR_API_KEY",
		"Never pass Linear tokens as command-line arguments",
		"LINEAR_API_KEY_FILE",
		`export LINEAR_API_KEY="$(cat "$LINEAR_API_KEY_FILE")"`,
		"--deploy=binary",
		"com.aiops-platform.linear-e2e",
		"AIOPS_LINEAR_E2E_PORT",
		`lsof -nP -iTCP:"$worker_port" -sTCP:LISTEN`,
		`worker_url="http://127.0.0.1:${worker_port}"`,
		`curl_cfg="/tmp/aiops-linear-e2e/curl.cfg"`,
		`launchctl bootout "gui/$(id -u)/$label"`,
		"worker_ready=false",
		`"$worker_url/livez"`,
		`"$worker_url/readyz"`,
		"FAIL worker did not become ready",
		"saw_running_2=false",
		"FAIL did not observe running: 2",
		"AIOPS_TIMEOUT_BIN",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("runbook missing %q", want)
		}
	}

	for _, stale := range []string{"AIS-59", "AIS-60", "edit-support", "subtitle task"} {
		if strings.Contains(text, stale) {
			t.Fatalf("runbook contains stale manual-run text %q", stale)
		}
	}

	if strings.Contains(extractWorkflowSnippet(t, text), "\ntools:\n") {
		t.Fatalf("workflow snippet uses stale tools.linear_graphql nesting; want codex.linear_graphql")
	}
	startup := textAfter(t, text, "Start a dedicated per-user launchd worker")
	assertInOrder(t, startup, []string{
		`launchctl bootout "gui/$(id -u)/$label"`,
		`lsof -nP -iTCP:"$worker_port" -sTCP:LISTEN`,
		"launchctl bootstrap",
		"worker_ready=false",
		`"$worker_url/readyz"`,
	})
	teardown := textAfter(t, text, "Stop and remove the temporary LaunchAgent")
	for _, want := range []string{
		`launchctl bootout "gui/$(id -u)" "$plist_path"`,
		`rm -f "$plist_path" "$wrapper_path" "$curl_cfg"`,
	} {
		if !strings.Contains(teardown, want) {
			t.Fatalf("teardown section missing %q", want)
		}
	}

	workflowPath := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte(extractWorkflowSnippet(t, text)), 0o600); err != nil {
		t.Fatalf("write extracted WORKFLOW.md: %v", err)
	}
	t.Setenv("LINEAR_API_KEY", "linear-docs-test-key")
	loaded, err := workflow.Load(workflowPath)
	if err != nil {
		t.Fatalf("workflow.Load(extracted WORKFLOW.md) = %v; want nil", err)
	}

	cfg := loaded.Config
	if cfg.Agent.Default != "codex-app-server" {
		t.Fatalf("agent.default = %q; want %q", cfg.Agent.Default, "codex-app-server")
	}
	if cfg.Agent.MaxConcurrentAgents != 2 {
		t.Fatalf("agent.max_concurrent_agents = %d; want 2", cfg.Agent.MaxConcurrentAgents)
	}
	if cfg.Tracker.Kind != "linear" {
		t.Fatalf("tracker.kind = %q; want %q", cfg.Tracker.Kind, "linear")
	}
	if cfg.Tracker.APIKey != "linear-docs-test-key" {
		t.Fatalf("tracker.api_key = %q; want env-expanded LINEAR_API_KEY", cfg.Tracker.APIKey)
	}
	if cfg.Tracker.ProjectSlug == "" {
		t.Fatalf("tracker.project_slug = %q; want non-empty placeholder", cfg.Tracker.ProjectSlug)
	}
	if !cfg.Codex.LinearGraphQL.AllowMutations {
		t.Fatalf("codex.linear_graphql.allow_mutations = false; want true")
	}
	wantMutations := []string{"issueUpdate", "commentCreate"}
	if !reflect.DeepEqual(cfg.Codex.LinearGraphQL.AllowedMutations, wantMutations) {
		t.Fatalf("codex.linear_graphql.allowed_mutations = %v; want %v", cfg.Codex.LinearGraphQL.AllowedMutations, wantMutations)
	}
}

func assertInOrder(t *testing.T, text string, wants []string) {
	t.Helper()

	offset := 0
	for _, want := range wants {
		idx := strings.Index(text[offset:], want)
		if idx == -1 {
			t.Fatalf("runbook missing %q after offset %d", want, offset)
		}
		offset += idx + len(want)
	}
}

func textAfter(t *testing.T, text, marker string) string {
	t.Helper()

	start := strings.Index(text, marker)
	if start == -1 {
		t.Fatalf("runbook missing section marker %q", marker)
	}
	return text[start:]
}

func extractWorkflowSnippet(t *testing.T, text string) string {
	t.Helper()

	const fence = "```yaml\n---\n"
	start := strings.Index(text, fence)
	if start == -1 {
		t.Fatalf("runbook missing workflow YAML fence starting with %q", fence)
	}
	start += len("```yaml\n")
	end := strings.Index(text[start:], "\n```")
	if end == -1 {
		t.Fatalf("runbook workflow YAML fence missing closing marker")
	}
	return text[start : start+end]
}
