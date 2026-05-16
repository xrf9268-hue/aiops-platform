package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xrf9268-hue/aiops-platform/internal/workflow"
)

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
		t.Fatalf("startup reconciliation log = %q, did not expect skip diagnostic for linear tracker", gotLog)
	}
}

func TestLoadWorkflowForStartupReconcileWarnsWhenConfiguredWorkflowIsNonLinear(t *testing.T) {
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
	for _, want := range []string{"startup reconciliation: workflow source=file", "path=" + workflowPath, "tracker.kind=gitea", "reconciliation will be skipped"} {
		if !strings.Contains(gotLog, want) {
			t.Fatalf("startup reconciliation log = %q, want substring %q", gotLog, want)
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
	for _, want := range []string{"startup reconciliation: workflow source=default", "tracker.kind=gitea", "reconciliation will be skipped"} {
		if !strings.Contains(gotLog, want) {
			t.Fatalf("startup reconciliation log = %q, want substring %q", gotLog, want)
		}
	}
}
