package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
	"github.com/xrf9268-hue/aiops-platform/internal/tracker"
	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

func TestRunTreatsCanceledPollContextAsGracefulShutdown(t *testing.T) {
	if err := normalizeRunError(context.Canceled, context.Canceled); err != nil {
		t.Fatalf("normalizeRunError(context.Canceled, context.Canceled) = %v, want nil", err)
	}
	if err := normalizeRunError(context.DeadlineExceeded, context.DeadlineExceeded); err != nil {
		t.Fatalf("normalizeRunError(context.DeadlineExceeded, context.DeadlineExceeded) = %v, want nil", err)
	}
	if err := normalizeRunError(os.ErrNotExist, nil); err == nil {
		t.Fatal("normalizeRunError(non-context error) = nil, want original error")
	}
}

func TestRunDoesNotTreatUnrelatedDeadlineErrorAsGracefulShutdown(t *testing.T) {
	err := normalizeRunError(context.DeadlineExceeded, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("normalizeRunError(context.DeadlineExceeded, nil) = %v, want deadline error", err)
	}
}

func TestWorkerEntrypointDoesNotRequirePostgresQueue(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	for _, forbidden := range []string{"internal/queue", "pgxpool", "DATABASE_URL"} {
		if strings.Contains(string(src), forbidden) {
			t.Fatalf("cmd/worker/main.go contains %q; worker startup must use tracker + orchestrator runtime state, not the Postgres queue", forbidden)
		}
	}
	for _, required := range []string{"orchestrator.NewOrchestratorState", "orchestrator.NewWorkflowRuntime", "orchestrator.NewRuntimeDispatcher", "orchestrator.NewRuntimePoller", "orchestrator.RunPollLoopWithRuntime", "orchestrator.RunWorkflowReloadLoop"} {
		if !strings.Contains(string(src), required) {
			t.Fatalf("cmd/worker/main.go missing %q; worker startup must poll tracker issues through dynamically reloaded reconciled orchestrator runtime state", required)
		}
	}
}

func TestLoadWorkflowForStartupReconcileUsesConfiguredWorkflowPath(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "linear-workflow.md")
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: linear\n  active_states: [\"AI Ready\"]\n  terminal_states: [\"Done\"]\n---\nprompt\n"
	if err := os.WriteFile(workflowPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	t.Setenv("AIOPS_WORKFLOW_PATH", workflowPath)

	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldOutput)

	wf, err := loadWorkflowForStartupReconcile()
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	if wf.Path != workflowPath {
		t.Fatalf("workflow path = %q, want %q", wf.Path, workflowPath)
	}
	if wf.Config.Tracker.Kind != "linear" {
		t.Fatalf("tracker kind = %q, want linear", wf.Config.Tracker.Kind)
	}
	gotLog := logs.String()
	for _, want := range []string{"startup reconciliation: workflow source=file", "path=" + workflowPath, "tracker.kind=linear"} {
		if !strings.Contains(gotLog, want) {
			t.Fatalf("startup reconciliation log = %q, want substring %q", gotLog, want)
		}
	}
	if strings.Contains(gotLog, "reconciliation will be skipped") {
		t.Fatalf("startup reconciliation log = %q, did not expect skip diagnostic", gotLog)
	}
}

func TestResolveStartupWorkflowUsesPositionalPath(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "service-WORKFLOW.md")
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: linear\n---\nservice prompt\n"
	if err := os.WriteFile(workflowPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	wf, res, err := resolveStartupWorkflow([]string{workflowPath})
	if err != nil {
		t.Fatalf("resolve startup workflow: %v", err)
	}
	if wf.Path != workflowPath {
		t.Fatalf("workflow path = %q, want %q", wf.Path, workflowPath)
	}
	if res.Source != workflow.SourceFile || res.Path != workflowPath {
		t.Fatalf("resolution = %+v, want file at positional path", res)
	}
}

