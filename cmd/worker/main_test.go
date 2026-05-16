package main

import (
	"os"
	"path/filepath"
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
}

func TestLoadWorkflowForStartupReconcileFallsBackToCWDWorkflow(t *testing.T) {
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

	wf, err := loadWorkflowForStartupReconcile()
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	if wf.Path != "WORKFLOW.md" {
		t.Fatalf("workflow path = %q, want WORKFLOW.md", wf.Path)
	}
	if wf.Config.Tracker.Kind != "linear" {
		t.Fatalf("tracker kind = %q, want linear", wf.Config.Tracker.Kind)
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

	wf, err := loadWorkflowForStartupReconcile()
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	if wf.Config.Tracker.Kind != workflow.DefaultConfig().Tracker.Kind {
		t.Fatalf("tracker kind = %q, want default %q", wf.Config.Tracker.Kind, workflow.DefaultConfig().Tracker.Kind)
	}
}
