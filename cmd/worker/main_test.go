package main

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/gitea"
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
	for _, required := range []string{"orchestrator.NewOrchestratorState", "orchestrator.NewPoller", "orchestrator.RunPollLoop"} {
		if !strings.Contains(string(src), required) {
			t.Fatalf("cmd/worker/main.go missing %q; worker startup must poll tracker issues through orchestrator runtime state", required)
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

func TestValidateWorkflowForRuntimePreservesPromptOnlyWorkflowCompatibility(t *testing.T) {
	cfg := workflow.DefaultConfig()

	if err := validateWorkflowForRuntime("WORKFLOW.md", workflow.SourcePromptOnly, cfg); err != nil {
		t.Fatalf("validateWorkflowForRuntime(prompt-only defaults) = %v, want nil", err)
	}
	if err := validateWorkflowForRuntime("WORKFLOW.md", workflow.SourceDefault, cfg); err != nil {
		t.Fatalf("validateWorkflowForRuntime(default workflow) = %v, want nil", err)
	}
}

func TestValidateWorkflowForRuntimeRejectsFrontMatterWorkflowMissingTaskFields(t *testing.T) {
	cfg := workflow.DefaultConfig()

	err := validateWorkflowForRuntime("WORKFLOW.md", workflow.SourceFile, cfg)
	if err == nil {
		t.Fatal("validateWorkflowForRuntime(file source defaults) = nil, want repo.clone_url error")
	}
	if !strings.Contains(err.Error(), "WORKFLOW.md: repo.clone_url is required") {
		t.Fatalf("validateWorkflowForRuntime error = %v, want repo.clone_url guidance", err)
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
	for _, want := range []string{"startup reconciliation: workflow source=file", "path=WORKFLOW.md", "tracker.kind=linear"} {
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