func TestResolveStartupWorkflowDefaultsToCwdWorkflowOnly(t *testing.T) {
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })

	if err := os.MkdirAll(filepath.Join(dir, ".aiops"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(dir, ".aiops", "WORKFLOW.md")
	if err := os.WriteFile(legacyPath, []byte("legacy prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wf, res, err := resolveStartupWorkflow(nil)
	if err != nil {
		t.Fatalf("resolveStartupWorkflow without cwd WORKFLOW.md: %v", err)
	}
	if res.Source != workflow.SourceDefault || res.Path != "" {
		t.Fatalf("resolution = %+v, want built-in default without legacy .aiops path", res)
	}
	if wf.Path != "" || wf.Source != workflow.SourceDefault {
		t.Fatalf("workflow = %+v, want built-in default source", wf)
	}
}

func TestLoadWorkflowForStartupReconcileLogsConfiguredGiteaWorkflow(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "gitea-workflow.md")
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: gitea\n---\nprompt\n"
	if err := os.WriteFile(workflowPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	t.Setenv("AIOPS_WORKFLOW_PATH", workflowPath)

	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldOutput)

	wf, err := loadWorkflowForStartupReconcile()
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	if wf.Config.Tracker.Kind != "gitea" {
		t.Fatalf("tracker kind = %q, want gitea", wf.Config.Tracker.Kind)
	}
	gotLog := logs.String()
	for _, want := range []string{"startup reconciliation: workflow source=file", "path=" + workflowPath, "tracker.kind=gitea"} {
		if !strings.Contains(gotLog, want) {
			t.Fatalf("startup reconciliation log = %q, want substring %q", gotLog, want)
		}
	}
	if strings.Contains(gotLog, "reconciliation will be skipped") {
		t.Fatalf("startup reconciliation log = %q, did not expect skip diagnostic", gotLog)
	}
}

func TestStartupReconcileConfigUsesEffectiveWorkspaceHooks(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Hooks = workflow.WorkspaceHooks{
		BeforeRemove: workflow.WorkspaceHook{Commands: []string{"printf top-level"}},
		TimeoutMs:    1234,
	}

	reconcile := startupReconcileConfigForWorkflow(cfg, nil)
	if !reflect.DeepEqual(reconcile.BeforeRemoveHook.Commands, []string{"printf top-level"}) {
		t.Fatalf("BeforeRemoveHook.Commands = %#v, want top-level effective hook", reconcile.BeforeRemoveHook.Commands)
	}
	if reconcile.HookTimeoutMillis != 1234 {
		t.Fatalf("HookTimeoutMillis = %d, want top-level effective timeout", reconcile.HookTimeoutMillis)
	}
}

func TestTrackerClientForWorkflowBuildsMultiProjectLinearClientForServiceRoutes(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Tracker.Kind = "linear"
	cfg.Tracker.ProjectSlug = ""
	cfg.Services = []workflow.ServiceConfig{
		{Name: "api", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "api-platform"}},
		{Name: "web", Tracker: workflow.ServiceTrackerRouteConfig{ProjectSlug: "web-platform"}},
	}

	client, err := trackerClientForWorkflow(cfg)
	if err != nil {
		t.Fatalf("tracker client: %v", err)
	}

	multi, ok := client.(interface{ Trackers() []trackerRuntimeClient })
	if !ok {
		t.Fatalf("client type = %T, want multi-project tracker", client)
	}
	got := multi.Trackers()
	if len(got) != 2 {
		t.Fatalf("linear tracker count = %d, want 2 service projects", len(got))
	}
	projects := make([]string, 0, len(got))
	for _, client := range got {
		linearClient, ok := client.(*tracker.LinearClient)
		if !ok {
			t.Fatalf("linear tracker type = %T, want *tracker.LinearClient", client)
		}
		projects = append(projects, linearClient.Config.ProjectSlug)
	}
	if !reflect.DeepEqual(projects, []string{"api-platform", "web-platform"}) {
		t.Fatalf("linear tracker projects = %#v, want service projects", projects)
	}
}

func TestTrackerClientForWorkflowUsesGiteaProjectSlugBeforeEnvBaseURL(t *testing.T) {
	t.Setenv("GITEA_BASE_URL", "https://gitea-env.example.test/")
	cfg := workflow.DefaultConfig()
	cfg.Tracker.Kind = "gitea"
	cfg.Tracker.ProjectSlug = "https://gitea-workflow.example.test/"
	cfg.Repo.Owner = "owner"
	cfg.Repo.Name = "repo"

	client, err := trackerClientForWorkflow(cfg)
	if err != nil {
		t.Fatalf("tracker client: %v", err)
	}
	giteaClient, ok := client.(*gitea.TrackerClient)
	if !ok {
		t.Fatalf("client type = %T, want *gitea.TrackerClient", client)
	}
	if giteaClient.BaseURL != "https://gitea-workflow.example.test" {
		t.Fatalf("base URL = %q, want tracker.project_slug without trailing slash", giteaClient.BaseURL)
	}
}

func TestValidateWorkflowForRuntimeRejectsPromptOnlyWorkflowMissingTaskFields(t *testing.T) {
	cfg := workflow.DefaultConfig()

	err := validateWorkflowForRuntime("WORKFLOW.md", workflow.SourcePromptOnly, cfg)
	if err == nil {
		t.Fatal("validateWorkflowForRuntime(prompt-only defaults) = nil, want repo.clone_url error")
	}
	for _, want := range []string{"WORKFLOW.md", "repo.clone_url", "poll-based worker runtime"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("validateWorkflowForRuntime error = %v, want substring %q", err, want)
		}
	}
}

func TestValidateWorkflowForRuntimeRejectsDefaultWorkflowMissingTaskFields(t *testing.T) {
	cfg := workflow.DefaultConfig()

	err := validateWorkflowForRuntime("WORKFLOW.md", workflow.SourceDefault, cfg)
	if err == nil {
		t.Fatal("validateWorkflowForRuntime(default workflow) = nil, want repo.clone_url error")
	}
	for _, want := range []string{"built-in workflow defaults", "repo.clone_url", "poll-based worker runtime"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("validateWorkflowForRuntime error = %v, want substring %q", err, want)
		}
	}
}

func TestValidateWorkflowForRuntimeAcceptsConfiguredRepo(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Repo.CloneURL = "git@example.com:o/r.git"

	for _, source := range []workflow.Source{workflow.SourceFile, workflow.SourcePromptOnly, workflow.SourceDefault} {
		if err := validateWorkflowForRuntime("WORKFLOW.md", source, cfg); err != nil {
			t.Fatalf("validateWorkflowForRuntime(source=%s) = %v, want nil", source, err)
		}
	}
}

func TestValidateWorkflowForRuntimeAcceptsServiceOnlyRepos(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Services = []workflow.ServiceConfig{
		{Name: "api", Repo: workflow.RepoConfig{CloneURL: "git@example.com:o/api.git"}},
	}

	if err := validateWorkflowForRuntime("WORKFLOW.md", workflow.SourceFile, cfg); err != nil {
		t.Fatalf("validateWorkflowForRuntime(service-only repos) = %v, want nil", err)
	}
}

func TestWorkerReconciliationConfigIncludesInactiveStates(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Repo.CloneURL = "git@example.com:o/r.git"
	cfg.Tracker.Kind = "linear"
	cfg.Tracker.ActiveStates = []string{"AI Ready", "In Progress", "Rework"}
	cfg.Tracker.TerminalStates = []string{"Done", "Canceled"}

	reconcile := reconciliationConfigForWorkflow(cfg)
	if len(reconcile.InactiveStates) == 0 {
		t.Fatalf("inactive reconciliation states = %v, want non-empty states for explicit inactive tracker observations", reconcile.InactiveStates)
	}
	if containsState(reconcile.InactiveStates, "AI Ready") || containsState(reconcile.InactiveStates, "In Progress") || containsState(reconcile.InactiveStates, "Rework") {
		t.Fatalf("inactive reconciliation states = %v, must not include configured active states", reconcile.InactiveStates)
	}
	if containsState(reconcile.InactiveStates, "Done") || containsState(reconcile.InactiveStates, "Canceled") {
		t.Fatalf("inactive reconciliation states = %v, must not duplicate terminal states", reconcile.InactiveStates)
	}
	if !containsState(reconcile.InactiveStates, "Backlog") || !containsState(reconcile.InactiveStates, "Human Review") {
		t.Fatalf("inactive reconciliation states = %v, want Backlog and Human Review", reconcile.InactiveStates)
	}
}

func TestWorkerReconciliationConfigDoesNotProbeUnmappedGiteaInactiveStates(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Repo.CloneURL = "git@example.com:o/r.git"
	cfg.Tracker.Kind = "gitea"
	cfg.Tracker.ActiveStates = []string{"AI Ready", "In Progress", "Rework"}
	cfg.Tracker.TerminalStates = []string{"Done", "Canceled"}

	reconcile := reconciliationConfigForWorkflow(cfg)
	if containsState(reconcile.InactiveStates, "Backlog") {
		t.Fatalf("inactive reconciliation states = %v, must not include unmapped Gitea Backlog state", reconcile.InactiveStates)
	}
	if !containsState(reconcile.InactiveStates, "Human Review") {
		t.Fatalf("inactive reconciliation states = %v, want mapped Gitea Human Review state", reconcile.InactiveStates)
	}
}

func TestWorkerReconciliationConfigUsesWorkflowInactiveStates(t *testing.T) {
	cfg := workflow.DefaultConfig()
	cfg.Repo.CloneURL = "git@example.com:o/r.git"
	cfg.Tracker.ActiveStates = []string{"AI Ready", "In Progress"}
	cfg.Tracker.TerminalStates = []string{"Done", "Canceled"}
	cfg.Tracker.InactiveStates = []string{"Paused", "Blocked", "Done", "AI Ready"}

	reconcile := reconciliationConfigForWorkflow(cfg)
	if !containsState(reconcile.InactiveStates, "Paused") || !containsState(reconcile.InactiveStates, "Blocked") {
		t.Fatalf("inactive reconciliation states = %v, want workflow-configured inactive states", reconcile.InactiveStates)
	}
	if containsState(reconcile.InactiveStates, "Done") || containsState(reconcile.InactiveStates, "AI Ready") {
		t.Fatalf("inactive reconciliation states = %v, must exclude configured active/terminal states", reconcile.InactiveStates)
	}
}

func containsState(states []string, want string) bool {
	for _, state := range states {
		if state == want {
			return true
		}
	}
	return false
}

func TestValidateWorkflowForRuntimeRejectsFrontMatterWorkflowMissingTaskFields(t *testing.T) {
	cfg := workflow.DefaultConfig()

	err := validateWorkflowForRuntime("WORKFLOW.md", workflow.SourceFile, cfg)
	if err == nil {
		t.Fatal("validateWorkflowForRuntime(file source defaults) = nil, want repo.clone_url error")
	}
	for _, want := range []string{"WORKFLOW.md", "repo.clone_url", "poll-based worker runtime"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("validateWorkflowForRuntime error = %v, want substring %q", err, want)
		}
	}
}

func TestLoadWorkflowForStartupReconcileClassifiesConfiguredPromptOnlyWorkflow(t *testing.T) {
	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "prompt-only-workflow.md")
	body := "Follow the repository workflow without YAML front matter.\n"
	if err := os.WriteFile(workflowPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	t.Setenv("AIOPS_WORKFLOW_PATH", workflowPath)

	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldOutput)

	wf, err := loadWorkflowForStartupReconcile()
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	if wf.Config.Tracker.Kind != workflow.DefaultConfig().Tracker.Kind {
		t.Fatalf("tracker kind = %q, want default %q", wf.Config.Tracker.Kind, workflow.DefaultConfig().Tracker.Kind)
	}
	gotLog := logs.String()
	for _, want := range []string{"startup reconciliation: workflow source=prompt_only", "path=" + workflowPath, "tracker.kind=gitea"} {
		if !strings.Contains(gotLog, want) {
			t.Fatalf("startup reconciliation log = %q, want substring %q", gotLog, want)
		}
	}
	for _, forbidden := range []string{"workflow source=file", "reconciliation will be skipped"} {
		if strings.Contains(gotLog, forbidden) {
			t.Fatalf("startup reconciliation log = %q, did not expect %q", gotLog, forbidden)
		}
	}
}

func TestLoadWorkflowForStartupReconcileResolvesCWDWorkflowAndLogsSource(t *testing.T) {
	dir := t.TempDir()
	body := "---\nrepo:\n  owner: o\n  name: r\n  clone_url: git@example.com:o/r.git\ntracker:\n  kind: linear\n---\nprompt\n"
	if err := os.WriteFile(filepath.Join(dir, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(oldwd); err != nil {
			t.Fatalf("restore wd: %v", err)
		}
	}()

	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldOutput)

	wf, err := loadWorkflowForStartupReconcile()
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	if wf.Path != filepath.Join(dir, "WORKFLOW.md") {
		t.Fatalf("workflow path = %q, want %q", wf.Path, filepath.Join(dir, "WORKFLOW.md"))
	}
	if wf.Config.Tracker.Kind != "linear" {
		t.Fatalf("tracker kind = %q, want linear", wf.Config.Tracker.Kind)
	}
	gotLog := logs.String()
	for _, want := range []string{"startup reconciliation: workflow source=file", "path=" + filepath.Join(dir, "WORKFLOW.md"), "tracker.kind=linear"} {
		if !strings.Contains(gotLog, want) {
			t.Fatalf("startup reconciliation log = %q, want substring %q", gotLog, want)
		}
	}
}

func TestLoadWorkflowForStartupReconcileDefaultsWhenNoWorkflowExists(t *testing.T) {
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() {
		if err := os.Chdir(oldwd); err != nil {
			t.Fatalf("restore wd: %v", err)
		}
	}()

	var logs bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldOutput)

	wf, err := loadWorkflowForStartupReconcile()
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	if wf.Config.Tracker.Kind != workflow.DefaultConfig().Tracker.Kind {
		t.Fatalf("tracker kind = %q, want default %q", wf.Config.Tracker.Kind, workflow.DefaultConfig().Tracker.Kind)
	}
	gotLog := logs.String()
	for _, want := range []string{"startup reconciliation: workflow source=default", "tracker.kind=gitea"} {
		if !strings.Contains(gotLog, want) {
			t.Fatalf("startup reconciliation log = %q, want substring %q", gotLog, want)
		}
	}
	if strings.Contains(gotLog, "reconciliation will be skipped") {
		t.Fatalf("startup reconciliation log = %q, did not expect skip diagnostic", gotLog)
	}
}
